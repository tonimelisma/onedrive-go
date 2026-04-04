package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// externalDBChanged checks whether another process (e.g., the CLI) wrote to
// the database since the last check. Uses PRAGMA data_version — changes every
// time another connection commits a write. The engine's own writes don't
// change it. Returns true if the version advanced.
func (rt *watchRuntime) externalDBChanged(ctx context.Context) bool {
	dv, err := rt.engine.baseline.DataVersion(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to check data_version",
			slog.String("error", err.Error()),
		)

		return false
	}

	if dv == rt.lastDataVersion {
		return false
	}

	rt.lastDataVersion = dv

	return true
}

// handleRecheckTick processes a recheck timer tick: detects external DB
// changes (e.g., `issues force-deletes`) and logs a watch summary.
func (rt *watchRuntime) handleRecheckTick(ctx context.Context) {
	if rt.externalDBChanged(ctx) {
		rt.handleExternalChanges(ctx)
	}

	rt.logWatchSummary(ctx)
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version. Currently handles big-delete clearance: if the
// counter is held but all big_delete_held rows have been cleared (via
// `issues force-deletes`), releases the counter so deletes resume on the next
// observation cycle.
func (rt *watchRuntime) handleExternalChanges(ctx context.Context) {
	if rt.deleteCounter != nil && rt.deleteCounter.IsHeld() {
		rows, err := rt.engine.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueBigDeleteHeld)
		if err != nil {
			rt.engine.logger.Warn("failed to check big-delete-held entries",
				slog.String("error", err.Error()),
			)

			return
		}

		if len(rows) == 0 {
			rt.deleteCounter.Release()
			rt.engine.logger.Info("big-delete protection cleared by user")
		}
	}

	rt.clearResolvedPermissionScopes(ctx)
	rt.mustAssertScopeInvariants(ctx, rt, "handle external changes")
}

// clearResolvedPermissionScopes checks if any permission scope blocks have had
// their sync_failures cleared (by user action via CLI), and releases the
// corresponding scope blocks.
func (rt *watchRuntime) clearResolvedPermissionScopes(ctx context.Context) {
	scopeKeys := rt.scopeController().scopeBlockKeys(rt)
	if len(scopeKeys) == 0 {
		return
	}

	localIssues, err := rt.engine.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
	if err != nil {
		rt.engine.logger.Warn("failed to check local permission failures",
			slog.String("error", err.Error()),
		)

		return
	}

	remoteIssues, err := rt.engine.baseline.ListRemoteBlockedFailures(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to check remote permission failures",
			slog.String("error", err.Error()),
		)

		return
	}

	activeScopes := make(map[synctypes.ScopeKey]bool, len(localIssues)+len(remoteIssues))
	for i := range localIssues {
		if localIssues[i].ScopeKey.IsPermDir() {
			activeScopes[localIssues[i].ScopeKey] = true
		}
	}
	for i := range remoteIssues {
		if remoteIssues[i].ScopeKey.IsPermRemote() {
			activeScopes[remoteIssues[i].ScopeKey] = true
		}
	}

	for _, key := range scopeKeys {
		if (key.IsPermDir() || key.IsPermRemote()) && !activeScopes[key] {
			if err := rt.scopeController().releaseScope(ctx, rt, key); err != nil {
				rt.engine.logger.Warn("failed to release externally-cleared permission scope",
					slog.String("scope", key.String()),
					slog.String("error", err.Error()),
				)
				continue
			}

			rt.engine.logger.Info("permission scope block cleared by user",
				slog.String("scope", key.String()),
			)
		}
	}
}

// logWatchSummary logs a periodic one-liner summary of actionable issues
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
func (rt *watchRuntime) logWatchSummary(ctx context.Context) {
	summary, err := rt.engine.baseline.ReadVisibleIssueSummary(ctx)
	if err != nil {
		return
	}

	groups, err := rt.engine.baseline.ListVisibleIssueGroups(ctx)
	if err != nil {
		return
	}

	rt.logRemoteBlockedChanges(groups)

	totalIssues := summary.VisibleTotal()
	if totalIssues == 0 {
		if rt.lastSummaryTotal != 0 || rt.lastSummarySignature != "" {
			rt.engine.logger.Info("visible issues cleared")
		}
		rt.lastSummaryTotal = 0
		rt.lastSummarySignature = ""
		return
	}

	signature, breakdown := summarySignature(summary)
	if signature == rt.lastSummarySignature {
		return
	}

	rt.lastSummaryTotal = totalIssues
	rt.lastSummarySignature = signature

	rt.engine.logger.Warn("visible issues",
		slog.Int("total", totalIssues),
		slog.String("breakdown", breakdown),
	)
}

