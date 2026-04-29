# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/worker_result.go, internal/sync/dep_graph.go, internal/sync/active_scopes.go, internal/sync/scope.go

Implements: R-2.3.1 [verified], R-2.8.6 [verified], R-2.14.2 [verified], R-6.2.3 [verified], R-6.2.4 [verified], R-6.4.4 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified]

## Overview

Execution takes a runtime plan, dispatches concrete side-effecting
work through a dependency graph, runs workers, and reports one
`ActionCompletion` per finished action. That runtime-plan handoff is
assembled on the engine side by the shared pipeline in
`engine_current_plan.go`, then admitted through `engine_runtime_start.go`
before execution begins. Publication-only planner actions are not executor
work: the engine
reduces them directly through the store before workers see any concrete
frontier.

The executor does **not** own retry policy, durable failure classification, or
scope lifecycle. It performs one action and reports the concrete outcome.

## Ownership Contract

- Owns: dispatch, dependency satisfaction, worker execution, conflict-copy creation, and success outcomes
- Does Not Own: planning, retry scheduling, scope activation policy, or store schema
- Source of Truth: planner-produced `ActionPlan` plus the rooted capabilities injected into the executor
- Allowed Side Effects: sync-root filesystem mutation, Graph transfer calls, and store success commits through the engine
- Mutable Runtime Owner: workers own only action execution. The engine owns
  runtime quiescence, held-work timing, admission, and dependency completion
  decisions above the worker pool.
- Error Boundary: Workers and executor helpers return concrete action outcomes and execution errors; the engine classifies those outcomes into retries, failures, scope transitions, and durable state changes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Edit/edit and create/create conflicts are resolved immediately by preserving both versions with a local conflict copy and downloading the canonical remote version. | `TestExecutor_Conflict_EditEdit_KeepBoth`, `TestExecutor_Conflict_EditEdit_KeepBoth_ConflictCopyCollisionGetsSuffix`, `TestConflictCopyPath_Normal` |
| Planner-generated edit/delete uploads remain concrete execution work, while stale local deletes requeue for replan instead of inventing new sync intent inside the executor. | `TestExecutor_Conflict_EditDelete_AutoResolve`, `TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition` |
| Publication-only planner actions commit baseline mutations without worker dispatch and release dependents through the engine-owned publication-drain stage. | `TestPublicationMutation_SyncedUpdate`, `TestPublicationMutation_SyncedUpdate_BaselineFallback`, `TestPublicationMutation_Cleanup`, `TestPublicationMutation_Cleanup_FolderType`, `TestRunPublicationDrainStage_DoesNotReleaseUnrelatedHeldWork` |
| Watch-mode replan keeps old-runtime work out of dispatch once it is no longer current and preserves dirty intent across recoverable local-observation failure. | `TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch`, `TestWatchRuntime_QueuePendingReplanRetiresOldOutbox`, `TestWatchRuntime_PendingReplanRetiresDependentsReleasedByRunningAction`, `TestWatchRuntime_PendingReplanLocalObservationFailureReschedulesDirtySignal`, `TestWatchRuntime_IdleReplanLocalObservationFailureReschedulesDirtySignal` |

## Worker And Dependency Model

`DepGraph` is the execution-time dependency graph. It tracks:

- which actions are in flight
- which dependencies remain unsatisfied
- which dependents become ready after completion

Workers run independent actions concurrently up to the configured worker limit.
The engine owns the worker pool lifecycle and completion drain. In watch mode,
the worker dispatch channel is intentionally unbuffered so not-yet-started
actions remain in the engine-owned outbox until a worker is ready to receive
one. The worker completion channel remains separately buffered so workers can
report completed actions without reintroducing a hidden dispatch backlog.
When a replan signal is already ready, the watch loop consumes it before
enabling the dispatch send for the current step, so the old outbox is retired
instead of racing an idle worker receive.

The dependency graph is dependency-only. It no longer defines runtime
quiescence. Held retry/scope work intentionally keeps exact nodes unresolved,
so the engine decides when the current runtime is quiescent based on outbox,
running work, and due held entries. `DepGraph` therefore does not expose a
runtime-completion channel; callers use dependency release plus engine-owned
settle checks instead. When shutdown has already started, the engine still
processes late worker completions for bookkeeping, but it immediately converts
any newly-ready frontier back into shutdown completion instead of reopening the
dispatch path.

When watch mode has a pending replan, newly-ready concrete frontier from the old
runtime is likewise not reopened into dispatch. The engine retires
not-yet-dispatched old outbox work and any dependents released by already-running
old actions without completing those dependency nodes as success. The next
runtime plan is rebuilt from current truth and durable retry/block state. If
local observation fails before replacement runtime installation, retired work
remains retired and the dirty/full-refresh intent is rescheduled for a later
steady-state replan. Idle replans that fail during local observation preserve
the same dirty/full-refresh intent through that scheduler instead of dropping
the trigger.

When the dependency graph releases `ActionUpdateSynced` or `ActionCleanup`,
the engine does not spend worker capacity on them. It commits the matching
baseline mutation synchronously, marks that graph node successful, drains any
further publication-only dependents through the engine-owned publication
drain stage, and only then releases concrete dependents for worker
dispatch.

