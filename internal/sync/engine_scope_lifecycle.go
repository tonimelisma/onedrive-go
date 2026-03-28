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
func (e *Engine) loadActiveScopes(ctx context.Context) error {
	if e.watch == nil {
		return nil
	}

	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	e.watch.activeScopes = e.watch.activeScopes[:0]
	for i := range blocks {
		e.watch.activeScopes = append(e.watch.activeScopes, *blocks[i])
	}

	return nil
}

// repairPersistedScopes normalizes persisted scope rows against current store
// evidence before any admission begins. The store remains authoritative for
// restart state; watch mode loads activeScopes only after this repair pass.
func (e *Engine) repairPersistedScopes(ctx context.Context) error {
	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing scope blocks: %w", err)
	}
	if len(blocks) == 0 {
		return nil
	}

	failures, err := e.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing sync failures: %w", err)
	}

	facts := summarizePersistedScopeFailures(failures)

	for i := range blocks {
		if err := e.repairPersistedScope(ctx, blocks[i], facts); err != nil {
			return err
		}
	}

	e.mustAssertScopeInvariants(ctx, "repair persisted scopes")

	return nil
}

func (e *Engine) repairPersistedScope(
	ctx context.Context,
	block *synctypes.ScopeBlock,
	facts persistedScopeFacts,
) error {
	switch scopeStartupPolicyFor(block.Key) {
	case scopeStartupRequiresBoundary:
		if facts.boundaryKeys[block.Key] {
			return nil
		}
		return e.releaseStartupScope(ctx, block.Key, "released scope without boundary evidence")
	case scopeStartupRequiresScopedFailures:
		if facts.failureCountByScope[block.Key] > 0 {
			return nil
		}
		return e.discardStartupScope(ctx, block.Key, "discarded scope without scoped failures")
	case scopeStartupServerTimedOnly:
		if block.TimingSource == synctypes.ScopeTimingServerRetryAfter {
			return nil
		}
		return e.releaseStartupScope(ctx, block.Key, "released non-server-timed restart scope")
	case scopeStartupRevalidateDisk:
		return e.repairDiskScope(ctx, block)
	default:
		panic(fmt.Sprintf("unknown startup policy %d", scopeStartupPolicyFor(block.Key)))
	}
}

func (e *Engine) releaseStartupScope(ctx context.Context, key synctypes.ScopeKey, note string) error {
	if err := e.baseline.ReleaseScope(ctx, key, e.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing startup scope %s: %w", key.String(), err)
	}
	e.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func (e *Engine) discardStartupScope(ctx context.Context, key synctypes.ScopeKey, note string) error {
	if err := e.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding startup scope %s: %w", key.String(), err)
	}
	e.emitDebugEvent(engineDebugEvent{
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

func (e *Engine) repairDiskScope(ctx context.Context, block *synctypes.ScopeBlock) error {
	if e.minFreeSpace <= 0 {
		if err := e.baseline.ReleaseScope(ctx, block.Key, e.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing disk scope %s with disabled min_free_space: %w", block.Key.String(), err)
		}
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope with disabled min_free_space",
		})
		return nil
	}

	available, err := e.diskAvailableFn(e.syncRoot)
	if err != nil {
		e.logger.Warn("repairPersistedScopes: disk revalidation failed, releasing stale disk scope",
			slog.String("scope_key", block.Key.String()),
			slog.String("error", err.Error()),
		)
		if releaseErr := e.baseline.ReleaseScope(ctx, block.Key, e.nowFunc()); releaseErr != nil {
			return fmt.Errorf("sync: releasing stale disk scope %s: %w", block.Key.String(), releaseErr)
		}
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope after revalidation error",
		})
		return nil
	}

	if int64(available) >= e.minFreeSpace {
		if err := e.baseline.ReleaseScope(ctx, block.Key, e.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing recovered disk scope %s: %w", block.Key.String(), err)
		}
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope after healthy revalidation",
		})
		return nil
	}

	now := e.nowFunc()
	interval := computeTrialInterval(block.Key, 0, 0)
	if err := e.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
		Key:           block.Key,
		IssueType:     synctypes.IssueDiskFull,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}); err != nil {
		return fmt.Errorf("sync: refreshing disk scope %s: %w", block.Key.String(), err)
	}
	e.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: block.Key,
		Note:     "refreshed disk scope from current local truth",
	})

	return nil
}

func (e *Engine) getScopeBlock(key synctypes.ScopeKey) (synctypes.ScopeBlock, bool) {
	if e.watch == nil {
		return synctypes.ScopeBlock{}, false
	}
	return syncdispatch.LookupScope(e.watch.activeScopes, key)
}

func (e *Engine) isScopeBlocked(key synctypes.ScopeKey) bool {
	if e.watch == nil {
		return false
	}
	return syncdispatch.HasScope(e.watch.activeScopes, key)
}

