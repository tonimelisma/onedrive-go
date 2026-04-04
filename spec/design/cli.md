# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-3.1 [verified], R-4.7 [verified], R-4.8.4 [verified], R-1.9 [verified], R-1.2.4 [verified], R-1.2.5 [verified], R-1.3.4 [verified], R-1.3.5 [verified], R-1.4.3 [verified], R-1.5.1 [verified], R-1.6.1 [verified], R-1.6.2 [verified], R-1.7.1 [verified], R-1.8.1 [verified], R-1.9.4 [verified], R-3.1.6 [verified], R-3.3.10 [verified], R-3.3.11 [verified], R-3.3.12 [verified], R-3.6.6 [verified], R-3.6.7 [verified], R-2.3.10 [verified], R-2.3.11 [verified], R-2.3.12 [verified], R-2.7.1 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.10.4 [verified], R-2.10.47 [verified], R-2.14.3 [verified], R-2.14.5 [verified], R-6.6.11 [verified], R-6.8.16 [verified], R-6.10.6 [verified]

## Overview

The root package is a thin process entrypoint. The Cobra command tree, CLI bootstrap, output formatting, and command handlers live in `internal/cli`.

`internal/cli/root.go` handles global flags (`--config`, `--drive`, `--verbose`, `--quiet`, `--debug`, `--json`), config loading via `PersistentPreRunE`, and single-drive resolution. Cobra `RunE` functions are intentionally thin: they parse command-local flags and delegate to service-layer collaborators (`authService`, `driveService`, `issuesService`, `statusService`, `syncControlService`, `recycleBinService`, `syncService`, `verifyService`). `internal/cli/sync.go` stays in the same package because multi-drive sync reuses the same Phase 1 CLI context but performs its own multi-drive resolution.
Sync-specific report rendering and watch bootstrap helpers stay in the same package, but they live outside the Cobra wiring file so `internal/cli/sync.go` remains focused on command construction, flag parsing, and multi-drive resolution.

`CLIContext` owns two distinct output boundaries:

- `OutputWriter`: primary command output (default `stdout`)
- `StatusWriter`: progress/status output (default `stderr`)

This keeps human/JSON command results separate from progress/status messages and avoids direct process-global stdout/stderr writes inside command families and CLI bootstrap paths.

## Ownership Contract

- Owns: Cobra command wiring, CLI bootstrap, signal and PID-file lifecycle, output formatting, and user-facing reason/action text for failures and issues.
- Does Not Own: Graph wire behavior, config semantics, sync planning/execution policy, or durable sync-state mutation rules.
- Source of Truth: `CLIContext`, parsed flags/args, and the domain results returned by lower layers.
- Allowed Side Effects: stdout/stderr output, log-file creation, PID-file lifecycle, signal handling, and calls into service packages.
- Mutable Runtime Owner: Each command invocation owns its own runtime state. `internal/cli` keeps no package-level mutable state and relies on lower layers for long-lived watch ownership.
- Error Boundary: The CLI turns lower-layer results into exit behavior plus human-readable reason/action text. It consumes the domain model from [error-model.md](error-model.md) instead of inventing a second classification scheme.

## Command Structure

Implements: R-6.2.8 [verified]

| Command | File | Description |
|---------|------|-------------|
| `login`, `logout`, `whoami` | `auth.go` | Authentication (device code, PKCE, token management) |
| `ls` | `ls.go` | List remote files |
| `get` | `get.go` | Download files (via `driveops.TransferManager`, with disk space pre-check) |
| `put` | `put.go` | Upload files (via `driveops.TransferManager`) |
| `rm` | `rm.go` | Delete remote items (recycle bin by default; `--permanent` bypasses the recycle bin) |
| `mkdir` | `mkdir.go` | Create remote folders |
| `mv` | `mv.go` | Server-side move/rename |
| `cp` | `cp.go` | Server-side async copy with polling |
| `stat` | `stat.go` | Display item metadata |
| `sync` | `sync.go` | Multi-drive sync command (see [sync-control-plane.md](sync-control-plane.md)) |
| `pause`, `resume` | `pause.go`, `resume.go` | Pause/resume sync through `syncControlService`. `resume` also cleans up stale config keys from expired timed pauses (paused=true + past paused_until). |
| `status` | `status.go` | Display account/drive status via `statusService` and read-only `syncstore.Inspector` snapshots |
| `issues` | `issues.go` | Conflict and failure management. All subcommands delegate to `issuesService`; `issues list` renders read-only `syncstore.Inspector` snapshots and mutating subcommands call service-owned mutation flows over the writable store/engine path. |
| `verify` | `verify.go` | Post-sync verification |
| `drive` | `drive.go` | Drive management (list/add/remove/search) |
| `recycle-bin` | `recycle_bin.go` | Recycle bin operations (list/restore/empty) via `recycleBinService` |

