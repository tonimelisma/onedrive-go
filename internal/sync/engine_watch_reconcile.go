package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
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
// changes and logs a watch summary.
func (rt *watchRuntime) handleRecheckTick(ctx context.Context) {
	if rt.externalDBChanged(ctx) {
		rt.handleExternalChanges(ctx)
	}

	rt.logWatchSummary(ctx)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRecheckTickHandled})
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version.
func (rt *watchRuntime) handleExternalChanges(ctx context.Context) {
	rt.clearResolvedPermissionScopes(ctx)
	rt.mustAssertInvariants(ctx, rt, "handle external changes")
}

// clearResolvedPermissionScopes checks if any permission scope blocks have had
// their sync_failures cleared externally and releases the corresponding scope
// blocks.
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
// watch loop, and the loop feeds the returned events into its buffer from its
// own goroutine.
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

	plan, err := rt.buildFullReconciliationPlan(ctx)
	if err != nil {
		if ctx.Err() == nil {
			rt.engine.logger.Error("full reconciliation planning failed",
				slog.String("error", err.Error()),
			)
		}
		return result
	}

	projectedPrimary, err := rt.observeCommittedFullReconciliationBatch(ctx, bl, &plan)
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

	events := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		projectedPrimary.emitted,
		false,
		true,
	)
	if len(events) == 0 {
		rt.engine.logger.Info("periodic full reconciliation complete: no changes",
			slog.Duration("duration", rt.engine.since(start)),
		)
		return result
	}

	result.events = events

	rt.engine.logger.Info("periodic full reconciliation complete",
		slog.Int("events", len(events)),
		slog.Duration("duration", rt.engine.since(start)),
	)

	return result
}

func (rt *watchRuntime) buildFullReconciliationPlan(ctx context.Context) (ObservationSessionPlan, error) {
	session := ObservationSession{Generation: 1}

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
) (remoteObservationResult, error) {
	fetchResult, err := rt.observeObservationPhase(ctx, bl, plan.PrimaryPhase, true)
	if err != nil {
		return remoteObservationResult{}, err
	}

	projectedPrimary := projectRemoteObservations(rt.engine.logger, fetchResult.events)
	if commitErr := rt.commitObservedItems(ctx, projectedPrimary.observed, ""); commitErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full reconciliation observations: %w", commitErr)
	}
	if tokenErr := rt.commitDeferredDeltaTokens(ctx, fetchResult.deferred); tokenErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full reconciliation delta tokens: %w", tokenErr)
	}

	if rt.afterReconcileCommit != nil {
		rt.afterReconcileCommit()
	}

	return projectedPrimary, nil
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

	for i := range result.events {
		rt.buf.Add(&result.events[i])
	}

	session := ObservationSession{Generation: 1}
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

	if err := rt.applyObservationState(ctx, false, &session, &plan); err != nil {
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
