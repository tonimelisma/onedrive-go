# New Developer Onboarding

This guide is the repo-wide orientation for new developers working on
`onedrive-go`. It starts with the helicopter view, then zooms inward until you
can place every major code area in the system and know how to approach real
work without getting lost.

The goal is not to memorize every file. The goal is to build the right mental
model:

- what the product does
- which package owns which truth
- how a request or sync pass moves through the system
- where durable state lives
- how to read the repo in the same order the runtime actually works
- how to make a change using the repo's required workflow

## How To Use This Guide

Read this guide once from top to bottom. Then keep it open while you walk the
actual code.

If you are brand new, do not start by reading `internal/sync/` file by file.
Start here, then read the design docs in the order this guide recommends, then
trace one simple vertical slice through the code.

## 1. Helicopter View

At the highest level, this repository is four things at once:

1. A user-facing CLI for OneDrive file operations, drive management, auth, and
   sync.
2. A sync engine that continuously reconciles local filesystem state and
   OneDrive state.
3. A repo-owned verification toolchain that codifies architecture, testing,
   worktree setup, and live-test policy.
4. A spec-led codebase where the design docs are part of the executable model,
   not optional prose on the side.

The most important repo-wide idea is ownership.

- `internal/cli` owns command wiring, output, and turning lower-layer results
  into user-facing behavior.
- `internal/graph` owns Graph API semantics, normalization, and auth flows.
- `internal/graphtransport` owns stateless Graph-facing HTTP transport profile construction.
- `internal/driveops` owns `SessionRuntime`, authenticated sessions, and
  transfer operations.
- `internal/multisync` owns multi-drive orchestration and control-socket
  lifecycle.
- `internal/sync` owns the single-drive sync runtime and durable sync-state
  mutation rules.
- `internal/devtool` owns verification, worktree creation, benchmarks, cleanup
  audit, and other repo tooling.

If you remember only one sentence, remember this:

> This repo is organized around authority boundaries, not around generic layers
> or convenience helpers.

### FAQ

**Why does the repo feel more "designed" than many Go CLIs?**

Because it is intentionally spec-led. Requirements, design docs, verifier
rules, and tests are meant to agree with the codebase's ownership boundaries.

**What should I optimize for while onboarding?**

Not speed-reading files. Optimize for answering: "who owns this decision,
state, side effect, or invariant?"

## 2. What The Product Actually Does

The product surface is easier to understand if you group it by capability:

- File operations: `ls`, `get`, `put`, `rm`, `mkdir`, `mv`, `cp`, `stat`
- Authentication and identity: `login`, `logout`, `status`
- Drive management: drive catalog/search/add/remove flows and drive selection
- Bidirectional sync: one-shot and `--watch`, with conflict tracking and
  resolution
- Read-only health and diagnostics: `status`, `perf`
- Developer tooling: `go run ./cmd/devtool ...`

The product is not just "a CLI wrapper around Graph". The difficult part of
the repo is the sync runtime: observing two worlds, planning deterministic
actions, executing with safety policy, and persisting just enough durable state
to recover correctly.

### FAQ

**Is this primarily a CLI repo or a sync engine repo?**

Both, but the sync system is the center of gravity. The CLI is the main product
surface, while the sync runtime is the main architectural core.

## 3. The Big Architectural Picture

The runtime can be sketched as three connected paths.

### 3.1 Simple command path

For a command like `ls` or `get`:

`main.go` -> `internal/cli` -> config/drive resolution ->
`driveops.SessionRuntime` bootstrap or target selection -> `graph` ->
result rendering

This path teaches you:

- how the CLI composes dependencies
- how drive selection and account context work
- how Graph calls are normalized
- how output and error presentation are owned by the CLI

### 3.2 Sync path

For a sync run:

`main.go` -> `internal/cli/sync*.go` -> `internal/multisync` ->
`internal/sync`

Inside `internal/sync`, the runtime currently has a hybrid sync pipeline:

- remote observer + local observer -> dirty debounce scheduler -> snapshot
  refresh -> SQLite comparison/reconciliation -> Go actionable set ->
  executor -> baseline/store updates
- `retry_state` and `scope_blocks` persist retry/trial timing and blocking
  semantics, not a durable executable plan

