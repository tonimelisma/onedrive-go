# W12 report: sync test harness proof against load

Branch `test/sync-harness-leftovers`, one commit d0ef3c95 on `origin/main` 27e76cc3 (all passes and the review fix amended into one); 39 files, +1115 -1016.

## What changed
- `internal/sync/test_helpers_test.go`: `eventWait` (one minute), the one named wait for an event that must come.
- 26 test files: 120 fixed waits (literals 1-30 s, `debugEventTimeout`, `dirtyBufferTestTimeout`, `gatedArrivalTimeout`, socket.io `testWaitTimeout`; `time.After`, `time.NewTimer`, `Eventually`, `context.WithTimeout`) wait `eventWait`; the four local constants are gone. Includes all four waits of `engine_one_shot_quiescence_test.go`, the measured one among them.
- `watch_test_helpers_test.go`: the run and receive helpers (below); `stopWatch` and `runWatchInBackground` moved here from `observer_local_watch_health_test.go`.
- `chaos_remote_session_test.go` (new): the chaos layer on the session route and its test. `scenario_remote_session_test.go`: `chaos` field, every session call goes through it; `dropSession`, `holdsSession`.
- `chaos_remote_test.go`: `chaosMutation` generic; `chaosOpCreate`, `chaosResponseLostSuffix`, `chaosCreateResponseLost`; `chaosRaceSeed` = 2 and its assertion. `engine_model_based_test.go`: the fault summary is logged from `t.Cleanup`.
- 18 test files: `// Validates:` on 112 tests that had none (below).
- `spec/design/system.md` Verification Policy: the event-wait rule, the rule that no wait for a watch or loop to return is unbounded, the chaos run's rules.

## Runs and bounded receives
- `runInBackground(t, ctx, run)`: the one owner of a watch or loop goroutine; its cleanup cancels and waits within `eventWait`. A run whose goroutine ends without returning (a `require` failed on it) now reports that at once. 18 direct sites (observer and remote `Watch`, socket.io `Run`, `runWatchLoop`, the results loop, the result-drain helper, three stub consumers) and under every helper below.
- `runWatchInBackground(t, eng, mode, opts)`: every `RunWatch` goroutine, 21 sites. `startMockWatch`/`startLocalWatch` (38 sites) start through `runInBackground`. `quietWatchOptions()` names options 16 tests repeated.
- `runWithin(t, ctx, what, run)`: a test's own call of a watch or loop, bounded and named; 25 sites: 23 inline calls (`Watch` 14, socket.io `Run` 4, the results loop 2, `bootstrapSync` 3) and two hand-rolled equivalents folded into it.
- `receiveWithin(t, ch, what)`: 64 sites, every unbounded `done` receive among them; it replaces `receiveBatch`, `waitForSignal`, `waitForCapturedTimer` (removed). `stopWatch(t, cancel, done)`: 60 sites, the repeated cancel-and-select blocks; results once ignored are now checked.
- Left inline, returning at once by the state set before the call (no event involved): `engine_watch_loop_completion_test.go:29`, `:41` and `engine_watch_loop_owner_test.go:56` (quiescent bootstrap); `engine_watch_loop_owner_test.go:127`, `:146`, `:169`, `:192` and `engine_phase0_test.go:338` (context canceled first); `engine_watch_loop_owner_test.go:110`, `:212`, `:275` and `engine_watch_bootstrap_test.go:53` (signal or channel close already queued); `engine_phase0_test.go:315`, `:358` (nothing to run); `engine_current_plan_test.go:365` (argument check). Receives inside `testing/synctest` bubbles stay too: a blocked one is a reported deadlock.

## Validates
160 tests in the files this commit touches had none; 112 now name the requirement their assertions prove (every ID checked against `spec/requirements`). 48 prove no requirement and carry none:
- Helpers with no product requirement: `TestExtractHTTPStatus_*` (4), `TestExtractRetryAfter_*` (3), `TestSkipEntry_*` (3), `TestEstimateDirCount_*` (2), `TestClassifyItemType`, `TestChangeEventsToObservedItems_RemoteOnly`, `_MapsAllFields`, `TestFullDelta_FolderEvent`, `TestFindOrphans_WithPathPrefix` (branch no caller uses).
- Test-only code: `TestItemTypeFromDirEntry`, `_Table`, `TestNewTestStore_TemplateMatchesFreshStore`.
- Internal mechanics no requirement states: `TestWorkerPool_ClosedDispatchChannelClosesCompletions`, `_ContextCancellationClosesCompletions`, `_StopIsIdempotentAfterCompletionsClose`, `_SendResultDropsWhenContextCancellationWins`, `_PanicRecovery`, `TestWatchRuntime_RunWatchLoop_BootstrapPhaseQuiescesAndReturnsToRunning`, `TestWatchRuntime_RunNonDrainingWatchStep_BootstrapDispatchUsesSharedHandler`, `TestWaitForQuiescence_EmptyGraph`, `TestExecutePlan_ActionsDepsLengthMismatch`, `TestFullDelta_SkipsRootItem`, `TestNewLocalObserver_MapsInitialized`, `TestWatch_IntervalClamping`, `TestWatch_BatchHandlerError_ReturnsError`, `TestRunFullReconciliationAsync_SkipLogPromotedToInfo`.
- Accessors no production code reads: `TestRemoteObserver_LastActivity`, `TestRemoteObserver_Stats`, `TestObserverStats_HashesComputed`.
- Smoke or propagation only: `TestFullScan_EmptyDir`, `TestFullDelta_EmptyDelta`, `_FetchError`, `_ContextCanceled`, `TestBootstrapSync_NoChanges`, `TestRunOnce_DeltaTokenPersisted`, `TestRunOnce_EmptyPlan_NoPanic`, `TestRunWatch_ProcessBatch_EmptyPlan`, `TestEngine_Close_CleansStaleAndIsIdempotent`, `TestNewEngine_ZeroDriveID_ReturnsError`, `TestNewEngine_InvalidDBPath`.

