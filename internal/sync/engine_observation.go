package sync

// observation is the part of a run that code off the loop goroutine may
// reach: the engine's collaborators (engine), the run's configuration (cfg)
// and its capabilities (deps). It holds no run state: the scheduler, the
// ledgers, the outbox, the timers and the loop's resources live on the flow
// and the runtime, which embed an observation but are not reachable from
// one.
//
// Every goroutine the watch starts for observation (the full remote refresh,
// the remote and local observers, the socket.io wake source) runs methods of
// *observation only, and gets the channels it reports on as send-only
// arguments, so the compiler keeps it from reading loop state.
type observation struct {
	// engine is the collaborators and engine-owned state the run still
	// reaches (fetchers, the protected-root set, the housekeeper).
	engine *Engine

	// cfg is the engine's configuration, copied when the run starts.
	cfg runConfig

	// deps are the capabilities this run needs regardless of what it decides.
	// See runtimeDeps for why they are passed rather than reached for.
	deps runtimeDeps
}
