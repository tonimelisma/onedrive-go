# W35 report

Commit: `d65cc842` on `test/multisync-absence-by-signal` (base `a0dbe10b`), not pushed.

## What changed
- `internal/multisync/orchestrator.go`: `drainWaiting func()` test hook field, nil outside tests.
- `internal/multisync/watch_runner_stop.go`: `retireAllWatchRunners` calls the hook after canceling every runner, before its wait loop; no lock is held.
- `internal/multisync/watch_config_file.go`: `configReloadWatch.ticks`; nil keeps the ticker of `interval`, created where it was.
- `internal/multisync/test_helpers_test.go`, `internal/cli/test_helpers_test.go`: `eventWait = time.Minute`, the twin of `internal/sync`'s.
- `internal/multisync/watch_runner_stop_test.go`: owner-lock test on the hook; `testWait` removed; one unbounded receive bounded.
- `internal/multisync/watch_config_file_test.go`: the triggers test drives ticks; `requireNoTrigger` ends two polls, then reads.
- `internal/multisync/{orchestrator,watch_lifecycle,watch_reporting,watch_pause_expiry,reload_log_level,perf_control,panic_boundary,control_socket_hardening}_test.go`: every positive wait bounded by `eventWait` (`controlTestGuard`, `contextPerfWait` removed).
- `internal/cli/{signal,status,auth}_test.go`: positive waits bounded by `eventWait` (`browserLaunchTimeout` removed).
- `spec/design/system.md`: `eventWait` declared in three packages; the two multisync window exceptions replaced by their signals.

