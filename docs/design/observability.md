# Observability Design

> **Status**: Design doc. Implementation target: Phase 12 (post-release).
> Referenced by roadmap increments 12.6 (RPC control socket), 12.8 (TUI),
> 12.9 (Prometheus metrics).

## 1. Problem Statement

When `onedrive-go sync --watch` runs as a daemon, the only way to know
what it's doing is to tail log output. The `status` command reads config
files and SQLite databases — it has zero awareness of the running daemon.

This creates five unanswerable questions:

1. **"Is it working?"** — Can't distinguish "idle because nothing changed"
   from "stuck in infinite retry loop."
2. **"Is it keeping up?"** — No visibility into throughput vs. change rate.
3. **"Is something wrong?"** — Errors happen silently. Token expiry, API
   throttling, and file failures are logged and discarded.
4. **"What is it doing right now?"** — No current phase, no in-flight
   transfer count, no progress.
5. **"How much has it done?"** — `SyncReport` is printed to stderr once per
   run and thrown away. No cumulative stats.

## 2. Current State

The codebase already computes most of the interesting data — it just
discards it immediately.

### Exists but never exposed to users

| Data | Location | Notes |
|------|----------|-------|
| `ObserverStats` (events, polls, errors, hashes) | `observer_remote.go` | `Stats()` called only in tests |
| `LastActivity()` on both observers | `observer_local.go`, `observer_remote.go` | Never read in production |
| `DroppedEvents()` on LocalObserver | `observer_local.go` | Logged once at shutdown |
| `failureTracker` (per-path retry counts) | `failure_tracker.go` | Internal only, no public accessor |

### Exists and partially exposed

| Data | Location | Exposure |
|------|----------|----------|
| `SyncReport` (9 action counts + outcomes) | `engine.go` | Printed to stderr, persisted to SQLite. Gone after run. |
| `WorkerPool.Stats()` (succeeded, failed) | `worker.go` | Flows into SyncReport, then discarded |
| Upload `ProgressFunc` | `graph/upload.go` | CLI `put` only — sync engine passes `nil` |
| PID file | `pidfile.go` | Duplicate-daemon prevention only |
| Last sync time, file count, conflicts | SQLite `sync_metadata` | Read by `status` command (offline) |

### Does not exist

| Data | Notes |
|------|-------|
| Bytes transferred (up/down) | No counting reader on transfers |
| Download progress callback | No mechanism at all |
| Graph API request/retry/429 counts | Individual retries logged, never counted |
| Current daemon state enum | No `idle`/`syncing`/`scanning`/`error` |
| In-flight transfer count | No gauge |
| Worker pool utilization | No `busy/total` metric |
| Goroutine count, heap, GC | Available via `runtime/metrics`, never collected |
| Per-drive daemon state | Orchestrator has no `States()` method |

## 3. What to Measure

### 3.1 Daemon Health (Question: "Is it working?")

| Metric | Type | Source |
|--------|------|--------|
| `daemon.uptime_seconds` | Gauge | `time.Since(startTime)` |
| `daemon.start_time` | Timestamp | Set once at daemon start |
| `daemon.state` | Enum | `idle` / `syncing` / `error` / `paused` |
| `daemon.pid` | Gauge | `os.Getpid()` |
| `daemon.goroutines` | Gauge | `runtime/metrics` `/sched/goroutines:goroutines` |
| `daemon.heap_bytes` | Gauge | `runtime/metrics` `/gc/heap/live:bytes` |
| `daemon.total_memory_bytes` | Gauge | `runtime/metrics` `/memory/classes/total:bytes` |
| `daemon.gc_cycles` | Counter | `runtime/metrics` `/gc/cycles/total:gc-cycles` |

### 3.2 Sync Throughput (Question: "Is it keeping up?")

