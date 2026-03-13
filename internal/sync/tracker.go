package sync

import (
	"context"
	"log/slog"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"
)

// Package-level API documentation for tracker.go (B-145):
//
// DepTracker is an in-memory dependency graph + scope-aware dispatch gate
// that dispatches sync actions to a single ready channel as their
// dependencies are satisfied. It bridges the planner's ActionPlan and the
// WorkerPool:
//
//   - Add(): Insert an action with its sequential ID and dependency IDs.
//     If all deps are satisfied, the action is dispatched immediately.
//   - Complete(): Mark an action done, decrement dependents' counters,
//     and dispatch any dependents that become ready.
//   - HasInFlight() / CancelByPath(): Deduplication for watch mode (B-122).
//   - Ready(): Single channel consumed by WorkerPool.
//
// The tracker does NOT handle retry. All retry is via sync_failures +
// FailureRetrier (R-6.8.10). The engine calls Complete on every result;
// failed items are recorded in sync_failures with next_retry_at, and the
// FailureRetrier re-injects them via buffer → planner → tracker.
//
// Two constructors: NewDepTracker (one-shot, Done() fires when all complete)
// and NewPersistentDepTracker (watch mode, workers exit via ctx cancellation).

// watchChanBuf is the channel buffer size for persistent-mode trackers.
// Large enough to absorb typical watch batches without blocking dispatch.
const watchChanBuf = 1024

// TrackedAction pairs an Action with an ID and a per-action cancel function.
// Workers pull TrackedActions from the ready channel. The ID is a sequential
// counter (assigned by the engine) used as a unique key for the tracker's
// internal maps.
type TrackedAction struct {
	Action Action
	ID     int64
	Cancel context.CancelFunc

	// IsTrial marks this action as a scope trial — a real action dispatched
	// from the held queue to test whether a blocked scope has recovered (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies which scope this trial is testing. Set by
	// DispatchTrial, propagated through WorkerResult so the engine knows
	// which scope to release on trial success.
	TrialScopeKey string

	depsLeft   atomic.Int32
	dependents []*TrackedAction
}

// DepTracker is an in-memory dependency graph that dispatches actions to a
// single ready channel as their dependencies are satisfied. It is populated
// from the planner's ActionPlan and driven to completion by worker
// Complete() calls.
//
// The tracker is a pure dispatch gate — dependency graph + scope blocks.
// Retry is handled entirely by sync_failures + FailureRetrier (R-6.8.10).
type DepTracker struct {
	mu         stdsync.Mutex
	actions    map[int64]*TrackedAction // sequential ID → tracked action
	byPath     map[string]*TrackedAction
	ready      chan *TrackedAction
	done       chan struct{} // closed when all actions complete (one-shot mode only)
	total      atomic.Int32
	completed  atomic.Int32
	persistent bool // when true, Done() never fires; workers exit on ctx.Done()
	logger     *slog.Logger

	// held stores actions blocked by scope-level failures (429, 507, 5xx).
	// Key is the scope key (e.g. "throttle:account", "quota:own").
	// Released when the scope block clears (R-2.10.11, R-2.10.15).
	held map[string][]*TrackedAction

	// scopeBlocks tracks active scope-level blocks. An action matching a
	// blocked scope is diverted to the held queue instead of ready.
	scopeBlocks map[string]*ScopeBlock
}

// NewDepTracker creates a tracker for one-shot mode with the given channel
// buffer size.
func NewDepTracker(bufSize int, logger *slog.Logger) *DepTracker {
	return &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		ready:       make(chan *TrackedAction, bufSize),
		done:        make(chan struct{}),
		logger:      logger,
		held:        make(map[string][]*TrackedAction),
		scopeBlocks: make(map[string]*ScopeBlock),
	}
}

// NewPersistentDepTracker creates a tracker for watch mode. In persistent mode,
// the global Done() channel never closes — workers exit via context cancellation
// instead.
func NewPersistentDepTracker(logger *slog.Logger) *DepTracker {
	return &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		ready:       make(chan *TrackedAction, watchChanBuf),
		done:        make(chan struct{}),
		persistent:  true,
		logger:      logger,
		held:        make(map[string][]*TrackedAction),
		scopeBlocks: make(map[string]*ScopeBlock),
	}
}

