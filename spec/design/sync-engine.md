# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/engine_watch*.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_capability.go, internal/sync/permission_evidence.go, internal/sync/permission_probe_local.go, internal/sync/permission_probe_remote.go, internal/sync/permission_policy.go, internal/sync/permission_decisions.go, internal/sync/observation_findings.go, internal/cli/sync_flow.go, internal/cli/sync_runtime.go

Implements: R-2.1 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.10 [designed], R-2.14 [designed], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified]

## Overview

The engine is the single-drive runtime owner. It coordinates:

- observation
- planning
- publication-only action commits
- execution
- durable outcome writes
- retry and trial scheduling
- scope lifecycle
- watch-mode refresh and maintenance work

The target engine persists durable status through three authorities:

- `observation_issues`
- `retry_work`
- `block_scopes`

It does not use a mixed failure table as durable control state.

`retry_work` and `block_scopes` are engine-owned control state, not
best-effort diagnostics. If the runtime cannot durably record or transition
required retry/scope state after an exact action result or admission decision,
it fails closed and terminates the current runtime. Product-facing
`sync_status` writes remain best-effort.

`observation_findings.go` is the engine-owned constructor boundary for
observation batches. Engine orchestration chooses when to reconcile those
batches durably or into a scratch planning store, but callers should not
assemble overlapping observation-managed batch shapes ad hoc.

## Ownership Contract

- Owns: single-drive runtime orchestration, watch-mode mutable state,
  worker-result classification, retry/trial scheduling, and scope lifecycle.
- Does Not Own: SQLite schema, Graph normalization, config parsing, or
  multi-drive daemon lifecycle.
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
| One-shot and watch share the same admission/runtime contract, while watch alone keeps the runtime alive for future timer release. | `TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue`, `TestReleaseDueHeldRetriesNow_ReleasesHeldRetryEntriesOnly`, `TestReleaseDueHeldTrialsNow_ReleasesFirstHeldScopeCandidateAsTrial`, `TestHandleWatchEvent_RetryTickReducesReleasedPublicationRetryOnEngineSide`, `TestWatchRuntime_RunBootstrapStep_RetryTickReducesReleasedPublicationRetryOnEngineSide`, `TestPhase0_OneShotEngineLoop_TrialSuccessMakesFailuresRetryableAndReinjectableWithoutExternalObservation` |

## Construction

`newEngine()` wires one resolved drive into a runtime:

- rooted sync tree
- store
- planner
- executor configuration
- transfer manager
- permission handler
- optional websocket wake source

For separately configured shared-root drives, the engine also carries the
configured `rootItemID`. That root item defines the remote boundary for scoped
observation and execution metadata.

Permission handling is intentionally split three ways:

- probe/evidence (`permission_probe_*.go`, `permission_evidence.go`) gathers
  filesystem or Graph facts only
- pure policy (`permission_policy.go`) turns one action completion plus
  permission evidence into an engine-facing `PermissionOutcome`
- direct engine runtime application (`permission_decisions.go`) persists
  blocked `retry_work`, activates or releases timed write scopes, and emits
  engine-owned logs without a separate controller shell

Normal completion handling and trial reclassification both reuse the same
engine helper to gather permission evidence and call `DecidePermissionOutcome`.
That shared helper decides once; it does not persist anything itself.

Permission timing follows the policy result, not the probe:

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

1. bootstrap durable state and startup checks
2. normalize persisted scopes against persisted blocked-work evidence
3. load baseline
4. refresh current remote and local snapshots once
5. compute SQL structural diff and reconciliation once
6. build the current actionable set in Go from structural reconciliation plus
   explicit truth-availability overlays
7. reconcile durable retry/blocker state to that actionable set
8. commit any ready publication-only actions directly through the store and
   drain publication-only dependents before worker dispatch
9. execute remaining concrete work once using the same blocker/trial admission
   model watch mode uses
10. persist outcomes and return a report

There is no mid-pass mailbox for user intent. New external DB writes during a
one-shot run are durable state for a later run.

The current-plan preparation stage is the handoff boundary between planning
and runtime startup. Observation remains entrypoint-specific, but once an
entrypoint has produced observed current truth the engine runs the same named
stage sequence: load planner inputs, build the current action plan from those
observed inputs, then prepare the runtime handoff by reconciling durable
retry/scope state and loading surviving `retry_work` / `block_scopes`.
Stale `retry_work` and empty `block_scopes` are pruned there, not from timer
held-release paths.

Scope startup cleanup follows the same policy with a deliberate
decision-then-apply split: the engine first derives which persisted scopes are
still justified by blocked `retry_work`, then applies only the required delete
mutations. The same timed-scope liveness rule also drives runtime
rearm-or-discard handling and store-side prune helpers so empty timed scopes
do not survive by accident in one path but not another.

Within that one-shot flow, the engine now treats "prepare current plan" as an
explicit stage sequence: observe current truth, build the current plan from
that observed state, then either prepare a runtime handoff or keep the
dry-run build in memory without touching durable held-work state. Live,
dry-run, watch bootstrap, and steady-state watch replans all use that same
observed-state -> build -> prepare boundary; they differ only in how they
collected the observed state and whether a deferred cursor commit is present.
The top-level coordinators should stay at that stage level rather than
inlining planner input loads, durable prune/load logic, or runtime-start
bookkeeping. The same rule applies to execution-only publication drain
helpers and post-sync housekeeping: keep them next to their stage so a reader
can see the flow at a glance.

