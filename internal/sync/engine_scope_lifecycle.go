package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	diskScopeInitialTrialInterval = 5 * time.Minute
	diskScopeMaxTrialInterval     = 1 * time.Hour
)

type scopeStartupPolicy int

const (
	scopeStartupRequiresBoundary scopeStartupPolicy = iota + 1
	scopeStartupRequiresScopedFailures
	scopeStartupServerTimedOnly
	scopeStartupRevalidateDisk
)

type persistedScopeFacts struct {
	boundaryKeys        map[synctypes.ScopeKey]bool
	failureCountByScope map[synctypes.ScopeKey]int
}

// loadActiveScopes refreshes watch runtime scope state from the persisted
// scope_blocks table. The store remains the restart/recovery record; watch
// mode keeps only the current working set in memory.
func (flow *engineFlow) loadActiveScopes(ctx context.Context, watch *watchRuntime) error {
	if watch == nil {
		return nil
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	watch.activeScopes = watch.activeScopes[:0]
	for i := range blocks {
		watch.activeScopes = append(watch.activeScopes, *blocks[i])
	}

	return nil
}

// repairPersistedScopes normalizes persisted scope rows against current store
// evidence before any admission begins. The store remains authoritative for
// restart state; watch mode loads activeScopes only after this repair pass.
func (flow *engineFlow) repairPersistedScopes(ctx context.Context, watch *watchRuntime) error {
	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing scope blocks: %w", err)
	}
	if len(blocks) == 0 {
		return nil
	}

	failures, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing sync failures: %w", err)
	}

	facts := summarizePersistedScopeFailures(failures)

	for i := range blocks {
		if err := flow.repairPersistedScope(ctx, blocks[i], facts); err != nil {
			return err
		}
	}

	flow.mustAssertScopeInvariants(ctx, watch, "repair persisted scopes")

	return nil
}

func (flow *engineFlow) repairPersistedScope(
	ctx context.Context,
	block *synctypes.ScopeBlock,
	facts persistedScopeFacts,
) error {
	switch scopeStartupPolicyFor(block.Key) {
	case scopeStartupRequiresBoundary:
		if facts.boundaryKeys[block.Key] {
			return nil
		}
		return flow.releaseStartupScope(ctx, block.Key, "released scope without boundary evidence")
	case scopeStartupRequiresScopedFailures:
		if facts.failureCountByScope[block.Key] > 0 {
			return nil
		}
		return flow.discardStartupScope(ctx, block.Key, "discarded scope without scoped failures")
	case scopeStartupServerTimedOnly:
		if block.TimingSource == synctypes.ScopeTimingServerRetryAfter {
			return nil
		}
		return flow.releaseStartupScope(ctx, block.Key, "released non-server-timed restart scope")
	case scopeStartupRevalidateDisk:
		return flow.repairDiskScope(ctx, block)
	default:
		panic(fmt.Sprintf("unknown startup policy %d", scopeStartupPolicyFor(block.Key)))
	}
}

