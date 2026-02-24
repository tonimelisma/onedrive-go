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

// TrackedAction pairs an Action with its ledger ID and a per-action cancel
// function. Workers pull TrackedActions from the interactive or bulk channels.
type TrackedAction struct {
	Action   Action
	LedgerID int64
	Cancel   context.CancelFunc

	depsLeft   atomic.Int32
	dependents []*TrackedAction
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
	done        chan struct{} // closed when all actions complete
	total       int32
	completed   atomic.Int32
	logger      *slog.Logger
}

// NewDepTracker creates a tracker with the given channel buffer sizes.
func NewDepTracker(interactiveBuf, bulkBuf int, logger *slog.Logger) *DepTracker {
	return &DepTracker{
		actions:     make(map[int64]*TrackedAction),
		byPath:      make(map[string]*TrackedAction),
		interactive: make(chan *TrackedAction, interactiveBuf),
		bulk:        make(chan *TrackedAction, bulkBuf),
		done:        make(chan struct{}),
		logger:      logger,
	}
}

// Add inserts an action into the tracker. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed), the action is
// dispatched immediately. Otherwise it waits until Complete() decrements
// its depsLeft to zero.
func (dt *DepTracker) Add(action *Action, ledgerID int64, depIDs []int64) {
	ta := &TrackedAction{
		Action:   *action,
		LedgerID: ledgerID,
	}

	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.actions[ledgerID] = ta
	dt.byPath[action.Path] = ta
	dt.total++

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
// actions are complete, the done channel is closed.
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

		if dt.completed.Add(1) == dt.total {
			close(dt.done)
		}

		return
	}

	dependents := ta.dependents
	dt.mu.Unlock()

	for _, dep := range dependents {
		if dep.depsLeft.Add(-1) == 0 {
			dt.dispatch(dep)
		}
	}

	if dt.completed.Add(1) == dt.total {
		close(dt.done)
	}
}

// CancelByPath cancels the in-flight action for the given path, if any.
func (dt *DepTracker) CancelByPath(path string) {
	dt.mu.Lock()
	ta, ok := dt.byPath[path]
	dt.mu.Unlock()

	if ok && ta.Cancel != nil {
		ta.Cancel()
	}
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
