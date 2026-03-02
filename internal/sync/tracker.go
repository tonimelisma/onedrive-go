package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
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
//     and dispatch any dependents that become ready. Advances per-cycle
//     completion tracking for delta token commits (B-121).
//   - HasInFlight() / CancelByPath(): Deduplication for watch mode (B-122).
//   - Ready(): Single channel consumed by WorkerPool.
//   - CycleDone() / CleanupCycle(): Per-cycle lifecycle for watch mode.
//
// Two constructors: NewDepTracker (one-shot, Done() fires when all complete)
// and NewPersistentDepTracker (watch mode, workers exit via ctx cancellation).

// watchChanBuf is the channel buffer size for persistent-mode trackers.
// Large enough to absorb typical watch batches without blocking dispatch.
const watchChanBuf = 1024

// TrackedAction pairs an Action with an ID and a per-action cancel function.
// Workers pull TrackedActions from the interactive or bulk channels. The ID
// is a sequential counter (assigned by the engine) used as a unique key for
// the tracker's internal maps.
type TrackedAction struct {
	Action  Action
	ID      int64
	CycleID string
	Cancel  context.CancelFunc

	depsLeft   atomic.Int32
	dependents []*TrackedAction
}

// cycleTracker tracks completion of actions within a single planning cycle.
// Used in watch mode (persistent tracker) to know when all actions from one
// batch have finished, so the delta token can be safely committed.
type cycleTracker struct {
	total     int32
	completed atomic.Int32
	done      chan struct{}
}

// DepTracker is an in-memory dependency graph that dispatches actions to a
// single ready channel as their dependencies are satisfied. It is populated
// from the planner's ActionPlan and driven to completion by worker
// Complete() calls.
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

	// Per-cycle completion tracking for watch mode (B-121).
	cyclesMu    stdsync.Mutex
	cycles      map[string]*cycleTracker
	cycleLookup map[int64]string // action ID → cycle ID
}

// NewDepTracker creates a tracker with the given channel buffer size.
// Current callers pass len(plan.Actions) so dispatch() never blocks.
func NewDepTracker(bufSize int, logger *slog.Logger) *DepTracker {
	return &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		ready:       make(chan *TrackedAction, bufSize),
		done:        make(chan struct{}),
		logger:      logger,
		cycles:      make(map[string]*cycleTracker),
		cycleLookup: make(map[int64]string),
	}
}

// NewPersistentDepTracker creates a tracker for watch mode. In persistent mode,
// the global Done() channel never closes — workers exit via context cancellation
// instead. Channel buffers are sized for continuous operation.
func NewPersistentDepTracker(logger *slog.Logger) *DepTracker {
	return &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		ready:       make(chan *TrackedAction, watchChanBuf),
		done:        make(chan struct{}),
		persistent:  true,
		logger:      logger,
		cycles:      make(map[string]*cycleTracker),
		cycleLookup: make(map[int64]string),
	}
}

// Add inserts an action into the tracker. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed), the action is
// dispatched immediately. Otherwise it waits until Complete() decrements
// its depsLeft to zero.
//
// cycleID groups the action with a planning cycle for per-cycle completion
// tracking (B-121). Pass empty string for one-shot mode (no cycle tracking).
func (dt *DepTracker) Add(action *Action, id int64, depIDs []int64, cycleID string) {
	ta := &TrackedAction{
		Action:  *action,
		ID:      id,
		CycleID: cycleID,
	}

	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.actions[id] = ta
	dt.byPath[action.Path] = ta
	dt.total.Add(1)

	// Register with per-cycle tracker if a cycleID is provided.
	if cycleID != "" {
		dt.registerCycleLocked(id, cycleID)
	}

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

// registerCycleLocked registers an action ID with a cycle tracker, creating
// the cycle tracker if it doesn't exist yet. Must be called with dt.mu held.
func (dt *DepTracker) registerCycleLocked(id int64, cycleID string) {
	dt.cyclesMu.Lock()
	defer dt.cyclesMu.Unlock()

	ct, ok := dt.cycles[cycleID]
	if !ok {
		ct = &cycleTracker{done: make(chan struct{})}
		dt.cycles[cycleID] = ct
	}

	ct.total++
	dt.cycleLookup[id] = cycleID
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
	// to the same slice in Phase 5.1+ (watch mode overlapping cycles).
	dependents := make([]*TrackedAction, len(ta.dependents))
	copy(dependents, ta.dependents)

	// Clean up byPath so long-lived trackers (watch mode) don't cancel
	// the wrong action if the same path appears in a subsequent cycle.
	delete(dt.byPath, ta.Action.Path)
	dt.mu.Unlock()

	for _, dep := range dependents {
		if dep.depsLeft.Add(-1) == 0 {
			dt.dispatch(dep)
		}
	}

	// Advance per-cycle tracker.
	dt.completeCycle(id)

	// In persistent mode, the global done channel never fires — workers
	// exit via context cancellation instead.
	newCompleted := dt.completed.Add(1)
	if !dt.persistent && newCompleted == dt.total.Load() {
		close(dt.done)
	}
}

// completeCycle advances the per-cycle completion counter. When all actions
// in a cycle have completed, the cycle's done channel is closed.
func (dt *DepTracker) completeCycle(id int64) {
	dt.cyclesMu.Lock()
	defer dt.cyclesMu.Unlock()

	cycleID, ok := dt.cycleLookup[id]
	if !ok {
		return
	}

	delete(dt.cycleLookup, id)

	ct, ok := dt.cycles[cycleID]
	if !ok {
		return
	}

	if ct.completed.Add(1) == ct.total {
		close(ct.done)
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
// action if the same path is re-added in a subsequent cycle.
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

// CycleDone returns a channel that is closed when all actions in the given
// cycle have completed. Returns a closed channel for unknown cycle IDs
// (defensive: prevents callers from blocking forever).
func (dt *DepTracker) CycleDone(cycleID string) <-chan struct{} {
	dt.cyclesMu.Lock()
	defer dt.cyclesMu.Unlock()

	ct, ok := dt.cycles[cycleID]
	if !ok {
		// Unknown cycle — return a closed channel so the caller doesn't block.
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	return ct.done
}

// CleanupCycle removes a completed cycle from the tracker's cycle map,
// preventing unbounded growth in long-running watch sessions.
func (dt *DepTracker) CleanupCycle(cycleID string) {
	dt.cyclesMu.Lock()
	defer dt.cyclesMu.Unlock()

	delete(dt.cycles, cycleID)
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

// dispatch sends a ready action to the ready channel.
func (dt *DepTracker) dispatch(ta *TrackedAction) {
	dt.ready <- ta
}
