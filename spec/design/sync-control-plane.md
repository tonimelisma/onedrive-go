# Sync Control Plane

GOVERNS: internal/multisync/*.go, internal/synccontrol/*.go, sync.go

Implements: R-2.8.1 [verified], R-2.8.2 [verified], R-2.8.3 [verified], R-2.9.1 [verified], R-2.9.2 [verified], R-2.9.3 [verified], R-3.4.2 [verified], R-6.3.3 [verified], R-6.3.4 [verified], R-6.6.15 [verified], R-6.6.16 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Overview

The control plane owns multi-mount sync lifecycle. It sits above the
single-mount engine in `internal/sync` and answers questions the engine should
not answer:

- which runtime mounts are active right now
- how those mounts are started and stopped
- how control-socket reload changes the active mount set
- how live control-socket requests are serialized through the running control loop
- how per-mount failures are isolated from one another

`sync.go` is the CLI entrypoint for this layer. `internal/multisync` is the
runtime package that implements it.

## Ownership Contract

- Owns: Multi-mount sync lifecycle, runtime mount-spec compilation, automatic shortcut reconciliation, per-mount engine startup/shutdown, reload diffing, control-socket ownership, and per-mount panic/error isolation.
- Does Not Own: Single-mount observation, planning, execution, retry/trial policy, or sync-store persistence semantics.
- Source of Truth: The current `config.Holder` snapshot, the CLI-compiled standalone mount configs, plus the runtime mount set and `runners` map owned by the watch-mode orchestrator loop.
- Allowed Side Effects: Session creation, engine construction/closure, Unix control-socket bind/unlink, per-mount goroutine startup, live perf capture, and control-plane logging.
- Mutable Runtime Owner: `RunWatch` owns the live `runners` map. Each `watchRunner` owns one cancel function and one completion channel for exactly one mount.
- Error Boundary: The control plane converts mount startup into structured per-mount startup outcomes, returns one-shot startup classification separately from completed `MountReport` values, and keeps watch-runner failures isolated to the affected mount or log path. Engine-internal errors remain inside the single-mount boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| `RunWatch` starts the runnable runtime mount set, skips incompatible-store mounts with immediate warnings, and rejects all-paused startup through the same startup-summary model. | `TestOrchestrator_RunWatch_SingleDrive`, `TestOrchestrator_RunWatch_MultiDrive`, `TestOrchestrator_RunWatch_SkipsIncompatibleStoreDriveWhenAnotherDriveStarts`, `TestOrchestrator_RunWatch_ReturnsErrorWhenAllDrivesPaused` |
| The Unix control socket is the single live-owner lock for one-shot and watch sync, reports owner mode/status, rejects unsupported one-shot control requests with typed `foreground_sync_running`, and keeps reload/stop serialized through the watch control loop. | `TestRunOnce_ControlSocketBlocksWatchOwner`, `TestOrchestrator_OneShotControlSocket_StatusAndRejectsNonStatus`, `TestOrchestrator_ControlSocket_StatusAndStop`, `TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners` |
| The control socket also exposes live perf snapshots and explicit capture bundles for both one-shot and watch owners without creating a second network surface or durable metrics store. | `TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture`, `TestOrchestrator_OneShotControlSocket_PerfCaptureRejectsInvalidDuration`, `internal/cli/perf_test.go` (`TestMainWithWriters_PerfCaptureJSON_ForOneShotOwner`, `TestMainWithWriters_PerfCaptureFailsWhenNoOwnerIsRunning`) |
| Socket files are permissioned private, stale sockets are removed only after a failed live probe, and empty hash-runtime socket directories are cleaned up on close. | `TestControlSocketServer_PermissionsStaleCleanupAndRuntimeDirRemoval` |
| Control-socket reload applies add/remove/pause/expired-pause diffs to the live runner set without bouncing unaffected mounts. | `TestOrchestrator_Reload_AddDrive`, `TestOrchestrator_Reload_RemoveMount`, `TestOrchestrator_Reload_PausedDrive`, `TestOrchestrator_Reload_TimedPauseExpiry` |

## Runtime Mount Specs

The control plane now compiles runtime `mountSpec` values before session
creation and engine construction.

Configured standalone drives are still the only explicit user-facing selection
surface, but they are no longer the runtime construction shape. The CLI resolves
configured drives and compiles them into `multisync.StandaloneMountConfig`
values before constructing the orchestrator. On startup and reload the control
plane now:

1. consumes the CLI-compiled standalone mount configs
2. compiles those standalone configs into runtime mounts
3. loads `mounts.json`
4. attaches valid managed child mounts beneath selected standalone parents
5. installs precise local subtree exclusions on those parents before engine
   construction

Managed child mounts are durable runtime facts, not synthetic follow-up drives
invented by the engine.

Each current `mountSpec` owns:

- stable runtime mount identity
- stable reporting identity and selection index
- local sync root and state DB path
- remote drive/root identity for rooted mounts
- token-owner identity and account email for sync session creation
- transfer/check/min-free-space tunables
- resolved pause state and rooted-observation hints
- parent-owned child subtree exclusions

`mountSpec` no longer carries `ResolvedDrive`, and `OrchestratorConfig` no
longer accepts resolved-drive values. Configured drives are compiled at the CLI
edge into `StandaloneMountConfig`, and sync session construction consumes those
facts through `driveops.MountSessionConfig`.

Engine construction is no longer drive-shaped. `internal/multisync` now derives
`sync.EngineMountConfig` from `mountSpec` and passes that sync-owned mount
config into `sync.NewMountEngine(...)`.

Managed child mounts currently inherit their token owner, pause state, and sync
tunables from the selected standalone parent mount. Conflicting content roots
are resolved in the control plane before engine startup: explicit standalone
mounts win over duplicate child projections, and duplicate child projections
for the same content root are skipped with structured startup outcomes.

Automatic shortcut reconciliation is also control-plane owned. Before one-shot
startup, before watch startup, on control-socket reload, and on the watch-mode
reconcile ticker, `internal/multisync` now:

- discovers shortcut placeholders under each selected standalone parent mount
- reconciles those bindings into `mounts.json`
- recompiles the runtime mount set
- starts or stops only the affected child mounts

Authoritative removal comes only from completed delta enumeration. Recursive
children enumeration remains positive-only: it can create or update child mount
records, but it never deletes them based on absence alone.

## Boundary To The Engine

The control plane does not observe, plan, execute, or persist sync state
itself. Those responsibilities remain in the single-mount engine.

- `internal/multisync` owns runtime mount selection, session resolution,
  derivation of sync-owned engine mount config, per-mount goroutines, reload,
  and shutdown.
- `internal/sync` owns one-shot execution, watch-mode runtime state, conflict
  execution, retry/trial logic, scope lifecycle, and reconciliation.

This split keeps the engine package focused on one mounted content root at a
time while allowing the CLI to run any number of mounts through one consistent control
surface.

## `Orchestrator`

`Orchestrator` is the multi-mount coordinator used by both one-shot `sync` and
watch-mode `sync --watch`.

It is always used, even for a single configured drive. There is no separate single-drive
CLI path, because special-casing `n=1` would create a second lifecycle model
for startup, shutdown, and reload.

### RunOnce

`RunOnce` compiles runtime mount specs, resolves sessions, builds one engine per
mount, and runs all mounts concurrently. Startup eligibility is classified per
mount first, including paused standalone parents, managed child mounts skipped because
their parent is missing or their content root conflicts. Runnable mounts then
produce one completed `MountReport` each, while startup-ineligible mounts
remain startup outcomes instead of synthetic completed reports. The control
plane never aborts the whole pass because one mount failed; partial failure is
isolated per mount. Both startup results and completed reports carry a stable
`SelectionIndex` matching the compiled runtime order so standalone parents and
their attached child mounts remain deterministic through orchestration,
rendering, and bookkeeping.

### RunWatch

`RunWatch` starts one watch-mode engine per runnable non-paused mount and then
owns the long-running control loop. It listens for:

- `ctx.Done()` for shutdown
- JSON-over-HTTP requests on the Unix control socket

Pause semantics come from `config.Drive.IsPaused()` and
`config.ClearExpiredPauses()` for configured parents plus managed child-mount
pause inheritance (`parent paused || child paused`). The control plane consumes
those rules; it does not redefine them inside the engine.

Existing state DBs that fail store compatibility checks are reported as
per-mount startup outcomes. Watch startup warns about those mounts
immediately, keeps healthy mounts running, and exits non-zero only when no
runnable mount starts. A paused-only selection is a structured startup refusal,
not a special string-only path.

### Control Socket

`RunOnce` and `RunWatch` both bind the configured Unix control socket before
starting engine work. This socket is the single process-owner lock: a live
socket means another sync owner is already running for the same data directory.
Stale socket files are removed only after a failed live dial proves no process
owns them.

The configured socket path normally lives under the app data directory. If that
absolute path would exceed the platform-safe Unix socket length, config derives
a stable hash-named runtime directory under the OS temp directory and stores
only the socket there; durable sync state remains in the drive state DB. If the
normal path and the hashed runtime fallback both exceed the Unix socket budget,
path derivation fails explicitly. `RunOnce` and `RunWatch` treat that as fatal
startup because the control socket is the single-owner lock.

Wire facts live in `internal/synccontrol`: endpoint constants, owner modes,
request/response structs, response statuses, and stable error codes. Server
lifecycle stays in `internal/multisync`; CLI transport stays in `internal/cli`.

The socket speaks JSON over HTTP:

- `GET /v1/status` returns the owner mode (`oneshot` or `watch`) and managed mounts.
- `GET /v1/perf` returns the owner mode plus the live aggregate and per-mount perf snapshots currently owned by the active sync runtime. This surface is live-only and returns whatever the owner has collected so far; it does not materialize historical perf state from SQLite.
- `POST /v1/perf/capture` triggers an explicit local capture bundle from the active owner. The request carries bounded duration plus optional output-dir, trace, and full-detail toggles; the response returns the local artifact paths for the completed bundle.
- `POST /v1/reload` reloads config in the watch owner.
- `POST /v1/stop` asks the watch owner to stop cleanly.

One-shot sync exposes status plus the direct perf endpoints above. Durable
control requests still return a busy response with
`code="foreground_sync_running"` because a foreground one-shot sync is already
the active owner. The CLI probes the owner boundary to decide whether live
control requests can be sent at all, but there is no parallel direct-DB
mutation path for sync decisions anymore.

Error responses have the shape `{status, code, message}`. Stable codes are
`invalid_request`, `foreground_sync_running`, `capture_unavailable`,
`capture_in_progress`, and `internal_error`.

### Reload

Control-socket reload does four things in order:

1. load config from disk
2. clear expired timed pauses
3. compile the new active runtime mount set
4. diff that set against running mounts

Removed or newly paused mounts are stopped and closed. Newly added or newly
resumed mounts are started when they are runnable; incompatible-store mounts
are warned and skipped without bouncing healthy runners. Already-running mounts
remain running. When a
timed pause has already expired by reload time, the config keys are cleaned up
but the running mount is not bounced.

## Runtime Ownership

The control plane has one mutable runtime structure in watch mode: the active
runner set.

- The `RunWatch` select loop is the single writer for the `runners` map.
- `startWatchRunner` creates one goroutine per running mount. That goroutine owns closing the runner's `done` channel exactly once on exit.
- The control command channel is internal to `RunWatch`; socket handlers send commands into that channel and wait for one response.
- The control plane itself owns no timers; reload, stop, and perf capture are event-driven through control-socket requests and context cancellation.
- The control plane owns live perf registration for active mounts, but not the
  counters themselves. `internal/perf` owns the aggregate/live snapshot state;
  the control plane only exposes that state through the local socket and
  forwards explicit capture requests into the owning runtime.

## `MountRunner`

`MountRunner` wraps a single mount's sync function with panic recovery and
error isolation. One mount panicking must become one isolated mount outcome,
not
a process-wide crash or a cross-mount failure cascade.

## CLI Contract

The `sync` Cobra command resolves drives, validates sync eligibility,
compiles selected drives into `multisync.StandaloneMountConfig`, constructs an
`Orchestrator`, and chooses between `RunOnce` and `RunWatch`.

- `--watch` selects daemon mode
- `--download-only` and `--upload-only` select sync mode
- `--dry-run` and `--full` apply only to one-shot mode
- first SIGINT/SIGTERM cancels the shared watch contexts and lets each mount's
  engine seal new admission and follow its normal shutdown path
- second signal forces exit

No timer escalates the first signal; forced exit is owned only by the second
signal.

The CLI command does not reach into per-mount engine internals. It only speaks
to the control-plane boundary.

## Design Constraints

- No mount-to-mount coordination state exists in memory or in SQLite.
- Each mount gets its own engine instance and state DB.
- Session creation goes through `driveops.SessionRuntime` with
  `driveops.MountSessionConfig`, keeping token-source caching owned in one
  place and keeping `ResolvedDrive` out of runtime construction.
- Reload updates config through one shared `config.Holder` and uses the
  CLI-supplied standalone-mount compiler, so both the control plane and session
  runtime see the same config snapshot without giving multisync authority over
  resolved-drive construction.

## Rationale

- **Separate control plane from runtime**: multi-mount lifecycle code changes
  for different reasons than single-mount execution logic. Keeping them in one
  package made both harder to reason about.
- **Always use the same top-level path**: one configured drive and many mounts share the
  same shutdown, reload, and panic-isolation semantics.
- **Per-mount isolation is explicit**: the control plane collects one report
  per mount and one panic cannot poison unrelated mounts.
