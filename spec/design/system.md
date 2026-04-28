# System Architecture

Implements: R-6.2.1 [verified], R-6.2.2 [verified], R-6.3.1 [verified], R-6.10.5 [verified], R-6.10.7 [verified], R-6.10.8 [verified], R-6.10.9 [verified], R-6.10.11 [verified], R-6.10.12 [verified], R-6.10.13 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| Package boundaries and privileged-call discipline remain documented in the owning design docs and exercised by package-level behavior tests. | `cmd/devtool/main_test.go`, `spec/design/sync-control-plane.md`, `spec/design/sync-engine.md`, `spec/design/graph-client.md`, `spec/design/drive-transfers.md` |
| The product continues to split multi-mount control, single mounted content-root sync, Graph I/O, and rooted filesystem access into explicit authority boundaries. | `spec/design/sync-control-plane.md`, `spec/design/sync-engine.md`, `spec/design/graph-client.md`, `spec/design/drive-transfers.md` |

## Package Structure

Implements: R-6.1.4 [verified], R-6.1.5 [verified], R-6.9.1 [verified]

```
.                             Root package (thin process entrypoint)
cmd/
  devtool/                    Repo-owned developer-tooling entrypoint
internal/
  authstate/                  Shared auth-health vocabulary and user-facing auth copy
  cli/                        Cobra command tree, CLI bootstrap, output formatting, command handlers

  config/                     TOML config, drive sections, XDG paths, override chain
  devtool/                    Verifier, benchmark, worktree, and cleanup-audit helpers
  driveid/                    Type-safe drive identity: ID, CanonicalID, ItemKey (leaf, stdlib-only)
  driveops/                   Authenticated drive access: sessions, transfers, hashing
  errclass/                   Shared runtime failure class enum (leaf, stdlib-only)
  fsroot/                     Root-bound managed-state file capabilities
  graph/                      Graph API client: auth, Graph normalization, items CRUD, delta, transfers
  graphtransport/             Graph-facing HTTP transport profile construction
  localpath/                  Explicit arbitrary-local-path boundary helpers
  logfile/                    Log file creation, rotation, retention
  multisync/                  Multi-mount sync control plane and watch reload
  perf/                       Command-scoped and live sync performance instrumentation plus capture bundles
  retry/                      Retry policies, exponential backoff with jitter, retry transport
  sharedref/                  Shared item selector parsing and formatting
  sync/                       Single mounted content-root sync engine, durable SQLite sync state, and read-only inspection
  synccontrol/                JSON-over-HTTP Unix-socket protocol shared by CLI and multisync owner
  synctest/                   Shared sync-package test helpers (test-only)
  synctree/                   Root-bound sync runtime filesystem capability
  syncverify/                 Baseline-versus-local sync verification helpers
  tokenfile/                  Pure OAuth token file I/O (leaf, stdlib + oauth2 only)
pkg/
  quickxorhash/               QuickXorHash algorithm (vendored from rclone, BSD-0)
e2e/                          E2E test suite (not production code)
scripts/                      Developer and CI-supporting helper scripts
testutil/                     Shared test helpers (not production code)
spec/                         Requirements, design docs, and reference docs
```

## Dependency Rules

```
root pkg → internal/cli/ → internal/graphtransport/ → internal/retry/
                         → internal/driveops/  → internal/graph/ → pkg/*
                         → internal/errclass/
                         → internal/perf/
                         → internal/multisync/ → internal/perf/
                                              → internal/sync/ → internal/driveops/
                         → internal/config/  → internal/driveid/
```

- No cycles. `driveops` does NOT import `sync`. `graph` does NOT import `config`.
- `multisync` owns multi-mount lifecycle; `sync` owns the single mounted
  content-root runtime.
- `driveid`, `errclass`, `tokenfile`, and `retry` are leaf packages (no internal imports).
- Both `graph` and `config` import `tokenfile` for token file I/O.
- Callers pass token paths to `graph` — no config coupling.
- `graphtransport` owns Graph-facing HTTP transport profile construction. `driveops` owns the session runtime that reuses those profiles and chooses between bootstrap, interactive, and sync paths.

## SQLite-Backed Sync Pipeline

The sync engine is now snapshot-first:

- observation produces dirty signals and current observation facts
- SQLite persists current local/remote snapshots, baseline, observation
  problems, retry work, block scopes, and refresh cadence
