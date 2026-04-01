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
func (e *Engine) externalDBChanged(ctx context.Context) bool {
	dv, err := e.baseline.DataVersion(ctx)
	if err != nil {
		e.logger.Warn("failed to check data_version",
			slog.String("error", err.Error()),
		)

		return false
	}

	if dv == e.watch.lastDataVersion {
		return false
	}

	e.watch.lastDataVersion = dv

	return true
}

// handleRecheckTick processes a recheck timer tick: detects external DB
// changes (e.g., `issues clear`) and logs a watch summary.
func (e *Engine) handleRecheckTick(ctx context.Context) {
	if e.externalDBChanged(ctx) {
		e.handleExternalChanges(ctx)
	}

	e.logWatchSummary(ctx)
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version. Currently handles big-delete clearance: if the
// counter is held but all big_delete_held rows have been cleared (via
// `issues clear`), releases the counter so deletes resume on the next
// observation cycle.
func (e *Engine) handleExternalChanges(ctx context.Context) {
	if e.watch != nil && e.watch.deleteCounter != nil && e.watch.deleteCounter.IsHeld() {
		rows, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueBigDeleteHeld)
		if err != nil {
			e.logger.Warn("failed to check big-delete-held entries",
				slog.String("error", err.Error()),
			)

			return
		}

		if len(rows) == 0 {
			e.watch.deleteCounter.Release()
			e.logger.Info("big-delete protection cleared by user")
		}
	}

	e.clearResolvedPermissionScopes(ctx)
	e.mustAssertScopeInvariants(ctx, "handle external changes")
}

// clearResolvedPermissionScopes checks if any permission scope blocks have had
// their sync_failures cleared (by user action via CLI), and releases the
// corresponding scope blocks.
func (e *Engine) clearResolvedPermissionScopes(ctx context.Context) {
	if e.watch == nil {
		return
	}

	scopeKeys := e.scopeBlockKeys()
	if len(scopeKeys) == 0 {
		return
	}

	localIssues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
	if err != nil {
		e.logger.Warn("failed to check local permission failures",
			slog.String("error", err.Error()),
		)

		return
	}

	remoteIssues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssuePermissionDenied)
	if err != nil {
		e.logger.Warn("failed to check remote permission failures",
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
			if err := e.releaseScope(ctx, key); err != nil {
				e.logger.Warn("failed to release externally-cleared permission scope",
					slog.String("scope", key.String()),
					slog.String("error", err.Error()),
				)
				continue
			}

			e.logger.Info("permission scope block cleared by user",
				slog.String("scope", key.String()),
			)
		}
	}
}

// logWatchSummary logs a periodic one-liner summary of actionable issues
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
func (e *Engine) logWatchSummary(ctx context.Context) {
	issues, err := e.baseline.ListActionableFailures(ctx)
	if err != nil || len(issues) == 0 {
		if e.watch.lastSummaryTotal != 0 {
			e.watch.lastSummaryTotal = 0
		}

		return
	}

	if len(issues) == e.watch.lastSummaryTotal {
		return
	}

	e.watch.lastSummaryTotal = len(issues)

	counts := make(map[string]int)
	for i := range issues {
		counts[issues[i].IssueType]++
	}

	parts := make([]string, 0, len(counts))
	for typ, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, typ))
	}

	sort.Strings(parts)

	e.logger.Warn("actionable issues",
		slog.Int("total", len(issues)),
		slog.String("breakdown", strings.Join(parts, ", ")),
	)
}

