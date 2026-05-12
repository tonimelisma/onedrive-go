# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-2.3.3 [verified], R-2.5.5 [verified], R-2.5.6 [verified], R-2.8.3 [verified], R-2.9 [verified], R-2.10.4 [designed], R-2.10.47 [verified], R-6.6.11 [verified], R-6.6.17 [verified], R-6.8.16 [verified]

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
| `status` stays read-only and remains the only sync-health command surface. | `TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON`, `TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper`, `TestStatusCommand_JSONSurfacesSyncAuthRejectedOffline`, `TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState`, `TestStatusCommand_NoAccountsDoesNotMutateManagedState`, `TestFilterStatusSnapshot_IntersectsAccountAndDriveSelectors`, `TestBuildChildStatusMount_FinalDrainGuidesAccessRestore`, `TestPrintMountStatus_RendersGuidedShortcutRecovery`, `TestStatusRuntime_SummaryJSONWithActiveOwner`, `TestStatusRuntimeOverlayApplyAndSummary`, `TestPrintMountStatus_ShowsRuntimeState`, `TestE2E_Status_NoLegacyHistorySurface`, `TestE2E_RoundTrip` |
| `drive reset-sync-state` remains the only destructive sync-state recreate surface and requires explicit drive selection plus confirmation. | `TestNewDriveResetSyncStateCmd_HasYesFlag`, `TestRunDriveResetSyncStateWithInput_RequiresDrive`, `TestRunDriveResetSyncStateWithInput_RequiresInteractiveConfirmationWithoutYes`, `TestRunDriveResetSyncStateWithInput_ResetsAndRecreatesStateDB`, `TestRunDriveResetSyncStateWithInput_RefusesLiveSyncOwner` |
| `pause` and `resume` remain CLI-owned config mutations rather than direct sync-store writes. | `TestPauseCommand_PersistsTimedPause`, `TestResumeCommand_ClearsPausedKeys`, `TestClearPausedKeys_RemovesBothKeys` |
| Watch and one-shot sync command wiring stays inside the CLI composition boundary and delegates runtime ownership to the sync daemon/orchestrator seam. | `TestDryRunFlagSurfaceOnlySyncCommand`, `TestRunSyncCommand_UsesConfigDryRunWhenFlagUnset`, `TestRunSyncCommand_DryRunOpensLogFileAndWarnsOnFailure`, `TestRunSyncCommand_DryRunFailsWhenControlSocketPathCannotBeDerived`, `TestRunSyncCommand_WatchRejectsEffectiveDryRun`, `TestRunSyncCommand_PassesMissingSyncDirToRunOnce`, `TestRunSyncCommand_DryRunPassesMissingSyncDirWithoutCreatingIt`, `TestRunSyncCommand_PassesPausedInvalidDriveToRunnerAsPaused`, `TestRunSyncWatch_UsesInjectedRunner`, `TestRunSyncDaemonWithFactory_CallsOrchestrator`, `TestPrintRunOnceResult_MatchesReportsBySelectionIndex` |
| Shortcut child lifecycle status is formatted from sync-owned `ShortcutRootStatusView` values, and the CLI supplies the managed data directory to multisync rather than letting the control plane derive ambient paths. | `TestBuildChildStatusMount_RendersLifecycleState`, `TestBuildChildStatusMount_SurfacesProtectedPaths`, `TestRunSyncDaemonWithFactory_CallsOrchestrator`, `TestBuildChildStatusMount_BlockedDetailAppendsInstanceDetail` |
| Command failure presentation exhaustively maps the shared error classes, while lower layers still own their own domain classification. | `TestClassifyCommandError`, `TestCommandFailurePresentationForClass` |
| Command-level side-effect contracts are tested at the CLI boundary: read-only commands do not mutate managed state, and mutating commands fail selector/path validation before remote mutation. | `TestRunLs_DoesNotMutateManagedState`, `TestRunRm_RequiresExplicitPathBeforeGraphMutation` |

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

Sync intent is derived from observation snapshots and planner reconciliation,
then applied by the sync executor through concrete file and remote side effects.

Configured standalone mount-root mounts keep `/` anchored at the configured
mount root, not the backing drive root. Path-oriented file operations such
as `mkdir` therefore walk and mutate only inside that configured subtree.

## Status And Read-Only Sync State

