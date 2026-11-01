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
		engine:  engine,
		deps:    newRuntimeDeps(engine),
		signals: noRuntimeSignals{},
		inspect: noWatchInspector{},
		runID:   engine.nextRuntimeRunID(),
		sched:   newActionScheduler(),
		retries: newRetryLedger(),

		supersedeStreaks: make(map[string]int),
	}
	return flow
}

type oneShotRunner struct {
	*engineFlow
}

func newOneShotRunner(engine *Engine) *oneShotRunner {
	return &oneShotRunner{
		engineFlow: newEngineFlow(engine),
	}
}