- SQLite computes structural diff and reconciliation outcomes
- Go builds the current actionable set, executes it, and publishes baseline

```
Remote Observer ──┐
                  ├──→ Dirty Buffer ──→ refresh snapshots ──────────────┐
Local Observer  ──┘         │                                           │
                      debounce/wake                                     │

Remote Obs ───────────────→ remote_state ──┐                            │
Local Scan/Obs ───────────→ local_state  ──┼──→ comparison_state ──→ reconciliation_state
Baseline ─────────────────→ baseline     ──┘                            │
                                                                         │
observation_issues / retry_work / block_scopes / observation_state ─────┤
                                                                         ↓
                                                          Go actionable-set planner
                                                                         ↓
                                                            final scope admission
                                                                         ↓
                                                               Executor / workers
                                                                         ↓
                                                                  baseline publish
```

### Design Principles

1. **Durable truth lives in SQLite.** `local_state`, `remote_state`,
   `baseline`, `observation_issues`, `retry_work`, `block_scopes`, and
   `observation_state` are the durable authority surfaces. Observation and
   execution rebuild working state from those tables rather than inventing a
   second durable coordinator.
2. **Dirty buffering is runtime scheduling only.** The runtime uses a buffer to
   coalesce dirty signals and wake replans, not to define sync truth.
3. **SQLite owns structural diff and reconciliation.**
   `comparison_state` and `reconciliation_state` compute the latest
   snapshot-vs-baseline truth from SQLite, while Go owns actionable-set
   construction, durable retry/blocker reconciliation, dependency ordering, and
   final admission on the ready frontier.
4. **Watch-primary.** `sync --watch` is the primary runtime mode. All components serve both one-shot and watch modes.
5. **Interface-driven testability.** All I/O (filesystem, network, database) is behind interfaces.
6. **Boundary-owned error translation.** Each boundary normalizes the failures it understands once, and downstream layers consume that shared contract. See [error-model.md](error-model.md).

### Rationale

The current design deliberately keeps durable truth and executable planning
separate. Current and synced truth are explicit in SQLite, while the
worker-dispatching actionable set remains runtime-owned in Go. This preserves
the tested executor and dependency-graph runtime while snapshot persistence,
retry/state ownership, and
latest-plan materialization move into durable SQLite surfaces.

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

## Cross-Cutting Reference Docs

| Topic | Reference |
|--------|-----------|
| New developer onboarding | [../reference/developer-onboarding.md](../reference/developer-onboarding.md) |
| Graph API quirks | [../reference/graph-api-quirks.md](../reference/graph-api-quirks.md) |
| OneDrive sync behavior | [../reference/onedrive-sync-behavior.md](../reference/onedrive-sync-behavior.md) |
| OneDrive glossary | [../reference/onedrive-glossary.md](../reference/onedrive-glossary.md) |
| Live incidents ledger | [../reference/live-incidents.md](../reference/live-incidents.md) |
| Live recurring families | [../reference/live-recurring-families.md](../reference/live-recurring-families.md) |
| Sync ecosystem patterns | [../reference/sync-ecosystem.md](../reference/sync-ecosystem.md) |

For live CI / E2E / integration debugging, open
[live-incidents.md](../reference/live-incidents.md) first for chronology and
evidence, then use
[live-recurring-families.md](../reference/live-recurring-families.md) as the
compact current-policy companion for recurring families.

## Source Of Truth Map

| Concern | Owner |
|---------|-------|
| Runtime failure class | `internal/errclass` + `internal/sync/engine_result_classify.go` |
| Sync-domain condition key and stored-condition grouping | `internal/sync/condition_keys.go` + `internal/sync/condition_projection.go` |
| Status title/reason/action rendering | `internal/cli/status_condition_descriptors.go` |
| Durable sync status facts | `internal/sync` SQLite tables (`observation_issues`, `retry_work`, `block_scopes`) plus catalog-owned account auth state |
| Account/auth presentation | `internal/authstate` vocabulary projected through `internal/cli/account_view_snapshot.go` |
| Read-only status snapshot | `internal/sync/store_inspect.go` |
| Production perf counters, live snapshots, and capture bundles | `internal/perf` with session/control-plane ownership split across `internal/cli` and `internal/multisync` |
| Durable mutation rules | `internal/sync` writable store APIs (`CommitMutation`, scope/failure helpers) plus engine-owned scope/result flow |

