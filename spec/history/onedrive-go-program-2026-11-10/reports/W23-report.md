# W23 report: watch absences proven by a signal; session-route chaos

Branch `test/sync-absence-and-session-chaos`, one commit 93a74cfe on 86674d33, not pushed. 25 files, +1017 -422. No live proof: test files, debug events and a move of the debug event declarations only.

## What changed
- `domain_debug_events.go` (new, LAYER 0): `engineDebugEventType`, `engineDebugEvent`, `DebugEvent`, observer names, the two observation events; `engine_debug_events.go` keeps the sink and the engine's own kinds (observer files, LAYER 2, may not name a LAYER 4 type).
- `observer_local.go`, `observer_remote.go`: one `debugEvents` field each; `observer_local_collisions.go`: `reportCaseCollisionSkip`; `observer_local_handlers.go`: one added call at each of the three collision drop sites; `observer_remote.go`: `drainWakeSignals` became a method that reports what it drops; `engine_watch.go`, `engine_primary_root_watch.go`: the runtime hands `rt.deps.emit` to both watch observers.
- Tests: `observer_local_collisions_test.go`, `observer_local_write_test.go`, `engine_watch_test.go`, `observer_remote_test.go` (the conversions below); `watch_test_helpers_test.go` (`requireDroppedWithoutEvent`, `requireNoChangeEvent`, `collisionSkipOf`, `startMockWatchInLoop`, `DebugEvents` option); `engine_test_helpers_test.go` (`newDebugEventRecorder`, `record`).
- `scenario_remote_session_test.go`, `scenario_remote_test.go`: the fake session takes fragments (4 x 320 KiB), keeps them across a failed request, answers a status query with the next expected range, 416 for an out-of-order fragment, 404 when its item changed; records the fragments that reached it; `interruptFragmentAt` knob.
- `chaos_remote_test.go`: `chaosAnswer` table, each answer's `createMarked` written once; creates draw a not-sent answer too; `chaosCreateFailure(name, answer, err)`. `chaos_remote_session_test.go`: per-fragment faults (`fragmentFault`, `:part-sent`), a final fragment's failure is `FinalFragmentError` as in graph.
- Docs: system.md Verification Policy (absence rule, chaos/session text), sync-domain.md (GOVERNS, overview), sync-engine.md (sink handed to observers), sync-observation.md (Verified By row).

## Absence by a window (internal/sync: 12 tests, 13 windows; all converted)
| Test | File | Rules out | Signal now |
|---|---|---|---|
| TestWatch_CaseCollision_EventSuppressed | observer_local_collisions_test.go | create event of a colliding name | `local_case_collision_skipped` (handleCreate) |
| TestScanNewDirectory_CaseCollision_Skipped | same | file events of colliding entries in a new dir | dir event, then two skips (scanNewDirectoryEntry); case-insensitive FS: the one file's event |
| TestWatch_DirectoryCollision_Suppressed | same | folder event Xyz | skip (handleCreate) |
| TestWatch_TwoDirectoryCollision_Suppressed | same | folder event docs | skip (handleCreate) |
| TestScanNewDirectory_SubdirCollision_Suppressed | same | ABC/abc folder events | parent event, two skips |
| TestWatch_DeleteCollider_ReEmitsSurvivor | same | create of file.txt | skip |
| TestWatch_DeleteCollider_ThreeWay_StillBlocked | same (2 windows) | creates; re-emitted peers' creates | two skips; the delete event, sent after the re-emission |
| TestHashAndEmit_CaseCollision_Suppressed | observer_local_write_test.go | modify after coalesced write | skip (HashAndEmit) |
| TestHashAndEmit_CaseCollision_CachedLookup | same | same | skip (HashAndEmit) |
| TestRunWatch_WebsocketDisabledKeepsPollingOnly | engine_watch_test.go | wake source started | local `observer_started` (after the wake-source decision) + factory call count |
| TestRunWatch_MountRootKeepsPollingOnly | same | wake source for a mount root | `websocket_fallback` + local `observer_started` + count |
| TestWatch_WakeSignalsCoalesce | observer_remote_test.go | a third poll | `remote_wakes_coalesced` Count=1, burst queued during the first apply |
Not windows: virtual_clock_test.go:64, :144 (synctest quiescence). multisync 5 tests and cli 0: the engine hook does not reach their mock engines (Seen and not fixed 1-3).

