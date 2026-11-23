# W35 Definition of Ready

Worktree `~/src/onedrive-go-wt/multisync-absence-by-signal`, branch
`test/multisync-absence-by-signal`, HEAD `a0dbe10b` (clean apart from the
untracked `.testdata` link).

1. PASS **Task intent**: repo-maintenance outcome. No test in
   `internal/multisync` or `internal/cli` proves absence by waiting a while,
   and every positive wait for an event in them is bounded by one named wait,
   `eventWait` (one minute), the twin of `internal/sync`'s.
2. PASS **Non-goals**: no change to what the drain or the reload does;
   `internal/sync` and its tests, `e2e`, `internal/graph`, `testutil`, CI
   files, the verification-policy and verify lines of `system.md` other than
   the two sentences this change needs, and requirement lines owned by other
   branches (R-6.10.23/24/27).
3. PASS **Governing docs**: `spec/design/system.md` Verification Policy (lines
   292-335: `eventWait`, the absence rule and its two multisync exceptions);
   `spec/design/sync-control-plane.md` (owner lock held until drives drain,
   lines 581-588; reload triggers, lines 714-723); R-6.10.10, R-6.3.9,
   R-2.9.4, R-4.4.3; repository `CLAUDE.md` test style and strategy.
4. PASS **Authority owner**: the RunWatch goroutine owns the drain
   (`retireAllWatchRunners`, `watch_runner_stop.go`) and the runner map; the
   control socket server owns the owner lock, closed by RunWatch's deferred
   `closeControlSocket` after the drain; `watchConfigReloadTriggers`
   (`watch_config_file.go`) owns its poll goroutine and ticker. The seams add
   no owner: a nil-by-default hook field on `Orchestrator`, called on the
   RunWatch goroutine outside any lock, and an optional ticks channel in
   `configReloadWatch`, read only by the poll goroutine.
5. PASS **Code scope**: product `internal/multisync/orchestrator.go` (field),
   `watch_runner_stop.go` (hook call), `watch_config_file.go` (ticks);
   tests in `internal/multisync` (`test_helpers_test.go`,
   `watch_runner_stop_test.go`, `watch_config_file_test.go`,
   `orchestrator_test.go`, `watch_lifecycle_test.go`,
   `watch_reporting_test.go`, `watch_pause_expiry_test.go`,
   `reload_log_level_test.go`, `perf_control_test.go`,
   `panic_boundary_test.go`, `control_socket_hardening_test.go`) and
   `internal/cli` (`test_helpers_test.go`, `signal_test.go`,
   `status_test.go`, `auth_test.go`); `spec/design/system.md`. Not touched:
   `internal/sync`, `e2e`, `internal/graph`, `internal/devtool`.
6. PASS **Regression plan**: `TestRunWatch_OwnerLockHeldUntilRunnersDrain`
   rewritten on the drain hook (and the engine close the drain does after
   the drive returned); `TestWatchConfigReloadTriggers_FileEditsAndSIGHUP`
   rewritten on injected ticks. Each is shown failing on a hand break (the
   drain returns before the drive stops; a reload fires on an unchanged
   file), then restored. Every other site keeps its test and changes only its
   bound.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`,
   `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`;
   `go test -count=1 ./internal/multisync/... ./internal/cli/...`, then with
   `-race` behind the heavy lock; each rewritten test `-race -count=50`;
   `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/` (four
   PASS); package tests named `ListsEvery|Table|Governs|Exhaustive|Registered`
   in the two packages; targeted golangci-lint last, behind the lock. No
   `devtool verify`, no e2e.
8. PASS **Data safety**: test code and two test seams that are nil or absent
   in production; no store, filesystem or remote mutation path changes; the
   drain's and the poll's steps keep their order.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata`,
   config directories or account names are read or written; tests keep their
   existing synthetic fixture names.
10. PASS **Live provider and destructive operations**: none; no live test, no
    command against OneDrive, no dependency upgrade, no history rewrite; one
    local commit, not pushed.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: which source-reading check holds the absence rule?
      Current assumption: none does. The paragraph's cited tests are behavior
      tests; the only source scan over test files that covers both packages is
      the `time.Sleep`/`t.Fatal` check in `internal/devtool`
      (`verify_test_conventions.go`), which walks the whole tree already; the
      e2e wait-rule scan covers `e2e` only. Validation step: none to widen;
      reported as a deviation. Owner if wrong: `internal/devtool` or the check
      that would hold the rule.
    - Question: is `fakeGraphTimeout` (30 s, three cli uses) a positive wait?
      Current assumption: no. It is the command's own context deadline, which
      the command's retry waits read (a wait that cannot fit returns at
      once), so changing it changes what the command does; it is named, not a
      bare number, and stays. Validation step: its comment and uses read.
      Owner if wrong: `internal/cli/json_contract_fake_graph_test.go`.
    - Question: does a blocking unbuffered tick send prove a poll ended?
      Current assumption: yes; the poll goroutine receives a tick only in its
      select, so a send is taken only after the previous poll (and any
      trigger it sent) finished. Validation step: the hand break fails the
      test deterministically. Owner if wrong: `watch_config_file.go`.

## Inventory (before any edit)

