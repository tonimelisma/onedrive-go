# CLI

GOVERNS: main.go, root.go, httpclient.go, format.go, signal.go, pidfile.go, auth.go, ls.go, rm.go, mkdir.go, mv.go, cp.go, stat.go, pause.go, resume.go, recycle_bin.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-3.1 [verified], R-4.7 [verified], R-4.8.4 [verified], R-1.9 [verified], R-1.2.4 [verified], R-1.3.4 [verified], R-1.4.3 [verified], R-1.5.1 [verified], R-1.6.1 [verified], R-1.7.1 [verified], R-1.8.1 [verified], R-1.9.4 [verified], R-3.1.6 [verified], R-3.3.10 [verified], R-3.3.11 [verified], R-2.3.10 [verified], R-2.7.1 [verified], R-2.3.7 [planned], R-2.3.8 [planned], R-2.3.9 [planned], R-6.6.11 [planned]

## Overview

Cobra CLI with Unix-style verbs. Root command (`root.go`) handles global flags (`--config`, `--drive`, `--verbose`, `--quiet`, `--debug`, `--json`), config loading via `PersistentPreRunE`, and drive resolution.

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
| `sync` | `sync.go` | Sync (see [sync-engine.md](sync-engine.md)) |
| `pause`, `resume` | `pause.go`, `resume.go` | Pause/resume sync. `resume` also cleans up stale config keys from expired timed pauses (paused=true + past paused_until). |
| `status` | `status.go` | Display account/drive status |
| `issues` | `issues.go` | Conflict and failure management (grouped display, per-scope sub-grouping) |
| `verify` | `verify.go` | Post-sync verification |
| `drive` | `drive.go` | Drive management (list/add/remove/search) |
| `recycle-bin` | `recycle_bin.go` | Recycle bin operations (list/restore/empty) |

## Logout Lifecycle and Whoami Logged-Out Display

Implements: R-3.1.3 [verified], R-3.1.4 [verified], R-3.1.5 [verified]

Logout proceeds in two stages. A plain `logout` removes the OAuth token and config sections but preserves state databases, account profiles, and drive metadata — enabling re-authentication without a full re-sync. A subsequent `logout --purge` removes all remaining data files.

**Orphan detection**: After a plain logout, the account is no longer in config but its `account_*.json` profile file remains on disk. `whoami` discovers these orphaned profiles via `config.DiscoverAccountProfiles()`, filters out accounts still in config, and checks each for a missing token file. Logged-out accounts are displayed in both text and JSON output with display name, drive type, and state DB count.

**Purge after prior logout**: `resolveLogoutAccount()` accepts `purge` and `logger` parameters. When config has zero accounts, it falls back to `discoverOrphanedEmails()` which scans account profile files. With `--purge`, it auto-selects a single orphan or requires `--account` for multiple. `executeLogout()` calls `purgeOrphanedFiles()` which removes state DBs (via `DiscoverStateDBsForEmail`), drive metadata (via `DiscoverDriveMetadataForEmail`), and account profiles for both personal and business CID variants.

## Output Formatting (`format.go`)

Two modes: human-readable (default) and JSON (`--json`). Human output to stderr, structured data to stdout.

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
| `drive search` | `printDriveSearchJSON` | `driveSearchResult` |
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
- `SessionProvider` caches `TokenSource`s by token file path — multiple drives sharing an account share one `TokenSource`, preventing OAuth2 refresh token rotation races.
- CLI handlers use `cmd.Context()` for signal propagation. Exception: upload session cancel paths use `context.Background()` because the cancel must succeed even when the original context is done.
- The status command uses a testable service layer with narrowed interfaces (`accountMetaReader`, `tokenStateChecker`, `syncStateQuerier`), decoupling status aggregation from Cobra wiring.
- Informational commands (`drive list`, `status`, `whoami`) use lenient config loading (`LoadOrDefaultLenient`) that collects validation errors as warnings instead of failing. This allows users to inspect their configuration and see drive status even when config has errors. Each of these commands (and `drive search`) must have `skipConfigAnnotation` on the leaf Cobra command — not just the parent — because Cobra checks annotations on the executing command, not parent commands. Safety net: `TestAnnotationTreeWalk` walks the entire command tree and fails if any leaf command with `RunE` is not explicitly classified as either a data command (no annotation) or an annotated command. New commands must be added to the `dataCommands` set or given the annotation. [verified]
- `loadAndResolve` passes errors from `ResolveDrive` unwrapped. `ResolveDrive` already wraps `LoadOrDefault` errors with `"loading config: "`, and `MatchDrive` errors are user-facing messages that read better without a prefix (e.g., `"no drives configured — ..."` instead of `"loading config: no drives configured — ..."`). [verified]
- Extract `multiHandler` from `root.go` to `internal/slogutil/` if logging grows (structured error reporting, log sampling). [planned]

## Planned: Issues Display Enhancements

Implements: R-2.3.7 [planned], R-2.3.8 [planned], R-2.3.9 [planned], R-6.6.11 [planned]

- **Grouped display**: >10 failures of same `issue_type` → single heading with count, first 5 paths shown. `--verbose` shows all paths.
- **Per-scope sub-grouping**: 507 quota and 403 permissions grouped by scope (own drive vs each shortcut). Different scopes = different owners = different user actions.
- **Human-readable names**: Shortcut-scoped failures display local path name, not internal drive IDs.
- **Per-error-type user action text**: Every failure includes plain-language reason + concrete user action. Scope-owner-specific variants: "Your OneDrive storage is full" (own drive) vs "Shared folder '{name}' owner's storage is full" (shortcut).
- Root package unit test coverage target: 60%+ (currently ~47%). CLI `RunE` handlers need interface-based mock injection. [planned]