`status` is intentionally read-only and account-centric. It is the only
sync-health command. The public presentation is account -> configured drives ->
shared folders. Internal runtime concepts such as mount IDs and stored
condition keys remain private implementation details.

- account identity comes from the validated config+catalog snapshot, enriched
  by `/me` when authentication succeeds
- configured drive rows come only from config/catalog; status never enumerates
  unconfigured Microsoft Graph drives into the output
- shared folder shortcuts discovered by a parent engine render as nested
  `Shared folders` under that configured parent drive
- sync-state snapshots come from store-owned raw authority reads
- the CLI translates sync-owned stored condition groups into public `Issues:`
  sections and JSON `issues`, with user-facing titles, reasons, actions,
  scope labels, ordering, and path sampling
- `status_sync_state.go` only assembles the high-level sync-state payload;
  `status_condition_descriptors.go` owns the public issue presentation while
  still keying descriptors from sync-owned `ConditionKey` values
- live account overlay fetches `/me` only for account display/sign-in proof, and
  fetches storage only for configured drives (`/me/drive` for the configured
  primary personal/business drive, or a narrow drive-by-ID lookup for a
  configured non-primary drive that already has a known remote drive ID)
- live runtime ownership comes from the active control socket when reachable;
  it may contribute summary/runtime perf facts but does not expose mount IDs in
  public status JSON
- live perf comes from the active owner over the control socket when requested

When both `--account` and `--drive` selectors are present, `status` applies
them as an intersection. `--drive` still selects configured standalone parent
drives, but each selected parent row automatically includes its attached shared
folder shortcut rows. The CLI does not widen the result to the union of independently
matched accounts and configured drives.

The JSON surface follows the user model:

- `summary.total_drives` counts configured top-level drive rows
- `summary.total_shared_folders` is present only when nested shared folders are
  displayed
- accounts use `accounts[].drives`
- shortcut projections use `shared_folders`
- sync-state issue groups use `issues`
- healthy account auth omits auth fields; `sign_in_required` appears only when
  the user needs to sign in
- public JSON omits `mount_id`, `namespace_id`, `canonical_id`,
  `projection_kind`, `drive_id`, `user_id`, `auth_state: ready`, and
  `live_drives`

Drive rows expose `kind`, `name`, `folder`, `state`, optional `storage`,
optional lifecycle fields, optional `sync_state`, and optional
`shared_folders`. User-facing states are `up_to_date`, `syncing`, `pending`,
`paused`, `issues`, and `unavailable`. Text output uses `Folder`, `Storage`,
`Files`, `Remote changes`, `Retrying`, and `Issues` labels. It never prints an
empty healthy issue section, never prints "No active conditions", and never
prints a healthy `Auth: ready` line.

Child lifecycle rows expose `state`, `state_reason`, `state_detail`,
`protected_current_path`, `protected_reserved_paths`, typed
`issue_class`/`recovery_class`, `recovery_action`, and `auto_retry` from
sync-owned `ShortcutRootStatusView` values plus child sync-state snapshots. The
CLI does not read raw `shortcut_roots` fields such as protected-path
bookkeeping, blocker detail, or waiting replacement internals; sync owns that
projection. Text and JSON status describe the protected-root state and the next
recovery step without duplicating engine transition policy or shortcut-state
copy tables in the CLI. Recovery copy uses the same product vocabulary as the
control plane: "shortcut alias", "child projection", "reserved path", and
"parent engine child work snapshots".

For `removed_final_drain`, status must make the retry/discard choice explicit:
the child keeps retrying while its state DB owns dirty content state; the user
can restore shared-folder access for normal retry, or delete the local shortcut
directory to discard the dirty local projection and let parent release cleanup
purge the child state automatically.

Shortcut child status distinguishes the recovery class rather than collapsing
all failures into one blocked row: target unavailable, local root unavailable,
unsafe/blocked path, cleanup blocked, and retryable final-drain work use
distinct `state_reason`/`state_detail` values. Retryable final drain and
cleanup states set `auto_retry=true`; cleanup or purge failures keep the child
visible until the next reconciliation succeeds. If the user deletes the local
shortcut projection to discard it, parent release cleanup removes the child row.

The target `status` surface projects the full sync model directly from:

- `observation_issues`
- `retry_work`
- `block_scopes`
- `baseline` / `remote_state` counts and drift facts
- account/auth/degraded overlays
- optional live perf

