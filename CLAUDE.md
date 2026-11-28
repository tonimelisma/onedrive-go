# CLAUDE.md

## What this is

onedrive-go is a fast, safe OneDrive CLI and sync client: Unix-style file operations (`ls`, `get`,
`put`, `mv`, `rm`) plus bidirectional sync and a watch daemon, for Linux and macOS, MIT licensed.
It ships as static Go binaries (`linux-amd64`, `darwin-arm64`) on GitHub Releases, each with a
build-provenance attestation. Go only; the repository's own tooling is `cmd/devtool`.

## Working style

- When it is clear what is wanted, do it. Ask only for what only the user can do, or for a real
  product choice nobody has made yet.
- Don't end a turn with a summary announcing the next step, an offer to continue, or a list of
  decisions that block nothing: take the next step.
- Stop only when nothing can move forward without the user, or when what blocks you is
  deliberately protected (a permission, a ruleset, a secret). Then say exactly what is needed.
- Still confirm destructive or irreversible actions outside the normal flow (deleting data,
  rewriting history, changing settings); the normal flow (branch, commit, PR, merge, release) is
  not one of them.
- Live tests clean up everything they create in their own teardown, even when they fail, and
  confirm the removal.

## Product invariants

- Never lose user data: a sync never deletes or overwrites a local or remote file whose change it
  has not seen synced, and every disagreement it cannot resolve becomes a kept conflict copy
  ([sync-planning.md](spec/design/sync-planning.md), [sync.md](spec/requirements/sync.md)).
- Files that matter are written atomically: a temp file in the target directory, `fsync`, then
  rename ([sync-execution.md](spec/design/sync-execution.md)).
- File identity is content, not timestamps: QuickXorHash and SHA-256 are the truth, a timestamp
  is only a fast-path hint ([drive-transfers.md](spec/design/drive-transfers.md)).
- The state database is a cache of decisions, never the source of truth: lost or corrupt state is
  rebuilt from the remote and the local tree without the user's help
  ([sync-store.md](spec/design/sync-store.md)).
- The app has no users yet and no compatibility requirement anywhere: config, token and state
  files, CLI flags, output and internal APIs change whenever a better design needs it, with no
  shims. This file is where that rule lives.
- Every merge to `main` is released, attested by `.github/workflows/release.yml` on
  `refs/heads/main`, and nothing else releases (R-6.10.41,
  [system.md](spec/design/system.md)).

## Engineering philosophy

- No compatibility shims. When a structure is wrong, replace it and migrate its callers in the
  same change.
- The smallest correct design. Simplicity first; measure before optimizing, never optimize on a
  hunch.
- One owner per fact, per mutation and per side effect. Two places that know the same thing will
  disagree.
- Abstractions only at real boundaries (a process, a network, a storage format, a platform), not
  in anticipation.
- Don't guess and don't fabricate: an unknown API, path, value or result is looked up, measured or
  reported as unknown.
- You own the repository. Nothing is "pre-existing" or someone else's problem, and red CI is never
  waived: it is fixed first.
- Comments and docs describe current behavior and why it is that way, never its history.

Repository-specific:

- **Functional core, imperative shell.** Planning, classification, validation and safety policy
  are deterministic and side-effect free; filesystem, network, database, clock and process effects
  stay at the edges behind explicit capabilities (narrow objects for a sync root, a token file, a
  store), never ambient `os` or `http`.
- **Single-owner runtime state.** Watch loops, worker pools, retries and lifecycle control have
  one owner; single-writer discipline beats defensive mutation spread across goroutines. In-memory
  state is an ephemeral working set or a derived index, rebuildable, never a competing authority.
- **No abstraction without authority.** A type, package or layer exists only if it owns I/O,
  policy, lifecycle or a durable truth boundary: no services, managers, adapters, builders or
  pass-through DTOs. Inside a package a short concrete helper beats a handler chain. Interfaces
  live at the consuming boundary, never around every concrete type.
