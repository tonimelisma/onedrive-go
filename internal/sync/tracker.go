package sync

import (
	"log/slog"
	stdsync "sync"
	"sync/atomic"
	"time"
)

// Package-level API documentation for tracker.go (B-145):
//
// DepTracker is a scope-aware dispatch gate that wraps a DepGraph (pure
// dependency graph) and dispatches sync actions to a single ready channel.
// It bridges the planner's ActionPlan and the WorkerPool:
//
//   - Add(): Insert an action into the dependency graph. If immediately
//     ready, route through the scope gate and dispatch.
//   - Complete(): Mark an action done in the graph, then route any
//     newly-ready dependents through the scope gate.
//   - HasInFlight() / CancelByPath(): Delegated to DepGraph (B-122).
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

// DepTracker wraps a DepGraph with scope-based admission control and channel
// dispatch. The DepGraph handles pure dependency resolution (no channels, no
// callbacks, no scope awareness). DepTracker adds scope gating, held queues,
// trial dispatch, and the ready channel.
//
// The tracker is a dispatch gate — dependency graph + scope blocks.
// Retry is handled entirely by sync_failures + FailureRetrier (R-6.8.10).
type DepTracker struct {
	mu         stdsync.Mutex
	dg         *DepGraph // pure dependency graph — no channels, no scope awareness
	ready      chan *TrackedAction
	done       chan struct{} // closed when all actions complete (one-shot mode only)
	total      atomic.Int32
	completed  atomic.Int32
	persistent bool // when true, Done() never fires; workers exit on ctx.Done()
	logger     *slog.Logger

	// held stores actions blocked by scope-level failures (429, 507, 5xx).
	// Key is the typed scope key. Released when the scope block clears
	// (R-2.10.11, R-2.10.15).
	held map[ScopeKey][]*TrackedAction

	// scopeBlocks tracks active scope-level blocks. An action matching a
	// blocked scope is diverted to the held queue instead of ready.
	scopeBlocks map[ScopeKey]*ScopeBlock

	// onHeld is called when dispatch() diverts an action to a held queue.
	// The engine sets this to armTrialTimer so the trial timer re-arms when
	// the held queue becomes non-empty. Must NOT be called under dt.mu —
	// callers invoke it after releasing the lock.
	onHeld func()
}

// NewDepTracker creates a tracker for one-shot mode with the given channel
// buffer size.
func NewDepTracker(bufSize int, logger *slog.Logger) *DepTracker {
	return &DepTracker{
		dg:          NewDepGraph(logger),
		ready:       make(chan *TrackedAction, bufSize),
		done:        make(chan struct{}),
		logger:      logger,
		held:        make(map[ScopeKey][]*TrackedAction),
		scopeBlocks: make(map[ScopeKey]*ScopeBlock),
	}
}

// NewPersistentDepTracker creates a tracker for watch mode. In persistent mode,
// the global Done() channel never closes — workers exit via context cancellation
// instead.
func NewPersistentDepTracker(logger *slog.Logger) *DepTracker {
	return &DepTracker{
		dg:          NewDepGraph(logger),
		ready:       make(chan *TrackedAction, watchChanBuf),
		done:        make(chan struct{}),
		persistent:  true,
		logger:      logger,
		held:        make(map[ScopeKey][]*TrackedAction),
		scopeBlocks: make(map[ScopeKey]*ScopeBlock),
	}
}

// Add inserts an action into the dependency graph. If all dependencies are
// already satisfied, the action is routed through the scope gate and
// dispatched. Otherwise it waits until Complete() decrements its depsLeft
// to zero.
//
// D-1 fix: dispatch is always called under dt.mu, preventing the data race
// where dispatch() reads scopeBlocks/held without synchronization.
func (dt *DepTracker) Add(action *Action, id int64, depIDs []int64) {
	ta := dt.dg.Add(action, id, depIDs)
	dt.total.Add(1)

	if ta == nil {
		// Action is waiting on dependencies — nothing to dispatch yet.
		return
	}

	// Action is immediately ready — route through scope gate under lock.
	var wasHeld bool

	dt.mu.Lock()
	wasHeld = dt.dispatch(ta)
	dt.mu.Unlock()

	if wasHeld && dt.onHeld != nil {
		dt.onHeld()
	}
}

