# W77 report: multisync and cli tests start and stop a watch only through bounded helpers

Worktree `~/src/onedrive-go-wt/multisync-watch-helpers`, branch
`test/multisync-watch-helpers`, one commit `456dacd4` on `ca8ecac6`, not
pushed. Test code and one spec sentence; no product file.

## What changed (one line per file)

- `internal/multisync/watch_test_helpers_test.go` (new): twins of `runInBackground`, `runWithin`, `receiveWithin`, `stopWatch`, `errRunExitedWithoutReturning` (same names, bodies, messages), plus `runWatchOf`/`runWatchInBackground(t, orch)` and `runOnceInBackground(t, orch)` (every call is bidirectional with default options, so no mode/options parameters: unparam).
- `internal/cli/watch_test_helpers_test.go` (new): twins of `runInBackground`, `runWithin`, `receiveWithin`, the sentinel. No cli test cancels a run itself, so `runInBackground` returns only the result channel (unparam), and `stopWatch`/`runWatchInBackground` would be unused.
- `internal/{multisync,cli,sync}/watch_test_helpers_failures_test.go` (new): the helper test.
- `internal/multisync/orchestrator_test.go`: 15 RunWatch and 4 RunOnce goroutines onto the helpers, 43 timer selects folded, 5 own RunWatch calls through `runWithin`, the unbounded `<-listeners`, 4 accept loops into one `occupyControlSocket`, 13 message-less polls given messages.
- `internal/multisync/watch_reporting_test.go`, `watch_runner_stop_test.go`, `watch_lifecycle_test.go`, `watch_pause_expiry_test.go`, `reload_log_level_test.go`, `perf_control_test.go`, `panic_boundary_test.go`, `owner_lock_test.go`, `control_socket_hardening_test.go`, `engine_close_test.go`, `watch_config_file_test.go`: their goroutines, selects and own calls as listed in W77-dor.md.
- `internal/cli/sync_notify_test.go`, `status_live_test.go`, `control_socket_semantics_test.go`, `replace_destination_test.go`, `signal_test.go`: the same.
- `spec/design/system.md`: the helper sentence covers all three packages (the separate, weaker multisync/cli sentence is gone), cites the new test, and names "a fake that returns at once" among the plain-call exceptions.

## Tests added, renamed, removed

`internal/multisync`: added `TestWatchHelpers_FailNamingWhatTheyWaitedFor` (subtests `receiveWithin`, `runWithin`, `stopWatch`, `runInBackground`). None renamed or removed.
`internal/sync`: added the same test, same four subtests. `internal/cli`: added the same test, subtests `receiveWithin`, `runWithin`, `runInBackground`.

