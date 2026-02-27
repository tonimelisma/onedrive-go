package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
)

// smallFileThreshold is the size boundary for routing actions to the
// interactive lane (below) vs the bulk lane (at or above).
const smallFileThreshold = 10 * 1024 * 1024 // 10 MB

// watchChanBuf is the channel buffer size for persistent-mode trackers.
// Large enough to absorb typical watch batches without blocking dispatch.
const watchChanBuf = 1024

// TrackedAction pairs an Action with its ledger ID and a per-action cancel
// function. Workers pull TrackedActions from the interactive or bulk channels.
type TrackedAction struct {
	Action   Action
	LedgerID int64
	Cancel   context.CancelFunc

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

// DepTracker is an in-memory dependency graph that dispatches actions to
// lane-based channels as their dependencies are satisfied. It is populated
// from ledger rows at cycle start and driven to completion by worker
// Complete() calls.
type DepTracker struct {
	mu          stdsync.Mutex
	actions     map[int64]*TrackedAction // ledger ID → tracked action
	byPath      map[string]*TrackedAction
	interactive chan *TrackedAction
	bulk        chan *TrackedAction
	done        chan struct{} // closed when all actions complete (one-shot mode only)
	total       atomic.Int32
	completed   atomic.Int32
	persistent  bool // when true, Done() never fires; workers exit on ctx.Done()
	logger      *slog.Logger

	// Per-cycle completion tracking for watch mode (B-121).
	cyclesMu    stdsync.Mutex
	cycles      map[string]*cycleTracker
	cycleLookup map[int64]string // ledger ID → cycle ID
}

// NewDepTracker creates a tracker with the given channel buffer sizes.
// Current callers pass len(plan.Actions) for both buffers, so dispatch()
// never blocks. When bounded channels are introduced for watch mode
// (concurrent-execution.md §10.2 refill loop), dispatch must be decoupled
// from Complete() to prevent worker deadlock — a worker blocked on a full
// channel inside Complete() cannot drain the channel it's blocking on.
func NewDepTracker(interactiveBuf, bulkBuf int, logger *slog.Logger) *DepTracker {
	return &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		interactive: make(chan *TrackedAction, interactiveBuf),
		bulk:        make(chan *TrackedAction, bulkBuf),
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
		interactive: make(chan *TrackedAction, watchChanBuf),
		bulk:        make(chan *TrackedAction, watchChanBuf),
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
func (dt *DepTracker) Add(action *Action, ledgerID int64, depIDs []int64, cycleID string) {
	ta := &TrackedAction{
		Action:   *action,
		LedgerID: ledgerID,
	}

	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.actions[ledgerID] = ta
	dt.byPath[action.Path] = ta
	dt.total.Add(1)

	// Register with per-cycle tracker if a cycleID is provided.
	if cycleID != "" {
		dt.registerCycleLocked(ledgerID, cycleID)
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

// registerCycleLocked registers a ledger ID with a cycle tracker, creating
// the cycle tracker if it doesn't exist yet. Must be called with dt.mu held.
func (dt *DepTracker) registerCycleLocked(ledgerID int64, cycleID string) {
	dt.cyclesMu.Lock()
	defer dt.cyclesMu.Unlock()

	ct, ok := dt.cycles[cycleID]
	if !ok {
		ct = &cycleTracker{done: make(chan struct{})}
		dt.cycles[cycleID] = ct
	}

	ct.total++
	dt.cycleLookup[ledgerID] = cycleID
}

// Complete marks an action as done and decrements the depsLeft counter on
// all dependents. Any dependent that reaches zero is dispatched. When all
// actions are complete (one-shot mode only), the done channel is closed.
//
// If ledgerID is unknown (not in the tracker), the completed counter is
// still incremented to prevent deadlock, and a warning is logged. This
// should never happen in normal operation but guards against subtle bugs
// in ledger/tracker population.
func (dt *DepTracker) Complete(ledgerID int64) {
	dt.mu.Lock()
	ta, ok := dt.actions[ledgerID]
	if !ok {
		dt.mu.Unlock()
		dt.logger.Warn("tracker: Complete called with unknown ledger ID",
			slog.Int64("ledger_id", ledgerID),
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
	dt.completeCycle(ledgerID)

	// In persistent mode, the global done channel never fires — workers
	// exit via context cancellation instead.
	newCompleted := dt.completed.Add(1)
	if !dt.persistent && newCompleted == dt.total.Load() {
		close(dt.done)
	}
}

// completeCycle advances the per-cycle completion counter. When all actions
// in a cycle have completed, the cycle's done channel is closed.
func (dt *DepTracker) completeCycle(ledgerID int64) {
	dt.cyclesMu.Lock()
	defer dt.cyclesMu.Unlock()

	cycleID, ok := dt.cycleLookup[ledgerID]
	if !ok {
		return
	}

	delete(dt.cycleLookup, ledgerID)

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

// Interactive returns the channel for small/interactive actions.
func (dt *DepTracker) Interactive() <-chan *TrackedAction {
	return dt.interactive
}

// Bulk returns the channel for large/bulk transfer actions.
func (dt *DepTracker) Bulk() <-chan *TrackedAction {
	return dt.bulk
}

// Done returns a channel that is closed when all tracked actions complete.
// In persistent mode (watch), this channel never closes — workers exit via
// context cancellation instead.
func (dt *DepTracker) Done() <-chan struct{} {
	return dt.done
}

// dispatch routes a ready action to the appropriate lane channel based on
// file size and action type. Non-transfer actions always go to interactive.
func (dt *DepTracker) dispatch(ta *TrackedAction) {
	if isLargeTransfer(ta) {
		dt.bulk <- ta
		return
	}

	dt.interactive <- ta
}

// isLargeTransfer returns true if the action is a download or upload with
// a size at or above the small file threshold.
func isLargeTransfer(ta *TrackedAction) bool {
	switch ta.Action.Type { //nolint:exhaustive // only transfers route to bulk
	case ActionDownload, ActionUpload:
		return actionSize(ta) >= smallFileThreshold
	default:
		return false
	}
}

// actionSize extracts the file size from the action's view, preferring
// remote state for downloads and local state for uploads.
func actionSize(ta *TrackedAction) int64 {
	if ta.Action.View == nil {
		return 0
	}

	if ta.Action.View.Remote != nil {
		return ta.Action.View.Remote.Size
	}

	if ta.Action.View.Local != nil {
		return ta.Action.View.Local.Size
	}

	return 0
}