Kinds: (i) sleeps, (ii) absence proven by a duration, (iii) a positive wait
for an event bounded by a literal duration, (iv) a positive wait bounded by a
package-local name other than the one long wait. Sleeps in either package: 0.

internal/multisync:
- watch_runner_stop_test.go TestRunWatch_OwnerLockHeldUntilRunnersDrain:739 (ii) `assert.Never` 200 ms on the owner lock -> drain hook, lock checked once there and once as the drain closes the engine
- watch_config_file_test.go TestWatchConfigReloadTriggers_FileEditsAndSIGHUP:50,54,64 (ii) `requireNoTrigger` 100 ms window (3 calls) -> injected ticks, two finished polls, then an empty triggers channel
- watch_config_file_test.go requireTrigger:20 (iii) 5 s -> eventWait, driving ticks
- watch_runner_stop_test.go testWait (5 s) at handleEventWithin:65, TestApplyWatchMountSetForParent...:236, startStuckWatch:595, stuckWatch.result:608, TestStartWatchRunner_CancelledRunnerDoesNotBlockOnAFullEventChannel:685, TestRunWatch_OwnerLockHeldUntilRunnersDrain:728,753 (iv) -> eventWait; testWait removed
- panic_boundary_test.go testWait at runWatchCatchingPanics:50, TestWatchConfigReloadTriggers_PanicIsReportedOnTheFatalChannel:297, TestControlServer_HandlerPanicIsLoggedThroughTheOwnersLogger:464, TestRunIndexedMountWork_APanicWhileStartingShortcutsIsThatDrivesReport:518 (iv) -> eventWait
- control_socket_hardening_test.go controlTestGuard (5 s) at sendControlRequest:188, TestControlSocketServer_SetsReadIdleAndHeaderLimits:224, TestControlSocket_SlowBodyIsCutOff:262, TestStartControlSocketServer_UnexpectedServeEndIsFatal:368 (iv) -> eventWait; controlTestGuard removed (shortControlWait is the subject and stays)
- perf_control_test.go contextPerfWait (5 s) at TestOrchestrator_MountPerfIncludesContextRecordedMetrics:144,157 (iv) -> eventWait
- orchestrator_test.go 55 sites (iii), all 5 s: control helpers getControlStatus:266, postControlJSON:292, postControlJSONAnyStatus:323 (Eventually); 14 more Eventually in RunWatch_MultiMount, IdlesWhenAllMountsPaused..., Reload_AddDrive (2), Reload_ContentFilterChange... (2), Reload_RemoveMount (2), Reload_PausedMount (2), Reload_InvalidConfig, Reload_EmitsMountLocalCompilationFailures, Reload_TimedPauseExpiry (2); 38 `time.After` selects in StartWatchRunner_*, RunOnce_*, RunWatch_*, Orchestrator_RunWatch_*, ControlSocket_*, Reload_* -> eventWait
- watch_lifecycle_test.go nextLifecycleEvent:23, TestRunWatch_ReportsLifecycleTransitions:94, TestRunWatch_StartupFailureIsNotReady:121 (`WithTimeout` 10 s) (iii) -> eventWait
- watch_reporting_test.go TestRunWatch_RestartsFailedRunnerWhenBackoffTimerFires:311,322,330, runFailingFirstWatchBesideASibling:576,585,602,610 (iii) -> eventWait
- watch_pause_expiry_test.go TestRunWatch_TimedPauseExpiresWithoutAReload:38,51,67 (iii) -> eventWait
- reload_log_level_test.go TestOrchestrator_Reload_AppliesTheConfigLogLevel:48,61,67 (iii) -> eventWait

internal/cli:
- signal_test.go TestShutdownContext_FirstSignalCancels:45 (2 s), TestShutdownContext_SecondSignalForcesExit:59 (`WithTimeout` 10 s), runFirstSignalSyncWatchSubprocess:141 (10 s), TestShutdownContext_ParentCancelStopsGoroutine:186 (2 s), TestShutdownContext_CancelReleasesSignalGoroutine:343 (5 s), TestShutdownContext_StopAfterFirstSignalReleasesSignalGoroutine:365 (2 s), 373 (5 s), TestShutdownContext_HandlerPanicCancelsTheCommandWithTheError:399 (5 s) (iii) -> eventWait
- status_test.go TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper:1261 (1 s) (iii) -> eventWait
- auth_test.go TestOpenBrowser_StartsValidatedCommand:409/425 (local `browserLaunchTimeout` 15 s) (iii) -> eventWait
- kept, not event waits: `fakeGraphTimeout` (put_test.go:91, folder_transfer_test.go:32, json_contract_test.go:86: the command's own deadline, read by its retry waits); `statusLiveTestGuard` and the budget deadline in status_live_test.go:98,192 (subject: the status budget); `controlTestClientDeadline` in control_socket_semantics_test.go:240,288 (subject: an owner that never answers)

Counts before: (i) 0; (ii) 4 sites in 2 tests; (iii) 72 in multisync + 10 in cli; (iv) 17 in multisync under three other names. Target after: (i) 0, (ii) 0, (iii) 0, (iv) 0; one `eventWait` per package.

Also seen while reading (to fix where the file is touched anyway): unbounded receives in watch_runner_stop_test.go:680 (`<-draining`), :733 (`<-canceled`) and control_socket_hardening_test.go:316 (`<-taken`).