// Add inserts an action into the tracker. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed), the action is
// dispatched immediately. Otherwise it waits until Complete() decrements
// its depsLeft to zero.
func (dt *DepTracker) Add(action *Action, id int64, depIDs []int64) {
	ta := &TrackedAction{
		Action: *action,
		ID:     id,
	}

	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.actions[id] = ta
	dt.byPath[action.Path] = ta
	dt.total.Add(1)

	var depsRemaining int32

	for _, depID := range depIDs {
		dep, ok := dt.actions[depID]
		if !ok {
			// Dependency not tracked (already completed or unknown) — skip.
			continue
		}

		dep.dependents = append(dep.dependents, ta)
		depsRemaining++
	}

	ta.depsLeft.Store(depsRemaining)

	if depsRemaining == 0 {
		dt.dispatch(ta)
	}
}

// Complete marks an action as done and decrements the depsLeft counter on
// all dependents. Any dependent that reaches zero is dispatched. When all
// actions are complete (one-shot mode only), the done channel is closed.
//
// If id is unknown (not in the tracker), the completed counter is still
// incremented to prevent deadlock, and a warning is logged. This should
// never happen in normal operation but guards against subtle bugs in
// tracker population.
func (dt *DepTracker) Complete(id int64) {
	dt.mu.Lock()
	ta, ok := dt.actions[id]
	if !ok {
		dt.mu.Unlock()
		dt.logger.Warn("tracker: Complete called with unknown ID",
			slog.Int64("id", id),
		)

		if !dt.persistent && dt.completed.Add(1) == dt.total.Load() {
			close(dt.done)
		}

		return
	}

	// Copy dependents under the lock to prevent races with Add() appending
	// to the same slice in watch mode (overlapping passes).
	dependents := make([]*TrackedAction, len(ta.dependents))
	copy(dependents, ta.dependents)

	// Clean up byPath so long-lived trackers (watch mode) don't cancel
	// the wrong action if the same path appears in a subsequent pass.
	delete(dt.byPath, ta.Action.Path)
	dt.mu.Unlock()

	for _, dep := range dependents {
		if dep.depsLeft.Add(-1) == 0 {
			dt.dispatch(dep)
		}
	}

	// In persistent mode, the global done channel never fires — workers
	// exit via context cancellation instead.
	newCompleted := dt.completed.Add(1)
	if !dt.persistent && newCompleted == dt.total.Load() {
		close(dt.done)
	}
}

// HasInFlight returns true if the given path has an in-flight action
// tracked by the tracker (B-122). Thread-safe.
func (dt *DepTracker) HasInFlight(path string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	_, ok := dt.byPath[path]
	return ok
}

// CancelByPath cancels the in-flight action for the given path, if any.
// Removes the byPath entry so long-lived trackers don't cancel the wrong
// action if the same path is re-added in a subsequent pass.
func (dt *DepTracker) CancelByPath(path string) {
	dt.mu.Lock()
	ta, ok := dt.byPath[path]
	if ok {
		delete(dt.byPath, path)
	}
	dt.mu.Unlock()

	if ok && ta.Cancel != nil {
		ta.Cancel()
	}
}

// InFlightCount returns the number of actions currently in the tracker that
// have not yet completed. Used for shutdown logging.
func (dt *DepTracker) InFlightCount() int {
	return int(dt.total.Load() - dt.completed.Load())
}

// Ready returns the channel for all ready actions.
func (dt *DepTracker) Ready() <-chan *TrackedAction {
	return dt.ready
}

// Done returns a channel that is closed when all tracked actions complete.
// In persistent mode (watch), this channel never closes — workers exit via
// context cancellation instead.
func (dt *DepTracker) Done() <-chan struct{} {
	return dt.done
}

