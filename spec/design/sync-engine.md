# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/engine_watch*.go, internal/sync/engine_runtime*.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/protected_roots.go, internal/sync/shortcut_root_lifecycle.go, internal/sync/shortcut_root_transition.go, internal/sync/shortcut_root_publication.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_capability.go, internal/sync/permission_evidence.go, internal/sync/permission_probe_local.go, internal/sync/permission_probe_remote.go, internal/sync/observation_findings.go, internal/cli/sync_flow.go, internal/cli/sync_runtime.go

Implements: R-2.1 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.10 [designed], R-2.14 [designed], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified]

## Overview

The engine is the single mounted content-root runtime owner. It coordinates:

- observation
- planning
- publication-only action commits
- execution
- durable outcome writes
- retry and trial scheduling
- scope lifecycle
- watch-mode refresh and maintenance work

The target engine persists durable content-sync status through three authorities:

- `observation_issues`
- `retry_work`
- `block_scopes`

It does not use a mixed failure table as durable control state.
Parent namespace engines also persist parent-owned shortcut alias lifecycle in
`shortcut_roots`. Those rows protect child-root paths from ordinary parent
content planning, record parent-scoped blockers and retries, and declare child
topology for the control plane. They are not child content retry state: child
engines still own `observation_issues`, `retry_work`, and `block_scopes` for the
shared-folder target content inside the child projection.

`retry_work` and `block_scopes` are engine-owned control state, not
best-effort diagnostics. If the runtime cannot durably record or transition
required retry/scope state after an exact action result or admission decision,
it fails closed and terminates the current runtime. Product-facing last-sync
history is not a durable engine authority.

`observation_findings.go` is the engine-owned constructor boundary for
observation batches. Engine orchestration chooses when to reconcile those
batches durably or into a scratch planning store, but callers should not
assemble overlapping observation-managed batch shapes ad hoc.

## Ownership Contract

- Owns: single-mount runtime orchestration, watch-mode mutable state,
  worker-result classification, retry/trial scheduling, and scope lifecycle.
- Does Not Own: SQLite schema, Graph normalization, config parsing, or
  multi-mount daemon lifecycle.
- Source of Truth: durable sync state in `SyncStore`, plus engine-owned
  in-memory runtime state for the currently running session.
- Allowed Side Effects: coordinating observers, planner, executor, and store
  writes through injected boundaries.
- Mutable Runtime Owner: `engineFlow` is the single per-run mutable owner.
  One-shot and watch both execute through that same runtime state; watch keeps
  it alive across timer ticks and observation batches.
- Error Boundary: the engine translates observer, planner, executor,
  permission, and store outcomes into engine-owned reports, retries, scope
  transitions, and durable authority writes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| One-shot sync remains a bounded observe-plan-execute pass without a live user-intent mailbox. | `TestBootstrapSync_NoChanges`, `TestBootstrapSync_WithChanges`, `TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork`, `TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady` |
| One-shot and watch share the same admission/runtime contract, while watch alone keeps the runtime alive for future timer release. | `TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue`, `TestReleaseDueHeldRetriesNow_ReleasesHeldRetryEntriesOnly`, `TestReleaseDueHeldTrialsNow_ReleasesFirstHeldScopeCandidateAsTrial`, `TestWatchRuntime_HandleWatchHeldRelease_RetryTickReducesReleasedPublicationRetryOnEngineSide`, `TestWatchRuntime_RunNonDrainingWatchStep_BootstrapRetryTickReducesReleasedPublicationRetryOnEngineSide`, `TestPhase0_OneShotEngineLoop_TrialSuccessMakesFailuresRetryableAndReinjectableWithoutExternalObservation` |
| Parent engines persist shortcut-root state, merge that state into protected-root observation filters on startup, route protected-root lifecycle signals through the parent engine, and suppress/report protected roots without turning them into parent content. | `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots`, `TestNewMountEngine_DoesNotProtectCleanupPendingShortcutRoot`, `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler`, `TestFullScan_ProtectedRootIdentityMatchSuppressesRenamedRoot`, `TestFullScan_ExpectedSyncRootIdentityMismatchReturnsMountRootUnavailable`, `TestEngine_ReconcileRemovedFinalDrainMissingLocalAliasReleasesWithoutRemoteDelete` |
| Parent shortcut-root transitions are table-validated and watch-mode alias lifecycle stays engine-internal before only child runner-action publications reach multisync. | `TestShortcutRootTransitionTableCoversStates`, `TestValidateShortcutRootTransitionAllowsKnownLifecycleEdges`, `TestValidateShortcutRootTransitionRejectsIllegalLifecycleEdges`, `TestWatchRuntime_HandleProtectedRootEventOwnsLocalAliasRename` |