## Production Performance Instrumentation

`internal/perf` is the repo-owned measurement boundary for production-visible
performance data.

- `internal/perf` owns command/session collectors, aggregate counter rollups,
  live per-drive snapshots, and opt-in capture bundle creation.
- `internal/perf.Runtime` is the single owner of drive registration and
  capture admission, so perf capture does not route through a second
  extra runtime object just to gate one in-flight bundle.
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
- `depguard` enforces architectural import boundaries, and `forbidigo` bans raw filesystem `os.*` calls in production/runtime packages outside the explicit boundary packages (`fsroot`, `synctree`, `localpath`). Repo tooling under `internal/devtool` is allowed to use stdlib filesystem calls directly because it does not participate in product runtime authority.
- Test-time synchronization is linted as well: direct `time.Sleep` is banned in test code, so unit and integration tests use explicit readiness handshakes or injected clocks/timers instead of wall-clock waits. The narrow exception set is live E2E helpers that wait on external process or provider propagation and carry an explicit `nolint` justification.
- `nolintlint` requires both a specific linter name and a short justification. Unused exclusions are surfaced with `linters.exclusions.warn-unused`.
- Inline suppressions are reserved for the small documented exception set where the code is correct and the linter cannot express that shape cleanly: interface-mandated receiver shapes, validated subprocess/request dispatch that the linter cannot follow across helper boundaries, non-cryptographic jitter, `driver.Valuer` SQL `NULL` semantics, fixed placeholder SQL, and intentional test fixtures/mocks.
- `cmd/devtool` is the single repo-owned verification and worktree-bootstrap entry point. `go run ./cmd/devtool verify` exposes explicit profiles: `public` (format, lint, build, race+coverage, coverage gate, and a compile-only `e2e_full` package guard), `default` (the `public` profile plus fast E2E), `e2e`, `e2e-full`, `integration`, and `stress`. The stress profile is intentionally targeted, not "repeat every test in every package": it runs the watch-order probes in `internal/sync` via the `stress` build tag and `-run TestWatchOrderingStress_`, repeats `./internal/multisync` under `-race -count=50 -timeout=20m`, and repeats `./internal/sync` under `-race -count=50 -timeout=30m`. The long `internal/sync` stress step executes via `go test -json`, and the verifier extracts the slowest completed tests into the same repo-owned summary surface instead of relying on raw timeout dumps alone. The verifier also exposes `--classify-live-quirks` for scheduled/manual live verification: it reruns a tiny documented registry of known provider-consistency failures once and only downgrades them when the rerun passes. `--summary-json <path>` writes a machine-readable end-of-run summary with per-phase durations, per-bucket full-suite results, quirk-event count, any classified reruns, and any recorded stress slow-test summaries.
- The compile-only `e2e_full` guard uses a per-run temporary output path rather
  than a fixed shared filename in `os.TempDir()`, so parallel verifier runs do
  not serialize or fail on cross-process artifact collisions.
- The verifier implementation is split by concrete ownership inside `internal/devtool`: `verify.go` owns profile orchestration and shared runners, `verify_summary.go` owns end-of-run summaries, `verify_e2e.go` owns fast/full live-suite orchestration, and `verify_stress.go` owns stress profiles. This split is structural only; `go run ./cmd/devtool verify ...` remains the single behavior contract and developer entrypoint.
- Verifier tests mirror that ownership split instead of one monolithic test file: `verify_runner_test.go` covers profile orchestration and summaries, and `verify_test_helpers_test.go` owns the shared fake runner.
- `go run ./cmd/devtool cleanup-audit` is the read-only git-state cleanup classifier. It fetches/prunes `origin`, then classifies local worktrees, local branches, and remote branches as `safe_remove`, `keep_attached`, `keep_dirty`, `keep_unmerged`, or `keep_main` without deleting anything.
- `go run ./cmd/devtool bench --scenario <name> [--subject <id>] [--runs N]
  [--warmup N] [--json] [--result-json <path>]` is the repo-owned benchmark
  runner entrypoint. The delivered slices measure the
  `startup-empty-config` controlled scenario and the manual
  `sync-partial-local-catchup-100m` live representative scenario against the
  built `onedrive-go` binary, emit a subject-aware JSON result bundle, and keep
  release report publication in the dedicated benchmarking design.
