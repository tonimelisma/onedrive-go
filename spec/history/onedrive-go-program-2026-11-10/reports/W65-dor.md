# W65 Definition of Ready: a stopping run records its completions on a detached, bounded context

Worktree `~/src/onedrive-go-wt/stop-records-completions`, branch
`fix/stop-records-completions`, HEAD `ca8ecac6`. Paths below are under
`internal/sync/` unless named otherwise; line numbers are at `ca8ecac6`.

## Brief questions, answered from the code

**1. What each caller applies during a stop, and on which context.**
- One-shot: `engine_run_once_results.go:207-211` `handleOneShotCompletion`
  sends every completion that arrives once `fatalErr != nil || ctx.Err() != nil`
  to `processShutdownCompletion(ctx, ...)` with `ctx` = the pass context
  `runCtx` (`engine_run_once.go:471-479`), which the user's cancel or `failPass`
  has canceled. (A fatal error from `releaseIdleDueHeldWork`, `:121-124`, sets
  `fatalErr` without a cancel; those completions are applied on a live context.)
- Watch: `engine_watch_drain.go:92` `handleDrainingCompletion` calls
  `processShutdownCompletion(ctx, ...)` with the watch context; the drain begins
  only once that context is done (`engine_watch_loop.go:102,132,266,616`).
- `engine_runtime_shutdown.go:12-21` adds only the freshness-bypass value to the
  ended context and runs the whole completion stage
  (`engine_runtime_completion.go:24-66`) on it: every store write of the
  decision (`recordRetryWork` :590, `forgetRemoteItemProvenGone` :202,
  `forgetLocalCtimeProvenChanged` :218, `parkWedgedAction` :828, scope
  activation `engine_runtime_lifecycle.go:275`, trial release
  `engine_runtime_release_ramp.go:212`) fails at once with `context canceled`,
  and the error is logged at Debug as a cancellation echo
  (`engine_runtime_shutdown.go:61-66`) except for a completion already lost to
  the store. Only the retry-work clear of a success is detached already
  (`engine_retry_trial.go:82`, own 30 s bound, hand-rolled, no
  `errStoreAccessTimedOut`).
- What a success commits is not in the drain: the worker commits the baseline
  itself on `runDetachedStoreAccess` (`worker.go:236-262`).

**2. The test that shows the loss (seen failing at `ca8ecac6`).**
`TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains` and
`TestWatchRuntime_ADrainRecordsTheCompletionsItReceives`
(`engine_runtime_shutdown_test.go`, real `SyncStore`, which honors its
context): three completions of a stopped run (a 502 upload, a download whose
item is gone, a folder create whose commit outlasted its bound). Both fail:
retry rows `[]` instead of `report.txt, Projects`; remote row of the gone item
still present. Log: `record retry_work for Projects: ... begin transaction:
context canceled`; the 502 and the gone item are lost at Debug, silently.
Log: `S/w65/dor-test.log`. Something is lost, so no stop condition.

**Found on the way (same rule, `worker.go:371-380`):** `sendResult` selects
between sending the completion and `<-ctx.Done()`. After a stop both are ready
and Go picks at random, so about half of the completions that finish after a
stop never reach the drain at all, even with room in the buffer (one-shot's
buffer is the plan size, watch's 4096). A few lines in a file this change
touches: fixed here with its own test (a send with room is never dropped).

**3. How many completions one drain can hold.** Dispatch is unbuffered in both
modes (`engine_run_once.go:457`, `engine_watch.go:25`), so a completion exists
only for an action that was dispatched and not yet completed. At most:
one-shot, the plan's actions (buffer = plan size, `engine_run_once.go:466`);
watch, `watchCompletionBuf` (4096) buffered plus one blocked send per worker.
In practice both loops read completions before dispatching more
(`engine_run_once_results.go:138-150`), so a drain holds about one completion
per worker. Bounded; and with one deadline the drain's time does not depend on
the count.

**4. Bound chosen: one deadline for the whole drain (decision b, default).**
Per-write bounds do not fit: a drain of N completions, each with up to two
sequential detached writes (record, then nested retry clear), would take up to
N x 30 s. The drain's one deadline is set once, when the run first sees its
stop (watch `beginWatchDrain`, one-shot the first loop turn after the stop),
at `OutcomeCommitTimeout` from then; each drained completion runs on a context
the existing helper derives from the run's (split into
`detachedStoreContext(ctx, deadline)` + `storeAccessError`: values kept,
cancel dropped, that deadline with cause `errStoreAccessTimedOut`). (Refreshed
after lint: holding one context in a struct was refused by `containedctx`.) A detached access nested in a detached context
shares its bound (the success's retry clear moves onto `runDetachedStoreAccess`).
Worst case of the drain: `OutcomeCommitTimeout` = 30 s from the stop's first
sight plus in-memory work (microseconds per completion), whatever the count.

**5. The rest of a drive's stop inside 45 s** (`multisync/watch_runner_stop.go:18,28`:
30 s + 15 s slack, measured from the cancel, `:63`).
- Workers, in parallel with the loop: an outcome commit or created-item record
  that was running at the cancel, each on its own 30 s bound
  (`worker.go:258`, `executor_transfer.go:637`), ends by cancel + 30 s; a
  worker's later completion still reaches the drain, which then has what is
  left of its own bound.