func (flow *engineFlow) releaseStartupScope(ctx context.Context, key synctypes.ScopeKey, note string) error {
	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func (flow *engineFlow) discardStartupScope(ctx context.Context, key synctypes.ScopeKey, note string) error {
	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func summarizePersistedScopeFailures(rows []synctypes.SyncFailureRow) persistedScopeFacts {
	facts := persistedScopeFacts{
		boundaryKeys:        make(map[synctypes.ScopeKey]bool),
		failureCountByScope: make(map[synctypes.ScopeKey]int),
	}

	for i := range rows {
		if rows[i].ScopeKey.IsZero() {
			continue
		}
		facts.failureCountByScope[rows[i].ScopeKey]++
		if rows[i].Role == synctypes.FailureRoleBoundary {
			facts.boundaryKeys[rows[i].ScopeKey] = true
		}
	}

	return facts
}

func scopeStartupPolicyFor(key synctypes.ScopeKey) scopeStartupPolicy {
	switch {
	case key.IsPermDir(), key.IsPermRemote():
		return scopeStartupRequiresBoundary
	case key == synctypes.SKQuotaOwn() || key.Kind == synctypes.ScopeQuotaShortcut:
		return scopeStartupRequiresScopedFailures
	case key == synctypes.SKThrottleAccount() || key == synctypes.SKService():
		return scopeStartupServerTimedOnly
	case key == synctypes.SKDiskLocal():
		return scopeStartupRevalidateDisk
	default:
		return scopeStartupRequiresScopedFailures
	}
}

func isPermissionScopeKey(key synctypes.ScopeKey) bool {
	return key.IsPermDir() || key.IsPermRemote()
}

func (flow *engineFlow) repairDiskScope(ctx context.Context, block *synctypes.ScopeBlock) error {
	if flow.engine.minFreeSpace <= 0 {
		if err := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing disk scope %s with disabled min_free_space: %w", block.Key.String(), err)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope with disabled min_free_space",
		})
		return nil
	}

	available, err := flow.engine.diskAvailableFn(flow.engine.syncRoot)
	if err != nil {
		flow.engine.logger.Warn("repairPersistedScopes: disk revalidation failed, releasing stale disk scope",
			slog.String("scope_key", block.Key.String()),
			slog.String("error", err.Error()),
		)
		if releaseErr := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); releaseErr != nil {
			return fmt.Errorf("sync: releasing stale disk scope %s: %w", block.Key.String(), releaseErr)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
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
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope after healthy revalidation",
		})
		return nil
	}

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(block.Key, 0, 0)
	if err := flow.engine.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
		Key:           block.Key,
		IssueType:     synctypes.IssueDiskFull,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}); err != nil {
		return fmt.Errorf("sync: refreshing disk scope %s: %w", block.Key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: block.Key,
		Note:     "refreshed disk scope from current local truth",
	})

	return nil
}

func (flow *engineFlow) getScopeBlock(watch *watchRuntime, key synctypes.ScopeKey) (synctypes.ScopeBlock, bool) {
	if watch == nil {
		return synctypes.ScopeBlock{}, false
	}
	return syncdispatch.LookupScope(watch.activeScopes, key)
}

func (flow *engineFlow) isScopeBlocked(watch *watchRuntime, key synctypes.ScopeKey) bool {
	if watch == nil {
		return false
	}
	return syncdispatch.HasScope(watch.activeScopes, key)
}

func (flow *engineFlow) activeBlockingScope(watch *watchRuntime, ta *synctypes.TrackedAction) synctypes.ScopeKey {
	if watch == nil {
		return synctypes.ScopeKey{}
	}
	return syncdispatch.FindBlockingScope(watch.activeScopes, ta)
}

func (flow *engineFlow) scopeBlockKeys(watch *watchRuntime) []synctypes.ScopeKey {
	if watch == nil {
		return nil
	}
	return syncdispatch.ScopeKeys(watch.activeScopes)
}