- Observation coordination is intentionally single-root. `internal/sync/engine_primary_root.go`
  owns the primary remote-root plan for the configured drive, while
  `internal/sync/engine_run_once.go`, `internal/sync/engine_watch.go`,
  `internal/sync/engine_watch_observation.go`, and
  `internal/sync/engine_watch_maintenance.go` execute that one root in
  one-shot, watch, and full-reconcile paths. Shared-root fallback policy lives
  alongside that execution rather than behind a separate session/phase
  framework, so the runtime reflects the real product model directly.
- `go run ./cmd/devtool worktree add --path <path> --branch <branch>` is the canonical way to create new worktrees from `origin/main`. It applies `.worktreeinclude` immediately so the new worktree is ready for fast E2E and local development.
- `cmd/devtool` is exercised both through package-level command assembly tests and black-box process tests for `verify` profile parsing plus `worktree bootstrap/add` semantics, including explicit `--source-root` selection. The documented developer entrypoint and the shipped binary must stay aligned.
- Fast E2E is mandatory in the default local `default` profile. The harness loads typed live-test config from exported env plus root `.env` and durable `.testdata/fixtures.env` defaults; verification does not silently skip fast E2E based on exported shell variables.
- `verify e2e` is intentionally a minimal live smoke lane: verifier-owned auth preflight, verifier-owned fast fixture preflight, `TestE2E_FileOpsSmokeCRUD`, `TestE2E_Sync_UploadOnly`, `TestE2E_Sync_DownloadOnly`, `TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount`, and `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression`, plus any non-live helper tests that happen to share the package.
- Repo verification regression-tests that contract via tagged-test discovery: `internal/devtool/verify_runner_test.go` asserts the `//go:build e2e` set still contains only the intended smoke live tests while the demoted live coverage remains full-only.
- The dedicated fast preflight tests are verifier-owned entrypoints, not part of the main fast suite contract. The verifier sets `ONEDRIVE_E2E_RUN_AUTH_PREFLIGHT=1` only for `TestE2E_AuthPreflight_Fast` and `ONEDRIVE_E2E_RUN_FAST_FIXTURE_PREFLIGHT=1` only for `TestE2E_FixturePreflight_Fast`, so the subsequent `go test -tags=e2e` pass does not silently rerun them.
- The fast auth preflight owns the one scrub of live suite artifacts. Later fast invocations set `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1`, so the fast fixture preflight and the main smoke suite reuse the already-scrubbed drive state instead of redoing the remote cleanup work.
- The per-PR fast E2E lane no longer uses `-race`. Race-heavy coverage stays in the default unit/public verification, stress, and `e2e-full` profiles where the extra compile and startup cost is acceptable.
- Required verification compiles the tagged `e2e_full` package with `go test -c -race -tags='e2e e2e_full' ./e2e` before any live suite runs. The compile-only `-c` form is intentional: the E2E package owns a live `TestMain`, so `go test -run=^$` would still execute credential validation and remote scrub side effects instead of acting as a pure build guard.
- Shared-file fixture resolution is identity-based, not recipient-slot-based. If
  more than one configured test account can open the same raw share link, the
  harness accepts that fixture as long as every matching configured recipient
  resolves to the same remote drive/item identity and then picks a stable
  recipient candidate deterministically.
- Known shared-folder fixture visibility through `drive list --json` is
  eventual because the command depends on best-effort
  `GET /me/drive/root/search(q='*')` discovery. Full-suite tests that validate
  exact shared selectors or compare successive shared-discovery outputs must
  poll across one ordinary read-only propagation window and skip that
  assertion if Graph never exposes the same known fixture on that run, rather
  than treating one empty search pass as a product regression.
