# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/planner_sqlite.go, internal/sync/planner_truth_overlay.go, internal/sync/truth_status.go, internal/sync/actions.go, internal/sync/api_types.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/core_types.go

Implements: R-2.1.3 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified]

## Overview

Planning is now split between SQLite structural diff and a Go actionable-set
builder.

- SQLite computes deterministic comparison and reconciliation rows from raw
  `baseline` plus planner-visible filtered `local_state` and `remote_state`
  temp views
- Go turns those rows into the current executable action set after blocking,
  filtering, conflict handling, and dependency detection

The executable plan is runtime-owned and is not a durable SQLite authority.
Shortcut-root lifecycle planning follows the same functional-core rule inside
`internal/sync`: remote shortcut topology observations and local alias identity
facts enter deterministic shortcut-root planner helpers, while Graph,
filesystem, SQLite, logging, clocks, and goroutines stay in the engine shell.
The output is parent-owned shortcut-root state plus child work snapshot
intent; multisync receives only that child work intent.

## Ownership Contract

- Owns: turning reconciliation rows into the current executable action set,
  including conflict expansion, filtering, deferral, and dependency ordering
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
| Planner conflict detection remains internal and deterministic for edit/edit, create/create, and edit/delete cases. | `TestClassifyFile_EF5_EditEditConflict`, `TestClassifyFile_EF12_CreateCreateConflict`, `TestClassifyFile_EF9_EditDeleteConflict`, `TestConflictClassificationsRemainConflictsAcrossSyncModes` |
| Mode-specific deferral and dependency ordering stay planner-owned rather than executor- or CLI-owned. | `TestSyncModeFromFlags`, `internal/sync/planner_test.go`, `internal/sync/planner_edge_test.go`, `internal/sync/planner_cascade_test.go` |
| Planner no longer depends on manual delete-approval or durable conflict-request state. | `internal/sync/planner_test.go`, `internal/sync/planner_crossdrive_test.go`, `internal/sync/planner_fuzz_test.go` |

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
Raw `baseline` is not filtered. Those views feed `comparison_state` and
`reconciliation_state`, including the invariant that a baseline row absent from
both filtered current snapshots becomes `baseline_remove`.

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
2. Run SQL structural diff and reconciliation over filtered current views plus
   baseline.
3. Load reconciliation rows into Go.
4. Derive per-path local/remote truth status from filtered `observation_issues`
   through `TruthAvailabilityIndex`.
5. Apply planner-owned suppression and sync-mode safety rules on top of that
   derived status.
6. Emit the current runtime action set, including conflict expansion into
   concrete actions.
7. Expand folder delete cascades so descendants get explicit work.
8. Bind ordinary actions to the engine's mounted drive/root context.
9. Build dependency edges and reject dependency cycles.

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
- local changed + remote deleted -> `ActionUpload` with `ConflictEditDelete` metadata

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

The actionable-set builder detects conflicts, records their type in
`ConflictInfo`, and expands them into concrete runtime actions as part of the
current action set.

- `ConflictEditEdit`: both sides changed existing content differently
- `ConflictCreateCreate`: both sides independently created different content
- `ConflictEditDelete`: local edit raced with remote delete

Execution applies the already-expanded conflict policy immediately:

- edit/edit and create/create -> preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path
- edit/delete -> local wins automatically by re-uploading local content

If executor-time precondition revalidation later proves a planned local delete
went stale, execution reports that stale precondition and returns control to
the engine; the next plan rebuild is still the only place that may emit the
concrete edit/delete upload.

## Folder And Move Semantics

Folder decisions are symmetric with file decisions, but folder deletes also
trigger descendant cascade expansion because Graph delta only reports the
deleted parent folder.

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
Conflict-copy is suppressed together with its paired remote-resolution download
in `upload-only`; the planner must not rename local truth into a fake
delete/create sequence when the corresponding remote-resolution action is
deferred by mode.

Permission scopes are different: planner is blocked-truth-aware for active read
scopes and observation-owned unreadable paths so unavailable truth is never
treated as a delete. Final runtime admission still happens later in the engine
against the ready frontier.

That invariant is subtree-wide: if a read-denied boundary blocks truth for a
folder, descendants and move endpoints below that boundary stay unavailable and
must not produce delete, move, cleanup, or repair actions until observation
restores trustworthy truth.
