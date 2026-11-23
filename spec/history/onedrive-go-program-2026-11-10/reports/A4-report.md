# A4 report: a mount's requests, transactions and transfers in its perf snapshot

PR #862, merged commit recorded in PLAN.md. Finished by the reviewing session
after the first subagent died without a commit.

## What changed

- internal/multisync/orchestrator_watch.go, orchestrator_run_once.go: the
  mount's run context carries the mount's collector.
- internal/multisync/perf_control_test.go: two tests (watch and one-shot),
  each asserting exactly one request, transaction and upload on the mount
  snapshot, the owner aggregate and the command collector.
- internal/sync/engine_perf_context_test.go, dispatch_order_test.go: the
  engine-half test, listed in runOnceScenarios.
- spec/design/sync-control-plane.md, system.md.

## Verification

- Both multisync tests seen failing without the production change.
- Race tests: internal/multisync, internal/perf whole; internal/sync targeted
  (the new test, its reverse-dispatch subtest, the table test).
- verify lint and verify quick pass; PR CI green.
- Live: before, 13 h of syncing showed 0 requests, 0 transactions; after, 84 s
  and one upload showed 96 requests, 30 transactions, 1 upload.

## Findings the brief asked for

**Does `perf.WithCollector` replace or chain?** It replaces
(`context.WithValue`). Totals stay right without any `internal/perf` change:
the mount collector's parent is the runtime (owner aggregate) collector
(`Runtime.RegisterMount` → `NewCollector(r.collector)`), whose parent is
`OrchestratorConfig.PerfParent`, and `Collector.apply` records on itself and
then on its parent. Both new multisync tests assert the count is exactly 1 at
all three levels. One precondition: `PerfParent` must be the collector the run
context carries. Both production construction sites
(`internal/cli/sync_runtime.go:223`, `internal/cli/sync_flow.go:199`) set it
from the same context they then run under. An owner built with a nil
`PerfParent` under a context that does carry a collector would now send these
four families to the mount and aggregate only; no production path does that.

**Other places that run mount work without the mount collector.** The two
paths in the brief are the only ones that create a mount collector
(`registerMountPerfCollector` has exactly those two callers), and child mounts
go through the same two functions. What still runs under the orchestrator's
context, and so counts on the command collector only:

- (a) The engine factory (`orchestrator_watch.go:403`, `orchestrator_run_once.go:218`).
  Construction sends no Graph request and opens no perf transaction today.
- (b) `engine.Close(ctx)` (`closeEngine`): stale upload-session cleanup and
  closing the state store; nothing is recorded through the context.
- (c) Parent-store steps the orchestrator drives itself: final-drain
  acknowledgement (`acknowledgeSuccessfulFinalDrains`), child artifact purge
  and cleanup, `finalizeSuccessfulFinalDrainMounts`.
- (d) Session creation (`o.cfg.Runtime.SyncSession(ctx, ...)`), which happens
  before the mount's collector is registered.

None of these was changed. The design doc now lists them.

## Deviations from the analysis and the brief

- Analysis, "Open detail": it says drive verification requests are made by
  `NewMountEngine` in the factory and so stay on the command collector. In the
  code, verification runs in the engine's startup stage
  (`runStartupStage` → `proveDriveIdentity`) inside `RunOnce`/`RunWatch`, under
  the mount's run context, so after this fix it counts for the mount. The
  factory context was therefore left unwrapped.
- Analysis, test sketch: it suggests the fake engine call
  `RecordHTTPRequest(...)` directly. The tests send the request through the
  production `perf.RoundTripper` instead, so the HTTP recorder itself is under
  test; the transaction and the upload are recorded directly on the context's
  collector, which is the call `tx.go` and `driveops` make.
- Brief, "engine-pass test (where the harness allows it)": the multisync
  harness always replaces the engine factory, and a real engine there would
  need a real `driveops.Session`. The engine-pass test is in `internal/sync`
  and passes before the fix (see test 3).
- Brief, test 1 wording asks for the mount's snapshot. The tests also assert
  the owner aggregate and the command collector, each exactly once, because
  that is what answers the replace-or-chain question.
- The brief's line numbers (415 and 240) matched the code.

## Recommended, not done

- Defense in depth: `Engine.RunOnce`/`RunWatch` could wrap their own context
  with `e.perfCollector` when one is set, so an owner that forgets the wrap is
  still counted correctly. Left out: it changes `internal/sync`, and it is not
  needed for this defect. The new engine test deliberately does not pin the
  opposite (a context without a collector records nothing), so this stays
  open.
- If engine construction or close ever starts recording through its context,
  pass those calls the mount-collector context too. Today it would change
  nothing.
- An e2e or CLI-level assertion that `status --perf` shows a non-zero HTTP
  request count for a syncing mount would close the gap the analysis names
  ("no test runs an engine and then reads the mount snapshot") end to end.
  e2e was out of scope here.
- `status --perf` reads only the per-mount snapshots; the aggregate that
  `GET /v1/perf` returns is not shown. Whether to show it is a product
  decision, out of scope.

## Unfinished

Nothing in scope. `devtool verify` and the PR are the reviewing agent's.