- Shortcut fixture coverage uses stable manually-created placeholders rather
  than trying to create shortcuts through Graph. The durable fixture contract in
  `.testdata/fixtures.env` names the writable shortcut parent drive, writable
  shortcut alias, sharer email, and sentinel path; the read-only shortcut parent
  drive, alias, sharer email, and sentinel path; plus the standalone shared
  folder name used to prove explicit shared-folder drives remain standalone.
  The canonical two-account CI fixture set infers those non-secret names from
  `ONEDRIVE_TEST_DRIVE` and `ONEDRIVE_TEST_DRIVE_2`, while custom accounts set
  the fixture env vars explicitly.
  The current canonical manual fixture set is:
  `Kikkeli Shared Test Folder` owned by kikkelimies and shortcut-added in
  testitesti18's root, `Read-only Shared Folder` owned by testitesti18 and
  shortcut-added in kikkelimies's root, and `Testi2 Shared Folder` owned by
  testitesti18 and added as an explicit `shared:` drive for standalone shared
  folder coverage.
  Full shortcut rename/move tests mutate only the manual shortcut placeholder,
  restore its original alias and parent path in cleanup, and validate local
  projection/state reuse through sync output and status. They do not depend on
  provider-specific direct traversal through `/shortcut` in the parent drive.
  Live shortcut delete/manual-discard E2E tests are intentionally out of scope:
  Microsoft Graph does not document a reliable create/recreate API for these
  shortcuts, so deleting a manually seeded CI placeholder would make the fixture
  unrecoverable. Delete, final-drain, and manual-discard behavior stays covered
  by fake-Graph and unit/integration tests.
- Full-suite sync tests that stress concurrent uploads must assert eventual
  sync convergence, not single-pass perfection. A one-shot `sync --upload-only`
  run may legitimately persist retryable transient `retry_work` when one exact
  upload hits a live 5xx service outage, and the next sync pass is the
  contract-owned recovery path.
