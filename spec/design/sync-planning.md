# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/actions.go, internal/sync/api_types.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/core_types.go

Implements: R-2.1.3 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified]

## Overview

Planning is now split between SQLite structural diff and a Go actionable-set
builder.

- SQLite computes deterministic comparison and reconciliation rows from
  `baseline`, `local_state`, and `remote_state`
- Go turns those rows into the current executable action set after blocking,
  filtering, conflict handling, and dependency detection

The executable plan is runtime-owned and is not a durable SQLite authority.

## Ownership Contract

- Owns: turning reconciliation rows into the current executable action set,
  including conflict expansion, filtering, deferral, and dependency ordering
- Does Not Own: observation, execution, retry timing, scope persistence, or remote/local I/O
- Source of Truth: `baseline`, `local_state`, and `remote_state` through SQLite reconciliation rows, plus engine-owned sync mode and runtime policy.
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
- `local_state`: latest admissible local snapshot
- `remote_state`: latest admissible remote snapshot
- `Mode`: bidirectional, download-only, or upload-only
- runtime safety/policy inputs owned by the engine

Those rows feed `comparison_state` and `reconciliation_state`, including the
invariant that a baseline row absent from both snapshots becomes
`baseline_remove`.

## Pipeline

1. Run SQL structural diff and reconciliation over snapshots plus baseline.
2. Load reconciliation rows into Go.
3. Apply sync-mode and planner-owned safety rules.
4. Emit the current runtime action set, including conflict expansion into
   concrete actions.
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
- local changed + remote changed with different hashes -> `ActionConflictCopy` + dependent `ActionDownload`
- local unchanged + remote deleted -> `ActionLocalDelete`
- local changed + remote deleted -> `ActionUpload` with `ConflictEditDelete` metadata

### No baseline

- local only -> `ActionUpload`
- remote only -> `ActionDownload`
- local and remote with equal hashes -> `ActionUpdateSynced`
- local and remote with different hashes -> `ActionConflictCopy` + dependent `ActionDownload`

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

Permission scopes are different: they are engine-owned admission policy.
Planner output is not suppressed by remote blocked-boundary lists; the engine
filters blocked work during admission and retry/trial handling.