## Construction

`newEngine()` wires one mounted content root into a runtime:

- rooted sync tree
- store
- planner
- executor configuration
- transfer manager
- permission handler
- optional websocket wake source

Production entrypoints now call `NewMountEngine()` with:

- the authenticated session capabilities for the target mount
- `EngineMountConfig`, the sync-owned constructor input carrying the non-client
  runtime facts for that mount
- logger, perf collector, and drive-verification flag

`NewMountEngine()` is the only exported engine constructor. Config-shaped
inputs such as `ResolvedDrive` are compiled into `EngineMountConfig` above the
engine boundary; there is no extra exported builder layer above
`EngineMountConfig`, and `engineInputs` remains an internal seam for focused
engine tests rather than a parallel production construction model.

For mount-root runtimes, the engine also carries the configured
`remoteRootItemID`. That root item defines the remote boundary for scoped
observation, planning, and execution. Planner and executor consume that
engine-owned root context directly; ordinary actions do not re-thread
per-action target-root overrides. Mount-root delta capability is
resolved in config and passed into the engine as construction input; the
engine does not reopen catalog state just to decide whether a mount root
should try folder delta. Separately configured shared folders and managed
shortcut child mounts both use this mount-root engine path when their remote
root is below the backing drive root.

Permission handling is intentionally split three ways:

- probe/evidence (`permission_probe_*.go`, `permission_evidence.go`) gathers
  filesystem or Graph facts only
- runtime classification (`engine_result_classify.go`) owns the condition
  family and ordinary retry/scope class for the finished action
- direct engine runtime permission handlers (`engine_runtime_permissions.go`
  and `engine_runtime_lifecycle.go`) gather probe evidence when needed,
  persist exact retry work or blocked scope rows, activate or release timed
  write scopes, and emit engine-owned logs without an intermediate policy DTO

Normal completion handling and trial reclassification both reuse the same
engine-owned permission-evidence handlers. The probe still returns facts only;
the engine decides directly whether to persist delayed retry work, persist
blocked retry work, activate a timed scope, or fall back to generic result
handling.

The remote permission probe walks boundaries directly from the engine-owned
`driveID` and `remoteRootItemID`. There is no separate remote-root carrier object;
the root item ID is the only special case, and all ancestor walking uses the
same boundary-path helpers the probe already owns.

Permission timing follows the engine-owned runtime decision, not the probe:

- file-scoped permission failures persist delayed `retry_work` and arm the
  ordinary retry timer in watch mode
- boundary-scoped permission failures persist blocked `retry_work` and any
  timed write scope they own, but they do not arm the ordinary retry timer
- known-active-boundary outcomes are a true no-op for durable state and timer
  arming because the boundary is already active
- unmatched permission evidence falls back to the generic classified result
  path; trial reclassification reuses that same fallback instead of treating
  inconclusive permission probes as fatal runtime errors

## One-Shot Sync

`RunOnce()` keeps one-shot behavior intentionally simple:

1. shared startup prep and durable startup checks
2. observe and commit current remote/local truth once
3. load planner inputs once from that committed truth
4. compute SQL structural diff and reconciliation once
5. build the current actionable set in Go from structural reconciliation plus
   explicit truth-availability overlays
6. reconcile durable retry/blocker state to that actionable set
7. commit any ready publication-only actions directly through the store and
   drain publication-only dependents before worker dispatch
8. execute remaining concrete work once using the same blocker/trial admission
   model watch mode uses
9. persist outcomes and return a report

There is no mid-pass mailbox for user intent. New external DB writes during a
one-shot run are durable state for a later run.

