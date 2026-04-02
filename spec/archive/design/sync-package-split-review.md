# Code Review: Sync Package Split

**Reviewer**: Claude
**Date**: 2026-03-16
**Scope**: All 7 packages produced by the `internal/sync/` modularization
**Governing spec**: `spec/design/sync-package-split.md`

---

## Executive Summary

The sync package split is **complete** — all findings from this review have been resolved. The original review identified 4 high, 3 medium, and 3 low severity findings. All 10 findings have been addressed (9 resolved, 1 deferred by design).

| Severity | Count | Status |
|----------|-------|--------|
| High | 4 | All resolved: PermissionHandler extracted, syncexec dependency fixed, types.go shim deleted, API pollution cleaned up |
| Medium | 3 | All resolved: testify violations converted, typed enums added, compute_status.go absorbed |
| Low | 3 | 2 resolved (Validates comments added, stale comment fixed), 1 deferred (engine_test.go split is optional) |

---

## 1. Package Structure

### 1.1 File Placement: PASS

All files landed in the correct packages per spec 3.1:

| Package | Prod files | Test files | Expected | Status |
|---------|-----------|------------|----------|--------|
| synctypes | 14 | 2 | ~8 prod | Exceeds (well-organized split into focused files) |
| syncstore | 12 | 10 | 11 prod | +1 (compute_status.go, see 3.2) |
| syncobserve | 9 | 17 | 9 prod | Correct |
| syncplan | 1 | 3 | 1 prod | Correct |
| syncdispatch | 4 | 4 | 4 prod | Correct |
| syncexec | 5 | 3 | 5 prod | Correct |
| sync (engine) | 6 | 9 | 5 prod | +1 (types.go re-export layer, see 4.1) |

### 1.2 No Old Files Left Behind: PASS

`internal/sync/` contains only engine/orchestration files. No store, observer, planner, dispatcher, or executor files remain.

---

## 2. Dependency Graph

### 2.1 synctypes (Leaf Package): PASS

Imports only `internal/driveid`, `internal/driveops` (interface types), `internal/graph` (type references in consumer interfaces), and stdlib. Zero sibling sync package imports. True leaf.

### 2.2 syncstore: PASS

Imports: synctypes, driveid, driveops. No forbidden siblings.

### 2.3 syncobserve: PASS

Imports: synctypes, driveid, driveops, graph, retry. No forbidden siblings. (Test files import syncplan and synctest — acceptable.)

### 2.4 syncplan: PASS

Imports: synctypes, driveid, stdlib only. Pure reconciliation logic.

### 2.5 syncdispatch: PASS

Imports: synctypes, stdlib only. No knowledge of OneDrive/Graph/filesystem.

### 2.6 syncexec: PASS (was FAIL — fixed in Step 1)

**Original finding**: `internal/syncexec/worker.go` imported `internal/syncstore` directly, violating the dependency diagram.

**Resolution**: Worker now accepts `synctypes.OutcomeWriter` interface instead of `*syncstore.SyncStore`. The syncstore import was removed. syncstore already satisfies OutcomeWriter via compile-time interface check. Dependency graph now matches spec.

### 2.7 sync (Engine): PASS

Imports all siblings as expected. Only package that does so.

---

## 3. Prerequisite Refactorings (Spec 4)

The spec defined 3 prerequisite refactorings (sections 4.1, 4.2, 4.3). **All now completed.**

### 3.1 PermissionHandler Extraction (Spec 4.1): DONE (Step 5)

**Resolution**: Extracted `PermissionHandler` struct to `internal/sync/permission_handler.go` with explicit dependencies. All 7 permission methods moved from `*Engine` to `*PermissionHandler`. Engine creates one in `NewEngine` and delegates via `e.permHandler`. Dependencies are now explicit struct fields (baseline, permChecker, permCache, logger, syncRoot, driveID, nowFn) plus callbacks for scope management (setScopeBlockFn, onScopeClearFn, isWatchModeFn).

### 3.2 compute_status.go Absorption (Spec 4.3): DONE (Step 3)

**Resolution**: All 4 functions (`computeNewStatus`, `computeDeleted`, `computeSameHash`, `computeDifferentHash`) and status constants absorbed into `store_observation.go`. Functions unexported as spec required. Tests moved to `store_observation_test.go`. Both `compute_status.go` and `compute_status_test.go` deleted.

### 3.3 Cross-Cutting Type Promotion (Spec 4.2): PASS

All cross-cutting types landed in synctypes as specified. ScopeKey, TrackedAction, WorkerResult, issue constants, store interfaces, consumer interfaces — all present and verified.

### 3.4 Shortcuts to Store: PASS

All 4 SyncStore shortcut CRUD methods correctly in `internal/syncstore/shortcuts.go`.

---