- **DRY applies to knowledge, not syntax.** Never duplicate a fact, a policy or a lifecycle rule;
  small code duplication is fine when it keeps ownership clear.
- **No ghost architecture.** A rename, move or deletion updates every comment, doc, `GOVERNS:`
  line, verifier rule, error prefix and test name in the same change (comments and error prefixes
  name the package that owns the code now); `internal/sync` is split
  along its authority boundaries (the routing table below) with no re-export layer.
- **Say what the code really does.** "Pure" only for code that neither logs, mutates nor does I/O;
  otherwise "deterministic".
- **Simple until measured.** Bottlenecks here are usually Graph latency, filesystem I/O or
  transaction boundaries, not where intuition points. A linear scan over a small per-folder or
  per-batch set, or a brute-force rebuild, beats a cache with subtle invalidation until a profile
  says otherwise. Data model first, algorithm second.

## Engineering practices

- Test first. A change in behavior starts with a test that fails without it.
- Every bug, CI failure and review finding gets a regression test, seen failing before the fix;
  quote its red output in the PR.
- The instrumentation flywheel, every increment:
  - Never guess a bug's cause. If the logs and metrics can't explain it, add the instrumentation
    first, so this occurrence or the next one is diagnosable, then fix.
  - Never guess at performance: measure, keep a baseline, optimize, and report before and after
    (`devtool bench`, `status --perf`, [performance-benchmarking.md](spec/design/performance-benchmarking.md)).
  - Even when nothing is blocked, review the instrumentation on every path you touched: add what
    was missing, remove what is noise.
- Tests are hermetic: temporary directories and fixtures, never the developer's real data, config
  or accounts.

Go conventions that lint and the repository rules do not enforce (lint does enforce the rest:
`.golangci.yml`, and `devtool verify` holds testify-only assertions, no `time.Sleep` in tests, the
`...ForTest` seams and the test-support import rules):

- **Errors.** Wrap as `fmt.Errorf("verb noun: %w", err)`, so the chain reads
  `sync file: upload chunk: HTTP POST: connection refused`; never "failed to". Translate Graph,
  filesystem and SQLite errors into domain errors at the boundary that understands them, and wrap
  once per boundary. A sentinel exists only when a caller branches on it. Never swallow an error:
  a handled one becomes an explicit decision, a durable record or a returned error, logged once at
  the boundary that owns the decision. Panics are for impossible states only, recovered at
  goroutine boundaries and logged at Error. Bulk operations collect, report and continue.
- **Concurrency.** Every goroutine has an owner that ensures it ends; no fire-and-forget. Every
  function that does I/O or can block takes `context.Context` first and honors it. Channel
  direction is declared in signatures and the sender closes. A mutex's comment says which fields
  it guards; no I/O or channel operation under a lock; `sync.Mutex` unless profiling shows read
  contention. Fan-out is bounded (a semaphore or a fixed worker set), never by input size.
- **Resources and files.** Check the error of `Close()` on writes. Stream; never read a body or
  file whole unless bounded and under 1 MB. Close every `http.Response` body. Temp files go in the
  target directory, so the rename is atomic. Paths use `filepath.Join`. Validate every path from
  outside (`..`, absolute names, reserved characters) before touching the filesystem. Preserve
  remote metadata you don't model: write back only the fields you manage.
- **APIs.** Timeout budgets propagate inward with `context.WithTimeout` at each layer. Every API
  response is untrusted: nil-check nested fields, validate enums, don't trust `Content-Length` or
  array order. Follow every `@odata.nextLink`. Retry automatically only idempotent operations.
- **State.** State transitions are enumerated and validated, illegal ones refused loudly; state
  persists atomically (a transaction or write-replace).
- **Types.** Make illegal states unrepresentable (`driveid.ID`, enums over strings); validate at
  system boundaries and trust inside a package. Named fields in every struct literal. Unexported by
  default. Every collection from outside is bounded or streamed. Timeouts on everything external.