## Events added
- `local_case_collision_skipped`: the local observer skipped an entry whose name collides (R-2.12.2) at a watch create, a new directory's scan entry, or a coalesced write's hash. Path, Note = colliding name.
- `remote_wakes_coalesced`: the remote observer dropped Count wakes waiting when a poll began (that poll covers them). Only when Count > 0.

## Each converted test shown able to fail (working copy, restored, diff checked)
- handleCreate collision `if` disabled: CaseCollision, DirectoryCollision, TwoDirectoryCollision, ReEmitsSurvivor, ThreeWay fail at once ("unexpected event"). scanNewDirectoryEntry check disabled: both ScanNewDirectory tests fail at once. HashAndEmit check disabled: both HashAndEmit tests fail at once.
- HandleDelete re-emits peers by plain TrySend: ThreeWay fails (create FILE.txt before the delete). `!enableWebsocket` guard removed: WebsocketDisabled fails (1 factory call). Wake source started before the mount-root check: MountRoot fails. Drain call removed: WakeSignalsCoalesce fails (no report; a third poll in the log).
- New tests: 408 `createMarked` flipped fails the pin test's 408 case; resume from 0 fails the resume test (416).

## Tests added
- `TestScenarioSession_ResumeSendsOnlyTheMissingRanges`: a fragment fault leaves the session holding two fragments; the resume starts at the next expected range and every fragment that reaches the session is accepted, summing to the file once.
- `TestChaosCreateFault_MarksWhatTheGraphClientMarks`: every create answer of the layer (seven transient, not-sent) played to `graph.CreateFile` from a loopback server; graph's mark equals `createMarked`; not-sent reaches no server.
- `TestChaosGraph_FragmentFaultLeavesAPartUploadedSessionToResume`: storm fragment faults leave part-uploaded sessions (each a counted `:part-sent`), which resume whole without resending.

## Checks (final tree)
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass. gofumpt, goimports: clean.
- Non-race `-run` over the converted, added and touched tests (all `TestWatch_`, `TestRunWatch_`, `TestScanNewDirectory_`, `TestHashAndEmit_`, `TestHandleWrite_`, `TestChaosGraph_`, the chaos model, the replay test, the session and delta-expiry RunOnce tests, the `ListsEvery|Table|Governs|Exhaustive|Registered` tests): ok 157 s.
- Same set (without the replay and list tests) `-race -cpu 1 -count=5`: ok 1023 s. Seeds unchanged (race seed 2, defaults 1, 2): the not-sent answer changes create draws, and under `-race` seed 2 still draws `create:response-lost` and `upload-session:timeout-after-create`.
- Lint `./internal/sync/...`: `0 issues.` (W23-lint.log). Layering checked by hand: observer files name only `domain_debug_events.go` declarations.

## Deviations
1. The model does not write large files (decision part 2 stopped). With one large local write per chaos batch (patch kept at scratchpad W23-model-large-write.patch), the model fails on the product: 4 of 24 seeds at two batches, seeds 2 and 6 at six, and seed 3 on one run but not another (the chaos run is timing-dependent). Every failure is `invariant: no upload session outlives its upload`: a large upload a fragment fault interrupted keeps its persisted record for resume; a concurrent write then makes the path a conflict (conflict copy uploaded, remote downloaded), the original upload is never retried, and its record stays in `upload-sessions` until `CleanStale` (7 days), its server session never canceled. Product, out of scope; the session-fault assertion is not in. Instead `TestChaosGraph_FragmentFaultLeavesAPartUploadedSessionToResume` draws fragment faults through the layer.
2. Three converted tests never reached the decision they name: on a case-sensitive FS `existing.txt` did not exist, so the create was dropped by a failed stat; the two scan tests created their directory before the watch's setup walk registered it, so the create was a duplicate and nothing was scanned. Setups fixed (`startMockWatchInLoop`, both spellings written); product unchanged.
3. Debug event declarations moved to the domain family (layering); not a new seam type.
4. TestWatch_WakeSignalsCoalesce uses a wake channel of two so the burst queues deterministically during the first apply.

