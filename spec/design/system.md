# System Architecture

Implements: R-6.2.1 [verified], R-6.2.2 [verified], R-6.3.1 [verified], R-6.10.5 [verified], R-6.10.7 [verified], R-6.10.8 [verified], R-6.10.9 [verified], R-6.10.11 [verified], R-6.10.12 [verified], R-6.10.13 [verified]

## Package Structure

Implements: R-6.1.4 [verified], R-6.1.5 [verified], R-6.9.1 [verified]

```
.                             Root package (thin process entrypoint)
cmd/
  devtool/                    Repo-owned verification and worktree bootstrap tooling
internal/
  cli/                        Cobra command tree, CLI bootstrap, output formatting, command handlers
  config/                     TOML config, drive sections, XDG paths, override chain
  driveid/                    Type-safe drive identity: ID, CanonicalID, ItemKey (leaf, stdlib-only)
  driveops/                   Authenticated drive access: sessions, transfers, hashing
  failures/                   Shared runtime/domain failure classes (leaf, stdlib-only)
  fsroot/                     Root-bound managed-state file capabilities
  graph/                      Graph API client: auth, Graph normalization, items CRUD, delta, transfers
  graphhttp/                  Graph-facing HTTP client profiles and shared throttle coordination
  localpath/                  Explicit arbitrary-local-path boundary helpers
  logfile/                    Log file creation, rotation, retention
  multisync/                  Multi-drive sync control plane and watch reload
  perf/                       Command-scoped and live sync performance instrumentation plus capture bundles
  retry/                      Retry policies, exponential backoff with jitter, retry transport
  sync/                       Single-drive sync engine (see pipeline below)
  syncstore/                  Durable SQLite sync state and read-only inspection
  synctree/                   Root-bound sync runtime filesystem capability
  tokenfile/                  Pure OAuth token file I/O (leaf, stdlib + oauth2 only)
pkg/
  quickxorhash/               QuickXorHash algorithm (vendored from rclone, BSD-0)
e2e/                          E2E test suite (not production code)
testutil/                     Shared test helpers (not production code)
```

## Dependency Rules

```
root pkg → internal/cli/ → internal/graphhttp/ → internal/retry/
                         → internal/driveops/  → internal/graph/ → pkg/*
                         → internal/failures/
                         → internal/perf/
                         → internal/multisync/ → internal/perf/
                                              → internal/sync/ → internal/driveops/
                                                               → internal/syncstore/
                         → internal/config/  → internal/driveid/
```

- No cycles. `driveops` does NOT import `sync`. `graph` does NOT import `config`.
- `multisync` owns multi-drive lifecycle; `sync` owns the single-drive runtime.
- `driveid`, `failures`, `tokenfile`, and `retry` are leaf packages (no internal imports).
- Both `graph` and `config` import `tokenfile` for token file I/O.
- Callers pass token paths to `graph` — no config coupling.
- `graphhttp` owns Graph-facing HTTP transport/client policy. `driveops` stays transport-policy agnostic and consumes injected clients through a resolver.

## Event-Driven Sync Pipeline

The sync engine uses an event-driven pipeline. One-shot sync is "collect all events, then process as one batch." Watch mode is the same pipeline triggered incrementally.

```
Remote Observer ──┐
                  ├──→ Change Buffer ──→ Planner ──→ Executor ──→ Baseline
Local Observer  ──┘         │                │            │           │
                      debounce/dedup    pure function   workers    per-action
                                        (no I/O)      (parallel)   commit
```

### Design Principles

1. **No database coordination.** Observers produce ephemeral events. The planner reads baseline (cached in memory). The executor writes outcomes per-action. The database is never the coordination mechanism between stages.
2. **Remote state separation.** Three-table model: `remote_state` (observed), `baseline` (confirmed synced), `sync_failures` (issues). Remote observations persist at observation time, decoupling the delta token from sync success. See [data-model.md](data-model.md).
3. **Pure-function planning.** The planner has no I/O. `Plan(changes, baseline, mode, safety) → ActionPlan`. Every decision is deterministic and reproducible.
4. **Watch-primary.** `sync --watch` is the primary runtime mode. All components serve both one-shot and watch modes.
5. **Interface-driven testability.** All I/O (filesystem, network, database) is behind interfaces.
6. **Boundary-owned error translation.** Each boundary normalizes the failures it understands once, and downstream layers consume that shared contract. See [error-model.md](error-model.md).

### Rationale

Event-driven processing was chosen over four alternatives (shared mutable DB, multi-table split, deferred persistence, pure snapshot pipeline) because it eliminates database coordination anti-patterns: tombstone split, synthetic ID lifecycle, SQLITE_BUSY during execution, incomplete lifecycle predicates, pipeline phase ordering, and asymmetric filter application. The event-driven model generalizes correctly — watch mode is the natural case, one-shot is the degenerate batch case.