- **Shutdown.** On SIGINT/SIGTERM stop admitting work, cancel in-flight operations and let each
  unwind, commit the records of work already done on a context detached from the cancel, then
  exit; a second signal exits at once. Cleanup runs on every exit path.
- **Logging.** `log/slog` with structured fields. Debug: HTTP, token acquisition, file I/O. Info:
  lifecycle (login, sync start and end, config load). Warn: degraded but recoverable (retries,
  fallbacks); bulk sync logs scope-level events and end-of-pass summaries, items at Debug. Error:
  terminal failures. Never log a secret.
- **Dependencies.** Prefer the standard library; a new dependency is weighed for maintenance,
  transitive dependencies and attack surface.
- **Tests.** `require` when the test cannot go on, `assert` otherwise. A test that proves a
  requirement carries `// Validates: R-X.Y.Z` (comma-separated for several) on the line before
  `func Test...`, or on a table case. Test the contract through exported APIs. Every I/O path has a
  failure-injection test; fakes implement the full contract (latency, partial results, errors),
  hand-written. Wait on readiness channels, injected clocks or observable state, never wall time.
  Golden files (`-update`) for structured output. Concurrent paths get `-race` and `-count=100`
  stress on flaky candidates. A test is exhaustive (`skipInShort`, skip message `exhaustive: runs
  in the full verify profiles`) only when it is a breadth, replay, randomized or many-pass variant
  of behavior a targeted test proves; under `-short` every targeted sync engine test runs.

## Commands

```sh
go run ./cmd/devtool verify --dod --stage pre-pr  # what to run before every push: lint, then quick
go run ./cmd/devtool verify                # the same `pr` profile without the DoD checks
go run ./cmd/devtool verify quick          # format (rewrites; --check-format only lists), repository rules, builds, tagged vets, go test -short
go run ./cmd/devtool verify public         # full tier: race, coverage gate, exhaustive tests (>1 h on 2 cores)
go test -run '^TestName$' ./internal/sync  # one test
go run ./cmd/devtool verify --dod --stage start | pre-merge --pr <N> | post-merge --pr <N>
```

## Where docs live

- `spec/` is the engineering record. Requirements live in `spec/requirements/*.md` (index:
  [index.md](spec/requirements/index.md)): stable IDs `R-X.Y.Z`, one sentence each, current
  behavior, each `[verified]` by a test tagged `// Validates: R-X.Y.Z` (or by the design doc that
  implements it) or marked with another status and its reason. A `[planned]` status names
  accepted future behavior; new future work is filed as a GitHub issue.
- `spec/design/<module>.md` says how each module is built and why; each starts with `GOVERNS:`
  naming the files it owns. `spec/reference/` holds API quirks, the live-incident ledger
  ([live-incidents.md](spec/reference/live-incidents.md)), onboarding
  ([developer-onboarding.md](spec/reference/developer-onboarding.md)) and the cloud environment
  ([cloud-development.md](spec/reference/cloud-development.md)).
  [lessons.md](spec/lessons.md) holds rules learned, research findings and open problems, and is
  pruned when they are resolved. `docs/` is the user documentation; `README.md` summarizes
  capability status from the requirements index and adds no roadmap of its own.
- Update the docs and this file in the same PR as the code they describe: the design doc of each
  touched module, requirement statuses, reference docs for a new API quirk, the live-incident
  entry for a live CI failure (reuse a recurring one), README for capability or workflow changes.
- Learnings go to [lessons.md](spec/lessons.md), decisions to the design docs. Never committed:
  per-PR readiness or report files, session state or resume files, ledgers of merged PRs, program
  notebooks. That information belongs in the PR body, its comments and GitHub issues.

## Routing Table

