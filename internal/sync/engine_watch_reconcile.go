package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// clearResolvedPermissionScopes checks whether persisted permission scope
// authorities still exist and releases any runtime scope whose backing
// block_scopes / blocked retry_work rows disappeared externally.
func (rt *watchRuntime) clearResolvedPermissionScopes(ctx context.Context) {
	scopeKeys := rt.scopeController().blockScopeKeys(rt)
	if len(scopeKeys) == 0 {
		return
	}

	blocks, err := rt.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to check persisted block scopes",
			slog.String("error", err.Error()),
		)

		return
	}

	activeScopes := make(map[ScopeKey]bool, len(blocks))
	for i := range blocks {
		if blocks[i] != nil && (blocks[i].Key.IsPermDir() || blocks[i].Key.IsPermRemote()) {
			activeScopes[blocks[i].Key] = true
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

			rt.engine.logger.Info("permission block scope cleared by user",
				slog.String("scope", key.String()),
			)
		}
	}
}

// logWatchSummary logs a periodic one-liner summary of active sync conditions
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
func (rt *watchRuntime) logWatchSummary(ctx context.Context) {
	snapshot, err := rt.engine.baseline.ReadDriveStatusSnapshot(ctx)
	if err != nil {
		return
	}
	summary, groups := buildWatchConditionSummary(&snapshot)
	rt.logRemoteBlockedChanges(groups)

	totalConditions := summary.VisibleTotal()
	if totalConditions == 0 {
		if rt.lastSummaryTotal != 0 || rt.lastSummarySignature != "" {
			rt.engine.logger.Info("sync conditions cleared")
		}
		rt.lastSummaryTotal = 0
		rt.lastSummarySignature = ""
		return
	}

	signature, breakdown := summarySignature(summary)
	if signature == rt.lastSummarySignature {
		return
	}

	rt.lastSummaryTotal = totalConditions
	rt.lastSummarySignature = signature

	rt.engine.logger.Warn("sync conditions",
		slog.Int("total", totalConditions),
		slog.String("breakdown", breakdown),
	)
}