The current-plan pipeline is the handoff boundary between planning and runtime
startup. Observation remains entrypoint-specific, but once an entrypoint has
produced observed current truth the engine runs the same named stage sequence:
observe current truth, load current inputs, build the current plan, then
reconcile runtime state by pruning/loading surviving `retry_work` /
`block_scopes`. In code, `engine_current_plan.go` owns that shared
observe/load/build/reconcile pipeline. Stale `retry_work` and empty
`block_scopes` are pruned there, not from timer held-release paths.

Scope startup cleanup follows the same policy with a deliberate
decision-then-apply split: the engine first derives which persisted scopes are
still justified by blocked `retry_work`, then applies only the required delete
mutations. The same timed-scope liveness rule also drives runtime
rearm-or-discard handling and store-side prune helpers so empty timed scopes
do not survive by accident in one path but not another.

Within that one-shot flow, the engine now treats current-plan construction as
an explicit stage sequence: observe current truth, load current inputs, build
the current plan from that observed state, then either reconcile runtime state
or keep the dry-run build in memory without touching durable held-work state.
Live, dry-run, watch bootstrap, and steady-state watch replans all use that
same current-plan pipeline; they differ only in how they collected the
observed state and whether a deferred cursor commit is present.
The top-level coordinators should stay at that stage level rather than
inlining planner input loads, durable prune/load logic, or runtime-start
bookkeeping. The same rule applies to the explicit runtime-start,
publication-drain, and post-sync housekeeping stages: keep them next to their
stage so a reader can see the flow at a glance. Current-plan construction
should read from `engine_current_plan.go`; runtime-start/admission should read
from `engine_runtime_start.go`; and completion plus publication drain should
read from `engine_runtime_completion.go` plus the trial-specific
`engine_runtime_completion_trial.go`.

Parent child-admission readiness is part of the normal parent run path.
Multisync attaches a child runner publication sink before it starts a selected
parent engine, then waits for that live parent to publish from the normal
current-plan pipeline.
That pipeline performs the same remote observation cadence decision, full local
observation, current-plan build, retry/block reconciliation, and shortcut-root
lifecycle publication the parent needs for ordinary work. One-shot publishes
before committing deferred remote observation progress; each one-shot parent
publication starts that parent child work immediately, without waiting for
unrelated parents to finish. Watch bootstrap and steady-state changes publish
through the live parent runner and reconcile only that parent child runner set.
Child runner admission is therefore derived from fresh parent local and remote
truth rather than cached control-plane state, and multisync never constructs a
temporary startup parent engine for shortcut admission.

Once one-shot shutdown has started, late worker completions no longer re-enter
the normal outbox path. The engine runs them through the same shutdown
completion boundary watch drain uses, immediately collapsing any newly-ready
frontier back into shutdown completion instead of handing it to dispatch.
Likewise, one-shot idle waiting must release any already-due held retry or
trial work before blocking on worker completions; held-work timing remains part
of the shared engine runtime, not a watch-only side path. If idle held-release
reduction fails after shutdown-completing a returned exact frontier, the
one-shot runner consumes that shutdown work immediately instead of surfacing the
same outbox to a second shutdown-completion pass.

Full-remote-refresh cadence is restart-safe even when a full remote refresh
returns no delta cursor. The engine still advances the persisted cadence in
`observation_state` so enumerate-only and mount-root sessions do not fall
into back-to-back expensive full refreshes.

That cadence is capability-driven, not websocket-driven:

- delta-based observation schedules the next full remote refresh 24 hours out
- enumerate-only observation schedules it 1 hour out

Websocket wakeups are additive only. They wake delta polling sooner, but they
do not replace delta polling and they do not change the full-refresh cadence.

If watch mode shortens `observation_state.next_full_remote_refresh_at` after
startup, it must rearm the in-memory timer in that same control path.
Mount-root enumerate fallback therefore clamps the persisted deadline and
immediately rebuilds the active timer instead of waiting for a later full
refresh commit.

In this increment, "degraded" means exactly "running without delta." The main
drive-root watch path remains delta-based. Mount-root watch chooses its
mode from the config-resolved capability surface:

- business/sharepoint mount roots skip folder delta and use recursive
  enumeration
- personal mount roots try folder delta first and retry it on later passes
  after any temporary enumerate fallback

## Watch Mode

`RunWatch()` is the long-lived runtime. It owns:

- observer startup and shutdown
- dirty-signal intake and debounce
- snapshot refresh and SQLite reconciliation
- action admission and dispatch
- action completion drain
- retry and trial timer scheduling
- periodic maintenance ticks and full remote refresh
- graceful drain on shutdown

`RunWatch` starts with the same shared startup boundary one-shot uses.
That startup boundary owns persisted-scope normalization, account-auth
normalization, and the single startup baseline load. `bootstrapSync`
consumes that startup baseline; it is not allowed to lazily reload or
recreate startup state.

The watch loop is the single owner of mutable scheduling/runtime state:
bootstrap/running/drain phase, outbox, dispatch admission, held-work timing,
refresh coordination, and drain behavior. Remote observation commits now
follow the same single-owner rule:
watch observers and full-refresh goroutines emit one loop-applied
`remoteObservationBatch` value, and the loop itself owns projected remote
observation commits, cursor commits, observation-finding reconciliation, dirty
marking, and refresh-timer re-arm.
Observer lifecycle follows that same runtime-owned model. `startObservers`
populates the watch runtime's observer error stream, local-event stream,
remote-batch stream, skipped-item stream, refresh channels, and active-observer
count directly. `watchPipeline` keeps only loop inputs that are not already
runtime-owned: baseline, replan-ready debounce output, worker completions,
maintenance ticks, worker pool, and cleanup.
That owner boundary stays concrete in code too: `runWatchLoop` is the shared
outer owner for bootstrap, steady-state, and drain; `runNonDrainingWatchStep`
is the one gated non-draining `select`; and `runDrainStep` remains the only
distinct shutdown shell instead of routing bootstrap, idle, and outbox cases
through overlapping wrappers.
Engine debug events expose those lifecycle points for tests and diagnostics
only; they do not own or redirect control flow.
Refresh timer callbacks and full-refresh goroutines signal through stable
runtime-owned channels for the lifetime of the watch session. The watch loop
disables select cases by phase instead of mutating those channel pointers
mid-shutdown, so asynchronous senders never race the single-owner runtime over
which signal channel is authoritative.

Local watcher events, remote delta batches, websocket wakes, and full remote
refresh results are scheduler hints only. After debounce or wake, watch
mode refreshes current truth, runs SQL comparison/reconciliation, rebuilds the
current actionable set in Go, reconciles durable retry/blocker state, and then
admits runnable actions. There is one steady-state replan entry for that work:
refresh local truth, load the already-committed remote/current state, build the
current plan, reconcile runtime state, then append the resulting concrete
worker frontier through the watch-owned frontier helpers. DirtyBuffer emits
only a coarse dirty/full-refresh scheduler signal, and that signal feeds only
this steady-state replan path; it does not define a second planning model.

Bootstrap uses that same outer owner after the initial
observe/build/reconcile/start-runtime handoff. The only bootstrap-specific semantics
that remain are the bootstrap quiescence rule, bootstrap wait logging, and the
fact that observers do not start until that bootstrap-owned quiescent boundary
has been reached.

Watch runtime replacement is linear: one current runtime graph at a time.
Dirty observation while work is still queued or running sets a pending replan
flag instead of appending a second graph into the current runtime. Once the
runtime reaches the idle boundary, the loop rebuilds from current committed
truth plus durable `retry_work` / `block_scopes`. The idle watch-step owner
still receives debounced coarse dirty hints directly; an empty outbox does not
mean steady-state replans can be deferred until some other watch event arrives.

Retry/trial is not an alternate planner. Timer-driven follow-up only
re-releases exact held actions that are already part of the current runtime.
The engine holds dependency-ready exact work in memory, keyed by exact
`RetryWorkKey` and grouped by `ScopeKey` for blocked scopes. Timer ticks do
not rebuild subset plans, do not compute dependency closure, and do not
revalidate stale rows. Stale-row cleanup belongs only to normal
current-plan build/runtime-state reconcile. Dependency tracking stays inside `DepGraph`, but runtime
completion does not: the engine owns quiescence and no longer waits on a
graph-owned completion signal.

