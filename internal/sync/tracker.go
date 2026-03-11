package sync

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
	"time"
)

// Package-level API documentation for tracker.go (B-145):
//
// DepTracker is an in-memory dependency graph that dispatches sync actions
// to a single ready channel as their dependencies are satisfied. It bridges
// the planner's ActionPlan and the WorkerPool:
//
//   - Add(): Insert an action with its sequential ID and dependency IDs.
//     If all deps are satisfied, the action is dispatched immediately.
//   - Complete(): Mark an action done, decrement dependents' counters,
//     and dispatch any dependents that become ready.
//   - HasInFlight() / CancelByPath(): Deduplication for watch mode (B-122).
//   - Ready(): Single channel consumed by WorkerPool.
//
// Two constructors: NewDepTracker (one-shot, Done() fires when all complete)
// and NewPersistentDepTracker (watch mode, workers exit via ctx cancellation).

// watchChanBuf is the channel buffer size for persistent-mode trackers.
// Large enough to absorb typical watch batches without blocking dispatch.
const watchChanBuf = 1024

// errBudgetExhausted is returned by ReQueue when the action has exhausted its
// retry budget (one-shot mode only). The caller should Complete the action as
// failed — the sync pass must end, and the next `onedrive sync` replans naturally.
var errBudgetExhausted = errors.New("tracker: retry budget exhausted")

// TrackedAction pairs an Action with an ID and a per-action cancel function.
// Workers pull TrackedActions from the ready channel. The ID is a sequential
// counter (assigned by the engine) used as a unique key for the tracker's
// internal maps.
type TrackedAction struct {
	Action Action
	ID     int64
	Cancel context.CancelFunc

	// NotBefore is the earliest dispatch time for re-queued actions. Zero
	// means dispatch immediately. Set by ReQueue to implement backoff
	// without blocking workers (R-6.8.7).
	NotBefore time.Time

	// Attempt is the current attempt number (0 = first execution).
	// Incremented by ReQueue on each retry.
	Attempt int

	// MaxAttempts is the retry budget. In one-shot mode, default 5 — after
	// exhaustion, the action completes as failed and the pass moves on.
	// In watch mode, 0 (unlimited) — the tracker retries forever with
	// increasing backoff. The tracker is the sole retry mechanism (R-6.8.10).
	MaxAttempts int

	// IsTrial marks this action as a scope trial — a real action dispatched
	// from the held queue to test whether a blocked scope has recovered (R-2.10.5).
	IsTrial bool

	depsLeft   atomic.Int32
	dependents []*TrackedAction

	// heapIndex is the position in the delayed queue's min-heap. Managed by
	// container/heap and must not be set externally.
	heapIndex int
}

// defaultOneShotBudget is the default retry budget for one-shot mode.
// After this many attempts, the action completes as failed and the pass
// moves on. The next `onedrive sync` run replans naturally.
const defaultOneShotBudget = 5

// DepTracker is an in-memory dependency graph that dispatches actions to a
// single ready channel as their dependencies are satisfied. It is populated
// from the planner's ActionPlan and driven to completion by worker
// Complete() calls.
//
// The tracker is the sole retry mechanism (R-6.8.10). Workers execute and
// report results; the engine classifies results and calls Complete or
// ReQueue. The delayed queue handles backoff timing without blocking workers.
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

	// nowFunc is an injectable clock for deterministic tests. Defaults to
	// time.Now. Used by the delayed queue timer and scope block trials.
	nowFunc func() time.Time

	// delayed is a min-heap of actions ordered by NotBefore. A timer
	// goroutine pops due actions and dispatches them (R-6.8.7, R-2.10.42).
	delayed *delayedQueue

	// held stores actions blocked by scope-level failures (429, 507, 5xx).
	// Key is the scope key (e.g. "throttle:account", "quota:own").
	// Released when the scope block clears (R-2.10.11, R-2.10.15).
	held map[string][]*TrackedAction

	// scopeBlocks tracks active scope-level blocks. An action matching a
	// blocked scope is diverted to the held queue instead of ready.
	scopeBlocks map[string]*ScopeBlock
}