## Also fixed
- `startTestRemoteBatchDrain` and TestWatch_WakeSignalTriggersImmediatePoll ran goroutines no one waited for; both now go through `runInBackground` (16 call sites take `t`).
- engine_primary_root_watch.go `_ = ctx`; handlers' comments named `DetectCaseCollisions` (it is `detectCaseCollisions`); collisions file header missed three functions; CachedLookup's comment sat on the wrong field; `SetDebugEventHook`/`emitDebugEvent` undocumented; TestChaosGraph_SessionRouteDrawsFaults misread a failed final fragment.

## Seen and not fixed (file:line, what, fix)
1. internal/multisync/watch_runner_stop_test.go:734 `assert.Never` 200 ms (lock held while draining). Fix: a signal that the orchestrator waits on the runners, then one lock attempt.
2. internal/multisync/orchestrator_test.go:3300, :3475, :3552 `assert.Never` 200 ms after a reload. Fix: the reload's answer (or the warning already received) is the signal; check the counts once.
3. internal/multisync/watch_config_file_test.go:25 `requireNoTrigger` 100 ms, used :47, :51, :61. Fix: drive the poller with an injected ticker and assert after a known number of polls.
4. internal/multisync tests: event waits of 5 s (`testWait`, `contextPerfWait`, `controlTestGuard`, literals). Fix: one named event wait like sync's `eventWait`.
5. internal/driveops/transfer_session.go:326 (record kept for resume) with internal/sync/executor_transfer.go:518: a persisted session of an upload the plan replaced (conflict, download) is never dropped or canceled. Fix: drop the record (cancel its session, R-5.6.13) when the path's upload is superseded; then apply the patch and assert session faults.
6. internal/sync/planner.go:294: dependency cycle seen with an intermediate variant of this change (chaos seed 3, two batches): local "move docs -> photos/photos, recreate docs" then "move photos/photos/50%.txt -> docs/renamed-50%.txt"; folder_create docs and two remote_moves were dropped as a cycle on every pass, never converging. Not reproducible by seed in the final tree. Fix: a scenario test of those two operations, then the planner breaks the cycle.
7. internal/sync/chaos_remote_test.go:657: chaos seeds do not replay across runs (with the large-write patch, seed 3 passed one six-batch run and failed another; seed 8 drew differently with and without -race). Cause unknown. Fix: find what the draws depend on beyond the seed, then make the chaos run replayable like the plain model.

Unfinished: decision part 2 (model large writes and its seed assertion), blocked on 5. Packages to test: none beyond internal/sync.

## Review (first pass, 2026-11-13 14:55, on `93a74cfe`)

Second pass asked for; it is small. A read-only reviewer read the whole diff; its findings are in the session scratchpad (`R-W23-findings.md`). I checked finding 1 myself (both comments are there). The rest stands on the reviewer's reading until the second pass's tests and my own suites prove or disprove it.

Borne out by the reviewer's reading: 12 tests and 13 windows converted, each with a variant that fails; the 16 drain call sites; the fake's 416 and resume behavior; the pin test plays every create answer through `graph.CreateFile`. Not checked by anyone yet: lint, the race runs, the seed numbers in deviation 1. Those are mine before stacking.

Must fix:

1. Two comments still name `DetectCaseCollisions`, which this commit removed: `internal/sync/observer_local.go:114` and `internal/sync/observer_local_collisions.go:37`.

Should fix:

2. `spec/design/system.md:289` now forbids every absence window, while five multisync tests keep seven of them. In this pass: the three sites in `internal/multisync/orchestrator_test.go` (`:3300`, `:3475`, `:3552`) need no product seam: count the `engineFactory` calls and read the count once after the reload's answer or warning; `:3478` already reads once, so the `Never` at `:3475` goes. The two that need a seam (`watch_runner_stop_test.go:734`, `watch_config_file_test.go:25`) keep their window with a comment that names the signal they are waiting for and the plan row (W35), and the sentence in `system.md` names those two exceptions until W35 removes them.
3. `internal/sync/engine_watch_test.go:165-166` and `:224-225`: after `stopWatch`, also assert `wakeSourcesMade.Load() == 0`.
4. The three-way collision test: require a second `collisionSkipOf("FILE.txt")` report before the delete.

Verdicts on what the first pass handed back:

- Deviation 1 (the model's large writes and the seed assertion are not in the tree) is accepted. The failure is a product fault: an upload sync gives up keeps its session record and its server session. It is plan row W33; the second part of this row lands with W33, and `W23-model-large-write.patch` stays in the scratchpad until then.
- Seen and not fixed 1, 3 and 4 (and the 2 s waits in `internal/cli/signal_test.go`, which the list missed) are plan row W35. Entry 2 is fixed in this second pass (should fix 2).
- Entry 5 is W33. Entry 6 (the planner cycle) is W36: the two operations become a scenario test first. Entry 7 (a seed does not fix a chaos run) is W37.

After the second pass: rebase onto the stack tip, my own run of `./internal/sync/...` and `./internal/multisync/...` with and without the race detector, lint, then the stack.

## Second pass

Commit: `d5e077e6` (amended). It sits on `91113542`, not on main's current `bb0d76fd`. Not pushed.

### Rebase
`git rebase --onto 91113542 86674d33` applied the first-pass commit cleanly as `4644c69d`. No conflicts were resolved by hand.

### What changed
- Must fix 1: the comments at `internal/sync/observer_local.go:114` and `internal/sync/observer_local_collisions.go:37` now name `detectCaseCollisions`.
- Should fix 2, converted with no product seam (`internal/multisync/orchestrator_test.go`). Each test counts the `engineFactory` calls and reads the count once, after the reload's answer or warning. The reload answers only after `applyWatchMountSet` has returned (`watch_reload.go:64-77`), and that call makes the restarted mounts' engines synchronously, so the count is final by then. The `assert.Never` windows are gone.
  - `TestOrchestrator_Reload_ContentFilterChangeRestartsOnlyAffectedMount` (:3232): after the reload answer, `made[rd1]==2`, `made[rd2]==1`, `stopped[rd2]==0`. The wait for rd1's second start stays.
  - `TestOrchestrator_Reload_InvalidConfig` (:3425): after the 422 answer, `made==1`, next to the existing `started` read.
  - `TestOrchestrator_Reload_EmitsMountLocalCompilationFailures` (:3487): after the warning, `made==1`.
- Should fix 2, the two windows kept. Each has a comment naming the signal it lacks and plan row W35:
  - `internal/multisync/watch_runner_stop_test.go:735`: the orchestrator's report that it is waiting for the canceled runner to drain.
  - `internal/multisync/watch_config_file_test.go:25` (`requireNoTrigger`): the poller's report that it looked at the file and decided not to reload.
- `spec/design/system.md:326-334`: the absence bullet now says that in multisync the reload's answer or warning is the signal, and names the two tests that keep a window until W35. The sentence about the chaos limit is unchanged, and part 2's patch is not applied.
- Should fix 3: `internal/sync/engine_watch_test.go:169` and `:233` read `wakeSourcesMade` again once `stopWatch` has returned (`noWakeSourceAfterTheDecision`).
- Should fix 4: `TestWatch_DeleteCollider_ThreeWay_StillBlocked` requires two `collisionSkipOf("FILE.txt")` reports once the delete event arrives. They are counted with a new `debugEventRecorder.countEvents` (`engine_test_helpers_test.go`). Its comment now says that the delete re-emits FILE.txt, which still collides with File.txt.
- Commit message: the websocket and three-way bullets now cover the new reads, and a paragraph covers the multisync reload tests and the two remaining windows.

### Failing form of each converted test
Each was shown on the working copy, then the product file was restored with `git checkout --` and the diff checked.
- (A) `watch_runner_stop.go:138` changed to `mountSpecsEquivalentForWatchRestart(wr.mount, next) && next == nil`, so every mount restarts on reload:
  - ContentFilterChange fails: `made[rd2]` is 2 and `stopped[rd2]` is 1.
  - EmitsMountLocalCompilationFailures fails: `made` is 2.
  - InvalidConfig still passes, because a reload that does not load never reaches the mount set.
- (B) (A), plus `watch_reload.go:28` falling back to the current config and mounts on a load error: InvalidConfig fails (`made` 2, `started` 2, and the 422 expectations fail).
- ThreeWay: HandleDelete's peer re-emission (`observer_local_handlers.go:1144`) removed. The test fails at `observer_local_collisions_test.go:562`: expected 2, actual 1.
- Websocket: a `socketIOWakeSourceFactory` call added at the start of the watch's cleanup (`engine_watch.go:232`), i.e. a wake source made after the decision. Both tests fail only at the after-stop read (`:169` and `:233`, "Should be zero, but was 1"); the decision-time reads still pass.

### Commands and results (final tree; the amend committed exactly these files)
- `go build ./...`: pass. `go vet ./internal/sync/... ./internal/multisync/...`: pass. `go vet -tags "e2e e2e_full" ./e2e/...`: pass. gofumpt and goimports on the eight changed Go files: clean.
- `go test -count=1 -timeout 60m ./internal/sync/...`: ok, 897 s.
  - A first attempt was stopped at my own 10-minute background limit while another builder's sync suite ran beside it. It had no result and is not counted.
- `go test -count=1 -timeout 60m ./internal/multisync/...`: ok, 3.7 s. A `-v` rerun showed 190 passing tests, including the five touched ones.
- `go test -count=1 -race -timeout 60m ./internal/multisync/...`: ok, 36 s.
- `go test -count=1 -race -timeout 110m ./internal/sync/...`, first run: load failure. It timed out at 1h50m in `TestCrashRecovery_EveryCrashPointConverges/delete.local_trashed/power-loss`, with no failed test and no data race before that. For most of the run, two other builders' sync suites ran beside it on the two cores (`pending-create-lifetime` and `shared-folder-release`), and a B2 pre-pr gate ran for part of it. Rerun once under `flock heavy.lock`:
  - The first rerun was killed during the host pause and gave no result.
  - Started again under the lock after the pause, on `d5e077e6`: ok, 5674 s (95 min), with no other heavy run beside it. That is 15 minutes inside the 110-minute deadline, so on this host the whole tree under `-race` needs a deadline of at least 100 minutes.
- Lint, under the lock: `golangci-lint run --allow-parallel-runners ./internal/sync/... ./internal/multisync/...` (GOTOOLCHAIN=go1.26.11) gave `0 issues.` (W23-lint.log).

### Seen and not fixed
1. `internal/sync/scanner_test.go:18`, `:319`, `:491` (section headers) and `:556` (a comment) still say `DetectCaseCollisions`. The `TestDetectCaseCollisions_*` names in that file and in `spec/design/sync-observation.md:577-578` use the old spelling too. The review named only the two product comments; renaming tests the spec cites would widen this commit.
2. The original brief (line 38) forbids plan IDs in code, tests and spec files. This pass writes "plan row W35" in two test comments and in `spec/design/system.md`, because the second-pass instruction asked for exactly that. The commit message names no plan ID. Coordinator to decide whether the ID stays in the tree or is reworded to name only the missing signal.


## Review of the second pass and restack (lead, 2026-11-14 01:40 Pacific)

- Accepted: the four first-pass findings are done and each converted test was shown able to fail.
- My edits, amended into the commit: the two kept multisync windows' comments and `spec/design/system.md` no longer carry a plan id (my own second-pass message had asked for one, against the rule); the `scanner_test.go` comments say `detectCaseCollisions` (item 1 of "Seen and not fixed"; the `TestDetectCaseCollisions_*` names stay, they are ordinary Go test names).
- Restacked from `91113542` onto main `bd2711b0` with no conflict, but the test package no longer compiled: `graph.ChunkAlignment` is not exported on main. `scenarioSessionFragmentSize` is now `graph.ChunkedUploadChunkSize / 8`, the same 1.25 MiB. `go build ./...` and `go vet ./internal/sync/... ./internal/multisync/...` pass; the session and fragment tests pass. The pre-PR gate runs on the restacked commit.


## Merge note (2026-11-14 06:15 PST)

Merged as PR #903, main `b5d2ccf4`, installed 06:16. The first pre-PR gate failed in lint on the linter's fixed 15-minute deadline (0 issues, then a timeout at 16m36s on a loaded host); the rerun passed unchanged. The pre-merge stage passed. The post-merge stage merged the PR and then failed on one `gh run list` call that could not reach GitHub (gh's own message for a host it could not connect to, which devtool's retry does not recognise yet); its second run passed. The model's large writes wait for the product fix of the upload session that outlives its upload.