func (rt *watchRuntime) logRemoteBlockedChanges(groups []syncstore.VisibleIssueGroup) {
	current := make(map[synctypes.ScopeKey]string, len(groups))

	for i := range groups {
		group := groups[i]
		if group.RemoteBlocked == nil || !group.ScopeKey.IsPermRemote() {
			continue
		}

		signature := strings.Join(group.RemoteBlocked.BlockedPaths, "\x00")
		current[group.ScopeKey] = signature

		switch previous, ok := rt.lastRemoteBlocked[group.ScopeKey]; {
		case !ok:
			rt.engine.logger.Warn("shared-folder writes blocked",
				slog.String("boundary", group.RemoteBlocked.BoundaryPath),
				slog.Int("blocked_writes", len(group.RemoteBlocked.BlockedPaths)),
			)
		case previous != signature:
			rt.engine.logger.Warn("shared-folder writes still blocked",
				slog.String("boundary", group.RemoteBlocked.BoundaryPath),
				slog.Int("blocked_writes", len(group.RemoteBlocked.BlockedPaths)),
			)
		}
	}

	for scopeKey := range rt.lastRemoteBlocked {
		if _, ok := current[scopeKey]; ok {
			continue
		}
		rt.engine.logger.Info("shared-folder write block cleared",
			slog.String("boundary", scopeKey.RemotePath()),
		)
	}

	rt.lastRemoteBlocked = current
}

func summarySignature(summary syncstore.IssueSummary) (string, string) {
	parts := make([]string, 0, len(summary.Groups))
	for i := range summary.Groups {
		parts = append(parts, fmt.Sprintf("%d %s", summary.Groups[i].Count, summary.Groups[i].Key))
	}
	sort.Strings(parts)
	breakdown := strings.Join(parts, ", ")
	return fmt.Sprintf("%d|%s", summary.VisibleTotal(), breakdown), breakdown
}