// NewDepTracker creates a tracker for one-shot mode with the given channel
// buffer size. Actions get a default retry budget of defaultOneShotBudget.
func NewDepTracker(bufSize int, logger *slog.Logger) *DepTracker {
	dt := &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		ready:       make(chan *TrackedAction, bufSize),
		done:        make(chan struct{}),
		logger:      logger,
		nowFunc:     time.Now,
		held:        make(map[string][]*TrackedAction),
		scopeBlocks: make(map[string]*ScopeBlock),
	}
	dt.delayed = newDelayedQueue(dt.nowFunc, dt.dispatchReady)
	return dt
}

// NewPersistentDepTracker creates a tracker for watch mode. In persistent mode,
// the global Done() channel never closes — workers exit via context cancellation
// instead. Actions have unlimited retry budget (MaxAttempts=0).
func NewPersistentDepTracker(logger *slog.Logger) *DepTracker {
	dt := &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		ready:       make(chan *TrackedAction, watchChanBuf),
		done:        make(chan struct{}),
		persistent:  true,
		logger:      logger,
		nowFunc:     time.Now,
		held:        make(map[string][]*TrackedAction),
		scopeBlocks: make(map[string]*ScopeBlock),
	}
	dt.delayed = newDelayedQueue(dt.nowFunc, dt.dispatchReady)
	return dt
}