Released held work always re-enters the engine-owned publication-drain stage
before any worker dispatch. Timer-released `ActionUpdateSynced` and
`ActionCleanup` actions stay engine-side, commit through the store, and unlock
dependents without ever crossing into the worker pool.

Action completion drain stays inside the engine boundary. When a completion
unlocks publication-only dependents, watch mode commits those mutations
synchronously and keeps draining them on the engine/store side until concrete
worker actions are the only dispatchable work left. That publication drain is
an effectful engine/store stage, not a pure transform: it durably applies
publication mutations, routes publication failures through normal completion
classification, and returns only the remaining concrete worker frontier.
Successful worker actions and successful publication-only actions still reuse
the same exact-action success bookkeeping: mark finished, increment success
counts, clear exact retry state, optionally feed scope-success detection, then
admit newly-ready dependents.
One-shot and watch coordinators still own their outbox state explicitly.

Runtime completion handling follows the same boundary shape everywhere:
classify the finished exact action, apply the resulting durable/runtime
mutation, then reduce the ready frontier through publication drain plus due-
held release. It does not mix that decision step with worker-queue ownership.
In code, the top-level effectful boundary is `applyRuntimeCompletionStage`,
while `reduceReadyFrontierStage` owns the frontier reduction and
`runPublicationDrainStage` remains the publication-only substage.

Watch replan failure policy is also explicit. Pre-authority local observation
failure is recoverable and drops that replan trigger. Once the engine starts
depending on authoritative current-truth writes or runtime state, failure is
fatal to the current watch session: remote observation apply, observation
findings reconciliation, local snapshot commit, current-plan build from
committed truth, and runtime startup/admission all fail closed. Shutdown cancellation is the
one exception: if context cancellation lands during that steady-state replan
handoff, the loop clears the best-effort sync-status batch and exits cleanly
into shutdown instead of surfacing a fatal watch error.

Once shutdown drain has sealed new admission, late action-completion
classification or persistence errors are treated as shutdown bookkeeping only.
The loop logs them, keeps draining the already-owned completion sources, and
returns clean shutdown rather than converting cancellation timing into a fatal
watch error. That same shutdown-completion boundary now lives in
`engine_runtime_shutdown.go` and is shared by watch drain plus one-shot
late-completion handling after fatal shutdown or cancellation.

### Maintenance And Refresh

Watch mode still owns periodic maintenance ticks for summary logging and full
remote refresh cadence, but it no longer polls SQLite for mysterious external
runtime-state changes. Live scope/runtime state changes must arrive through the
engine's own control paths; there is no generic external DB reconciliation
loop.

Watch summary grouping is engine-owned. `watch_summary.go` builds only raw
watch-condition aggregates: condition-key counts, total condition count,
retrying count, and raw remote-write-block groups keyed by `ScopeKey`. Those
remote-write groups come only from active remote-write
`block_scopes`, not from every projected condition group that happens to share
the same condition key. The watch runtime owns log phrasing and churn
suppression separately. Signature/fingerprint helpers belong in that watch-log
boundary too, but the fingerprint itself must stay raw-only and must not
depend on the current human-readable breakdown wording. The store does not own
grouped watch-condition projections or watch-specific presentation.

## Drive-Root And Mount-Root Runtime Shapes

The engine supports two runtime shapes:

- drive-root sessions rooted at the remote drive root
- mount-root sessions rooted below the remote drive root

The constructor chooses the runtime shape from `EngineMountConfig.RemoteRootItemID`.
A blank `RemoteRootItemID` means drive-root observation and execution. A non-blank
`RemoteRootItemID` means the engine must stay scoped below that remote boundary for
observation, planning, and execution.

Separately configured shared folders and managed shortcut child mounts use the
mount-root path when their content root is below the backing drive root, but
those product surfaces should not be confused with the engine's internal
runtime model.