- The nightly/manual full E2E suite is layered on top of the fast suite. Its files use `//go:build e2e && e2e_full`, so the canonical invocation is the verifier's `e2e-full` profile, which sets both tags, preserves the fast-then-full ordering, and runs the full shared-folder fixture preflight before the `e2e_full` buckets.
- `e2e_full` owns the demoted live coverage that no longer runs on every PR: drive-list catalog checks, shared-file discovery checks, richer caller-proof/logout coverage, extended direct file-operation coverage, current condition/status rendering, slower sync state/recovery checks, engine-owned conflict protection, richer shortcut child-mount checks (`TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount`, `TestE2E_Shortcut_ExplicitStandaloneSharedFolderRemainsConfiguredDrive`, `TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible`, `TestE2E_Shortcut_RenameReusesChildMountState`, `TestE2E_Shortcut_MoveReusesChildMountState`, `TestE2E_Shortcut_WritableUploadSyncsToSharedTarget`, `TestE2E_Shortcut_ReadOnlyBlockedUploadStatus`, `TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes`, `TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget`, and `TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot`), and websocket startup smoke. Recurring live E2E does not delete manual shortcut placeholders because Microsoft does not document a supported Graph create/recreate contract for Add-to-OneDrive shortcuts; local delete lifecycle remains covered by fake-Graph/unit integration tests.
- `e2e_full` runs as three explicit verifier-owned buckets instead of one monolithic package invocation: `full-parallel-misc` (`-parallel 5` for tests already marked safe), `full-serial-sync`, and `full-serial-watch-shared`. The verifier executes those buckets sequentially after the single full-suite preflight so only the vetted misc bucket regains concurrency. The bucket lists intentionally exclude removed manual-resolution, delete-approval, and path-narrowing workflows; nightly coverage only names tests that still match the current product contract.
- The one-time full-suite preflight still owns the global live-drive artifact scrub. Bucketed `go test` runs set `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1` so they reuse that cleaned fixture state instead of re-scrubbing both configured drives before every bucket process.
- When `verify e2e` or `verify e2e-full` runs without an explicit `--e2e-log-dir`, the verifier resolves the repo-owned default temp log dir once up front, scrubs stale timing and quirk artifacts there, and passes that same `E2E_LOG_DIR` through the fast suite, the full-suite preflight, and every full bucket so live diagnostics stay in one place.
- The verifier gives every full bucket the same `60m` timeout budget; the older `30m` cap was no longer compatible once the suite intentionally stopped overlapping mutating tests.
- Websocket-specific E2E timing starts only after `observer_started(remote)`, and the timed mutation must target a subtree that bootstrap already materialized locally. Waiting merely for the socket connection is not enough on initial startup or daemon restart, and mixing the timed wake assertion with fresh parent-folder creation reintroduces the same delta-lag race the websocket harness is supposed to isolate from.
- Managed shortcut child projections are mount-root engines and intentionally emit a `websocket_fallback` debug event with note `mount_root`; the parent drive's primary-root watcher can still establish socket.io. Websocket smoke tests treat that child fallback as expected and only fail on primary-root endpoint/connect/fallback failures.
- Historical isolated shared-folder mount-root fixtures did not become ready just because owner-drive `stat /folder` succeeded. The helper must also wait for `ls /` against the derived mount-root drive to succeed before upload-only or watch tests start; otherwise Graph can still return `404 itemNotFound` on the mount-root children route for that same fresh parent.
- Mount-root remote read helpers must re-establish that same `ls /` boundary before and after known route-lag failures. Owner-drive path visibility or an earlier successful mount-root listing does not prove later exact-path `stat`, `get`, or other mount-root read pollers will avoid the documented `items/{rootID}/children` `404 itemNotFound` family.
- `--classify-live-quirks` is intentionally narrow. Today it covers `LI-20260405-04` (fast `TestE2E_Sync_DownloadOnly`), `LI-20260405-06` (strict auth preflight `/me/drives` propagation lag), and `LI-20260405-03` (the isolated `full-serial-watch-shared` recurrence of `TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart`). Unknown failures, or repeated failures after the one rerun, stay red.
- PR `e2e` and scheduled/manual `e2e-full` runs emit repo-owned verifier artifacts: the `--summary-json` report, the shared E2E `timing-summary.json` file, and the shared E2E `quirk-summary.json` file. The timing summary aggregates remote-write visibility, delete-disappearance, scope-transition, and sync-convergence wait windows across the fast suite, the full preflight, and all full buckets. The quirk summary records repo-owned live-provider evidence observed by the harness itself: CLI-emitted `graph.QuirkEvidence`, strict auth-preflight retry decisions, and fixture-helper recurrence classifications emitted by the helper that actually observed the recurrence.
- CI uses `--classify-live-quirks` only for scheduled/manual `e2e-full` runs. Required per-PR verification remains strict.
- CI keeps the expensive runtime stress lane out of the required per-PR path, but exposes it explicitly on a scheduled cadence and via manual dispatch so race-heavy watch/runtime regressions remain visible. Scheduled/manual stress runs now upload the verifier `--summary-json` artifact so timeout growth and slowest-test evidence remain visible even when the package step stays green.
- Managed repo-state files use `internal/fsroot` root capabilities.
- Sync-runtime filesystem operations under one configured sync root use `internal/synctree`. Managed shortcut projection roots use opt-in `synctree` helpers for no-symlink ancestor validation, safe component creation, root-contained path-state checks, rooted renames, empty-target replacement, byte-identical tree comparison, no-follow tree removal, and platform device/inode identity capture for same-parent shortcut alias rename detection; ordinary content sync symlink policy remains owned by the existing sync observation/execution rules.
- Arbitrary local file paths outside those rooted domains use `internal/localpath` as the explicit trust boundary. `localpath.AtomicWrite` is the default arbitrary-path writer when replace-in-place semantics matter.
- Append-only log file creation in `internal/logfile` is the single documented exception to atomic replace-style writes. It is allowed because the log is an append-only operational artifact, not an authoritative config/state file.
- `fsroot.Root` and `synctree.Root` carry unexported injectable ops so managed-state and sync-runtime I/O failure paths can be tested deterministically without package-level test hooks.

## Planned Improvements

- Resource consumption guarantees documented per component. [planned]
- Representative benchmark scenarios, reporting, and publication policy are defined in [performance-benchmarking.md](performance-benchmarking.md). The repo-owned `devtool bench` runner, the `startup-empty-config` harness-validation scenario, and the manual `sync-partial-local-catchup-100m` live representative scenario are implemented; release artifact promotion and published benchmark reports remain planned. [planned]
- Production ignored-return audit complete: invariant-bearing results such as `DepGraph.Complete` and dispatch admission booleans are handled explicitly, baseline/account metadata lookups now use explicit helpers or `found` checks, and the remaining ignored returns are limited to explicit value-only helper drops plus documented impossible-error builder writes. [verified]
- Bare `assert.NoError` audit complete: remaining sites are intentional cleanup, test-plumbing, or pure error-contract assertions, while weak CLI happy-path tests now assert rendered text, JSON shape, and preserved config/state. [verified]
- Write-path `Close` audit complete: sync-store close failures now propagate on internal baseline verification helpers and explicit sync-state reset flows, while read-only inspector close paths remain best-effort debug logged by design. [verified]
- Direct command/workflow coverage across the flattened `internal/cli/` command flows remains above the current target. [verified]
