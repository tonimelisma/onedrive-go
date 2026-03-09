# CLI

GOVERNS: main.go, root.go, format.go, signal.go, pidfile.go, auth.go, ls.go, rm.go, mkdir.go, mv.go, cp.go, stat.go, pause.go, resume.go, recycle_bin.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-3.1 [implemented], R-4.7 [implemented], R-1.9 [implemented]

## Overview

Cobra CLI with Unix-style verbs. Root command (`root.go`) handles global flags (`--config`, `--drive`, `--verbose`, `--quiet`, `--debug`, `--json`), config loading via `PersistentPreRunE`, and drive resolution.

## Command Structure

| Command | File | Description |
|---------|------|-------------|
| `login`, `logout`, `whoami` | `auth.go` | Authentication (device code, PKCE, token management) |
| `ls` | `ls.go` | List remote files |
| `get` | `get.go` | Download files (via `driveops.TransferManager`) |
| `put` | `put.go` | Upload files (via `driveops.TransferManager`) |
| `rm` | `rm.go` | Delete remote items (recycle bin by default) |
| `mkdir` | `mkdir.go` | Create remote folders |
| `mv` | `mv.go` | Server-side move/rename |
| `cp` | `cp.go` | Server-side async copy with polling |
| `stat` | `stat.go` | Display item metadata |
| `sync` | `sync.go` | Sync (see [sync-engine.md](sync-engine.md)) |
| `pause`, `resume` | `pause.go`, `resume.go` | Pause/resume sync |
| `status` | `status.go` | Display account/drive status |
| `issues` | `issues.go` | Conflict and failure management |
| `verify` | `verify.go` | Post-sync verification |
| `drive` | `drive.go` | Drive management (list/add/remove/search) |
| `recycle-bin` | `recycle_bin.go` | Recycle bin operations (list/restore/empty) |

## Output Formatting (`format.go`)

Two modes: human-readable (default) and JSON (`--json`). Human output to stderr, structured data to stdout.

## Signal Handling (`signal.go`)

Two-signal shutdown for watch mode: first SIGINT = drain current operations, second SIGINT = force exit.

## PID File (`pidfile.go`)

Single-instance enforcement via advisory file lock. Created at daemon start, removed on shutdown. Stale PID files detected and cleaned up.

## Logging (`internal/logfile/logfile.go`)

Log file creation with parent directory auto-creation. Append mode. Retention-based rotation (`log_retention_days`).

## Design Constraints

- Config flows through Cobra context (`CLIContext` stored via `context.WithValue` with unexported key type). No global flag variables.
- Two-phase `PersistentPreRunE`: Phase 1 (all commands) reads flags + creates logger. Phase 2 (data commands only) loads config + resolves drive. Commands skip Phase 2 via `skipConfigAnnotation` in `Annotations`.
- `SessionProvider` caches `TokenSource`s by token file path — multiple drives sharing an account share one `TokenSource`, preventing OAuth2 refresh token rotation races.
- CLI handlers use `cmd.Context()` for signal propagation. Exception: upload session cancel paths use `context.Background()` because the cancel must succeed even when the original context is done.
- The status command uses a testable service layer with narrowed interfaces (`accountMetaReader`, `tokenStateChecker`, `syncStateQuerier`), decoupling status aggregation from Cobra wiring.
