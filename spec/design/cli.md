# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-2.3.3 [verified], R-2.8.3 [verified], R-2.9 [verified], R-2.10.4 [verified], R-2.10.47 [verified], R-6.6.11 [verified]

## Overview

`main.go` is a thin entrypoint. `internal/cli` owns:

- Cobra command wiring
- CLI bootstrap and shared flags
- user-facing rendering
- control-socket client logic
- dependency composition for Graph-facing command runtimes

The CLI is a presentation and composition boundary, not a second sync policy
engine.

Durable account and drive inventory mutation is not CLI-owned. Commands may
discover account/drive facts, delete token files, and invoke config entrypoints
that edit `config.toml`, but `internal/config` owns how `catalog.json` records
are inserted, updated, pruned, and validated.

## Ownership Contract

- Owns: command grammar, output shaping, reason/action copy, control-socket client routing, and command-scoped runtime assembly
- Does Not Own: Graph wire semantics, sync planning/execution policy, SQLite schema rules, or durable catalog lifecycle mutation rules
- Source of Truth: parsed args/flags plus lower-layer domain results
- Allowed Side Effects: stdout/stderr output, log-file setup, control-socket RPCs, and ordinary command-layer calls into lower packages
- Mutable Runtime Owner: `CLIContext` owns command-scoped mutable state for one invocation, including logger replacement, status writer error state, and test seams. Long-lived daemon runtime state remains owned by `internal/multisync` and `internal/sync`.
- Error Boundary: CLI commands translate lower-layer failures into user-facing guidance and command exit errors, but transport, store, and engine packages remain responsible for their own domain error semantics.

## Verified By

| Behavior | Evidence |
| --- | --- |
| `status` stays read-only, renders sync-state snapshots, and surfaces auth/issue state without reviving manual conflict or delete-approval UI. | `TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON`, `TestQuerySyncState_UsesReadOnlyProjectionHelper`, `TestStatusCommand_JSONSurfacesSyncAuthRejectedOffline` |
| `pause` and `resume` remain CLI-owned config mutations rather than direct sync-store writes. | `TestPauseCommand_PersistsTimedPause`, `TestResumeCommand_ClearsPausedKeys`, `TestClearPausedKeys_RemovesBothKeys` |
| Watch and one-shot sync command wiring stays inside the CLI composition boundary and delegates runtime ownership to the sync daemon/orchestrator seam. | `TestRunSyncCommand_UsesConfigDryRunWhenFlagUnset`, `TestRunSyncCommand_WatchRejectsEffectiveDryRun`, `TestRunSyncWatch_UsesInjectedRunner`, `TestRunSyncDaemonWithFactory_CallsOrchestrator` |

## Command Surface

| Command family | Purpose |
| --- | --- |
| `login`, `logout` | auth and account session lifecycle |
| `ls`, `get`, `put`, `rm`, `mkdir`, `mv`, `cp`, `stat` | file operations |
| `drive` | drive management |
| `shared*` | shared-item discovery and add flows |
| `sync`, `pause`, `resume` | sync control |
| `status` | read-only account and sync health |
| `perf` | live owner perf view and capture |
| `recover` | state-store repair/rebuild/reset flow |
| `recycle-bin` | recycle-bin operations |

There is no `resolve` command family anymore.

## Status And Read-Only Sync State

`status` is intentionally read-only and account-centric.

- account and drive identity come from the validated config+catalog snapshot
- sync-state snapshots come from store-owned read helpers
- live authenticated account identity and drive catalog overlays come from
  bounded Graph proof/discovery owned by the command
- live perf comes from the active owner over the control socket when requested

There is no separate history-only surface for resolved conflicts, and status no
longer exposes delete-safety or manual conflict-request sections.

## Control Socket

The control socket is JSON-over-HTTP over a Unix domain socket. The CLI uses
it for:

- owner/status probing
- `status --perf`
- `perf capture`
- watch-owner `reload`
- watch-owner `stop`

One-shot owners expose status/perf and reject live control mutations. Watch
owners expose the remaining daemon controls above.

The socket is no longer a sync-decision submission surface because the
architecture no longer has manual conflict or delete-approval workflows.

## Pause / Resume

`pause` and `resume` remain config mutations owned by the CLI. After writing
config, the CLI notifies a running watch owner to reload when possible. If no
watch owner is running, the updated config takes effect on the next start.

## Recover

`recover` is the supported repair surface for sync state DB problems. It is
explicit, confirmation-gated, and store-owned. The CLI owns only the user
interaction and final reporting.

## What The CLI No Longer Owns

The CLI no longer owns:

- durable conflict-resolution requests
- held-delete approvals
- fallback direct DB writes for sync decisions
- `resolve` subcommands

Manual sync decisions were removed from the current product surface.