## Inventory (DoR has the per-site list)
Before: sleeps 0; absence by a duration 4 sites in 2 tests; literal positive bounds 72 (multisync) + 10 (cli); positive bounds under other names 17 (multisync: `testWait`, `controlTestGuard`, `contextPerfWait`). After: 0, 0, 0, 0. Kept on purpose: `fakeGraphTimeout` (a command's own deadline, read by its retry waits), `statusLiveTestGuard`/budget and `controlTestClientDeadline` (deadlines that are the subject), `shortControlWait` (subject).

## Seam choice for the config watch
Injected ticks, not a poll-done report: the loop keeps its one select and gains no send; a report would add a send to every poll, which in production has no reader and so needs a nil check or a non-blocking send, and the real ticker would still poll between the test's steps. An unbuffered tick channel already gives "the previous poll ended".

## Tests and what they prove
- `TestRunWatch_OwnerLockHeldUntilRunnersDrain`: the lock is held when the drain starts waiting for a canceled drive that returns only when released, and still when the drain closes its engine; RunWatch cannot return first.
- `TestWatchConfigReloadTriggers_FileEditsAndSIGHUP`: two ended polls of an unchanged (or missing) file send no trigger; edits, atomic replace, return of a removed file and SIGHUP do.

## Hand breaks (each restored)
- Drain returns after canceling, before the hook: `RunWatch returned before the drain waited for its drive` (deterministic).
- Drain calls the hook, then returns: 20/20 failed, `the owner lock was free ... as the drain began waiting for the drive` or `RunWatch returned before the drain closed the drive's sync state` (the second is deterministic).
- `closeControlSocket` deferred to run before the drain: 10/10 `the owner lock was free ... as the drain began waiting for the drive`.
- `observe` drops `current == p.last` (unchanged file reloads on its second poll): 20/20 `unexpected reload trigger: config file changed` at the first `requireNoTrigger`.
- Every poll sends a trigger: 20/20 failed the same way.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/multisync/... ./internal/cli/...`: pass; same with `-race` behind the lock, on the final tree: pass (multisync 26.8 s, cli 19.7 s).
- `-race -count=50` of the owner-lock test, the config-watch test and `TestStartWatchRunner_CancelledRunnerDoesNotBlockOnAFullEventChannel`: 150/150 pass.
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: four PASS.
- `ListsEvery|Table|Governs|Exhaustive|Registered` in both packages: pass.
- Lint (`W35-lint.log`): `0 issues.`
- Spec citations: both test names in `system.md` exist; no `P-n`, no `B-NNN`.

## Deviations
- No source-reading check holds the absence rule: `internal/sync` has none (the paragraph cites behavior tests), the e2e wait-rule scan covers `e2e` only, and the `time.Sleep` ban in `internal/devtool` already walks the whole tree. Step 4 had nothing to widen; no check was added.
- The owner-lock test also injects a never-firing retire timer and observes the engine close (existing `retireTimer` and `mockEngine.closeFn`): it makes a drain that signals and then returns fail deterministically.

## Also fixed
- `watch_runner_stop_test.go`: unbounded `<-draining` bounded by `eventWait`.
- `control_socket_hardening_test.go`: unbounded `<-taken` bounded.
- `orchestrator_test.go`: three control helpers called `require.NoError` inside an `Eventually` condition (another goroutine); now `assert` and retry.
- `perf_control_test.go`: `synccontrol` import moved into the local group.
- `auth_test.go`: the browser stub's variable `CODEX_BROWSER_OUT` renamed `ONEDRIVE_GO_TEST_BROWSER_OUT`.
- `signal_test.go`: messages no longer name the old 2-second bound.

## Not fixed
1. `internal/sync/test_helpers_test.go:45`: `eventWait`'s comment says an absence wait "stays short, at its use", against the rule in `system.md`. Fix: say absence is shown after a handled signal.
2. `internal/multisync` tests start `RunWatch` on goroutines nobody waits for (`go func() { errCh <- orch.RunWatch(...) }()`, about 25 tests in `orchestrator_test.go`, `watch_lifecycle_test.go:57`, `watch_reporting_test.go:303`, `watch_pause_expiry_test.go:32`, `reload_log_level_test.go:45`, `perf_control_test.go:134`, `watch_runner_stop_test.go:588, :799 (that one releases its drive on cleanup, so the watch ends)`, `panic_boundary_test.go:38`); a test that fails before its last receive leaves the watch running. Fix: a `runInBackground`/`receiveWithin` twin of `internal/sync`'s.
3. `internal/multisync/watch_runner_stop_test.go:54`: `handleEventWithin`'s goroutine is left blocked when the wait fails. Fix: same helper as 2.
4. No check holds "no absence by a window" in any package. Fix: a source scan of `_test.go` for testify `Never`/`Consistently` and selects whose timer arm passes the test.
5. `internal/multisync/orchestrator_test.go` and siblings repeat `select { case <-x: case <-time.After(eventWait): FailNow }` about 70 times. Fix: a `receiveWithin` helper (with 2).

## Unfinished
None.

## For the reviewer
No other package's tests were needed: the product changes are unexported and used only in `internal/multisync`.

## Review fixes
HEAD is now `f13c0d61` (amended, one commit on `f82a5f84`). Changes against `7ccbbbb6`, per item:
1. `internal/cli/sync_notify_test.go:52`: the notify read deadline is `eventWait` (was 5 s); :55 names the wait ("no notify message arrived"); :160-165 the daemon's return is read in a select on `eventWait` ("the sync daemon did not return after the watch ended"). Commit message: "82" became "83 literal bounds", "every positive wait" became "every timed wait ... for an event that must come", and the old "two unbounded receives" line became a sentence on every unbounded receive.
2. `internal/multisync/watch_config_file_test.go:107` `TestWatchConfigReloadTriggers_PollsOnItsIntervalWithoutInjectedTicks`: no ticks, 1 ms interval, waits (eventWait) for the watcher's first stamp read through an injected `readStamp` wrapper (otherwise an edit before that read is the starting version and never reloads), edits, requires "config file changed" within `eventWait`. Seen failing with `ticks = ticker.C` removed (watch_config_file.go:93): "no reload trigger for config file changed" after 60 s; restored.
3. Bounded with `eventWait` and a message naming the event: `orchestrator_test.go:1155`, `:1235` (the two `<-wr.done`); `watch_reporting_test.go:185` (`exit`'s `<-wr.done`), `:417-427` (`<-draining`, `<-wr.done`); `internal/cli/status_live_test.go:182-192` (cleanup after cancel, `assert.Fail` since it runs in cleanup). Further sites found by the search (statement, argument and assignment receives in both packages' test files): `watch_reporting_test.go:44-55` (`stopTestRunners`, `assert.Fail`, it runs in cleanup), `orchestrator_test.go:2304-2311`, `:2401-2409`, `:2506-2514` (runner loops after cancel), `engine_close_test.go:62-67` (the close context, sent by the close goroutine that a fired timer may not have run yet). Left as not waits: `<-acceptDone` (test-owned accept loops after the listener closed), `perf_control_test.go:199` (length checked first), `orchestrator_test.go:3155` (`<-listeners`, filled before the engine started), every receive inside a fake engine or handler.
4. `spec/design/system.md:299-311`: the no-unbounded-wait sentence and the helper sentences now begin "In `internal/sync`"; new sentence: in `internal/multisync` and `internal/cli` every wait for a watch or loop a test runs on a goroutine is bounded by `eventWait` and names what it waited for. "In internal/sync" also covers "or is the test's own call", because direct own calls of `RunWatch` remain in multisync (see Not fixed 1).
5. `internal/multisync/orchestrator.go:131-132`: "It must not block: the runners' stop bound is already running when it is called."
6. `internal/cli/json_contract_fake_graph_test.go:103-105`: the retry waits read this deadline (checked: `retry/transport.go:194` `waitFits`), so it is part of what the cases test, stays short, and is not an event wait.
7. `watch_runner_stop_test.go:716-753, 775-830`: `newDrainingDrives` (one drive per mount ID, one shared unbuffered `closing`), owner-lock test with two drives, each with its own release; the lock is checked at each drive's close. 44 lines added, 27 removed (a third are comments), at the "about 40" limit. Seen failing with the control socket closed after the first retired runner (temporary hook in `retireAllWatchRunners`): "the owner lock was free ... as the drain closed the sync state of drive 2 of 2"; the one-drive version passed under the same break. Restored.
8. `internal/cli/replace_destination_test.go:281-295`: the call runs on a goroutine and is received within `eventWait` ("replaceDestination did not return after the restore of the original hung"); the 20 s elapsed assertion stays. No helper existed; none added. Seen failing with the restore bound set to 2 minutes (replace_destination.go:60): the select failed at 60 s; restored.
9. `internal/sync/test_helpers_test.go:45-46`: replaced by the multisync sentence.

Checks: `go vet ./...` and with `-tags=e2e,e2e_full,integration,stress`: clean. Notify test `-race -count=20`: ok. Both config tests `-race -count=50`: ok. Owner-lock test `-race -count=3`: ok. Both packages `-count=1` and `-race -count=1` (behind the lock): ok. `-run TestRealRepo ./internal/devtool/` matched no test; ran `-run RealRepo` (the `_RealRepoIsClean` tests): ok. gofumpt -l on the 11 changed Go files: nothing. No lint run.

Not fixed:
1. Direct, unbounded own calls of a watch that wait for a product event: `multisync/panic_boundary_test.go:210`, `cli/sync_notify_test.go:251` (both in a child process), and `multisync/orchestrator_test.go:2958`, `:3048`, `:3678`, `watch_lifecycle_test.go:123` (not checked whether each returns at once). Fix: a `runWithin` twin, with the helper row.
2. `multisync/orchestrator_test.go` eventWait arms use `require.Fail`, not `FailNow` (e.g. :1870, :2602, :2827, :3151), so a failed wait runs on into the next one; :3151 then blocks on `<-listeners`. Fix: `FailNow`, with the helper row.
3. `multisync/owner_lock_test.go:52`: `wg.Wait()` for two control-server starts is unbounded. Fix: a done channel read within `eventWait`.


## Merged as #931

- Merge commit `41919e442b0820598017037b571e108358b0b373`, 2026-11-15 23:12 PST. The pre-pr gate passed on the final commit, the race run of `internal/multisync` and `internal/cli` passed on it, and the pre-merge and post-merge stages passed. Installed at 23:32 PST.
- Nightly failures: the branch held no claim at its merge.
