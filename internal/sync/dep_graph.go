package sync

import (
	"log/slog"
)

// trackedNode is an internal wrapper around *trackedAction that adds
// the dependency-tracking fields (depsLeft, dependents). These fields are
// deliberately not on trackedAction — they are graph internals that
// workers and the engine never need to touch directly.
type trackedNode struct {
	*trackedAction
	depsLeft   int32
	dependents []*trackedNode
}

// depGraph is a dependency graph with no channels, no callbacks, and no
// scope awareness. It tracks actions by sequential ID and resolves dependency
// edges: when all of an action's dependencies are satisfied, it becomes ready.
//
// Methods return data — callers decide what to do with ready actions (dispatch
// to channels, run active-scope admission, etc.). This separation keeps
// dependency tracking independent from scope admission.
//
// The graph has no lock: only the goroutine that runs the engine's RunOnce
// or RunWatch calls its methods. Workers report outcomes on the completion
// channel and never touch it.
type depGraph struct {
	actions map[int64]*trackedNode
	logger  *slog.Logger
	// ownerCheck, when set, runs at the top of every method; nil in
	// production. Tests install a check that fails when the graph is called
	// from a goroutine other than the run's (installOwnerCheck).
	ownerCheck func()
}

// checkOwner runs the graph's owner check, if a test installed one.
func (g *depGraph) checkOwner() {
	if g.ownerCheck != nil {
		g.ownerCheck()
	}
}

// newDepGraph creates a new dependency graph.
func newDepGraph(logger *slog.Logger) *depGraph {
	return &depGraph{
		actions: make(map[int64]*trackedNode),
		logger:  logger,
	}
}

// Add registers an action and wires up its dependencies in a single call. If
// all dependencies are already satisfied (depIDs is empty or all deps already
// completed/unknown), the trackedAction is returned as immediately ready.
// Otherwise nil is returned and the action waits until Complete() decrements
// its depsLeft to zero.
// WARNING: depIDs must reference actions that have ALREADY been added —
// forward references (depID not yet in the graph) are silently dropped.
// Every plan registers through Register and WireDeps (registerPlanActions),
// which take dependencies in any order; tests use Add to build a graph in one
// call.
func (g *depGraph) Add(action *Action, id int64, depIDs []int64) *trackedAction {
	// Wrap the trackedAction in an internal trackedNode that carries the
	// dependency-tracking fields (depsLeft, dependents).
	node := &trackedNode{
		trackedAction: &trackedAction{
			Action: *action,
			ID:     id,
		},
	}

	g.checkOwner()

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

	node.depsLeft = depsRemaining

	if depsRemaining == 0 {
		return node.trackedAction
	}

	return nil
}

// Register adds an action to the graph without wiring any dependencies.
// The action is NOT immediately ready — call WireDeps to resolve
// dependencies and determine readiness. Used in the two-phase pattern
// (Register all, then WireDeps all) to avoid forward-reference issues
// where parent actions depend on children that haven't been added yet.
func (g *depGraph) Register(action *Action, id int64) {
	node := &trackedNode{
		trackedAction: &trackedAction{
			Action: *action,
			ID:     id,
		},
	}

	g.checkOwner()

	g.actions[id] = node

	// Set depsLeft to 1 to prevent premature readiness. WireDeps will
	// set the correct value. This sentinel ensures that if WireDeps is
	// never called, the action stays blocked rather than executing.
	node.depsLeft = 1
}

// WireDeps resolves dependency edges for a previously-registered action.
// All depIDs must reference already-registered actions (guaranteed by
// the Register-all-first pattern). Returns the trackedAction if the
// action is immediately ready (no unresolved deps), nil otherwise.
func (g *depGraph) WireDeps(id int64, depIDs []int64) *trackedAction {
	g.checkOwner()

	node, ok := g.actions[id]
	if !ok {
		g.logger.Debug("dep_graph: WireDeps called with unknown ID",
			slog.Int64("id", id),
		)

		return nil
	}

	var depsRemaining int32

	for _, depID := range depIDs {
		dep, ok := g.actions[depID]
		if !ok {
			// Should not happen with Register-all-first pattern.
			g.logger.Debug("dep_graph: WireDeps references unregistered action",
				slog.Int64("id", id),
				slog.Int64("dep_id", depID),
			)

			continue
		}

		dep.dependents = append(dep.dependents, node)
		depsRemaining++
	}

	node.depsLeft = depsRemaining

	if depsRemaining == 0 {
		return node.trackedAction
	}

	return nil
}

// Get returns the tracked action for the given ID if it is still registered in
// the graph. Callers must treat the returned action as graph-owned runtime
// state and must not mutate dependency bookkeeping fields directly.
func (g *depGraph) Get(id int64) (*trackedAction, bool) {
	g.checkOwner()

	node, ok := g.actions[id]
	if !ok {
		return nil, false
	}

	return node.trackedAction, true
}

// Complete marks an action as done, deletes it from the graph, and decrements
// the depsLeft counter on all dependents. Returns (newly-ready
// dependents as trackedAction pointers, true) on success.
//
// If id is unknown (not in the graph), a warning is logged and (nil, false)
// is returned. The bool distinguishes "unknown ID" from "known ID with no
// dependents" (which returns a non-nil empty slice).
func (g *depGraph) Complete(id int64) ([]*trackedAction, bool) {
	g.checkOwner()

	node, ok := g.actions[id]
	if !ok {
		g.logger.Debug("dep_graph: Complete called with unknown ID",
			slog.Int64("id", id),
		)
		return nil, false
	}

	dependents := node.dependents

	// Delete the completed action so future Add/Register callers treat the ID
	// as already satisfied instead of wiring new dependents to a stale node.
	delete(g.actions, id)

	// Collect ready dependents, converting internal trackedNode to the
	// *trackedAction that callers (engine, tests) expect.
	ready := make([]*trackedAction, 0, len(dependents))

	for _, dep := range dependents {
		dep.depsLeft--
		if dep.depsLeft == 0 {
			ready = append(ready, dep.trackedAction)
		}
	}

	return ready, true
}

// InFlightCount returns the number of actions currently in the graph that have
// not yet completed.
func (g *depGraph) InFlightCount() int {
	g.checkOwner()

	return len(g.actions)
}
