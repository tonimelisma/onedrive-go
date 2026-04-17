# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/actions.go, internal/sync/api_types.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/core_types.go

Implements: R-2.1.3 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified]

## Overview

The planner is a deterministic function:

`Plan(changes, baseline, mode, safetyConfig, deniedPrefixes) -> ActionPlan`

It owns only classification and ordering. It does not touch SQLite, Graph, or
the local filesystem.

In the clean-cut SQLite refactor, comparison and reconciliation are moving
down into SQLite before planner materialization. `internal/sync/sqlite_compare.go`
now computes durable snapshot-vs-baseline comparison and desired-outcome rows
from `baseline`, `local_state`, and `remote_state`. The legacy planner still
consumes event-shaped inputs until the next increment deletes that boundary.

## Ownership Contract

- Owns: path classification, move detection, action dependency ordering, and directional deferral reporting
- Does Not Own: observation, execution, retry timing, scope persistence, or remote/local I/O
- Source of Truth: `PathChanges`, baseline snapshot, sync mode, and denied-prefix policy supplied by the engine
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

- `PathChanges`: local and remote observations already normalized into one path-keyed structure
- `Baseline`: last known synced common ancestor
- `Mode`: bidirectional, download-only, or upload-only
- `SafetyConfig`: current planner-facing safety knobs; no delete-approval workflow remains
- `deniedPrefixes`: engine-owned read-only subtrees, typically from permission policy

SQLite-side pre-planning inputs now exist in parallel:

- `local_state`: latest admissible local snapshot
- `remote_state`: latest admissible remote snapshot
- `baseline`: last converged synced truth

Those rows feed `comparison_state` and `reconciliation_state`, including the
new invariant that a baseline row absent from both snapshots becomes
`baseline_remove`.

## Pipeline

1. Build `PathView` values from changes plus baseline.
2. Detect remote moves from `ChangeMove` events.
3. Detect local moves by correlating delete/create pairs with matching hashes.
4. Classify each remaining path view into one or more actions.
5. Expand folder delete cascades so descendants get explicit work.
6. Enrich actions with target-drive and target-root metadata.
7. Build dependency edges and reject dependency cycles.

## File Decisions

### Baseline exists

- local deleted + remote unchanged -> `ActionRemoteDelete`
- local deleted + remote changed -> `ActionDownload` (remote wins canonical path)
- local deleted + remote deleted -> `ActionCleanup`
- local changed + remote unchanged -> `ActionUpload`
- local unchanged + remote changed -> `ActionDownload`
- local changed + remote changed with equal hashes -> `ActionUpdateSynced`
- local changed + remote changed with different hashes -> `ActionConflict(ConflictEditEdit)`
- local unchanged + remote deleted -> `ActionLocalDelete`
- local changed + remote deleted -> `ActionConflict(ConflictEditDelete)`

### No baseline

- local only -> `ActionUpload`
- remote only -> `ActionDownload`
- local and remote with equal hashes -> `ActionUpdateSynced`
- local and remote with different hashes -> `ActionConflict(ConflictCreateCreate)`

## Conflict Planning

The planner still detects conflicts, but conflicts are no longer durable
workflow state. `ActionConflict` is immediate executor work.

- `ConflictEditEdit`: both sides changed existing content differently
- `ConflictCreateCreate`: both sides independently created different content
- `ConflictEditDelete`: local edit raced with remote delete

Execution decides the concrete conflict action:

- edit/edit and create/create -> preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path
- edit/delete -> local wins automatically by re-uploading local content

## Folder And Move Semantics

Folder decisions are symmetric with file decisions, but folder deletes also
trigger descendant cascade expansion because Graph delta only reports the
deleted parent folder.

Local or remote moves that stay within one drive become move actions. Moves
that cross drive ownership are decomposed into delete + upload because Graph
move is a single-drive operation.

## Shared-Root Target Metadata

Some configured drives are rooted below the remote drive root. The planner
therefore enriches actions with:

- `TargetDriveID`
- `TargetRootItemID`
- `TargetRootLocalPath`

This lets execution and post-mutation path convergence address the correct
remote drive and strip the correct local shared-root prefix without
rediscovering that ownership ad hoc.

## Directional Suppression

`download-only` and `upload-only` do not stop observation. They suppress only
the forbidden action classes and record those counts in `DeferredByMode`.

`deniedPrefixes` are different: they are engine-owned permission policy.
Planner suppression caused by denied prefixes is **not** counted as ordinary
directional deferral.