Embedded shared-folder links discovered inside another synced drive are still
suppressed by ordinary drive-root content observation and never become nested
engine-owned sub-sessions. For parent namespace mounts, the same observer emits
those placeholders as shortcut publication facts before remote cursor commit. The
parent engine also persists parent-owned `shortcut_roots` state in its sync
store: binding item ID, alias path, target identity, protected parent-local
paths, lifecycle state, and same-path replacement waiting state. The
multi-mount control plane consumes the parent-declared child runner actions only to
start, drain, skip, or purge child runners.

Managed shortcut child runner publications also carry the parent-observed local root
identity when the parent has materialized the alias directory. Child engines
verify that identity at construction and before full local scans. If the local
root disappeared, moved away, or was deleted and recreated at the same path, the
engine reports `ErrMountRootUnavailable` instead of producing an empty local
snapshot that could plan remote deletes.

Shortcut placeholder rename/delete mutations are parent-engine operations by
binding item ID. The parent engine observes the need for local alias
rename/delete from its protected-root scan/watch path, applies the Graph
mutation itself, persists the retry/block state in `shortcut_roots`, and then
publishes the resulting child runner actions. Multisync must not rediscover parent
remote state, call parent-drive alias mutation APIs, or decide parent alias
lifecycle.

When a child final drain completes, multisync acknowledges that completion to
the already-running parent engine. The parent engine first persists
`removed_release_pending`, then releases its own protected alias projection or
promotes a waiting same-path replacement to `active`. After parent release, the
old binding remains in `removed_child_cleanup_pending` without protected paths
and publishes a child artifact cleanup request. Multisync purges the
child-owned artifacts and acknowledges cleanup; only then does the parent
delete the cleanup-pending row. If cleanup is interrupted or blocked, startup
and later topology refresh retry the release from `removed_release_pending` or
`removed_cleanup_blocked`; a later complete topology batch is not required to
release that parent protected-root.

If the parent later observes that a retiring or cleanup-blocked shortcut alias
directory is gone, it treats that as user-directed manual discard of the local
projection. The parent removes the shortcut root, or promotes a same-path
waiting replacement, without calling the shortcut delete/rename Graph mutation
path and without interpreting the missing directory as child content deletion.
Multisync receives the release through a parent cleanup request publication and
purges child-owned state.

If a mounted sync root disappears, the engine treats that as mount lifecycle
(`ErrMountRootUnavailable`) rather than as content deletion below the root.
Startup and watch root-loss checks stop or fail the mount before planner input
can be built from a missing root.

## Conflict Handling

Conflicts remain engine-owned and immediate:

- edit/edit and create/create preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path
- edit/delete is planner-expanded into a local-wins upload
- executor-time local-delete hash mismatch reports a stale precondition so the
  next replan can emit the correct upload from current truth

There is no durable conflict-request workflow and no CLI `resolve` command.

## Outcome And Scope Lifecycle

The engine classifies results into:

- success cleanup
- retryable transient work
- actionable current-truth/content problems
- shared blocker activation / re-arm / release-or-discard decisions

### Durable persistence rules

- observation-time invalid or unsyncable truth -> `observation_issues`
- exhausted transient exact work -> `retry_work`
- shared blockers -> `block_scopes` plus blocked `retry_work`
- normal worker-result handling and retry/trial never write
  `observation_issues`; only observation-owned reconciliation does
- execution may still persist `retry_work` for a failed exact action even when
  a later observation pass may record the corresponding durable current-truth
  issue; planning suppression plus retry pruning collapses that overlap on the
  next normal current-plan cycle
- durable `retry_work` / `block_scopes` mutations are fail-closed runtime
  control writes; if the engine cannot persist or transition them, it stops
  the current runtime instead of logging and continuing

### Runtime admission model

`DepGraph` remains dependency-only. The engine still performs final scope
admission after dependency readiness:

- build the graph from current actions
- ask the graph for ready work
- gate that ready work against active scopes
- dispatch allowed work
- keep exact failed/blocked roots represented durably in `retry_work`

Held work stays unresolved in the dependency graph. Dependents are not
persisted as cascade retry rows; they remain blocked naturally until the exact
parent action succeeds, is released for retry, or the whole runtime is
replaced by a fresh current-plan cycle.

