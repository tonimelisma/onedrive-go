# A4 — Definition of Ready

Increment: "Mount perf collectors receive context-recorded metrics"
Branch `fix/mount-perf-collector`, worktree `~/src/onedrive-go-wt/mount-perf-collector`, base `origin/main` = `70301c0e`.
Written 2026-11-11, before any code edit.

1. **Task intent** — PASS. One product-visible behavior: while a sync owner
   runs, `status --perf` and the perf capture show each mount's HTTP request,
   HTTP retry, DB transaction and transfer counts, and the owner's aggregate
   includes them. Today those counts are zero for every mount. Evidence:
   `internal/multisync/orchestrator_watch.go:415` makes the watch runner's
   context from the orchestrator context without the mount collector;
   `internal/multisync/orchestrator_run_once.go:240` runs the one-shot engine
   with the work context as given. The recorders read the collector from the
   context: `internal/sync/tx.go:38` (`perf.FromContext(ctx)` in
   `beginPerfTx`), `internal/perf/http.go` (`RoundTripper`),
   `internal/retry/transport.go:210,278`, `internal/driveops/transfer_upload.go:213,401`,
   `internal/driveops/transfer_download.go:311`.

2. **Non-goals** — PASS. No change to what `status --perf` prints, no new
   metric, no change to the fake Graph, no e2e test, no change to
   `internal/perf` (the collector parent chain already carries totals upward;
   see item 12), no change to which context the engine factory, engine close
   or orchestrator-driven parent-store work run under (findings go in the
   report).

