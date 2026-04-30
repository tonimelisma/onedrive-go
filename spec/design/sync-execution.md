# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_preconditions.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/worker_result.go, internal/sync/action_freshness.go, internal/sync/dep_graph.go, internal/sync/active_scopes.go, internal/sync/scope.go

Implements: R-2.3.1 [verified], R-2.8.6 [verified], R-2.8.7 [verified], R-2.8.9 [verified], R-2.8.10 [verified], R-2.14.2 [verified], R-6.2.3 [verified], R-6.2.4 [verified], R-6.4.4 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified]

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
| Planner-generated edit/delete uploads remain concrete execution work, while stale local deletes return a superseded precondition outcome so the engine replans instead of inventing new sync intent inside the executor. | `TestExecutor_Conflict_EditDelete_AutoResolve`, `TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition`, `TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess` |
| Worker-start validation rejects already-submitted stale actions before executor side effects, while suspect local truth disables local-state-based rejection. Dependent uploads after planned remote moves tolerate move-produced eTag churn but still reject proven remote content drift, and executable actions without planner truth fail closed. | `TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution`, `TestWorkerStartFreshness_SuspectLocalTruthDoesNotSupersedeFromLocalState`, `TestActionFreshness_PostRemoteMoveUploadAllowsMoveProducedETagChange`, `TestActionFreshness_PostRemoteMoveUploadRejectsRemoteContentChange`, `TestActionFreshness_MissingPlannerViewFailsClosedForExecutableAction` |
| Executor live preconditions reject stale work at the side-effect boundary without mutating local or remote state. | `TestExecuteRemoteDelete_NotFoundPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecuteRemoteDelete_ETagMismatchPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecuteRemoteDelete_TransientPreflightFailureIsOrdinaryFailure`, `TestExecutor_RemoteMove_StaleSourcePreflightReturnsStalePrecondition`, `TestExecutor_CreateRemoteFolder_MissingParentPreflightReturnsStalePrecondition`, `TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition`, `TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition`, `TestExecutor_Download_MountRootAllowsGraphDriveRootPath`, `TestExecutor_LocalMove_SourceChangedReturnsStalePrecondition` |
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

Before a worker begins executor work, it validates the exact action against
latest committed truth using the shared action-freshness predicate. Admission
uses the same predicate only for actions that are about to enter the worker
outbox; retry-held or scope-held actions are checked when they later become
dispatch candidates. A stale worker-start action returns
`ErrActionPreconditionChanged`, so the engine classifies it as superseded
instead of ordinary retry. This gate runs before hashing, upload-session
creation, download writes, delete/move mutation, or other executor side
effects. It is intentionally not a second planner: it only rejects when current
truth disproves the exact planned assumptions. Local absence or presence can
reject only while `local_truth_complete` is true;
suspect local truth leaves local-state-based rejection to the next full local
refresh. Remote-state changes can reject when committed remote truth proves a
path, item identity, hash, or eTag assumption false. A dependent upload after a
planned remote move is the narrow exception: its planned remote snapshot is the
pre-move item, so the freshness predicate ignores eTag churn caused by the
planned move itself while still requiring the current target row to match item
identity and content facts. Executable actions must carry planner view truth;
missing planner view data is an internal validation error once committed truth
is authoritative, not a reason to skip stale work checks. Move actions
additionally check the source/destination peer path that is not represented by
the main `PathView`, so a changed local source, reappeared remote source,
missing local move destination, or occupied remote move destination supersedes
the old move before side effects.

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

## Executor Live Preconditions

Worker-start and admission freshness checks keep stale work from reaching the
executor when committed truth already proves the action obsolete. The executor
still owns the final side-effect boundary, because local files and Graph items
can change after dispatch and before the mutation or transfer actually begins.
`executor_preconditions.go` is that boundary owner.

For uploads, the executor validates that the local source path is still inside
the sync root, still a regular file, and still matches planned identity,
size/mtime, and hash facts when those facts are available. The transfer manager
then reuses the executor-supplied callback before upload reads so a large file
can abort if it changes mid-session. Expected source-hash mismatches are
returned as `ErrActionPreconditionChanged`.

For downloads, the executor validates the planned destination before transfer
starts and supplies the same callback for the transfer manager to run
immediately before the partial file is atomically renamed into place. If the
plan expected absence and a local file appears, or the plan expected an
overwrite and the existing file no longer matches, the download is superseded
and the partial remains available for ordinary transfer recovery.

For local deletes and moves, the executor rejects missing or changed planned
sources instead of treating them as successful mutation. Local moves also reject
an unexpectedly occupied destination before `Rename`.

For remote deletes, moves, downloads, overwrite uploads, and remote folder/file
creates, the executor performs a live `GetItem` preflight for the planned
source item or parent. A live `itemNotFound` result, changed item identity,
drive, item type, eTag, hash, size, or same-coordinate path where Graph supplies
that fact is a stale precondition and maps to `ErrActionPreconditionChanged`.
Mount-root engines do not use raw `GetItem` parent-reference paths as
staleness proof because Graph reports those paths relative to the drive root
while the executor's actions are relative to the mounted item. A transient `GetItem`
failure is not superseded; it remains an ordinary read failure so retry policy
can handle it normally. A dependent upload after a planned remote move keeps
the same eTag exception used by worker-start freshness: move-produced eTag churn
alone does not make the upload stale when item identity and content facts still
match.

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
  return a stale-precondition/superseded outcome so the engine replans from
  current truth without retrying that exact old delete

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
