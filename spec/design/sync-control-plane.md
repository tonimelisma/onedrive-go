# Sync Control Plane

GOVERNS: internal/multisync/*.go, sync.go

Implements: R-2.8.1 [verified], R-2.8.3 [verified], R-3.4.2 [verified], R-6.3.4 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Overview

The control plane owns multi-drive sync lifecycle. It sits above the
single-drive engine in `internal/sync` and answers questions the engine should
not answer:

- which drives are active right now
- how those drives are started and stopped
- how SIGHUP reload changes the active drive set
- how per-drive failures are isolated from one another

`sync.go` is the CLI entrypoint for this layer. `internal/multisync` is the
runtime package that implements it.

## Ownership Contract

- Owns: Multi-drive sync lifecycle, drive-set resolution for sync, per-drive engine startup/shutdown, reload diffing, and per-drive panic/error isolation.
- Does Not Own: Single-drive observation, planning, execution, retry/trial policy, or sync-store persistence semantics.
- Source of Truth: The current `config.Holder` snapshot plus the `runners` map owned by the watch-mode orchestrator loop.
- Allowed Side Effects: Session creation, engine construction/closure, SIGHUP consumption, per-drive goroutine startup, and control-plane logging.
- Mutable Runtime Owner: `RunWatch` owns the live `runners` map. Each `watchRunner` owns one cancel function and one completion channel for exactly one drive.
- Error Boundary: The control plane converts drive startup, panic, and watch-runner failures into isolated `DriveReport` or log outcomes. Engine-internal errors remain inside the single-drive boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| `RunWatch` starts the configured drive set and keeps zero-drive watch mode valid without inventing a second startup path. | `TestOrchestrator_RunWatch_SingleDrive`, `TestOrchestrator_RunWatch_MultiDrive`, `TestOrchestrator_RunWatch_ZeroDrives` |
| SIGHUP reload applies add/remove/pause/expired-pause diffs to the live runner set without bouncing unaffected drives. | `TestOrchestrator_Reload_AddDrive`, `TestOrchestrator_Reload_RemoveDrive`, `TestOrchestrator_Reload_PausedDrive`, `TestOrchestrator_Reload_TimedPauseExpiry` |

## Boundary To The Engine

The control plane does not observe, plan, execute, or persist sync state
itself. Those responsibilities remain in the single-drive engine.

- `internal/multisync` owns drive selection, session resolution, engine
  construction, per-drive goroutines, reload, and shutdown.
- `internal/sync` owns one-shot execution, watch-mode runtime state, conflicts,
  retry/trial logic, scope lifecycle, and reconciliation.

This split keeps the engine package focused on one drive at a time while
allowing the CLI to run any number of drives through one consistent control
surface.

## `Orchestrator`

`Orchestrator` is the multi-drive coordinator used by both one-shot `sync` and
watch-mode `sync --watch`.

It is always used, even for a single drive. There is no separate single-drive
CLI path, because special-casing `n=1` would create a second lifecycle model
for startup, shutdown, and reload.

### RunOnce

`RunOnce` resolves sessions, builds one engine per configured drive, and runs
all drives concurrently. Each drive produces one `DriveReport`. The control
plane never aborts the whole pass because one drive failed; partial failure is
reported per drive.

### RunWatch

`RunWatch` starts one watch-mode engine per non-paused drive and then owns the
long-running control loop. It listens for:

- `ctx.Done()` for shutdown
- SIGHUP for config reload

Pause semantics come from `config.Drive.IsPaused()` and
`config.ClearExpiredPauses()`. The control plane consumes those rules; it does
not redefine them.

### Reload

SIGHUP reload does four things in order:

1. load config from disk
2. clear expired timed pauses
3. resolve the new active drive set
4. diff that set against running drives

Removed or newly paused drives are stopped and closed. Newly added or newly
resumed drives are started. Already-running drives remain running. When a
timed pause has already expired by reload time, the config keys are cleaned up
but the running drive is not bounced.

## Runtime Ownership

The control plane has one mutable runtime structure in watch mode: the active
runner set.

- The `RunWatch` select loop is the single writer for the `runners` map.
- `startWatchRunner` creates one goroutine per running drive. That goroutine owns closing the runner's `done` channel exactly once on exit.
- `SIGHUPChan` is an external read-only input. The control plane never closes it.
- The control plane itself owns no timers; reload and shutdown are event-driven through context cancellation and SIGHUP delivery.

## `DriveRunner`

`DriveRunner` wraps a single drive's sync function with panic recovery and
error isolation. One drive panicking must become one `DriveReport` error, not
a process-wide crash or a cross-drive failure cascade.

## CLI Contract

The `sync` Cobra command resolves drives, validates sync eligibility,
constructs an `Orchestrator`, and chooses between `RunOnce` and `RunWatch`.

- `--watch` selects daemon mode
- `--download-only` and `--upload-only` select sync mode
- `--dry-run` and `--full` apply only to one-shot mode
- first SIGINT/SIGTERM cancels the shared watch contexts and lets each drive's
  engine seal new admission and follow its normal shutdown path
- second signal forces exit

The CLI command does not reach into per-drive engine internals. It only speaks
to the control-plane boundary.

## Design Constraints

- No drive-to-drive coordination state exists in memory or in SQLite.
- Each drive gets its own engine instance and state DB.
- Session creation goes through `driveops.SessionProvider`, which remains the
  single owner of token-source caching.
- Reload updates config through one shared `config.Holder`, so both the
  control plane and session provider see the same config snapshot.

## Rationale

- **Separate control plane from runtime**: multi-drive lifecycle code changes
  for different reasons than single-drive execution logic. Keeping them in one
  package made both harder to reason about.
- **Always use the same top-level path**: one drive and many drives share the
  same shutdown, reload, and panic-isolation semantics.
- **Per-drive isolation is explicit**: the control plane collects one report
  per drive and one panic cannot poison unrelated drives.
