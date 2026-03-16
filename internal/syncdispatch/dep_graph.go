package syncdispatch

import (
	"log/slog"
	stdsync "sync"
	"sync/atomic"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// trackedNode is an internal wrapper around *synctypes.TrackedAction that adds
// the dependency-tracking fields (depsLeft, dependents). These fields are
// deliberately not exported on TrackedAction — they are graph internals that
// workers and the engine never need to touch directly.
type trackedNode struct {
	*synctypes.TrackedAction
	depsLeft   atomic.Int32
	dependents []*trackedNode
}

// DepGraph is a pure dependency graph with no channels, no callbacks, and no
// scope awareness. It tracks actions by sequential ID and resolves dependency
// edges: when all of an action's dependencies are satisfied, it becomes ready.
//
// Methods return data — callers decide what to do with ready actions (dispatch
// to channels, check scope gates, etc.). This separation keeps dependency
// tracking independent from scope admission (ScopeGate).
type DepGraph struct {
	mu        stdsync.Mutex
	actions   map[int64]*trackedNode
	byPath    map[string]*trackedNode
	total     atomic.Int32  // total actions added
	completed atomic.Int32  // total actions completed
	done      chan struct{} // closed when completed == total && total > 0
	closeOnce stdsync.Once  // ensures done is closed exactly once
	logger    *slog.Logger

	// emptyCh is closed when actions map hits 0 after a WaitForEmpty call.
	// Nil until WaitForEmpty is called. One-shot per call — callers must
	// call WaitForEmpty again for subsequent emptiness checks.
	emptyCh   chan struct{}
	emptyOnce *stdsync.Once
}

// NewDepGraph creates a new dependency graph. The done channel is closed
// when all added actions have been completed (completed == total && total > 0).
func NewDepGraph(logger *slog.Logger) *DepGraph {
	return &DepGraph{
		actions: make(map[int64]*trackedNode),
		byPath:  make(map[string]*trackedNode),
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

// WaitForEmpty returns a channel that is closed when InFlightCount drops
// to zero. If the graph is already empty, returns a pre-closed channel.
// One-shot: call again for subsequent emptiness checks.
func (g *DepGraph) WaitForEmpty() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.actions) == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	g.emptyCh = make(chan struct{})
	g.emptyOnce = &stdsync.Once{}
	return g.emptyCh
}

// Add inserts an action into the graph. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed/unknown), the
// TrackedAction is returned as immediately ready. Otherwise nil is returned
// and the action waits until Complete() decrements its depsLeft to zero.
func (g *DepGraph) Add(action *synctypes.Action, id int64, depIDs []int64) *synctypes.TrackedAction {
	// Wrap the public TrackedAction in an internal trackedNode that carries
	// the dependency-tracking fields (depsLeft, dependents).
	node := &trackedNode{
		TrackedAction: &synctypes.TrackedAction{
			Action: *action,
			ID:     id,
		},
	}

	g.total.Add(1)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.actions[id] = node
	g.byPath[action.Path] = node

	var depsRemaining int32

	for _, depID := range depIDs {
		dep, ok := g.actions[depID]
		if !ok {
			// Dependency not tracked (already completed or unknown) — skip.
			continue
		}

		dep.dependents = append(dep.dependents, node)
		depsRemaining++
	}

	node.depsLeft.Store(depsRemaining)

	if depsRemaining == 0 {
		return node.TrackedAction
	}

	return nil
}

// Complete marks an action as done, deletes it from both the actions and
// byPath maps (D-10 fix), and decrements the depsLeft counter on all
// dependents. Returns (newly-ready dependents as TrackedAction pointers, true)
// on success.
//
// If id is unknown (not in the graph), a warning is logged and (nil, false)
// is returned. The bool distinguishes "unknown ID" from "known ID with no
// dependents" (which returns a non-nil empty slice).
func (g *DepGraph) Complete(id int64) ([]*synctypes.TrackedAction, bool) {
	g.mu.Lock()

	node, ok := g.actions[id]
	if !ok {
		g.mu.Unlock()
		g.logger.Warn("dep_graph: Complete called with unknown ID",
			slog.Int64("id", id),
		)
		return nil, false
	}

	// Copy dependents under the lock to prevent races with Add() appending
	// to the same slice in watch mode (overlapping passes).
	dependents := make([]*trackedNode, len(node.dependents))
	copy(dependents, node.dependents)

	// D-10 fix: delete from both maps so the completed action doesn't
	// linger. Without this, a subsequent Add could find the completed
	// action, wire a dependency edge to it, and the dependent waits forever.
	delete(g.actions, id)
	// Only delete byPath if it still points to this action. When CancelByPath
	// removes the old entry and Add inserts a new one for the same path,
	// unconditionally deleting would strand the replacement action.
	if g.byPath[node.Action.Path] == node {
		delete(g.byPath, node.Action.Path)
	}

	// Snapshot emptiness while holding lock — the len check is consistent
	// with the delete above. emptyOnce/emptyCh are only set by WaitForEmpty
	// (also under mu), so capturing them here is race-free.
	empty := len(g.actions) == 0
	emptyOnce := g.emptyOnce
	emptyCh := g.emptyCh

	g.mu.Unlock()

	// Collect ready dependents, converting internal trackedNode to the
	// public *synctypes.TrackedAction that callers (engine, tests) expect.
	ready := make([]*synctypes.TrackedAction, 0, len(dependents))

	for _, dep := range dependents {
		if dep.depsLeft.Add(-1) == 0 {
			ready = append(ready, dep.TrackedAction)
		}
	}

	// Check if all actions are done. Close the done channel exactly once.
	if g.completed.Add(1) >= g.total.Load() && g.total.Load() > 0 {
		g.closeOnce.Do(func() { close(g.done) })
	}

	// Signal WaitForEmpty watchers when the actions map is drained.
	if empty && emptyOnce != nil {
		emptyOnce.Do(func() { close(emptyCh) })
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
// Removes the byPath entry so long-lived graphs don't cancel the wrong
// action if the same path is re-added in a subsequent pass.
func (g *DepGraph) CancelByPath(path string) {
	g.mu.Lock()
	node, ok := g.byPath[path]
	if ok {
		delete(g.byPath, path)
	}
	g.mu.Unlock()

	if ok && node.Cancel != nil {
		node.Cancel()
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
