# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/dep_graph.go, internal/sync/active_scopes.go, internal/sync/scope.go, internal/localtrash/trash.go

Implements: R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.3 [verified], R-6.2.4 [verified], R-6.4.4 [verified], R-6.4.5 [verified], R-6.4.6 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified]

## Overview

Execution takes an `ActionPlan`, dispatches ready work through a dependency
graph, runs workers, and returns one `ActionOutcome` per completed action.

The executor does **not** own retry policy, durable failure classification, or
scope lifecycle. It performs one action and reports the concrete outcome.

## Ownership Contract

- Owns: dispatch, dependency satisfaction, worker execution, local trash integration, conflict-copy creation, and success outcomes
- Does Not Own: planning, retry scheduling, scope activation policy, or store schema
- Source of Truth: planner-produced `ActionPlan` plus the rooted capabilities injected into the executor
- Allowed Side Effects: sync-root filesystem mutation, Graph transfer calls, and store success commits through the engine
- Mutable Runtime Owner: Each executor instance owns only one action plan, one dependency graph, and one bounded worker pool for the lifetime of that execution pass. The engine owns higher-level admission, retries, and scope state.
- Error Boundary: Workers and executor helpers return concrete action outcomes and execution errors; the engine classifies those outcomes into retries, failures, scope transitions, and durable state changes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Edit/edit and create/create conflicts are resolved immediately by preserving both versions with a local conflict copy and downloading the canonical remote version. | `TestExecutor_Conflict_EditEdit_KeepBoth`, `TestExecutor_Conflict_EditEdit_KeepBoth_ConflictCopyCollisionGetsSuffix`, `TestConflictCopyPath_Normal` |
| Local edit versus remote delete is auto-resolved without a durable conflict mailbox. | `TestExecutor_Conflict_EditDelete_AutoResolve`, `TestExecutor_LocalDelete_HashMismatch_ConflictCopy` |
| Execution keeps ordinary per-item delete safety and routes concrete failures back to the engine instead of persisting manual approval state. | `TestExecutor_LocalDelete_HashMismatch_ConflictCopy`, `TestExecutor_SyncedUpdate`, `TestExecutor_SyncedUpdate_BaselineFallback` |

## Worker And Dependency Model

`DepGraph` is the execution-time dependency graph. It tracks:

- which actions are in flight
- which dependencies remain unsatisfied
- which dependents become ready after completion

Workers run independent actions concurrently up to the configured worker limit.
The engine owns the worker pool lifecycle and result drain.

## File And Folder Mutation

### Uploads and downloads

Execution delegates transfer mechanics to `driveops.TransferManager`.

- downloads use atomic local writes and integrity verification
- uploads prefer overwrite-by-item-ID when the planner already knows the
  authoritative remote item
- true creates still use parent-path upload because no remote item identity
  exists yet

### Local deletes

Local delete keeps the ordinary per-item safety rule:

- if the file still hashes to the baseline hash, delete it
- if the file changed after planning, do **not** delete newer local content;
  auto-resolve as an edit/delete conflict and recreate the remote file

Directory delete also preserves non-disposable local content by refusing to
remove directories blocked by non-disposable children.

### Trash behavior

- remote deletes go through OneDrive recycle bin by default
- local deletes may go through OS trash depending on configuration and platform

## Conflict Execution

`ActionConflict` is immediate execution work. There is no durable
conflict-request mailbox.

### Edit/edit and create/create

The executor preserves both versions:

1. rename local file to `<stem>.conflict-<timestamp><ext>`
2. download remote file back to the canonical path

If the download fails, the executor restores the original local file from the
conflict copy.

### Edit/delete

The executor auto-resolves `ConflictEditDelete` with local-wins behavior:

1. keep the local file in place
2. upload it to recreate the remote item

No conflict copy is needed for this case.

## Shared-Root Execution

For configured drives rooted below the remote drive root, execution uses
planner-supplied target metadata:

- `TargetDriveID`
- `TargetRootItemID`
- `TargetRootLocalPath`

That metadata is used for:

- target-scoped remote calls
- path convergence checks after successful remote mutation
- correct scope classification for target-drive results

## Scope Helpers

`active_scopes.go` is no longer a separate runtime subsystem. It provides pure
helper functions over an engine-owned slice of active scope blocks.

Execution itself does not decide whether an action is blocked. The engine asks
those helpers before dispatch and after worker results.

## What Execution No Longer Owns

Execution no longer includes:

- delete counters or held-delete approval workflows
- durable conflict request application
- embedded shared-folder runtime machinery inside another drive

Those concepts were removed from the current architecture.
