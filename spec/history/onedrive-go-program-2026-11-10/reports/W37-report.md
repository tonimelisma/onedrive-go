# Report: a chaos run is replayable from its seed — STOPPED at step 1

Stopped on a condition the brief names: step 1 shows the client's own requests
differ between two runs of one seed for one key. Condition (g) applies too (a
product seam is needed). No commit was made; the changes are uncommitted in the
worktree (base `3e5c1bd4`). How draws are made is unchanged.

## Step 1 table

| seed | mode | runs | runs that differed | first differing entry (first differing run) |
|---|---|---|---|---|
| 1 | plain | 1 | 0 | none |
| 2 | plain | 2 | 2 | log entry 326, stream `get-item remote-file-4`, entry 18: run 1 `call 4 step 8 storm=true open`, run 2 `call 4 step 9 storm=false open` |
| 1, 2 | plain x20, `-race`, `GOMAXPROCS=1`, `GOMAXPROCS=2` | 0 | not run | stopped: the condition above held on the first runs |

(The first seed-2 run, before call opens and steps were logged, differed at the
same call: `get-item remote-file-4` call 4 drawn in a storm in run 1, absent in
run 2.) One run of a seed took 78 s (seed 1) and 153-205 s (seed 2) plain, at
host load 5-10.

## What the log shows (fact)

In step 8 (a storm pass) both runs' logs are identical, entry for entry and in
the same global order, through entry 325 (`download remote-file-7` call 1).
Then run 1 sends `get-item remote-file-4` (call 4) and `get-item remote-folder-2`
(call 3, draws a 503) before `get-item remote-file-9` (call 2); run 2 sends
`get-item remote-file-9` first and sends `get-item remote-file-4` call 4 only in
step 9, in calm. Those are second attempts, within the same step, of calls that
failed earlier in it (`remote-folder-2` 502 at entry 287, `remote-file-9` 429
at 293). The draw inputs (seed, operation, key, count, storm) were not what
moved: the client chose different requests in the same step.

## Where the client depends on time (code, consistent with the log)

- A one-shot pass releases held retry work that is due on every frontier
  (`reduceReadyFrontier` -> `drainDueHeldWorkNow(now)`,
  `engine_runtime_start.go:72`, `engine_runtime_lifecycle.go:583-599`). Due =
  engine clock (`modelClock`: wall clock + offset) + `retry.ReconcilePolicy().Delay`
  (`engine_runtime_completion.go:692`), whose jitter is global `math/rand`
  (`internal/retry/policy.go:44`). So which retries a pass sends depends on
  elapsed wall time (host load) and unseeded jitter.
- Ramp batch members are admitted at `now + clock.Jitter(1s)`
  (`engine_runtime_release_ramp.go:231`; `realClock.Jitter` is global rand,
  `domain_clock.go:207`), and the one-shot loop waits for those instants in
  real time (`engine_run_once_results.go:162-171`).
- Probe: a `modelClock` frozen within a pass made seed 2 hang (40 min timeout)
  in `runResultsLoopWithOutbox` (`engine_run_once_results.go:180`) waiting for an
  admission instant that never comes. Freezing the clock is not a fix.

## Seam needed (g)

