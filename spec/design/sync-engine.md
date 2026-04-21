# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/watch_*.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_capability.go, internal/sync/permission_evidence.go, internal/sync/permission_probe_local.go, internal/sync/permission_probe_remote.go, internal/sync/permission_policy.go, internal/sync/permission_decisions.go, internal/sync/observation_findings.go, internal/cli/sync_flow.go, internal/cli/sync_runtime.go

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
- watch-mode reconcile and maintenance work

The target engine persists durable status through three authorities:

- `observation_issues`
- `retry_work`
- `block_scopes`

It does not use a mixed failure table as durable control state.

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
- Mutable Runtime Owner: `Engine` owns all single-drive mutable runtime state.
  In watch mode that includes the event loop, outbox, in-flight actions,
  retry/trial timers, scope state, and admission flags.
- Error Boundary: the engine translates observer, planner, executor,
  permission, and store outcomes into engine-owned reports, retries, scope
  transitions, and durable authority writes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| One-shot sync remains a bounded observe-plan-execute pass without a live user-intent mailbox. | `TestBootstrapSync_NoChanges`, `TestBootstrapSync_WithChanges`, `TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork`, `TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady` |
| Watch mode keeps single-owner runtime admission, retry/trial scheduling, and external-change reconciliation inside the engine boundary. | `TestEngine_CascadeRecordAndComplete_RecordsBlockedRetryWork`, `TestEngine_ExternalDBChanged`, `TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue`, `TestRunWatch_ShutdownStopsRetryAndTrialTimers` |

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
- apply/logging (`permission_decisions.go`) persists blocked `retry_work`,
  activates or releases timed write scopes, and emits engine-owned logs

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

`materializeCurrentActionPlan()` is the durable reconciliation boundary. It
should prune stale `retry_work`, prune empty `block_scopes`, and align blocked
work with active scopes. It does not persist a durable executable plan table.
Every persisted scope must still have blocked `retry_work`; empty scopes are
discarded immediately.

Scope startup cleanup follows the same policy with a deliberate
decision-then-apply split: the engine first derives which persisted scopes are
still justified by blocked `retry_work`, then applies only the required delete
mutations. The same timed-scope liveness rule also drives runtime
rearm-or-discard handling and store-side prune helpers so empty timed scopes
do not survive by accident in one path but not another.

Within that one-shot flow, the engine now treats "prepare current plan" as an
explicit stage: observe current truth, derive the current action plan,
materialize durable retry/scope reconciliation, and only then hand the
prepared plan to execution/reporting. The outer orchestration should stay at
that stage level rather than inlining every sub-step. Live and dry-run now
share the same observed-state -> current-plan stage shape; they differ only in
whether the prepared plan is materialized durably and whether a deferred
cursor commit is returned. The stage helpers that observe, load planner
inputs, build the current plan, and materialize durable retry/scope state
belong in the current-plan stage boundary, not in the top-level `RunOnce()`
coordinator. The same rule applies to bootstrap, execution-only publication
drain helpers, and post-sync housekeeping: keep them next to their stage so a
reader can see the one-shot flow at a glance.

Full-remote-refresh cadence is restart-safe even when a full reconcile returns
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
- periodic recheck and full remote refresh
- graceful drain on shutdown

The watch loop is the single owner of mutable runtime state. Other packages may
signal it, but they do not mutate its runtime data structures directly.

Local watcher events, remote delta batches, websocket wakes, and full
reconciliation results are scheduler hints only. After debounce or wake, watch
mode refreshes current truth, runs SQL comparison/reconciliation, rebuilds the
current actionable set in Go, reconciles durable retry/blocker state, and then
admits runnable actions.

Retry/trial is not an alternate planner. Timer-driven follow-up consumes the
last successfully materialized current action plan produced by the normal watch
observation/planning path. Watch runtime caches that materialized plan as an
indexed snapshot keyed by exact `RetryWorkKey` so retry/trial can extract just
the needed dependency-closed subset without rescanning the whole plan. If a
due retry or due scope trial is absent from that cached snapshot, retry/trial
may run targeted revalidation for the exact retry row or blocked scope, but it
must not refresh snapshots, rebuild the plan, mark the runtime dirty, or
reconcile observation-owned issues.

Action completion drain stays inside the engine boundary. When a completion
unlocks publication-only dependents, watch mode commits those mutations
synchronously and keeps draining them on the engine/store side until concrete
worker actions are the only dispatchable work left.

### Recheck And External DB Changes

Watch mode periodically checks SQLite `PRAGMA data_version`. If another
connection committed a write, `handleExternalChanges()` runs.

That hook is intentionally narrow. It rechecks externally cleared scope state
and aligns runtime admission with the persisted `block_scopes` and blocked
`retry_work` rows. It is not a generic user-intent ingestion path.

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
  next normal plan materialization

### Runtime admission model

`DepGraph` remains dependency-only. The engine still performs final scope
admission after dependency readiness:

- build the graph from current actions
- ask the graph for ready work
- gate that ready work against active scopes
- dispatch allowed work
- keep blocked work represented durably in `retry_work`

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
manual recheck CLI for them. Observation may create or clear read-boundary
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

### Retry and trial reconstruction

Retry and trial reconstruction is retry-owned. The engine revalidates due or
blocked work directly from `retry_work`, exact semantic work identity, and the
current snapshot/baseline view. Scope lifecycle is owned only by
`block_scopes` plus blocked/unblocked `retry_work`. Timed transient scopes are
discarded when no blocked `retry_work` remains for their `scope_key`. Remote
write block scopes follow that same rule; recovery happens through normal timed
trials or successful writes, not through a separate maintenance loop. This
runtime ownership rule is narrower than the store's structural linkage
invariant: a scope row may still share a `scope_key` with observation or ready
retry rows for reporting/history, but the engine keeps a blocker active only
while blocked `retry_work` still exists. Missing-row follow-up uses one
row-level revalidation contract:

- clear the exact retry row when current truth resolved it
- clear the exact retry row when targeted observation/probing now skips it
- rearm unblocked retry work when the exact action still needs later follow-up
- keep held retry work blocked behind its scope when the blocker still applies

Retry sweeps and scope trials may apply those outcomes differently, but they
must derive them from the same row-level revalidation policy rather than
duplicating action-type-specific cleanup logic at each caller.

## What The Engine Does Not Own

The engine does not own:

- multi-drive orchestration
- manual resolution workflows
- a durable action queue
- a mixed failure-reporting/control table

Those concepts do not belong in the target architecture.