## Module Design Docs

For detailed module design, see:

| Module | Design Doc |
|--------|-----------|
| Graph API client and HTTP runtime | [graph-client.md](graph-client.md) |
| Configuration | [config.md](config.md) |
| Drive identity | [drive-identity.md](drive-identity.md) |
| Drive transfers | [drive-transfers.md](drive-transfers.md) |
| Retry infrastructure | [retry.md](retry.md) |
| Sync observation | [sync-observation.md](sync-observation.md) |
| Sync planning | [sync-planning.md](sync-planning.md) |
| Sync execution | [sync-execution.md](sync-execution.md) |
| Sync engine | [sync-engine.md](sync-engine.md) |
| Sync control plane | [sync-control-plane.md](sync-control-plane.md) |
| Sync store | [sync-store.md](sync-store.md) |
| Data model (schema) | [data-model.md](data-model.md) |
| Error model | [error-model.md](error-model.md) |
| Threat model | [threat-model.md](threat-model.md) |
| Degraded mode | [degraded-mode.md](degraded-mode.md) |
| Performance benchmarking | [performance-benchmarking.md](performance-benchmarking.md) |
| CLI commands | [cli.md](cli.md) |

## Source Of Truth Map

| Concern | Owner |
|---------|-------|
| Runtime failure class | `internal/failures` + `internal/sync/engine_result_classify.go` |
| Sync-domain summary/rendering key | `internal/synctypes/summary_keys.go` |
| Durable sync issue facts | `internal/syncstore` SQLite tables (`sync_failures`, `scope_blocks`, conflicts) |
| Account/auth presentation | `internal/authstate` vocabulary projected through `internal/cli/account_read_model_service.go` |
| Read-only issue/status read model | `internal/syncstore/inspector.go` |
| Production perf counters, live snapshots, and capture bundles | `internal/perf` with session/control-plane ownership split across `internal/cli` and `internal/multisync` |
| Durable mutation rules | `internal/syncstore` writable store APIs (`CommitMutation`, scope/failure/conflict helpers) plus engine-owned scope/result flow |

## Production Performance Instrumentation

`internal/perf` is the repo-owned measurement boundary for production-visible
performance data.

- `internal/perf` owns command/session collectors, aggregate counter rollups,
  live per-drive snapshots, and opt-in capture bundle creation.
- `internal/cli` owns command-session lifetime and human-visible rendering:
  final/periodic log summaries, `status --perf`, and the `perf capture`
  command.
- `internal/multisync` owns live sync-owner registration and the control-socket
  surfaces that expose live snapshots and trigger captures.
- Logs are the durable history. `status --perf` and the control socket are
  live-owner views only and intentionally do not invent a new persistent perf
  database.
- Always-on production logs carry aggregate counters and timings only. Rich
  per-drive detail is reserved for explicit capture bundles.

## Verification Policy

Static verification is a first-class architectural constraint, not a best-effort hygiene step.

- `golangci-lint` runs with `default: none`; every enabled linter is an explicit policy choice.
- `depguard` enforces architectural import boundaries, and `forbidigo` bans raw filesystem `os.*` calls outside the explicit boundary packages (`fsroot`, `synctree`, `localpath`).
- `nolintlint` requires both a specific linter name and a short justification. Unused exclusions are surfaced with `linters.exclusions.warn-unused`.
- Inline suppressions are reserved for the small documented exception set where the code is correct and the linter cannot express that shape cleanly: interface-mandated receiver shapes, validated subprocess/request dispatch that the linter cannot follow across helper boundaries, non-cryptographic jitter, `driver.Valuer` SQL `NULL` semantics, fixed placeholder SQL, and intentional test fixtures/mocks.
- `cmd/devtool` is the single repo-owned verification and worktree-bootstrap entry point. `go run ./cmd/devtool verify` exposes explicit profiles: `default` (default local run: format, lint, build, race+coverage, coverage gate, repo-consistency checks, fast E2E), `public`, `e2e`, `e2e-full`, `integration`, and `stress`. The stress profile is intentionally targeted, not "repeat every test in every package": it runs the watch-order probes in `internal/sync` via the `stress` build tag and `-run TestWatchOrderingStress_`, then repeats `./internal/multisync ./internal/sync` under `-race -count=50 -timeout=20m`. The explicit stress timeout is part of the verifier contract so the repeated package loop is limited by repo-owned policy instead of the Go tool's shorter default timeout. The verifier also exposes `--classify-live-quirks` for scheduled/manual live verification: it reruns a tiny documented registry of known provider-consistency failures once and only downgrades them when the rerun passes. `--summary-json <path>` writes a machine-readable end-of-run summary with per-phase durations, per-bucket full-suite results, quirk-event count, and any classified reruns.
- `go run ./cmd/devtool cleanup-audit` is the read-only git-state cleanup classifier. It fetches/prunes `origin`, then classifies local worktrees, local branches, and remote branches as `safe_remove`, `keep_attached`, `keep_dirty`, `keep_unmerged`, or `keep_main` without deleting anything.
- `go run ./cmd/devtool watch-capture --scenario <name> [--json] [--repeat N]
  [--settle D]` captures raw per-OS fsnotify event sequences for scoped-watch
  scenarios into a stable JSON shape. The command is diagnostic tooling only;
  product CLI stays unchanged. Checked-in captures under
  `internal/sync/testdata/watch_capture/<goos>/<scenario>/<variant>.json`
  back the replay tests in `internal/sync`. Those fixtures now cover
  both `darwin` and `linux`, with Linux replay exercised in the default
  Ubuntu CI lane.
