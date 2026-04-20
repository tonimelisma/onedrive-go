package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const (
	diskScopeInitialTrialInterval = 5 * time.Minute
	diskScopeMaxTrialInterval     = 1 * time.Hour
)

type scopeStartupPolicy int

const (
	scopeStartupRequiresScopedFailures scopeStartupPolicy = iota + 1
	scopeStartupServerTimedOnly
	scopeStartupRevalidateDisk
)

type persistedScopeFacts struct {
	blockedRetryCountByScope map[ScopeKey]int
}

// loadActiveScopes refreshes watch runtime scope state from the persisted
// block_scopes table. The store remains the restart/recovery record; watch
// mode keeps only the current working set in memory.
func (controller *scopeController) loadActiveScopes(ctx context.Context, watch *watchRuntime) error {
	flow := controller.flow

	if watch == nil {
		return nil
	}

	blocks, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	activeScopes := make([]ActiveScope, 0, len(blocks))
	for i := range blocks {
		activeScopes = append(activeScopes, activeScopeFromBlockScopeRow(blocks[i]))
	}
	watch.replaceActiveScopes(activeScopes)

	return nil
}

// normalizePersistedScopes normalizes non-permission scope rows against
// persisted retry evidence before any admission begins. Permission scopes keep
// their persisted state until the permission-maintenance boundary revalidates
// them after baseline load.
func (controller *scopeController) normalizePersistedScopes(
	ctx context.Context,
	watch *watchRuntime,
) error {
	flow := controller.flow

	blocks, listScopeErr := flow.engine.baseline.ListBlockScopes(ctx)
	if listScopeErr != nil {
		return fmt.Errorf("sync: listing block scopes: %w", listScopeErr)
	}

	blockedRetries, err := controller.loadNormalizedPersistedBlockedRetries(ctx)
	if err != nil {
		return err
	}
	if err := controller.normalizePersistedNonAuthScopes(ctx, blocks, blockedRetries); err != nil {
		return err
	}

	flow.mustAssertInvariants(ctx, watch, "normalize persisted scopes")

	return nil
}

func (controller *scopeController) loadNormalizedPersistedBlockedRetries(
	ctx context.Context,
) ([]RetryWorkRow, error) {
	flow := controller.flow

	rows, err := flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing blocked retry_work rows: %w", err)
	}

	return rows, nil
}

func (controller *scopeController) normalizePersistedNonAuthScopes(
	ctx context.Context,
	blocks []*BlockScope,
	blockedRetries []RetryWorkRow,
) error {
	facts := summarizePersistedBlockedRetries(blockedRetries)

	for i := range blocks {
		if blocks[i] == nil || blocks[i].Key.IsPermDir() || blocks[i].Key.IsPermRemote() {
			continue
		}
		if err := controller.normalizePersistedScope(ctx, blocks[i], facts); err != nil {
			return err
		}
	}

	return nil
}

func (controller *scopeController) normalizePersistedScope(
	ctx context.Context,
	block *BlockScope,
	facts persistedScopeFacts,
) error {
	blockedRetryCount := facts.blockedRetryCountByScope[block.Key]

	switch scopeStartupPolicyFor(block.Key) {
	case scopeStartupRequiresScopedFailures:
		if blockedRetryCount > 0 {
			return nil
		}
		return controller.discardStartupScope(ctx, block.Key, "discarded scope without blocked retry work")
	case scopeStartupServerTimedOnly:
		if blockedRetryCount == 0 {
			return controller.discardStartupScope(ctx, block.Key, "discarded scope without blocked retry work")
		}
		if block.TimingSource == ScopeTimingServerRetryAfter {
			return nil
		}
		return controller.releaseStartupScope(ctx, block.Key, "released non-server-timed restart scope")
	case scopeStartupRevalidateDisk:
		if blockedRetryCount == 0 {
			return controller.discardStartupScope(ctx, block.Key, "discarded disk scope without blocked retry work")
		}
		return controller.normalizeDiskScope(ctx, block)
	default:
		panic(fmt.Sprintf("unknown startup policy %d", scopeStartupPolicyFor(block.Key)))
	}
}