// Add inserts an action into the tracker. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed), the action is
// dispatched immediately. Otherwise it waits until Complete() decrements
// its depsLeft to zero.
func (dt *DepTracker) Add(action *Action, id int64, depIDs []int64) {
	ta := &TrackedAction{
		Action:    *action,
		ID:        id,
		heapIndex: -1, // not in heap
	}
	// Set retry budget based on mode: one-shot gets a finite budget so the
	// pass eventually ends; watch mode retries forever (scope blocks handle
	// scope-level failures, individual items use backoff).
	if !dt.persistent {
		ta.MaxAttempts = defaultOneShotBudget
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

// dispatch routes a ready action through scope and delay gates before sending
// it to the ready channel. This is the central dispatch point — called by Add,
// Complete (for dependents), ReQueue, and releaseScope.
func (dt *DepTracker) dispatch(ta *TrackedAction) {
	// Gate 1: Scope block — prevent wasted requests to blocked scopes.
	// An action matching a blocked scope goes to the held queue instead.
	if key := dt.blockedScope(ta); key != "" {
		dt.held[key] = append(dt.held[key], ta)
		return
	}

	// Gate 2: NotBefore — delayed re-queue actions wait in the min-heap
	// until their backoff expires (R-6.8.7).
	if !ta.NotBefore.IsZero() && dt.nowFunc().Before(ta.NotBefore) {
		dt.delayed.push(ta)
		return
	}

	// Past all gates — send to workers.
	dt.dispatchReady(ta)
}

// dispatchReady sends an action directly to the ready channel, bypassing
// gates. Used by the delayed queue timer when an action's NotBefore has
// expired and by dispatch() after gate checks pass.
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

	return ""
}

// ReQueue increments the attempt counter, sets the NotBefore time, and
// re-enters the action into the dispatch pipeline. In one-shot mode
// (MaxAttempts > 0), returns errBudgetExhausted if the attempt count
// exceeds the budget. In watch mode (MaxAttempts == 0), always succeeds
// and retries forever (R-6.8.10).
//
// Does NOT increment the completed counter — the action stays in-flight.
func (dt *DepTracker) ReQueue(id int64, notBefore time.Time) error {
	dt.mu.Lock()
	ta, ok := dt.actions[id]
	if !ok {
		dt.mu.Unlock()
		return fmt.Errorf("tracker: ReQueue called with unknown ID %d", id)
	}

	ta.Attempt++
	ta.NotBefore = notBefore
	ta.IsTrial = false // clear trial flag on re-queue

	// Budget check: one-shot mode has a finite budget so the pass ends.
	if ta.MaxAttempts > 0 && ta.Attempt >= ta.MaxAttempts {
		dt.mu.Unlock()
		return errBudgetExhausted
	}

	dt.mu.Unlock()

	dt.dispatch(ta)
	return nil
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

// DispatchTrial pops one action from the held queue for the given scope key,
// marks it as a trial (IsTrial=true), and dispatches it directly to the
// ready channel. Returns false if the held queue is empty (R-2.10.5).
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

	dt.logger.Debug("tracker: dispatching trial action",
		slog.String("scope_key", key),
		slog.String("path", ta.Action.Path),
	)

	// Bypass gates — trial goes directly to workers.
	dt.dispatchReady(ta)
	return true
}

// StopDelayed stops the delayed queue timer goroutine. Must be called
// during shutdown to prevent goroutine leaks.
func (dt *DepTracker) StopDelayed() {
	if dt.delayed != nil {
		dt.delayed.stop()
	}
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

// ---------------------------------------------------------------------------
// Delayed queue (min-heap by NotBefore)
// ---------------------------------------------------------------------------

// delayedQueue is a min-heap of TrackedActions ordered by NotBefore. A timer
// dispatches due actions without blocking workers (R-6.8.7, R-2.10.42).
type delayedQueue struct {
	mu       stdsync.Mutex
	items    []*TrackedAction
	timer    *time.Timer
	nowFunc  func() time.Time
	dispatch func(ta *TrackedAction)
	stopped  bool
}

func newDelayedQueue(nowFunc func() time.Time, dispatchFn func(ta *TrackedAction)) *delayedQueue {
	return &delayedQueue{
		nowFunc:  nowFunc,
		dispatch: dispatchFn,
	}
}

// push adds an action to the delayed queue and resets the timer if this
// action has an earlier NotBefore than the current timer target.
func (dq *delayedQueue) push(ta *TrackedAction) {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	heap.Push(dq, ta)
	dq.resetTimerLocked()
}

// stop cancels the timer and prevents further dispatches. Call during
// shutdown to prevent goroutine leaks.
func (dq *delayedQueue) stop() {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	dq.stopped = true
	if dq.timer != nil {
		dq.timer.Stop()
		dq.timer = nil
	}
}

// resetTimerLocked sets the timer to fire when the earliest action becomes
// due. Must be called with dq.mu held.
func (dq *delayedQueue) resetTimerLocked() {
	if dq.stopped || len(dq.items) == 0 {
		return
	}

	if dq.timer != nil {
		dq.timer.Stop()
	}

	earliest := dq.items[0].NotBefore
	delay := earliest.Sub(dq.nowFunc())
	if delay <= 0 {
		// Already due — fire immediately.
		go dq.fireDue()
		return
	}

	dq.timer = time.AfterFunc(delay, dq.fireDue)
}

// fireDue pops all due actions and dispatches them.
func (dq *delayedQueue) fireDue() {
	dq.mu.Lock()
	if dq.stopped {
		dq.mu.Unlock()
		return
	}

	now := dq.nowFunc()
	var due []*TrackedAction
	for len(dq.items) > 0 && !dq.items[0].NotBefore.After(now) {
		ta, _ := heap.Pop(dq).(*TrackedAction) //nolint:errcheck // heap always contains *TrackedAction
		due = append(due, ta)
	}

	// Reset timer for the next earliest, if any remain.
	dq.resetTimerLocked()
	dq.mu.Unlock()

	for _, ta := range due {
		ta.NotBefore = time.Time{} // clear so dispatch() doesn't re-enqueue
		dq.dispatch(ta)
	}
}

// Len returns the number of items in the heap (implements heap.Interface).
func (dq *delayedQueue) Len() int { return len(dq.items) }

// Less reports whether item i should be dequeued before item j (implements heap.Interface).
func (dq *delayedQueue) Less(i, j int) bool {
	return dq.items[i].NotBefore.Before(dq.items[j].NotBefore)
}

// Swap swaps items i and j (implements heap.Interface).
func (dq *delayedQueue) Swap(i, j int) {
	dq.items[i], dq.items[j] = dq.items[j], dq.items[i]
	dq.items[i].heapIndex = i
	dq.items[j].heapIndex = j
}

// Push adds an item to the heap (implements heap.Interface).
func (dq *delayedQueue) Push(x any) {
	ta, _ := x.(*TrackedAction) //nolint:errcheck // heap always contains *TrackedAction
	ta.heapIndex = len(dq.items)
	dq.items = append(dq.items, ta)
}

// Pop removes the minimum item from the heap (implements heap.Interface).
func (dq *delayedQueue) Pop() any {
	old := dq.items
	n := len(old)
	ta := old[n-1]
	old[n-1] = nil // avoid memory leak
	ta.heapIndex = -1
	dq.items = old[:n-1]
	return ta
}