## Auth Lifecycle, Auth Health, And Proof Boundaries

Implements: R-3.1.3 [verified], R-3.1.4 [verified], R-3.1.5 [verified], R-3.1.6 [verified], R-2.10.47 [verified]

The CLI presents one user-facing umbrella state for auth problems:
`Authentication required`. Internally it keeps two sources of truth separate on
purpose:

- token and account-profile discovery own "missing or invalid saved login"
- `scope_blocks.auth:account` owns "sync proved that OneDrive rejected the saved login"

`status` and `issues` are intentionally offline read models. They use local
token/account-profile state plus persisted sync state to show best-known auth
health, but they never probe Graph and never mutate `auth:account`.
Offline auth-scope detection now goes through the read-only
`syncstore.Inspector` boundary as an exact `auth:account` scope-block query;
CLI auth-health helpers do not open the mutable `SyncStore` path just to read
persisted auth state.

`internal/authstate` is the leaf package that owns the shared auth-health
vocabulary and copy:

- states: `ready`, `authentication_required`
- reasons: `missing_login`, `invalid_saved_login`, `sync_auth_rejected`
- shared title/reason/action text for CLI auth surfaces and `IssueUnauthorized`

`accountReadModelService` builds one offline account catalog from configured drives,
discovered account profiles, discovered token files, and persisted
`auth:account` scope presence. `status`, `whoami`, `drive list`, and
`drive search` all read from that same catalog so account discovery and auth
projection stay consistent across surfaces.

`whoami`, `drive list`, `drive search`, and ordinary single-drive file commands
are proof surfaces because they already perform authenticated Graph requests.
The first successful authenticated Graph response for an account clears stale
`auth:account` scope blocks across that account's state databases. This uses a
CLI-owned authenticated-success hook installed on live `graph.Client` and
`driveops.Session` instances. Pre-authenticated upload and download URLs
intentionally bypass that hook and do not count as proof.

`login` and plain `logout` are explicit auth-boundary transitions. Successful
`login` clears stale `auth:account` scope blocks for the account. Plain
`logout` clears those scope blocks from preserved state databases before
removing token/config state. `logout --purge` removes the state databases
entirely.

`whoami` no longer has a special "logged out accounts" concept. It reports
`accounts_requiring_auth`, which may come from:

- missing saved login
- invalid saved login on disk
- persisted sync-time `auth:account` rejection

After a plain logout, the account is no longer in config but its
`account_*.json` profile file remains on disk. `whoami` still discovers these
orphaned profiles via `config.DiscoverAccountProfiles()`, but it renders them
through the shared auth-required model instead of a dedicated logged-out bucket.

With `--purge`, `resolveLogoutAccount()` falls back to
`discoverOrphanedEmails()` when config has zero accounts. It auto-selects a
single orphan or requires `--account` for multiple. `executeLogout()` then
calls `purgeOrphanedFiles()`, which removes state DBs (via
`DiscoverStateDBsForEmail`), drive metadata (via
`DiscoverDriveMetadataForEmail`), and account profiles for both personal and
business CID variants.

## Output Formatting (`format.go`)

Two modes: human-readable (default) and JSON (`--json`). Primary command output goes through `OutputWriter` (default `stdout`), while progress/status/warning text goes through `StatusWriter` (default `stderr`).

