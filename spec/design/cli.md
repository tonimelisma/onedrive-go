# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-3.1 [verified], R-4.7 [verified], R-4.8.4 [verified], R-1.9 [verified], R-1.2.4 [verified], R-1.3.4 [verified], R-1.4.3 [verified], R-1.5.1 [verified], R-1.6.1 [verified], R-1.7.1 [verified], R-1.8.1 [verified], R-1.9.4 [verified], R-3.1.6 [verified], R-3.3.10 [verified], R-3.3.11 [verified], R-2.3.10 [verified], R-2.7.1 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.10.47 [verified], R-6.6.11 [verified], R-6.8.16 [verified], R-6.10.6 [verified]

## Overview

The root package is a thin process entrypoint. The Cobra command tree, CLI bootstrap, output formatting, and command handlers live in `internal/cli`.

`internal/cli/root.go` handles global flags (`--config`, `--drive`, `--verbose`, `--quiet`, `--debug`, `--json`), config loading via `PersistentPreRunE`, and single-drive resolution. Cobra `RunE` functions are intentionally thin: they parse command-local flags and delegate to service-layer collaborators (`authService`, `driveService`, `issuesService`, `statusService`, `syncService`, `verifyService`). Sync-specific report rendering and watch bootstrap helpers stay in the same package, but they live outside the Cobra wiring file so `internal/cli/sync.go` remains focused on command construction and flag parsing.

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
| `rm` | `rm.go` | Delete remote items (recycle bin by default) |
| `mkdir` | `mkdir.go` | Create remote folders |
| `mv` | `mv.go` | Server-side move/rename |
| `cp` | `cp.go` | Server-side async copy with polling |
| `stat` | `stat.go` | Display item metadata |
| `sync` | `sync.go` | Multi-drive sync command (see [sync-control-plane.md](sync-control-plane.md)) |
| `pause`, `resume` | `pause.go`, `resume.go` | Pause/resume sync. `resume` also cleans up stale config keys from expired timed pauses (paused=true + past paused_until). |
| `status` | `status.go` | Display account/drive status |
| `issues` | `issues.go` | Conflict and failure management (grouped display, per-scope sub-grouping) |
| `verify` | `verify.go` | Post-sync verification |
| `drive` | `drive.go` | Drive management (list/add/remove/search) |
| `recycle-bin` | `recycle_bin.go` | Recycle bin operations (list/restore/empty) |

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

Two-signal shutdown for watch mode: first SIGINT = drain current operations, second SIGINT = force exit.

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
  - `statusService`: account/drive status aggregation
  - `syncService`: multi-drive sync command assembly
  - `verifyService`: baseline verification flow
- `SessionProvider` caches `TokenSource`s by token file path — multiple drives sharing an account share one `TokenSource`, preventing OAuth2 refresh token rotation races.
- CLI handlers use `cmd.Context()` for signal propagation. Exception: upload session cancel paths use `context.Background()` because the cancel must succeed even when the original context is done.
- Production command code writes primary output through `CLIContext.OutputWriter`, and status/warning/error text through the CLI-owned status writer boundary instead of raw process-global stdout/stderr. This keeps command output injectable in tests and prevents hidden process-global output dependencies from creeping back into services, bootstrap warnings, or sync-daemon cleanup.
- `go run ./cmd/devtool verify` enforces that production `internal/cli` files do not call `fmt.Print*`, `fmt.Fprint*(os.Stdout|os.Stderr, ...)`, or direct `os.Stdout` / `os.Stderr` writer methods. The only allowed process-global output boundary is the tiny process entrypoint outside `internal/cli`.
- Browser auth URLs are validated against loopback or Microsoft auth hosts before launching the platform browser command. Validation and launch failures must not echo the full auth URL or any query tokens. The remaining inline `gosec` suppression on the `exec.CommandContext` call is intentional: the command comes from a fixed allowlist, but the linter cannot prove that through the helper boundary.
- PID file and log file opens use root-based trusted-path helpers once the CLI/config layer has resolved the target path.
- If the configured log file cannot be opened, CLI bootstrap warns through the CLI status writer and falls back to console-only logging instead of failing the command before any user-facing work can run.
- Direct `runSync` and service-level tests cover caller-visible failure paths such as config-load errors, all-drives-paused/no-drives guidance, and log-file-open fallback warnings through the injected status/output writers rather than process-global stderr assumptions.
- The status command uses a testable service layer with narrowed interfaces (`accountMetaReader`, `accountAuthChecker`, `syncStateQuerier`), decoupling status aggregation from Cobra wiring.
- Informational commands (`drive list`, `status`, `whoami`) use lenient config loading (`LoadOrDefaultLenient`) that collects validation errors as warnings instead of failing. This allows users to inspect their configuration and see drive status even when config has errors. Each of these commands (and `drive search`) must have `skipConfigAnnotation` on the leaf Cobra command — not just the parent — because Cobra checks annotations on the executing command, not parent commands. Safety net: `TestAnnotationTreeWalk` walks the entire command tree and fails if any leaf command with `RunE` is not explicitly classified as either a data command (no annotation) or an annotated command. New commands must be added to the `dataCommands` set or given the annotation. [verified]
- `loadAndResolve` passes errors from `ResolveDrive` unwrapped. `ResolveDrive` already wraps `LoadOrDefault` errors with `"loading config: "`, and `MatchDrive` errors are user-facing messages that read better without a prefix (e.g., `"no drives configured — ..."` instead of `"loading config: no drives configured — ..."`). [verified]
- CLI presentation is the final error boundary. It maps the domain classes from [error-model.md](error-model.md) to process exit behavior and user-facing reason/action text, but it does not inspect raw transport payloads directly.
- The root error boundary maps both `graph.ErrNotLoggedIn` and `graph.ErrUnauthorized` into the same user-facing family, `Authentication required`, with cause-specific detail and a shared remediation path (`onedrive-go login`).
- Offline auth surfaces (`status`, `issues`) never mutate `auth:account`. Live proof surfaces clear `auth:account` only after an authenticated Graph response succeeds. Pre-authenticated upload/download URL success is not treated as auth proof.
- Extract `multiHandler` from `internal/cli/root.go` to `internal/slogutil/` if logging grows (structured error reporting, log sampling). [planned]

## Issues Display

Implements: R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-6.6.11 [verified]

- **Grouped display**: >10 failures of same `issue_type` → single heading with count, first 5 paths shown. `--verbose` shows all paths.
- **Per-scope sub-grouping**: 507 quota and 403 permissions grouped by scope (own drive vs each shortcut). Different scopes = different owners = different user actions.
- **Human-readable names**: Shortcut-scoped failures display local path name, not internal drive IDs.
- **Per-error-type user action text**: Every failure includes plain-language reason + concrete user action. Scope-owner-specific variants: "Your OneDrive storage is full" (own drive) vs "Shared folder '{name}' owner's storage is full" (shortcut).
- **Auth scope display**: `auth:account` renders as an account-level `Authentication required` issue with no path list.
- `internal/cli` unit test coverage target: 60%+ (currently ~53.7%). The service split and output-writer injection are in place, but more direct `RunE`/service black-box coverage is still needed to reach the target. [planned]
