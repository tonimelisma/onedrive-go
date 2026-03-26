# Sync Planning

GOVERNS: internal/syncplan/planner.go, internal/synctypes/*.go, internal/sync/types.go

Implements: R-2.2 [verified], R-2.3.1 [verified], R-6.4.1 [verified], R-6.4.2 [verified], R-6.4.3 [verified], R-6.7.7 [verified], R-2.14.2 [verified]

## Overview

The planner is the intellectual core of the sync engine. It is a pure function — no I/O, no database access. Takes `([]PathChanges, *Baseline, SyncMode, *SafetyConfig, deniedPrefixes)` and returns `(*ActionPlan, error)`.

## Pipeline

1. Build `PathView` values from changes + baseline
2. Detect moves (remote: from ChangeMove events; local: hash-based correlation)
3. Classify each PathView using the decision matrix
4. Apply filters symmetrically to both remote and local items
5. Order the plan (folder creates before files, depth-first for deletes)
6. Safety checks (big-delete) as pure functions on ActionPlan + Baseline

## File Decision Matrix

| E# | Local | Remote | Baseline | Action |
|----|-------|--------|----------|--------|
| EF1 | unchanged | unchanged | exists | no-op |
| EF2 | unchanged | changed | exists | download |
| EF3 | changed | unchanged | exists | upload |
| EF4 | changed | changed (same hash) | exists | update synced (convergent edit) |
| EF5 | changed | changed (diff hash) | exists | **conflict** (edit-edit) |
| EF6 | deleted | unchanged | exists | remote delete |
| EF7 | deleted | changed | exists | download (remote wins) |
| EF8 | unchanged | deleted | exists | local delete |
| EF9 | changed | deleted | exists | **conflict** (edit-delete) |
| EF10 | deleted | deleted | exists | cleanup |
| EF11 | new | new (same hash) | none | update synced (convergent create) |
| EF12 | new | new (diff hash) | none | **conflict** (create-create) |
| EF13 | new | absent | none | upload |
| EF14 | absent | new | none | download |

## Folder Decision Matrix

| E# | Local | Remote | Baseline | Action |
|----|-------|--------|----------|--------|
| ED1 | exists | exists | exists | no-op |
| ED2 | exists | exists | none | adopt |
| ED3 | absent | exists | none | create locally |
| ED4 | absent | exists | exists | recreate locally |
| ED5 | exists | absent | none | create remotely |
| ED6 | exists | deleted | exists | delete locally |
| ED7 | absent | deleted | exists | cleanup |
| ED8 | absent | absent | exists | cleanup |

Folders use existence-based reconciliation — no hash check needed.

## Change Detection

Per-side baselines for SharePoint enrichment correctness:
- `detectLocalChange`: compares `Local.Hash` against `Baseline.LocalHash`
- `detectRemoteChange`: compares `Remote.Hash` against `Baseline.RemoteHash`

## Big-Delete Protection (One-Shot)

Implements: R-6.2.5 [verified], R-6.4.1 [verified]

Single absolute count threshold: `exceedsDeleteThreshold(deleteCount, threshold)` returns true when `deleteCount > threshold` and `threshold > 0`. No percentage checks, no per-folder checks (industry standard: rclone, rsync, abraunegg).

`SafetyConfig` has one field: `BigDeleteThreshold int` (default: 1000, from user config). When `--force` is set, threshold is `math.MaxInt32` (effectively disabled). Returns `ErrBigDeleteTriggered` when exceeded.

In watch mode, the planner-level check is disabled (`threshold=MaxInt32`) — the engine's rolling `deleteCounter` handles protection instead (see sync-engine.md).

## Design Constraints

- `localDeleted` implies `localChanged` (detectLocalChange returns true when Local is nil). Switch cases must check `localDeleted` before `localChanged` to prevent EF3 from stealing EF6's matches.
- Folder classifiers use upfront mode filtering (`localChanged = false` for download-only, `remoteChanged = false` for upload-only) parallel to the file classifier. Per-case mode filtering is error-prone (easy to miss a case).
- `RemoteState` carries `DriveID` for cross-drive correctness. Shared folder items from Drive A in Drive B's delta carry Drive A's DriveID. Planner DriveID propagation: Remote.DriveID wins → Baseline.DriveID fallback → empty for new local items.
- `RemoteState` carries `RemoteDriveID` and `RemoteItemID` for shortcut scope identity (D-5). These are transient fields populated by `remoteStateFromEvent()` from `ChangeEvent` — not persisted in `remote_state` table. `makeAction()` uses them to populate `Action.targetShortcutKey` and `Action.targetDriveID` so active-scope admission can distinguish own-drive vs shortcut-scoped failures (R-6.8.12, R-6.8.13).
- The planner detects action dependency cycles using DFS with white/gray/black node coloring after `buildDependencies()`. Cycle detection prevents deadlock in the DepGraph.
- Property-based tests for planner with random inputs — verify DAG invariant holds under all generated scenarios. [planned]

## Folder Delete Cascade Expansion

When the Graph API delta endpoint reports a parent folder as deleted, it does NOT report individual child item deletions. Without intervention, the planner generates a single `ActionLocalDelete` for the parent, and the executor's `DeleteLocalFolder` refuses to remove a non-empty directory.

**Solution**: Step 2.5 in `Plan()` — `expandFolderDeleteCascades()` runs after per-path classification but before dependency building. For each folder `ActionLocalDelete` or `ActionCleanup`, it walks `baseline.DescendantsOf(path)` to find all baseline entries under the folder and synthesizes additional delete/cleanup actions.

**Deduplication**: Maintains an `existingPaths` set to prevent double-generation when delta reports both parent and child.

**Safety preservation**:
- Hash-before-delete (S4): cascaded file deletes go through `DeleteLocalFile` which verifies hash against baseline before deletion — if locally modified, creates conflict copy.
- Big-delete protection: cascaded actions increase the delete count → threshold check at Step 4 happens after cascade → triggers correctly.
- Non-disposable check: `DeleteLocalFolder` remains as defense-in-depth.
- Upload-only mode: cascade is skipped entirely (no local deletions in upload-only).

## Cross-Drive Move Guard

Implements: R-6.7.21 [verified]

`detectLocalMoves()` correlates local deletes+creates by hash to detect renames. `detectRemoteMoves()` processes `ChangeMove` events from the delta API. Both can match moves that cross drive boundaries (e.g., own drive → shared folder shortcut). However, `MoveItem` is a single-drive API call — cross-drive moves fail.

**Guard logic**: Before emitting a move action, the planner checks whether the source and destination paths belong to different drives:
- `isCrossDriveLocalMove()`: source drive from `views[deletePath].Baseline.DriveID`; destination drive from `resolvePathDriveID(createPath, baseline)` which walks up parent directories in the baseline to find the owning drive.
- `isCrossDriveRemoteMove()`: compares `view.Baseline.DriveID` with `view.Remote.DriveID`.

When a cross-drive move is detected, the match is skipped — paths fall through to normal per-path classification which produces a delete + upload (the correct decomposition for cross-drive operations).

**Conservative zero-guard**: If either drive ID is unknown (zero), the guard returns `false` (don't decompose). This prevents false positives for items with incomplete baseline data.

## Types (`types.go`)

Core types: `ChangeEvent`, `ChangeSource`, `ChangeType`, `ItemType`, `BaselineEntry`, `PathView`, `RemoteState`, `LocalState`, `Action`, `ActionPlan`, `Outcome`, `SyncMode`.
