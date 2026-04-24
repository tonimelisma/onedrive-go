# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-2.3.3 [verified], R-2.5.5 [verified], R-2.5.6 [verified], R-2.8.3 [verified], R-2.9 [verified], R-2.10.4 [designed], R-2.10.47 [verified], R-6.6.11 [verified]

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
| `status` stays read-only and remains the only sync-health command surface. | `TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON`, `TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper`, `TestStatusCommand_JSONSurfacesSyncAuthRejectedOffline`, `TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState`, `TestFilterStatusSnapshot_IntersectsAccountAndDriveSelectors`, `TestE2E_Status_NoLegacyHistorySurface`, `TestE2E_RoundTrip` |
| `drive reset-sync-state` remains the only destructive sync-state recreate surface and requires explicit drive selection plus confirmation. | `TestNewDriveResetSyncStateCmd_HasYesFlag`, `TestRunDriveResetSyncStateWithInput_RequiresDrive`, `TestRunDriveResetSyncStateWithInput_RequiresInteractiveConfirmationWithoutYes`, `TestRunDriveResetSyncStateWithInput_ResetsAndRecreatesStateDB`, `TestRunDriveResetSyncStateWithInput_RefusesLiveSyncOwner` |
| `pause` and `resume` remain CLI-owned config mutations rather than direct sync-store writes. | `TestPauseCommand_PersistsTimedPause`, `TestResumeCommand_ClearsPausedKeys`, `TestClearPausedKeys_RemovesBothKeys` |
| Watch and one-shot sync command wiring stays inside the CLI composition boundary and delegates runtime ownership to the sync daemon/orchestrator seam. | `TestRunSyncCommand_UsesConfigDryRunWhenFlagUnset`, `TestRunSyncCommand_WatchRejectsEffectiveDryRun`, `TestRunSyncCommand_SkipsPausedInvalidDrivesDuringValidation`, `TestRunSyncWatch_UsesInjectedRunner`, `TestRunSyncDaemonWithFactory_CallsOrchestrator`, `TestPrintRunOnceResult_MatchesReportsBySelectionIndex` |

## Command Surface

| Command family | Purpose |
| --- | --- |
| `login`, `logout` | auth and account session lifecycle |
| `ls`, `get`, `put`, `rm`, `mkdir`, `mv`, `cp`, `stat` | file operations |
| `drive` | drive management and explicit per-drive sync-state reset |
| `shared*` | shared-item discovery and add flows |
| `sync`, `pause`, `resume` | sync control |
| `status` | read-only account and sync health |
| `perf` | live owner perf view and capture |
| `recycle-bin` | recycle-bin operations |

There is no `resolve` command family anymore.

Configured standalone mount-root mounts keep `/` anchored at the configured
mount root, not the backing drive root. Path-oriented file operations such
as `mkdir` therefore walk and mutate only inside that configured subtree.

## Status And Read-Only Sync State

`status` is intentionally read-only and account-centric. It is the only
sync-health command.

- account identity comes from the validated config+catalog snapshot
- runtime mount identity comes from configured standalone drives plus managed
  child-mount records in `mounts.json`
- sync-state snapshots come from store-owned raw authority reads
- the CLI renders status conditions from the sync-owned stored-condition
  projection, using the sync-owned `ConditionKey` taxonomy and ordering helpers
- one CLI-owned presentation boundary shapes status-condition titles, reasons,
  actions, scope-kind labels, ordering, truncation, and JSON output
- `status_sync_state.go` only assembles the high-level sync-state payload;
  `status_condition_descriptors.go` owns the condition presentation and JSON
  shaping surface itself, including typed scope-kind labels and descriptor
  tables keyed directly by sync-owned `ConditionKey`
- live authenticated account identity and drive catalog overlays come from
  bounded Graph proof/discovery owned by the command
- live perf comes from the active owner over the control socket when requested

