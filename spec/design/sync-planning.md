# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/planner_sqlite.go, internal/sync/planner_visibility.go, internal/sync/planner_truth_overlay.go, internal/sync/truth_status.go, internal/sync/actions.go, internal/sync/api_types.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/core_types.go

Implements: R-2.1.3 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified]

## Overview

Planning is split between SQLite reconciliation and a Go action builder.

- SQLite computes deterministic comparison and reconciliation rows from raw
  `baseline` plus planner-visible `local_state` and `remote_state` temp tables.
  Those temp tables are filter-scoped and then pruned for same-side descendants
  under a baseline folder that is missing on that side.
- Go turns reconciliation rows into the current executable action set, then
  applies blocked-truth suppression, parent-folder preservation, sync-mode
  filtering, and dependency detection.

The executable plan is runtime-owned and is not a durable SQLite authority.
Shortcut-root lifecycle planning follows the same functional-core rule inside
`internal/sync`: remote shortcut topology observations and local alias identity
facts enter deterministic shortcut-root planner helpers, while Graph,
filesystem, SQLite, logging, clocks, and goroutines stay in the engine shell.
The output is parent-owned shortcut-root state plus child work snapshot
intent; multisync receives only that child work intent.

## Ownership Contract

- Owns: preparing planner-visible current truth for SQLite reconciliation and
  turning reconciliation rows into the current executable action set, including
  conflict expansion, filtering, deferral, and dependency ordering
- Does Not Own: observation, execution, retry timing, scope persistence, or remote/local I/O
- Source of Truth: raw `baseline`, raw current-state tables, and the engine's
  compiled `ContentFilter`. Planning sees filtered current-state views through
  SQLite reconciliation rows, plus engine-owned sync mode and runtime policy.
- Allowed Side Effects: none
- Mutable Runtime Owner: None. Planning is deterministic value transformation over one input set and owns no long-lived mutable runtime state.
- Error Boundary: planner errors stop at invalid or unsupported planning states. Transport, filesystem, store, and execution failures stay outside the planner boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Conflict reconciliation rows expand into concrete actions for edit/edit, create/create, and edit/delete cases. | `TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions`, `TestPlannerPlanCurrentState_ExpandsCreateCreateConflictIntoConcreteActions`, `TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID` |
| Folder-delete descendants are reconciled by SQLite after planner-visible pruning, with parent availability preserved only when descendant work requires it. | `TestReplacePlannerVisibleStateTx_PrunesRemoteDescendantsWhenBaselineFolderMissingRemotely`, `TestReplacePlannerVisibleStateTx_PrunesLocalDescendantsWhenBaselineFolderMissingLocally`, `TestPlannerPlanCurrentState_RemoteParentDeletePlansDescendantLocalDeleteThroughSQLite`, `TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild`, `TestPlannerPlanCurrentState_LocalParentDeleteCreatesParentForChangedRemoteChild`, `TestPlannerPlanCurrentState_BothParentSidesDeletedCleansUpDescendantsThroughSQLite` |
| Mode-specific deferral and dependency ordering stay planner-owned rather than executor- or CLI-owned. | `TestSyncModeFromFlags`, `internal/sync/planner_sqlite_test.go`, `internal/sync/planner_dependency_test.go` |
| Planner decisions stay row-driven and action-shaped across conflict and folder-parent preservation cases. | `TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID`, `TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild`, `TestPlannerPlanCurrentState_DownloadOnlyKeepsParentDeleteWhenEditedChildUploadDeferred`, `internal/sync/planner_visibility_test.go` |

## Inputs

- `baseline`: last known synced common ancestor
- `local_state`: latest admissible local snapshot, filtered again when planning
  so stale rows from a changed filter cannot leak into comparison
- `remote_state`: latest raw manageable remote snapshot, filtered when planning
  so disabling a filter can re-present already-observed remote truth
- `observation_issues`: durable blocked-truth facts for unreadable or
  unsyncable current state
- `Mode`: bidirectional, download-only, or upload-only
- runtime safety/policy inputs owned by the engine