| Metric | Type | Source |
|--------|------|--------|
| `sync.runs_completed` | Counter | Increment after each `RunOnce` |
| `sync.last_run_duration_seconds` | Gauge | `SyncReport.Duration` |
| `sync.last_run_time` | Timestamp | `time.Now()` after run |
| `sync.files_uploaded` | Counter | Cumulative from `SyncReport.Uploads` |
| `sync.files_downloaded` | Counter | Cumulative from `SyncReport.Downloads` |
| `sync.files_deleted` | Counter | Cumulative local + remote deletes |
| `sync.bytes_uploaded` | Counter | New: counting writer on upload body |
| `sync.bytes_downloaded` | Counter | New: counting reader on download body |
| `sync.events_dropped` | Counter | `LocalObserver.DroppedEvents()` |

### 3.3 Error Health (Question: "Is something wrong?")

| Metric | Type | Source |
|--------|------|--------|
| `sync.actions_failed` | Counter | Cumulative from `SyncReport.Failed` |
| `sync.conflicts_unresolved` | Gauge | Baseline query |
| `sync.failures_suppressed` | Gauge | `failureTracker` record count |
| `api.requests_total` | Counter | New: increment in `graph.Client.do()` |
| `api.retries_total` | Counter | New: increment in retry loop |
| `api.throttles_total` | Counter | New: increment on HTTP 429 |
| `api.errors_total` | Counter | New: increment on terminal failure |
| `api.server_errors_total` | Counter | New: increment on HTTP 5xx |

### 3.4 Current Activity (Question: "What is it doing right now?")

| Metric | Type | Source |
|--------|------|--------|
| `sync.phase` | Enum | `idle` / `observing` / `planning` / `executing` |
| `sync.workers_busy` | Gauge | New: atomic counter in worker dispatch/complete |
| `sync.workers_total` | Gauge | Worker pool size |
| `sync.transfers_in_flight` | Gauge | New: atomic counter on transfer start/end |
| `sync.actions_pending` | Gauge | `DepTracker` total minus completed |

### 3.5 Cumulative Totals (Question: "How much has it done?")

| Metric | Type | Source |
|--------|------|--------|
| `sync.total_files_synced` | Counter | Sum of uploads + downloads + deletes |
| `sync.total_bytes_transferred` | Counter | Sum of bytes up + bytes down |
| `sync.total_errors` | Counter | Sum of all failed actions |
| `sync.total_conflicts` | Counter | Cumulative conflicts detected |

### 3.6 Per-Drive Metrics

In multi-drive mode, all §3.2–3.5 metrics are tracked per drive (keyed by
`driveid.CanonicalID`) and aggregated for the daemon-level view. §3.1
(daemon health) and §3.3 API metrics are daemon-global.

## 4. Architecture

### 4.1 In-Process Metrics Registry

A single `MetricsRegistry` struct with `sync/atomic` fields. No external
dependencies. The struct is created at daemon start and passed to the
engine, orchestrator, and graph client.

```
┌─────────────────────────────────────┐
│         MetricsRegistry             │
│                                     │
│  daemon_*    (global gauges)        │
│  api_*       (global counters)      │
│  drives      map[CanonicalID]*DriveMetrics │
│    sync_*    (per-drive counters)   │
│    phase     (per-drive enum)       │
└──────────┬──────────────────────────┘
           │ read by
           ▼
    ┌──────────────┐
    │ Snapshot()   │──── JSON serialization
    └──────────────┘
```

`Snapshot()` returns a plain struct (no atomics) suitable for JSON
marshaling. Called by the status command and any export pathway.

The registry is ~30 atomic fields. No histograms, no distributions, no
label cardinality. Counters are `atomic.Int64`, gauges are `atomic.Int64`,
enums are `atomic.Int32` with typed constants, timestamps are
`atomic.Int64` storing `UnixNano`.

### 4.2 Instrumentation Points

Where existing code needs changes to feed the registry:

| Component | Change |
|-----------|--------|
| `graph.Client.do()` | Increment `api.requests_total`. On retry: `api.retries_total`. On 429: `api.throttles_total`. On 5xx: `api.server_errors_total`. |
| `graph.Client` download path | Wrap response body in counting reader → `bytes_downloaded` |
| `graph.Client` upload path | Wrap request body / count chunk callbacks → `bytes_uploaded` |
| `Engine.RunOnce()` | Set phase enum at each stage. After run: accumulate `SyncReport` fields into registry counters. |
| `WorkerPool` dispatch/complete | Increment/decrement `workers_busy` gauge |
| `Orchestrator` | Per-drive registry keyed by canonical ID |
| `failureTracker` | Expose `Len()` for `failures_suppressed` gauge |

