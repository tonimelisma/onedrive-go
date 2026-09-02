package sync

// engineFlow holds mutable per-run execution state shared by one-shot and
// watch coordinators. The Engine itself remains an immutable dependency
// container; run-scoped state lives here instead.
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