// dispatch routes a ready action through the scope gate before sending it
// to the ready channel. This is the central dispatch point — called by Add,
// Complete (for dependents), and ReleaseScope.
func (dt *DepTracker) dispatch(ta *TrackedAction) {
	// Scope block — prevent wasted requests to blocked scopes.
	// An action matching a blocked scope goes to the held queue instead.
	if key := dt.blockedScope(ta); key != "" {
		dt.held[key] = append(dt.held[key], ta)
		return
	}

	dt.dispatchReady(ta)
}

// dispatchReady sends an action directly to the ready channel, bypassing
// gates. Used by DispatchTrial for trial actions and dispatch() after gate
// checks pass.
func (dt *DepTracker) dispatchReady(ta *TrackedAction) {
	dt.ready <- ta
}

// blockedScope returns the scope key blocking this action, or "" if none.
// Checks the action's target drive against active scope blocks.
func (dt *DepTracker) blockedScope(ta *TrackedAction) string {
	if len(dt.scopeBlocks) == 0 {
		return ""
	}

	// throttle:account blocks ALL actions across all drives (R-6.8.4, R-2.10.26).
	if _, ok := dt.scopeBlocks[scopeKeyThrottleAccount]; ok {
		return scopeKeyThrottleAccount
	}

	// service blocks ALL actions across all drives (R-2.10.28).
	if _, ok := dt.scopeBlocks[scopeKeyService]; ok {
		return scopeKeyService
	}

	// quota:own blocks own-drive uploads only (R-2.10.19).
	if _, ok := dt.scopeBlocks[scopeKeyQuotaOwn]; ok {
		if ta.Action.TargetsOwnDrive() && ta.Action.Type == ActionUpload {
			return scopeKeyQuotaOwn
		}
	}

	// quota:shortcut:$key blocks uploads to that specific shortcut (R-2.10.20).
	if scKey := ta.Action.ShortcutKey(); scKey != "" {
		scopeKey := scopeKeyQuotaShortcut + scKey
		if _, ok := dt.scopeBlocks[scopeKey]; ok {
			if ta.Action.Type == ActionUpload {
				return scopeKey
			}
		}
	}

	// perm:dir:$path blocks all actions whose path falls under the denied
	// directory (R-2.10.12). O(n) over active perm:dir blocks — expected
	// to be tiny (1-3 typically).
	for key := range dt.scopeBlocks {
		if !strings.HasPrefix(key, scopeKeyPermDir) {
			continue
		}

		dirPath := strings.TrimPrefix(key, scopeKeyPermDir)
		if ta.Action.Path == dirPath || strings.HasPrefix(ta.Action.Path, dirPath+"/") {
			return key
		}
	}

	return ""
}

// HoldScope registers a scope block. Future dispatches matching this scope
// key are diverted to the held queue. If there's an existing block for the
// same key, it is replaced (updated trial timing).
func (dt *DepTracker) HoldScope(key string, block *ScopeBlock) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.scopeBlocks[key] = block
	dt.logger.Info("tracker: scope blocked",
		slog.String("scope_key", key),
		slog.String("issue_type", block.IssueType),
	)
}

// ReleaseScope clears a scope block and dispatches all held actions for
// that scope key (R-2.10.11, R-2.10.27).
func (dt *DepTracker) ReleaseScope(key string) {
	dt.mu.Lock()
	held := dt.held[key]
	delete(dt.held, key)
	delete(dt.scopeBlocks, key)
	dt.mu.Unlock()

	if len(held) > 0 {
		dt.logger.Info("tracker: scope released, dispatching held actions",
			slog.String("scope_key", key),
			slog.Int("count", len(held)),
		)
	}

	for _, ta := range held {
		dt.dispatch(ta)
	}
}

// DiscardScope clears a scope block and completes all held actions for that
// scope key without dispatching them. Used when the scope's source is removed
// (e.g., shortcut deleted) and held actions are no longer valid (R-2.10.38).
// Unlike ReleaseScope, discarded actions are never dispatched to workers.
func (dt *DepTracker) DiscardScope(key string) {
	dt.mu.Lock()
	held := dt.held[key]
	delete(dt.held, key)
	delete(dt.scopeBlocks, key)
	dt.mu.Unlock()

	if len(held) > 0 {
		dt.logger.Info("tracker: scope discarded, completing held actions without dispatch",
			slog.String("scope_key", key),
			slog.Int("count", len(held)),
		)
	}

	for range held {
		// Mark completed without dispatching — these actions are orphaned.
		newCompleted := dt.completed.Add(1)
		if !dt.persistent && newCompleted == dt.total.Load() {
			close(dt.done)
		}
	}
}

