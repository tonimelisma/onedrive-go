# Sync Engine

GOVERNS: internal/sync/engine.go, internal/sync/engine_shortcuts.go, internal/sync/orchestrator.go, internal/sync/drive_runner.go, sync.go, sync_helpers.go

Implements: R-2.1 [verified], R-2.6 [verified], R-2.8 [verified], R-3.4.2 [verified], R-2.10.1 [planned], R-2.10.2 [planned], R-2.10.3 [planned], R-2.10.4 [planned], R-2.10.7 [planned], R-2.10.9 [planned], R-2.10.10 [planned], R-2.10.12 [planned], R-2.10.13 [planned], R-2.10.17 [planned], R-2.10.18 [planned], R-2.10.19 [planned], R-2.10.20 [planned], R-2.10.23 [planned], R-2.10.24 [planned], R-2.10.25 [planned], R-2.10.26 [planned], R-2.10.28 [planned], R-2.10.29 [planned], R-2.10.30 [planned], R-2.10.31 [planned], R-2.10.35 [planned], R-2.10.36 [planned], R-2.10.37 [planned], R-2.10.38 [planned], R-6.6.7 [planned], R-6.6.8 [planned], R-6.6.9 [planned], R-6.6.10 [planned], R-6.6.12 [planned], R-6.7.27 [planned], R-6.8.15 [planned]

## Engine (`engine.go`)

Wires the sync pipeline: observers → buffer → planner → executor → SyncStore. Two entry points:

- `RunOnce()`: one-shot sync. Observes all changes, plans, executes, returns `SyncReport`.
- `RunWatch()`: continuous sync. Runs `RunOnce` in a loop, triggered by filesystem events and delta polling.

Watch mode uses a unified tick loop: filesystem events are debounced by the change buffer, remote changes are polled at `poll_interval` (default 5 minutes). Periodic full reconciliation runs every 24 hours to detect missed delta deletions.

### Planned: Error Classification (`classifyResult()`)

Implements: R-6.8.9 [planned], R-6.8.15 [planned], R-6.7.27 [planned]

Single classification point for all worker results. Maps HTTP status codes + error types → result class (success, transient, actionable, fatal). Replaces executor's `classifyError`/`classifyStatusCode`.

Target-drive-aware scope routing: `WorkerResult.TargetDriveID` and `ShortcutKey` determine scope key. Own-drive actions (empty `ShortcutKey`) route to `quota:own` or `perm:remote:{path}`. Shortcut actions route to `quota:shortcut:$drive:$item` or `perm:remote:{path}`. 429/5xx always route to account/service scope regardless of target drive. Empty `TargetDriveID` (local-only errors) skips remote scope routing entirely.

Transient classification (R-6.8.15): 5xx → `server_error`, 408 → `request_timeout`, 412 → `transient_conflict`, 404 → `transient_not_found`, 423 → `resource_locked`. 423 reclassified from skip to transient — non-blocking tracker re-queue handles multi-hour SharePoint locks naturally.

### Planned: Scope Detection and Management (`updateScope()`)

Implements: R-2.10.3 [planned], R-2.10.17 [planned], R-2.10.18 [planned], R-2.10.19 [planned], R-2.10.20 [planned], R-2.10.23 [planned], R-2.10.26 [planned], R-2.10.28 [planned], R-2.10.29 [planned]

Target-drive-aware scope routing:
- 507/403 → per-drive scope key (`quota:own`, `quota:shortcut:$drive:$item`, `perm:remote:{path}`)
- 429 → `throttle:account` (all drives share the same OAuth token)
- 5xx → `service` scope (shared infrastructure)
- Empty `TargetDriveID` (local-only errors like `os.ErrPermission`) → skip remote scope routing

Sliding window detection: N unique-path failures with no intervening success within T seconds. Success from any path in scope resets the counter.

### Planned: ScopeState

Implements: R-2.10.35 [planned], R-2.10.36 [planned], R-2.10.37 [planned]

In-memory data structure: blocks map (scope_key → `ScopeBlock`), sliding windows (scope_key → window), trial timers. Engine-internal — no cross-engine coordination (each engine discovers independently).

### Planned: Scanner ScanResult Contract

Implements: R-2.11.5 [planned], R-2.10.2 [planned]

Scanner returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. Engine processes skipped items via `recordSkippedItems()` (batch-upserts to `sync_failures` as actionable) and `clearResolvedActionableFailures()` (deletes entries for files no longer skipped).

### Planned: Aggregated Logging

Implements: R-6.6.7 [planned], R-6.6.8 [planned], R-6.6.9 [planned], R-6.6.10 [planned], R-6.6.12 [planned]

When >10 items share the same warning category, log 1 WARN summary + individual DEBUG. Transient retries at DEBUG, resolved at INFO, exhausted at WARN. Extends to execution-time transient failures: when >10 transient failures of the same `issue_type` exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG (R-6.6.12).

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