// recordSkippedItems records observation-time rejections (invalid names,
// path too long, file too large) as actionable failures in sync_failures.
func (flow *engineFlow) recordSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	eng := flow.engine

	if len(skipped) == 0 {
		return
	}

	byReason := make(map[string][]synctypes.SkippedItem)
	for i := range skipped {
		byReason[skipped[i].Reason] = append(byReason[skipped[i].Reason], skipped[i])
	}

	for reason, items := range byReason {
		const aggregateThreshold = 10
		if len(items) > aggregateThreshold {
			const sampleCount = 3
			samples := make([]string, 0, sampleCount)
			for i := range items {
				if i >= sampleCount {
					break
				}
				samples = append(samples, items[i].Path)
			}

			eng.logger.Warn("observation filter: skipped files",
				slog.String("issue_type", reason),
				slog.Int("count", len(items)),
				slog.Any("sample_paths", samples),
			)
		} else {
			for i := range items {
				eng.logger.Warn("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		}

		failures := make([]synctypes.ActionableFailure, len(items))
		for i := range items {
			failures[i] = synctypes.ActionableFailure{
				Path:       items[i].Path,
				DriveID:    eng.driveID,
				Direction:  synctypes.DirectionUpload,
				ActionType: synctypes.ActionUpload,
				IssueType:  reason,
				Error:      items[i].Detail,
				FileSize:   items[i].FileSize,
			}
		}

		if err := eng.baseline.UpsertActionableFailures(ctx, failures); err != nil {
			eng.logger.Error("failed to record skipped items",
				slog.String("issue_type", reason),
				slog.Int("count", len(failures)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// clearResolvedSkippedItems removes sync_failures entries for scanner-detectable
// issue types that are no longer present in the current scan.
func (flow *engineFlow) clearResolvedSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	eng := flow.engine

	currentByType := make(map[string][]string)
	for i := range skipped {
		currentByType[skipped[i].Reason] = append(currentByType[skipped[i].Reason], skipped[i].Path)
	}

	scannerIssueTypes := []string{
		synctypes.IssueInvalidFilename, synctypes.IssuePathTooLong,
		synctypes.IssueFileTooLarge, synctypes.IssueCaseCollision,
		synctypes.IssueHashPanic,
	}
	for _, issueType := range scannerIssueTypes {
		paths := currentByType[issueType]
		if err := eng.baseline.ClearResolvedActionableFailures(ctx, issueType, paths); err != nil {
			eng.logger.Error("failed to clear resolved failures",
				slog.String("issue_type", issueType),
				slog.String("error", err.Error()),
			)
		}
	}
}

// resolveReconcileInterval returns the configured reconcile interval or the
// default. Negative values disable periodic reconciliation. Values below
// minReconcileInterval are clamped up.
func (e *Engine) resolveReconcileInterval(opts synctypes.WatchOpts) time.Duration {
	if opts.ReconcileInterval < 0 {
		return 0
	}

	if opts.ReconcileInterval > 0 {
		if opts.ReconcileInterval < minReconcileInterval {
			e.logger.Warn("reconcile interval below minimum, clamping",
				slog.Duration("requested", opts.ReconcileInterval),
				slog.Duration("minimum", minReconcileInterval),
			)

			return minReconcileInterval
		}

		return opts.ReconcileInterval
	}

	return defaultReconcileInterval
}

// newReconcileTicker creates a ticker for periodic reconciliation. Returns
// nil if the interval is 0 (disabled).
func (e *Engine) newReconcileTicker(interval time.Duration) syncTicker {
	if interval <= 0 {
		return nil
	}

	return e.newTicker(interval)
}

// initReconcileTicker creates the periodic full-reconciliation timer.
func (e *Engine) initReconcileTicker(opts synctypes.WatchOpts) syncTicker {
	interval := e.resolveReconcileInterval(opts)
	ticker := e.newReconcileTicker(interval)

	if ticker == nil {
		return nil
	}

	e.logger.Info("periodic full reconciliation enabled",
		slog.Duration("interval", interval),
	)

	return ticker
}

// runFullReconciliationAsync spawns a goroutine for full delta enumeration +
// orphan detection. Non-blocking — the watch loop continues processing events
// while reconciliation runs. The goroutine sends a reconcileResult back to the
// watch loop, and the loop applies shortcut snapshot updates plus buffer
// injection from its own goroutine.
func (rt *watchRuntime) runFullReconciliationAsync(ctx context.Context, bl *synctypes.Baseline) {
	if rt.reconcileActive {
		rt.engine.logger.Info("full reconciliation skipped — previous still running")
		return
	}
	rt.reconcileActive = true

	go func() {
		result := reconcileResult{}

		start := time.Now()
		rt.engine.logger.Info("periodic full reconciliation starting")

		events, deltaToken, err := rt.observeRemoteFull(ctx, bl)
		if err != nil {
			if ctx.Err() == nil {
				rt.engine.logger.Error("full reconciliation failed",
					slog.String("error", err.Error()),
				)
			}
			rt.finishFullReconciliation(ctx, result)
			return
		}

		if commitErr := rt.commitObservedRemote(ctx, events, deltaToken); commitErr != nil {
			rt.engine.logger.Error("failed to commit full reconciliation observations",
				slog.String("error", commitErr.Error()),
			)
			rt.finishFullReconciliation(ctx, result)
			return
		}

		if rt.afterReconcileCommit != nil {
			rt.afterReconcileCommit()
		}

		if ctx.Err() != nil {
			rt.engine.logger.Info("full reconciliation: observations committed, stopping for shutdown")
			rt.finishFullReconciliation(ctx, result)
			return
		}

		events = filterOutShortcuts(events)

		shortcutEvents, scErr := rt.shortcutCoordinator().reconcileShortcutScopes(ctx, bl)
		if scErr != nil {
			rt.engine.logger.Warn("shortcut reconciliation failed during full reconciliation",
				slog.String("error", scErr.Error()),
			)
		}

		events = append(events, shortcutEvents...)

		if len(events) == 0 {
			rt.engine.logger.Info("periodic full reconciliation complete: no changes",
				slog.Duration("duration", time.Since(start)),
			)
			rt.finishFullReconciliation(ctx, result)
			return
		}
		result.events = events
		if refreshed, refreshErr := rt.engine.baseline.ListShortcuts(ctx); refreshErr == nil {
			result.shortcuts = refreshed
		}

		rt.engine.logger.Info("periodic full reconciliation complete",
			slog.Int("events", len(events)),
			slog.Duration("duration", time.Since(start)),
		)

		rt.finishFullReconciliation(ctx, result)
	}()
}

func (rt *watchRuntime) finishFullReconciliation(ctx context.Context, result reconcileResult) {
	select {
	case rt.reconcileResults <- result:
	case <-ctx.Done():
		select {
		case rt.reconcileResults <- result:
		default:
		}
	}
}

func (rt *watchRuntime) applyReconcileResult(result reconcileResult) {
	rt.reconcileActive = false

	if len(result.shortcuts) > 0 {
		rt.setShortcuts(result.shortcuts)
	}

	for i := range result.events {
		rt.buf.Add(&result.events[i])
	}
}