## 4. Backward Compatibility Layer

### 4.1 types.go Re-Export Shim: REMOVED (Step 4)

**Resolution**: All consumers migrated to import from canonical sub-packages directly (synctypes, syncstore, syncplan, syncdispatch, syncexec). CLI files (issues.go, verify.go, sync_helpers.go, failure_display.go), test files, and engine files all updated. The 348-line `internal/sync/types.go` re-export shim has been deleted entirely. No backward-compatibility artifacts remain.

---

## 5. API Pollution

### 5.1 Exported-for-Test Symbols: RESOLVED (Step 6)

**Resolution**: 8 syncplan functions unexported (`detectLocalChange`, `detectRemoteChange`, `resolvePathDriveID`, `isCrossDriveLocalMove`, `isCrossDriveRemoteMove`, `buildDependencies`, `detectDependencyCycle`, `exceedsDeleteThreshold`). All syncplan test files use `package syncplan` (internal test package) so they can access unexported functions directly. `ActionsOfType` moved to `internal/synctest/helpers.go` as a shared test utility. Cross-package callers in `syncobserve/enrichment_test.go` restructured to test through `Plan()` rather than calling internal functions.

### 5.2 Untyped String Enums: RESOLVED (Step 7)

**Resolution**: Typed enums `Direction` and `FailureCategory` added to `internal/synctypes/enums.go`. `SyncFailureParams.Direction`, `SyncFailureParams.Category`, `SyncFailureRow.Direction`, `SyncFailureRow.Category`, and `ActionableFailure.Direction` all updated to use typed fields. All raw string literals replaced with constants (`DirectionUpload`, `DirectionDownload`, `DirectionDelete`, `CategoryTransient`, `CategoryActionable`).

---

## 6. Code Quality

### 6.1 Testify Assertions: RESOLVED (Step 2)

**Resolution**: All 9 testify violations converted. 7 in `observer_local_collisions_test.go` and 2 in `observer_local_write_test.go` replaced with `require.NoError`, `require.FailNow`, `require.Len`, and `assert.FailNow` as appropriate.

### 6.2 Sentinel Errors: PASS

### 6.3 Package-Level Mutable State: PASS

### 6.4 Logging Conventions: PASS

### 6.5 Accept Interfaces / Return Structs: PASS

### 6.6 Nil Context: PASS

### 6.7 Comments: PASS

### 6.8 Compile-Time Interface Checks: PASS

---

## 7. Test Quality

### 7.1 Test Placement: PASS

### 7.2 Shared Test Helpers: PASS

`syncstore/testhelpers_test.go` duplicates `synctest` helpers due to circular import (synctest → syncstore). This is the correct solution — verified.

### 7.3 require vs assert Discipline: PASS

### 7.4 Missing `// Validates:` Comments: RESOLVED (Step 8)

**Resolution**: `// Validates:` comments added to all 14 test files that contain test functions. The 2 helper files (executor_test_helpers_test.go, test_helpers_test.go) correctly excluded as they contain no test functions.

### 7.5 engine_test.go Size: 4,898 LINES

119 test functions covering 7+ categories (one-shot, watch, conflict, result classification, scope/trial, observation/reconciliation, configuration). Not a quality problem — tests are well-organized with section comments — but a maintainability concern.

**Recommended split** (not blocking):
- Extract `engine_conflict_test.go` (8 tests, ~270 lines)
- Extract `engine_result_test.go` (9 tests, ~400 lines)
- Extract `engine_observe_test.go` (24 tests, ~1,000 lines)
- Keep `engine_test.go` at ~2,500 lines (RunOnce, RunWatch, config, mocks)

### 7.6 synctypes Coverage: 26.9%

The 4 functions flagged (`FindOrphans`, `BlocksAction`, `ScopeKeyForStatus`, `TargetsOwnDrive`) all have **0% coverage in synctypes' own tests** but DO have direct unit tests in other packages (syncdispatch/scope_test.go, syncexec/worker_test.go, sync/engine_test.go). These are real unit tests, not just integration coverage. The coverage metric is misleading because the tests live in the consuming packages. Moving them to synctypes would improve the metric but provides no additional safety.

---

## 8. Documentation

### 8.1 GOVERNS Lines: PASS

### 8.2 CLAUDE.md Routing Table: PASS

### 8.3 sync-package-split.md Status: RESOLVED

**Resolution**: Status updated to "Complete — review findings resolved" now that all prerequisite refactorings are done, types.go shim is deleted, and dependency graph matches spec.

### 8.4 Stale References: RESOLVED (Step 3)

**Resolution**: `compute_status.go` was absorbed into `store_observation.go` (Step 3), eliminating the stale comment along with the file.

---

## 9. Build & Tooling