All commands with `--json` support use extracted `printXxxJSON(w io.Writer, out T) error` functions that encode via `json.NewEncoder(w)` with 2-space indent. This enables unit testing via `bytes.Buffer` roundtrip without CLI wiring.

Timestamp presentation follows one rule across command families: zero `time.Time` means "unknown". Human-readable output renders that as `unknown`; JSON output emits an empty string instead of a fabricated RFC3339 value or Go's year-0001 zero timestamp.

`verify` keeps one stable report shape across both output modes:
`verified` plus `mismatches[] { path, status, expected, actual }`. The
underlying mismatch slice is already sorted by path before CLI formatting, so
human-readable tables and JSON output stay deterministic. When mismatches are
found, `verifyService` returns the sentinel `errVerifyMismatch`; the root error
boundary translates that into exit code `1` without printing the generic
`Error:` prefix that ordinary command failures use.

| Command | JSON function(s) | Schema type(s) |
|---------|------------------|----------------|
| `ls` | `printItemsJSON` | `lsJSONItem` |
| `get` | `printGetJSON`, `printGetFolderJSON` | `getJSONOutput`, `getFolderJSONOutput` |
| `put` | `printPutJSON`, `printPutFolderJSON` | `putJSONOutput`, `putFolderJSONOutput` |
| `rm` | `printRmJSON` | `rmJSONOutput` |
| `mkdir` | `printMkdirJSON` | `mkdirJSONOutput` |
| `stat` | `printStatJSON` | `statJSONOutput` |
| `mv` | `printMvJSON` | `mvJSONOutput` |
| `cp` | `printCpJSON` | `cpJSONOutput` |
| `recycle-bin list` | `formatRecycleBinJSON` | `recycleBinJSONItem` |
| `recycle-bin restore` | `printRecycleBinRestoreJSON` | `recycleBinJSONItem` |
| `whoami` | `printWhoamiJSON` | `whoamiOutput` |
| `drive list` | `printDriveListJSON` | `driveListJSONOutput` |
| `drive search` | `printDriveSearchJSON` | `driveSearchJSONOutput` |
| `issues` | `printIssuesJSON` | `issueJSON` |
| `verify` | `printVerifyJSON` | `sync.VerifyReport` |
| `status` | `printStatusJSON` | `statusOutput` |

## Signal Handling (`signal.go`)

Two-signal shutdown for watch mode:

- first SIGINT/SIGTERM = cancel the shared shutdown context and let the engine drain
- second SIGINT/SIGTERM = force process exit with status 1

`sighupChannel()` is separate because config reload is a control-plane signal,
not a shutdown request.

## PID File (`pidfile.go`)

Implements: R-6.3.3 [verified]

Single-instance enforcement via advisory file lock. Created at daemon start, removed on shutdown. Stale PID files detected and cleaned up.

## Logging (`internal/logfile/logfile.go`)

Implements: R-6.6.1 [verified], R-6.6.2 [verified], R-6.6.3 [verified], R-6.6.4 [verified]

Log file creation with parent directory auto-creation. Append mode. Retention-based rotation (`log_retention_days`).

## Design Constraints

- Config flows through Cobra context (`CLIContext` stored via `context.WithValue` with unexported key type). No global flag variables.
- Two-phase `PersistentPreRunE`: Phase 1 (all commands) reads flags + creates logger. Phase 2 (data commands only) loads config + resolves drive. Commands skip Phase 2 via `skipConfigAnnotation` in `Annotations`.
- Command handlers are wiring only. Command-family services own the runtime behavior:
  - `authService`: login/logout/whoami flows
  - `driveService`: drive list/add/remove/search flows
  - `issuesService`: issue listing and failure/conflict mutations
  - `statusService`: account/drive status aggregation, lenient config warning handling, offline auth-health projection, and read-only sync-state inspection
  - `syncControlService`: pause/resume config mutation flows
  - `recycleBinService`: recycle-bin list/restore/empty flows
  - `syncService`: multi-drive sync command assembly
  - `verifyService`: baseline verification flow