When both `--account` and `--drive` selectors are present, `status` applies
them as an intersection. `--drive` still selects configured standalone parent
drives, but each selected parent row automatically includes its attached child
mount rows. The CLI does not widen the result to the union of independently
matched accounts and configured drives.

The runtime status read model is now mount-shaped:

- standalone configured drives render as standalone mount rows
- managed shortcut child mounts render as child rows immediately under their
  parent drive row
- child rows carry their own sync-state snapshot and live perf overlay
- parent rows do not absorb child state or child perf totals

The target `status` surface projects the full sync model directly from:

- `observation_issues`
- `retry_work`
- `block_scopes`
- `baseline` / `remote_state` counts and drift facts
- account/auth/degraded overlays
- optional live perf

There is no second sync-health command. There is no separate history-only
surface for resolved conflicts, and `status` no longer exposes delete-safety or
manual conflict-request sections. It also no longer persists or renders a
store-owned "last sync / duration / last error" history block.

Minimal-config direct file-operation coverage keeps that contract explicit:
the full-suite `TestE2E_RoundTrip` status check asserts the current empty
snapshot surface (`No active conditions.`) and rejects any reintroduced
`Last sync:` history line.

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

The control-socket protocol is now mount-shaped:

- `GET /v1/status` returns `mounts`, not `drives`
- `GET /v1/perf` returns per-mount live snapshots keyed by mount ID
- the CLI status/perf overlay matches those mount IDs against the runtime mount
  rows it renders

The socket is no longer a sync-decision submission surface because the
architecture no longer has manual conflict or delete-approval workflows.

## Pause / Resume

`pause` and `resume` remain config mutations owned by the CLI. After writing
config, the CLI notifies a running watch owner to reload when possible. If no
watch owner is running, the updated config takes effect on the next start.

## Logout Recovery

`logout --purge` is also the recovery path when local config/catalog state is
damaged. The CLI first tries validated state, but if that fails it falls back
to best-effort config and catalog loads, logs warnings, resolves the selected
account from the recoverable durable facts, and continues the purge instead of
refusing all cleanup.

## State DB Handling

The CLI owns the only destructive operator surface for per-drive sync state:
`onedrive-go drive reset-sync-state --drive <drive>`.

- `--drive` is mandatory and must resolve to exactly one configured drive
- the default flow requires typing `RESET`; `--yes` is the non-interactive bypass
- the command refuses to run while that drive has a live sync owner
- the command deletes only the selected drive's state DB family and recreates a
  fresh canonical DB immediately

`status` remains read-only. Missing or unreadable DBs collapse to an empty sync
snapshot for that drive; status does not mutate state and does not become a DB
health control surface.

`sync` and `sync --watch` never auto-delete an existing DB. When startup sees
an unreadable, incompatible, or unsupported existing DB, the CLI renders the
same guidance in one-shot and watch flows: pause that drive first, rerun with
`--drive` selecting only other drives, or run `drive reset-sync-state --drive
...`. One-shot renders startup-ineligible drives through the shared startup
message path, reports completed runs separately, and exits non-zero after
reporting any affected drives. Watch mode warns immediately about each skipped
drive and continues healthy mounts unless none can start.

One-shot sync resolves the full selected drive set, but it validates only
runnable non-paused drives before startup. Paused drives remain in the startup
summary as skipped drives; an invalid paused drive must not block unrelated
runnable drives from executing.

One-shot and watch sync both materialize the resolved `sync_dir` before they
hand a runnable drive to the sync runtime. Config validation is allowed to
accept a missing directory, but startup must create that root first so the
scanner never receives a runnable drive whose local sync tree still does not
exist.

Generated follow-up commands in startup messages are shell-safe. Canonical IDs
and other user-controlled values are single-quoted before they are rendered
into suggested `pause` or `drive reset-sync-state` commands.

## What The CLI No Longer Owns

The CLI no longer owns:

- durable conflict-resolution requests
- blocked-delete approvals
- fallback direct DB writes for sync decisions
- `resolve` subcommands

Manual sync decisions were removed from the current product surface.
