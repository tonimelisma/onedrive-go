# Sync Control Plane

GOVERNS: internal/multisync/*.go, internal/synccontrol/*.go, sync.go

Implements: R-2.4.8 [verified], R-2.4.9 [verified], R-2.4.10 [verified], R-2.8.1 [verified], R-2.8.2 [verified], R-2.8.3 [verified], R-2.9.1 [verified], R-2.9.2 [verified], R-2.9.3 [verified], R-3.4.2 [verified], R-6.3.3 [verified], R-6.3.4 [verified], R-6.6.15 [verified], R-6.6.16 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Overview

The control plane owns multi-mount sync lifecycle. It sits above the
single-mount engine in `internal/sync` and answers questions the engine should
not answer:

- which runtime mounts are active right now
- how those mounts are started and stopped
- how control-socket reload changes the active mount set
- how live control-socket requests are serialized through the running control loop
- how per-mount failures are isolated from one another

`sync.go` is the CLI entrypoint for this layer. `internal/multisync` is the
runtime package that implements it.

## Ownership Contract

- Owns: Multi-mount sync lifecycle, runtime mount-spec compilation, shortcut topology decisions from parent-engine facts, mount inventory mutation, per-mount engine startup/shutdown, reload diffing, control-socket ownership, and per-mount panic/error isolation.
- Does Not Own: Parent-drive Graph discovery, single-mount content observation, planning, execution, retry/trial policy, or sync-store persistence semantics.
- Source of Truth: The current `config.Holder` snapshot, the CLI-compiled standalone mount configs, plus the runtime mount set and `runners` map owned by the watch-mode orchestrator loop.
- Allowed Side Effects: Session creation, engine construction/closure, Unix control-socket bind/unlink, per-mount goroutine startup, live perf capture, and control-plane logging.
- Mutable Runtime Owner: `RunWatch` owns the live `runners` map. Each `watchRunner` owns one cancel function and one completion channel for exactly one mount.
- Error Boundary: The control plane converts mount startup into structured per-mount startup outcomes, returns one-shot startup classification separately from completed `MountReport` values, and keeps watch-runner failures isolated to the affected mount or log path. Engine-internal errors remain inside the single-mount boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| `RunWatch` starts the runnable runtime mount set, skips incompatible-store mounts with immediate warnings, and rejects all-paused startup through the same startup-summary model. | `TestOrchestrator_RunWatch_SingleMount`, `TestOrchestrator_RunWatch_MultiMount`, `TestOrchestrator_RunWatch_SkipsIncompatibleStoreMountWhenAnotherMountStarts`, `TestOrchestrator_RunWatch_ReturnsErrorWhenAllMountsPaused` |
| The Unix control socket is the single live-owner lock for one-shot and watch sync, is acquired before any mount-inventory reconciliation or removed-child cleanup, reports owner mode/status, rejects unsupported one-shot control requests with typed `foreground_sync_running`, and keeps reload/stop serialized through the watch control loop. | `TestRunOnce_ControlSocketBlocksWatchOwner`, `TestRunOnce_BindsControlSocketBeforeInventoryReconciliation`, `TestOrchestrator_OneShotControlSocket_StatusAndRejectsNonStatus`, `TestOrchestrator_ControlSocket_StatusAndStop`, `TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners` |
| The control socket also exposes live perf snapshots and explicit capture bundles for both one-shot and watch owners without creating a second network surface or durable metrics store. | `TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture`, `TestOrchestrator_OneShotControlSocket_PerfCaptureRejectsInvalidDuration`, `internal/cli/perf_test.go` (`TestMainWithWriters_PerfCaptureJSON_ForOneShotOwner`, `TestMainWithWriters_PerfCaptureFailsWhenNoOwnerIsRunning`) |
| Socket files are permissioned private, stale sockets are removed only after a failed live probe, and empty hash-runtime socket directories are cleaned up on close. | `TestControlSocketServer_PermissionsStaleCleanupAndRuntimeDirRemoval` |
| Control-socket reload applies add/remove/pause/expired-pause diffs to the live runner set without bouncing unaffected mounts. | `TestOrchestrator_Reload_AddDrive`, `TestOrchestrator_Reload_RemoveMount`, `TestOrchestrator_Reload_PausedMount`, `TestOrchestrator_Reload_TimedPauseExpiry` |
| Managed shortcut lifecycle stays control-plane owned while parent engines remain the sole parent-drive Graph observers: topology batches update `mounts.json`, persistence failures prevent cursor commit, unavailable binding records skip child runners without touching sync-store state, duplicate content roots persist conflict reasons, local root collisions skip only affected child mounts, shortcut projection moves reserve old paths until the local move completes, and local alias rename/delete mutate only the shortcut placeholder. | `TestFullDeltaWithShortcutTopology_EmitsShortcutFactsAndSuppressesContent`, `TestWatch_TopologyApplyFailureDoesNotAdvanceCursor`, `TestApplyShortcutTopologyBatch_CompleteEnumerationUpdatesAndRemovesBindings`, `TestApplyShortcutTopologyBatch_EmptyCompleteEnumerationRemovesOldBindings`, `TestApplyShortcutTopologyBatch_RemoteDeleteMarksPendingRemoval`, `TestApplyShortcutTopologyBatch_SamePathReplacementDefersNewBinding`, `TestApplyShortcutTopologyBatch_UnavailableBindingPersistsUnavailableRecord`, `TestApplyDurableProjectionConflicts_DuplicateChildrenAllConflict`, `TestBuildRuntimeMountSet_LocalRootSaveFailureDoesNotBlockStandaloneMount`, `TestApplyInventoryPersistFailureSkipsOnlyDirtyChild`, `TestApplyChildProjectionMoves_SkipsDirtyChildDroppedAfterPersistFailure`, `TestApplyChildProjectionMoves_PersistFailureStillMovesCleanChild`, `TestApplyChildProjectionMoves_RenamesLocalProjectionAndClearsReservation`, `TestFinalizeRuntimeMountSetLifecycle_RecompilesAfterProjectionMove`, `TestApplyChildProjectionMoves_TargetEmptyAutoResolves`, `TestApplyChildProjectionMoves_MatchingTreesAutoResolve`, `TestApplyChildProjectionMoves_TargetConflictMarksChildConflictAndSkips`, `TestApplyChildProjectionMoves_MissingSourceAndTargetStaysUnavailable`, `TestReconcileChildMountLocalRoots_RenamedMaterializedRootCreatesAliasRenameAction`, `TestApplyChildRootLifecycleActions_RenamePatchesPlaceholderAndPreservesMount`, `TestApplyChildRootLifecycleActions_DeleteDeletesPlaceholderAndQueuesStatePurge`, `TestApplyChildRootLifecycleActions_FailureFiltersStaleProjectionMoves`, `TestFinalizePendingMountRemovals_DirtyProjectionStaysReserved`, `TestFinalizePendingMountRemovals_ProjectionStatErrorStaysReserved`, `TestFinalizePendingMountRemovals_RecompileReleasesParentSkipDir` |

## Runtime Mount Specs

The control plane now compiles runtime `mountSpec` values before session
creation and engine construction.

Configured standalone drives are still the only explicit user-facing selection
surface, but they are no longer the runtime construction shape. The CLI resolves
configured drives and compiles them into `multisync.StandaloneMountSelection`:
valid selections become `StandaloneMountConfig` values, while per-drive
conversion failures become `MountStartupResult` values before the orchestrator
is constructed. One-shot and watch startup bind the owner control socket before
the following inventory-mutating work so a losing sync process cannot reconcile
`mounts.json`, finalize pending removals, or purge child state. On startup and
reload the control plane now:

1. consumes the CLI-compiled standalone mount selection
2. compiles those standalone configs into runtime mounts
3. loads `mounts.json`
4. attaches valid managed child mounts beneath selected standalone parents
5. installs precise local subtree exclusions on those parents before engine
   construction

Managed child mounts are durable runtime facts, not synthetic follow-up drives
invented by the engine.

Each current `mountSpec` owns:

- stable runtime mount identity
- stable reporting identity and selection index
- local sync root and state DB path
- remote drive/root identity for mount-root mounts
- token-owner identity and account email for sync session creation
- transfer/check/min-free-space tunables
- resolved pause state and mount-root observation hints
- parent-owned child subtree exclusions

`mountSpec` no longer carries `ResolvedDrive`, and `OrchestratorConfig` no
longer accepts resolved-drive values. Configured drives are compiled at the CLI
edge into `StandaloneMountConfig`, and sync session construction consumes those
facts through `driveops.MountSessionConfig`.

Engine construction is no longer drive-shaped. `internal/multisync` now derives
`sync.EngineMountConfig` from `mountSpec` and passes that sync-owned mount
config into `sync.NewMountEngine(...)`.

Runtime reporting is mount-identified. Standalone mounts keep their configured
canonical drive ID inside `MountIdentity`, but managed child mounts report by
durable `MountID` and do not synthesize `shared:` canonical drive IDs. CLI
status output follows the same boundary: child rows expose `mount_id` and omit
`canonical_id`, while standalone rows retain `canonical_id`. JSON nests child
rows below their parent mount in `child_mounts`, and text output indents child
rows beneath the parent drive so the control surface remains parent-owned.

Managed child mounts persist their token owner in `mounts.json`; pause and sync
tunables still come from the selected standalone namespace mount. There is no
child pause, resume, reset, config, or CLI control surface. A child projection
is controlled by the OneDrive shortcut itself and by the parent drive's pause
state. Conflicting content roots are resolved durably before engine startup:
explicit standalone mounts win over duplicate child projections, and duplicate
child projections for the same namespace/content root are marked `conflict`
with a structured reason and reported as skipped startup outcomes.

Unavailable child mounts are also durable control-plane lifecycle state. If the
parent engine observes a shortcut binding item but cannot return enough target
metadata to materialize it, topology application stores or updates the child record as
`unavailable` with `state_reason: shortcut_binding_unavailable`. The child
engine is not started, the existing child state DB is left untouched, and the
parent namespace continues to reserve the child path. A later complete topology
fact for the same binding reactivates the same `MountID`, fills the remote
target IDs, and clears the reason. Recovery does not require the control plane
to call Graph; it happens when the parent engine replays or refreshes topology
facts through its normal remote observation path.

Shortcut rename and move are local projection changes, not new child mounts.
Because child `MountID` comes from `(NamespaceID, BindingItemID)`, the control
plane reuses the same child state DB. While the filesystem move is pending,
`mounts.json` keeps the new path plus `reserved_local_paths` for old paths so
the parent mount excludes both. Records with reserved projection paths do not
eagerly create the new child root; the orchestrator first applies the local
projection move after stopping any old runner, then validates or creates the
resulting child root before starting the new runner. If old and new local
projection paths both exist, the control plane auto-resolves only the cases
that are provably data-preserving: an empty target directory is removed and
replaced by the old projection root, while two byte-identical directory trees
are collapsed by keeping the new path and removing the old reserved path. Tree
comparison is a mount-infrastructure safety check, not a metadata clone check:
it compares relative directory/file structure plus regular-file size and
SHA-256 content, and it refuses to auto-resolve symlinks or unsupported
entries. Real content differences become `conflict` with
`state_reason: local_projection_conflict`; filesystem inspection, hashing, or
removal failures become `unavailable` with
`state_reason: local_projection_unavailable`. If the source and target are both
missing, the control plane keeps the child unavailable rather than creating an
empty root that could be mistaken for a real remote delete. Case-only renames on
case-insensitive filesystems use a temporary sibling rename, and symlinked
projection ancestors are rejected before creating or moving child roots.

Shortcut lifecycle states are intentionally small and producer-owned:

| Situation | Durable state | Runner behavior | Parent exclusion |
| --- | --- | --- | --- |
| Shortcut binding is healthy | `active` | child runner may start unless parent is paused | current child path |
| Parent drive is paused | `active` | child runner is paused with the parent | current child path |
| Binding target cannot be refreshed | `unavailable: shortcut_binding_unavailable` | child runner skipped, retry on next reconciliation | current child path |
| Shortcut was authoritatively removed | `pending_removal: shortcut_removed` until state purge/final delete | child runner stopped before purge | current and reserved paths until finalized |
| Removed shortcut projection still has local content | `pending_removal: removed_projection_dirty` | child runner stopped; user must move/remove local projection content | current and reserved paths until finalized |
| Removed shortcut projection cannot be inspected or removed | `pending_removal: removed_projection_unavailable` | child runner stopped and cleanup retried | current and reserved paths until finalized |
| Same-path replacement arrives before old projection finalizes | old mount remains `pending_removal`; new binding is deferred | old child stopped, new child not started | shared path remains reserved |
| Duplicate child content root | `conflict: duplicate_content_root` | duplicate child runners skipped | conflicting child paths |
| Explicit standalone mount owns same content root | `conflict: explicit_standalone_content_root` | automatic child skipped | child path remains reserved |
| Projection move source and target differ | `conflict: local_projection_conflict` | child skipped | current and reserved paths |
| Projection move cannot inspect/hash/remove safely | `unavailable: local_projection_unavailable` | child skipped and retried | current and reserved paths |
| Child local root is a file, final symlink, or unsafe collision | `conflict: local_root_collision` | child skipped | child path |
| Previously materialized child local root is renamed to one same-parent identity match | `active` until the alias PATCH is applied | child runner stopped, placeholder renamed, same child state DB restarts at new path | current path plus same-parent identity reservation |
| Previously materialized child local root is missing with no same-parent identity match | `pending_removal: shortcut_removed` after placeholder DELETE succeeds | child runner stopped, child state purged | child path until finalized |
| Previously materialized child local root has multiple same-parent identity matches | `conflict: local_alias_rename_conflict` | child skipped until user resolves ambiguity | current and candidate paths |
| Local alias rename/delete cannot mutate the placeholder | `unavailable: local_alias_rename_unavailable` or `unavailable: local_alias_delete_unavailable` | child skipped and retried | current and candidate paths |
| Previously materialized child local root is missing but identity is unavailable | `unavailable: local_root_unavailable` | child skipped and retried after the user restores the directory | child path |
| Child local root stat/create has a transient filesystem error | `unavailable: local_root_unavailable` | child skipped and retried | child path |

After pending-removal finalization or a successful projection move, the
orchestrator reloads `mounts.json` and recompiles runtime mounts in the same
cycle. Parent `localSkipDirs` therefore releases finalized or moved paths
immediately. If saving lifecycle mutations to `mounts.json` fails, the
orchestrator keeps standalone parents and already-durable children eligible,
skips only dirty child records whose in-memory state could not be trusted, and
keeps their parent skip dirs reserved for that run. Runtime mount-set
construction uses one pipeline for reconciliation, child-root materialization,
compilation, dirty-child filtering, removal finalization, projection moves,
recompile, and final child-root validation. Projection moves, skipped startup
results, removed-mount finalization, and parent exclusions therefore derive
from the same current mount inventory view instead of from stale side lists.
Projection move handling keeps deterministic decision logic separate from
filesystem mutation: unexported classifiers first decide whether the current
path state means rename, already-moved success, case-only rename, safe
auto-resolution, conflict, or unavailable, and only the executor performs the
rooted filesystem effects for that decision.

Shortcut topology is split by authority. The parent engine is the only runtime
that calls Graph delta/list/get for the parent drive. It classifies shortcut
placeholder observations as `ShortcutTopologyBatch` facts and suppresses those
items from normal content planning. `internal/multisync` consumes those facts
only after they leave the engine boundary:

- upsert facts create, update, reactivate, or rename child mount records
- delete facts mark existing child records `pending_removal`
- unavailable facts persist `unavailable: shortcut_binding_unavailable`
- complete batches mark previously known but absent bindings pending removal
- same-path replacement stores the incoming binding in
  `deferred_shortcut_bindings` until the old projection finalizes, so
  `mounts.json` never contains duplicate sibling paths
- all active, conflicted, unavailable, deferred, and pending-removal paths remain
  parent reservations
- successful topology mutation in watch mode returns `ErrMountTopologyChanged`,
  causing the parent runner to exit before cursor commit; the orchestrator then
  recompiles inventory and restarts affected runners

If `mounts.json` cannot durably accept a topology batch, the parent engine does
not commit its remote observation cursor. The same Graph facts replay later
from the parent engine, preserving the one-observer ownership boundary without
letting the control plane rediscover remote state.

A complete topology batch is authoritative even when it contains zero shortcut
facts. That empty complete batch means the parent engine enumerated the parent
drive and saw no current shortcut bindings, so multisync still applies it and
marks older bindings `pending_removal`. This keeps startup/reload token-reset
and offline-delete cases on the same parent-engine observation path as ordinary
incremental deletes.

Runtime mount-set construction then creates missing child local roots
component-by-component after any pending projection move, marks file,
final-symlink, symlinked-ancestor, or traversal collisions as durable
child-mount conflicts before child engine startup, recompiles the runtime mount
set, and starts or stops only the affected child mounts.

Authoritative removal is a lifecycle transition. A removed shortcut is first
marked `pending_removal`, the control plane stops any active child runner, and
the parent namespace keeps reserving that local projection. Cleanup inspects the
local projection before purging the managed child state DB. Missing or empty
projection roots finalize and release the reservation. Dirty or unavailable
projection roots remain `pending_removal` with
`removed_projection_dirty` or `removed_projection_unavailable`, so parent sync
cannot re-upload child content as ordinary parent-drive files. Cleanup treats
only proven absence (`ENOENT` or a successful no-follow stat with `Exists=false`)
as safe missing state; any other stat, inspection, removal, or state-purge error
keeps the reservation and the child DB.
Parent namespace mounts continue to reserve every child mount path they own
while records are active, paused, conflicted, unavailable, or pending removal.
Reserved old projection paths are included in the same parent exclusion set
until the move completes or the operator resolves the conflict.
The namespace owner therefore owns child root materialization and conflict
classification; the child engine starts only after its mount root exists as a
directory inside the parent sync-root boundary. A failed inventory save after
local-root classification is logged and retried by a later reconciliation; the
current run still uses the classified in-memory inventory so unrelated mounts
can start. Parent engines must apply that reservation consistently to local
scan/watch surfaces and post-sync transfer housekeeping, so parent cleanup
never walks into a child mount's in-flight transfer artifacts.

Local child-root deletion and local child-root rename are not target-content
delete or target-content rename operations. `mounts.json` records whether a
child root has ever been materialized and stores its filesystem identity. If
the directory later appears at exactly one sibling path with the same identity,
the orchestrator stops the child runner, PATCHes the shortcut placeholder name
by `binding_item_id`, updates the record, and restarts the same child mount ID
at the new path. If the directory disappears with no same-parent identity
candidate, the orchestrator stops the child runner, DELETEs only the shortcut
placeholder, marks the child pending removal, and purges only the child state
DB. During the short pre-mutation window, parent observation suppresses the
same-parent renamed directory by stored root identity instead of persisting a
projection-move reservation that would be ambiguous after a crash. Ambiguous
identity matches, cross-directory moves, symlink/collision
states, and Graph mutation failures become durable conflict/unavailable states
rather than target-content mutations.

## Boundary To The Engine

The control plane does not observe, plan, execute, or persist sync state
itself. Those responsibilities remain in the single-mount engine.

- `internal/multisync` owns runtime mount selection, session resolution,
  derivation of sync-owned engine mount config, per-mount goroutines, reload,
  and shutdown.
- `internal/sync` owns one-shot execution, watch-mode runtime state, conflict
  execution, retry/trial logic, scope lifecycle, and reconciliation.

This split keeps the engine package focused on one mounted content root at a
time while allowing the CLI to run any number of mounts through one consistent control
surface.

Parent engines receive a narrow managed-root reservation list derived from
`mounts.json`: reserved relative paths, child mount IDs, binding item IDs, and
optional filesystem identities. The engine uses those reservations only to
suppress/report local facts at observation boundaries. It does not decide
shortcut lifecycle or mutate Graph. Path-reserved events and same-parent
identity matches are reported back to `Orchestrator`, which triggers shortcut
reconciliation in the single control-plane owner.

## `Orchestrator`

`Orchestrator` is the multi-mount coordinator used by both one-shot `sync` and
watch-mode `sync --watch`.

It is always used, even for a single configured drive. There is no separate
single-mount CLI path, because special-casing `n=1` would create a second
lifecycle model for startup, shutdown, and reload.

### RunOnce

`RunOnce` compiles runtime mount specs, resolves sessions, builds one engine per
mount, and runs all mounts concurrently. Startup eligibility is classified per
mount first, including CLI conversion failures, paused standalone parents, and
managed child mounts skipped because their parent is missing or their content
root conflicts. Runnable mounts then produce one completed `MountReport` each,
while startup-ineligible mounts remain startup outcomes instead of synthetic
completed reports. The control plane never aborts the whole pass because one
mount failed; partial failure is isolated per mount. Both startup results and
completed reports carry a stable `SelectionIndex` matching the compiled runtime
order plus a `MountIdentity` matching the current boundary, so standalone
parents and their attached child mounts remain deterministic through
orchestration, rendering, and bookkeeping.

### RunWatch

`RunWatch` starts one watch-mode engine per runnable non-paused mount and then
owns the long-running control loop. It listens for:

- `ctx.Done()` for shutdown
- JSON-over-HTTP requests on the Unix control socket

Pause semantics come from `config.Drive.IsPaused()` and
`config.ClearExpiredPauses()` for configured parents. Managed child mounts
inherit the parent drive state and have no independent pause state. The control
plane consumes those rules; it does not redefine them inside the engine.

Existing state DBs that fail store compatibility checks are reported as
per-mount startup outcomes. CLI conversion failures use the same startup-result
path, including on reload, so one bad selected mount does not prevent healthy
mounts from running. Watch startup warns about those mounts immediately, keeps
healthy mounts running, and exits non-zero only when no runnable mount starts.
A paused-only or all-failed selection is a structured startup refusal, not a
special string-only path.

### Control Socket

`RunOnce` and `RunWatch` both bind the configured Unix control socket before
starting engine work. This socket is the single process-owner lock: a live
socket means another sync owner is already running for the same data directory.
Stale socket files are removed only after a failed live dial proves no process
owns them.

The configured socket path normally lives under the app data directory. If that
absolute path would exceed the platform-safe Unix socket length, config derives
a stable hash-named runtime directory under the OS temp directory and stores
only the socket there; durable sync state remains in the drive state DB. If the
normal path and the hashed runtime fallback both exceed the Unix socket budget,
path derivation fails explicitly. `RunOnce` and `RunWatch` treat that as fatal
startup because the control socket is the single-owner lock.
If watch startup fails after the socket is bound, the control plane closes and
unlinks that socket before returning the startup error.

Wire facts live in `internal/synccontrol`: endpoint constants, owner modes,
request/response structs, response statuses, and stable error codes. Server
lifecycle stays in `internal/multisync`; CLI transport stays in `internal/cli`.

The socket speaks JSON over HTTP:

- `GET /v1/status` returns the owner mode (`oneshot` or `watch`) and managed mounts.
- `GET /v1/perf` returns the owner mode plus the live aggregate and per-mount perf snapshots currently owned by the active sync runtime. This surface is live-only and returns whatever the owner has collected so far; it does not materialize historical perf state from SQLite.
- `POST /v1/perf/capture` triggers an explicit local capture bundle from the active owner. The request carries bounded duration plus optional output-dir, trace, and full-detail toggles; the response returns the local artifact paths for the completed bundle.
- `POST /v1/reload` reloads config in the watch owner.
- `POST /v1/stop` asks the watch owner to stop cleanly.

One-shot sync exposes status plus the direct perf endpoints above. Durable
control requests still return a busy response with
`code="foreground_sync_running"` because a foreground one-shot sync is already
the active owner. The CLI probes the owner boundary to decide whether live
control requests can be sent at all, but there is no parallel direct-DB
mutation path for sync decisions anymore.

Error responses have the shape `{status, code, message}`. Stable codes are
`invalid_request`, `foreground_sync_running`, `capture_unavailable`,
`capture_in_progress`, and `internal_error`.

### Reload

Control-socket reload does four things in order:

1. load config from disk
2. clear expired timed pauses
3. compile the new active runtime mount set
4. diff that set against running mounts

Removed or newly paused mounts are stopped and closed. Newly added or newly
resumed mounts are started when they are runnable; incompatible-store mounts
are warned and skipped without bouncing healthy runners. Already-running mounts
remain running. When a
timed pause has already expired by reload time, the config keys are cleaned up
but the running mount is not bounced.

## Runtime Ownership

The control plane has one mutable runtime structure in watch mode: the active
runner set.

- The `RunWatch` select loop is the single writer for the `runners` map.
- `startWatchRunner` creates one goroutine per running mount. That goroutine owns closing the runner's `done` channel exactly once on exit.
- The control command channel is internal to `RunWatch`; socket handlers send commands into that channel and wait for one response.
- The control plane itself owns no timers; reload, stop, and perf capture are event-driven through control-socket requests and context cancellation.
- The control plane owns live perf registration for active mounts, but not the
  counters themselves. `internal/perf` owns the aggregate/live snapshot state;
  the control plane only exposes that state through the local socket and
  forwards explicit capture requests into the owning runtime.

## `MountRunner`

`MountRunner` wraps a single mount's sync function with panic recovery and
error isolation. One mount panicking must become one isolated mount outcome,
not
a process-wide crash or a cross-mount failure cascade.

## CLI Contract

The `sync` Cobra command resolves drives, validates sync eligibility,
compiles selected drives into `multisync.StandaloneMountSelection`, constructs
an `Orchestrator`, and chooses between `RunOnce` and `RunWatch`.

- `--watch` selects daemon mode
- `--download-only` and `--upload-only` select sync mode
- `--dry-run` and `--full` apply only to one-shot mode
- first SIGINT/SIGTERM cancels the shared watch contexts and lets each mount's
  engine seal new admission and follow its normal shutdown path
- second signal forces exit

No timer escalates the first signal; forced exit is owned only by the second
signal.

The CLI command does not reach into per-mount engine internals. It only speaks
to the control-plane boundary.

## Design Constraints

- No mount-to-mount coordination state exists in memory or in SQLite.
- Each mount gets its own engine instance and state DB.
- Session creation goes through `driveops.SessionRuntime` with
  `driveops.MountSessionConfig`, keeping token-source caching owned in one
  place and keeping `ResolvedDrive` out of runtime construction.
- Reload updates config through one shared `config.Holder` and uses the
  CLI-supplied standalone-mount selection compiler, so both the control plane
  and session runtime see the same config snapshot without giving multisync
  authority over resolved-drive construction.

## Rationale

- **Separate control plane from runtime**: multi-mount lifecycle code changes
  for different reasons than single-mount execution logic. Keeping them in one
  package made both harder to reason about.
- **Always use the same top-level path**: one configured drive and many mounts share the
  same shutdown, reload, and panic-isolation semantics.
- **Per-mount isolation is explicit**: the control plane collects one report
  per mount and one panic cannot poison unrelated mounts.