- Loop: sees the stop in milliseconds from its select or from a step on the
  run's context. Drain: at most 30 s from then. Loop return, `pool.Stop`,
  pipeline cleanup: milliseconds. The engine close has its own bound after it.
- Sum, store healthy: milliseconds. Store not answering, loop not inside a
  detached wait at the cancel: 30 s + ms <= 45 s (15 s left).
- Residual case (stated, not fixed): if the cancel lands while the loop
  goroutine is itself inside a detached wait (a live success's retry clear,
  `engine_watch_loop.go:389` findings picked in the same instant, the
  shortcut-release restore), the drain starts after it: 30 + 30 s > 45 s and the
  runner is reported stuck, which `watch_runner_stop.go:24-27` defines as the
  outcome for a store that needs two waits. Today that case is worse: every
  drained success adds its own 30 s clear (N x 30 s).
- The 45 s limit is not changed (decision g).

**6. What the next run does today with a result the store never got**
(`R-2.5.3`: rediscovered from truth and replanned). Per kind (upload,
download, local/remote move, local/remote delete, folder create):
- Success, any kind: nothing is lost by the drain (baseline committed by the
  worker, retry clear detached). Out of scope: a worker commit that itself
  failed (the executor's own record).
- Failure, any kind (transient, actionable, 429/507/disk scope, a store
  failure): the retry_work row and a scope row are lost. The next run plans the
  action again and tries it at once, without backoff or the scope's pause: a
  repeat of the failed attempt; status does not list an actionable condition
  until that attempt fails again. No conflict copy, no data lost.
- Superseded by a remote 404 (download, move, delete): remote row not
  forgotten; the next plan targets it, gets the 404 again: a repeat.
- Superseded local delete (content changed): stored ctime not forgotten; the
  next scan trusts the row and plans the delete again; the executor's content
  check refuses again before deleting: a repeat, no data lost.
- Trial outcome (release/extend/rearm) and a wedged action's park: the next run
  starts from the scope or streak as before the trial: a repeat (a delay).
Verdict: a loss of records, never of user data; the cost is repeated work.

## Design (decisions a-h applied)

- Helper (`worker.go`): `runDetachedStoreAccess` = `detachedStoreContext` +
  `storeAccessError`; a context already detached keeps its bound for nested
  accesses; no second helper. `sendResult` delivers whenever the buffer has room.
- Drain (`engine_runtime_shutdown.go`): `engineFlow.stop` holds the one
  deadline, the count of results it cut, and the first such error.
  `beginStopDrain` (idempotent) at the stop's first sight;
  `processShutdownCompletion` runs each completion on a detached context with
  that deadline, marked as a stopping run's; `endStopDrain` at the end of the
  drain says the cut results once (Warn with the count) and returns the store
  failure.
- A stopping run's completion starts nothing: under the marker, admission
  returns the ready set (completed as shutdown, no freshness read, no held-row
  write), the publication drain applies nothing, the permission probes do not
  run (no Graph request during a stop: the failure is recorded as retry work),
  and persisted invariant checks are skipped as for a canceled context. The
  freshness bypass in `action_freshness.go` becomes dead and is removed.
- One-shot: the stop also seals the outbox (as the watch drain does) and stops
  re-selecting the ended context, which today busy-spins the loop
  (`engine_run_once_results.go:98-105,191,243-249`) until the workers return;
  a stopped pass no longer releases due held work.
- Exit (d): the one-shot loop returns the drain's store failure when it has no
  earlier fatal error; the watch drain step returns it, and
  `isWatchShutdownError` no longer reads an error carrying
  `errStoreAccessTimedOut` as a clean stop (as `classifyResult` already does).
- (e): the clean path creates one context at the stop and waits on nothing new.

## The 12 items

1. PASS **Task intent**: a stopping run (one-shot or watch) records the
   results of completions it drains on a context detached from its cancel under
   one bound for the whole stop; a store that does not answer ends the drain at
   that bound, the cut results are said once and the run exits as for a store
   failure.
2. PASS **Non-goals**: the executor's own records (outcome commit, created-item
   record); the 45 s stop limit; `internal/multisync` (read only),
   `internal/graph`; what a completion writes; no stored field, no store
   generation change, no new issue type; no e2e test; findings-after-cancel and
   other hand-rolled detaches outside touched files (listed as not fixed).
