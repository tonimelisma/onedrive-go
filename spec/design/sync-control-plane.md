# Sync Control Plane

GOVERNS: internal/multisync/*.go, internal/synccontrol/*.go, sync.go

Implements: R-2.4.8 [verified], R-2.4.9 [verified], R-2.4.10 [verified], R-2.8.1 [verified], R-2.8.2 [verified], R-2.8.3 [verified], R-2.9.1 [verified], R-2.9.2 [verified], R-2.9.3 [verified], R-3.4.2 [verified], R-6.3.3 [verified], R-6.3.4 [verified], R-6.6.15 [verified], R-6.6.16 [verified], R-6.6.17 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

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

- Owns: Multi-mount sync lifecycle, runtime work from exact parent-declared child work snapshots, child runner start/final-drain/stop/purge process work, execution-failure skips, explicit child-artifact cleanup from the CLI-supplied data directory, reload diffing, control-socket ownership, and per-mount panic/error isolation.
- Does Not Own: Parent-drive Graph discovery, shortcut placeholder mutation, parent sync-dir shortcut alias filesystem policy, single-mount content observation, planning, execution, retry/trial policy, or sync-store persistence semantics.
- Source of Truth: The current `config.Holder` snapshot, the CLI-compiled standalone mount configs, plus the runtime runner set, run-owned parent child-work snapshot cache, and `runners` map owned by the active one-shot/watch runtime.
- Allowed Side Effects: Session creation, engine construction/closure, child artifact cleanup scoped by parent snapshot, Unix control-socket bind/unlink, per-mount goroutine startup, live perf capture, and control-plane logging.
- Mutable Runtime Owner: One-shot and watch each own their current parent child-work snapshots. `RunWatch` owns the live `runners` map. Each `watchRunner` owns one cancel function and one completion channel for exactly one mount.
- Error Boundary: The control plane converts mount startup into structured per-mount startup outcomes, returns one-shot startup classification separately from completed `MountReport` values, and keeps watch-runner failures isolated to the affected mount or log path. Engine-internal errors remain inside the single-mount boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| `RunWatch` starts the runnable runtime mount set, skips incompatible-store mounts with immediate warnings, and rejects all-paused startup through the same startup-summary model. | `TestOrchestrator_RunWatch_SingleMount`, `TestOrchestrator_RunWatch_MultiMount`, `TestOrchestrator_RunWatch_SkipsIncompatibleStoreMountWhenAnotherMountStarts`, `TestOrchestrator_RunWatch_ReturnsErrorWhenAllMountsPaused` |
| The Unix control socket is the single owner lock for one-shot and watch sync, is acquired before parent engines start, reports owner mode/status, rejects unsupported one-shot control requests with typed `foreground_sync_running`, and keeps reload/stop serialized through the watch control loop. Dry-run one-shot sync uses the same owner lock as live one-shot sync. | `TestRunOnce_ControlSocketBlocksWatchOwner`, `TestRunOnce_BindsControlSocketBeforeEngineStartup`, `TestRunOnce_DryRunBindsControlSocketBeforeEngineStartup`, `TestRunWatch_BindsControlSocketBeforeEngineStartup`, `TestOrchestrator_OneShotControlSocket_StatusAndRejectsNonStatus`, `TestOrchestrator_ControlSocket_StatusAndStop`, `TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners` |
| The control socket also exposes live perf snapshots and explicit capture bundles for both one-shot and watch owners without creating a second network surface or durable metrics store. | `TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture`, `TestOrchestrator_OneShotControlSocket_PerfCaptureRejectsInvalidDuration`, `internal/cli/perf_test.go` (`TestMainWithWriters_PerfCaptureJSON_ForOneShotOwner`, `TestMainWithWriters_PerfCaptureFailsWhenNoOwnerIsRunning`) |
| Socket files are permissioned private, stale sockets are removed only after a failed live probe, and empty hash-runtime socket directories are cleaned up on close. | `TestControlSocketServer_PermissionsStaleCleanupAndRuntimeDirRemoval` |
| Control-socket reload applies add/remove/pause/expired-pause/filter diffs to the live runner set without bouncing unaffected mounts. | `TestOrchestrator_Reload_AddDrive`, `TestOrchestrator_Reload_RemoveMount`, `TestOrchestrator_Reload_PausedMount`, `TestOrchestrator_Reload_TimedPauseExpiry`, `TestOrchestrator_Reload_ContentFilterChangeRestartsOnlyAffectedMount` |
| Parent engines own shortcut-root state, alias mutation, protected-root derivation, and durable cleanup retry state before multisync sees child work. | `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain`, `TestSyncStore_markShortcutChildFinalDrainReleasePendingIsDurable`, `TestSyncStore_SamePathUpsertDoesNotDowngradeActiveProtectedOwner`, `TestSyncStore_DuplicateAutomaticShortcutTargetIsParentBlocked`, `TestEngine_AcknowledgeChildFinalDrainReleasesParentShortcutRoot`, `TestEngine_ReconcileShortcutRootLocalStateRetriesRemovedReleasePending`, `TestEngine_ReconcileShortcutRootLocalStatePersistsCleanupBlockedBeforeReturningError`, `TestEngine_ShortcutAliasRenameMutatesThroughParentAndUpdatesRootState`, `TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain` |
| Multisync owns runtime-only shortcut child admission from exact parent snapshots: one-shot stores the exact publication and starts that parent's children only after that parent's safe point, while watch initial startup starts parent runners only and admits children from live parent publications that still match the live runner/cache state. | `TestReceiveParentChildWorkSnapshot_StoresSnapshotInMemory`, `TestReceiveParentChildWorkSnapshot_EmptySnapshotClearsCachedChildren`, `TestRunOnce_PublishesParentChildWorkSnapshotBeforeStartingChildren`, `TestRunOnce_StartsParentChildrenAfterPublishingParentSafePoint`, `TestRunOnce_StartsParentChildrenWithoutWaitingForOtherParents`, `TestRunOnce_UsesFinalParentSnapshotInsteadOfIntermediateSkip`, `TestRunWatch_PublishesParentChildWorkSnapshotBeforeStartingChildren`, `TestRunWatch_ReconcilesChildRunnersFromLiveParentSnapshot`, `TestApplyWatchMountSet_ParentRestartClearsSnapshotAndDoesNotRestartChild`, `TestHandleWatchRunnerEvent_ParentExitStopsChildrenAndForgetsCachedSnapshot`, `TestHandleWatchRunnerEvent_IgnoresStaleParentSnapshotEvents` |
| Multisync executes parent-declared final-drain and artifact-cleanup work without becoming the parent lifecycle owner; cleanup paths use explicit orchestrator `DataDir`, parent-scoped cleanup diagnostics remain transient, and runtime work compilation keeps shortcut child commands scoped to the declaring parent. Final-drain one-shot option rewriting may force bidirectional full reconcile, but it must preserve caller options such as dry-run. | `TestRunOnce_FinalDrainChildRunsBidirectionalFullReconcileAndReleasesAfterSuccess`, `TestBuildEngineWork_FinalDrainChildPreservesDryRunOption`, `TestRunOnce_FinalDrainChildFailureKeepsProjectionReserved`, `TestStartWatchRunner_FinalDrainRunsOnceBidirectionalFullReconcile`, `TestHandleFinalDrainWatchRunnerEvent_DoesNotAckParentWhenDrainErrs`, `TestRunOnce_ParentCleanupRequestPurgesShortcutChildStateArtifacts`, `TestOrchestratorCleanupWithEmptyDataDirFailsLoudly`, `TestOrchestratorPurgeShortcutChildArtifactsClearsDiagnosticsWhenNoCleanupWorkRemains`, `TestBuildRuntimeWorkFromParentChildWorkSnapshot_DoesNotClassifyDuplicateAutomaticChildren`, `TestBuildRuntimeWorkFromParentChildWorkSnapshot_StandaloneContentRootRunsBesideChild`, `TestBuildRuntimeWork_ParentBlockedSnapshotHasNoChildWork`, `TestClassifyShortcutChildDrainResultsOnlyCleanIsAckable`, `TestBuildChildStatusMount_RendersLifecycleState` |

## Runtime Mount Specs

The control plane compiles runtime work from two inputs:
configured standalone parent mounts and parent-declared child work snapshots. The CLI
resolves configured drives and compiles them into
`multisync.StandaloneMountSelection`; valid selections become
`StandaloneMountConfig` values, while per-drive conversion failures become
`MountStartupResult` values before the orchestrator is constructed.

One-shot and watch startup bind the owner control socket before parent engines
start. Each run owns its parent child-work snapshot cache; those snapshots are
discarded with the one-shot/watch runtime instead of living on the
`Orchestrator`. On startup, reload, and parent snapshot events the control
plane:

1. consumes the CLI-compiled standalone mount selection
2. starts selected standalone parent engines with the child work snapshot sink
   attached before the engine begins its normal one-shot or watch bootstrap
3. caches each exact parent-declared child work snapshot in runtime memory when
   the live parent publishes from its normal current-plan path
4. reconciles child runners for that parent directly from normal and
   final-drain child work commands plus explicit cleanup commands

There is no separate shortcut-only startup path and no control-plane startup
publisher. The parent engine derives local protection and child publications
from the same current truth and current plan that it uses for ordinary work.
Watch initial startup starts selected standalone parent runners only. Children
are never admitted from a cached child work snapshot when a watch runtime
starts, or when a parent exits or fails before publishing; multisync stops any
existing children for that parent, forgets that runtime's snapshot for the
parent, and waits for a fresh live parent snapshot before admitting
replacements. Watch snapshot events are buffered process messages, so the watch
loop admits a parent-scoped event only when a live standalone parent runner
still exists and the event snapshot exactly matches the run-owned cache; stale
queued events from an exited/restarted parent are ignored. In one-shot,
multisync records the latest exact snapshot
published by each parent and starts that parent's child work only after that
same parent reaches its one-shot safe point. Parent A's children do not wait
for unrelated parent B, and child engines still run ordinary `RunOnce` passes.
Because one-shot child work begins after the parent safe point, final-drain and
artifact-cleanup acknowledgements can proceed through that same live parent
without a separate parent-done gate.

Managed child mounts are runtime projections declared by the parent engine, not
synthetic configured drives and not durable control-plane inventory.

Runtime mount inputs are discriminated parent/child values. Parent runtime
values carry configured namespace identity, token-owner/session facts, sync
tunables, and the child work sink. Child runtime values carry only the
parent-declared child engine input, child mount ID, final-drain mode, and
opaque acknowledgement reference. Parent values cannot carry child ack refs,
child run mode, expected shortcut identity, or child engine specs. Child values
cannot masquerade as configured standalone drives. The common runner boundary
receives the lowered runtime value only after the kind-specific shape has been
validated.

Watch lifecycle is parent-first and child-safe. Reload stops child runners
before parents and starts parents before children admitted from the run-owned
snapshot cache. If a parent runner exits,
panics, loses its root, is paused/removed by reload, or fails startup,
multisync immediately cancels all child runners whose `parentMountID` matches
that parent and forgets the runtime child work snapshot for that parent.
Replacement children can start only after the live parent has published fresh
child work commands through the normal engine path.

Runtime mount values do not carry parent-owned protected child paths or
protected roots; parent engines rebuild those from their own `shortcut_roots`.
`mountSpec` no longer carries `ResolvedDrive`, and `OrchestratorConfig` no
longer accepts resolved-drive values. Configured drives are compiled at the CLI
edge into `StandaloneMountConfig`, and sync session construction consumes those
facts through `driveops.MountSessionConfig`. `internal/multisync` derives
`sync.EngineMountConfig` from the runtime mount value. For shortcut children it
asks sync-owned child command helpers to validate and lower the parent command
into engine config, rather than inspecting shortcut lifecycle, protected-path,
or parent status facts.

`OrchestratorConfig.DataDir` is also explicit. The CLI/config composition
boundary may call `config.DefaultDataDir()`, but `internal/multisync` never
falls back to that ambient path. Child artifact cleanup fails as setup error
when the data directory dependency is missing, which keeps cleanup scope tied
to the orchestrator invocation and makes tests inject the directory they use.

Runtime reporting is mount-identified. Standalone mounts keep their configured
canonical drive ID inside `MountIdentity`, but managed child mounts report by
stable `MountID` and do not synthesize `shared:` canonical drive IDs. CLI
status output follows the same boundary: child rows expose `mount_id` and omit
`canonical_id`, while standalone rows retain `canonical_id`. JSON nests child
rows below their parent mount in `child_mounts`, and text output indents child
rows beneath the parent drive so the control surface remains parent-owned.

Managed child token owner and sync tunables are inherited from the selected
standalone namespace mount. There is no child pause, resume, reset, config, or
CLI control surface. A child projection is controlled by the OneDrive shortcut
itself and by the parent drive's pause state. Explicit standalone shared-folder
mounts may project the same remote content root as automatic shortcut children
when their configured local `sync_dir`s do not overlap. Duplicate automatic
shortcuts inside one parent remain parent-engine shortcut-root state.

Shortcut lifecycle state is producer-owned:

| Situation | Durable state | Runner behavior | Parent protection |
| --- | --- | --- | --- |
| Shortcut binding is healthy | parent root `active` | child runner may start unless parent is paused | current child path |
| Parent drive is paused | parent root `active` | child runner is paused with the parent | current child path |
| Binding target cannot be refreshed | parent root `target_unavailable` | no child work command; retry on parent refresh | current child path |
| Shortcut was authoritatively removed | parent root `removed_final_drain` | child runs a final bidirectional full sync, then stops after parent release | current and reserved paths until finalized |
| Clean final drain acknowledged, parent release not yet complete | parent root `removed_release_pending` | child is already drained; release cleanup retries in parent | current and reserved paths until finalized |
| Removed shortcut final drain cannot reach the shared-folder target | parent root `removed_final_drain` plus child retry/block state | child final-drain runner retries normally; status guides target-access recovery | current and reserved paths until finalized |
| Removed shortcut cleanup cannot release the alias root | parent root `removed_cleanup_blocked` | child is already drained; release cleanup retries in parent | current and reserved paths until finalized |
| Parent released the alias root and child artifacts still need purge | parent root `removed_child_cleanup_pending` | no child runner; multisync purges child-owned artifacts and acknowledges completion | none |
| Same-path replacement arrives before old child finalizes | parent root with `Waiting` replacement | old child drains first, new child starts after parent promotion | shared path remains reserved |
| Duplicate automatic shortcut to same target in one parent | parent root `duplicate_target` | duplicate root does not publish a child work command | conflicting child paths |
| Parent cannot safely move or inspect a renamed shortcut alias root | parent root `blocked_path` or `removed_cleanup_blocked` | no child work command while parent retries or waits for user action | current and reserved paths |
| Child local root is a file, final symlink, or unsafe path | parent root `blocked_path` | no child work command | child path |
| Child local root is unavailable while the parent still owns the alias lifecycle | parent root `local_root_unavailable` | no child work command until parent retry or user recovery | child path |
| Previously materialized child local root is renamed to one same-parent identity match | parent root remains protected while parent alias rename is applied | same child state DB restarts at new path | current path plus same-parent identity protected-root |
| Previously materialized child local root is missing with no same-parent identity match | parent deletes only the shortcut alias, then emits a final-drain child work command | child final-drains if runnable, then releases | child path until finalized |
| Previously materialized child local root has multiple same-parent identity matches | parent root `rename_ambiguous` | no child work command until user resolves ambiguity | current and candidate paths |
| Local alias rename/delete cannot mutate the placeholder | parent root `alias_mutation_blocked` | no child work command while parent retries | current and candidate paths |

Shortcut child work publication is split by authority. The parent engine is the
only runtime that calls Graph delta/list/get for the parent drive. It
classifies shortcut placeholder observations, persists parent-owned
`shortcut_roots`, suppresses those aliases from normal content planning,
mutates shortcut placeholders by `binding_item_id`, and publishes child work
commands to `internal/multisync`:

- normal child work commands start or continue child runners
- final-drain child work commands tell multisync to run the child once in
  bidirectional full-reconcile mode
- cleanup requests tell multisync to purge child-owned artifacts after child
  sync is clean and the parent has released its protected alias path; each
  request carries an opaque acknowledgement reference, child mount ID, local
  root, and cleanup reason, and multisync rejects incomplete requests instead
  of deriving scope from parent namespace data
- work snapshots contain only execution data and cleanup commands, so
  cleanup is an explicit parent-published executor request rather than inferred
  absence from a later snapshot
- complete batches mark previously known but absent parent roots
  `removed_final_drain`
- empty complete batches are applied through the same engine handler path
  because they mean the parent engine completed a parent-drive enumeration and
  saw no current shortcut aliases; empty incremental batches are skipped
- same-path replacement stays in parent `shortcut_roots` as a waiting
  replacement until the old child final drain is acknowledged
- all active, conflicted, unavailable, waiting, release-pending, and
  removed-final-drain paths
  remain parent protected paths
- runnable child work snapshots carry the parent-observed local root identity when
  available; child engines verify that identity before startup and full scans
  so a deleted/recreated or moved projection cannot be mistaken for an empty
  tree to sync
- in one-shot mode, each configured parent runs as an ordinary engine and
  children are admitted from that parent's latest exact child-work snapshot only
  after that parent reaches its safe point. No parent waits for unrelated
  parents before its children can run. The one-shot child-run coordinator only
  owns report aggregation, artifact cleanup, and live-parent acknowledgement
  ordering; children still execute ordinary engine `RunOnce` passes
- successful child work mutation in watch mode publishes a new
  parent-owned child snapshot through the live parent runner; multisync
  reconciles only that parent's child runners without stopping the parent

If the parent sync store cannot durably accept a topology batch, the parent
engine does not commit its remote observation cursor. The same Graph facts
replay later from the parent engine, preserving the one-observer ownership
boundary without letting the control plane rediscover remote state.

Parent shortcut lifecycle transition decisions are centralized in the sync
engine planner. The parent shell consumes remote shortcut observations,
protected-root local observations, stored `shortcut_roots`, and child-drain
acknowledgements, executes Graph/filesystem/store effects at its own I/O
boundary, and feeds those value outcomes through planner helpers. Multisync
consumes only the resulting child work snapshot: start normal child
commands, run final-drain commands to final drain, purge explicit cleanup
commands, acknowledge clean drain through the live parent
`ShortcutChildAckHandle`, then stop and forget child runtime state after the
parent release succeeds.

Runtime mount-set construction does not inspect or mutate parent shortcut alias
roots. Parent engines create, reserve, move, block, or release alias
projections inside their sync root. Child engine construction may still fail
with `ErrMountRootUnavailable` when the parent has not made a runnable child
root available; that is reported as mount startup state rather than converted
into parent alias policy by multisync.

Authoritative removal is a two-owner lifecycle transition. The parent marks the
shortcut root `removed_final_drain` and keeps its protected path active. The
control plane runs the child as an ordinary bidirectional full reconcile so
local content changes in the projection can reach the shared-folder target. If
the child reports retry/block/content failures, root unavailability, target
access loss, or no mount report, the typed final-drain result is not
acknowledged and the parent protection remains. The child state DB remains the
owner of dirty content retry/block state. Only a `clean` final-drain result
produces an opaque acknowledgement to the live parent engine. The parent
first persists `removed_release_pending`, then idempotently removes/releases
the alias projection or promotes a waiting replacement. If release cleanup is
blocked, the parent persists `removed_cleanup_blocked` and retries from that
durable truth on startup or later parent refresh. When release cleanup
succeeds, the old binding stays in the parent store as
`removed_child_cleanup_pending`: the alias path is no longer protected and no
child runner is published, but the parent explicitly publishes a child artifact
cleanup request. Multisync purges the child-owned DB, SQLite sidecars, catalog
residue, and upload sessions, then acknowledges artifact cleanup to the same
live parent engine. The parent deletes the cleanup-pending row only after that
acknowledgement, so cleanup retry is durable and parent-owned. Cleanup
diagnostics on the control socket are transient executor status: a failure
records the source and domain class, and a later successful retry clears that
source's old diagnostic. An empty cleanup work set only clears old published
cleanup diagnostics when it covers every configured parent; parent-scoped watch
reconciliation must not clear another parent's active cleanup failure.

Local alias deletion is explicit and local-only. If the user deletes the local
shortcut alias while the parent root is active, the parent deletes only the
shortcut placeholder and emits a final-drain child work command. This does not delete
the shared-folder target and does not mutate configured standalone shared-drive
catalog records.

Local shortcut alias deletion and same-parent local shortcut alias rename are
not target-content delete or target-content rename operations. Parent engines
own the local observation and shortcut placeholder mutation by
`binding_item_id`. If the alias directory later appears at exactly one sibling
path with the same stored identity, the parent alias rename is applied and the
same child mount ID restarts at the new path. If the alias disappears with no
same-parent identity candidate, the parent deletes only the shortcut alias and
emits a final-drain child work command. During all blocked or retiring states, parent
observation suppresses protected roots so they are not uploaded into the parent
drive. Cross-parent behavior is unsupported by design: parent engines are
isolated by account/namespace authority and never compare shortcut roots across
parents.

Status is the guided recovery surface for these states. Non-active shortcut
children expose the protected current path, reserved previous/candidate paths,
state/reason/detail, concise `recovery_action`, `auto_retry`, and waiting
replacement state when present from the parent shortcut-root status snapshot,
plus sync-owned typed issue/recovery classes for JSON consumers. None of this
status-only metadata comes from multisync child work snapshots.

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

Parent engines rebuild protected child-root paths from parent-owned
`shortcut_roots`: reserved relative paths, binding IDs, target identity, and
optional filesystem identity. Multisync receives only the parent-declared child
child work snapshots derived from those rows. Parent namespace decisions
and retry/block state remain in the parent engine.

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
root conflicts. Paused standalone parents are preserved as paused mount inputs
without sync-dir structural validation, so stale dormant config cannot turn a
paused drive into a fatal startup failure for unrelated runnable mounts. When a
drive is unpaused, normal conversion and engine startup validation apply again.
Runnable mounts then produce one completed `MountReport` each,
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
- `GET /v1/perf` returns the owner mode plus the live aggregate and per-mount perf snapshots currently owned by the active sync runtime. This includes path-free stale-work, local-observation, and replan-idle aggregates. The surface is live-only and returns whatever the owner has collected so far; it does not materialize historical perf state from SQLite.
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
remain running only when their runtime mount specs are equivalent. Content
filter fields (`included_dirs`, `ignored_dirs`, `ignored_paths`,
`ignore_dotfiles`, `ignore_junk_files`, `follow_symlinks`) are part of that
equivalence check, so a filter config change restarts the affected runner and
forces fresh startup observation under the new visibility policy. When a timed
pause has already expired by reload time, the config keys are cleaned up but the
running mount is not bounced.

Reload compiles active non-paused mounts without creating sync roots. A newly
added or resumed drive whose resolved `sync_dir` is missing is still included in
the new runtime set; its engine startup records `ErrMountRootUnavailable` as a
per-mount startup failure while the reload continues applying unrelated
add/remove/pause changes.

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
- `--dry-run` binds, probes, and unlinks the control socket like any one-shot
  sync owner; it only changes plan execution and commit semantics
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