- `go run ./cmd/devtool bench --scenario <name> [--subject <id>] [--runs N]
  [--warmup N] [--json] [--result-json <path>]` is the repo-owned benchmark
  runner entrypoint. The first delivered slice measures the
  `startup-empty-config` controlled scenario against the built `onedrive-go`
  binary, emits a subject-aware JSON result bundle, and keeps broader
  representative sync scenarios and release report publication in the dedicated
  benchmarking design.
- Observation coordination is intentionally split into a pure planner and
  effectful executors. `internal/sync/engine_scope_session.go` is the pure
  scoped-observation planner, while `internal/sync/engine_observation_phase.go`
  and `internal/sync/engine_observation_watch.go` own effectful execution for
  one-shot/reconcile and watch mode respectively. That keeps driver/fallback/
  token policy out of orchestration code.
- Planner policy also has a repo-owned golden surface. Stable JSON projections
  of `ObservationSessionPlan` live under
  `internal/sync/testdata/observation_session_plan/` and are updated with the
  standard local `-update` golden workflow in same-package tests.
- Repo-consistency checks in `cmd/devtool verify` enforce the repo-level architecture constraints linters do not express cleanly: governed design docs must carry ownership contracts, required cross-cutting docs must exist and be linked from this document, production `internal/cli` code must not bypass its output-writer boundary with direct process-global stdout/stderr writes, guarded runtime packages must not reintroduce raw `os.*` filesystem calls, known stale wording classes for filter ownership are rejected in live docs, recurring `spec/reference/live-incidents.md` `Promoted docs:` links must resolve to real files/anchors, `internal/graph/client_preauth.go` remains the only raw production `http.Client.Do` boundary, `exec.CommandContext` is limited to validated browser launch and devtool runner entrypoints, `sql.Open` is limited to `internal/syncstore`, `signal.Notify` is limited to `internal/cli/signal.go`, and the internal dependency graph stays inside explicit guardrails: at most 27 internal packages, at most 80 internal import edges, no direct `cli -> synctypes`, `multisync -> synctypes`, or `syncstore -> sync` edge, and `synctypes` may import only `driveid` among internal packages.
- The same repo-consistency pass also forbids raw `exec.Command` in production, narrows `signal.Stop` and `os.Exit` to the documented process-lifecycle entrypoints, validates that `Validates:` and `Implements:` references resolve to declared requirement IDs, enforces that governed lifecycle docs cite exact named tests in their evidence sections, and enforces the degraded-mode evidence-table structure that links each `DM-*` row to named tests.
- `go run ./cmd/devtool worktree add --path <path> --branch <branch>` is the canonical way to create new worktrees from `origin/main`. It applies `.worktreeinclude` immediately so the new worktree is ready for fast E2E and local development.
- `cmd/devtool` is exercised both through package-level command assembly tests and black-box process tests for `verify` profile parsing plus `worktree bootstrap/add` semantics, including explicit `--source-root` selection. The documented developer entrypoint and the shipped binary must stay aligned.
- Fast E2E is mandatory in the default local `default` profile. The harness loads typed live-test config from exported env plus root `.env` and durable `.testdata/fixtures.env` defaults; verification does not silently skip fast E2E based on exported shell variables.
- `verify e2e` runs a repo-owned fast fixture preflight before the main fast suite so shared-link drift fails immediately with a targeted message instead of surfacing later as an expensive suite failure.
- Shared-file fixture resolution is identity-based, not recipient-slot-based. If
  more than one configured test account can open the same raw share link, the
  harness accepts that fixture as long as every matching configured recipient
  resolves to the same remote drive/item identity and then picks a stable
  recipient candidate deterministically.