Each subtest runs its helper on an event that never comes in a child test binary (`synctest.RunInChild`) inside a `testing/synctest` bubble and requires the child to fail with the helper's message, exactly `eventWait` (virtual) after the wait began; the parent spends milliseconds. Seen failing (each then restored): receiveWithin's message changed (multisync, cli: receiveWithin/runWithin/stopWatch subtests fail), its bound doubled (multisync, sync: the same three fail on the timing check), runInBackground's cleanup bound doubled (multisync, cli) and its message changed (multisync, sync: runInBackground subtest fails), stopWatch's message changed (multisync: stopWatch fails).

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/multisync/ ./internal/cli/` behind the lock: ok; with `-race -timeout 60m`: ok. Both run again on the final tree after the lint fixes: ok (3.7 s, 3.8 s; race 26.5 s, 18.1 s). Each file's package run after its edits: ok.
- `-race -count=30`, one command per package: multisync (helper test, `TestOrchestrator_RunWatch_SingleMount`, `TestRunWatch_OwnerLockHeldUntilRunnersDrain`, `TestRunWatch_RestartsFailedRunnerWhenBackoffTimerFires`, `TestStartWatchRunner_ThreadsWebsocketConfig`): ok. cli (helper test, `TestRunSyncDaemon_NotifiesSystemdOfLifecycle`, `TestRunSyncDaemon_WatchdogPanicEndsTheWatchWithItsError`, `TestProbeControlOwner_ClassifiesOutcomes`): ok, again after the lint fix. internal/sync (helper test, `-timeout 30m`): ok.
- `RealRepoIsClean`: 4 PASS. Lint: the first run found 2 issues (nilerr in `occupyControlSocket`, unparam on cli's unused cancel), both fixed; the second run: `0 issues.` (W77-lint.log).

## Deviations

- Decision (d): the parent's wait for a child is `childTestTimeout` (2 min, `internal/synctest/child.go`), not `eventWait`, and names only "the child test binary failed". Bounding it by eventWait needs that non-test file, so instead the child's own call (`panic_boundary_test.go` TestRunWatch_ControlServerPanicEndsTheWatch, `sync_notify_test.go` TestRunSyncDaemon_WatchdogPanicEndsTheWatchWithItsError) goes through `runWithin`: the child fails within eventWait naming what it waited for, and the parent prints that output.
- Own RunWatch calls that do return at once (control socket held, no drive, every engine failing) go through `runWithin` too: the refusal is what each test proves, and a regression would turn it into a hang.
- `TestRunWatch_StartupFailureIsNotReady`: its `eventWait` deadline context and `require.NoError(ctx.Err(), …)` became the runWithin bound and its message ("a watch whose only drive failed to start to return, not idle").

## Also fixed

- `orchestrator_test.go` `occupyControlSocket`: the accept loops ended silently on any accept error and on a failed `conn.Close`; now only the closed listener ends the loop cleanly, and any other error is the run's error, checked on release.
- `orchestrator_test.go` TestRunOnce_StartsParentChildrenAfterPublishingParentSafePoint: a send-or-close release became `close`.
- `orchestrator_test.go` TestStartWatchRunner_ThreadsWebsocketConfig: `defer cancel()` instead of a cancel only on the passing path.
- 14 `require.Eventually(…, eventWait, …)` polls without a message (`orchestrator_test.go`, `reload_log_level_test.go`) now name what they wait for.
- `watch_reporting_test.go` `stopTestRunners`: its `assert.Fail` arm ran on (also from a test body); now `receiveWithin`.

## Not fixed

- `internal/synctest/child.go:23,65`: a child is bounded by `childTestTimeout` and its kill reports only the child's output so far. Fix: take the bound from the caller (eventWait) and say the child was killed at it.
- `internal/multisync/orchestrator_test.go:750, 855, 992-996, 1401-1420, 1715, 1780-1804, 1891-1923, 2027, 2191, 2278, 2369, 3416-3422`: mock engines and engine factories call `require.*` (FailNow) on the watch's or pass's goroutines, where it ends only that goroutine (on runInBackground's own goroutine the test now at least reads `errRunExitedWithoutReturning`). Fix: `assert` and return an error.
- `internal/multisync/control_socket_hardening_test.go:157,209,245,354`, `perf_control_test.go:201,272`, `panic_boundary_test.go:433`, `orchestrator_test.go:2937`, `internal/cli/control_socket_semantics_test.go:128`: cleanups close a control server with `context.Background()`; `Shutdown` then waits for idle connections without a bound. Fix: a context bounded by eventWait.
- `internal/cli/sync_runtime_test.go:29`: `_ = ctx` silences an unused parameter. Fix: name it `_`.

## Unfinished

None. Packages the reviewer should test beyond these three: none (test files only).

## Review fixes

The one commit, amended on the same branch, not pushed. Line numbers are
of the amended tree; `orchestrator_test.go` is `internal/multisync/`.

Per item:

- Finding 1: `orchestrator_test.go:3399` TestOrchestrator_RunWatch_ZeroMounts calls RunWatch through `runWithin` ("RunWatch to refuse a start with no drives").
- Finding 2: commit message: "two of whose timeout arms called assert.Fail and so ran on"; "the test's own calls of a watch, two of them in child processes"; "every own call of RunWatch goes through runWithin" kept, now true; the select count dropped (not re-counted); one new paragraph on closeWithin and the fakes off the test's goroutine.
- Finding 3: `orchestrator_test.go:1865-1875`: after `runWatchInBackground`, `releaseParent := sync.OnceFunc(close parentRelease)`, `t.Cleanup(releaseParent)`, and `releaseParent()` on the passing path, with a comment.
- Finding 4: `closeWithin(t, what, close)` in `multisync/watch_test_helpers_test.go:132-143` and `cli/watch_test_helpers_test.go:89-100`: a fresh `context.WithTimeout(context.Background(), eventWait)`, and `assert.NoError` naming the server, so a failed or timed-out close fails the test. Nine sites, one line each: `control_socket_hardening_test.go:156,206,240,347`, `perf_control_test.go:200,269`, `panic_boundary_test.go:433`, `orchestrator_test.go:2994`, `cli/control_socket_semantics_test.go:128`. The header comments of both helper files say closeWithin is the package's own (not a twin of internal/sync).
- Note 5, all in `orchestrator_test.go`:
  - (a) 15 `require` calls inside a run's fake (the finding's "about 18" are 15) now assert and return the fake's error: 733, 758, 865, 1005, 1008, 1013, 1424, 1427, 1431, 1814, 1818, 1939, 1942, 1950, 3482. testifylint's require-error flags three of them (an error assertion followed by further calls in its block); each carries `//nolint:testifylint` saying the fake runs on the pass's or the watch's goroutine, where require cannot end the test (732, 1430, 1817).
  - (b) 6 factory and default-arm sites: 1446, 1743, 1829, 1841, 1916, 1977 (`assert.Fail`, or `if !assert.X`, plus the error return). The RunOnce factory at 3477: RunOnce calls a drive's factory from `prepareRunOnceWork` on its caller's goroutine (here the test's), but a shared folder's factory call, the one the assertion is there to catch, comes from `runChildrenForParent`, which `startChildrenForParent` starts on its own goroutine (`orchestrator_run_once_children.go:139`); so it takes rule (b), with a two-line comment (3475).
  - (c) 6 records built on the test goroutine before the run: 721 (cleanup ack ref), 1416, 1804, 1907-1908 (the harness's two snapshots, now passed to `parentSnapshotEngine`), 3471. None was left under the STOP rule: every record depends only on the parent's mount id, which equals the parent's canonical id (`mount_spec.go:386`), and the binding.
  - The four non-faults keep `require`, each with a one-line comment that the reconcile, apply or event handling below calls the factory on the test's goroutine: 2081, 2246, 2334, 2426.
  - Proof: TestRunOnce_PublishesParentChildWorkSnapshotBeforeStartingChildren with its fake's first assertion inverted failed in about a hundredth of a second: "Should be true" with the message that the parent child work sink was published more than once, the trace showing the call on the pass's per-drive goroutine, then the follow-on assertion that the pass had two reports. No panic, no hang. Restored.
- Note 7: `spec/design/system.md:303-313`: "(`runWatchInBackground` for `RunWatch` on the test's context)" and "Each package declares those its tests use in its own test helpers"; the rest of the paragraph rewrapped, unchanged.
- Note 8: `watch_lifecycle_test.go:83-88` now says a paused drive cannot stand in for one that never becomes ready; `watch_runner_stop_test.go:40-43` says the call is bounded because an event from a replaced runner that waited on its replacement's done channel would never return.
- Note 13: `cli/sync_runtime_test.go:28` names the parameter `_`; `_ = ctx` is gone.

Also fixed, found while there:

- `orchestrator_test.go:902` TestRunOnce_ParentCleanupRequestPurgesShortcutChildStateArtifacts built its cleanup ack ref inside the pass's fake, a seventh (c) site the finding did not list; built before the run like the others.
- `orchestrator_test.go:2602`: the control-socket-owned RunWatch test's factory would run on runWithin's goroutine; `require.Fail` became `assert.Fail` (the error return follows). Its two RunOnce twins (636, 2575) keep `require`: RunOnce calls the drive's factory on the test's goroutine.
- `orchestrator_test.go:1923-1938`: with (a) a failed parent run returns an error, and the watch restarts the runner after its backoff; the restarted run would close `parentStarted` again and panic the binary. Only the first run closes it now (doc comment on `parentSnapshotEngine`); on the passing path the run count is 1, as the test asserts.
- `orchestrator_test.go:1986`: `shortcutSnapshotWithContext` received only `t.Context()` after (c); it is `shortcutSnapshot(t, ...)` now, its four calls with it.

Tests added, renamed, removed: `internal/multisync`: subtest `closeWithin` added to TestWatchHelpers_FailNamingWhatTheyWaitedFor. `internal/cli`: the same subtest. `internal/sync`: none. Nothing renamed or removed. Both subtests were seen failing first with the helper's bound doubled (each failed on the bound check), then passed with it restored.

Corrected counts (notes 12, 14): own RunWatch calls through `runWithin` in multisync: 4 before this fix, 5 after. Accept loops folded into `occupyControlSocket`: 3 in `orchestrator_test.go`, 1 in `watch_reporting_test.go`. `cli/sync_notify_test.go:271` stays a plain call because the daemon refuses an empty drive list before it makes any orchestrator; a regression past that check would call the nil factory and panic, not hang. It is not a fake that returns at once.

Checks: build, vet, vet with all tags: pass. `gofumpt -l` and `goimports -l`: nothing. The review's named run: ok. multisync and cli: ok; with `-race`: ok (multisync again with `-race` after the last code edit but the lint directives: ok). internal/sync helper test with `-race`: ok, with `-timeout 30m` (the sync-lane rule) instead of 20m. RealRepoIsClean: 4 PASS. Lint: the first run found the three require-error issues above; the second run, on the final tree: `0 issues.` (W77-lint.log).

Not fixed:

- `internal/multisync/shortcut_child_work_test.go:42` and `test_helpers_test.go:54`: `testChildRecordWithContext` and `testShortcutChildAckRefWithContext` now only ever receive `t.Context()`. Fix: fold each into its t-only form.
- Note 9 (the sync helper test matches the sync stress pattern) and note 10 (`internal/synctest/child.go`) stay for the rows that own them.

## After the hand-back

- The first "Not fixed" item above is fixed: `testChildRecordWithContext` and `testShortcutChildAckRefWithContext` are folded into their t-only forms (vet, gofumpt and the package's tests pass).
- Rebased onto `ec5a0efe`; the one commit is `3bcf0c39`. No independent second review: the reviewer agents stopped on the weekly model limit. My own pass read the fix report against the tree, the helper file of `internal/multisync` whole, and ran both packages twice under the race detector.

## Merged as #938

Squash-merged on 2026-11-16 at 13:18 PST as `0e31e63c`; installed at 13:37. The post-merge stage passed (merge, worktree cleanup and post-merge CI). Test files and one design doc only, so the install changes no runtime behavior. The first nightly after the merge is still to be read.