3. PASS **Governing docs**: `spec/design/sync-engine.md` (engine_*), 
   `spec/design/sync-execution.md` (worker.go, action_freshness.go; one line),
   `spec/design/sync-control-plane.md` (Stopping a runner, read),
   `spec/requirements/sync.md` R-2.8.3, R-2.5.3;
   `spec/requirements/non-functional.md` R-6.3.9, R-6.6.21; repo CLAUDE.md
   Shutdown and Lifecycle.
4. PASS **Authority owner**: the engine's runtime (`engineFlow`, one goroutine)
   owns the drain, its context and its count; the store owns the rows; the
   worker pool owns delivery; the multisync control plane owns the 45 s limit;
   user-facing output is the one Warn plus the run's error (CLI prints it).
5. PASS **Code scope**: `worker.go`, `engine_runtime_shutdown.go`,
   `engine_runtime_types.go`, `engine_run_once_results.go`,
   `engine_watch_drain.go`, `engine_watch.go` (`isWatchShutdownError`),
   `engine_runtime_admission.go`, `engine_runtime_completion.go`,
   `engine_runtime_permissions.go`, `engine_runtime_invariants.go`,
   `engine_retry_trial.go`, `action_freshness.go`; tests in
   `engine_runtime_shutdown_test.go`, `request_timeout_outage_test.go` (rider),
   `worker_test.go`; spec as in 3. Not touched: files other branches change
   (executor*.go, actions.go, crash_points.go, store_write_baseline.go,
   dispatch_order_test.go, planner*, multisync).
6. PASS **Regression plan**: the two tests of question 2 (fail today); a store
   that never answers ends the drain at its one bound and the loss is said once;
   every record of a drain, a success's nested clear included, shares that one
   bound (the stop limit); a stopped pool still delivers completions it has room
   for; a canceled one-shot pass seals its outbox; the rider test.
7. PASS **Verification plan**: build, vet (plain and with tags); targeted runs
   of new tests seen failing first; each new test that waits on a stopping
   engine 30x under `-race`; `go test ./internal/sync/...` and
   `-race ./internal/sync/` (90m) behind the lock; lint behind the lock last;
   `RealRepoIsClean`; tests named `ListsEvery|Table|Governs|Exhaustive|Registered`.
8. PASS **Data safety**: no delete, overwrite or local/remote mutation is added;
   the drain writes only the rows the completion stage already writes for a
   live run, and starts no new work (no admission, publication, probe). The
   store keeps its transactions; a cut write fails whole.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata` or
   account names; test ids invented.
10. PASS **Live provider and destructive operations**: none; no live test, no
    dependency or history change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does anything in the completion stage besides the permission
      probe start network or filesystem work that a detached context would let
      run during a stop? Current assumption: no (grep of Graph clients in
      engine files finds only `permission_probe_remote.go`). Validation step:
      the whole `internal/sync` race run. Owner if wrong:
      `engine_runtime_shutdown.go`, `sync-engine.md`.
    - Question: what the next run does with a success whose own commit failed.
      Current assumption: not checked, out of scope (executor's record).
      Validation: none here. Owner: `worker.go`, `sync-execution.md`.
    - Question: the residual two-wait case of question 5. Current assumption:
      accepted by the existing stop-limit design. Validation: stated in the
      report for the owner. Owner: `multisync/watch_runner_stop.go`.