## Publication-Only Actions

`ActionUpdateSynced` and `ActionCleanup` remain planner-visible action types so
they still participate in dependency ordering, accounting, and reporting.

Execution does not own them because they perform no external side effects:

- no filesystem mutation
- no Graph call
- no transfer manager work

Their only durable effect is baseline publication, so the engine/store
boundary commits them directly via `CommitMutation()`.

Publication success stays entirely on the engine side: the engine marks the
graph node successful and releases any dependents without synthesizing a worker
completion. Publication failure still uses the shared result classifier so the
exact publication action can persist `retry_work` and remain held in the
current runtime instead of terminating the loop on a transient store error.
When held publication work becomes due again, the engine routes it back through
the publication-drain stage before any worker dispatch. Workers reject
publication-only action types only as an invariant guard; normal runtime flow
must never send those actions to the worker pool.

Publication drain itself is not an outbox helper. It is an effectful
engine/store stage, and `reduceReadyFrontierStage` is the owning runtime stage
that sequences publication drain plus due-held release. Callers hand the engine
exact ready actions, the engine durably applies publication mutations, routes
publication failures through normal completion handling, releases any already-
due held work, and returns only the surviving concrete worker frontier to the
caller’s dispatch queue. Watch bootstrap, steady-state completions, and held-
release ticks all re-enter that same stage before anything reaches the worker
outbox.

## File And Folder Mutation

### Uploads and downloads

Execution delegates transfer mechanics to `driveops.TransferManager`.

- downloads use atomic local writes and integrity verification
- uploads prefer overwrite-by-item-ID when the planner already knows the
  authoritative remote item
- true creates still use parent-path upload because no remote item identity
  exists yet

Successful outcomes still preserve remote ancestry for later baseline writes,
but execution no longer reloads that parent ID from `remote_state`. Outcome
parent recovery is:

- use the live Graph item returned by the mutation when it already carries
  `ParentID`
- otherwise resolve the parent from the action path against current baseline
  state (`ResolveParentID`)
- finally fall back to the baseline view already attached to the action when a
  durable parent is known there

That keeps `baseline.parent_id` as the durable ancestry authority without
requiring a second persisted parent field in `remote_state`.

Execution-time validation is always-on where it matters. Upload overwrite
preflight and similar validation-before-mutate checks are executor-owned and
apply in both one-shot and watch mode; they are not gated on a watch-only
policy flag.

### Local deletes

Local delete keeps the ordinary per-item safety rule:

- if the file still hashes to the baseline hash, delete it
- if the file changed after planning, do **not** delete newer local content;
  return a stale-precondition failure so the engine replans from current truth

Directory delete also preserves non-disposable local content by refusing to
remove directories blocked by non-disposable children.

## Conflict Execution

The snapshot-based runtime does not execute abstract conflict rows. Conflict
handling is expanded before execution into concrete work such as
`ActionConflictCopy`, `ActionDownload`, and conflict-tagged `ActionUpload`.
There is no durable conflict-request mailbox.

### Edit/edit and create/create

The executor preserves both versions:

1. execute `ActionConflictCopy` to rename the local canonical file to `<stem>.conflict-<timestamp><ext>`
2. execute the dependent `ActionDownload` back to the canonical path

If the download fails, the preserved local conflict copy remains on disk and
the canonical path stays pending for retry/replan. Execution does not recreate
an abstract conflict action to roll the pair back.

### Edit/delete

The planner expands `ConflictEditDelete` into a concrete `ActionUpload`. The
executor then performs that upload as ordinary concrete work:

1. keep the local file in place
2. upload it to recreate the remote item

No conflict copy is needed for this case, and the executor does not invent the
upload from a stale local delete.

## Mount-Local Execution

For engines rooted below the remote drive root, execution uses the engine's own
mount context:

- `ExecutorConfig.driveID`
- `ExecutorConfig.remoteRootItemID`
- `Action.DriveID`

Path convergence checks after successful remote mutation are resolved relative
to that mounted subtree, not via per-action target-root overrides. Scope
classification likewise uses the action completion's authoritative `DriveID`.

Direct shared-item CLI flows still keep explicit target-scoped behavior above
sync through `driveops.SharedTargetClients(...)` and root-aware
`driveops.MountSession` path operations. That is a transfer/CLI boundary, not
ordinary sync execution.

## Scope Helpers

`active_scopes.go` is no longer a separate runtime subsystem. It provides pure
helper functions over an engine-owned slice of active block scopes.

Execution itself does not decide whether an action is blocked. The engine asks
those helpers before dispatch and after action completions.

Scope admission evaluates both the action's current path and, for moves, the
source `OldPath`. A move whose source subtree is blocked stays blocked even if
its destination path is outside the blocked subtree.

## What Execution No Longer Owns

Execution no longer includes:

- delete counters or blocked-delete approval workflows
- durable conflict request application
- baseline-only publication work for converged rows or fully removed rows
- embedded shared-folder runtime machinery inside another drive

Those concepts were removed from the current architecture.
