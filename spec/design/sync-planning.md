# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/types.go

Implements: R-2.2 [implemented], R-2.3.1 [implemented], R-6.4.1 [implemented], R-6.4.2 [implemented], R-6.4.3 [implemented]

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

## Big-Delete Protection

Implements: R-6.2.5 [implemented]

Pure function on ActionPlan + Baseline. Triggers when:
- Global delete count exceeds `BigDeleteMaxCount` (default: 1000)
- Global delete percentage exceeds `BigDeleteMaxPercent` (default: 50%)
- Any single folder has ≥ max percent of its children being deleted AND that folder had ≥ min items

Returns `ErrBigDeleteTriggered`. Both global and per-folder checks apply.

## Design Constraints

- `localDeleted` implies `localChanged` (detectLocalChange returns true when Local is nil). Switch cases must check `localDeleted` before `localChanged` to prevent EF3 from stealing EF6's matches.
- Folder classifiers use upfront mode filtering (`localChanged = false` for download-only, `remoteChanged = false` for upload-only) parallel to the file classifier. Per-case mode filtering is error-prone (easy to miss a case).
- `RemoteState` carries `DriveID` for cross-drive correctness. Shared folder items from Drive A in Drive B's delta carry Drive A's DriveID. Planner DriveID propagation: Remote.DriveID wins → Baseline.DriveID fallback → empty for new local items.
- The planner detects action dependency cycles using DFS with white/gray/black node coloring after `buildDependencies()`. Cycle detection prevents deadlock in the DepTracker.
- Property-based tests for planner with random inputs — verify DAG invariant holds under all generated scenarios. [planned]

## Types (`types.go`)

Core types: `ChangeEvent`, `ChangeSource`, `ChangeType`, `ItemType`, `BaselineEntry`, `PathView`, `RemoteState`, `LocalState`, `Action`, `ActionPlan`, `Outcome`, `SyncMode`.
