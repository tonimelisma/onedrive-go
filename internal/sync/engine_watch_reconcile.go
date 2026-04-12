package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
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
// changes (e.g., `resolve deletes`) and logs a watch summary.
func (rt *watchRuntime) handleRecheckTick(ctx context.Context) {
	if rt.externalDBChanged(ctx) {
		rt.handleExternalChanges(ctx)
	}

	rt.logWatchSummary(ctx)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRecheckTickHandled})
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version. Approved held deletes release the rolling counter; the
// engine consumes the approved rows through user-intent dispatch.
func (rt *watchRuntime) handleExternalChanges(ctx context.Context) {
	if rt.deleteCounter != nil && rt.deleteCounter.IsHeld() {
		rows, err := rt.engine.baseline.ListHeldDeletesByState(ctx, HeldDeleteStateHeld)
		if err != nil {
			rt.engine.logger.Warn("failed to check held-delete entries",
				slog.String("error", err.Error()),
			)

			return
		}

		if len(rows) == 0 {
			rt.deleteCounter.Release()
			rt.engine.logger.Info("delete safety threshold cleared by user")
		}
	}

	rt.clearResolvedPermissionScopes(ctx)
	rt.mustAssertInvariants(ctx, rt, "handle external changes")
}

// clearResolvedPermissionScopes checks if any permission scope blocks have had
// their sync_failures cleared (by user action via CLI), and releases the
// corresponding scope blocks.
func (rt *watchRuntime) clearResolvedPermissionScopes(ctx context.Context) {
	scopeKeys := rt.scopeController().scopeBlockKeys(rt)
	if len(scopeKeys) == 0 {
		return
	}

	localIssues, err := rt.engine.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
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

	activeScopes := make(map[ScopeKey]bool, len(localIssues)+len(remoteIssues))
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

func (rt *watchRuntime) logRemoteBlockedChanges(groups []VisibleIssueGroup) {
	current := make(map[ScopeKey]string, len(groups))

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

func summarySignature(summary IssueSummary) (string, string) {
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
func (flow *engineFlow) recordSkippedItems(ctx context.Context, skipped []SkippedItem) {
	eng := flow.engine

	if len(skipped) == 0 {
		return
	}

	byReason := make(map[string][]SkippedItem)
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
			// Keep full per-path visibility at Debug while avoiding a warning
			// storm once a single scanner issue fans out across many files.
			for i := range items {
				eng.logger.Debug("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		} else {
			for i := range items {
				eng.logger.Warn("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		}

		failures := make([]ActionableFailure, len(items))
		for i := range items {
			failures[i] = ActionableFailure{
				Path:       items[i].Path,
				DriveID:    eng.driveID,
				Direction:  DirectionUpload,
				ActionType: ActionUpload,
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
func (flow *engineFlow) clearResolvedSkippedItems(ctx context.Context, skipped []SkippedItem) {
	eng := flow.engine

	currentByType := make(map[string][]string)
	for i := range skipped {
		currentByType[skipped[i].Reason] = append(currentByType[skipped[i].Reason], skipped[i].Path)
	}

	scannerIssueTypes := []string{
		IssueInvalidFilename, IssuePathTooLong,
		IssueFileTooLarge, IssueCaseCollision,
		IssueHashPanic,
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
func (e *Engine) resolveReconcileInterval(opts WatchOptions) time.Duration {
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
func (e *Engine) initReconcileTicker(opts WatchOptions) syncTicker {
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
func (rt *watchRuntime) runFullReconciliationAsync(ctx context.Context, bl *Baseline) {
	if rt.reconcileActive {
		rt.engine.logger.Info("full reconciliation skipped — previous still running")
		return
	}
	rt.reconcileActive = true
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileStarted})

	go func() {
		rt.finishFullReconciliation(ctx, rt.performFullReconciliation(ctx, bl))
	}()
}

func (rt *watchRuntime) performFullReconciliation(
	ctx context.Context,
	bl *Baseline,
) reconcileResult {
	result := reconcileResult{}
	start := rt.engine.nowFunc()
	defer func() {
		rt.engine.collector().RecordReconcile(len(result.events), rt.engine.since(start))
	}()

	rt.engine.logger.Info("periodic full reconciliation starting")

	scopeSnapshot := rt.currentScopeSnapshot()
	scopeGeneration := rt.currentScopeGeneration()
	plan, err := rt.buildFullReconciliationPlan(ctx, scopeSnapshot, scopeGeneration)
	if err != nil {
		if ctx.Err() == nil {
			rt.engine.logger.Error("full reconciliation planning failed",
				slog.String("error", err.Error()),
			)
		}
		return result
	}

	scopedPrimary, err := rt.observeCommittedFullReconciliationBatch(ctx, bl, &plan, scopeSnapshot, scopeGeneration)
	if err != nil {
		if ctx.Err() == nil {
			rt.engine.logger.Error("full reconciliation failed",
				slog.String("error", err.Error()),
			)
		}
		return result
	}

	if ctx.Err() != nil {
		rt.engine.logger.Info("full reconciliation: observations committed, stopping for shutdown")
		return result
	}

	events, shortcutErr := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		scopedPrimary.emitted,
		scopeSnapshot,
		scopeGeneration,
		false,
		true,
	)
	if shortcutErr != nil {
		rt.engine.logger.Warn("shortcut reconciliation failed during full reconciliation",
			slog.String("error", shortcutErr.Error()),
		)
		events = filterOutShortcuts(scopedPrimary.emitted)
	}
	if len(events) == 0 {
		rt.engine.logger.Info("periodic full reconciliation complete: no changes",
			slog.Duration("duration", rt.engine.since(start)),
		)
		return result
	}

	result.events = events
	if refreshed, refreshErr := rt.shortcutCoordinator().loadShortcutSnapshot(ctx); refreshErr == nil {
		result.shortcuts = refreshed
	}

	rt.engine.logger.Info("periodic full reconciliation complete",
		slog.Int("events", len(events)),
		slog.Duration("duration", rt.engine.since(start)),
	)

	return result
}

func (rt *watchRuntime) buildFullReconciliationPlan(
	ctx context.Context,
	scopeSnapshot syncscope.Snapshot,
	scopeGeneration int64,
) (ObservationSessionPlan, error) {
	session := ScopeSession{
		Current:    scopeSnapshot,
		Generation: scopeGeneration,
	}

	return rt.BuildObservationSessionPlan(ctx, ObservationPlanRequest{
		Session:       &session,
		SyncMode:      SyncBidirectional,
		FullReconcile: true,
		Purpose:       observationPlanPurposeWatch,
	})
}

func (rt *watchRuntime) observeCommittedFullReconciliationBatch(
	ctx context.Context,
	bl *Baseline,
	plan *ObservationSessionPlan,
	scopeSnapshot syncscope.Snapshot,
	scopeGeneration int64,
) (remoteScopeResult, error) {
	fetchResult, err := rt.observeObservationPhase(ctx, bl, plan.PrimaryPhase, true)
	if err != nil {
		return remoteScopeResult{}, err
	}

	scopedPrimary := applyRemoteScope(rt.engine.logger, scopeSnapshot, scopeGeneration, fetchResult.events)
	if commitErr := rt.commitObservedItems(ctx, scopedPrimary.observed, ""); commitErr != nil {
		return remoteScopeResult{}, fmt.Errorf("commit full reconciliation observations: %w", commitErr)
	}
	if tokenErr := rt.commitDeferredDeltaTokens(ctx, fetchResult.deferred); tokenErr != nil {
		return remoteScopeResult{}, fmt.Errorf("commit full reconciliation delta tokens: %w", tokenErr)
	}

	if rt.afterReconcileCommit != nil {
		rt.afterReconcileCommit()
	}

	return scopedPrimary, nil
}

func (rt *watchRuntime) runEnteredScopeReconciliationAsync(
	ctx context.Context,
	bl *Baseline,
	enteredPaths []string,
) {
	if rt.reconcileActive {
		rt.engine.logger.Info("scope re-entry reconciliation skipped — previous still running")
		return
	}
	rt.reconcileActive = true
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileStarted})

	go func() {
		result := reconcileResult{}

		fetchResult, err := rt.reconcilePrimaryScopeEntries(ctx, bl, enteredPaths, nil)
		if err != nil {
			if ctx.Err() == nil {
				rt.engine.logger.Error("scope re-entry reconciliation failed",
					slog.String("error", err.Error()),
				)
			}
			rt.finishFullReconciliation(ctx, result)
			return
		}

		scopeSnapshot := rt.currentScopeSnapshot()
		scoped := applyRemoteScope(rt.engine.logger, scopeSnapshot, rt.currentScopeGeneration(), fetchResult.events)
		if commitErr := rt.commitObservedItems(ctx, scoped.observed, ""); commitErr != nil {
			rt.engine.logger.Error("failed to commit scope re-entry observations",
				slog.String("error", commitErr.Error()),
			)
			rt.finishFullReconciliation(ctx, result)
			return
		}
		if tokenErr := rt.commitDeferredDeltaTokens(ctx, fetchResult.deferred); tokenErr != nil {
			rt.engine.logger.Error("failed to commit scope re-entry delta tokens",
				slog.String("error", tokenErr.Error()),
			)
			rt.finishFullReconciliation(ctx, result)
			return
		}

		finalEvents, shortcutErr := rt.processCommittedPrimaryBatch(
			ctx,
			bl,
			scoped.emitted,
			scopeSnapshot,
			rt.currentScopeGeneration(),
			false,
			false,
		)
		if shortcutErr != nil {
			rt.engine.logger.Warn("shortcut processing failed during scope re-entry reconciliation",
				slog.String("error", shortcutErr.Error()),
			)
			finalEvents = filterOutShortcuts(scoped.emitted)
		}

		result.events = finalEvents
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

func (rt *watchRuntime) applyReconcileResult(ctx context.Context, result reconcileResult) {
	rt.reconcileActive = false

	if len(result.shortcuts) > 0 {
		rt.setShortcuts(result.shortcuts)
	}

	for i := range result.events {
		rt.buf.Add(&result.events[i])
	}

	session := ScopeSession{
		Current:    rt.currentScopeSnapshot(),
		Generation: rt.currentScopeGeneration(),
	}
	plan, err := rt.BuildObservationSessionPlan(ctx, ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	if err != nil {
		rt.engine.logger.Warn("failed to rebuild scope plan after reconciliation",
			slog.String("error", err.Error()),
		)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileApplied})
		return
	}
	plan.Reentry.Pending = false

	if err := rt.applyScopeState(ctx, false, &session, &plan); err != nil {
		rt.engine.logger.Warn("failed to persist scope state after reconciliation",
			slog.String("error", err.Error()),
		)
	}

	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileApplied})
}

func (rt *watchRuntime) dropReconcileResultOnShutdown() {
	rt.reconcileActive = false
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileDroppedOnShutdown})
}