func (rt *watchRuntime) logRemoteBlockedChanges(groups []watchRemoteBlockedGroup) {
	current := make(map[ScopeKey]string, len(groups))

	for i := range groups {
		group := groups[i]
		if !group.ScopeKey.IsPermRemoteWrite() {
			continue
		}

		signature := strings.Join(group.BlockedPaths, "\x00")
		current[group.ScopeKey] = signature

		switch previous, ok := rt.lastRemoteBlocked[group.ScopeKey]; {
		case !ok:
			rt.engine.logger.Warn("shared-folder writes blocked",
				slog.String("boundary", group.BoundaryPath),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		case previous != signature:
			rt.engine.logger.Warn("shared-folder writes still blocked",
				slog.String("boundary", group.BoundaryPath),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
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

func summarySignature(summary ConditionSummary) (string, string) {
	parts := make([]string, 0, len(summary.Groups))
	for i := range summary.Groups {
		parts = append(parts, fmt.Sprintf("%d %s", summary.Groups[i].Count, summary.Groups[i].Key))
	}
	sort.Strings(parts)
	breakdown := strings.Join(parts, ", ")
	return fmt.Sprintf("%d|%s", summary.VisibleTotal(), breakdown), breakdown
}

func (flow *engineFlow) reconcileSkippedObservationFindings(
	ctx context.Context,
	watch *watchRuntime,
	skipped []SkippedItem,
) {
	eng := flow.engine

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
	}

	batch := observationFindingsBatchFromSkippedItems(eng.driveID, skipped)
	flow.reconcileObservationFindingsBatch(ctx, watch, batch, "failed to reconcile local observation findings")
}

func observationFindingsBatchFromSkippedItems(
	driveID driveid.ID,
	skipped []SkippedItem,
) ObservationFindingsBatch {
	batch := ObservationFindingsBatch{
		Issues: make([]ObservationIssue, 0, len(skipped)),
		ManagedIssueTypes: []string{
			IssueInvalidFilename,
			IssuePathTooLong,
			IssueFileTooLarge,
			IssueCaseCollision,
			IssueLocalReadDenied,
			IssueHashPanic,
		},
		ManagedReadScopeKinds: []ScopeKeyKind{ScopePermDirRead},
	}

	for i := range skipped {
		item := skipped[i]
		if item.Reason == "" || item.Path == "" {
			continue
		}

		issue := ObservationIssue{
			Path:       item.Path,
			DriveID:    driveID,
			ActionType: ActionUpload,
			IssueType:  item.Reason,
			Error:      item.Detail,
			FileSize:   item.FileSize,
		}
		if item.Reason == IssueLocalReadDenied && item.BlocksReadScope {
			issue.ScopeKey = SKPermLocalRead(item.Path)
			batch.ReadScopes = append(batch.ReadScopes, issue.ScopeKey)
		}

		batch.Issues = append(batch.Issues, issue)
	}

	return batch
}

func (flow *engineFlow) reconcileObservationFindingsBatch(
	ctx context.Context,
	watch *watchRuntime,
	batch ObservationFindingsBatch,
	failureMessage string,
) {
	eng := flow.engine

	if err := eng.baseline.ReconcileObservationFindings(ctx, batch, eng.nowFunc()); err != nil {
		eng.logger.Error(failureMessage,
			slog.Int("issues", len(batch.Issues)),
			slog.Int("read_scopes", len(batch.ReadScopes)),
			slog.String("error", err.Error()),
		)
		return
	}

	if watch != nil {
		if err := flow.scopeController().loadActiveScopes(ctx, watch); err != nil {
			eng.logger.Warn("failed to refresh watch scopes after observation reconcile",
				slog.String("error", err.Error()),
			)
		}
	}
}

func (e *Engine) fullRemoteReconcileDelay(ctx context.Context) (time.Duration, error) {
	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return 0, fmt.Errorf("sync: reading observation state for reconcile cadence: %w", err)
	}
	if state.NextFullRemoteRefreshAt == 0 {
		if state.LastFullRemoteRefreshAt == 0 {
			return 0, nil
		}
		state.NextFullRemoteRefreshAt = time.Unix(0, state.LastFullRemoteRefreshAt).
			Add(remoteRefreshIntervalForMode(state.RemoteRefreshMode)).
			UnixNano()
	}

	dueAt := time.Unix(0, state.NextFullRemoteRefreshAt)
	delay := dueAt.Sub(e.nowFunc())
	if delay < 0 {
		return 0, nil
	}

	return delay, nil
}

func (e *Engine) shouldRunFullRemoteReconcile(ctx context.Context, requested bool) (bool, error) {
	if requested {
		return true, nil
	}

	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: reading observation state for full reconcile: %w", err)
	}
	if state.Cursor == "" || state.NextFullRemoteRefreshAt == 0 {
		return true, nil
	}

	dueAt := time.Unix(0, state.NextFullRemoteRefreshAt)
	return !e.nowFunc().Before(dueAt), nil
}

func (rt *watchRuntime) armFullReconcileTimer(ctx context.Context) error {
	delay, err := rt.engine.fullRemoteReconcileDelay(ctx)
	if err != nil {
		return err
	}
	state, err := rt.engine.baseline.ReadObservationState(ctx)
	if err != nil {
		return fmt.Errorf("sync: reading observation state for reconcile timer: %w", err)
	}
	interval := remoteRefreshIntervalForMode(state.RemoteRefreshMode)

	rt.resetReconcileTimer(rt.engine.afterFunc(delay, func() {
		select {
		case rt.reconcileCh <- rt.engine.nowFunc():
		default:
		}
	}))

	rt.engine.logger.Info("full remote reconciliation armed",
		slog.Duration("delay", delay),
		slog.Duration("interval", interval),
	)

	return nil
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

	plan := rt.buildPrimaryRootObservationPlan(true)
	projectedPrimary, err := rt.observeCommittedFullReconciliationBatch(ctx, bl, plan)
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

func (rt *watchRuntime) observeCommittedFullReconciliationBatch(
	ctx context.Context,
	bl *Baseline,
	plan primaryRootObservationPlan,
) (remoteObservationResult, error) {
	fetchResult, err := rt.executePrimaryRootObservation(ctx, bl, plan)
	if err != nil {
		return remoteObservationResult{}, err
	}

	projectedPrimary := projectRemoteObservations(rt.engine.logger, fetchResult.events)
	if commitErr := rt.commitObservedItems(ctx, projectedPrimary.observed, ""); commitErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full reconciliation observations: %w", commitErr)
	}
	if tokenErr := rt.commitPendingPrimaryCursor(ctx, fetchResult.pending); tokenErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full reconciliation primary cursor: %w", tokenErr)
	}
	if armErr := rt.armFullReconcileTimer(ctx); armErr != nil {
		return remoteObservationResult{}, fmt.Errorf("arm full reconciliation timer: %w", armErr)
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

func (rt *watchRuntime) applyReconcileResult(result reconcileResult) {
	rt.reconcileActive = false

	if rt.dirtyBuf != nil {
		if len(result.events) == 0 {
			rt.dirtyBuf.MarkFullRefresh()
		}
		for i := range result.events {
			if result.events[i].Path != "" {
				rt.dirtyBuf.MarkPath(result.events[i].Path)
			}
			if result.events[i].OldPath != "" {
				rt.dirtyBuf.MarkPath(result.events[i].OldPath)
			}
		}
	}

	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileApplied})
}

func (rt *watchRuntime) dropReconcileResultOnShutdown() {
	rt.reconcileActive = false
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileDroppedOnShutdown})
}