| When modifying... | Read first | Also consult |
|-------------------|-----------|--------------|
| `internal/graph/` | `spec/design/graph-client.md` | `spec/reference/graph-api-quirks.md` |
| `internal/graphtransport/` | `spec/design/graph-client.md` | |
| `internal/tokenfile/` | `spec/design/graph-client.md` | |
| `internal/config/` | `spec/design/config.md` | |
| `internal/cli/drive_reset_sync_state.go`, `internal/cli/drive_rebind_root.go` | `spec/design/drive-identity.md` | `spec/design/sync-store.md`, `spec/design/sync-engine.md`, `spec/design/data-model.md` |
| `internal/driveid/`, `internal/cli/drive*.go`, `internal/cli/purge.go` | `spec/design/drive-identity.md` | |
| `internal/driveops/`, `internal/cli/get.go`, `internal/cli/put.go` | `spec/design/drive-transfers.md` | |
| `pkg/quickxorhash/` | `spec/design/drive-transfers.md` | |
| `internal/retry/` | `spec/design/retry.md` | |
| `internal/errclass/` | `spec/design/error-model.md` | `spec/design/system.md` (carries its `GOVERNS:` line) |
| `internal/localtrash/`, `internal/localpath/`, `internal/fsroot/`, `internal/namekey/`, `internal/sharedref/`, `internal/authstate/`, `internal/synctest/`, `internal/sync/internal/syncfixture/`, `internal/sync/internal/syncstoretest/`, `internal/panicguard/`, `internal/filelock/`, `internal/humanunit/` | `spec/design/system.md` | `spec/design/sync-execution.md` (local trash), `spec/design/sync-observation.md` (name keys, root identity) |
| `internal/loglevel/` | `spec/design/cli.md` | `spec/reference/cli-output-guidelines.md` |
| `internal/perf/` | `spec/design/system.md` | `spec/design/cli.md`, `spec/design/sync-control-plane.md` |
| `internal/devtool/` | `spec/design/system.md` | |
| `internal/synctree/` | `spec/design/system.md` | `spec/design/sync-engine.md`, `spec/design/sync-observation.md`, `spec/design/drive-transfers.md` |
| `internal/logfile/` | `spec/design/cli.md` | `spec/requirements/configuration.md` |
| `internal/sdnotify/`, `internal/cli/sync_notify.go` | `spec/design/cli.md` | `spec/design/sync-control-plane.md`, `spec/reference/cli-output-guidelines.md` |
| `internal/sync/internal/syncobserve/` | `spec/design/sync-observation.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/syncdomain/` | `spec/design/sync-domain.md` | `spec/design/sync-planning.md`, `spec/design/sync-store.md` |
| `internal/sync/internal/syncplan/` | `spec/design/sync-planning.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/internal/syncexec/` | `spec/design/sync-execution.md` | |
| `internal/sync/` (the files directly in it: the engine), `internal/cli/sync_flow.go`, `internal/cli/sync_runtime.go` | `spec/design/sync-engine.md` | |
| `internal/sync/syncshortcut/`, `internal/sync/syncstore/store_shortcut_roots.go`, `internal/sync/syncstore/store_shortcut_root_status_read.go` | `spec/design/sync-shortcut.md` | `spec/design/sync-engine.md`, `spec/design/sync-store.md` |
| `internal/multisync/`, `internal/synccontrol/`, `internal/cli/sync.go` | `spec/design/sync-control-plane.md` | `spec/design/sync-engine.md`, `spec/design/config.md` |
| `internal/sync/syncstore/`, `internal/cli/status.go`, `internal/cli/status_snapshot.go`, `internal/syncverify/` | `spec/design/sync-store.md` | `spec/design/data-model.md`, `spec/design/sync-execution.md`, `spec/design/sync-engine.md` |
| `internal/cli/root.go`, `internal/cli/format.go`, `internal/cli/signal.go`, `internal/cli/auth*.go`, `internal/cli/account_lifecycle.go`, `internal/cli/account_view*.go`, `internal/cli/email_reconcile.go`, `internal/cli/shared*.go`, `internal/cli/get_shared.go`, `internal/cli/put_shared.go`, `internal/cli/ls.go`, `internal/cli/rm.go`, `internal/cli/mkdir.go`, `internal/cli/mv.go`, `internal/cli/cp.go`, `internal/cli/stat.go`, `internal/cli/replace_destination.go`, `internal/cli/pause.go`, `internal/cli/resume.go`, `internal/cli/sync_pause_resume.go`, `internal/cli/recycle_bin.go`, `internal/cli/recycle_bin_flow.go`, `internal/cli/trash.go`, `internal/cli/status*.go`, `internal/cli/perf.go`, `internal/cli/cleanup.go`, `internal/cli/failure_class.go`, `internal/cli/error_presentation.go`, `internal/cli/json_output.go`, `internal/cli/journal.go`, `internal/cli/console_log*.go`, `internal/cli/control_client.go`, `internal/cli/degraded_discovery.go`, `internal/cli/sync_console*.go`, `internal/cli/sync_render.go`, `internal/cli/sync_dir.go`, `internal/cli/sync_startup_messages.go`, `internal/cli/sync_cli_format.go`, `internal/cli/sync_debug_events.go`, `main.go` | `spec/design/cli.md` | |