Full-remote-refresh cadence is restart-safe even when a full remote refresh returns
no delta cursor. The engine still advances the persisted cadence in
`observation_state` so enumerate-only and shared-root sessions do not fall into
back-to-back expensive full refreshes.

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

The watch loop is the single owner of mutable scheduling/runtime state:
outbox, dispatch admission, held-work timing, refresh coordination, and drain
behavior. Remote observation commits now follow the same single-owner rule:
watch observers and full-refresh goroutines emit one loop-applied
`remoteObservationBatch` value, and the loop itself owns projected remote
observation commits, cursor commits, observation-finding reconciliation, dirty
marking, and refresh-timer re-arm.

Local watcher events, remote delta batches, websocket wakes, and full remote
refresh results are scheduler hints only. After debounce or wake, watch
mode refreshes current truth, runs SQL comparison/reconciliation, rebuilds the
current actionable set in Go, reconciles durable retry/blocker state, and then
admits runnable actions. There is one steady-state replan entry for that work:
refresh local truth, load the already-committed remote/current state, build the
current plan, prepare the runtime handoff, then append the resulting concrete
worker frontier through the watch-owned frontier helpers. DirtyBuffer still
emits `DirtyBatch` scheduler hints, but those hints feed only this steady-state
replan path; they do not define a second planning model.

Watch runtime replacement is linear: one current runtime graph at a time.
Dirty observation while work is still queued or running sets a pending replan
flag instead of appending a second graph into the current runtime. Once the
runtime reaches the idle boundary, the loop rebuilds from current committed
truth plus durable `retry_work` / `block_scopes`.

Retry/trial is not an alternate planner. Timer-driven follow-up only
re-releases exact held actions that are already part of the current runtime.
The engine holds dependency-ready exact work in memory, keyed by exact
`RetryWorkKey` and grouped by `ScopeKey` for blocked scopes. Timer ticks do
not rebuild subset plans, do not compute dependency closure, and do not
revalidate stale rows. Stale-row cleanup belongs only to normal
prepare/reconcile. Dependency tracking stays inside `DepGraph`, but runtime
completion does not: the engine owns quiescence and no longer waits on a
graph-owned completion signal.

Released held work always re-enters publication reduction before any worker
dispatch. Timer-released `ActionUpdateSynced` and `ActionCleanup` actions stay
engine-side, commit through the store, and unlock dependents without ever
crossing into the worker pool.

Action completion drain stays inside the engine boundary. When a completion
unlocks publication-only dependents, watch mode commits those mutations
synchronously and keeps draining them on the engine/store side until concrete
worker actions are the only dispatchable work left. That frontier reduction is
transform-only: it takes exact ready actions and returns only the concrete
worker frontier. One-shot and watch coordinators still own their outbox state
explicitly.

Runtime completion handling follows the same boundary shape everywhere:
classify the finished exact action, apply the resulting durable/runtime
mutation, then release any due held work back into the ready frontier. It does
not mix that decision step with worker-queue ownership.

Watch replan failure policy is also explicit. Pre-authority local observation
failure is recoverable and drops that replan trigger. Once the engine starts
depending on authoritative local snapshot or runtime state, failure is fatal to
the current watch session: local snapshot commit, prepare-from-committed-truth,
and runtime startup/admission all fail closed. Shutdown cancellation is the
one exception: if context cancellation lands during that steady-state replan
handoff, the loop clears the best-effort sync-status batch and exits cleanly
into shutdown instead of surfacing a fatal watch error.

Once shutdown drain has sealed new admission, late action-completion
classification or persistence errors are treated as shutdown bookkeeping only.
The loop logs them, keeps draining the already-owned completion sources, and
returns clean shutdown rather than converting cancellation timing into a fatal
watch error.

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

## Shared-Root Drives

Shared folders are separate configured drives. The engine therefore supports
two drive shapes:

- ordinary drive-root sessions
- shared-root sessions rooted below the remote drive root

Embedded shared-folder links discovered inside another synced drive are ignored
by observation and never become nested engine-owned sub-sessions.

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
  next normal prepare cycle
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
replaced by a fresh prepare cycle.

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
trigger to probe; the probe returns evidence only, the pure permission policy
maps that evidence to an engine-facing outcome, and only the engine apply path
may activate or release a write-side scope.

Ownership splits by access kind:

- read boundaries (`perm:dir:read`, `perm:remote:read`) are observation-owned
  boundary tags on `observation_issues`
- write scopes (`perm:dir:write`, `perm:remote:write`) are
  probe/execution-owned persisted `block_scopes`
- file-level local permission failures remain exact `retry_work` rows with the
  normal reconcile backoff; they are not promoted into `block_scopes` unless a
  later probe proves a subtree-wide boundary

Permission outcomes may still tag blocked `retry_work` with a read-boundary
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
continue without waiting for a replan.

## What The Engine Does Not Own

The engine does not own:

- multi-drive orchestration
- manual resolution workflows
- a durable action queue
- a mixed failure-reporting/control table

Those concepts do not belong in the target architecture.