The key design choice is that planning stays deterministic and execution owns
side effects, while SQLite owns current durable truth and the persisted
retry/scope state.

### 3.3 Dev workflow path

For everyday development:

`cmd/devtool/main.go` -> `internal/devtool`

This is not optional repo furniture. `devtool` is the repo-owned way to:

- run verification profiles
- create and bootstrap worktrees
- run benchmark scenarios
- inspect cleanup candidates

### FAQ

**Why are `graph`, `graphtransport`, and `driveops` separate?**

`graph` owns Graph API semantics and normalization. `graphtransport` owns stateless
HTTP profile construction. `driveops.SessionRuntime` owns the command-scoped
runtime reuse of those profiles, plus authenticated sessions and higher-level
file workflows. The split keeps API behavior, transport policy, and runtime
reuse from collapsing into one owner.

**Why are `multisync` and `sync` separate?**

`multisync` owns multi-drive orchestration, watch lifecycle, and the control
socket. `sync` owns one drive's runtime, policy, and store mutation rules.

## 4. Step-By-Step In-Depth Onboarding Journey

If you want to get familiar with the entire repo in a deliberate order, this is
the best path.

### Step 1: Learn the repo's worldview

Read these first:

1. `README.md`
2. `AGENTS.md`
3. `CLAUDE.md`
4. `spec/requirements/index.md`
5. `spec/design/system.md`

By the end of this step, you should know:

- the product capabilities
- the repo's engineering philosophy
- the definition of done
- that docs and tests are part of the architecture contract
- that `cmd/devtool` is the canonical repo-owned verification entrypoint

### Step 2: Learn one simple vertical slice

Read the CLI and file-transfer path before sync:

1. `spec/design/cli.md`
2. `spec/design/config.md`
3. `spec/design/drive-identity.md`
4. `spec/design/graph-client.md`
5. `spec/design/drive-transfers.md`

Then trace a real command through the code:

- `ls` for a read-only Graph flow
- `get` for remote-to-local transfer
- `put` for local-to-remote transfer

This teaches the command boundary, output contract, account/drive resolution,
Graph client setup, and transfer behavior without requiring you to internalize
the full sync engine yet.

### Step 3: Learn the sync system in pipeline order

Do not read `internal/sync` alphabetically. Read it in runtime order:

1. `spec/design/sync-observation.md`
2. `spec/design/sync-planning.md`
3. `spec/design/sync-execution.md`
4. `spec/design/sync-engine.md`
5. `spec/design/sync-control-plane.md`
6. `spec/design/sync-store.md`
7. `spec/design/data-model.md`
8. `spec/design/error-model.md`

This is the core of the repo.

You want to come away understanding:

- what is observed
- what is planned
- what is executed
- what is persisted
- what is read-only projection
- what stays single-drive vs multi-drive

### Step 4: Learn the live edges and operational reality

Read the reference material that captures provider behavior and recurring
lessons:

1. `spec/reference/graph-api-quirks.md`
2. `spec/reference/onedrive-sync-behavior.md`
3. `spec/reference/live-incidents.md`
4. `spec/reference/onedrive-glossary.md`
5. `spec/reference/sync-ecosystem.md`

This step is what keeps a new developer from treating Graph, OneDrive, and live
sync behavior as a clean-room abstraction.

### Step 5: Learn the verifier and test strategy

Read:

1. `go run ./cmd/devtool --help`
2. `go run ./cmd/devtool verify --help`
3. `go run ./cmd/devtool worktree --help`
4. `internal/devtool/*.go`

Then read the test strategy sections in `AGENTS.md` / `CLAUDE.md`.

The important lesson is that verification is a productized part of the repo.
The repo does not rely on each contributor improvising their own workflow.

### Step 6: Ship one small increment end to end

Pick a small change and do it the repo's way:

- claim the work
- run `git fetch origin`, then create a fresh worktree from `origin/main`
- write or adjust tests first when behavior changes
- update docs in the same increment
- run `go run ./cmd/devtool verify default`
- produce a compliance report and checklist

This step turns the onboarding material into lived context.

### FAQ

**What is the single most important onboarding move?**

Reading the sync docs in pipeline order instead of file order.

**What is the second most important move?**

Using the `Verified By` sections in design docs to jump from design intent to
the tests that prove it.

