# W77 Definition of Ready

Worktree `~/src/onedrive-go-wt/multisync-watch-helpers`, branch
`test/multisync-watch-helpers`, HEAD `ca8ecac6` (main), status clean but for
the untracked `.testdata`. Line numbers below are of that commit.

1. PASS **Task intent**: repo-maintenance outcome. In the tests of
   `internal/multisync` and `internal/cli`, as already in `internal/sync`, a
   watch or loop a test starts runs only through `runInBackground` (whose
   cleanup cancels it and waits within `eventWait`), and every wait for an
   event that must come is one call (`receiveWithin`, `stopWatch`,
   `runWithin`) that fails the test at once naming what it waited for.
   `spec/design/system.md` states that rule once for all three packages.
2. PASS **Non-goals**: no product code (`internal/synctest/child.go`
   included); no edit to `internal/sync` test files beyond one new helper
   test file; no source check that holds the wait rules; no change to any
   test's assertions or order of events; `require.Eventually` polls, mock
   engines' and handlers' own waits (`<-ctx.Done()`, `<-release`) and
   non-blocking `default:` checks are not waits of the test and stay.
3. PASS **Governing docs**: `spec/design/system.md` lines 292-312 (the
   `eventWait` and helper bullets, read by line range), routing table
   `CLAUDE.md` lines 5-53 (`internal/multisync/`, `internal/cli/sync.go` ->
   `sync-control-plane.md`), `CLAUDE.md` lines 55-236 and 252-278,
   `spec/requirements/non-functional.md` R-6.10.10 (deterministic watch
   lifecycle tests; cited by the new helper tests).
4. PASS **Authority owner**: test code only. Each package's test helper file
   owns the lifecycle of every goroutine a test starts (runInBackground's
   cleanup); `internal/synctest.RunInChild` keeps owning child processes.
   No durable state, product lifecycle or user-facing output changes.
5. PASS **Code scope**: new `internal/multisync/watch_test_helpers_test.go`
   and `internal/cli/watch_test_helpers_test.go` (the twins), a helper test
   file in each of the three packages, edits to the multisync and cli test
   files listed in the inventory below, one sentence of
   `spec/design/system.md`. Must not touch: product files, other
   `internal/sync` test files, other worktrees.
6. PASS **Regression plan**: `TestWatchHelpers_FailNamingWhatTheyWaitedFor`
   in each package: each wait helper, run in a child test binary inside a
   `testing/synctest` bubble (eventWait passes in virtual time), must fail
   its test with its own message. Seen failing with the message or the
   bound broken, then restored. Every moved test keeps passing.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, `go vet
   -tags=e2e,e2e_full,integration,stress ./...`; per moved file its
   package's tests; `go test -count=1` then `-race -timeout 60m` of
   `./internal/multisync/ ./internal/cli/` behind the heavy lock; the
   internal/sync helper test alone (`-run`, `-race -timeout 30m`); each
   helper test and three moved watch tests 30x under `-race`, one command
   per package; `golangci-lint` once, last, behind the lock;
   `RealRepoIsClean` (4 PASS). No e2e, no `devtool verify`.