// Complete marks an action as done in the dependency graph and dispatches
// any newly-ready dependents through the scope gate. When all actions are
// complete (one-shot mode only), the done channel is closed.
//
// If id is unknown (not in the graph), the completed counter is still
// incremented to prevent deadlock, and a warning is logged.
//
// D-1 fix: dispatch is always called under dt.mu, preventing the data race
// where the old Complete() called dispatch() after releasing the lock.
func (dt *DepTracker) Complete(id int64) {
	ready, known := dt.dg.Complete(id)

	// Route all newly-ready dependents through the scope gate under lock.
	var anyHeld bool

	if len(ready) > 0 {
		dt.mu.Lock()
		for _, dep := range ready {
			if dt.dispatch(dep) {
				anyHeld = true
			}
		}
		dt.mu.Unlock()
	}

	if anyHeld && dt.onHeld != nil {
		dt.onHeld()
	}

	// Always increment completed — even for unknown IDs — to prevent
	// deadlock if total was already incremented by Add. The unknown-ID
	// warning is logged by DepGraph.Complete.
	_ = known // DepGraph already logged the warning for unknown IDs.
	newCompleted := dt.completed.Add(1)
	if !dt.persistent && newCompleted == dt.total.Load() {
		close(dt.done)
	}
}

// HasInFlight returns true if the given path has an in-flight action
// tracked by the graph (B-122). Thread-safe.
func (dt *DepTracker) HasInFlight(path string) bool {
	return dt.dg.HasInFlight(path)
}

// CancelByPath cancels the in-flight action for the given path, if any.
// Delegates to DepGraph which removes the byPath entry so long-lived
// trackers don't cancel the wrong action if the same path is re-added.
func (dt *DepTracker) CancelByPath(path string) {
	dt.dg.CancelByPath(path)
}

// InFlightCount returns the number of actions currently in the tracker that
// have not yet completed. Uses the tracker's own total/completed atomics
// because DiscardScope increments completed without graph knowledge.
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
// to the ready channel. Returns true if the action was diverted to a held
// queue (scope blocked). Callers use the return value to fire onHeld
// outside the lock.
//
// MUST be called under dt.mu — reads scopeBlocks and writes held maps.
// This is the central dispatch point — called by Add, Complete (for
// dependents), and ReleaseScope.
func (dt *DepTracker) dispatch(ta *TrackedAction) bool {
	// Scope block — prevent wasted requests to blocked scopes.
	// An action matching a blocked scope goes to the held queue instead.
	if key := dt.blockedScope(ta); !key.IsZero() {
		dt.held[key] = append(dt.held[key], ta)
		return true
	}

	dt.dispatchReady(ta)
	return false
}

// dispatchReady sends an action directly to the ready channel, bypassing
// gates. Used by DispatchTrial for trial actions and dispatch() after gate
// checks pass.
func (dt *DepTracker) dispatchReady(ta *TrackedAction) {
	dt.ready <- ta
}

// blockedScope returns the scope key blocking this action, or the zero-value
// ScopeKey if none. Evaluates all active scope blocks using
// ScopeKey.BlocksAction(), returning the first match. Priority is enforced
// by checking global blocks first, then progressively narrower scopes.
func (dt *DepTracker) blockedScope(ta *TrackedAction) ScopeKey {
	if len(dt.scopeBlocks) == 0 {
		return ScopeKey{}
	}

	// Priority-ordered fixed keys: global blocks first, then narrower scopes.
	// New scope types are added here.
	priorityKeys := [...]ScopeKey{
		SKThrottleAccount, // blocks ALL actions (R-6.8.4, R-2.10.26)
		SKService,         // blocks ALL actions (R-2.10.28)
		SKDiskLocal,       // blocks downloads only (R-2.10.43)
		SKQuotaOwn,        // blocks own-drive uploads (R-2.10.19)
	}

	scKey := ta.Action.ShortcutKey()
	targetsOwn := ta.Action.TargetsOwnDrive()

	for _, sk := range priorityKeys {
		if _, ok := dt.scopeBlocks[sk]; ok && sk.BlocksAction(ta.Action.Path, scKey, ta.Action.Type, targetsOwn) {
			return sk
		}
	}

	// Dynamic-key scopes: shortcut quota and perm:dir depend on action context.
	// O(n) over active scope blocks — expected to be tiny (1-5 typically).
	for sk := range dt.scopeBlocks {
		switch sk.Kind { //nolint:exhaustive // only parameterized scopes need per-action checking
		case ScopeQuotaShortcut, ScopePermDir:
			if sk.BlocksAction(ta.Action.Path, scKey, ta.Action.Type, targetsOwn) {
				return sk
			}
		}
	}

	return ScopeKey{}
}