## 5. Repo Map: Every Major Code Area

This section is the "all code in the repo at a glance" reference. You should be
able to scan this table and place every top-level code area.

### 5.1 Top-level paths

| Path | Owns | Read when |
| --- | --- | --- |
| `main.go` | Thin production process entrypoint | You want the CLI root entrypoint |
| `cmd/devtool/` | Devtool Cobra entrypoint | You want the repo-owned developer workflow |
| `internal/` | Production packages plus a few test-only helpers | Nearly all product/runtime work |
| `pkg/quickxorhash/` | QuickXorHash implementation used for OneDrive-compatible hashing | You are working on transfer or sync hashing behavior |
| `e2e/` | Live end-to-end coverage against real OneDrive accounts | You are validating product behavior under real provider conditions |
| `testutil/` | Shared general-purpose test helpers | You are writing package tests outside the sync-specific helpers |
| `scripts/` | Credential/bootstrap/dev helper scripts | You are setting up local live-test or CI-supporting developer flows |
| `spec/` | Requirements, design docs, and reference docs | You are learning or changing behavior |

### 5.2 Internal packages

| Package | Owns | Read when |
| --- | --- | --- |
| `internal/authstate` | Shared auth-health vocabulary and user-facing auth copy | You are working on auth status, `status`, or unauthorized sync presentation |
| `internal/cli` | Cobra command tree, CLI bootstrap, output formatting, command-family workflows, and final user-facing issue rendering | You are changing any user-facing command or command wiring |

| `internal/config` | TOML config loading, validation, path resolution, drive sections, and account reconciliation helpers | You are changing config semantics or lookup rules |
| `internal/devtool` | Repo-owned verifier, benchmarks, worktree tooling, and cleanup audit | You are changing developer workflow or repo consistency policy |
| `internal/driveid` | Type-safe drive identity types and canonicalization | You are touching drive selectors or canonical drive identity |
| `internal/driveops` | `SessionRuntime`, authenticated drive sessions, transfer operations, and path-convergence workflows | You are changing runtime reuse, get/put behavior, or local transfer rules |
| `internal/errclass` | Canonical runtime failure class enum | You are changing cross-boundary failure classification |
| `internal/fsroot` | Root-bound filesystem capabilities for managed state files | You are touching durable repo-managed file writes outside sync roots |
| `internal/graph` | Graph API semantics, normalization, auth, delta, item CRUD, transfer session behavior | You are changing Graph behavior or provider quirks |
| `internal/graphtransport` | Stateless Graph-facing HTTP transport profiles and transport defaults | You are changing HTTP runtime policy or profile construction |
| `internal/localpath` | Explicit arbitrary local-path filesystem operations | You are doing local file I/O outside a rooted capability |
| `internal/logfile` | Log file creation, rotation, retention | You are changing logging file lifecycle |
| `internal/multisync` | Multi-drive sync control plane and watch reload | You are changing `sync` orchestration across drives |
| `internal/perf` | Production-visible perf sessions, counters, live snapshots, and capture bundles | You are changing performance instrumentation or capture behavior |
| `internal/retry` | Retry policies and stateful backoff | You are changing retry semantics |
| `internal/sharedref` | Shared item selector parsing/formatting | You are working on shared links or shared item targeting |
| `internal/sync` | Single-drive sync engine, observation, planning, execution, store, and raw issue/status facts | You are changing sync behavior |
| `internal/synccontrol` | JSON-over-HTTP Unix-socket protocol shared by CLI and multisync owner | You are changing daemon/control-socket communication |
| `internal/synctest` | Shared sync-package test helpers | You are writing sync-adjacent tests |
| `internal/synctree` | Rooted filesystem capability for sync-runtime operations under one sync root | You are changing sync-local filesystem interaction |
| `internal/syncverify` | Re-hash local files against the persisted baseline | You are changing baseline verification behavior |
| `internal/tokenfile` | OAuth token file I/O | You are changing token persistence semantics |

### 5.3 Test and support code

