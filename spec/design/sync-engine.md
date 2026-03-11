# Sync Engine

GOVERNS: internal/sync/engine.go, internal/sync/engine_shortcuts.go, internal/sync/orchestrator.go, internal/sync/drive_runner.go, sync.go, sync_helpers.go

Implements: R-2.1 [verified], R-2.6 [verified], R-2.8 [verified], R-3.4.2 [verified], R-2.10.1 [verified], R-2.10.2 [planned], R-2.10.3 [verified], R-2.10.4 [planned], R-2.10.7 [verified], R-2.10.9 [planned], R-2.10.10 [planned], R-2.10.12 [planned], R-2.10.13 [planned], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [planned], R-2.10.24 [planned], R-2.10.25 [planned], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified], R-2.10.30 [planned], R-2.10.31 [planned], R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified], R-2.10.38 [planned], R-6.6.7 [planned], R-6.6.8 [planned], R-6.6.9 [planned], R-6.6.10 [planned], R-6.6.12 [planned], R-6.7.27 [planned], R-6.8.15 [verified]

## Engine (`engine.go`)

Wires the sync pipeline: observers → buffer → planner → executor → SyncStore. Two entry points:

- `RunOnce()`: one-shot sync. Observes all changes, plans, executes, returns `SyncReport`.
- `RunWatch()`: continuous sync. Runs `RunOnce` in a loop, triggered by filesystem events and delta polling.

Watch mode uses a unified tick loop: filesystem events are debounced by the change buffer, remote changes are polled at `poll_interval` (default 5 minutes). Periodic full reconciliation runs every 24 hours to detect missed delta deletions.

### Error Classification (`classifyResult()`)

Implements: R-6.8.9 [verified], R-6.8.15 [verified], R-6.7.27 [planned]

Pure function `classifyResult(*WorkerResult) → (resultClass, scopeKey)`. Single classification point for all worker results. No side effects — classification is separate from routing ("functions do one thing"). Six result classes:

- `resultSuccess`: action succeeded
- `resultRequeue`: transient failure — re-queue with backoff
- `resultScopeBlock`: scope-level failure (429, 507, 5xx pattern) — feed scope detection
- `resultSkip`: non-retryable — record and move on
- `resultShutdown`: context canceled — discard silently, no failure recorded
- `resultFatal`: abort sync pass (401 unrecoverable auth)

Classification table: 401 → fatal, 403 → skip (with handle403 side effect), 429 → scopeBlock `throttle:account`, 507 → scopeBlock `quota:own` or `quota:shortcut:$key`, 400 + outage pattern → requeue, 5xx → requeue, 408/412/404/423 → requeue, context.Canceled → shutdown, os.ErrPermission → skip.

`isOutagePattern()` detects known 400 outage patterns (e.g., "ObjectHandle is Invalid") that are actually transient service outages. Distinguished from phantom drive 400s (R-6.7.11) by error body inspection.

### Scope Detection and Management

Implements: R-2.10.3 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [planned], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified]

`processWorkerResult()` classifies each result and routes it: success → Complete + counter, requeue → ReQueue with backoff, scopeBlock → feed scope detection + ReQueue, skip → handle403 side effect + Complete, shutdown → Complete (no failure), fatal → Complete.

`feedScopeDetection()` feeds results into `ScopeState.UpdateScope()`. When a threshold is crossed, creates a scope block via `applyScopeBlock()` which calls `tracker.HoldScope()`.

The engine owns all completion decisions — workers are pure executors. Engine-owned counters (`succeeded`, `failed` atomics) replace the worker-owned counters removed in this refactoring. `drainWorkerResults()` processes results concurrently for both one-shot and watch modes.

Unified backoff: `min(1s * 2^attempt, 5min)` via `computeBackoff()`. Single mechanism — no reconciler handoff. `recordDiagnosticFailure()` writes to `sync_failures` for `onedrive status` visibility without retry scheduling.

### ScopeState

Implements: R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified]