3. **Governing docs** — PASS. Read: `spec/design/sync-control-plane.md`
   (`GOVERNS: internal/cli/sync.go, internal/multisync/*.go,
   internal/synccontrol/*.go`; "Runtime Ownership" perf bullet at lines
   550–553, "Verified By" row for the perf control socket at line 68, the
   `GET /v1/perf` description at line 468); `spec/design/system.md`
   (`GOVERNS` includes `internal/perf/*.go`; "Production Performance
   Instrumentation", lines 207–230); `spec/requirements/non-functional.md:81`
   (R-6.6.15, `[verified]`); the analysis
   `analysis/01b-product-and-e2e.md` lines 533–601 (no authority; checked
   against the code).

4. **Authority owner** — PASS. `internal/perf.Runtime` owns the counters and
   the per-mount registry (`RegisterMount`, `SnapshotByMount`,
   `AggregateSnapshot`). `internal/multisync.Orchestrator` owns mount
   registration and the lifetime of each mount's run context, so it is the
   owner that must put the mount collector in that context. The engine
   (`internal/sync`) and the transports only record into whatever collector
   the context carries. No durable state, no user-facing output format and no
   side effect changes.

5. **Code scope** — PASS. Production: `internal/multisync/orchestrator_watch.go`
   (one line in `startWatchRunner`), `internal/multisync/orchestrator_run_once.go`
   (one line in `buildEngineWork` plus the `internal/perf` import). Tests:
   `internal/multisync/perf_control_test.go`, one new test in `internal/sync`.
   Docs: `spec/design/sync-control-plane.md`, `spec/design/system.md`. Not to
   be touched: `internal/perf`, `internal/cli`, `e2e/`, `internal/testutil`
   fakes, `cmd/devtool`, the main checkout, any other worktree.

6. **Regression plan** — PASS. Written first and seen failing before the fix:
   `TestOrchestrator_MountPerfIncludesContextRecordedMetrics` (watch runner:
   work that records through the collector in its context shows up in that
   mount's snapshot, in the runtime aggregate and in the command collector,
   once each) and a one-shot variant for the run-once path, both
   `// Validates: R-6.6.15`, in `internal/multisync/perf_control_test.go`.
   Plus an engine-pass test in `internal/sync` asserting
   `Snapshot().DBTransactionCount > 0` after a real `RunOnce` that commits to
   the store under a context carrying the collector. That third test pins the
   engine half of the contract and is expected to pass before the fix, because
   the defect is in `internal/multisync`; the report will say so.

7. **Verification plan** — PASS. Focused: `go -C <worktree> test -race -count=1
   -run 'MountPerfIncludesContextRecordedMetrics' ./internal/multisync/`
   before and after the fix. Then `go -C <worktree> test -race -count=1
   ./internal/multisync/...` and `./internal/sync/...` (one at a time; this
   host has 2 cores), `go -C <worktree> build ./...`, `go -C <worktree> vet
   ./...`, and `go -C <worktree> vet -tags 'e2e e2e_full' ./e2e/...` (compiles
   only). `gofumpt` and `goimports` on changed Go files. `devtool verify` and
   every e2e tier are left to the reviewing agent and PR CI, per the subagent
   rules.

8. **Data safety** — PASS. The change adds a value to a context. It alters no
   planning, execution, store write, delete or overwrite path. Cancellation is
   unchanged: `perf.WithCollector` wraps with `context.WithValue`, so the
   wrapped context has the same `Done`, deadline and error as the one it
   wraps (`internal/perf/context.go`).

9. **Secret and credential safety** — PASS. No token file, `.env`, CI secret
   or live account is read or written. Tests use the package's existing
   `@example.com` placeholder identities. No account name, drive ID or
   credential goes into code, docs, the commit message or the reports.

10. **Live provider and destructive operations** — PASS. None. No live
    OneDrive call, no state reset, no release artifact, no history rewrite, no
    dependency change. Only unit tests with in-process fakes and temp
    directories run.

11. **Review carryover and nightly** — PASS — `go run ./cmd/devtool verify --dod --stage start` (2026-11-11): manifest ready, no actionable stale review threads; nightly run 42865435904 concluded failure; `TestE2E_SyncWatch_PauseResume` is held by branch `fix/upload-placeholder-retry` and skipped here.

12. **Unknowns and assumptions** — ASSUMPTION (three, each validated before the
    commit).

    - Question: does `perf.WithCollector` replace the context's collector, and
      if so do process-level totals lose what the mount now receives?
      Current assumption: it replaces, and nothing is lost or double counted,
      because the mount collector's parent is the runtime collector
      (`Runtime.RegisterMount` → `NewCollector(r.collector)`) whose parent is
      the command collector (`perf.NewRuntime(cfg.PerfParent)`), and
      `Collector.apply` records on itself and then on its parent.
      Validation step: both new multisync tests set `cfg.PerfParent` and
      assert the mount snapshot, `AggregateSnapshot()` and the parent
      collector each show the count exactly once.
      Owner document/code path affected if assumption is wrong:
      `internal/perf/collector.go`, `internal/perf/runtime.go`,
      `spec/design/system.md`.

    - Question: does any other place run mount work under a context that lacks
      the mount collector?
      Current assumption: the two run paths in the brief are the only ones
      that record context metrics for a registered mount. Engine construction
      and engine close take the orchestrator context but open no perf
      transaction and send no request; drive verification runs in the
      engine's startup stage inside `RunOnce`/`RunWatch`, under the mount
      context.
      Validation step: read `NewMountEngine`/`newEngine`, `Engine.Close`,
      `runStartupStage` and every `beginPerfTx` call site (done); list what
      stays on the command collector in the report.
      Owner document/code path affected if assumption is wrong:
      `internal/multisync/orchestrator_watch.go`,
      `internal/multisync/orchestrator_run_once.go`.

    - Question: does R-6.6.15's wording claim something that was not true
      before this fix?
      Current assumption: no. It requires live snapshots through
      `status --perf` and the socket and forbids fabricated history; it does
      not enumerate per-mount counters. The text and its `[verified]` status
      stay; the design docs gain the sentence that was missing.
      Validation step: re-read `spec/requirements/non-functional.md:81`
      against the final diff.
      Owner document/code path affected if assumption is wrong:
      `spec/requirements/non-functional.md`.
