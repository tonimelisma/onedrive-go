package sync

// engineFlow holds mutable per-run execution state shared by one-shot and
// watch coordinators. The Engine itself remains a dependency container whose
// fields are fixed after construction, apart from the few it guards itself
// (protectedRoots under protectedRootsMu, the atomic counters); run-scoped
// state lives here instead.
type engineFlow struct {
	engine *Engine

	// deps are the capabilities this run needs regardless of what it decides.
	// See runtimeDeps for why they are passed rather than reached for.
	deps runtimeDeps

	// signals is how this flow asks for future work. Watch supplies the live
	// runtime; one-shot supplies no-ops, because a one-shot pass has no future
	// in which a timer could fire. See runtimeSignals.
	signals runtimeSignals

	// inspect is the debug-only view of watch state used by the invariant
	// pass, which production leaves disabled.
	inspect watchInspector

	// admitted is the runtime holding admitted, not yet dispatched work. A
	// block scope that activates takes back what it blocks through it
	// (R-6.8.24). Watch and one-shot each supply their own outbox; a bare
	// flow has none.
	admitted admittedWorkOwner

	// The four owners below split this run's mutable state by authority; see
	// engine_runtime_owners.go. One goroutine still owns all four.
	sched   actionScheduler
	retries retryLedger
	scopes  scopeLedger
	results runResults

	runID string

	// supersedeStreaks counts consecutive superseded completions per action
	// key (type, old path, path) and resets when that action succeeds. A
	// superseded action leaves no retry row and no user-facing output, so an
	// action that is superseded on every replan would otherwise stall
	// silently (a folder move once did, on a churning folder eTag).
	supersedeStreaks map[string]int
}

func newEngineFlow(engine *Engine) *engineFlow {
	flow := &engineFlow{
		engine:   engine,
		deps:     newRuntimeDeps(engine),
		signals:  noRuntimeSignals{},
		inspect:  noWatchInspector{},
		admitted: noAdmittedWork{},
		runID:    engine.nextRuntimeRunID(),
		sched:    newActionScheduler(),
		retries:  newRetryLedger(),

		supersedeStreaks: make(map[string]int),
	}
	return flow
}

type oneShotRunner struct {
	*engineFlow

	// outbox is the admitted work the results loop has not handed to a
	// worker yet. Owned by the results loop goroutine; a scope activation
	// applied by that goroutine takes blocked work back out of it.
	outbox []*trackedAction
}

func newOneShotRunner(engine *Engine) *oneShotRunner {
	runner := &oneShotRunner{
		engineFlow: newEngineFlow(engine),
	}
	runner.admitted = runner

	return runner
}

// takeAdmittedWorkBlockedBy satisfies admittedWorkOwner for the one-shot
// results loop.
func (r *oneShotRunner) takeAdmittedWorkBlockedBy(key ScopeKey) []*trackedAction {
	taken, kept := takeOutboxBlockedBy(r.outbox, key)
	if len(taken) == 0 {
		return nil
	}
	r.outbox = kept

	return taken
}