| Working on capability... | Requirements | Design docs |
|--------------------------|-------------|-------------|
| R-1 File Operations | `spec/requirements/file-operations.md` | `spec/design/cli.md`, `spec/design/drive-transfers.md` |
| R-2 Sync | `spec/requirements/sync.md` | `spec/design/sync-*.md`, `spec/design/data-model.md` |
| R-3 Drive Management | `spec/requirements/drive-management.md` | `spec/design/drive-identity.md`, `spec/design/config.md` |
| R-4 Configuration | `spec/requirements/configuration.md` | `spec/design/config.md` |
| R-5 Transfers | `spec/requirements/transfers.md` | `spec/design/drive-transfers.md` |
| R-6 Non-Functional | `spec/requirements/non-functional.md` | `spec/design/system.md`, `spec/design/retry.md` |

Every production `.go` file under `internal/`, `pkg/`, and `cmd/` must be named
by exactly one design doc's `GOVERNS:` line, every `` `TestXxx` `` cited as
evidence in a design doc must exist, and every path referenced in the routing
table above must resolve. All three are enforced by
`go run ./cmd/devtool verify public`; when two docs claim the same file, the
more specific pattern wins.

## Increment lifecycle

1. **Start**: `go run ./cmd/devtool verify --dod --stage start --branch <branch>`. It writes the
   local, ignored `.dod-pr-comments.json` from the review threads of recent merged PRs: classify
   every unresolved one `fixed`, `already_fixed` (with `what_changed`, `how_fixed`, evidence) or
   `non_actionable` (with `reason`), and fold every actionable one into this increment. It also
   reads the newest scheduled run of `main` (R-6.10.23): every failure is claimed by this
   increment, and a claimed failure is fixed in it, root cause from the run's logs, recorded in
   [live-incidents.md](spec/reference/live-incidents.md); or it is reported as held by another
   branch. Hand back a claim beyond this increment with `go run ./cmd/devtool nightly release
   --branch <branch> --key <key>` and say so in the report. A failing model seed prints
   `seed-replay: <command>`: replay it at the commit the run tested, keep the seed as a named
   test, and close its `seed-failure` issue with `Closes #N`. For a live failure, search
   [live-incidents.md](spec/reference/live-incidents.md) first. Read the open issues, the
   governing design doc and requirements (routing table), and fix any drift between docs and
   code before building on it.
2. **Ready**: write the Definition of Ready in the PR body (below); if discovery later changes
   the scope, owner, regression or verification plan or the safety profile, update it.
3. **Test first** (red, green, refactor), then implement with the instrumentation flywheel.
4. **Verify**: `go run ./cmd/devtool verify --dod --stage pre-pr` (lint, then quick, and the
   branch contains `origin/main`), green before pushing. A failed go test step keeps its full
   output under `.verify-logs/` and names the file.
5. **Docs** in the same PR (Where docs live). After a boundary move, grep for the deleted names
   and remove every stale reference.