func (e *Engine) activeBlockingScope(ta *synctypes.TrackedAction) synctypes.ScopeKey {
	if e.watch == nil {
		return synctypes.ScopeKey{}
	}
	return syncdispatch.FindBlockingScope(e.watch.activeScopes, ta)
}

func (e *Engine) scopeBlockKeys() []synctypes.ScopeKey {
	if e.watch == nil {
		return nil
	}
	return syncdispatch.ScopeKeys(e.watch.activeScopes)
}

func (e *Engine) activateScope(ctx context.Context, block synctypes.ScopeBlock) error {
	if err := e.baseline.UpsertScopeBlock(ctx, &block); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if e.watch != nil {
		e.watch.activeScopes = syncdispatch.UpsertScope(e.watch.activeScopes, block)
	}
	e.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	e.mustAssertScopeInvariants(ctx, "activate scope")

	return nil
}

func (e *Engine) extendScopeTrial(ctx context.Context, scopeKey synctypes.ScopeKey, retryAfter time.Duration) {
	if e.watch == nil {
		return
	}

	block, ok := e.getScopeBlock(scopeKey)
	if !ok {
		return
	}

	newInterval := computeTrialInterval(scopeKey, retryAfter, block.TrialInterval)
	nextAt := e.nowFunc().Add(newInterval)

	e.logger.Debug("extending trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
		slog.Duration("retry_after", retryAfter),
	)

	block.NextTrialAt = nextAt
	block.TrialInterval = newInterval
	block.TrialCount++
	block.TimingSource = scopeTimingSource(retryAfter)
	if err := e.activateScope(ctx, block); err != nil {
		e.logger.Warn("extendScopeTrial: failed to persist interval extension",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	e.armTrialTimer()
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
func (e *Engine) isObservationSuppressed() bool {
	return e.isScopeBlocked(synctypes.SKThrottleAccount()) || e.isScopeBlocked(synctypes.SKService())
}

// feedScopeDetection feeds a worker result into scope detection sliding
// windows. If a threshold is crossed, creates a scope block. Called directly
// from the normal processWorkerResult switch — never called for trial results
// (the scope is already blocked, and re-detecting would overwrite the doubled
// interval).
func (e *Engine) feedScopeDetection(ctx context.Context, r *synctypes.WorkerResult) {
	if e.watch == nil {
		return
	}

	// Local errors (HTTPStatus==0) must not feed scope detection windows.
	// Only remote API errors should increment service/quota counters (R-6.7.27).
	if r.HTTPStatus == 0 {
		return
	}

	sr := e.watch.scopeState.UpdateScope(r)
	if sr.Block {
		e.applyScopeBlock(ctx, sr)
	}
}

// applyScopeBlock persists and activates a new scope block. Uses
// computeTrialInterval for the initial interval, ensuring the same
// Retry-After-vs-backoff policy as extendScopeTrial.
func (e *Engine) applyScopeBlock(ctx context.Context, sr synctypes.ScopeUpdateResult) {
	now := e.nowFunc()
	interval := computeTrialInterval(sr.ScopeKey, sr.RetryAfter, 0)

	if err := e.activateScope(ctx, synctypes.ScopeBlock{
		Key:           sr.ScopeKey,
		IssueType:     sr.IssueType,
		TimingSource:  scopeTimingSource(sr.RetryAfter),
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}); err != nil {
		e.logger.Warn("applyScopeBlock: failed to persist scope block",
			slog.String("scope_key", sr.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	e.logger.Warn("scope block active — actions held",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("issue_type", sr.IssueType),
		slog.Duration("trial_interval", interval),
	)

	e.armTrialTimer() // arm so the first trial fires at NextTrialAt (R-2.10.5)
}

// releaseScope atomically removes the scope block, deletes any actionable
// boundary row for the scope, and makes held descendants retryable now.
func (e *Engine) releaseScope(ctx context.Context, key synctypes.ScopeKey) error {
	if err := e.baseline.ReleaseScope(ctx, key, e.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing scope %s: %w", key.String(), err)
	}

	if e.watch != nil {
		e.watch.activeScopes = syncdispatch.RemoveScope(e.watch.activeScopes, key)
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
		e.kickRetrySweepNow()
		e.armTrialTimer()
	} else {
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
	}

	e.logger.Info("scope block cleared — failures unblocked",
		slog.String("scope_key", key.String()),
	)

	e.mustAssertReleasedScope(ctx, key, "release scope")
	e.mustAssertScopeInvariants(ctx, "release scope")

	return nil
}

// discardScope atomically removes the scope block and deletes all failure rows
// tied to it. Used when the blocked subtree itself disappears.
func (e *Engine) discardScope(ctx context.Context, key synctypes.ScopeKey) error {
	if err := e.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	if e.watch != nil {
		e.watch.activeScopes = syncdispatch.RemoveScope(e.watch.activeScopes, key)
		e.armTrialTimer()
	}
	e.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	e.mustAssertDiscardedScope(ctx, key, "discard scope")
	e.mustAssertScopeInvariants(ctx, "discard scope")

	return nil
}

// admitReady applies watch-mode trial interception and scope admission to a
// ready action set, returning the actions that should enter the watch loop's
// outbox. It is the single admission path used by both newly-planned actions
// and newly-ready dependents from result processing.
func (e *Engine) admitReady(ctx context.Context, ready []*synctypes.TrackedAction) []*synctypes.TrackedAction {
	var dispatch []*synctypes.TrackedAction

	for _, ta := range ready {
		// Trial interception — watch mode only (one-shot has no trials).
		var isTrial bool
		var entry trialEntry
		if e.watch != nil {
			entry, isTrial = e.watch.trialPending[ta.Action.Path]
			if isTrial {
				delete(e.watch.trialPending, ta.Action.Path)
			}
		}

		if isTrial {
			if entry.scopeKey.BlocksAction(ta.Action.Path,
				ta.Action.ShortcutKey(), ta.Action.Type,
				ta.Action.TargetsOwnDrive()) {
				ta.IsTrial = true
				ta.TrialScopeKey = entry.scopeKey
				dispatch = append(dispatch, ta)
			} else {
				// Trial candidate no longer matches scope — clear stale failure,
				// run normal admission. Best-effort: log on error, don't abort.
				if err := e.baseline.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID); err != nil {
					e.logger.Debug("admitReady: failed to clear stale trial failure",
						slog.String("path", ta.Action.Path),
						slog.String("error", err.Error()),
					)
				}

				// e.watch is guaranteed non-nil — admitReady is only
				// called from processBatch (watch-mode only).
				if key := e.activeBlockingScope(ta); key.IsZero() {
					e.setDispatch(ctx, &ta.Action)
					dispatch = append(dispatch, ta)
				}
				e.armTrialTimer()
			}

			continue
		}

		// Normal scope admission.
		if e.watch != nil {
			if key := e.activeBlockingScope(ta); !key.IsZero() {
				e.cascadeRecordAndComplete(ctx, ta, key)
				continue
			}
		}

		e.setDispatch(ctx, &ta.Action)
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
func (e *Engine) cascadeRecordAndComplete(ctx context.Context, ta *synctypes.TrackedAction, scopeKey synctypes.ScopeKey) {
	seen := make(map[int64]bool)
	queue := []*synctypes.TrackedAction{ta}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		e.recordScopeBlockedFailure(ctx, &current.Action, scopeKey)
		// No resetDispatchStatus — setDispatch was never called for blocked
		// actions (active-scope admission runs BEFORE setDispatch, per section 2.2).
		ready, _ := e.depGraph.Complete(current.ID)
		queue = append(queue, ready...)
	}
}

// cascadeFailAndComplete records each transitive dependent as a cascade
// failure and completes it in the DepGraph. BFS ensures grandchildren are
// not stranded. Used for worker failures (non-scope-related).
func (e *Engine) cascadeFailAndComplete(ctx context.Context, ready []*synctypes.TrackedAction, r *synctypes.WorkerResult) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		e.recordCascadeFailure(ctx, &current.Action, r)
		next, _ := e.depGraph.Complete(current.ID)
		queue = append(queue, next...)
	}
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown (context canceled — not a failure).
func (e *Engine) completeSubtree(ready []*synctypes.TrackedAction) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next, _ := e.depGraph.Complete(current.ID)
		queue = append(queue, next...)
	}
}

// recordScopeBlockedFailure records a sync_failure for an action that was
// blocked by an active scope. Uses next_retry_at = NULL (nil delayFn) so the
// retry sweep ignores it until releaseScope sets next_retry_at.
func (e *Engine) recordScopeBlockedFailure(ctx context.Context, action *synctypes.Action, scopeKey synctypes.ScopeKey) {
	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	if err := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      action.Path,
		DriveID:   driveID,
		Direction: direction,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  scopeKey,
		ErrMsg:    "scope blocked: " + scopeKey.String(),
	}, nil); err != nil { // nil delayFn → next_retry_at = NULL
		e.logger.Warn("failed to record scope-blocked failure",
			slog.String("path", action.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// recordCascadeFailure records a sync_failure for a dependent whose parent
// failed. The dependent inherits the parent's error context but gets its
// own direction and a fresh failure_count. Uses retry.Reconcile.Delay for
// exponential backoff — the dependent retries independently.
func (e *Engine) recordCascadeFailure(ctx context.Context, action *synctypes.Action, parentResult *synctypes.WorkerResult) {
	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	issueType := issueTypeForHTTPStatus(parentResult.HTTPStatus, parentResult.Err)

	if err := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      action.Path,
		DriveID:   driveID,
		Direction: direction,
		Category:  synctypes.CategoryTransient,
		IssueType: issueType,
		ErrMsg:    "parent action failed: " + parentResult.ErrMsg,
	}, retry.ReconcilePolicy().Delay); err != nil {
		e.logger.Warn("failed to record cascade failure",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}