## Tests
- `TestChaosGraph_SessionRouteDrawsFaults` (new): each session-route fault is the failure it models; failed before the routing with all seven counts zero.
- `TestModel_ChaosGraphConverges`: the race seed must draw `create:response-lost` and `upload-session:timeout-after-create`; red with seed 1 under `-race`, seeds 1-16 measured.

## Checks
On the final tree, in this order: `go vet ./internal/sync/...` clean; `gofumpt -l`, `goimports -l` on the touched files clean; `-run` over the 106 touched tests `ok` 10.3 s; `go test -count=1 -timeout 60m ./internal/sync/...` `ok` 438.5 s; `go test -race -cpu 1 -count=1 -run` over the 106 `ok` 53.5 s; lint `./internal/sync/...` once, last, `0 issues.` (W12-lint.log). No `B-NNN` citation removed. Earlier, for the record: the load proof, two copies side by side of `-race -cpu 1` over the watch and observer tests, `ok` 298.6 s and 305.2 s (the measured test included).

## Waits left short (their expiry is the passing path)
- Must not happen: `engine_watch_test.go:175`, `:216` (100 ms); `observer_local_collisions_test.go:247`, `:350`, `:394`, `:499`, `:584`, `observer_local_write_test.go:438`, `:499` (300-500 ms); `observer_remote_test.go` the 50 ms `AfterFunc` (a third delta cycle). Collect windows: `observer_local_collisions_test.go:281`, `:436`, `:598`. Virtual time: `virtual_clock_test.go:41`, `:94`.

## Deviations
- "Session fault" in the race-seed rule is the small-file create's session that timed out after its item existed; the model writes no file above 4 MiB, so the large-file route is proven by the layer test only. Race seed 2: of seeds 1-16 under `-race`, only it draws a lost create answer.
- A small file's create counts its own faults as `create:<status>` (was `upload-session:<status>`); the fault stream keeps its name, so seeds draw as before.
- `TestRunWatch_ShutdownStopsRetryAndTrialTimers` runs in parallel again (it was serial only for the ten-second deadline).
- Validates IDs were proposed per test by four read-only agents from the test bodies, then checked against the requirement files and spot-read.
- One-shot passes (`RunOnce`, hundreds of calls) also wait on worker completions; they are not watches or loops and stay plain calls.

## Also fixed
- `startMockWatch`, `startLocalWatch` and the tests that copied their readiness wait: a watch that returned nil before it was ready failed only later on a drained channel; now it fails at once. `mustOpenSyncTree` no longer runs `require` on a watch goroutine.
- Formerly entry 8: the stub consumers of `TestWatch_TopologyApplyFailureDoesNotAdvanceCursor`, `TestWatch_EmptyCompleteTopologyApplyFailureDoesNotAdvanceCursor` and `TestOneShotRunner_RunResultsLoopIdle_ReleasesDueHeldWorkBeforeBlocking` run through `runInBackground` and select on its context; none outlives the test.
- Four remote observer tests no longer wait for their cancelling event on an unbounded helper goroutine; `runPoolWithDrain` takes `t` and bounds its two waits; stale "within 10s" and "Run is synchronous" comments fixed; `CancelUploadSession` uses `dropSession`.

## Seen and not fixed
3, 4, 5: planned separately (absence windows; the model never takes the session route; the fake session sends all bytes in one call).

Unfinished: nothing. Packages to test besides `internal/sync`: none (no other package changed).

## Review (2026-11-12)
All three passes read in full (test files of `internal/sync` and one paragraph of `spec/design/system.md`). No product file changed.
- **Second pass asked**: the first read found no defect; the writer was sent back to fix the entries of its own "Seen and not fixed" list that belong to this change.
- **Third pass asked**: three stub goroutines that no helper owned, and about 14 watches a test called inline with no bound on their return. Result: `runWithin`, `runInBackground` reporting a goroutine that ended without returning, and the Verification Policy sentence widened to "never waits unboundedly for a watch or loop to return".
- **Fixed by me in review**: `TestWatch_ZeroEvents_NoTokenAdvance` started its batch drain after the watch had returned on an already canceled context, so the drain did nothing and the "no cursor in the store" assertion could not fail. The drain and the store are gone; the test asserts that the batch channel is empty after the watch returns, and keeps the token assertion.
- **Decision**: the 26 `// Validates:` lines that open a doc comment with text right after them stay as they are. Main has 46 in that form and the test-conventions check accepts all three placements; one placement rule for the tree is not this change.
- **Left for planned work**: entries 3, 4 and 5 (absence windows proven by a signal; the model's session route; the fake session's byte ranges) and the 50 ms coalescing window in `TestWatch_WakeSignalsCoalesce`, all in the watch-absence and session-chaos change.
- **Checks of mine**: plan-id and retired-backlog-id diffs empty; `gofumpt` clean; the edited test three times; the whole non-race `go test ./internal/sync/...` (417 s); `-race -count=3` over `TestModel_ChaosGraphConverges` and the edited test (654 s); the pre-pr gate on d0ef3c95 (10m37s: lint over the whole tree, the repository checks, build, tagged vet, short unit tests).