The retry delay's jitter cannot be seeded from a test: `retry.Policy.Delay`
draws from the global source, not from the engine clock's `Jitter`. A
replayable pass needs that jitter (and the ramp's) from an injectable source,
and the model clock to be virtual (advancing on `AfterFunc`/`Sleep`), or the
pass not to release due retries mid-pass in the model. Your call.

## Worktree state (uncommitted)

- `internal/sync/chaos_remote_test.go`: fault log (`chaosDraw`, `drawLog`); a
  `chaosCall` carries its identity and step and logs every open, roll, intN and
  fault (`fault` replaces `count`); `startStep`; `chaosRunOptions`,
  `chaosBatches`; the concurrent write draws through its call and is logged.
- `internal/sync/chaos_remote_session_test.go`: `cc.fault`.
- `internal/sync/engine_model_based_test.go`: `modelOptions.batchCount`,
  `startStep`, the rerun line logged by a failing chaos run.
- `internal/sync/chaos_replay_test.go` (new): `TestModel_ChaosSameSeedReplaysIdentically`
  (fails today, seed 2), `TestChaosRerunLine_NamesTheTestTheSeedAndTheBatchCount`
  (passes). Rerun line: `seed-replay: ONEDRIVE_GO_MODEL_SEED=2 go test -count=1 -run '^TestModel_ChaosGraphConverges$' ./internal/sync/` (`-race` added under race).
- Docs not written (`system.md`, header comment): they would describe a
  property that does not hold.

## Checks

`go vet ./internal/sync/` pass; chaos unit tests, `TestChaosCreateFault_*` and
the rerun-line test pass; `TestFindTestConventionViolations_RealRepoIsClean`
pass. Not run (stopped): build/vet of the whole tree, the whole package, race,
lint (no `W37-lint.log`).

## Seen and not fixed

- `internal/retry/policy.go:44`: jitter from global `math/rand`, not injectable; take it from a source the caller passes.
- `internal/sync/domain_clock.go:207` with `engine_model_based_test.go` `modelClock`: the model clock embeds `realClock`, so its `Jitter` is unseeded; override it with a seeded source.
- `internal/sync/engine.go:124-129` / `observer_remote.go:1026`: observers run on the wall clock, so a storm's throttled or failed delta page sleeps its Retry-After or backoff in real time (up to `chaosMaxRetryAfter`, 30 s); likely much of the chaos run time (not measured).
- `internal/sync/chaos_remote_test.go` `servePages`: next-page tokens come from one counter shared by every window (`pageSeq`), so a key depends on state shared between streams; derive it from the originating call.
- `internal/sync/chaos_remote_test.go` `wrap`: upload/create keys are `parentID/name`; a conflict copy's name holds a wall-clock stamp (`executor_conflict.go:211`), so its stream identity differs between runs; not seen in this log.



## Decision (mine, 2026-11-14 about 13:10 PST)

The row's rule ("a chaos run is a function of its seed and of what the client did, never of scheduling") does not hold today, and test code alone cannot make it hold: the log shows the client choosing different requests in the same step of two runs of one seed. The writer's reading of where that comes from (retry work released by elapsed wall time, jitter from the global random source) is consistent with the log and is not proven by it. The row is therefore cut in three.

1. **This row, second pass (test code only, same worktree, a new writer).** It lands what is true today:
   - the fault log, the call identity and step, the rerun line and its test, as they stand uncommitted in the worktree;
   - `TestModel_ChaosSameSeedReplaysIdentically` at the model level is taken out (it asserts a property that does not hold) and replaced by a test of the fault layer alone: two layers built from one seed, given the same calls in two different global orders, produce equal draws per stream. That is the half of the rule the fault layer owns, and it must be seen failing against a deliberately shared generator before it is trusted;
   - next-page tokens are derived from the call that produced the page, not from one counter shared by every window (entry 4 of "Seen and not fixed"; a smell in a file the change touches);
   - `spec/design/system.md` and the model's header comment say what a seed fixes (every draw of a stream, for a given call number and storm state) and what it does not (which requests the client sends in which step, which today depends on real time), and that the rerun line reruns the seed, not the run.
2. **New row W45: the chaos model runs in virtual time.** Its definition of ready first proves whether the model can run inside a `testing/synctest` bubble (which goroutines are not durably blocked there: the store, the fakes, the observers). It takes entries 3 and 5: observers that sleep on the wall clock instead of the engine clock (a product smell whatever the model does), and the conflict copy's wall-clock stamp. Whether this also removes most of a chaos seed's 78 to 205 seconds is not measured.
3. **New row W46: jitter comes from a source the caller passes, and same-seed replay is asserted.** `retry.Policy.Delay` and the clock's `Jitter` take their randomness from an injected source (entries 1 and 2), seeded per key in the model so that arrival order cannot move it. Then the model-level replay test comes back and is asserted over several seeds. After W45, and after the rows that change the retry loops (W31, W8).

Unknown and left so: whether goroutine scheduling order leaves further divergence after both rows. Each row's definition of ready has to measure it; neither may assume it.

The replay assertion that this row's brief asked for is owed by W46, not dropped.


## Second pass

Commit `593b93b6` on `3e5c1bd4` (not pushed): "Log every chaos draw by stream and print the line that reruns a failing seed". 5 files.

### What changed

- `internal/sync/chaos_remote_test.go`: fault log kept and tightened (`chaosDraw` fields are stream op/key, call, step, storm, `outcome`; `stream()`/`String()` beside the type; `fault` counts and logs under one lock); `setStorm` removed, `startStep` is the one way to set the storm; `pageSeq` removed: a split window's nextLinks are `chaos-page-<op>/<key>/call-<n>/page-<i>` (`chaosCall.pageTokenBase`, `chaosPendingWindow.token`); `chaosOpDeltaNextPage`; header comment on streams, call numbers, steps, storm count, fault log, what a seed fixes and does not.
- `internal/sync/chaos_remote_session_test.go`: `cc.fault`, `startStep`.
- `internal/sync/engine_model_based_test.go`: `batchCount`, `startStep`, rerun line on failure (as left).
- `internal/sync/chaos_seed_test.go` (new; replaces the untracked `chaos_replay_test.go`): the order test, `firstStreamDifference`, rerun line (now with `-timeout 30m`) and its parser, `TestChaosRerunLine_NamesTheTestTheSeedAndTheBatchCount`.
- `spec/design/system.md`: 18 lines appended to the chaos bullet, none reflowed.

### Items

1. Kept and fixed as above. No `seed-replay:` helper exists in `test_helpers_test.go`, so the chaos line keeps its own format.
2. `TestModel_ChaosSameSeedReplaysIdentically` deleted, with `firstChaosDifference`; the per-stream comparison survives as `firstStreamDifference`.
3. `TestChaosGraph_StreamDrawsIgnoreHowStreamsInterleave`: get-item, download, upload and two delta listing streams, storm then calm step, in turn against reversed, interleaved and one goroutine each. On the tree as it stood the fault-log comparison passed (no shared draw input). Mutant (`len(c.draws)` mixed into the PCG seed): `interleaved: a stream drew differently: stream "delta ", entry 1: "call 0 step 1 storm=true roll/12=5" in the first, "call 0 step 1 storm=true roll/12=6" in the second`. Restored.
4. Same test, answers comparison, failed first on `pageSeq`: `in reverse: a call got another answer: stream "listings from \"\"", entry 0: "5 items, next chaos-page-1" in the first, "5 items, next chaos-page-7" in the second`. No other reader of the token's form.
5. Done.

No chaos test changed its result: `TestModel_ChaosGraphConverges` passed plain (141 s) and under `-race` (125 s; seed 2 still draws both create faults).

### Checks

build, vet, tagged vet pass; gofumpt and goimports clean; `TestChaos` plain and `-race` pass; chaos model test pass; `./internal/sync/...` whole pass (748 s, lock); convention test pass; lint `0 issues.` (W37-lint.log); cited tests exist; deleted name nowhere; status clean.

### Seen and not fixed

- `internal/sync/scenario_remote_test.go:160` `nextID`: item IDs from one counter for every create, so a stream keyed by an item ID depends on the order creates of different paths arrived; derive the ID from the created path.
- `internal/sync/scenario_remote_session_test.go:296` `sessionSeq`: upload URLs from one counter, and `chaosSessionKey` (`chaos_remote_session_test.go:135`) keys a session without an item ID by its URL; derive the URL from the item or the create's parent and name.
- `internal/sync/engine_model_based_test.go:439` `failureContext`: names only `ONEDRIVE_GO_MODEL_SEED`, not the test or `-race`, so a chaos failure prints two rerun hints; give every model run the one rerun line from `runModelScenarioWith`'s cleanup.



## Review fixes (2026-11-14 about 17:25 PST, made in review, amended into the one commit)

- The commit is now `e7b7cda1` on `3e5c1bd4` (was `593b93b6`). The review of the second pass is `scratchpad/R-W37b-findings.md`: 3 must, 2 should, 9 notes.
- Narrowed what the code and the design doc claim (must 1, must 2, should 5): a seed fixes the values a call draws, in their order, for a given stream, call number and storm state. It does not fix how many values a call draws, the range another client's write draws from (the files the tree holds at that moment; nothing on an empty tree), which stream a request on a name the fake handed out falls in (item IDs, upload URLs and delta tokens are counted in arrival order), nor which requests the client sends in which step. Storm passes are the first passes of every loop the model syncs to quiet in. Changed: the header comment of `chaos_remote_test.go`, the header and the order test's comment in `chaos_seed_test.go`, `spec/design/system.md`, the commit message.
- A failing chaos run printed two rerun hints (must 3). `failureContext` now names only the seed for a chaos run; `TestModelRun_ChaosFailureContextCarriesNoSecondRerunHint` holds it.
- Smaller: the `calls` field comment (should 4); `firstStreamDifference` walks stream names sorted, so its answer is the same on every run (note 9); the order test's guard no longer counts another client's write as a failed follow-up page (note 10); the two new tests cited requirements they do not prove, so the tags are gone (note 11).
- Tried and taken back: drawing the other client's file from a fixed range before the tree is read. `TestModel_ChaosGraphConverges/seed=2` then failed its own assertion twice ("seed 2 drew no upload-session:timeout-after-create fault") and its plain run took about 360 s instead of 134 s. The change renumbers what every seed draws, so the race seed would have to be chosen again; why the run got longer is unknown. Recorded as a rider of the virtual-time row, with three more: names the fake hands out derived from what they name, whether a seed that hangs until `-timeout` prints the rerun line (not checked), and saving the fault log of a failing run.
- Checks on the amended tree: `gofumpt -l` and `go vet ./internal/sync/` clean; the four tests `TestChaosGraph_StreamDrawsIgnoreHowStreamsInterleave`, `TestChaosRerunLine_NamesTheTestTheSeedAndTheBatchCount`, `TestModelRun_ChaosFailureContextCarriesNoSecondRerunHint` and `TestModel_ChaosGraphConverges` pass plain (134 s of test time) and under `-race` (149 s); `TestFindTestConventionViolations_RealRepoIsClean` passes. The lint over `./internal/sync/...` was still queued behind the heavy-run lock when this was written; its result is in the merged section below or in the gate.
- Open for the rebase: the rerun line's format against the seed-replay line of the race-and-seeds change if that merges first, and whether devtool's rerun of a failed test carries the model seed (review note 13, not checked).



## Merged as #913 (`51e554fa`, 2026-11-14 about 18:37 PST)

- One commit `d8d6bfdf` on main `ec6b0417` (rebased about 17:33 without conflicts; before the rebase it was `e7b7cda1` on `3e5c1bd4`). The pre-pr gate waited 41 minutes behind the heavy-run lock, then ran from 18:16 and passed (lint first, then the quick tier). Pre-merge passed in 246 s with no review thread; the post-merge stage merged, removed the worktree `chaos-replay-from-seed` and passed in 166 s.
- Installed `51e554fa` at 18:39; the watch client restarted and reports "Up to date, watching for changes", 19 files.
- No live proof: the change is test files and one design document; no product file changed.
- Still open: (1) the line that reruns a failing seed has two shapes until the race-and-seeds change is rebased onto this one. This commit prints `seed-replay: ONEDRIVE_GO_MODEL_SEED=<seed> go test[ -race] -count=1 -timeout 30m -run '^<test>$' ./internal/sync/`; the reader of that change takes only `<assignments> go test [-race] -run '^<test>$' ./internal/sync`. My decision for that rebase: one writer and one reader; the strict shape of the reader wins, the chaos test calls the shared helper, and this commit's own line builder and its parser go. (2) The seed tests are to carry the seed-replay requirement of that change once it exists. (3) The riders written into the virtual-time row (W45) and the jitter row (W46).
- Free after it: W45 once the transport-timeout change (W31) and the race-and-seeds change (A11) are in.