// DispatchTrial pops one action from the held queue for the given scope key,
// marks it as a trial (IsTrial=true) with TrialScopeKey set, and dispatches
// it directly to the ready channel. Returns false if the held queue is
// empty (R-2.10.5).
func (dt *DepTracker) DispatchTrial(key string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	held := dt.held[key]
	if len(held) == 0 {
		return false
	}

	// Pop the first held action as a trial.
	ta := held[0]
	dt.held[key] = held[1:]
	ta.IsTrial = true
	ta.TrialScopeKey = key

	dt.logger.Debug("tracker: dispatching trial action",
		slog.String("scope_key", key),
		slog.String("path", ta.Action.Path),
	)

	// Bypass gates — trial goes directly to workers.
	dt.dispatchReady(ta)
	return true
}

// NextDueTrial returns the scope key and NextTrialAt of the first scope
// block where now >= block.NextTrialAt, or ("", time.Time{}, false) if
// no trials are due. Thread-safe.
func (dt *DepTracker) NextDueTrial(now time.Time) (string, time.Time, bool) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for key, block := range dt.scopeBlocks {
		if block.NextTrialAt.IsZero() {
			continue
		}

		if !now.Before(block.NextTrialAt) && len(dt.held[key]) > 0 {
			return key, block.NextTrialAt, true
		}
	}

	return "", time.Time{}, false
}

// EarliestTrialAt returns the earliest NextTrialAt across all scope blocks
// that have non-empty held queues. Returns (time.Time{}, false) if no
// trials are pending. Used by the engine's trial timer (R-2.10.5).
func (dt *DepTracker) EarliestTrialAt() (time.Time, bool) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	var earliest time.Time
	found := false

	for key, block := range dt.scopeBlocks {
		if block.NextTrialAt.IsZero() || len(dt.held[key]) == 0 {
			continue
		}

		if !found || block.NextTrialAt.Before(earliest) {
			earliest = block.NextTrialAt
			found = true
		}
	}

	return earliest, found
}

// GetScopeBlock returns a snapshot of the ScopeBlock for the given key, or
// (ScopeBlock{}, false) if the scope is not blocked. Returns a copy to
// prevent unsynchronized mutation of tracker-owned state. Thread-safe.
func (dt *DepTracker) GetScopeBlock(key string) (ScopeBlock, bool) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	block, ok := dt.scopeBlocks[key]
	if !ok {
		return ScopeBlock{}, false
	}
	return *block, ok
}

// ExtendTrialInterval atomically doubles the block's TrialInterval (capped
// at maxInterval), sets NextTrialAt, and increments TrialCount. All mutation
// happens under the lock — callers must not mutate the block externally.
// Thread-safe.
func (dt *DepTracker) ExtendTrialInterval(key string, nextAt time.Time, newInterval time.Duration) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	block, ok := dt.scopeBlocks[key]
	if !ok {
		return
	}

	block.TrialInterval = newInterval
	block.NextTrialAt = nextAt
	block.TrialCount++
}

// ---------------------------------------------------------------------------
// ScopeBlock
// ---------------------------------------------------------------------------

// ScopeBlock represents an active scope-level failure block. When a scope is
// blocked, new actions matching that scope are diverted to the held queue
// instead of being dispatched to workers.
type ScopeBlock struct {
	Key       string // scope key (e.g. "throttle:account", "quota:own")
	IssueType string // "service_outage", "quota_exceeded", "rate_limited"

	BlockedAt     time.Time     // when the block was created
	TrialInterval time.Duration // current interval between trial actions (grows with backoff)
	NextTrialAt   time.Time     // when to dispatch the next trial
	TrialCount    int           // consecutive failed trials (for backoff)
}