The benchmark target is earlier durable reconciliation, not removal of the
post-graph admission gate.

### Scope taxonomy

The target scope families are:

- `service`
- `throttle:target:drive:<driveID>`
- `quota:own`
- `disk:local`
- `perm:dir:write:<path>`
- `perm:remote:write:<boundary>`
- `account:throttled`

Write-side shared permission blockers are first-class persisted
`block_scopes`. Read-side subtree blockers are observation-owned boundary facts
carried on `observation_issues` via `ScopeKey`, not a second persisted scope
table.

### Permission recovery

Permission blockers are revalidated automatically; there is no manual retry or
manual maintenance CLI for them. Observation may create or clear read-boundary
facts directly when current truth proves the blocker or its recovery. Probe and
execution may create or clear persisted write scopes when write access is
affirmatively denied or restored. A raw `403` or `os.ErrPermission` is only a
trigger to probe; the probe returns evidence only, and only the engine-owned
permission handlers may activate or release a write-side scope.

Ownership splits by access kind:

- read boundaries (`perm:dir:read`, `perm:remote:read`) are observation-owned
  boundary tags on `observation_issues`
- write scopes (`perm:dir:write`, `perm:remote:write`) are
  probe/execution-owned persisted `block_scopes`
- file-level local permission failures remain exact `retry_work` rows with the
  normal reconcile backoff; they are not promoted into `block_scopes` unless a
  later probe proves a subtree-wide boundary

Permission handling may still tag blocked `retry_work` with a read-boundary
`ScopeKey` for derived truth and reporting, but only
`ScopeKey.PersistsInBlockScopes()` outcomes may materialize `block_scopes`
rows.

Read boundaries clear only when a later observation batch no longer proves the
corresponding issue-boundary fact.
Remote write block scopes clear through normal timed trials, successful write work, or
cleanup that leaves no blocked work. They do not have a separate global
maintenance cadence.

Remote `403` handling is intentionally narrow:

- raw `403` never creates a permission block scope by itself
- only remote-write actions may invoke remote write-denial probing
- probe-confirmed write denial may activate `perm:remote:write`
- observation-owned `remote_read_denied` findings come only from remote
  observation/probe at the observation orchestration seam

### Retry and trial release

Retry and trial release is retry-owned, but it is no longer timer-time
replanning. Current-plan preparation prunes stale `retry_work` against the
latest actionable set, loads the surviving `retry_work` / `block_scopes`, and
initializes the current runtime's held-work indexes. Admission then decides,
for each dependency-ready exact action, whether it dispatches now, stays held
behind `next_retry_at`, or stays held behind an active `scope_key`.

Held retry release and held trial release operate only on that current
runtime state:

- held retry release emits exact held actions whose `next_retry_at` is due
- held trial release emits one deterministic held blocked candidate for each
  due scope
- neither held-release path rebuilds an `ActionPlan`, refreshes current
  truth, or walks a second dependency closure

Scope lifecycle is owned only by `block_scopes` plus blocked/unblocked
`retry_work`. Timed transient scopes are discarded when no blocked
`retry_work` remains for their `scope_key`. Remote write block scopes follow
that same rule; recovery happens through normal timed trials or successful
writes, not through a separate maintenance loop. A scope row may still share a
`scope_key` with observation or ready retry rows for reporting/history, but
the engine keeps a blocker active only while blocked `retry_work` still
exists. Scope release updates both authorities in one logical step: store-side
blocked rows become ready immediately, and the current runtime flips the
matching held entries into due retry-held work so one-shot and watch can
continue without waiting for a replan. Trial-driven reclassification from one
blocked scope to another follows that same ownership rule: the newly
reclassified exact work moves to the new scope immediately, but the old scope is
discarded only after its prior `scope_key` no longer owns any blocked
`retry_work`. If blocked work remains under the old scope, the engine rearms that
retained scope's next trial interval in the same transition so it does not stay
immediately due after the just-finished trial.

## What The Engine Does Not Own

The engine does not own:

- multi-drive orchestration
- manual resolution workflows
- a durable action queue
- a mixed failure-reporting/control table

Those concepts do not belong in the target architecture.