| Path | Owns |
| --- | --- |
| `internal/synctest` | Sync-specific shared test helpers |
| `testutil` | Repo-wide test helpers |
| `e2e` | Live provider coverage, fixture preflight, and CLI/sync end-to-end behavior |
| `scripts/bootstrap-test-credentials.sh` | One-time live-test credential bootstrap that rebuilds `.testdata/` with token files, `catalog.json`, and `config.toml` |
| `scripts/migrate-test-data-to-ci.sh` | Move local `.testdata/` tokens, `catalog.json`, `config.toml`, and fixture env into CI secrets storage |
| `scripts/check-oauth2-fork.sh` | OAuth dependency fork checks |
| `scripts/dev-env.sh` | Local development environment helper |

Live-test note: `.testdata/` should contain `token_*.json`, `catalog.json`,
`config.toml`, and any expected state DB fixtures from
`bootstrap-test-credentials.sh`. The E2E isolation harness copies that
catalog-era fixture layout directly; it does not reconstruct legacy account or
drive JSON artifacts.

### FAQ

**Do I need to know every package on day one?**

No. But you should know that each package exists, what it owns, and whether it
is runtime code, support code, or repo tooling.

## 6. How To Read `internal/cli`

`internal/cli` is large because it owns the entire command surface, but it is
organized by file families.

| File family | Role |
| --- | --- |
| `root.go`, `doc.go`, `format.go`, `signal.go`, `cleanup.go`, `failure_class.go` | CLI bootstrap, process wiring, shared output boundaries, signal handling, error classification |
| `auth*.go`, `account_view.go`, `account_view_snapshot.go`, `email_reconcile.go` | Login/logout, account view projection, auth-health, email reconciliation |
| `drive.go`, `drive_add.go`, `drive_catalog.go`, `drive_cleanup.go`, `drive_list.go`, `drive_list_render.go`, `drive_remove.go`, `drive_search.go` | Drive add/list/search/remove, catalog projection, and drive selection flows |
| `get.go`, `get_shared.go`, `put.go`, `put_shared.go`, `ls.go`, `rm.go`, `mkdir.go`, `mv.go`, `cp.go`, `stat.go`, `purge.go` | User-facing file commands including shared-item get/put |
| `shared.go`, `shared_discovery.go`, `shared_target.go` | Shared item discovery, selection, and shared-target bootstrap |
| `sync.go`, `sync_flow.go`, `sync_runtime.go`, `sync_render.go`, `sync_cli_format.go`, `sync_debug_events.go`, `sync_pause_resume.go`, `control_client.go` | Sync command wiring, daemon/control-socket interaction, and sync lifecycle |
| `status.go`, `status_snapshot.go`, `status_sync_state.go`, `status_issue_descriptors.go`, `status_render.go`, `status_render_sections.go`, `status_perf.go` | Read-only status snapshot assembly and final issue rendering |
| `recycle_bin.go`, `recycle_bin_flow.go`, `perf.go`, `pause.go`, `resume.go` | Recycle-bin, perf, and pause/resume surfaces |
| `degraded_discovery.go` | CLI-owned degraded discovery behavior and presentation |

The right way to read `internal/cli` is by command family, not by filename
suffix. Start from the user-visible command you care about, then follow the
family files that share its prefix.

### FAQ

**Why are there so many small files in `internal/cli`?**

Because the package owns one large user-facing boundary, but the repo keeps
command families split so the Cobra wiring file does not become the workflow
owner for every command.

## 7. How To Read Graph, HTTP, And Transfers

The remote-access stack is easier to understand if you read it from the outside
in:

| Package or file family | Role |
| --- | --- |
| `internal/graphtransport` | Builds stateless Graph-facing HTTP transport profiles and transport defaults |
| `internal/graph/auth.go`, `normalize.go`, `errors.go` | Authentication and Graph-specific normalization/error handling |
| `internal/graph/delta.go`, `items.go`, `socketio.go`, `transfers.go` | Graph API surfaces for sync, file ops, websocket/bootstrap, and transfers |
| `internal/driveops` | `SessionRuntime`, authenticated drive/file operations, and transfer/path-convergence workflows |
| `internal/retry` | Stateful retry and backoff behavior reused across remote operations |
| `internal/driveid` | Canonical identity types used to keep drive references explicit |
| `internal/tokenfile` | Token persistence boundary used by config and graph code |
| `pkg/quickxorhash` | OneDrive-compatible content hashing utility used by transfer/sync comparisons |

If you are working on an issue that smells like "Graph did something odd",
decide which layer actually owns it:

- HTTP transport or profile construction -> `internal/graphtransport`
- Graph endpoint semantics or normalization -> `internal/graph`
- command-scoped runtime reuse, authenticated sessions, or transfer behavior -> `internal/driveops`
- retry/backoff -> `internal/retry`

### FAQ

**Why does the repo have `graph`, `graphtransport`, and `driveops`?**

`graph` speaks Graph. `graphtransport` builds the raw HTTP transport profiles Graph work needs.
`driveops.SessionRuntime` chooses and reuses those profiles for concrete
command/watch work, then `driveops` layers authenticated drive/file workflows
on top. That keeps API semantics, transport construction, and runtime reuse
explicit.

## 8. How To Read `internal/sync`

`internal/sync` is the densest package in the repo. It is easiest to navigate
when you treat it as several file families sharing one single-drive owner.

| File family | Role |
| --- | --- |
| `observer_local*.go`, `observer_remote.go`, `scanner.go`, `item_converter.go`, `socketio*.go`, `buffer.go`, `local_hash_reuse.go`, `observed_items.go` | Observation: remote and local change capture plus dedupe/buffering |
| `planner*.go`, `single_path.go`, `actions.go` | Pure planning: turn observed change plus baseline into deterministic actions |
| `executor*.go`, `worker*.go`, `worker_result.go`, `dep_graph.go`, `active_scopes.go` | Execution: worker dispatch, dependency ordering, scope admission, and conflict-safe file application |
| `engine.go`, `engine_config.go`, `engine_loop.go`, `engine_run_once.go`, `engine_watch*.go` | Runtime orchestration: main loop, one-shot run, watch lifecycle and batch reconciliation |
| `engine_primary_root*.go`, `engine_observation_postprocess.go`, `observed_items.go` | Engine-owned primary-root observation: root selection, shared-root fallback, postprocessing, and remote observation projection |
| `engine_result_*.go`, `engine_results.go`, `engine_retry_trial.go` | Result classification, retry-trial decisions, and scope-level result flow |
| `engine_scope_invariants.go`, `engine_scope_lifecycle.go` | Failure-scope lifecycle: mount/unmount and invariant enforcement for retry/permission scopes |
| `engine_runtime_state.go`, `engine_runtime_types.go`, `engine_time.go`, `engine_log_fields.go`, `engine_policy_controllers.go` | Engine runtime state, time helpers, structured logging, and policy controllers |
| `permissions.go`, `permission_capability.go`, `permission_decisions.go`, `permission_handler.go` | Capability-based permission boundaries and denied-path policy |
| `scope.go`, `scope_block.go`, `scope_key.go` | Scope types, scope blocking, and scope key canonicalization |
| `debug_event_sink.go` | Debug event recording for test and diagnostic observability |
| `store*.go`, `store_inspect.go`, `issue_summary.go`, `scope_key_wire.go`, `store_types.go`, `schema.go`, `tx.go` | Durable SQLite state: schema, store-generation validation, transactions, inspection, grouped issue summaries, persisted scope-key helpers, run-status/scope admin helpers, reset-required diagnosis, and explicit reset support |
| `store_read_*.go`, `store_write_*.go` | Store I/O: read projections (failures, remote state, snapshots) and write operations (baseline, failures, observation, scope blocks) |
| `summary_keys.go`, `visible_issues.go`, `issue_types.go` | Shared issue classification, summary keys, and raw read-only issue facts consumed by the CLI |
| `core_types.go`, `api_types.go`, `types.go`, `enums.go`, `errors.go`, `tracked_action.go`, `safety_config.go`, `baseline_orphans.go` | Common sync-domain vocabulary, API boundary types, and safety policy |
| `inotify_*`, `symlink_observation.go`, `engine_shared_root.go` | Platform or feature-specific observation/runtime helpers |

If you are debugging sync behavior, first decide which stage owns the problem:

- observation bug
- planning bug
- execution bug
- engine/lifecycle bug
- store/projection bug

That one decision usually cuts the search space down dramatically.

### FAQ

**Why is `internal/sync` one package instead of several smaller packages?**

Because the repo intentionally keeps the single-drive authority together.
Within that owner, file-family organization is the navigation tool.

**What should I read first inside `internal/sync`?**