8. PASS **Data safety**: tests only, on temp dirs and fakes; no real sync
   folder, store or account is touched.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata` or
   account names are read or written; test identities are the invented
   `@example.com` ones already in the files.
10. PASS **Live provider and destructive operations**: none; no history
    rewrite, no dependency change, nothing pushed.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD
    start stage has already been run in this worktree; no carryover threads
    or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does a `testing/synctest` bubble run receiveWithin's
      FailNow and runInBackground's cleanup timer in virtual time, with the
      cleanups still running? Current assumption: yes. Validation step:
      done before this report, a stdlib prototype under `S/w77/proto`
      failed each shape at once with its message and ended cleanly.
      Owner if wrong: the new helper tests.
    - Question: does a failing subtest of a `RunInChild` child exit 1 with
      its message in the output? Current assumption: yes (go test exits 1
      on a failed test). Validation step: the helper tests themselves.
      Owner if wrong: the new helper tests' child check.

## Inventory (evidence for items 5 and 6)

The `internal/sync` helpers (`watch_test_helpers_test.go` 176-270) have no
test of their own (no test names them; grep of `internal/sync`), so the
helper test is added to all three packages. Name check: none of
`runInBackground`, `runWithin`, `runWatchInBackground`, `receiveWithin`,
`stopWatch`, `errRunExitedWithoutReturning` exists in multisync or cli.
Twins: multisync gets all six; `runWatchInBackground(t, orch)` drops the mode
and options, which every multisync call passes as `SyncBidirectional` and
`WatchOptions{}` (unparam). cli gets `runInBackground`, `runWithin`,
`receiveWithin` and the sentinel only: no cli test cancels a watch and
waits for nil, so `stopWatch` and `runWatchInBackground` would be unused.

### Goroutines tests start, and the helper each moves to

multisync (36):
- `orchestrator_test.go` RunWatch -> `runWatchInBackground`: 1863, 1899,
  2814, 2861, 2912, 2998, 3070, 3139, 3232, 3302, 3369, 3425, 3480, 3558,
  3625. RunOnce -> `runInBackground` (result captured in the closure):
  1498, 1573, 1711, 2590. Accept loops of a held control socket -> one
  helper `occupyControlSocket` over `runInBackground`: 599, 2628, 2672.
- `watch_reporting_test.go`: 311, 586 -> `runWatchInBackground`; 451
  accept loop -> `occupyControlSocket`.
- `watch_runner_stop_test.go`: 54 (`handleEventWithin`) -> `runWithin`; 587
  (`startStuckWatch`), 811 -> `runWatchInBackground` (811's drive release
  cleanup registered after it, so it runs first).
- `watch_lifecycle_test.go` 57, `watch_pause_expiry_test.go` 32,
  `reload_log_level_test.go` 45 -> `runWatchInBackground`;
  `perf_control_test.go` 133 -> `runInBackground` (collector context).
- `panic_boundary_test.go`: 38 (`runWatchCatchingPanics`, recover kept in
  the closure), 509 -> `runInBackground`.
- `owner_lock_test.go` 45: two contenders per round -> `runInBackground`;
  the unbounded `wg.Wait()` (52) becomes `receiveWithin` of each done.
- `control_socket_hardening_test.go` 168 (`takeOneCommand`) ->
  `runInBackground`.

cli (6): `sync_notify_test.go` 133 -> `runInBackground` (release closed by a
cleanup registered after it); `status_live_test.go` 178 -> `runInBackground`
(the guard select stays, below); `control_socket_semantics_test.go` 118
(`server.Serve`) -> `runInBackground`, Shutdown cleanup registered after
it; `replace_destination_test.go` 283 -> `runWithin`. Stays:
`status_live_test.go` 100, the `statusLiveBudgetContext` seam's goroutine:
product code calls the seam, off the test goroutine, and its returned cancel
ends it (decision b).

### Hand-written selects with a timer arm: 86

multisync 78: `orchestrator_test.go` 43 (1150, 1157, 1224, 1232, 1237, 1504,
1509, 1517, 1589, 1596, 1728, 1733, 1739, 1869, 1878, 1905, 1910, 1920,
2308, 2405, 2505, 2596, 2612, 2821, 2830, 2875, 2918, 2928, 2937, 3009,
3025, 3076, 3100, 3145, 3156, 3255, 3335, 3392, 3448, 3510, 3574, 3587,
3663); `watch_reporting_test.go` 10 (50, 187, 330, 338, 419, 425, 592, 601,
618, 626); `watch_runner_stop_test.go` 8 (62, 233, 592, 605, 679, 686, 765,
832); `watch_config_file_test.go` 4 (30, 48, 128, 136);
`panic_boundary_test.go` 3 (50, 297, 518); 2 each in
`control_socket_hardening_test.go` (315, 368), `watch_lifecycle_test.go`
(23, 94), `perf_control_test.go` (140, 153), `watch_pause_expiry_test.go`
(51, 67); 1 each in `reload_log_level_test.go` (67), `engine_close_test.go`
(65).

cli 8: `status_live_test.go` 189, 197; `signal_test.go` 45, 186, 365, 399;
`sync_notify_test.go` 163; `replace_destination_test.go` 290.

All fold into `receiveWithin`/`stopWatch`/`runWithin` except these, which
stay: `watch_config_file_test.go` 30 (the arm bounds a send of a tick, not a
receive) and 41-49 (one deadline over a loop that sends ticks until a
trigger comes); `watch_runner_stop_test.go` 765 (`receiveBeforeReturn`: two
event arms, RunWatch returning early is its own failure; already one call
with FailNow); `status_live_test.go` 197 (its bound `statusLiveTestGuard` is
the subject of those tests, shorter than `statusLiveTestSignInTimeout`; a
deadline that is the subject keeps its name). 189 goes with the move to
`runInBackground`.

The `require.Fail`/`assert.Fail` fault (one narrow grep: the line after
each `time.After(eventWait)` arm): 38 arms. multisync 34:
`orchestrator_test.go` 1870, 1879, 1906, 1911, 1921, 2597, 2613, 2822, 2831,
2876, 2919, 2929, 2938, 3010, 3026, 3077, 3101, 3146 (then the unbounded
`<-listeners` at 3150), 3157, 3256, 3336, 3393, 3449, 3511, 3575, 3588,
3664; `perf_control_test.go` 141, 154; `control_socket_hardening_test.go`
369; `watch_lifecycle_test.go` 24 (`nextLifecycleEvent`), 95;
`watch_reporting_test.go` 51 (`stopTestRunners`, also called from a test
body at 120 and from `watch_runner_stop_test.go` 236); `panic_boundary_test.go`
298. cli 4: `status_live_test.go` 190 (cleanup), `signal_test.go` 46, 187,
366. All become `receiveWithin` (FailNow). Other unbounded receives on a
test goroutine: `orchestrator_test.go` 627, 2658, 2700 and
`watch_reporting_test.go` 465 (`<-acceptDone`, into `occupyControlSocket`),
3150 (`<-listeners`, into `receiveWithin`); `perf_control_test.go` 199
stays (a value already queued: `require.Len(…, 1)` just before).

### Direct calls of a watch on the test's own goroutine

Through `runWithin`, because each returns only by the product's own decision
(a refusal or an end), which is what the test proves and a regression would
turn into a hang:
- `orchestrator_test.go` 2602 (control socket held by the running RunOnce:
  refused at its first step, the bind), 2694 (socket held by a test
  listener: same), 2953 and 3043 (every engine fails to start: refused
  after the bind), 3673 (no drives: refused at the first check);
  `watch_lifecycle_test.go` 123 (only drive fails; the fixture once idled
  instead; its deadline context and `ctx.Err` check become the runWithin
  bound and message).
- Child processes, decision (d): `panic_boundary_test.go` 210 and
  `sync_notify_test.go` 251 do not return at once (the control server's
  panic, the keepalive's panic end them). The parent's wait for the child
  is `exec.CommandContext` with `childTestTimeout` (2 min,
  `internal/synctest/child.go` 23), not `eventWait`, and fails naming only
  "the child test binary failed". Bounding the parent's wait by eventWait
  needs a change to that non-test file; instead the child's own call goes
  through `runWithin`, so the child fails within eventWait naming what it
  waited for and the parent prints that output.

Stay plain, a fake returns at once (the spec's exception): cli
`sync_notify_test.go` 183, 205 (`lifecycleOrchestrator` with no release
returns its error), 269 (no drives, before any orchestrator);
`sync_runtime_test.go` 75 (the `syncWatch` seam returns nil; it also
asserts it got `t.Context()`), 362, 390, 431, 473, 509, 551
(`testSyncDaemonOrchestrator` returns at once; factories call require on
the test goroutine). Not watches: `RunOnce` passes and the loop's single
steps (`handleWatchRunnerEvent`, `reconcileWatchRunners`,
`applyWatchMountSet*`, `retire*`) on hand-made runners whose done is closed
or whose retire bound is an already fired timer; cli `runSync` one-shot
calls in `sync_test.go` 383-446 (config errors before any sync). Child
helpers in `signal_test.go` (191-316) run in a child bounded by the
parent's `exec.CommandContext` with `eventWait` (59, 141).