6. **Land**: push, open the PR, then `go run ./cmd/devtool verify --dod --stage pre-merge --pr
   <N>`: it waits for CI (reruns jobs a runner cancelled), posts the classified review-thread
   replies, resolves handled threads and requires none unresolved. Don't wait for automatic
   review comments that never arrive; a late actionable one is fixed in a follow-up PR. Then
   `go run ./cmd/devtool verify --dod --stage post-merge --pr <N>` on the clean branch: it
   squash-merges over REST pinned to the head it checked, with the attribution trailer, marks the
   branch's nightly claims fixed, checks out a fresh `main` and deletes the merged branch,
   waits for the push-to-`main` CI run, prunes merged builder worktrees and runs the cleanup
   audit. The release follows by itself.
7. **Report**: after the merge, one PR comment with the Definition of Done checklist and the final
   increment report (below), posted with
   `gh api -X POST repos/tonimelisma/onedrive-go/issues/<N>/comments -F body=@<file>` from a file
   outside the repository. The chat gets the same text or its link.

A plan that spans several increments lives in a GitHub tracking issue with a checklist, never in
a file.

### Definition of Ready

The PR body's `## Definition of Ready` section lists these items, each `PASS` or `ASSUMPTION`
with its evidence (a file, command, design-doc section or the assumption and how it will be
validated). Code edits start only when none is `FAIL`.

1. **Task intent**: one product-visible behavior, maintenance outcome or investigation result.
2. **Non-goals**: what is out of scope.
3. **Governing docs**: the requirements, design and reference docs read.
4. **Authority owner**: the owner of truth, mutation, side effects, durable state, runtime
   lifecycle and user-facing output for the change.
5. **Code scope**: the packages and files expected, and what must not be touched.
6. **Regression plan**: the failing or new test, or the verification surface, that proves it.
7. **Verification plan**: focused test commands, race, stress or live needs, the verify profile.
8. **Data safety**: how unintended local or remote deletes, overwrites, state corruption and
   stale-truth mutation are avoided.
9. **Secret and credential safety**: whether tokens, CI secrets, test account names or
   credential-bearing logs are involved, and the redaction plan.
10. **Live and destructive operations**: whether it can mutate live OneDrive state, reset local
    state, alter release artifacts, rewrite history or upgrade dependencies; if so the exact
    operation, why, and the recovery plan.
11. **Review carryover and nightly**: the start stage's result, how stale threads are folded in,
    which nightly failures were claimed (and the planned fix) or found held elsewhere.
12. **Unknowns**: each as `Question:`, `Current assumption:`, `Validation step:`, `Doc or code
    affected if wrong:`.

The body also carries `## Review-thread carryover` (prior PR, comment, evidence, status, or
`none`) and, when the branch holds nightly claims, `## Nightly failures` naming each claimed key
and its fix: pre-merge refuses a body that misses a key the branch holds.

## Verification tiers

What each profile and CI lane runs, its deadlines and how they scale with the cores, sharding,
the nightly lanes, stress membership, seeds and the live buckets are specified in
[system.md](spec/design/system.md), Verification Policy (R-6.10.22). What you do:

- Before a PR: the pre-pr stage (`pr` profile). `verify public` is the full tier: run it as the
  pre-PR gate (`verify public --dod --stage pre-pr`) only on a host that can finish it.
- PR CI runs the quick lane: the lint matrix (linux, darwin with `race`, freebsd) and the
  `verify quick` shards. Every push to `main` adds `verify race` (the `./internal/sync/...` tree
  under the race detector). The nightly runs `verify public` on both platforms, `stress`,
  `seeds` and `e2e-full`. **A data race outside the sync tree, a coverage drop or an
  exhaustive-tier failure can reach `main` and is caught by the nightly**, which can therefore be
  red through no change of yours: the start stage says what to do.