- The nightly/manual full E2E suite is layered on top of the fast suite. Its files use `//go:build e2e && e2e_full`, so the canonical invocation is the verifier's `e2e-full` profile, which sets both tags, preserves the fast-then-full ordering, and runs the full shared-folder fixture preflight before the `e2e_full` buckets.
- `e2e_full` now runs as three explicit verifier-owned buckets instead of one monolithic package invocation: `full-parallel-misc` (`-parallel 5` for tests already marked safe), `full-serial-sync`, and `full-serial-watch-shared`. The verifier executes those buckets sequentially after the single full-suite preflight so only the vetted misc bucket regains concurrency.
- The one-time full-suite preflight still owns the global live-drive artifact scrub. Bucketed `go test` runs set `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1` so they reuse that cleaned fixture state instead of re-scrubbing both configured drives before every bucket process.
- When `verify e2e` or `verify e2e-full` runs without an explicit `--e2e-log-dir`, the verifier resolves the repo-owned default temp log dir once up front, scrubs stale timing and quirk artifacts there, and passes that same `E2E_LOG_DIR` through the fast suite, the full-suite preflight, and every full bucket so live diagnostics stay in one place.
- The verifier gives every full bucket the same `60m` timeout budget; the older `30m` cap was no longer compatible once the suite intentionally stopped overlapping mutating tests.
- Websocket-specific E2E timing starts only after `observer_started(remote)`, and the timed mutation must target a subtree that bootstrap already materialized locally. Waiting merely for the socket connection is not enough on initial startup or daemon restart, and mixing the timed wake assertion with fresh parent-folder creation reintroduces the same delta-lag race the websocket harness is supposed to isolate from.
- `--classify-live-quirks` is intentionally narrow. Today it only covers `LI-20260405-04` (fast `TestE2E_Sync_DownloadOnly`) and `LI-20260405-06` (strict auth preflight `/me/drives` propagation lag). Unknown failures, or repeated failures after the one rerun, stay red.
- PR `e2e` and scheduled/manual `e2e-full` runs emit repo-owned verifier artifacts: the `--summary-json` report, the shared E2E `timing-summary.json` file, and the shared E2E `quirk-summary.json` file. The timing summary aggregates remote-write visibility, delete-disappearance, scope-transition, and sync-convergence wait windows across the fast suite, the full preflight, and all full buckets. The quirk summary records repo-owned live-provider evidence observed by the harness itself: CLI-emitted `graph.QuirkEvidence`, strict auth-preflight retry decisions, and fixture-helper recurrence classifications emitted by the helper that actually observed the recurrence.
- CI uses `--classify-live-quirks` only for scheduled/manual `e2e-full` runs. Required per-PR verification remains strict.
- CI keeps the expensive runtime stress lane out of the required per-PR path, but exposes it explicitly on a scheduled cadence and via manual dispatch so race-heavy watch/runtime regressions remain visible.
- Managed repo-state files use `internal/fsroot` root capabilities.
- Sync-runtime filesystem operations under one configured sync root use `internal/synctree`.
- Arbitrary local file paths outside those rooted domains use `internal/localpath` as the explicit trust boundary. `localpath.AtomicWrite` is the default arbitrary-path writer when replace-in-place semantics matter.
- Sync-state integrity inspection and deterministic safe repair live behind `internal/syncstore` and surface operationally through `go run ./cmd/devtool state-audit`.
- Append-only log file creation in `internal/logfile` is the single documented exception to atomic replace-style writes. It is allowed because the log is an append-only operational artifact, not an authoritative config/state file.
- `fsroot.Root` and `synctree.Root` carry unexported injectable ops so managed-state and sync-runtime I/O failure paths can be tested deterministically without package-level test hooks.

## Planned Improvements

- Resource consumption guarantees documented per component. [planned]
- Representative benchmark scenarios, reporting, and publication policy are defined in [performance-benchmarking.md](performance-benchmarking.md). The repo-owned `devtool bench` runner and `startup-empty-config` harness-validation scenario are implemented; broader representative sync scenarios, release artifact promotion, and published benchmark reports remain planned. [planned]
- Production ignored-return audit complete: invariant-bearing results such as `DepGraph.Complete` and dispatch admission booleans are handled explicitly, baseline/account metadata lookups now use explicit helpers or `found` checks, and the remaining ignored returns are limited to explicit value-only helper drops plus documented impossible-error builder writes. [verified]
- Bare `assert.NoError` audit complete: remaining sites are intentional cleanup, test-plumbing, or pure error-contract assertions, while weak CLI happy-path tests now assert rendered text, JSON shape, and preserved config/state. [verified]
- Write-path `Close` audit complete: sync-store close failures now propagate on internal baseline verification helpers and `resolve deletes`, while read-only inspector close paths remain best-effort debug logged by design. [verified]
- Direct handler/service coverage over the `internal/cli/` service split is above the current target: `go test ./internal/cli/... -cover` reports 67.8%. [verified]