// HoldScope registers a scope block. Future dispatches matching this scope
// key are diverted to the held queue. If there's an existing block for the
// same key, it is replaced (updated trial timing).
func (dt *DepTracker) HoldScope(key ScopeKey, block *ScopeBlock) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.scopeBlocks[key] = block
	dt.logger.Info("tracker: scope blocked",
		slog.String("scope_key", key.String()),
		slog.String("issue_type", block.IssueType),
	)
}

// ReleaseScope clears a scope block and dispatches all held actions for
// that scope key through the scope gate (R-2.10.11, R-2.10.27). Released
// actions may be re-held if another scope blocks them.
func (dt *DepTracker) ReleaseScope(key ScopeKey) {
	dt.mu.Lock()
	held := dt.held[key]
	delete(dt.held, key)
	delete(dt.scopeBlocks, key)

	if len(held) > 0 {
		dt.logger.Info("tracker: scope released, dispatching held actions",
			slog.String("scope_key", key.String()),
			slog.Int("count", len(held)),
		)
	}

	var anyHeld bool

	for _, ta := range held {
		if dt.dispatch(ta) {
			anyHeld = true
		}
	}

	dt.mu.Unlock()

	if anyHeld && dt.onHeld != nil {
		dt.onHeld()
	}
}

// DiscardScope clears a scope block and completes all held actions for that
// scope key without dispatching them. Used when the scope's source is removed
// (e.g., shortcut deleted) and held actions are no longer valid (R-2.10.38).
// Unlike ReleaseScope, discarded actions are never dispatched to workers.
func (dt *DepTracker) DiscardScope(key ScopeKey) {
	dt.mu.Lock()
	held := dt.held[key]
	delete(dt.held, key)
	delete(dt.scopeBlocks, key)
	dt.mu.Unlock()

	if len(held) > 0 {
		dt.logger.Info("tracker: scope discarded, completing held actions without dispatch",
			slog.String("scope_key", key.String()),
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
func (dt *DepTracker) DispatchTrial(key ScopeKey) bool {
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

	// Clear NextTrialAt so NextDueTrial won't return this scope again
	// until the trial result re-arms via processTrialResult → armTrialTimer.
	// Without this, the drain loop would dispatch ALL held actions as
	// simultaneous trials (R-2.10.5 requires one real action per tick).
	block := dt.scopeBlocks[key]
	if block != nil {
		block.NextTrialAt = time.Time{}
	}

	dt.logger.Debug("tracker: dispatching trial action",
		slog.String("scope_key", key.String()),
		slog.String("path", ta.Action.Path),
	)

	// Bypass gates — trial goes directly to workers.
	dt.dispatchReady(ta)
	return true
}

// NextDueTrial returns the scope key and NextTrialAt of the first scope
// block where now >= block.NextTrialAt, or (ScopeKey{}, time.Time{}, false)
// if no trials are due. Thread-safe.
func (dt *DepTracker) NextDueTrial(now time.Time) (ScopeKey, time.Time, bool) {
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

	return ScopeKey{}, time.Time{}, false
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
func (dt *DepTracker) GetScopeBlock(key ScopeKey) (ScopeBlock, bool) {
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
func (dt *DepTracker) ExtendTrialInterval(key ScopeKey, nextAt time.Time, newInterval time.Duration) {
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

// ScopeBlockKeys returns the keys of all active scope blocks. Used by
// handleExternalChanges to detect when perm:dir failures have been cleared
// via CLI. Thread-safe.
func (dt *DepTracker) ScopeBlockKeys() []ScopeKey {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	keys := make([]ScopeKey, 0, len(dt.scopeBlocks))
	for k := range dt.scopeBlocks {
		keys = append(keys, k)
	}

	return keys
}

// ---------------------------------------------------------------------------
// ScopeBlock
// ---------------------------------------------------------------------------

// ScopeBlock represents an active scope-level failure block. When a scope is
// blocked, new actions matching that scope are diverted to the held queue
// instead of being dispatched to workers.
type ScopeBlock struct {
	Key       ScopeKey // typed scope key
	IssueType string   // "service_outage", "quota_exceeded", "rate_limited"

	BlockedAt     time.Time     // when the block was created
	TrialInterval time.Duration // current interval between trial actions (grows with backoff)
	NextTrialAt   time.Time     // when to dispatch the next trial
	TrialCount    int           // consecutive failed trials (for backoff)
}