- The coverage gate's number lives only in requirement R-6.10.4, which devtool reads.
- Live tests run against live OneDrive test accounts, only in CI. Test account names are never
  committed and never GitHub variables: CI reads the test drives and the allowlist from Azure
  Key Vault, masks them, writes `.testdata/` and exports the test-account variables;
  `TestE2E_TrackedFilesNameNoTestAccount` fails when a tracked file spells a configured account.
  The `e2e` tag is the PR smoke lane, its tests listed in `fastE2ELiveTestNames`. The `e2e_full`
  tag is the nightly suite, run only through `go run ./cmd/devtool verify e2e-full`: a full-tier
  test's name `TestE2E_<Family>_<Case>` places it through the family table
  `fullE2EFamilyBuckets` (`internal/devtool/verify_e2e_tiers.go`), and a name the table cannot
  place stops the run before its first live call (R-6.10.28). A family joins
  `full-parallel-misc` only when each of its tests is safe beside every test of that bucket.

## Where work happens

Increments are built in Claude Code cloud sessions on the `onedrive-go` cloud environment: an
ephemeral Linux container running as root, GitHub reached through `gh`'s REST API (GraphQL is
refused). The reasons and the environment's setup script are in
[cloud-development.md](spec/reference/cloud-development.md).

- CI is the verification. Go tests run as the non-root `dev` user: as root on Linux, `devtool
  verify` runs them as `dev` whenever that user exists (`--as-user` names another).
- No `.env`, `.testdata` or token file ever exists in a session; nothing worth keeping lives only
  in the container.
- Builder subagents work in harness worktrees under `.claude/worktrees`, which start from
  `origin/main`: a builder whose work needs unmerged commits cherry-picks them first (name the
  SHAs in its brief). Integrate a builder's commits as they are (cherry-pick), so
  `go run ./cmd/devtool worktree prune-merged` can prove them merged; post-merge runs it too.
- You own the environment's fitness. At session start check it against
  [cloud-development.md](spec/reference/cloud-development.md): the `onedrive-go` environment, the
  pinned tools (golangci-lint, gofumpt, goimports) on `PATH`, the `dev` user, a deep enough
  clone. Repair the container, save the fix with the environment tools (each approved by the
  maintainer), and say in the report what was wrong.
- On request, and never by default, a session on a machine where the client is installed and
  signed in may read that client's logs and test it by hand. It never builds over, reinstalls or
  reconfigures that installation, and live checks there touch only what the person running the
  session names.

### Maintainer setup

The live-test credentials are bootstrapped once, by the maintainer, outside any session (it needs
a browser), after which CI keeps them fresh by writing rotated tokens back to Key Vault:

1. `./scripts/bootstrap-test-credentials.sh` once per test account: an OAuth sign-in that writes
   `.testdata/` (token files, `catalog.json`, `config.toml`); repeated runs add accounts.
2. `./scripts/migrate-test-data-to-ci.sh` (signed in with `az login`): uploads `.testdata/` to
   the CI Key Vault.

CI reaches that vault keylessly: the live jobs run in the `live` GitHub environment (variables
`AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_KEY_VAULT`), the Entra app trusts only that
environment's immutable OIDC subject, and `scripts/ci-keyvault.sh` exchanges the job's token with
curl to read the test data and write rotated tokens back.

## Git, CI and PRs

- One branch per increment, cut from a fresh `origin/main`, named `feat/`, `fix/`, `docs/`,
  `chore/`, `refactor/` or `perf/` plus a short topic. Never reuse a branch after its PR merged;
  never name a branch for the tool that wrote it.
- Every change lands through a PR whose body fills `.github/pull_request_template.md`. CI checks
  the body, and that a PR titled `fix...` changes a test.
- `CI gate` is the one required check, in the `main` ruleset: a job that needs every job a PR or
  a push runs. Wait for it (the pre-merge stage), and fix every failure; never bypass it.
- Squash-merge as soon as `CI gate` is green, without asking: the post-merge stage does it over
  REST (`PUT repos/tonimelisma/onedrive-go/pulls/<N>/merge` pinned to the head); locally
  `gh pr merge --auto --squash` also works.