The engine first materializes transaction-local planner-visible current-state
tables from `local_state` and `remote_state` using the compiled `ContentFilter`.
It then removes same-side descendants below baseline folders that are absent
from that side's visible current table. Raw `baseline` is not filtered or
mutated. Those views feed `comparison_state` and `reconciliation_state`,
including the invariant that a baseline row absent from both planner-visible
current snapshots becomes `baseline_remove`.

Before Go emits actions, a shared derivation step computes one per-path
truth-status value from `observation_issues`, including any read-boundary
`ScopeKey` tags. Planner then applies its own suppression policy so unreadable
or unobservable paths stay unavailable instead of being misread as deletions.

`TruthAvailabilityIndex` is the one raw derived read model for that question.
Planner uses it directly, and read-only inspection of specific paths uses the
same derivation rather than a second planner-shaped helper.

## Pipeline

1. Build filtered transaction-local current-state views from `local_state` and
   `remote_state`; keep `baseline` raw.
2. Prune planner-visible same-side descendants under any baseline folder that is
   missing from that side's visible current-state table. Descendant matching uses
   exact path-prefix checks, not pattern matching.
3. Run SQL structural diff and reconciliation over planner-visible current views
   plus baseline.
4. Load reconciliation rows into Go.
5. Derive per-path local/remote truth status from filtered `observation_issues`
   through `TruthAvailabilityIndex`.
6. Apply planner-owned suppression and sync-mode safety rules on top of that
   derived status.
7. Emit the current runtime action set from reconciliation rows, including
   conflict expansion into concrete actions.
8. Preserve a deleted parent folder only when mode-admitted descendant actions
   need that parent to exist.
9. Bind ordinary actions to the engine's mounted drive/root context.
10. Apply mode filtering, build dependency edges, and reject dependency cycles.

## File Decisions

### Baseline exists

- local deleted + remote unchanged -> `ActionRemoteDelete`
- local deleted + remote changed -> `ActionDownload` (remote wins canonical path)
- local deleted + remote deleted -> `ActionCleanup` (publication-only)
- local changed + remote unchanged -> `ActionUpload`
- local unchanged + remote changed -> `ActionDownload`
- local changed + remote changed with equal hashes -> `ActionUpdateSynced` (publication-only)
- local changed + remote changed with different hashes -> `ActionConflictCopy` + dependent `ActionDownload`
- local unchanged + remote deleted -> `ActionLocalDelete`
- local changed + remote deleted -> `ActionUpload` with no stale item ID, so execution recreates the remote file

### No baseline

- local only -> `ActionUpload`
- remote only -> `ActionDownload`
- local and remote with equal hashes -> `ActionUpdateSynced` (publication-only)
- local and remote with different hashes -> `ActionConflictCopy` + dependent `ActionDownload`

## Publication-Only Planner Actions

`ActionUpdateSynced` and `ActionCleanup` remain real planner action types.
They stay in the action set so dependency ordering, counts, and reporting can
see them.

They are publication-only outcomes, not executor side effects:

- the planner still emits them from reconciliation rows
- the dependency graph still orders them against dependent actions
- the engine commits their baseline mutation directly through the store
- the executor does not own handlers for them

## Blocked Truth Overlay

Observation-owned unreadable or unsyncable truth must not be interpreted as
structural absence:

- local read-denied, invalid, or otherwise unsyncable paths are unavailable
  local truth, not local deletes
- a local unreadable subtree boundary suppresses all descendant structural
  actions; descendants are unavailable current truth, not "missing local"
- remote read-denied subtrees are unavailable remote truth, not remote deletes
- a remote unreadable subtree boundary suppresses all descendant structural
  actions; descendants are unavailable current truth, not "missing remote"
- subtree unavailability also suppresses publication-only cleanup for those
  descendants; baseline rows under an unreadable subtree must not be cleaned
  up just because current observation could not safely prove their absence
- tagged read-boundary issues suppress destructive and mutating work under the
  blocked subtree until later observation restores trustworthy truth

