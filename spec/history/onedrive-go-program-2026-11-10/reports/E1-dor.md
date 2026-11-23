# E1 Definition of Ready: guarded protected-roots read, dead code, stale comments

Worktree `~/src/onedrive-go-wt/engine-guarded-read`, branch `fix/engine-guarded-read`, base `83fe1f82`.

## Items checked against the tree

| Item | Where it is today (quoted) | Status |
|------|---------------------------|--------|
| P1 unguarded read | `engine_housekeeping.go:96` `SkipDirs: transferArtifactCleanupSkipDirs(e.contentFilter, e.protectedRoots),` in `cleanTransferArtifacts`, called by `postSyncHousekeeping` after `refreshProtectedRootsForHousekeeping`. The third writer is `acknowledgeChildFinalDrain` (`engine_shortcut_topology.go:39`) -> `reconcileShortcutRootLocalState` (`shortcut_root_lifecycle.go:75`) -> `refreshProtectedRootsFromStore` -> `setProtectedRoots`. No caller of `cleanTransferArtifacts` holds `protectedRootsMu` (the lock is taken only inside `setProtectedRoots`/`currentProtectedRoots`, deferred unlock, no calls under it): no deadlock | open |
| P2a stored cancel | `tracked_action.go:15` `Cancel context.CancelFunc`; written at `worker.go:192` `ta.Cancel = cancel`; `grep -rlw Cancel` over the module: no reader in any file | open |
| P2b trial marking | `dep_graph.go:159` `func (g *depGraph) MarkTrial(id int64, scopeKey ScopeKey) bool {`; `grep -rlw MarkTrial`: only `dep_graph.go` | open |
| P2c disk-space field | `engine.go:77` `diskAvailableFn       func(string) (uint64, error)`, assigned `engine.go:251` `diskAvailableFn:        driveops.DiskAvailable,`; `grep -rl diskAvailableFn`: only `engine.go` | open |
| P2d stale graph capture | `engine_watch.go:189` `depGraph := newDepGraph(rt.deps.logger)`, read in cleanup `engine_watch.go:243` `inFlight := depGraph.InFlightCount()`; `startRuntimeStage` (`engine_runtime_start.go:24`) replaces `sched.graph` for every plan and is the only registrar (`:99`), so the captured graph is always empty and its log line never fires | open |
| P3a "Set by DispatchTrial" | `tracked_action.go:21-22` `// TrialScopeKey identifies which scope this trial is testing. Set by` / `// DispatchTrial, ...`; writers today: `drainDueHeldWorkNow` (`engine_runtime_lifecycle.go:622`), `releaseScopeBatch` (`engine_runtime_release_ramp.go:229`) | open |
| P3b executor field name | `executor.go:41` `// expectedSyncRoot is the sync folder identity the engine validated;` above field `syncRootIdentity` | open |
| P3c runtime owner struct | `engine_runtime_owners.go:63-64` `// mu guards active and nothing else. It is the one field here read outside` / `// the runtime goroutine, by admission checks and status snapshots.`; every caller of the eight `scopeLedger` methods is an `engineFlow`/`watchRuntime` method on the runtime goroutine; none in the refresh, observer or primary-root watch goroutines | open |
| P3d dependency graph | `dep_graph.go:27-28` `// mu guards actions. The engine adds and completes nodes while workers` / `// report outcomes.`; workers report on the completion channel (`worker.go:48`, `:58`) and never call the graph | open |
| P3e engine fields | `engine.go:139-143` `... Atomic for the same reason as the flag above.` over `protectedRootRefreshFailing` and the three housekeeping flags; their only callers are `postSyncHousekeeping` (`engine_run_once.go:142`, `engine_watch.go:334`) and watch maintenance (`engine_watch_maintenance.go:15`), all on the goroutine running the engine's run | open |
| P3f engine field, writers | `engine.go:64-68` names "the full-refresh goroutine and ... the remote observer's expired-delta recovery", omits the ack handle | open |
| P3g `protected_roots.go` | `protected_roots.go:12-15` same two writers, omits the ack handle | open |
| Spec | `spec/design/sync-engine.md:66-70` (ownership) names the same two off-loop writers; verification row `:111`. No spec file names `MarkTrial`, `diskAvailableFn`, `DispatchTrial`, `expectedSyncRoot`, the cancel field or the cleanup log line | open |

## Checklist

1. PASS **Task intent**: repo-maintenance outcome: housekeeping reads the protected roots through `currentProtectedRoots`, four pieces of code nothing reads are deleted, and seven comments say what the code does.
2. PASS **Non-goals**: the vestigial locks and atomics (kept as they are), the retry ledger, observer start/stop, watch phases, the two "blocked retry work" comments over the scope-discard SQL, the second copy of the scope discard, receiver names; no behaviour change other than the guarded read.
3. PASS **Governing docs**: `spec/design/sync-engine.md` (engine files, ownership contract, verification table), `spec/design/sync-execution.md` (`dep_graph.go`, `worker.go`, `executor.go`), `spec/design/sync-domain.md` (`tracked_action.go`), `spec/design/sync-shortcut.md` (`protected_roots.go`), `spec/requirements/non-functional.md` R-6.3.4 and R-6.3.5.
4. PASS **Authority owner**: the protected roots are owned by `Engine` under `protectedRootsMu`, rederived only by `refreshProtectedRootsFromStore`; housekeeping is owned by the goroutine running the engine's run; the dependency graph and scope ledger by the runtime goroutine. No durable state or user-facing output changes (the deleted log line never printed).
5. PASS **Code scope**: `internal/sync`: `engine_housekeeping.go`, `engine.go`, `protected_roots.go`, `tracked_action.go`, `worker.go`, `dep_graph.go`, `engine_watch.go` (two hunks only; another branch is in it), `executor.go`, `engine_runtime_owners.go`, `engine_housekeeping_test.go`; `spec/design/sync-engine.md`. Not touched: multisync, store files, observers.
6. PASS **Regression plan**: new `TestPostSyncHousekeeping_ReadsProtectedRootsUnderTheLock` (one goroutine loops the ack handle's final-drain acknowledgement, another loops housekeeping), run under `-race` on the base first, then after the fix with `-count=20`.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, tagged vet of `./e2e/... ./internal/graph/... ./internal/devtool/...`; `go test -count=1 ./internal/sync/...` (behind the heavy lock) and `./internal/multisync/...`; one `-race` run of the touched tests' names; the new test `-race -count=20`; lint of `./internal/sync/...` last, behind the lock; spec citations checked by hand.
8. PASS **Data safety**: no local or remote delete, overwrite or store change; the cleanup's skip list is the same list, read under the lock.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or logs from a live run are involved.
10. PASS **Live provider and destructive operations**: none; no e2e, no live account, no dependency change, no history rewrite.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does the race reproduce under `-race` on the base? Current assumption: yes, when an ack's write lands between housekeeping's refresh and its next store read. Validation step: run the new test on the base with `-race -count=20` before the fix. Owner document/code path affected if wrong: the test (made certain by construction), `engine_housekeeping_test.go`.
    - Question: does R-6.3.4 fit the test? Current assumption: yes: the field comment cites it for "read through `currentProtectedRoots`, write through `setProtectedRoots`", and the existing concurrency test of the same field validates it. Validation step: read R-6.3.4 (done: "document the single owner of ... mutex-guarded fields"). Owner: the test's `Validates:` line.