func (controller *scopeController) releaseStartupScope(ctx context.Context, key ScopeKey, note string) error {
	flow := controller.flow

	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeNormalized,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func (controller *scopeController) discardStartupScope(ctx context.Context, key ScopeKey, note string) error {
	flow := controller.flow

	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeNormalized,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func summarizePersistedBlockedRetries(rows []RetryWorkRow) persistedScopeFacts {
	facts := persistedScopeFacts{
		blockedRetryCountByScope: make(map[ScopeKey]int),
	}

	for i := range rows {
		if rows[i].ScopeKey.IsZero() || !rows[i].Blocked {
			continue
		}
		facts.blockedRetryCountByScope[rows[i].ScopeKey]++
	}

	return facts
}

func scopeStartupPolicyFor(key ScopeKey) scopeStartupPolicy {
	switch {
	case key == SKQuotaOwn():
		return scopeStartupRequiresScopedFailures
	case key.IsThrottleTarget(), key == SKService():
		return scopeStartupServerTimedOnly
	case key == SKDiskLocal():
		return scopeStartupRevalidateDisk
	default:
		return scopeStartupRequiresScopedFailures
	}
}

func (controller *scopeController) normalizeDiskScope(ctx context.Context, block *BlockScope) error {
	flow := controller.flow

	if flow.engine.minFreeSpace <= 0 {
		if err := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing disk scope %s with disabled min_free_space: %w", block.Key.String(), err)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeNormalized,
			ScopeKey: block.Key,
			Note:     "released disk scope with disabled min_free_space",
		})
		return nil
	}

	available, err := flow.engine.diskAvailableFn(flow.engine.syncRoot)
	if err != nil {
		flow.engine.logger.Warn("normalizePersistedScopes: disk revalidation failed, releasing stale disk scope",
			slog.String("scope_key", block.Key.String()),
			slog.String("error", err.Error()),
		)
		if releaseErr := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); releaseErr != nil {
			return fmt.Errorf("sync: releasing stale disk scope %s: %w", block.Key.String(), releaseErr)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeNormalized,
			ScopeKey: block.Key,
			Note:     "released disk scope after revalidation error",
		})
		return nil
	}

	if int64(available) >= flow.engine.minFreeSpace {
		if err := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing recovered disk scope %s: %w", block.Key.String(), err)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeNormalized,
			ScopeKey: block.Key,
			Note:     "released disk scope after healthy revalidation",
		})
		return nil
	}

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(block.Key, 0, 0)
	if err := flow.engine.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           block.Key,
		ConditionType: IssueDiskFull,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}); err != nil {
		return fmt.Errorf("sync: refreshing disk scope %s: %w", block.Key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeNormalized,
		ScopeKey: block.Key,
		Note:     "refreshed disk scope from current local truth",
	})

	return nil
}