In-memory data structure in `scope.go`: sliding windows (scope_key → `slidingWindow`) for scope escalation detection. Engine-internal — no cross-engine coordination (each engine discovers independently). Scope blocks are stored in the tracker's `scopeBlocks` map and enforce held queuing.

### Scanner ScanResult Contract

Implements: R-2.11.5 [implemented], R-2.10.2 [planned]

Scanner returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. Engine processes skipped items via two methods:

- **`recordSkippedItems(skipped []SkippedItem)`** — Groups skipped items by reason, batch-upserts to `sync_failures` as actionable failures. Uses aggregated logging: when >10 items share the same reason, logs 1 WARN summary with count and sample paths, individual paths at DEBUG. When <=10 items, logs each as an individual WARN.
- **`clearResolvedSkippedItems(skipped []SkippedItem)`** — Deletes `sync_failures` entries for files that are no longer skipped (e.g., user renamed a previously invalid file). Compares current skipped paths against recorded failures and removes stale entries.

### Aggregated Logging

Implements: R-6.6.7 [planned], R-6.6.8 [planned], R-6.6.9 [planned], R-6.6.10 [planned], R-6.6.12 [planned]

When >10 items share the same warning category, log 1 WARN summary with count and sample paths + individual paths at DEBUG. When <=10 items, log each as an individual WARN. This pattern is implemented in `recordSkippedItems()` for scanner-time validation failures. Transient retries at DEBUG, resolved at INFO, exhausted at WARN. Extends to execution-time transient failures: when >10 transient failures of the same `issue_type` exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG (R-6.6.12).

### Planned: Local Permission Handling

Implements: R-2.10.12 [planned], R-2.10.13 [planned]

`os.ErrPermission` → check parent directory accessibility. Inaccessible directory: one `local_permission_denied` at directory level, suppress operations under it. Accessible directory: file-level failure. Recheck directory-level issues at start of each sync pass.

### Planned: Observation Suppression

Implements: R-2.10.30 [planned], R-2.10.31 [planned]

During `throttle:account` or `service` scope block, suppress shortcut observation polling (wastes API calls). During `quota:shortcut:*` block, observation continues (read-only).

### Shortcut Integration (`engine_shortcuts.go`)

Detects shortcuts to shared folders in the delta stream. Creates additional delta scopes for shared folder observation. Handles shortcut removal (cleanup local copies).

## Orchestrator (`orchestrator.go`)

Multi-drive coordination. Runs one `DriveRunner` per configured, non-paused drive. Each drive gets its own goroutine, state DB, and sync engine instance. Engines do not coordinate scope blocks across engine boundaries — each engine discovers independently. Bounded waste accepted (one request per engine for 429). Implements: R-2.10.35 [planned]

Handles:
- Drive add/remove via SIGHUP config reload
- Pause/resume per drive
- Graceful shutdown (drain all drives)

## DriveRunner (`drive_runner.go`)

Per-drive lifecycle manager. Creates the engine, opens the state DB, and runs the sync loop. Handles drive-level errors and restart.

## CLI Sync Command (`sync.go`, `sync_helpers.go`)

Cobra command wiring. Sets up the orchestrator, handles `--watch`, `--download-only`, `--upload-only`, `--dry-run`, `--full`, `--drive` flags. Signal handling: first SIGINT = drain, second = force exit.

## Watch Mode Behavior

- SIGHUP → reload `config.toml`, apply drive changes immediately
- PID file with flock for single-instance enforcement
- Two-signal shutdown (drain, then force)
- Periodic full reconciliation (default 24h)

### Rationale

- **Idempotent planner = free crash recovery**: On restart after crash, delta re-observation produces the same actions. Items completed before crash are in baseline (EF1 no-ops). Items not completed get fresh actions. No persistent action queue needed.
- **Always use Orchestrator, even for single drive**: N=1 means one DriveRunner — same logic, no special case. Prevents "works for N=1 but breaks for N=2" class of bugs.