### 4.3 Transport Options

How the `status` command (and external tools) read the registry.

#### Option A: Enhanced Status Command (No IPC)

The `status` command continues reading state DB for persistent data. The
daemon writes a snapshot to a JSON file periodically. Status reads both.

- **Daemon**: `MetricsRegistry.Snapshot()` → write to
  `$XDG_RUNTIME_DIR/onedrive-go/metrics.json` every 5 seconds via atomic
  rename.
- **Status**: Read `metrics.json` if it exists and timestamp < 30s old.
  Show live metrics. Fall back to state DB if file missing/stale.
- **Complexity**: ~50 lines. Zero new goroutines for IPC.
- **Limitation**: Up to 5s stale. Read-only. No future extensibility for
  daemon control.

#### Option B: Unix Domain Socket (Recommended)

The daemon opens a Unix socket and serves JSON over HTTP-over-UDS. The
status command connects to it when the daemon is running.

- **Daemon**: `net.Listen("unix", socketPath)` + `http.Serve`. Endpoints:
  - `GET /status` — full snapshot (application + runtime metrics)
  - `GET /health` — `{"ok": true}` (liveness probe)
- **Status**: Try connecting to socket. If connected, show live metrics.
  If not, fall back to state DB (existing behavior).
- **Complexity**: ~150 lines (listener + handlers + client).
- **Advantage**: Real-time. Extensible — future endpoints for daemon
  control, SSE event streaming, pprof.
- **Socket path**: `$XDG_RUNTIME_DIR/onedrive-go.sock` on Linux,
  `/tmp/onedrive-go-$(id -u).sock` on macOS (104-byte path limit).
- **Cleanup**: Remove stale socket at startup. Defer remove on shutdown.

#### Option C: Socket + Prometheus Exposition

Everything from Option B, plus a `/metrics` endpoint serving Prometheus
exposition format. Optionally, a configurable HTTP listener
(`metrics_listen = "localhost:9182"`) for Prometheus to scrape over TCP.