The planner therefore keeps structural reconciliation raw, then attaches an
explicit local/remote truth-status value to each path view before action
emission. `TruthAvailabilityIndex` is the reusable derived view over durable
authorities; planner suppression is the separate policy layer that blocks
action emission when observation issues prove the path is currently
unavailable.

That same truth-status derivation may also power debug/read-only inspection for
specific paths. Those debug reads must stay derived-only: descendants under a
read-denied subtree are reported as unavailable by scope derivation, not by
writing additional descendant `observation_issues` rows.

## Conflict Planning

SQLite emits conflict reconciliation rows, and the action builder expands those
rows directly into concrete runtime actions as part of the current action set.
It does not create or preserve separate conflict metadata.

Execution applies the already-expanded conflict policy immediately:

- edit/edit and create/create -> preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path; the dependent
  download explicitly requires the canonical local target to be missing after
  the conflict copy
- edit/delete -> keep local content in place and upload it as a new remote item

If executor-time precondition revalidation later proves a planned local delete
went stale, execution reports that stale precondition and returns control to
the engine; the next plan rebuild is still the only place that may emit the
concrete edit/delete upload.

## Folder And Move Semantics

Folder decisions are symmetric with file decisions. When Graph delta reports a
deleted parent folder without descendant delete rows, planner-visible pruning
removes the stale same-side descendant rows before SQLite reconciliation. SQLite
then emits ordinary descendant reconciliation rows: unchanged descendants under
the deleted parent delete locally, edited descendants recreate remote content,
and descendants missing on both sides clean up baseline.

After action building, a narrow parent-preservation pass rewrites only the
deleted parent folder action when descendant work that is admitted in the
current sync mode needs the folder to exist: local parent deletes become remote
folder creates, remote parent deletes become local folder creates, and cleanup
stays cleanup. Deferred descendant work must not preserve a parent that the
current mode would otherwise delete.

Folder reconciliation is existence/type-based, not metadata-based. Folder
size, mtime, and ETag churn caused by child mutations must not be treated as
content drift or expanded into conflict/download actions.

Local or remote moves that stay within one Graph drive become move actions.
Local move detection first matches unique filesystem identity
(`local_device`, `local_inode`, `local_has_identity`) between `baseline` and
`local_state` for both files and directories. File-hash matching is a fallback
only for files without identity. A local folder move emits one
`ActionRemoteMove` for the folder and makes descendant work depend on that
move instead of re-uploading or deleting/recreating the subtree. A moved file
whose content also changed emits the remote move before the upload/update at
the moved path. Identity mismatch at the same path is treated as replacement
or delete/create truth, not as a move. Moves that cross drive ownership are
decomposed into delete + upload because Graph move cannot cross drive
ownership.

## Mount-Local Planning

Some engines are rooted below the remote drive root. The planner therefore
receives engine-owned mount context:

- `DriveID`
- `RemoteRootItemID`

Ordinary sync actions are bound directly to that mounted subtree. When
`MakeAction(...)` leaves `Action.DriveID` empty for brand-new local work, the
planner fills it from the mount context before runtime admission. Ordinary
actions do not carry separate target-drive or target-root override fields.

Cross-drive move decomposition still happens in planning. By the time work
reaches execution, ordinary actions are mount-local concrete work.

## Directional Suppression

`download-only` and `upload-only` do not stop observation. They suppress only
the forbidden action classes and record those counts in `DeferredByMode`.
Conflict-copy is suppressed together with its paired remote-winner download in
`upload-only`; the planner must not rename local truth into a fake delete/create
sequence when the corresponding download is deferred by mode.

Permission scopes are different: planner is blocked-truth-aware for active read
scopes and observation-owned unreadable paths so unavailable truth is never
treated as a delete. Final runtime admission still happens later in the engine
against the ready frontier.

That invariant is subtree-wide: if a read-denied boundary blocks truth for a
folder, descendants and move endpoints below that boundary stay unavailable and
must not produce delete, move, cleanup, or repair actions until observation
restores trustworthy truth.
