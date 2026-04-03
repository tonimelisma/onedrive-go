package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

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
// changes (e.g., `issues clear`) and logs a watch summary.
func (rt *watchRuntime) handleRecheckTick(ctx context.Context) {
	if rt.externalDBChanged(ctx) {
		rt.handleExternalChanges(ctx)
	}

	rt.logWatchSummary(ctx)
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version. Currently handles big-delete clearance: if the
// counter is held but all big_delete_held rows have been cleared (via
// `issues clear`), releases the counter so deletes resume on the next
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
	issues, err := rt.engine.baseline.ListActionableFailures(ctx)
	if err != nil {
		return
	}

	remoteBlocked, err := rt.engine.baseline.ListRemoteBlockedFailures(ctx)
	if err != nil {
		return
	}

	remoteScopeCount := 0
	seenRemote := make(map[synctypes.ScopeKey]bool, len(remoteBlocked))
	for i := range remoteBlocked {
		if !remoteBlocked[i].ScopeKey.IsPermRemote() || seenRemote[remoteBlocked[i].ScopeKey] {
			continue
		}
		seenRemote[remoteBlocked[i].ScopeKey] = true
		remoteScopeCount++
	}

	totalIssues := len(issues) + remoteScopeCount
	if totalIssues == 0 {
		if rt.lastSummaryTotal != 0 {
			rt.lastSummaryTotal = 0
		}

		return
	}

	if totalIssues == rt.lastSummaryTotal {
		return
	}

	rt.lastSummaryTotal = totalIssues

	counts := make(map[string]int)
	for i := range issues {
		counts[issues[i].IssueType]++
	}
	if remoteScopeCount > 0 {
		counts[synctypes.IssueSharedFolderBlocked] = remoteScopeCount
	}

	parts := make([]string, 0, len(counts))
	for typ, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, typ))
	}

	sort.Strings(parts)

	rt.engine.logger.Warn("visible issues",
		slog.Int("total", totalIssues),
		slog.String("breakdown", strings.Join(parts, ", ")),
	)
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
func (e *Engine) newReconcileTicker(interval time.Duration) *time.Ticker {
	if interval <= 0 {
		return nil
	}

	return time.NewTicker(interval)
}

// initReconcileTicker creates the periodic full-reconciliation timer and
// returns its channel plus a stop function. If reconciliation is disabled,
// both the channel and stop function are nil.
func (e *Engine) initReconcileTicker(opts synctypes.WatchOpts) (<-chan time.Time, func()) {
	interval := e.resolveReconcileInterval(opts)
	ticker := e.newReconcileTicker(interval)

	if ticker == nil {
		return nil, nil
	}

	e.logger.Info("periodic full reconciliation enabled",
		slog.Duration("interval", interval),
	)

	return ticker.C, ticker.Stop
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

		observed := changeEventsToObservedItems(rt.engine.logger, events)
		if commitErr := rt.engine.baseline.CommitObservation(
			ctx, observed, deltaToken, rt.engine.driveID,
		); commitErr != nil {
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