- Merging to main releases; nobody tags or publishes by hand.
- The cloud sessions' GitHub proxy refuses tag pushes, branch deletion and GraphQL: use the REST
  API (`gh api`), and leave branch deletion to GitHub's delete-on-merge and the branch-cleanup
  workflow.
- Commit messages: an imperative subject saying what changed; the body says why.
- Releases: when the push-to-`main` CI run of a commit succeeds, `.github/workflows/release.yml`
  tags it with the next patch of the series in `VERSION` (`MAJOR.MINOR`; a new series is a PR
  that edits that line, starting at `.0`), builds the static binaries with the tag stamped in, smoke-tests
  the Linux one, attests them, publishes the release with `SHA256SUMS`, and verifies the published
  files as a consumer does (`gh attestation verify --signer-workflow
  tonimelisma/onedrive-go/.github/workflows/release.yml --source-ref refs/heads/main`). The path
  of that workflow and its `main` ref are the signing identity consumers check: never change
  either without moving every consumer's policy first. Versioning, the build and verification are
  in [system.md](spec/design/system.md) and [CONTRIBUTING.md](CONTRIBUTING.md), Releases.

## Privacy review

Before every commit, read your own staged diff (`git diff --cached`) and remove anything private
about the maintainer, their family or their computers: people's names, addresses, personal email
addresses and accounts, hostnames, home-directory paths, IP addresses, tenant, client or vault
IDs, real personal documents or photos. Use neutral stand-ins in tests and examples. There is no
list of private terms to check against: judge the diff.

## Definition of Done

An increment is done when every line holds; the final report marks each ✅ or ❌ with evidence:

- [ ] Start stage clean: main CI, the last scheduled run and recent review threads were green or
      handled first; every nightly failure claimed was fixed, released or found held elsewhere.
- [ ] Tests first; the regression test's red output before the fix is quoted.
- [ ] Format, lint, build and the short unit tier green in the pre-pr stage, on a branch that
      contains the latest `origin/main`; CI green on the final commit, fast e2e included.
- [ ] Coverage and the exhaustive tier: run in-session with `verify public` on a host that can
      finish it, or left to the nightly and said so.
- [ ] Repository rules green (requirements, `GOVERNS:`, routing table, doc links) and the PR
      body check green.
- [ ] Docs and CLAUDE.md are updated in the same PR; no stale reference to a deleted package,
      file or boundary remains.
- [ ] Instrumentation reviewed on touched paths: what was added, what removed, or why it is
      adequate.
- [ ] Privacy review done on every commit's staged diff.
- [ ] Every review thread is answered or resolved (pre-merge).
- [ ] Merged by the post-merge stage; main CI green; the release it produced is linked.
- [ ] Clean up: only main and open PRs' branches remain on the remote; builder worktrees whose
      work is on `main` are pruned (any kept one is explained); the working tree is clean.

### Final increment report

One PR comment after the merge, with these parts:

1. **Done / not done**: what the increment delivered, and anything left out.
2. **Definition of Ready**: as written, and whether it held or was refreshed.
3. **Requirements and docs**: IDs added, changed or retired; design and reference docs touched.
4. **Design decisions**: each with the alternatives considered and why they lost; every
   deviation from the plan and whether it is the long-term answer.
5. **Verification evidence**: commands, CI run links and durations, release link.
6. **Regression test**: name and red output before the fix (or "no bug fixed").
7. **Diagnosis**: for a bug, the root cause and how it was established.
8. **Review threads and nightly**: current and carried-over threads and how each was handled;
   live-incident entries added or updated; nightly failures claimed and fixed, released, or held
   elsewhere.
9. **Instrumentation**: added, removed, and gaps still open.
10. **Performance**: before and after numbers, or "not touched".
11. **Privacy review**: done on every commit; anything removed.
12. **Smells**: what looked wrong but was out of scope, without holding back.
13. **Follow-ups**: each filed as an issue, linked.
14. **Definition of Done**: the checklist above, each line ✅ or ❌ with evidence.