func (flow *engineFlow) activateScope(ctx context.Context, watch *watchRuntime, block synctypes.ScopeBlock) error {
	if err := flow.engine.baseline.UpsertScopeBlock(ctx, &block); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if watch != nil {
		watch.activeScopes = syncdispatch.UpsertScope(watch.activeScopes, block)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertScopeInvariants(ctx, watch, "activate scope")

	return nil
}

func (flow *engineFlow) extendScopeTrial(ctx context.Context, watch *watchRuntime, scopeKey synctypes.ScopeKey, retryAfter time.Duration) {
	if watch == nil {
		return
	}

	block, ok := flow.getScopeBlock(watch, scopeKey)
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
	if err := flow.activateScope(ctx, watch, block); err != nil {
		flow.engine.logger.Warn("extendScopeTrial: failed to persist interval extension",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	watch.armTrialTimer()
}

// computeTrialInterval is the single source of truth for trial interval
// computation (R-2.10.14). Both initial scope block creation and subsequent
// trial extensions use this function, preventing policy divergence.
//
//   - retryAfter > 0: server-provided value used directly, no cap (R-2.10.7)
//   - retryAfter == 0, currentInterval > 0: double current, cap at defaultMaxTrialInterval
//   - retryAfter == 0, currentInterval == 0: use defaultInitialTrialInterval
func computeTrialInterval(scopeKey synctypes.ScopeKey, retryAfter, currentInterval time.Duration) time.Duration {
	initialInterval := syncdispatch.DefaultInitialTrialInterval
	maxInterval := syncdispatch.DefaultMaxTrialInterval
	if scopeKey == synctypes.SKDiskLocal() {
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

func scopeTimingSource(retryAfter time.Duration) synctypes.ScopeTimingSource {
	if retryAfter > 0 {
		return synctypes.ScopeTimingServerRetryAfter
	}

	return synctypes.ScopeTimingBackoff
}

// isObservationSuppressed returns true if a global scope block
// (throttle:account or service) is active, meaning shortcut observation
// polling should be skipped to avoid wasting API calls (R-2.10.30).
func (flow *engineFlow) isObservationSuppressed(watch *watchRuntime) bool {
	return flow.isScopeBlocked(watch, synctypes.SKThrottleAccount()) || flow.isScopeBlocked(watch, synctypes.SKService())
}

// feedScopeDetection feeds a worker result into scope detection sliding
// windows. If a threshold is crossed, creates a scope block. Called directly
// from the normal processWorkerResult switch — never called for trial results
// (the scope is already blocked, and re-detecting would overwrite the doubled
// interval).
func (flow *engineFlow) feedScopeDetection(ctx context.Context, watch *watchRuntime, r *synctypes.WorkerResult) {
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
		flow.applyScopeBlock(ctx, watch, sr)
	}
}

// applyScopeBlock persists and activates a new scope block. Uses
// computeTrialInterval for the initial interval, ensuring the same
// Retry-After-vs-backoff policy as extendScopeTrial.
func (flow *engineFlow) applyScopeBlock(ctx context.Context, watch *watchRuntime, sr synctypes.ScopeUpdateResult) {
	now := flow.engine.nowFunc()
	interval := computeTrialInterval(sr.ScopeKey, sr.RetryAfter, 0)

	if err := flow.activateScope(ctx, watch, synctypes.ScopeBlock{
		Key:           sr.ScopeKey,
		IssueType:     sr.IssueType,
		TimingSource:  scopeTimingSource(sr.RetryAfter),
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}); err != nil {
		flow.engine.logger.Warn("applyScopeBlock: failed to persist scope block",
			slog.String("scope_key", sr.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Warn("scope block active — actions held",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("issue_type", sr.IssueType),
		slog.Duration("trial_interval", interval),
	)

	if watch != nil {
		watch.armTrialTimer() // arm so the first trial fires at NextTrialAt (R-2.10.5)
	}
}

// releaseScope atomically removes the scope block, deletes any actionable
// boundary row for the scope, and makes held descendants retryable now.
func (flow *engineFlow) releaseScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.activeScopes = syncdispatch.RemoveScope(watch.activeScopes, key)
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

	flow.engine.logger.Info("scope block cleared — failures unblocked",
		slog.String("scope_key", key.String()),
	)

	flow.mustAssertReleasedScope(ctx, watch, key, "release scope")
	flow.mustAssertScopeInvariants(ctx, watch, "release scope")

	return nil
}

// discardScope atomically removes the scope block and deletes all failure rows
// tied to it. Used when the blocked subtree itself disappears.
func (flow *engineFlow) discardScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.activeScopes = syncdispatch.RemoveScope(watch.activeScopes, key)
		watch.armTrialTimer()
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	flow.mustAssertDiscardedScope(ctx, watch, key, "discard scope")
	flow.mustAssertScopeInvariants(ctx, watch, "discard scope")

	return nil
}

// admitReady applies watch-mode trial interception and scope admission to a
// ready action set, returning the actions that should enter the watch loop's
// outbox. It is the single admission path used by both newly-planned actions
// and newly-ready dependents from result processing.
func (flow *engineFlow) admitReady(
	ctx context.Context,
	watch *watchRuntime,
	ready []*synctypes.TrackedAction,
) []*synctypes.TrackedAction {
	var dispatch []*synctypes.TrackedAction

	for _, ta := range ready {
		if ta.IsTrial {
			if ta.TrialScopeKey.BlocksAction(ta.Action.Path,
				ta.Action.ShortcutKey(), ta.Action.Type,
				ta.Action.TargetsOwnDrive()) {
				dispatch = append(dispatch, ta)
			} else {
				// Trial candidate no longer matches scope — clear stale failure,
				// run normal admission. Best-effort: log on error, don't abort.
				if err := flow.engine.baseline.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID); err != nil {
					flow.engine.logger.Debug("admitReady: failed to clear stale trial failure",
						slog.String("path", ta.Action.Path),
						slog.String("error", err.Error()),
					)
				}

				if key := flow.activeBlockingScope(watch, ta); key.IsZero() {
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
			if key := flow.activeBlockingScope(watch, ta); !key.IsZero() {
				flow.cascadeRecordAndComplete(ctx, ta, key)
				continue
			}
		}

		flow.setDispatch(ctx, &ta.Action)
		dispatch = append(dispatch, ta)
	}

	return dispatch
}

// cascadeRecordAndComplete records a scope-blocked action and all its
// transitive dependents as sync_failures, completing each in the graph.
// Uses BFS to traverse the dependency tree. Each dependent inherits the
// parent's scope_key (section 3.4).
//
// Safe for concurrent use — depGraph.Complete uses a mutex. Two cascades
// from different goroutines cannot return the same dependent (depsLeft is
// atomic — the last parent to complete returns the dependent).
func (flow *engineFlow) cascadeRecordAndComplete(
	ctx context.Context,
	ta *synctypes.TrackedAction,
	scopeKey synctypes.ScopeKey,
) {
	seen := make(map[int64]bool)
	queue := []*synctypes.TrackedAction{ta}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		flow.recordScopeBlockedFailure(ctx, &current.Action, scopeKey)
		// No resetDispatchStatus — setDispatch was never called for blocked
		// actions (active-scope admission runs BEFORE setDispatch, per section 2.2).
		ready, _ := flow.depGraph.Complete(current.ID)
		queue = append(queue, ready...)
	}
}

// cascadeFailAndComplete records each transitive dependent as a cascade
// failure and completes it in the DepGraph. BFS ensures grandchildren are
// not stranded. Used for worker failures (non-scope-related).
func (flow *engineFlow) cascadeFailAndComplete(
	ctx context.Context,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		flow.recordCascadeFailure(ctx, &current.Action, r)
		next, _ := flow.depGraph.Complete(current.ID)
		queue = append(queue, next...)
	}
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown (context canceled — not a failure).
func (flow *engineFlow) completeSubtree(ready []*synctypes.TrackedAction) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next, _ := flow.depGraph.Complete(current.ID)
		queue = append(queue, next...)
	}
}

// recordScopeBlockedFailure records a sync_failure for an action that was
// blocked by an active scope. Uses next_retry_at = NULL (nil delayFn) so the
// retry sweep ignores it until releaseScope sets next_retry_at.
func (flow *engineFlow) recordScopeBlockedFailure(ctx context.Context, action *synctypes.Action, scopeKey synctypes.ScopeKey) {
	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	if err := flow.engine.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      action.Path,
		DriveID:   driveID,
		Direction: direction,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  scopeKey,
		ErrMsg:    "scope blocked: " + scopeKey.String(),
	}, nil); err != nil { // nil delayFn → next_retry_at = NULL
		flow.engine.logger.Warn("failed to record scope-blocked failure",
			slog.String("path", action.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// recordCascadeFailure records a sync_failure for a dependent whose parent
// failed. The dependent inherits the parent's error context but gets its
// own direction and a fresh failure_count. Uses retry.ReconcilePolicy().Delay for
// exponential backoff — the dependent retries independently.
func (flow *engineFlow) recordCascadeFailure(ctx context.Context, action *synctypes.Action, parentResult *synctypes.WorkerResult) {
	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	issueType := issueTypeForHTTPStatus(parentResult.HTTPStatus, parentResult.Err)

	if err := flow.engine.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      action.Path,
		DriveID:   driveID,
		Direction: direction,
		Category:  synctypes.CategoryTransient,
		IssueType: issueType,
		ErrMsg:    "parent action failed: " + parentResult.ErrMsg,
	}, retry.ReconcilePolicy().Delay); err != nil {
		flow.engine.logger.Warn("failed to record cascade failure",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}
