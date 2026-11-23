# R7 report: engine construction (E6, E11, E13, E7, E8)

Branch `refactor/engine-construction` in `~/src/onedrive-go-wt/engine-construction`, on `f0ac23e5`. Five commits, one per row; 132 files, about +2000/-1300. No change to which file operations a sync performs.

## Commits
- E6 `bc607bad` Gather the engine's test seams into one hooks value: `engineHooks` (engine_hooks.go) on `engineInputs.hooks` and `Engine.hooks`; 14 seams moved (assertInvariants, ownerCheckFactory, dispatchOrder, serializeWorkers, oneShotPassLimit, wait, reconciliationQuery, clearAccountAuthRequirement, clock, watcher and wake-source factories, lockFlock, interposeSyncTree, storeDriver). `runClock()` replaces the production-assigned `e.clock`; the wake-source default is resolved at use. New R-6.10.29.
- E11 `d61bb3e4` Give the executor, store and observer one hooks value each: `syncexec.Hooks` {CrashAt, HashFile, HashReader, StoreAccessTimeout, OutcomeCommitTimeout}, `syncstore.Hooks` {CrashAt, Driver}, `syncobserve.Hooks` {WatcherFactory, HashFile, AfterSafetyScan, BeforeWatchLoop, WriteCoalesceCooldown}; engineHooks carries exec/store/observe and passes them through. Deleted SetCrashHookForTest (both), SetStoreAccessTimeoutForTest, SetOutcomeCommitTimeoutForTest, SetWatcherFactory, OpenEngineSyncStoreWithDriver. Crash tests arm a test-engine crash switch built in at construction. devtool test-conventions check: no hooks-typed literal with fields and no `x.hooks.F =` in non-test files.
- E13 `15fa00c3` Build the executor configuration and worker pools from input structs: `NewExecutorConfig(*ExecutorInputs) (*ExecutorConfig, error)` refusing a missing required field (Items, Downloads, Uploads, Transfers, SyncTree, DriveID, Logger, Now); `NewWorkerPool(WorkerPoolInputs)` with Perf and Serialize. Deleted all nine setters, SetRuntimeOptions, SetItemsForTest, SetLoggerForTest, LoggerForTest, Items/Downloads/Uploads getters, configureEngineExecutor. New R-6.10.30.
- E7 `8efa4aed` Hand each run its configuration, capabilities and hooks: `runConfig`, `flowInputs`, `Engine.flowInputs()` (also takes the run ID), `newEngineFlow(*flowInputs)`; runtimeDeps gains perf. Flow reads of config, store/logger/clock/events/perf and hooks no longer go through `flow.engine`; four engine nil-checks dropped; receivers `f`->`flow`, `eng`->`e`.
- E8 `a4d973aa` Give the protected roots and housekeeping their own owners: `protectedRootSet` (engine_protected_roots.go) and `housekeeper` (engine_housekeeping.go, purge time as time.Time, streaks as plain fields); `reportStreakOutcome` is a function of its logger.

HEAD and gate results: see the end of this file.

## Tests added (9)
- TestEngineHooks_ZeroValueIsProduction (sync); TestHooks_ZeroValueIsProduction in syncexec, syncstore, syncobserve (R-6.10.29)
- TestRunTestConventionsRefusesAProductionFileThatSetsAHook (devtool, R-6.10.29)
- TestNewExecutorConfig_RejectsEachMissingRequiredField (syncexec, R-6.10.30)
- TestEngine_EachRunGetsADistinctRunID (R-6.6.13), TestEngine_FlowInputsCopyTheConfigurationAndHooks (R-6.10.29)
- TestRunWatch_HousekeepingRunsBeforeTheFirstRefreshTimerIsArmed (R-2.4.10; checked to fail with the order swapped)

## Gates
- `verify --dod --stage start`: nightly run on 197f4d24 concluded success, nothing claimed or released (no failing tests, so the four parked ones had no claims to release).
- `go test -short -p 1` of internal/sync/..., devtool, multisync, cli after E13: all ok. Each commit builds and vets (`git rebase --exec`).
- Targeted golangci-lint (sync/..., devtool): 11 findings, all fixed into their commits; the pre-pr lint step then passed.
- pre-pr run 1: mutex-doc check (fixed). Run 2: short tests failed in sync (5 tests mutating engine config after the test flow existed, now refreshed through `refreshTestFlowInputs`), syncstore (stored-time site table listed engine_housekeeping.go) and devtool (stress member list); all fixed into their commits.

## Deviations
- E13: `remoteParentChecks` stays in the config and `TestWorkerPool_ParentChecksAreResetPerPlanAndSharedByWorkers` was not written (R7-1); Trash/PendingCreates stay optional (R7-2).
- E11: the stores' and observers' clocks are not hooks (production sets them to the engine clock), so `syncstore.Hooks` has no Now and `syncobserve.Hooks` no Clock; the observer gets its hooks through `SetHooks`, not a constructor input (NewLocalObserver has 138 call sites).
- E7: `flow.engine` remains for collaborators and owned state (deleted by E10). The run owner check still reads `engine.runOwnerCheck`.
- Flows now copy config and hooks at run start: a test that changes an engine field after building a flow must refresh it (helper added).

## Parked
R7-1 to R7-7 appended to PARKED.md (parent checks, optional trash, seams outside hooks, store/observer setters, tests mutating engine config, totalWorkers vs poolSize floor, duplicate trash/sessionStore pointers).

## Open questions
- R7-6: whether `watchRuntime.totalWorkers` omitting the pool floor is intended: unknown.

## Final
- HEAD `a4d973aa`, pushed to `origin/refactor/engine-construction` (no PR).
- pre-pr run 3: pass (lint, format, conventions, governs, mutex docs, traceability, build, tagged vet, short unit tests; rebase and manifest gates passed; nightly on 197f4d24 success).
- `onedrive-go-install` done: `onedrive-go version a4d973aa`. Not done here: the CLAUDE.md "currently" commit and the CHANGELOG install entry in ~/agent.
- Not run: `-race` (at ship), the non-short unit tier (skipped tests may still mutate engine config after a flow exists; see R7-5).

## Merged as #963
- Review fixes folded into their commits: S1 purge time stored with `Round(0)` (wall-time interval as before; E8), S2 hook check walks the whole selector, nested case added and seen failing (E11), S4 config reads (account, mount root, scan coverage) through `flow.cfg` (E7); the engine-level hook reads parked as R7-8. Nits not taken.
- `-race -p 1 -timeout 150m` over devtool, sync, syncexec, syncobserve, syncdomain, syncstore and syncplan: all ok (internal/sync 6516 s). pre-pr, pre-merge passed; squash-merged as `f2392533`, installed. Live `TestE2E_Sync_UploadOnly` passed (103 s).
