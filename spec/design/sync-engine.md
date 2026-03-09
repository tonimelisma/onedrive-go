# Sync Engine

GOVERNS: internal/sync/engine.go, internal/sync/engine_shortcuts.go, internal/sync/orchestrator.go, internal/sync/drive_runner.go, sync.go, sync_helpers.go

Implements: R-2.1 [implemented], R-2.6 [implemented], R-2.8 [implemented], R-3.4.2 [implemented]

## Engine (`engine.go`)

Wires the sync pipeline: observers → buffer → planner → executor → SyncStore. Two entry points:

- `RunOnce()`: one-shot sync. Observes all changes, plans, executes, returns `SyncReport`.
- `RunWatch()`: continuous sync. Runs `RunOnce` in a loop, triggered by filesystem events and delta polling.

Watch mode uses a unified tick loop: filesystem events are debounced by the change buffer, remote changes are polled at `poll_interval` (default 5 minutes). Periodic full reconciliation runs every 24 hours to detect missed delta deletions.

### Shortcut Integration (`engine_shortcuts.go`)

Detects shortcuts to shared folders in the delta stream. Creates additional delta scopes for shared folder observation. Handles shortcut removal (cleanup local copies).

## Orchestrator (`orchestrator.go`)

Multi-drive coordination. Runs one `DriveRunner` per configured, non-paused drive. Each drive gets its own goroutine, state DB, and sync engine instance. Handles:
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