func (controller *scopeController) activateScope(ctx context.Context, watch *watchRuntime, block *ActiveScope) error {
	flow := controller.flow

	if block == nil {
		return fmt.Errorf("sync: activating scope: missing block")
	}

	persisted, err := blockScopeRowFromActiveScope(*block)
	if err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if err := flow.engine.baseline.UpsertBlockScope(ctx, persisted); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if watch != nil {
		watch.upsertActiveScope(block)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertInvariants(ctx, watch, "activate scope")

	return nil
}

func (controller *scopeController) extendScopeTrial(
	ctx context.Context,
	watch *watchRuntime,
	scopeKey ScopeKey,
	retryAfter time.Duration,
) {
	flow := controller.flow

	if watch == nil {
		return
	}

	block, ok := watch.lookupActiveScope(scopeKey)
	if !ok {
		return
	}

	newInterval := computeTrialInterval(scopeKey, retryAfter, block.TrialInterval)
	nextAt := flow.engine.nowFunc().Add(newInterval)

	flow.engine.logger.Debug("extending trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
		slog.Duration("retry_after", retryAfter),
	)

	block.NextTrialAt = nextAt
	block.TrialInterval = newInterval
	block.TrialCount++
	block.TimingSource = scopeTimingSource(retryAfter)
	if err := controller.activateScope(ctx, watch, &block); err != nil {
		flow.engine.logger.Warn("extendScopeTrial: failed to persist interval extension",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	watch.armTrialTimer()
}

func (controller *scopeController) rearmScopeTrial(ctx context.Context, watch *watchRuntime, scopeKey ScopeKey) {
	flow := controller.flow

	if watch == nil {
		return
	}

	block, ok := watch.lookupActiveScope(scopeKey)
	if !ok {
		return
	}
	if block.TrialInterval <= 0 {
		return
	}

	block.NextTrialAt = flow.engine.nowFunc().Add(block.TrialInterval)
	if err := controller.activateScope(ctx, watch, &block); err != nil {
		flow.engine.logger.Warn("rearmScopeTrial: failed to persist rearmed interval",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Debug("rearming trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("interval", block.TrialInterval),
	)

	watch.armTrialTimer()
}

func (controller *scopeController) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	_, found, err := controller.flow.engine.baseline.PickRetryTrialCandidate(ctx, scopeKey)
	if err != nil {
		return false, fmt.Errorf("sync: checking blocked retry work for scope %s: %w", scopeKey.String(), err)
	}

	return found, nil
}

func (controller *scopeController) rearmOrDiscardScope(ctx context.Context, watch *watchRuntime, scopeKey ScopeKey) {
	if scopeKey.IsZero() {
		return
	}
	if scopeKey.IsPermDir() || scopeKey.IsPermRemote() {
		return
	}

	flow := controller.flow
	hasBlockedWork, err := controller.scopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil {
		flow.engine.logger.Warn("rearmOrDiscardScope: failed to check blocked retry work",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	if hasBlockedWork {
		controller.rearmScopeTrial(ctx, watch, scopeKey)
		return
	}

	if err := controller.discardScope(ctx, watch, scopeKey); err != nil {
		flow.engine.logger.Warn("rearmOrDiscardScope: failed to discard empty scope",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// computeTrialInterval is the single source of truth for trial interval
// computation (R-2.10.14). Both initial block scope creation and subsequent
// trial extensions use this function, preventing policy divergence.
//
//   - retryAfter > 0: server-provided value used directly, no cap (R-2.10.7)
//   - retryAfter == 0, currentInterval > 0: double current, cap at defaultMaxTrialInterval
//   - retryAfter == 0, currentInterval == 0: use defaultInitialTrialInterval
func computeTrialInterval(scopeKey ScopeKey, retryAfter, currentInterval time.Duration) time.Duration {
	initialInterval := DefaultInitialTrialInterval
	maxInterval := DefaultMaxTrialInterval
	if scopeKey == SKDiskLocal() {
		initialInterval = diskScopeInitialTrialInterval
		maxInterval = diskScopeMaxTrialInterval
	}

	if retryAfter > 0 {
		return retryAfter
	}
	if currentInterval > 0 {
		doubled := currentInterval * 2
		if doubled > maxInterval {
			return maxInterval
		}
		return doubled
	}
	return initialInterval
}

func scopeTimingSource(retryAfter time.Duration) ScopeTimingSource {
	if retryAfter > 0 {
		return ScopeTimingServerRetryAfter
	}

	return ScopeTimingBackoff
}

// isObservationSuppressed returns true if a global block scope is active,
// meaning all remote observation polling should be skipped to avoid wasting
// API calls. Target-scoped 429 blocks are handled separately.
func (controller *scopeController) isObservationSuppressed(watch *watchRuntime) bool {
	return watch != nil && watch.hasActiveScope(SKService())
}

// feedScopeDetection feeds an action completion into scope detection sliding
// windows. If a threshold is crossed, creates a block scope. Called directly
// from the normal processActionCompletion switch — never called for trial results
// (the scope is already blocked, and re-detecting would overwrite the doubled
// interval).
func (controller *scopeController) feedScopeDetection(ctx context.Context, watch *watchRuntime, r *ActionCompletion) {
	if watch == nil {
		return
	}

	// Local errors (HTTPStatus==0) must not feed scope detection windows.
	// Only remote API errors should increment service/quota counters (R-6.7.27).
	if r.HTTPStatus == 0 {
		return
	}

	sr := watch.scopeState.UpdateScope(r)
	if sr.Block {
		controller.applyBlockScope(ctx, watch, sr)
	}
}

// applyBlockScope persists and activates a new block scope. Uses
// computeTrialInterval for the initial interval, ensuring the same
// Retry-After-vs-backoff policy as extendScopeTrial.
func (controller *scopeController) applyBlockScope(ctx context.Context, watch *watchRuntime, sr ScopeUpdateResult) {
	flow := controller.flow

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(sr.ScopeKey, sr.RetryAfter, 0)

	block := &ActiveScope{
		Key:           sr.ScopeKey,
		TimingSource:  scopeTimingSource(sr.RetryAfter),
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}
	if err := controller.activateScope(ctx, watch, block); err != nil {
		flow.engine.logger.Warn("applyBlockScope: failed to persist block scope",
			slog.String("scope_key", sr.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Warn("block scope active — actions blocked",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("condition_type", sr.ConditionType),
		slog.Duration("trial_interval", interval),
	)

	if watch != nil {
		watch.armTrialTimer() // arm so the first trial fires at NextTrialAt (R-2.10.5)
	}
}

// releaseScope atomically removes the block scope and makes blocked retry work
// under that scope eligible to run again.
func (controller *scopeController) releaseScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	flow := controller.flow

	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.removeActiveScope(key)
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
		watch.kickRetrySweepNow()
		watch.armTrialTimer()
	} else {
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
	}

	flow.engine.logger.Info("block scope cleared — blocked work released",
		slog.String("scope_key", key.String()),
	)

	flow.mustAssertReleasedScope(ctx, watch, key, "release scope")
	flow.mustAssertInvariants(ctx, watch, "release scope")

	return nil
}

// discardScope atomically removes the block scope and deletes blocked retry
// work tied to it. Used when the blocked subtree itself disappears.
func (controller *scopeController) discardScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	flow := controller.flow

	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.removeActiveScope(key)
		watch.armTrialTimer()
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	flow.mustAssertDiscardedScope(ctx, watch, key, "discard scope")
	flow.mustAssertInvariants(ctx, watch, "discard scope")

	return nil
}

func (controller *scopeController) clearBlockedRetryWork(
	ctx context.Context,
	row *RetryWorkRow,
	caller string,
) {
	if row == nil {
		return
	}

	work := retryWorkKeyForRetryWork(row)

	if err := controller.flow.engine.baseline.ClearBlockedRetryWork(ctx, work, row.ScopeKey); err != nil {
		controller.flow.engine.logger.Debug(caller+": failed to clear blocked retry work",
			slog.String("path", row.Path),
			slog.String("scope_key", row.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// admitReady applies watch-mode trial interception and scope admission to a
// ready action set, returning the actions that should enter the watch loop's
// outbox. It is the single admission path used by both newly-planned actions
// and newly-ready dependents from result processing.
func (controller *scopeController) admitReady(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
) []*TrackedAction {
	flow := controller.flow

	var dispatch []*TrackedAction

	for _, ta := range ready {
		if ta.IsTrial {
			if ta.TrialScopeKey.BlocksAction(ta.Action.Path,
				ta.Action.ThrottleTargetKey(), ta.Action.Type) {
				dispatch = append(dispatch, ta)
			} else {
				// Trial candidate no longer matches scope — clear stale failure,
				// run normal admission. Best-effort: log on error, don't abort.
				controller.clearBlockedRetryWorkForScope(ctx, retryWorkKeyForAction(&ta.Action), ta.TrialScopeKey)

				if key := watch.findBlockingScope(ta); key.IsZero() {
					flow.setDispatch(ctx, &ta.Action)
					dispatch = append(dispatch, ta)
				}
				if watch != nil {
					watch.armTrialTimer()
				}
			}

			continue
		}

		// Normal scope admission.
		if watch != nil {
			if key := watch.findBlockingScope(ta); !key.IsZero() {
				controller.cascadeRecordAndComplete(ctx, ta, key)
				continue
			}
		}

		flow.setDispatch(ctx, &ta.Action)
		dispatch = append(dispatch, ta)
	}

	return dispatch
}

// cascadeRecordAndComplete records a scope-blocked action and all its
// transitive dependents as blocked retry_work, completing each in the graph.
// Uses BFS to traverse the dependency tree. Each dependent inherits the
// parent's scope_key (section 3.4).
//
// Safe for concurrent use — depGraph.Complete uses a mutex. Two cascades
// from different goroutines cannot return the same dependent (depsLeft is
// atomic — the last parent to complete returns the dependent).
func (controller *scopeController) cascadeRecordAndComplete(
	ctx context.Context,
	ta *TrackedAction,
	scopeKey ScopeKey,
) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := []*TrackedAction{ta}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		controller.recordBlockedRetryWork(ctx, &current.Action, scopeKey)
		// No resetDispatchStatus — setDispatch was never called for blocked
		// actions (active-scope admission runs BEFORE setDispatch, per section 2.2).
		ready := flow.completeDepGraphAction(current.ID, "cascadeRecordAndComplete")
		queue = append(queue, ready...)
	}
}

// cascadeFailAndComplete records each transitive dependent as a cascade
// failure and completes it in the DepGraph. BFS ensures grandchildren are
// not stranded. Used for worker failures (non-scope-related).
func (controller *scopeController) cascadeFailAndComplete(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
	r *ActionCompletion,
) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		controller.recordCascadeRetryWork(ctx, watch, &current.Action, r)
		next := flow.completeDepGraphAction(current.ID, "cascadeFailAndComplete")
		queue = append(queue, next...)
	}
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown (context canceled — not a failure).
func (controller *scopeController) completeSubtree(ready []*TrackedAction) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next := flow.completeDepGraphAction(current.ID, "completeSubtree")
		queue = append(queue, next...)
	}
}

// recordBlockedRetryWork records retry_work for an action that is currently
// blocked by an active scope. Blocked rows have no retry timing until the
// scope is released or trialed.
func (controller *scopeController) recordBlockedRetryWork(ctx context.Context, action *Action, scopeKey ScopeKey) {
	flow := controller.flow

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          action.Path,
		OldPath:       action.OldPath,
		ActionType:    action.Type,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		Blocked:       true,
	}, nil); err != nil {
		flow.engine.logger.Warn("failed to record blocked retry_work",
			slog.String("path", action.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

func (controller *scopeController) rehomeBlockedRetryWork(
	ctx context.Context,
	r *ActionCompletion,
	scopeKey ScopeKey,
) {
	flow := controller.flow

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          r.Path,
		OldPath:       r.OldPath,
		ActionType:    r.ActionType,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		HTTPStatus:    r.HTTPStatus,
		Blocked:       true,
	}, nil); err != nil {
		flow.engine.logger.Warn("failed to rehome blocked retry_work",
			slog.String("path", r.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// recordCascadeRetryWork records retry_work for a dependent whose parent failed.
// Dependents inherit the parent's issue classification and scope evidence, but
// keep their own path and action identity.
func (controller *scopeController) recordCascadeRetryWork(
	ctx context.Context,
	watch *watchRuntime,
	action *Action,
	parentResult *ActionCompletion,
) {
	flow := controller.flow

	parentDecision := classifyResult(parentResult)
	scopeKey := parentDecision.ScopeEvidence
	blocked := flow.retryWorkShouldBeBlocked(watch, parentDecision.Class, scopeKey)

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          action.Path,
		OldPath:       action.OldPath,
		ActionType:    action.Type,
		ConditionType: parentDecision.ConditionType,
		ScopeKey:      scopeKey,
		LastError:     "parent action failed: " + parentResult.ErrMsg,
		HTTPStatus:    parentResult.HTTPStatus,
		Blocked:       blocked,
	}, retry.ReconcilePolicy().Delay); err != nil {
		flow.engine.logger.Warn("failed to record cascade retry_work",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}
