# Cycle Terminology Cleanup Plan

Tracking document for removing the misleading "cycle" concept from the codebase. The watch-mode sync engine is event-driven with no discrete cycles — three independent subsystems (observers, buffer+planner, worker pool) run concurrently. Multiple batches can be in-flight simultaneously. The `cycleID` infrastructure is dead code in watch mode.

**Replacement terms:**
- "sync cycle" → "sync run" (one-shot) or "sync pass" (general)
- "per-cycle" → "per-batch" or "per-run"
- "next cycle" → "next run", "next pass", or "next observation"
- "cycle complete" → "run complete" or "batch complete"

**Not in scope:** dependency cycle detection (`ErrDependencyCycle`), recycle bin, lifecycle, vault lock/unlock, import cycle, CPU cycles.

---

## 1. Dead Code Removal

### `internal/sync/tracker.go`

Remove the entire `cycleTracker` subsystem. Always passed `""` in watch mode. Never called from production watch-mode code.

| What | Lines |
|------|-------|
| `CycleID` field on `TrackedAction` | 39 |
| `cycleTracker` type | 46-49 |
| `cycles` map, `cycleLookup` map, `cyclesMu` mutex | 70-73, 85-86, 101-102 |
| `cycleID` parameter on `Add()` | 111-130 |
| `registerCycleLocked()` | 152-166 |
| `completeCycle()` | 219-236 |
| `CycleDone()` | 268-284 |
| `CleanupCycle()` | 286-293 |
| Comments: "overlapping cycles" | 193, 198 |
| Comment: "Advance per-cycle tracker" | 208-209 |

### `internal/sync/tracker_test.go`

Remove dead cycle tests.

| What | Lines |
|------|-------|
| `TestDepTracker_CycleDone` | 467-534 |
| `TestDepTracker_CleanupCycle` | 536-568 |
| `TestDepTracker_CycleDone_UnknownCycle` | 599-612 |

### `internal/sync/worker.go`

| What | Lines |
|------|-------|
| `CycleID string` field on `WorkerResult` | 53 |
| "cycle result tracking" comment | 42 |
| "in-memory cycle result tracking" comment | 265 |

### `internal/sync/worker_test.go`

| What | Lines |
|------|-------|
| `tracker.Add(..., "cycle-fail")` | 363 |
| "correct cycle IDs" comment | 398 |
| `tracker.Add(..., "test-cycle")` | 416 |
| `assert.Equal(t, "test-cycle", result.CycleID)` | 447 |
| `tracker.Add(..., "panic-cycle")` | 560 |

### `internal/sync/fault_injection_test.go`

| What | Lines |
|------|-------|
| `tracker.Add(action, 0, nil, "cycle-1")` | 54 |

---

## 2. Misleading Comments in Production Code

### `internal/sync/engine.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 48 | "per-cycle options for RunOnce" | "per-run options for RunOnce" |
| 78 | "orchestrates a complete sync cycle" | "orchestrates a complete sync run" |
| 208 | "executes a single sync cycle" | "executes a single sync run" |
| 221 | `"sync cycle starting"` log message | `"sync run starting"` |
| 265 | `"sync cycle complete: no changes detected"` | `"sync run complete: no changes detected"` |
| 313 | `"sync cycle complete"` | `"sync run complete"` |
| 329 | "after a sync cycle" | "after a sync run" |
| 375 | `Add(..., "")` — remove empty cycleID arg after tracker cleanup | — |
| 789 | "since the last cycle" | "since the last pass" |

### `internal/sync/executor.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 43 | `errClassFatal // abort the entire sync cycle` | `errClassFatal // fail action immediately; in watch mode, other actions continue` |

### `internal/sync/planner.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 38 | "The sync cycle should halt" | "The sync run should halt" |

### `internal/sync/orchestrator.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 67 | "single sync cycle" | "single sync run" |

### `internal/sync/drive_runner.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 10 | "single drive's sync cycle" | "single drive's sync run" |

### `internal/sync/baseline.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 119 | "baseline at cycle start and commits outcomes at cycle end" | "baseline at run start and commits outcomes at run end" |
| 1494 | "completed RunOnce cycle" | "completed RunOnce run" |

### `internal/sync/observer_local.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 91 | "reuse across cycles" | "reuse across runs" |
| 152 | "per-cycle drops" | "per-run drops" |
| 160 | "double-counting across cycles" | "double-counting across runs" |