// recordSkippedItems records observation-time rejections (invalid names,
// path too long, file too large) as actionable failures in sync_failures.
func (e *Engine) recordSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
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

			e.logger.Warn("observation filter: skipped files",
				slog.String("issue_type", reason),
				slog.Int("count", len(items)),
				slog.Any("sample_paths", samples),
			)
		} else {
			for i := range items {
				e.logger.Warn("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		}

		failures := make([]synctypes.ActionableFailure, len(items))
		for i := range items {
			failures[i] = synctypes.ActionableFailure{
				Path:      items[i].Path,
				DriveID:   e.driveID,
				Direction: synctypes.DirectionUpload,
				IssueType: reason,
				Error:     items[i].Detail,
				FileSize:  items[i].FileSize,
			}
		}

		if err := e.baseline.UpsertActionableFailures(ctx, failures); err != nil {
			e.logger.Error("failed to record skipped items",
				slog.String("issue_type", reason),
				slog.Int("count", len(failures)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// clearResolvedSkippedItems removes sync_failures entries for scanner-detectable
// issue types that are no longer present in the current scan.
func (e *Engine) clearResolvedSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	currentByType := make(map[string][]string)
	for i := range skipped {
		currentByType[skipped[i].Reason] = append(currentByType[skipped[i].Reason], skipped[i].Path)
	}

	scannerIssueTypes := []string{
		synctypes.IssueInvalidFilename, synctypes.IssuePathTooLong,
		synctypes.IssueFileTooLarge, synctypes.IssueCaseCollision,
	}
	for _, issueType := range scannerIssueTypes {
		paths := currentByType[issueType]
		if err := e.baseline.ClearResolvedActionableFailures(ctx, issueType, paths); err != nil {
			e.logger.Error("failed to clear resolved failures",
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
func (e *Engine) runFullReconciliationAsync(ctx context.Context, bl *synctypes.Baseline) {
	if e.watch == nil {
		return
	}

	if e.watch.reconcileActive {
		e.logger.Info("full reconciliation skipped — previous still running")
		return
	}
	e.watch.reconcileActive = true

	go func() {
		result := reconcileResult{}

		start := time.Now()
		e.logger.Info("periodic full reconciliation starting")

		events, deltaToken, err := e.observeRemoteFull(ctx, bl)
		if err != nil {
			if ctx.Err() == nil {
				e.logger.Error("full reconciliation failed",
					slog.String("error", err.Error()),
				)
			}
			e.finishFullReconciliation(ctx, result)
			return
		}

		observed := changeEventsToObservedItems(e.logger, events)
		if commitErr := e.baseline.CommitObservation(
			ctx, observed, deltaToken, e.driveID,
		); commitErr != nil {
			e.logger.Error("failed to commit full reconciliation observations",
				slog.String("error", commitErr.Error()),
			)
			e.finishFullReconciliation(ctx, result)
			return
		}

		if e.watch.afterReconcileCommit != nil {
			e.watch.afterReconcileCommit()
		}

		if ctx.Err() != nil {
			e.logger.Info("full reconciliation: observations committed, stopping for shutdown")
			e.finishFullReconciliation(ctx, result)
			return
		}

		events = filterOutShortcuts(events)

		shortcutEvents, scErr := e.reconcileShortcutScopes(ctx, bl)
		if scErr != nil {
			e.logger.Warn("shortcut reconciliation failed during full reconciliation",
				slog.String("error", scErr.Error()),
			)
		}

		events = append(events, shortcutEvents...)

		if len(events) == 0 {
			e.logger.Info("periodic full reconciliation complete: no changes",
				slog.Duration("duration", time.Since(start)),
			)
			e.finishFullReconciliation(ctx, result)
			return
		}
		result.events = events
		if refreshed, refreshErr := e.baseline.ListShortcuts(ctx); refreshErr == nil {
			result.shortcuts = refreshed
		}

		e.logger.Info("periodic full reconciliation complete",
			slog.Int("events", len(events)),
			slog.Duration("duration", time.Since(start)),
		)

		e.finishFullReconciliation(ctx, result)
	}()
}

func (e *Engine) finishFullReconciliation(ctx context.Context, result reconcileResult) {
	if e.watch == nil {
		return
	}

	select {
	case e.watch.reconcileResults <- result:
	case <-ctx.Done():
		select {
		case e.watch.reconcileResults <- result:
		default:
		}
	}
}

func (e *Engine) applyReconcileResult(result reconcileResult) {
	if e.watch == nil {
		return
	}

	e.watch.reconcileActive = false

	if len(result.shortcuts) > 0 {
		e.setShortcuts(result.shortcuts)
	}

	for i := range result.events {
		e.watch.buf.Add(&result.events[i])
	}
}