- `accountReadModelService` is the shared read-model collaborator under `statusService`, `authService`, and `driveService`. It owns lenient config loading, warning logging, and offline account/auth projection so those command families do not rebuild account semantics independently.
- `SessionProvider` caches `TokenSource`s by token file path — multiple drives sharing an account share one `TokenSource`, preventing OAuth2 refresh token rotation races.
- CLI handlers use `cmd.Context()` for signal propagation. Exception: upload session cancel paths use `context.Background()` because the cancel must succeed even when the original context is done.
- Production command code writes primary output through `CLIContext.OutputWriter`, and status/warning/error text through the CLI-owned status writer boundary instead of raw process-global stdout/stderr. This keeps command output injectable in tests and prevents hidden process-global output dependencies from creeping back into services, bootstrap warnings, or sync-daemon cleanup.
- `go run ./cmd/devtool verify` enforces that production `internal/cli` files do not call `fmt.Print*`, `fmt.Fprint*(os.Stdout|os.Stderr, ...)`, or direct `os.Stdout` / `os.Stderr` writer methods. The only allowed process-global output boundary is the tiny process entrypoint outside `internal/cli`.
- Browser auth URLs are validated against loopback or Microsoft auth hosts before launching the platform browser command. Validation and launch failures must not echo the full auth URL or any query tokens. The remaining inline `gosec` suppression on the `exec.CommandContext` call is intentional: the command comes from a fixed allowlist, but the linter cannot prove that through the helper boundary.
- PID file and log file opens use root-based trusted-path helpers once the CLI/config layer has resolved the target path.
- If the configured log file cannot be opened, CLI bootstrap warns through the CLI status writer and falls back to console-only logging instead of failing the command before any user-facing work can run.
- Direct `runSync` and service-level tests cover caller-visible failure paths such as config-load errors, all-drives-paused/no-drives guidance, and log-file-open fallback warnings through the injected status/output writers rather than process-global stderr assumptions.
- The status command uses a testable service layer with narrowed interfaces (`accountMetaReader`, `accountAuthChecker`, `syncStateQuerier`), decoupling status aggregation from Cobra wiring. The concrete state reader is `syncstore.Inspector`; CLI code no longer opens SQLite directly.
- `issuesService.runList` also uses `syncstore.Inspector`. CLI formatting code
  is no longer the owner of issue grouping, pending-retry aggregation, or
  scope labeling semantics.
- Offline auth-health projection also uses `syncstore.Inspector` for persisted
  `auth:account` checks, so read-only CLI account discovery no longer pays the
  writable-store checkpoint/close path.
- Sync-domain issue/status presentation uses the shared
  [`synctypes.SummaryKey`](/Users/tonimelisma/Development/onedrive-go-shared-failure-summaries/internal/synctypes/summary_keys.go)
  contract. `issues` groups persisted failures by normalized summary key plus
  humanized scope, while `status` consumes the store-owned `IssueSummary`
  projection instead of rebuilding visible-issue math locally.
- `status` now preserves the store-owned issue-group projection instead of
  flattening it to totals. Per-drive sync state includes grouped visible issue
  families with `summary_key`, `count`, `scope_kind`, and optional humanized
  `scope` in JSON, while text output renders the same groups under each drive's
  sync-state section before the aggregate retry counters. This is the CLI-side
  contract for `R-2.10.4`.