Read the design docs first, then open the matching file families in the same
runtime order: observation -> planning -> execution -> engine -> store.

## 9. Source Of Truth And Durable State

A new developer should know where truth lives before changing any behavior.

| Concern | Source of truth | Notes |
| --- | --- | --- |
| CLI args and output mode | `internal/cli` runtime state | Command-scoped, ephemeral |
| User config | `internal/config` TOML file | Drive sections, overrides, pause state, account metadata |
| OAuth token persistence | `internal/tokenfile` files | Shared by config and graph boundaries |
| Remote truth | Microsoft Graph / OneDrive | Observed through `internal/graph` |
| Graph HTTP policy | `internal/graphtransport` | Shared runtime transport policy, not user config |
| Graph session/runtime reuse | `internal/driveops.SessionRuntime` | Command-scoped bootstrap, interactive, and sync client reuse |
| Status issue title/reason/action rendering | `internal/cli/status_issue_descriptors.go` | Final human-facing status copy belongs to the CLI |
| Read-only sync issue/status snapshot | `internal/sync` store inspection helpers | Raw durable issue facts stay in sync-owned read paths |
| Per-drive sync state | `internal/sync` SQLite DB | One DB per drive |
| Remote observation mirror | `remote_state` table | Latest observed remote truth |
| Confirmed synced state | `baseline` table | Shared local/remote agreement |
| Durable sync issues | `sync_failures`, `scope_blocks`, catalog account-auth state | Sync retry, restart, and failure state |
| Log files | `internal/logfile` | Durable operational history, not authoritative state |
| Perf live snapshots/captures | `internal/perf` | Live or explicit capture surfaces, not a second persistent DB |

The biggest data-model idea is that sync uses a separated durable model:

- `remote_state` is the latest observed remote mirror
- `baseline` is confirmed synced agreement
- `retry_state`, `sync_failures`, and `scope_blocks` capture durable retry,
  reporting, and restart-safe blocking state

That separation is what keeps observation, planning, execution, and restart
from collapsing into one mutable pile.

### FAQ

**Is the database the sync pipeline coordinator?**

No. The store is durable truth and restart state. The runtime pipeline is still
owned by in-process observation, planning, execution, and engine coordination.

## 10. Tests, Verification, And Proof

A big part of being "familiar with all the code" in this repo is being
comfortable with the proof surfaces.

### 10.1 Unit and package tests

Most behavior is covered in package-level tests living beside the production
files. These are the fastest way to see the intended contract for a package.

Two conventions matter a lot:

- `// Validates: ...` comments connect tests to requirement IDs.
- Design-doc `Verified By` sections point you to the exact tests that prove a
  behavior.

### 10.2 End-to-end tests

`e2e/` covers live behavior against real OneDrive accounts.

The important thing to remember:

- fast `e2e` is the minimal per-PR smoke lane
- `e2e-full` is the slower, richer live suite
- live tests require explicit credentials and fixture setup

### 10.3 Verifier-owned workflow

The canonical local verification entrypoint is:

```bash
go run ./cmd/devtool verify default
```

Other profiles exist for public, e2e, e2e-full, integration, and stress work.

If you are new here, it is worth opening `internal/devtool/verify.go`,
`verify_docs.go`, `verify_e2e.go`, `verify_repo_checks.go`, and
`verify_summary.go`. Those files explain a lot about what the repo considers
architecturally important.

### FAQ

**Why not just run `go test ./...` and call it done?**

Because the repo treats architecture checks, doc consistency, live-test
orchestration, and coverage policy as part of the contract. `devtool verify`
encodes that contract.

## 11. What `cmd/devtool` Owns

It helps to make `devtool` explicit during onboarding because it is easy to
mistake it for optional contributor convenience.

`cmd/devtool` and `internal/devtool` own:

- `verify`: repo-owned verification profiles
- `worktree`: worktree creation/bootstrap rooted at `origin/main`; callers must fetch first
- `bench`: benchmark scenarios and result bundles
- `cleanup-audit`: read-only git cleanup classification

The file-family split inside `internal/devtool` is also worth knowing:

| File family | Role |
| --- | --- |
| `verify.go`, `verify_docs.go`, `verify_e2e.go`, `verify_repo_checks.go`, `verify_stress.go`, `verify_summary.go` | Verification profile orchestration, docs checks, repo checks, E2E orchestration, stress profiles, summaries |
| `worktree.go` | Worktree creation and bootstrap rooted at `origin/main`; callers must fetch first |
| `bench.go`, `bench_live.go` | Benchmark scenarios, result recording, and live benchmark runs against real accounts |
| `cleanup_audit.go` | Read-only git cleanup classification |
| `runner.go` | Shared subprocess execution helpers for repo tooling |

This means the repo owns both the product runtime and the contributor runtime.

### FAQ

**When should I read `internal/devtool` deeply?**

As soon as you want to contribute regularly. It explains how the repo expects
changes to be validated, and it often encodes architectural rules that are only
mentioned briefly elsewhere.

## 12. Common "Why Is It Designed This Way?" Questions

### Why is planning supposed to be pure or deterministic?

Because planning is where the repo wants reproducible, debuggable decisions.
Observation and execution are effectful; planning is where the code tries to
stay value-oriented.

### Why does the repo prefer worktrees and PR increments?

Because the workflow is designed around clean, reviewable increments from a
freshly fetched `origin/main`, with verification and docs updated in the same
change.

### Why are there so many docs?

Because the docs are not only onboarding material. They are the ownership map,
the architecture contract, the traceability layer, and the verifier's expected
model.

### Why does the guide keep telling me to ask "who owns this?"

Because most mistakes in this repo come from changing behavior in the wrong
owner, duplicating authority, or letting read-only projection code mutate
durable truth.

## 13. Recommended Reading Order For The Actual Code

If you want one practical sequence for becoming familiar with the entire repo,
use this:

### Day 1

1. `README.md`
2. `AGENTS.md`
3. `CLAUDE.md`
4. `spec/requirements/index.md`
5. `spec/design/system.md`
6. `cmd/devtool/main.go`
7. `main.go`

### Day 2

1. `spec/design/cli.md`
2. `internal/cli/root.go`
3. one simple command family such as `ls.go` or `get.go`
4. `spec/design/config.md`
5. `internal/config/*`
6. `spec/design/graph-client.md`
7. `internal/graph/*` and `internal/graphtransport/*`
8. `spec/design/drive-transfers.md`
9. `internal/driveops/*`

### Days 3-5

1. `spec/design/sync-observation.md`
2. `spec/design/sync-planning.md`
3. `spec/design/sync-execution.md`
4. `spec/design/sync-engine.md`
5. `spec/design/sync-control-plane.md`
6. `spec/design/sync-store.md`
7. `spec/design/data-model.md`
8. matching `internal/sync/*`, `internal/multisync/*`, `internal/synccontrol/*`

### Week 2

1. `spec/reference/graph-api-quirks.md`
2. `spec/reference/onedrive-sync-behavior.md`
3. `spec/reference/live-incidents.md`
4. `internal/devtool/*`
5. `e2e/*`

### Week 3

1. pick one small increment
2. create a worktree
3. make the change with tests and doc updates
4. run `go run ./cmd/devtool verify default`
5. open a PR and treat review comments as another learning pass through the
   ownership model

## 14. How To Start Work Without Getting Lost

When you pick up a task, use this checklist:

1. Find the owning package from the routing table in `AGENTS.md` / `CLAUDE.md`.
2. Read the governing requirement and design doc first.
3. Decide whether the task is primarily CLI, Graph, transfer, control-plane,
   sync-runtime, or store work.
4. Open the relevant package tests before changing code.
5. Look at the `Verified By` section in the design doc for the nearest behavior.
6. Make the smallest increment that keeps ownership clear.
7. Update docs in the same change.
8. Run the repo-owned verifier.

That process sounds slower at first, but it makes this repo much easier to
reason about once you internalize it.

## 15. Final Mental Model To Keep In Your Head

If you want one compact model of the whole repo, use this:

- the CLI owns user interaction
- graph owns remote API semantics
- graphtransport owns stateless HTTP transport profile construction
- driveops owns session runtime reuse and transfer behavior
- multisync owns multi-drive orchestration
- sync owns one drive's runtime and durable sync state
- devtool owns contributor/runtime verification tooling
- specs explain what should happen
- tests prove what actually happens

When those layers and ownership boundaries line up in your head, the rest of
the repo becomes much easier to navigate.
