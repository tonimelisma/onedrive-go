package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
)

// DepGraph is a pure dependency graph with no channels, no callbacks, and no
// scope awareness. It tracks actions by sequential ID and resolves dependency
// edges: when all of an action's dependencies are satisfied, it becomes ready.
//
// Methods return data — callers decide what to do with ready actions (dispatch
// to channels, check scope gates, etc.). This separation enables the
// DepTracker to wrap DepGraph with scope logic without mixing concerns.
type DepGraph struct {
	mu        stdsync.Mutex
	actions   map[int64]*TrackedAction
	byPath    map[string]*TrackedAction
	total     atomic.Int32  // total actions added
	completed atomic.Int32  // total actions completed
	done      chan struct{} // closed when completed == total && total > 0
	closeOnce stdsync.Once  // ensures done is closed exactly once
	logger    *slog.Logger
}

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
	TrialScopeKey ScopeKey

	depsLeft   atomic.Int32
	dependents []*TrackedAction
}

// NewDepGraph creates a new dependency graph. The done channel is closed
// when all added actions have been completed (completed == total && total > 0).
func NewDepGraph(logger *slog.Logger) *DepGraph {
	return &DepGraph{
		actions: make(map[int64]*TrackedAction),
		byPath:  make(map[string]*TrackedAction),
		done:    make(chan struct{}),
		logger:  logger,
	}
}

// Done returns a channel that is closed when all actions have been completed.
// Closing happens exactly once via sync.Once. Returns a never-closed channel
// if total is 0 (no actions added).
func (g *DepGraph) Done() <-chan struct{} {
	return g.done
}

// Add inserts an action into the graph. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed/unknown), the
// action is returned as immediately ready. Otherwise nil is returned and
// the action waits until Complete() decrements its depsLeft to zero.
func (g *DepGraph) Add(action *Action, id int64, depIDs []int64) *TrackedAction {
	ta := &TrackedAction{
		Action: *action,
		ID:     id,
	}

	g.total.Add(1)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.actions[id] = ta
	g.byPath[action.Path] = ta

	var depsRemaining int32

	for _, depID := range depIDs {
		dep, ok := g.actions[depID]
		if !ok {
			// Dependency not tracked (already completed or unknown) — skip.
			continue
		}

		dep.dependents = append(dep.dependents, ta)
		depsRemaining++
	}

	ta.depsLeft.Store(depsRemaining)

	if depsRemaining == 0 {
		return ta
	}

	return nil
}

// Complete marks an action as done, deletes it from both the actions and
// byPath maps (D-10 fix), and decrements the depsLeft counter on all
// dependents. Returns (newly-ready dependents, true) on success.
//
// If id is unknown (not in the graph), a warning is logged and (nil, false)
// is returned. The bool distinguishes "unknown ID" from "known ID with no
// dependents" (which returns a non-nil empty slice).
func (g *DepGraph) Complete(id int64) ([]*TrackedAction, bool) {
	g.mu.Lock()

	ta, ok := g.actions[id]
	if !ok {
		g.mu.Unlock()
		g.logger.Warn("dep_graph: Complete called with unknown ID",
			slog.Int64("id", id),
		)
		return nil, false
	}

	// Copy dependents under the lock to prevent races with Add() appending
	// to the same slice in watch mode (overlapping passes).
	dependents := make([]*TrackedAction, len(ta.dependents))
	copy(dependents, ta.dependents)

	// D-10 fix: delete from both maps so the completed action doesn't
	// linger. Without this, a subsequent Add could find the completed
	// action, wire a dependency edge to it, and the dependent waits forever.
	delete(g.actions, id)
	delete(g.byPath, ta.Action.Path)

	g.mu.Unlock()

	ready := make([]*TrackedAction, 0, len(dependents))

	for _, dep := range dependents {
		if dep.depsLeft.Add(-1) == 0 {
			ready = append(ready, dep)
		}
	}

	// Check if all actions are done. Close the done channel exactly once.
	if g.completed.Add(1) >= g.total.Load() && g.total.Load() > 0 {
		g.closeOnce.Do(func() { close(g.done) })
	}

	return ready, true
}

// HasInFlight returns true if the given path has an in-flight action
// tracked by the graph. Thread-safe.
func (g *DepGraph) HasInFlight(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	_, ok := g.byPath[path]
	return ok
}

// CancelByPath cancels the in-flight action for the given path, if any.
// Removes the byPath entry so long-lived trackers don't cancel the wrong
// action if the same path is re-added in a subsequent pass.
func (g *DepGraph) CancelByPath(path string) {
	g.mu.Lock()
	ta, ok := g.byPath[path]
	if ok {
		delete(g.byPath, path)
	}
	g.mu.Unlock()

	if ok && ta.Cancel != nil {
		ta.Cancel()
	}
}

// InFlightCount returns the number of actions currently in the graph that
// have not yet completed. Accurate because Complete deletes from the
// actions map (D-10 fix).
func (g *DepGraph) InFlightCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.actions)
}