- Informational commands (`drive list`, `status`, `whoami`) use lenient config loading (`LoadOrDefaultLenient`) that collects validation errors as warnings instead of failing. This allows users to inspect their configuration and see drive status even when config has errors. Each of these commands (and `drive search`) must have `skipConfigAnnotation` on the leaf Cobra command — not just the parent — because Cobra checks annotations on the executing command, not parent commands. Safety net: `TestAnnotationTreeWalk` walks the entire command tree and fails if any leaf command with `RunE` is not explicitly classified as either a data command (no annotation) or an annotated command. New commands must be added to the `dataCommands` set or given the annotation. [verified]
- `loadAndResolve` passes errors from `ResolveDrive` unwrapped. `ResolveDrive` already wraps `LoadOrDefault` errors with `"loading config: "`, and `MatchDrive` errors are user-facing messages that read better without a prefix (e.g., `"no drives configured — ..."` instead of `"loading config: no drives configured — ..."`). [verified]
- CLI presentation is the final error boundary. `classifyCommandError` and `commandFailurePresentationForClass` map the domain classes from [error-model.md](error-model.md) to process exit behavior and user-facing reason/action text, while `authErrorMessage` specializes the user-facing auth copy for saved-login failures without re-inspecting raw transport payloads.
- The root error boundary maps both `graph.ErrNotLoggedIn` and `graph.ErrUnauthorized` into the same user-facing family, `Authentication required`, with cause-specific detail and a shared remediation path (`onedrive-go login`).
- Offline auth surfaces (`status`, `issues`) never mutate `auth:account`. Live proof surfaces clear `auth:account` only after an authenticated Graph response succeeds. Pre-authenticated upload/download URL success is not treated as auth proof.
- The authenticated-success proof recorder logs one attributed scope-repair event per account and proof source (`whoami`, `drive-list`, `drive-search`, `drive-session`) when it clears stale `auth:account` blocks. It does not log every successful request and does not create a persistent audit trail.
- Checked-in golden tests lock the human and JSON output shapes for `status` and `issues list`. Formatting changes are intentional and use the standard `-update` flow in `internal/cli/golden_test.go`.
- Extract `multiHandler` from `internal/cli/root.go` to `internal/slogutil/` if logging grows (structured error reporting, log sampling). [planned]

## Issues Display

Implements: R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.3.12 [verified], R-2.14.3 [verified], R-2.14.5 [verified], R-6.6.11 [verified]

- **Grouped display**: >10 failures of same `issue_type` → single heading with count, first 5 paths shown. `--verbose` shows all paths.
- **Per-scope sub-grouping**: 507 quota and shared-folder write blocks are grouped by scope (own drive vs each shortcut). Different scopes = different owners = different user actions.
- **Human-readable names**: Shortcut-scoped failures display local path name, not internal drive IDs.
- **Scope-aware reason/action copy**: Failure text is selected from `issue_type` plus the raw scope key, so shortcut-scoped quota failures say the shared-folder owner is out of space instead of implying the user's own drive is full.
- **JSON shape**: `issues --json` emits separate `conflicts`, `failure_groups`, and `held_deletes` arrays instead of a heterogeneous mixed list. This keeps grouped failure metadata and held-delete history stable for machine readers.
- **Derived shared-folder issues**: `perm:remote` is displayed from held blocked-write rows, not from a standalone boundary issue. The CLI shows one visible issue per denied boundary only while blocked write intent still exists.
- **Retry semantics**: `issues retry` on shared-folder write blocks is path-specific manual trial. Retrying the boundary name is rejected; the user must retry one blocked child path.
- **Shared summary descriptors**: Every sync issue renders from the shared `SummaryKey` descriptor table, with the humanized scope shown separately. This keeps sync logs, `status`, and `issues` grouped by the same normalized issue family without duplicating display taxonomies in each layer.
- **Store-owned read model**: `issues list` renders `IssuesSnapshot` from
  `syncstore.Inspector`; the CLI does not rebuild groups from raw SQL rows.
  This keeps the visible `issues` surface aligned with the same store-owned
  semantics that feed `status`.
- **Auth scope display**: `auth:account` renders as an account-level `Authentication required` issue with no path list.
- **Replay-safe mutations**: `issues clear`, `issues retry`, and repeated
  `issues resolve` calls are replay-safe. Repeating a cleared failure or an
  already-resolved conflict returns a stable no-op/already-resolved result
  instead of duplicating store mutations or partially releasing a scope.
- `internal/cli` unit test coverage target: 60%+ (currently ~53.7%). The service split and output-writer injection are in place, but more direct `RunE`/service black-box coverage is still needed to reach the target. [planned]