### `internal/sync/observer_remote.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 101 | "next sync cycle" | "next sync run" |

### `internal/sync/permissions.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 37 | "each cycle" | "each run" |
| 46 | "each sync cycle" | "each sync pass" |
| 239-240 | "every cycle" | "every pass" |
| 246 | "each cycle" | "each pass" |

### `internal/sync/executor_delete.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 97 | "next sync cycle" | "next sync pass" |

### `internal/driveops/stale_partials.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 12 | "After a sync cycle completes" | "After a sync run completes" |

### `internal/driveops/cleanup.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 10 | "fail a sync cycle" | "fail a sync run" |

### `internal/graph/delta.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 150 | "next sync cycle" | "next sync run" |

### `internal/graph/upload.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 383 | "next sync cycle" | "next sync pass" |

### `sync.go` (root CLI)

| Line | Current | Replacement |
|------|---------|-------------|
| 20 | "one-shot sync cycle" | "one-shot sync run" |

---

## 3. Test Renames

### `internal/sync/engine_test.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 314 | `TestRunOnce_Bidirectional_FullCycle` | `TestRunOnce_Bidirectional_FullRun` |
| 576 | `TestRunOnce_BaselineUpdatedAfterCycle` | `TestRunOnce_BaselineUpdatedAfterRun` |

### `internal/sync/enrichment_test.go`

| Line | Current | Replacement |
|------|---------|-------------|
| 22 | "cycle does NOT produce" | "run does NOT produce" |
| 36 | "Next cycle:" | "Next run:" |
| 75 | "next cycle" | "next run" |
| 127 | `TestPerSideHash_5CycleStabilityProof` | `TestPerSideHash_5RunStabilityProof` |
| 137 | `for cycle := range 5` | `for run := range 5` |
| 167 | `"cycle %d"` | `"run %d"` |
| 170 | `"cycle %d:"` | `"run %d:"` |

---

## 4. Documentation

### `LEARNINGS.md`

| Line | Current | Replacement |
|------|---------|-------------|
| 96 | "next sync cycle" | "next sync pass" |
| 201 | "next sync cycle" | "next sync pass" |
| 351 | "wastes a cycle" | "wastes a pass" |
| 451 | "next sync cycle" | "next sync pass" |
| 617 | "Legacy cycle tracking replaced by durable failure state" | "Legacy in-memory tracking replaced by durable failure state" |
| 618 | `cycleFailures`/`watchCycleCompletion` reference | Reword to describe the old mechanism without "cycle" framing |
| 624-625 | "current cycle" | "current run" |

### `BACKLOG.md`

| Line | Current | Replacement |
|------|---------|-------------|
| 268 | B-121: "cycleTracker" | Update to note cycleTracker was removed; B-121 completed via durable failure state |

### `docs/design/observability.md`

| Line | Current | Replacement |
|------|---------|-------------|
| 23 | "Gone after cycle" | "Gone after run" |
| 43 | "Gone after cycle" | "Gone after run" |

### `docs/design/failures.md`

This document uses "cycle" extensively to describe the delta token bug. Reword throughout:
- "failed cycle → successful cycle → token committed" → "failed batch → successful batch → token committed"
- "one poll interval (typically 5 minutes)" framing is correct but "cycle" is not
- "the next successful cycle" → "the next successful batch"

### `docs/archive/prerelease_review.md`

| Line | Current | Replacement |
|------|---------|-------------|
| 83 | "cyclesMu" lock ordering | Update if cyclesMu is removed; otherwise note as historical |
| 110 | "full cycle success", "CycleDone" | Low priority — archive doc |

---

## 5. Summary

| Category | Files | Occurrences | Priority |
|----------|-------|-------------|----------|
| Dead code removal (tracker cycle infra) | 5 | ~80 | P1 — remove |
| Production comments | 14 | ~30 | P1 — reword |
| Test names and comments | 4 | ~15 | P2 — rename |
| Design docs | 3 | ~10 | P2 — reword |
| LEARNINGS.md | 1 | ~6 | P2 — reword |
| Archive docs | 1 | ~4 | P3 — low priority |
| **Total** | **~25 files** | **~145** | |

Delete this file after the cleanup is complete.
