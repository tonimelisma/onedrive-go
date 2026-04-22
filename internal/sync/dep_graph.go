package sync

import (
	"log/slog"
	stdsync "sync"
	"sync/atomic"
)

// trackedNode is an internal wrapper around *TrackedAction that adds
// the dependency-tracking fields (depsLeft, dependents). These fields are
// deliberately not exported on TrackedAction — they are graph internals that
// workers and the engine never need to touch directly.
type trackedNode struct {
	*TrackedAction
	depsLeft   atomic.Int32
	dependents []*trackedNode
}

// DepGraph is a pure dependency graph with no channels, no callbacks, and no
// scope awareness. It tracks actions by sequential ID and resolves dependency
// edges: when all of an action's dependencies are satisfied, it becomes ready.
//
// Methods return data — callers decide what to do with ready actions (dispatch
// to channels, run active-scope admission, etc.). This separation keeps
// dependency tracking independent from scope admission.
type DepGraph struct {
	mu      stdsync.Mutex
	actions map[int64]*trackedNode
	logger  *slog.Logger
}

// NewDepGraph creates a new dependency graph.
func NewDepGraph(logger *slog.Logger) *DepGraph {
	return &DepGraph{
		actions: make(map[int64]*trackedNode),
		logger:  logger,
	}
}

// Add inserts an action into the graph. If all dependencies are already
// satisfied (depIDs is empty or all deps already completed/unknown), the
// TrackedAction is returned as immediately ready. Otherwise nil is returned
// and the action waits until Complete() decrements its depsLeft to zero.
// Add registers an action and wires up dependencies in a single call.
// WARNING: depIDs must reference actions that have ALREADY been added —
// forward references (depID not yet in the graph) are silently dropped.
// For one-shot mode with arbitrary dependency ordering, use the two-phase
// Register + WireDeps approach instead.
func (g *DepGraph) Add(action *Action, id int64, depIDs []int64) *TrackedAction {
	// Wrap the public TrackedAction in an internal trackedNode that carries
	// the dependency-tracking fields (depsLeft, dependents).
	node := &trackedNode{
		TrackedAction: &TrackedAction{
			Action: *action,
			ID:     id,
		},
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.actions[id] = node

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

// Register adds an action to the graph without wiring any dependencies.
// The action is NOT immediately ready — call WireDeps to resolve
// dependencies and determine readiness. Used in the two-phase pattern
// (Register all, then WireDeps all) to avoid forward-reference issues
// where parent actions depend on children that haven't been added yet.
func (g *DepGraph) Register(action *Action, id int64) {
	node := &trackedNode{
		TrackedAction: &TrackedAction{
			Action: *action,
			ID:     id,
		},
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.actions[id] = node

	// Set depsLeft to 1 to prevent premature readiness. WireDeps will
	// set the correct value. This sentinel ensures that if WireDeps is
	// never called, the action stays blocked rather than executing.
	node.depsLeft.Store(1)
}

// WireDeps resolves dependency edges for a previously-registered action.
// All depIDs must reference already-registered actions (guaranteed by
// the Register-all-first pattern). Returns the TrackedAction if the
// action is immediately ready (no unresolved deps), nil otherwise.
func (g *DepGraph) WireDeps(id int64, depIDs []int64) *TrackedAction {
	g.mu.Lock()
	defer g.mu.Unlock()

	node, ok := g.actions[id]
	if !ok {
		g.logger.Warn("dep_graph: WireDeps called with unknown ID",
			slog.Int64("id", id),
		)

		return nil
	}

	var depsRemaining int32

	for _, depID := range depIDs {
		dep, ok := g.actions[depID]
		if !ok {
			// Should not happen with Register-all-first pattern.
			g.logger.Warn("dep_graph: WireDeps references unregistered action",
				slog.Int64("id", id),
				slog.Int64("dep_id", depID),
			)

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

// MarkTrial marks an already-registered action as a scope trial. This lets the
// engine tag an action before it becomes ready, so the metadata survives normal
// dependency resolution instead of relying on a separate pending-trial map.
func (g *DepGraph) MarkTrial(id int64, scopeKey ScopeKey) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	node, ok := g.actions[id]
	if !ok {
		return false
	}

	node.IsTrial = true
	node.TrialScopeKey = scopeKey
	return true
}

// Get returns the tracked action for the given ID if it is still registered in
// the graph. Callers must treat the returned action as graph-owned runtime
// state and must not mutate dependency bookkeeping fields directly.
func (g *DepGraph) Get(id int64) (*TrackedAction, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	node, ok := g.actions[id]
	if !ok {
		return nil, false
	}

	return node.TrackedAction, true
}

// Complete marks an action as done, deletes it from the graph, and decrements
// the depsLeft counter on all dependents. Returns (newly-ready
// dependents as TrackedAction pointers, true) on success.
//
// If id is unknown (not in the graph), a warning is logged and (nil, false)
// is returned. The bool distinguishes "unknown ID" from "known ID with no
// dependents" (which returns a non-nil empty slice).
func (g *DepGraph) Complete(id int64) ([]*TrackedAction, bool) {
	g.mu.Lock()

	node, ok := g.actions[id]
	if !ok {
		g.mu.Unlock()
		g.logger.Warn("dep_graph: Complete called with unknown ID",
			slog.Int64("id", id),
		)
		return nil, false
	}

	// Copy dependents under the lock so concurrent Add/Register callers cannot
	// append to the same slice while completion walks it.
	dependents := make([]*trackedNode, len(node.dependents))
	copy(dependents, node.dependents)

	// Delete the completed action so future Add/Register callers treat the ID
	// as already satisfied instead of wiring new dependents to a stale node.
	delete(g.actions, id)

	g.mu.Unlock()

	// Collect ready dependents, converting internal trackedNode to the
	// public *TrackedAction that callers (engine, tests) expect.
	ready := make([]*TrackedAction, 0, len(dependents))

	for _, dep := range dependents {
		if dep.depsLeft.Add(-1) == 0 {
			ready = append(ready, dep.TrackedAction)
		}
	}

	return ready, true
}

// InFlightCount returns the number of actions currently in the graph that have
// not yet completed.
func (g *DepGraph) InFlightCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.actions)
}