- **Without `prometheus/client_golang`**: Hand-write exposition format for
  ~30 metrics (trivial — it's just `metric_name value\n`).
- **With `prometheus/client_golang`**: Full histogram/summary support, but
  25 transitive dependencies including protobuf. Overkill.
- **Recommendation**: Hand-write the exposition format. No dependency.

#### Option D: OpenTelemetry SDK

Use OTel metrics SDK with `ManualReader` for in-process collection. Export
to stdout, OTLP, or any backend.

- **Dependencies**: 15 transitive (metrics-only).
- **Advantage**: Vendor-neutral, future-proof if users want Datadog/Grafana
  Cloud integration.
- **Disadvantage**: Complex API surface for ~30 counters. Nobody running a
  personal file sync tool has an OTLP collector.
- **Verdict**: Not justified unless the user base demands it.

### 4.4 Recommended Approach

**Option B (Unix socket)** for the transport layer. Reasons:

1. Zero external dependencies (stdlib only).
2. Real-time, on-demand — no polling, no staleness.
3. Connection success = liveness proof.
4. Foundation for 12.7 (live sync trigger) and 12.8 (TUI) — same socket,
   new endpoints.
5. Proven pattern: Docker, gopls, Syncthing, Tailscale.
6. The `status` command already has the offline fallback path (state DB).

Add Prometheus exposition format on `/metrics` (hand-written, no
dependency) for users who want monitoring integration.

## 5. Status Command Changes

### Current behavior (preserved as fallback)

```
Account: user@example.com
  Token:  valid (expires 2026-03-15)
  Drive:  personal:user@example.com
    State:     ready
    Sync dir:  /home/user/OneDrive
    Last sync: 2026-03-02 14:30:00 (3 files, 0 conflicts)
```

### Enhanced behavior (when daemon is running)

```
Account: user@example.com
  Token:  valid (expires 2026-03-15)
  Drive:  personal:user@example.com
    State:     syncing (executing — 3/8 workers busy)
    Sync dir:  /home/user/OneDrive
    Last sync: 12s ago (42 files, 1.2 MB uploaded)
    Uptime:    3d 14h 22m
    Run:       #847 (0 errors, 0 conflicts)
    API:       1,247 requests, 3 retries, 0 throttles
    Transfers: 847 files synced, 2.1 GB transferred
```

The live section appears only when the daemon socket is connectable. When
the daemon is not running, the output looks exactly like today plus a
`Daemon: not running` line.

### JSON output

`status --json` gains a `daemon` top-level key when the daemon is running:

```json
{
  "accounts": [...],
  "summary": {...},
  "daemon": {
    "running": true,
    "uptime_seconds": 309720,
    "goroutines": 24,
    "heap_bytes": 15728640,
    "drives": {
      "personal:user@example.com": {
        "state": "idle",
        "runs_completed": 847,
        "files_uploaded": 423,
        "files_downloaded": 312,
        "bytes_uploaded": 1258291200,
        "bytes_downloaded": 891289600,
        "actions_failed": 0,
        "last_run_duration_ms": 1234,
        "last_run_time": "2026-03-02T14:30:12Z"
      }
    },
    "api": {
      "requests_total": 12470,
      "retries_total": 31,
      "throttles_total": 2,
      "errors_total": 0
    }
  }
}
```

When the daemon is not running, `"daemon": {"running": false}`.

## 6. Implementation Plan

### Layer 1: Metrics Registry (no transport)

1. Define `MetricsRegistry` struct in `internal/sync/metrics.go`.
2. Define `DriveMetrics` sub-struct for per-drive counters.
3. `Snapshot()` method returns a plain struct for JSON.
4. Add `runtime/metrics` collection to snapshot.
5. Wire into `Engine`, `Orchestrator`, `WorkerPool` — increment counters
   at the instrumentation points listed in §4.2.
6. Wire into `graph.Client` — request/retry/throttle counters.
7. Add bytes-transferred counting (upload chunk callback, download body
   wrapper).

This layer is useful standalone — `SyncReport` can pull from the registry
instead of building its own counts.

### Layer 2: Unix Socket Transport

1. Socket listener in a new file (`internal/sync/rpc.go` or `daemon.go`).
2. `GET /status` handler calling `registry.Snapshot()`.
3. `GET /health` handler.
4. `GET /metrics` handler (Prometheus text format).
5. Socket lifecycle: create on daemon start, remove on shutdown, stale
   cleanup on startup.
6. Client helper for the `status` command to query the socket.

### Layer 3: Status Command Integration

1. `status.go` attempts socket connection before falling back to DB.
2. Merge live metrics into existing output format.
3. `--json` includes `daemon` key.
4. Detect daemon-running state from socket connectivity.

### Layer 4: Future Endpoints (not in this increment)

- `GET /events` — SSE stream for TUI (12.8)
- `POST /sync` — trigger immediate run (12.7)
- `GET /debug/pprof/*` — runtime profiling

## 7. Open Questions

1. **Where does the registry live?** Options: `internal/sync/` (near the
   engine), or a new `internal/metrics/` package. The graph client needs
   access too, which creates an import direction question.

2. **Should `graph.Client` accept a metrics interface?** The client is in
   `internal/graph/` which must not import `internal/sync/`. Options:
   (a) define a counter interface in `graph/`, (b) define it in a shared
   leaf package, (c) use a callback/hook pattern.

3. **Socket path conventions**: `$XDG_RUNTIME_DIR` on Linux, but macOS
   doesn't have it (falls back to `/tmp`). Should the socket path be
   configurable via config file?

4. **Multiple daemon instances**: If two daemons run with different configs,
   they need different socket paths. Key by config file path hash? PID?

5. **Transfer byte counting granularity**: Count at the HTTP body level
   (includes protocol overhead) or at the file content level (logical
   bytes)? HTTP level is simpler and more useful for bandwidth awareness.