| Check | Status |
|-------|--------|
| `go build ./...` | PASS |
| `go vet ./...` | PASS (no import cycles) |
| `gofumpt` | PASS |
| `goimports` | PASS |
| `golangci-lint run` | PASS |

---

## 10. Findings Summary

### High Severity — All Resolved

| # | Finding | Resolution | Step |
|---|---------|-----------|------|
| H-1 | PermissionHandler not extracted | Extracted to `permission_handler.go` with explicit deps, Engine delegates | Step 5 |
| H-2 | syncexec imports syncstore | Worker accepts `synctypes.OutcomeWriter` interface, syncstore import removed | Step 1 |
| H-3 | 348-line re-export shim | All consumers migrated to canonical imports, `types.go` deleted | Step 4 |
| H-4 | Exported-for-test API pollution | 8 functions unexported, `ActionsOfType` moved to `synctest` | Step 6 |

### Medium Severity — All Resolved

| # | Finding | Resolution | Step |
|---|---------|-----------|------|
| M-1 | 9 testify violations | All converted to `require`/`assert` calls | Step 2 |
| M-2 | Untyped string enums | `Direction` and `FailureCategory` typed enums added, all literals replaced | Step 7 |
| M-3 | compute_status.go not absorbed | Absorbed into `store_observation.go`, unexported, file deleted | Step 3 |

### Low Severity — 2 Resolved, 1 Deferred

| # | Finding | Resolution | Step |
|---|---------|-----------|------|
| L-1 | 14 test files missing `// Validates:` | Comments added to all 14 test files | Step 8 |
| L-2 | Stale comment referencing old package path | Eliminated when `compute_status.go` was absorbed | Step 3 |
| L-3 | engine_test.go at 4,898 lines / 119 tests | **Deferred** — optional split, tests are well-organized with section comments | N/A |

### Validated Non-Issues

| Claim | Verdict | Why |
|-------|---------|-----|
| ExecutorConfig embeds too many concerns | **Not an issue** | 11 fields, 6 in constructor. Write-once in production. Within normal Go bounds. |
| Mock explosion needs shared syncmock | **Not an issue** | 14 mocks across 5 packages, zero semantic duplication. Each mocks a different interface. Local mocks are clearer. |
| synctypes coverage gap (26.9%) | **Misleading metric** | All 4 flagged functions have direct unit tests in consuming packages. Coverage is real, just measured in the wrong package. |
| synctest/syncstore circular dep duplication | **Justified** | synctest → syncstore import cycle. Local testhelpers_test.go with ~60 lines of duplication is the correct fix. |

---

## 11. Concrete Fix Designs (All Implemented)

All fix designs below have been implemented. The designs are retained for reference.

### Fix 1: H-2 — syncexec/worker.go imports syncstore (DONE — Step 1)

Worker now accepts `synctypes.OutcomeWriter` instead of `*syncstore.SyncStore`. Import removed.

### Fix 2: H-1 — Extract PermissionHandler struct (DONE — Step 5)

`PermissionHandler` extracted to `internal/sync/permission_handler.go`. Engine creates one in `NewEngine` and delegates via `e.permHandler`.

### Fix 3: H-3 — Delete types.go re-export shim (DONE — Step 4)

All consumers migrated to canonical sub-package imports. `internal/sync/types.go` deleted.

### Fix 4: H-4 — Narrow exported-for-test symbols in syncplan (DONE — Step 6)

8 functions unexported. `ActionsOfType` moved to `internal/synctest/helpers.go`.

### Fix 5: M-1 — Convert 9 testify violations (DONE — Step 2)

All 9 violations in `observer_local_collisions_test.go` and `observer_local_write_test.go` converted.

### Fix 6: M-2 — Typed Direction and FailureCategory enums (DONE — Step 7)

Typed enums added to `synctypes/enums.go`, all struct fields and literals updated.

### Fix 7: M-3 — Absorb compute_status.go into store_observation.go (DONE — Step 3)

Functions absorbed and unexported. Files deleted.

---

## 12. Resolution Summary

All findings resolved in the following order:

1. **H-2** (Step 1): Fixed syncexec→syncstore dependency via OutcomeWriter interface
2. **M-1** (Step 2): Converted 9 testify violations
3. **M-3** (Step 3): Absorbed compute_status.go into store_observation.go (also resolves L-2)
4. **H-3** (Step 4): Deleted types.go re-export shim, migrated all consumers
5. **H-1** (Step 5): Extracted PermissionHandler struct
6. **H-4** (Step 6): Unexported 8 syncplan functions, moved ActionsOfType to synctest
7. **M-2** (Step 7): Added typed Direction and FailureCategory enums
8. **L-1** (Step 8): Added Validates comments to 14 test files

**Remaining optional work:**

- **L-3**: Split engine_test.go by concern (deferred — well-organized, not blocking)