`status` is the single read-only sync-health surface. It renders current
issues, drive/shared-folder state, sign-in-required overlays, and optional live
perf; handled sync work is represented by current planning and execution state
rather than a separate history-only status section.

Minimal-config direct file-operation coverage keeps that contract explicit:
the full-suite `TestE2E_RoundTrip` status check asserts that healthy output
omits auth/debug lines and empty issue sections, and rejects any reintroduced
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
- `GET /v1/status` may include transient `shortcut_cleanup_failures` from the
  current owner so debug/status clients can distinguish child artifacts still
  remaining from artifacts already purged but not acknowledged by the parent
  owner; those transient rows are cleared when a retry succeeds or when the
  current parent snapshot no longer contains cleanup work for that source
- `GET /v1/perf` returns per-mount live snapshots keyed by mount ID
- the CLI status/perf overlay matches those mount IDs against the runtime mount
  rows it renders
- `status --perf` renders stale-work, local-observation, and replan-idle
  aggregate lines only when those counters are nonzero; the text remains
  path-free and ID-free.

The socket is runtime control and read-only observation only. Sync intent comes
from observation, planner reconciliation, and executor side effects; clients
cannot submit sync decisions over the socket.

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

That standalone-drive guidance is never rendered for managed child mounts. A
child startup or state-store warning names the child `mount_id`, may include the
child state DB path for diagnosis, and states that control belongs to the
parent drive or the OneDrive shortcut. It must not suggest `--drive ''`,
`--mount`, child pause/resume, or child reset commands.

One-shot sync resolves the full selected drive set, but it validates only
runnable non-paused drives before startup. Paused drives remain in the startup
summary as skipped drives; an invalid paused drive must not block unrelated
runnable drives from executing.

For `sync` and `sync --watch`, the CLI/config composition boundary passes the
managed data directory into `multisync.OrchestratorConfig`. Lower layers use
that explicit value for child state DBs, catalog, transfer scratch, and upload
session cleanup. `internal/multisync` must not call `config.DefaultDataDir()`,
`config.MountStatePath(...)`, or derive child runner/cleanup paths from ambient
process state.

Login and `drive add` materialize the configured `sync_dir` when they enroll or
repair a drive. The CLI validates the post-tilde `sync_dir` is absolute before
creating any directory, so stale relative config cannot create directories
under the caller's current working directory. If login cannot materialize the
sync root after moving the token and recording catalog metadata, it rolls those
login side effects back to the pre-login token/catalog/config state before
returning the failure. One-shot sync, watch startup, dry-run, and
control-socket reloads never create a missing root. They compile resolved
drives into mount configs and let engine startup report a missing root as a
per-mount `ErrMountRootUnavailable`, so a missing `sync_dir` on one drive does
not prevent other runnable mounts or reload changes from proceeding.

Dry-run one-shot sync resolves the dry-run decision before setup only so watch
mode can reject an effective dry-run before doing work. `sync --dry-run` is the
only command-line dry-run flag surface; config `dry_run=true` is still honored
for one-shot sync and rejected for watch unless the user explicitly passes
`sync --watch --dry-run=false`. Valid dry-run one-shot sync then follows the
same bootstrap as a live one-shot run: sync-bootstrap email/config
reconciliation, normal OAuth token sources with refresh persistence, `log_file`
open/create, and control-socket ownership all remain active. The command
resolves selected drives and delegates preview reporting to multisync/engine
with `DryRun=true`; lower layers use that flag to suppress plan execution and
sync-progress commits, not CLI/process setup. The dry-run product contract is
limited and explicit: no local sync-tree content mutation and no remote
OneDrive content mutation. Operational side effects from command setup,
authenticated Graph access, managed state open/checkpoint, and scratch
planning are still allowed.

`login`, `drive add`, and `drive remove` ask a running watch owner to reload
configuration through the control socket after successful config mutation. The
daemon process is not restarted; `internal/multisync` diffs the new mount set
and starts, stops, or keeps per-drive runners in place.

Generated follow-up commands in startup messages are shell-safe. Canonical IDs
and other user-controlled values are single-quoted before they are rendered
into suggested `pause` or `drive reset-sync-state` commands.

## CLI Boundary

The CLI owns command parsing, configuration mutations, presentation, and control
requests to a running sync owner. Sync decisions come from sync-owned
observation, planning, execution, and store transactions; the CLI does not write
planner decisions or executor outcomes directly.
