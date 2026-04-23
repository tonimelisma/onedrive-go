// Package sync implements the end-to-end OneDrive sync engine.
//
// Phase order:
//   - startup: prove drive identity, normalize persisted scope/auth state, load baseline
//   - observe_current: refresh remote/local current truth when the entrypoint requires it
//   - load_current_inputs: read committed planner inputs from the store
//   - build_current_plan: build the current actionable set and report
//   - reconcile_runtime_state: prune/load durable retry and scope state
//   - start_runtime: register the graph, admit ready work, and release due held work
//   - publication_drain: keep publication-only actions on the engine/store side
//   - drain_runtime: apply completions, release held work, and append the ready frontier
//
// Durable owners:
//   - current truth and sync control state live in SyncStore tables
//   - run-scoped mutable state lives in engineFlow
//   - watch-only scheduling state, observer-produced channels, and refresh
//     lifecycle live in watchRuntime
//
// Side-effect boundaries:
//   - observation commit helpers own remote/local snapshot writes
//   - reduceReadyFrontierStage owns publication drain plus due-held release
//   - runPublicationDrainStage owns publication-only mutation writes
//   - applyRuntimeCompletionStage owns exact-action completion mutation
//   - runWatchLoop owns watch scheduling, coarse dirty/full-refresh intake, and graceful drain
package sync
