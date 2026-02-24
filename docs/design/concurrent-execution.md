# Execution Architecture

This document is the definitive specification for the execution layer of
onedrive-go's sync engine. It covers how planned actions are scheduled,
dispatched to workers, committed to the database, and recovered after crashes.

The execution architecture uses a two-layer design: a **persistent ledger**
(SQLite `action_queue` table) provides durability, crash recovery, and audit
trail; an **in-memory dependency tracker** provides instant dispatch, lane-based
fairness, and action cancellation. The ledger stores the full queue on disk.
The tracker holds a bounded working window in memory.

---

## 1. Design Requirements

### 1.1 Tier 1 --- Safety and Correctness (Non-Negotiable)

1. **Data safety (S1-S7).** No data loss, ever. Hash-before-delete (S4), atomic
   file writes via `.partial` + rename (S3), big-delete protection (S5), no
   partial uploads reaching remote (S7), never delete remote without synced base
   (S1), never process deletions from incomplete enumeration (S2), disk space
   check before downloads (S6).
2. **Crash recovery.** Process can die at any point. Restart resumes without
   duplicating or losing work.
3. **Ordering correctness.** Parents before children, children before parent
   deletes, no races between dependent actions.
4. **Delta token consistency.** Token advances only when all outcomes from that
   delta are durable in baseline.

### 1.2 Tier 2 --- Performance and Responsiveness

5. **Maximum parallelism.** Independent actions run concurrently. No artificial
   barriers between action types.
6. **Watch mode compatibility.** Observers run continuously, transfers drain
   independently, new changes detected while transfers are in-flight. Three
   concurrent subsystems: observers, transfer queue, baseline commits.
7. **Incremental progress.** Completed transfers are durable immediately, not
   held hostage by incomplete ones.
8. **Idle efficiency.** < 1% CPU when nothing is changing. < 100 MB for 100K
   synced files (PRD section 20).
9. **Worker fairness.** N large files must not monopolize all workers. A flood
   of small changes must not starve large transfers. Both must make progress
   simultaneously.
10. **Bandwidth control.** Per-direction limits (upload, download), combined
    limits, and scheduled throttling (full speed at night, throttled during work
    hours).
11. **Adaptive concurrency.** Scale worker count up when many small files queue
    (throughput-bound), down when few large files queue (bandwidth-bound), and
    back off when hitting 429 throttle responses.
12. **Graceful shutdown.** Ctrl-C stops cleanly with no lost progress. In-flight
    actions must be recoverable on restart.
13. **Action cancellation.** If a file changes while its upload is in-flight,
    cancel the stale upload and start a new one.
14. **Backpressure.** If the planner produces actions faster than workers
    execute (massive initial sync), the system must not accumulate unbounded
    memory.
15. **Progress reporting.** Users see real-time status: "downloading 3 files
    (2.1 GB), uploading 5 files (340 MB), 12 waiting." Not just at cycle end.

### 1.3 Tier 3 --- Engineering Quality

16. **Testability.** Scheduler, dependency resolver, and commit logic must be
    unit-testable without I/O or network.
17. **Debuggability.** Inspect the state of every action, see what's blocked
    and why, replay failures. State should survive process exit for post-mortem
    analysis.
18. **Extensibility.** Straightforward to add priority scheduling, pause/resume,
    progress bars, upload session resume.
19. **Invariant simplicity.** Fewer simultaneous invariants = safer. The system
    must be correct, but the proof of correctness should be as simple as
    possible.

### 1.4 Architectural Constraints

These are existing architectural decisions that the execution layer respects:

- **Event-driven pipeline.** Observers produce events (no DB writes), planner
  is pure function (no I/O, no DB), executor produces outcomes (no DB writes),
  BaselineManager is sole writer.
- **Baseline is the only durable per-item state.** Remote and local
  observations are ephemeral --- rebuilt from the API and filesystem each cycle.
- **Planner must remain a pure function.** Signature:
  `([]PathChanges, *Baseline, SyncMode, SafetyConfig) -> *ActionPlan`.
  No I/O, no database access, deterministic.
- **Each drive is independent.** Own engine, own goroutine, own state DB. No
  cross-drive coordination needed.
- **Crash recovery is idempotent.** Transfers that complete but aren't committed
  to baseline are safe: on next cycle, the planner sees them as convergent
  (EF4/EF11) and produces an update-synced action.
- **Delta token committed only after all cycle actions complete.** Prevents
  token-advancement-without-execution crash bug (decision E3).
- **Ephemeral execution context per operation.** Immutable `ExecutorConfig` +
  ephemeral `Executor` via `NewExecution()`. No temporal coupling.
- **Upload lifecycle encapsulated in `graph.Client.Upload()`.** Consumers call
  one method; the provider handles simple-vs-chunked routing, session lifecycle,
  and cleanup.
- **`io.ReaderAt` for retry-safe uploads.** `SectionReader`s from the same file
  handle make retries safe without re-opening the file.
- **Cache-through baseline loading.** `BaselineManager.Load()` returns cached
  baseline. `CommitOutcome()` incrementally patches the in-memory cache via
  `updateBaselineCache()`.

### 1.5 PRD Requirements Traceability

| PRD Section | Requirement | How Addressed |
|-------------|-------------|---------------|
| PRD SS7 | All sync modes (bidirectional, download-only, upload-only, one-shot, `--watch`) | Section 15 --- execution mode walkthroughs |
| PRD SS7 | Watch mode: inotify/FSEvents local, polling/WebSocket remote, 5-min fallback | Section 15.4 --- observers never stop |
| PRD SS7 | `--watch` combines with direction flags (e.g., `sync --watch --download-only`) | Section 15.4 --- direction flags skip irrelevant observer |
| PRD SS7 | Pause/resume: process stays alive, events collected, no transfers, efficient resume | Section 15.7 --- pause/resume walkthrough |
| PRD SS10 | Parallel transfers (configurable: `parallel_downloads`, `parallel_uploads`, `parallel_checkers`) | Section 6 --- lane-based workers with config mapping |
| PRD SS10 | Resumable transfers (upload sessions, Range headers, state persisted to disk) | Section 14 --- graceful shutdown, ledger `session_url` |
| PRD SS10 | Bandwidth limiting with time-of-day scheduling | Section 7 --- token-bucket rate limiter |
| PRD SS10 | Upload thresholds (<=4MB simple PUT, >4MB resumable, chunk size configurable) | Unchanged --- handled in `graph.Client.Upload()` |
| PRD SS11 | Big-delete protection, dry-run, recycle bin, disk space reservation, crash recovery | Section 15.5 (dry-run), section 14 (crash recovery), safety in planner |
| PRD SS13 | Service integration (systemd/launchd) --- `sync --watch` as long-running service | Section 15.4 --- continuous pipeline |
| PRD SS20 | <100MB for 100K files, <1% CPU idle, <10min initial sync 10K files, zero data loss crash recovery, automatic 429 backoff, graceful network resume | Sections 8, 10, 11, 14 |

---

## 2. Architecture Overview

### 2.1 Two-Layer Design

The execution layer consists of two complementary components:

| Component | Storage | Purpose | Key Properties |
|-----------|---------|---------|---------------|
| **Persistent ledger** | SQLite `action_queue` table | Durability, crash recovery, audit trail, upload session URLs | Survives process exit, unlimited capacity, queryable |
| **Dependency tracker** | In-memory data structure | Scheduling, dispatch, lane fairness, cancellation | Zero latency, bounded memory, channel-based |

The ledger is the source of truth for what work exists. The tracker is a cached,
bounded working window that provides instant dispatch when dependencies are
satisfied.

### 2.2 Component Diagram

```
Planner --> ActionPlan with dependency DAG
               |
               v
     +--------------------+       +-----------------------+
     | Persistent Ledger  |<----->| Dependency Tracker    |
     | (SQLite action_    |       | (in-memory, bounded)  |
     |  queue table)      |       |                       |
     |                    |       | ready channels:       |
     | * Full action list |       |   interactive []      |
     | * session_url      |       |   bulk        []      |
     | * bytes_done       |       |                       |
     | * crash recovery   |       | * lane dispatch       |
     +--------------------+       | * cancellation        |
               ^                  | * dep counting        |
               |                  +----------+------------+
               |                             |
               |              +--------------+--------------+
               |              v              v              v
               |          Worker 1       Worker 2       Worker N
               |          (interactive)  (bulk)         (shared)
               |              |              |              |
               |              +--------------+--------------+
               |                             |
               +-------- per-action ---------+
                          commit
                     (baseline upsert +
                      ledger status update
                      in same SQLite tx)
```

### 2.3 Startup Sequence

1. Load baseline from database (`BaselineManager.Load()`)
2. Load pending/claimed actions from ledger into dependency tracker
3. Reclaim stale claims (actions stuck in `claimed` status past timeout)
4. Start workers (interactive lane, bulk lane, shared pool)
5. Start observers (remote, local)
6. Workers begin draining tracker immediately (resume after crash)

### 2.4 Execution Flow

1. Observers detect changes, emit `ChangeEvent` values to the change buffer
2. Buffer debounces (2s) and flushes `[]PathChanges`
3. Planner produces `ActionPlan` with dependency DAG (pure function)
4. Actions written to persistent ledger (status=pending)
5. Actions loaded into dependency tracker (if capacity available)
6. Tracker dispatches ready actions to worker lanes via channels
7. Workers execute actions (same per-action logic: downloads, uploads, deletes, moves)
8. On completion: per-action atomic commit (baseline upsert + ledger status update)
9. Tracker notified: `Complete(actionID)` decrements dependent counters, dispatches newly ready actions
10. When all actions for a cycle complete: commit delta token

---

## 3. Persistent Ledger

### 3.1 Purpose

The ledger provides:

- **Crash recovery**: On restart, read the ledger to know exactly what was
  pending, what was in-flight, and what completed. No full re-observation needed.
- **Upload session resume**: The `session_url` column stores the pre-authenticated
  upload URL. On restart, continue from `bytes_done` offset.
- **Download resume**: Workers can use HTTP `Range` headers to resume from
  `.partial` file size.
- **Audit trail**: `SELECT * FROM action_queue WHERE status != 'done'` shows
  all pending work. Post-mortem analysis with standard SQLite tools.
- **Backpressure**: Unlimited disk capacity absorbs planner output without
  memory pressure.

### 3.2 Schema

```sql
CREATE TABLE action_queue (
    id           INTEGER PRIMARY KEY,
    cycle_id     TEXT    NOT NULL,                -- groups actions from one planning pass
    action_type  TEXT    NOT NULL CHECK(action_type IN (
                     'download', 'upload', 'local_delete', 'remote_delete',
                     'local_move', 'remote_move', 'folder_create',
                     'conflict', 'update_synced', 'cleanup'
                 )),
    path         TEXT    NOT NULL,                -- target path (relative to sync root)
    old_path     TEXT,                            -- for moves: source path
    status       TEXT    NOT NULL DEFAULT 'pending'
                         CHECK(status IN ('pending', 'claimed', 'done', 'failed', 'canceled')),
    depends_on   TEXT,                            -- JSON array of action IDs
    drive_id     TEXT,                            -- normalized drive ID
    item_id      TEXT,                            -- server-assigned item ID
    parent_id    TEXT,                            -- server parent ID
    hash         TEXT,                            -- expected content hash
    size         INTEGER,                         -- file size in bytes
    mtime        INTEGER,                         -- expected mtime (Unix nanoseconds)
    session_url  TEXT,                            -- upload session URL for resume
    bytes_done   INTEGER NOT NULL DEFAULT 0,      -- transfer progress (bytes)
    claimed_at   INTEGER,                         -- when a worker claimed this action
    completed_at INTEGER,                         -- when the action finished
    error_msg    TEXT                             -- error description for failed actions
);

CREATE INDEX idx_action_queue_status ON action_queue(status);
CREATE INDEX idx_action_queue_cycle ON action_queue(cycle_id);
CREATE INDEX idx_action_queue_path ON action_queue(path);
```

### 3.3 Column Descriptions

| Column | Purpose |
|--------|---------|
| `id` | Auto-increment primary key, also used as dependency reference |
| `cycle_id` | UUID grouping actions from one planning pass. Delta token is committed when all actions for a cycle_id reach `done`. |
| `action_type` | Maps to `ActionType` enum. Determines which per-action execution function runs. |
| `path` | Target path for the action. For moves, this is the destination. |
| `old_path` | Source path for moves. NULL for non-move actions. |
| `status` | Lifecycle state (see section 3.4). |
| `depends_on` | JSON array of action IDs that must complete before this action can execute. NULL or `[]` means no dependencies. |
| `drive_id` | Normalized drive ID for API operations. |
| `item_id` | Server-assigned item ID. NULL for new uploads (assigned after completion). |
| `parent_id` | Server parent folder ID. Used by folder creates and uploads. |
| `hash` | Expected QuickXorHash (base64). For downloads: expected remote hash. For uploads: local file hash. |
| `size` | File size in bytes. Used for lane routing (interactive vs bulk). |
| `mtime` | Local modification time at action creation (Unix nanoseconds). |
| `session_url` | Pre-authenticated upload session URL. Set when chunked upload session is created. Used for crash recovery resume. |
| `bytes_done` | Bytes transferred so far. Updated periodically during transfers. Used for progress reporting and resume. |
| `claimed_at` | Timestamp when a worker started this action. Used for stale claim detection. |
| `completed_at` | Timestamp when the action finished (success or failure). |
| `error_msg` | Human-readable error for failed actions. NULL on success. |

### 3.4 Status Lifecycle

```
pending  --[worker claims]--> claimed  --[success]--> done
                                |
                                +------[failure]--> failed
                                |
                                +------[canceled]--> canceled

On restart:
  claimed (stale) --[reclaim after timeout]--> pending
  failed --[retry logic]--> pending (or stays failed after max retries)
```

- **pending**: Action is in the ledger, waiting for dependencies to be satisfied
  and a worker to claim it.
- **claimed**: A worker is actively executing this action. `claimed_at` records
  when execution started.
- **done**: Action completed successfully. Baseline updated. Action stays in
  ledger for audit trail (compacted periodically).
- **failed**: Action failed after all retries. Error recorded in `error_msg`.
  May be retried in a future cycle.
- **canceled**: Action was superseded (e.g., file changed while upload was
  in-flight). Not retried.

### 3.5 Crash Recovery Semantics

On restart, the ledger is the source of truth:

1. **`done` actions**: Already committed to baseline. No action needed.
2. **`pending` actions**: Not yet started. Load into tracker and execute.
3. **`claimed` actions**: Were in-flight when process died. Reclaim to `pending`
   after stale timeout (default: 30 minutes). For uploads with `session_url`:
   query the upload session endpoint to determine resume point. For downloads:
   check `.partial` file size for resume via `Range` header.
4. **`failed` actions**: Logged for debugging. May be retried in a future cycle.
5. **`canceled` actions**: Ignored. Already superseded.

---

## 4. Dependency Tracker

### 4.1 Purpose

The tracker provides:

- **Zero-latency dispatch**: When action X completes and unblocks action Y,
  Y is dispatched to the ready channel immediately (in the same `Complete()`
  call). No polling.
- **Lane-based fairness**: Ready actions are routed to interactive or bulk
  channels based on file size and action type.
- **Action cancellation**: Each in-flight action has a `context.CancelFunc`.
  Cancellation is instant --- no DB round-trip.
- **Progress snapshots**: `tracker.Progress()` returns live counts and byte
  totals with a single mutex acquisition.

### 4.2 Data Structures

```go
type DepTracker struct {
    mu          sync.Mutex
    actions     map[ActionID]*trackedAction
    byPath      map[string]*trackedAction      // for cancellation by path
    interactive chan *trackedAction             // small files, folder ops, deletes
    bulk        chan *trackedAction             // large file transfers
    capacity    chan struct{}                   // signals refill loop when space opens
}

type trackedAction struct {
    action     Action
    ledgerID   int64                           // references action_queue.id
    cancel     context.CancelFunc              // set when worker claims the action
    deps       []ActionID
    depsLeft   int32                           // atomic counter
    status     actionStatus                    // pending, claimed, done
    dependents []*trackedAction                // actions waiting on this one
}
```

### 4.3 Operations

**Add(action, deps)**: Insert an action into the tracker. If `depsLeft == 0`,
dispatch to the appropriate ready channel immediately. Otherwise, register as
a dependent of each dependency.

**Complete(id)**: Mark action done. For each dependent action, atomically
decrement `depsLeft`. If any dependent reaches zero, dispatch it to the ready
channel. Signal the capacity channel to trigger a refill from the ledger.

**Cancel(path)**: Look up by path. If the action is claimed (in-flight), call
its `context.CancelFunc` to cancel the worker's context immediately. Update
status to canceled in both tracker and ledger.

### 4.4 Ready Channel Dispatch

When an action becomes ready (all dependencies satisfied), the tracker routes
it to the appropriate lane channel:

```go
func (dt *DepTracker) dispatch(ta *trackedAction) {
    if ta.action.Size < sizeThreshold || !ta.action.IsTransfer() {
        dt.interactive <- ta
    } else {
        dt.bulk <- ta
    }
}
```

Workers pull from their assigned channel. See section 6 for the lane model.

---

## 5. Dependency Model

### 5.1 Per-Path Dependency Edges

The planner emits explicit dependency edges in the `ActionPlan`. The dependency
information (folder creates before files, depth-first for deletes) is expressed
as edges in the action DAG rather than implicit ordering assumptions.

There are three types of dependency edges:

1. **Parent-before-child**: A child operation depends on its parent folder
   existing. `upload /A/B/f.txt` depends on `mkdir /A/B` (if `/A/B` is being
   created in this cycle).
2. **Children-before-parent-delete**: A parent folder deletion depends on all
   child deletions completing first. `rmdir /A` depends on
   `delete /A/file1.txt` and `delete /A/file2.txt`.
3. **Move target parent**: A move operation depends on the target parent folder
   existing. `move /X/a.txt -> /Y/a.txt` depends on `mkdir /Y` (if `/Y` is
   being created in this cycle).

### 5.2 DAG Construction by Planner

The planner constructs the dependency DAG during plan generation:

```
mkdir /A         -> []                  (no deps --- root or parent in baseline)
mkdir /A/B       -> [mkdir /A]          (parent created in this cycle)
upload /A/B/f.tx -> [mkdir /A/B]        (parent created in this cycle)
download /C/x.pd -> []                  (parent /C/ already in baseline)
delete /old/y.tx -> []                  (independent)
rmdir /old       -> [delete /old/y.txt] (children first)
move /X -> /A/Z  -> [mkdir /A]          (target parent created in this cycle)
```

Actions whose parent folders already exist in the baseline have no
dependencies and are immediately ready for execution.

### 5.3 Concrete Examples

**Upload to new folder tree**:
```
mkdir /Photos              deps: []
mkdir /Photos/2024         deps: [mkdir /Photos]
upload /Photos/2024/a.jpg  deps: [mkdir /Photos/2024]
upload /Photos/2024/b.jpg  deps: [mkdir /Photos/2024]
```
Both uploads become ready simultaneously when `mkdir /Photos/2024` completes.
They execute in parallel.

**Delete folder tree**:
```
delete /Old/sub/file1.txt  deps: []
delete /Old/sub/file2.txt  deps: []
rmdir /Old/sub             deps: [delete /Old/sub/file1.txt, delete /Old/sub/file2.txt]
rmdir /Old                 deps: [rmdir /Old/sub]
```
Both file deletes run in parallel. Folder deletes cascade bottom-up.

**Mixed operations (download + delete + upload)**:
```
download /A/new.txt        deps: []           (parent exists)
delete /B/old.txt          deps: []           (independent)
upload /C/edited.txt       deps: []           (parent exists)
mkdir /D                   deps: []           (parent exists)
upload /D/report.pdf       deps: [mkdir /D]   (parent created in this cycle)
```
Four actions start immediately in parallel. The fifth waits only for `mkdir /D`.

---

## 6. Worker Lanes and Fairness

### 6.1 Lane Model

Workers are organized into two lanes with reserved capacity plus a shared
overflow pool:

```
Total workers: N (configurable, default 16)

Lane: interactive (files < 10 MB, folder ops, deletes, moves, conflicts)
  Reserved workers: 2 minimum (always available for small ops)

Lane: bulk (files >= 10 MB)
  Reserved workers: 2 minimum (always available for large transfers)

Shared pool: N-4 workers
  Assigned dynamically to whichever lane has work
  Interactive lane has priority for shared workers
```

### 6.2 Fairness Guarantees

- If all N workers are doing 10 GB uploads, 2 workers are reserved for the
  interactive lane. A small file change gets picked up immediately.
- If a trillion small files flood the interactive lane, 2 workers are reserved
  for the bulk lane. Large transfers keep making progress.
- When one lane is empty, its reserved workers plus all shared workers serve
  the other lane.

### 6.3 Worker Implementation

```go
// Reserved interactive workers
for i := 0; i < reservedInteractive; i++ {
    go worker(tracker.interactive)
}

// Reserved bulk workers
for i := 0; i < reservedBulk; i++ {
    go worker(tracker.bulk)
}

// Shared workers: prefer interactive, fall back to bulk
for i := 0; i < shared; i++ {
    go func() {
        for {
            select {
            case action := <-tracker.interactive:
                execute(action)
            default:
                select {
                case action := <-tracker.interactive:
                    execute(action)
                case action := <-tracker.bulk:
                    execute(action)
                }
            }
        }
    }()
}
```

### 6.4 Config Mapping

The PRD specifies three separate configuration keys: `parallel_downloads`,
`parallel_uploads`, and `parallel_checkers`. The lane-based model unifies
downloads and uploads into interactive/bulk lanes. The configuration mapping:

| PRD Config Key | Default | Lane Mapping |
|----------------|---------|-------------|
| `parallel_downloads` | 8 | Contributes to total worker count |
| `parallel_uploads` | 8 | Contributes to total worker count |
| `parallel_checkers` | 8 | Separate checker pool (unchanged --- not in lanes) |

Total lane workers = `parallel_downloads + parallel_uploads` (default 16).
The checker pool remains separate because hash computation is CPU-bound, not
I/O-bound, and runs during observation, not execution.

The interactive/bulk split and reserved worker counts are derived from the total:
- Reserved interactive: `max(2, total / 8)`
- Reserved bulk: `max(2, total / 8)`
- Shared: `total - reserved_interactive - reserved_bulk`

The size threshold between interactive and bulk lanes is 10 MB (fixed). This
provides predictable behavior without requiring tuning.

---

## 7. Bandwidth Limiting

Transport-layer concern, orthogonal to the execution architecture. A standard
token-bucket rate limiter wraps the HTTP transport:

```go
type throttledTransport struct {
    base       http.RoundTripper
    uploadBw   *rate.Limiter  // e.g., 10 MB/s
    downloadBw *rate.Limiter  // e.g., 50 MB/s
}
```

Each `Read()` or `Write()` on the HTTP body acquires tokens from the limiter.
Workers naturally share the bandwidth budget --- 8 workers doing downloads
share the download bandwidth limit.

**Scheduled throttling**: A background goroutine adjusts the limiter's rate
based on time-of-day rules (configurable in TOML). Example: full speed from
midnight to 6 AM, throttled to 10 MB/s during work hours.

**Per-direction and combined limits**: Separate limiters for upload and download
bandwidth. An optional combined limiter caps total throughput when both
directions are active.

---

## 8. Adaptive Concurrency

AIMD (additive increase, multiplicative decrease) applied to worker concurrency,
similar to TCP congestion control:

| Signal | Response |
|--------|----------|
| High 429 rate from Graph API | Multiplicative decrease: halve active workers |
| Low error rate + high throughput | Additive increase: add one worker |
| Many small files in queue | Increase workers (parallelism helps throughput) |
| Few large files in queue | Decrease workers (bandwidth contention hurts) |
| High latency per action | Hold steady or decrease (network saturated) |

The controller adjusts a semaphore that gates worker dispatch from the
tracker's ready channels:

```go
type AdaptiveLimiter struct {
    current    int32
    min, max   int
    errorRate  float64
    throughput float64
}

func (al *AdaptiveLimiter) Adjust() {
    if al.errorRate > throttleThreshold {
        al.current = max(al.min, al.current / 2)
    } else if al.throughput > highThroughput {
        al.current = min(al.max, al.current + 1)
    }
}
```

This is clean because the controller, tracker, and workers share memory. No SQL
round-trips for control decisions. The semaphore sits between the tracker's
ready channels and the actual execution, allowing dynamic concurrency adjustment
without restarting workers.

---

## 9. Action Cancellation

### 9.1 Per-Action Context

Each tracked action has a `context.CancelFunc` set when a worker claims it:

```go
type trackedAction struct {
    action   Action
    cancel   context.CancelFunc  // set when worker claims the action
    deps     []ActionID
    depsLeft int32
    status   actionStatus
}
```

### 9.2 Cancellation Flow

When the observer detects a new change to a file currently being uploaded:

1. New events flow through the buffer and planner
2. Pre-dispatch deduplication checks the tracker for in-flight actions on the
   same path
3. Tracker calls `Cancel(path)`:
   - Calls `ta.cancel()` to cancel the worker's context immediately
   - Updates tracker status to `canceled`
   - Updates ledger row to `canceled`
4. Worker detects context cancellation, stops transfer, returns partial outcome
5. Replacement action is written to ledger and added to tracker

One mechanism, not two. The tracker holds the cancel function; the ledger
records the final status.

---

## 10. Backpressure

### 10.1 Bounded Tracker with Ledger Spillover

The tracker holds a bounded working window. The ledger holds the full queue
on disk:

```go
const maxTrackerSize = 10_000

func (dt *DepTracker) Add(action Action, deps []ActionID) {
    if len(dt.actions) >= maxTrackerSize {
        // Action is already in ledger. Don't load into tracker.
        // Refill loop will pull it when capacity opens.
        return
    }
    // ... normal add to in-memory tracker
}
```

### 10.2 Refill Loop

A background goroutine refills the tracker from the ledger when capacity opens:

```go
func (dt *DepTracker) refillLoop(db *sql.DB) {
    for range dt.capacitySignal {
        rows := queryPendingFromLedger(db, maxTrackerSize - len(dt.actions))
        for _, row := range rows {
            dt.addFromLedger(row)
        }
    }
}
```

Refill triggers when the tracker drops below 50% capacity. Refill fills to 80%
of `maxTrackerSize`. This provides a comfortable buffer without approaching the
memory bound.

### 10.3 Memory Analysis

| Component | Count | Memory |
|-----------|-------|--------|
| Tracker | 10,000 actions * ~500 bytes | ~5 MB (bounded) |
| Ledger | On disk | 0 MB (unlimited disk) |
| Baseline | 100K items * ~200 bytes | ~19 MB |
| **Total overhead** | | **~5 MB above baseline** |

A million-file initial sync stores 1M rows in the ledger (disk), loads 10K at
a time into the tracker (memory), and workers drain them at full speed. No
memory explosion.

### 10.4 Tracker-Ledger Consistency

The tracker is always a subset of the ledger's non-done actions. Invariant
enforcement:

- **Add**: Always writes to ledger first, then adds to tracker if capacity
  allows.
- **Complete/Cancel**: Always updates both tracker and ledger atomically
  (ledger update is part of the per-action commit transaction).
- **Startup**: Always rebuilds tracker from ledger (single source of truth).
- **Refill**: Only adds actions that are `pending` in the ledger and not
  already in the tracker.

---

## 11. Progress Reporting

### 11.1 Live Display from Tracker

The tracker provides real-time progress with zero DB overhead:

```go
func (dt *DepTracker) Progress() ProgressSnapshot {
    dt.mu.RLock()
    defer dt.mu.RUnlock()
    return ProgressSnapshot{
        Downloading:    dt.countByStatusAndType(claimed, download),
        Uploading:      dt.countByStatusAndType(claimed, upload),
        Waiting:        dt.countByStatus(pending),
        BytesInFlight:  dt.sumBytesInFlight(),
    }
}
```

### 11.2 Durable Audit Trail from Ledger

The ledger provides history for post-mortem analysis:

```sql
SELECT action_type, status, COUNT(*), SUM(bytes_done)
FROM action_queue
WHERE cycle_id = ?
GROUP BY action_type, status
```

Both are available: live display from the tracker during runtime, audit trail
from the ledger after process exit.

---

## 12. Commit Model

### 12.1 Per-Action Atomic Commits

Each completed action is committed in a single SQLite transaction that spans
both the baseline and the ledger:

```sql
BEGIN;
  -- Update baseline
  INSERT OR REPLACE INTO baseline (...) VALUES (...);
  -- Mark action done in ledger
  UPDATE action_queue SET status = 'done', completed_at = ? WHERE id = ?;
COMMIT;
```

If the transaction fails, neither the baseline nor the ledger is updated. The
action remains `claimed` and will be retried on restart.

If the process crashes after the transaction commits but before the tracker is
notified, the restart loads the ledger and sees the action is `done` --- no
duplicate execution.

### 12.2 Same Database

Both the `action_queue` and `baseline` tables live in the same SQLite database.
This provides transactional atomicity between baseline updates and ledger status
updates. Compatible with the existing sole-writer pattern
(`SetMaxOpenConns(1)`) since baseline writes are already serialized.

### 12.3 Per-Action Commit Operations

The per-action commit operation depends on the action type:

| Action Type | Baseline Operation | Ledger Operation |
|-------------|-------------------|-----------------|
| Download, Upload, UpdateSynced, FolderCreate | `INSERT ... ON CONFLICT(path) DO UPDATE` | `SET status = 'done'` |
| LocalMove, RemoteMove | `DELETE` old path + `INSERT` new path | `SET status = 'done'` |
| LocalDelete, RemoteDelete, Cleanup | `DELETE FROM baseline WHERE path = ?` | `SET status = 'done'` |
| Conflict | `INSERT INTO conflicts` + baseline upsert if applicable | `SET status = 'done'` |

---

## 13. Delta Token Management

### 13.1 Cycle-Scoped Tokens

The delta token is cycle-scoped. Each planning pass produces actions tagged
with a `cycle_id`. The token for that cycle is committed only when all actions
with that `cycle_id` reach `done`:

```go
func (cm *CycleManager) OnActionComplete(cycleID string) {
    remaining := countPendingForCycle(cycleID)
    if remaining == 0 {
        commitDeltaToken(cycleID, token)
    }
}
```

### 13.2 Multi-Cycle Overlap

Multiple cycles can overlap. Cycle 2's token might commit before cycle 1's if
cycle 2 has fewer/faster actions. Each cycle's token is independent.

If the process crashes with cycle 1 half-done, the delta token for cycle 1 is
not saved. On restart, the same delta is re-fetched. The planner sees that some
actions from that delta are already in baseline (committed individually per
action) and skips them. The remaining actions are re-planned and re-executed.
Idempotent by construction.

### 13.3 Token Commit Transaction

The delta token commit is a separate transaction from the per-action commits:

```sql
BEGIN;
  INSERT OR REPLACE INTO delta_tokens (drive_id, token, updated_at)
  VALUES (?, ?, ?);
COMMIT;
```

This is safe because all actions for the cycle are already committed to
baseline individually. The token commit is the final step that "seals" the
cycle.

---

## 14. Graceful Shutdown

### 14.1 Two-Signal Protocol

| Signal | Action |
|--------|--------|
| **First SIGINT/SIGTERM** | Stop accepting new actions from planner. Cancel all in-flight worker contexts. Workers detect cancellation, stop transfers, return partial outcomes. In-flight actions remain `claimed` in ledger. Exit cleanly. |
| **Second SIGINT/SIGTERM** | Immediate cancellation. No cleanup. SQLite WAL ensures DB consistency. |
| **SIGHUP** | Reload configuration. Re-initialize filter engine and bandwidth limiter. Continue running. |

### 14.2 Ledger State Preservation

On first signal:
1. Stop accepting new actions from planner
2. Cancel all in-flight worker contexts
3. Workers detect cancellation, stop transfers
4. In-flight actions remain `claimed` in ledger with `bytes_done` recording
   progress
5. Upload sessions with `session_url` can be resumed on restart
6. Download `.partial` files record download progress for `Range` header resume

### 14.3 Upload Resume

The ledger's `session_url` column stores the pre-authenticated upload URL. On
restart:

1. Load claimed upload actions from ledger
2. Check session expiry (typically 48 hours from creation)
3. Expired sessions: discard, re-upload from scratch
4. Valid sessions: verify local file hash matches expected hash (detect
   mutation during crash window). If match: query upload session endpoint for
   accepted byte ranges, resume from `bytes_done`. If hash differs: discard
   session, re-upload from scratch.

### 14.4 Download Resume

On restart, the `.partial` file's size indicates download progress. The worker
uses an HTTP `Range` header to resume from the last byte. Hash verification
covers the entire file after download completes.

---

## 15. Execution Modes

### 15.1 One-Off Upload-Only

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline from database
2. `LocalObserver.FullScan()` --- walk filesystem, compare against baseline,
   produce `[]ChangeEvent` (creates, modifies, deletes). Remote observer is
   skipped entirely.
3. `ChangeBuffer.AddAll() + FlushImmediate()` --- batch events by path
4. `Planner.Plan(changes, baseline, SyncUploadOnly, config)` --- produce
   `ActionPlan` with dependency DAG. Only upload-direction actions emitted
   (uploads, remote folder creates, remote deletes). Downloads suppressed.
5. Write actions to persistent ledger (status=pending)
6. Populate dependency tracker from ledger
7. Workers execute concurrently (uploads, folder creates, remote deletes) ---
   per-action baseline commits as each completes
8. All actions complete --- commit delta token (none for upload-only, but
   cycle tracking still applies)
9. Done. Baseline reflects all uploaded files.

### 15.2 One-Off Download-Only

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline
2. `RemoteObserver.FullDelta()` --- fetch all remote changes since last delta
   token, produce `[]ChangeEvent`. Local observer is skipped entirely.
3. `ChangeBuffer.AddAll() + FlushImmediate()` --- batch events by path
4. `Planner.Plan(changes, baseline, SyncDownloadOnly, config)` --- produce
   `ActionPlan` with dependency DAG. Only download-direction actions emitted
   (downloads, local folder creates, local deletes). Uploads suppressed.
5. Write actions to persistent ledger
6. Populate dependency tracker from ledger
7. Workers execute concurrently (downloads, folder creates, local deletes) ---
   per-action baseline commits
8. All actions complete --- commit delta token
9. Done. Baseline reflects all downloaded files.

### 15.3 One-Off Bidirectional Sync

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline
2. `RemoteObserver.FullDelta()` and `LocalObserver.FullScan()` run
   **concurrently** --- both produce `[]ChangeEvent`
3. `ChangeBuffer.AddAll() + FlushImmediate()` --- merge and batch by path
4. `Planner.Plan(changes, baseline, SyncBidirectional, config)` --- produce
   `ActionPlan` with dependency DAG. All action types: downloads, uploads,
   folder creates, deletes, moves, conflicts.
5. Write actions to persistent ledger
6. Populate dependency tracker from ledger
7. Workers execute concurrently --- **downloads and uploads run
   simultaneously**, no phase barriers. A download to `/A/x.txt` and an upload
   from `/B/y.txt` run in parallel if they have no dependency relationship.
8. Per-action baseline commits as each action completes
9. All actions complete --- commit delta token
10. Done. Baseline reflects full bidirectional sync state.

### 15.4 Watch Mode (Continuous)

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline
2. If pending actions exist in ledger (crash recovery), load into tracker and
   resume execution immediately
3. `RemoteObserver.Watch()` --- continuous delta polling (default 5-min
   interval) or WebSocket subscription. Emits `ChangeEvent` values to buffer.
4. `LocalObserver.Watch()` --- continuous inotify/FSEvents monitoring. Emits
   `ChangeEvent` values to buffer.
5. Observers and workers run independently --- observers never stop, workers
   are always available
6. `ChangeBuffer` debounces (2s) --- when ready, flushes `[]PathChanges`
7. `Planner.Plan()` --- incremental `ActionPlan` for changed paths only
8. Deduplicate against in-flight actions in tracker (by path). If a file is
   already being uploaded, cancel the stale upload (section 9).
9. Write new actions to ledger, add to tracker
10. Workers drain continuously --- per-action baseline commits
11. Loop from step 6 on next buffer ready signal

**Direction flags in watch mode**: `sync --watch --download-only` skips the
local observer entirely (step 4). Only remote changes are detected and
downloaded. Similarly, `sync --watch --upload-only` skips the remote observer.

**Periodic full scan**: As a safety net, the engine performs a periodic full
scan (configurable interval, default 1 hour) to catch any events missed by the
filesystem watcher (e.g., NFS mounts where inotify is unreliable).

**Action cancellation**: When a file changes while its upload is in-flight,
the new event flows through the buffer and planner. The pre-dispatch
deduplication step cancels the stale upload and replaces it with the new
version.

### 15.5 Dry-Run Mode

Steps 1-4 only (observe, buffer, plan). Print `ActionPlan`. No ledger write,
no execution, no commit. Zero side effects.

```
1. BaselineManager.Load()
2. Observe (remote and/or local, per direction mode)
3. ChangeBuffer flush
4. Planner.Plan() -> ActionPlan
5. Print plan summary to stdout
6. STOP. No ledger, no tracker, no workers, no commit.
```

Same as PRD SS11 dry-run requirement.

### 15.6 Initial Sync (Large Drive)

No delta token exists. The delta API returns every item. Batch processing
bounds memory:

1. Fetch delta page by page
2. Every 50K items:
   a. Flush buffer
   b. Plan (only these items)
   c. Write actions to ledger
   d. Load into tracker, workers execute
   e. Per-action baseline commits as transfers complete
   f. Commit intermediate delta token (next-link)
3. After all pages: commit final delta token

The ledger handles backpressure naturally: 50K items produce ~50K actions in
the ledger (disk), the tracker loads 10K at a time (bounded memory), workers
drain at full speed.

### 15.7 Pause/Resume (Watch Mode Only)

**Pause**:
1. Observers continue running (collecting events)
2. Buffer accumulates events
3. Workers stop pulling from tracker (paused flag)
4. In-flight actions complete normally (graceful pause, not hard stop)

**Resume**:
1. Flush buffer (potentially large batch of accumulated events)
2. Plan (incremental)
3. Write new actions to ledger
4. Add to tracker
5. Unpause workers --- they resume pulling from tracker

**High-water mark**: If >100K events accumulate during pause, collapse to a
full sync on resume (discard buffer, re-observe everything). This prevents
excessive memory consumption during extended pauses.

---

## 16. Impact on Existing Architecture

### 16.1 What Stays As-Is

- **Package layout**: `driveid`, `config`, `graph`, `sync`, `quickxorhash`, `cmd`
- **Type system**: `ChangeEvent`, `PathChanges`, `PathView`, `Action`, `Outcome`,
  `BaselineEntry`
- **Observer algorithms**: RemoteObserver delta fetch + normalization,
  LocalObserver walk + hash
- **Change buffer**: debounce, dedup, move dual-keying, `FlushImmediate`
- **Planner**: pure function, decision matrices EF1-EF14 / ED1-ED8, move
  detection, safety checks
- **Graph API client**: auth, retry, items CRUD, delta, transfers, upload
  lifecycle
- **Safety guarantees S1-S7**: enforced in planner and per-action execution,
  not scheduling
- **Error classification**: fatal/retryable/skip/deferred
- **API quirk normalization**: 23 known quirks at observer boundary
- **Config system**: TOML, multi-drive, XDG paths, hot-reload
- **CLI commands**: ls, get, put, rm, mkdir, stat, sync, status, conflicts,
  resolve, verify
- **SQLite configuration**: WAL, SYNCHRONOUS=FULL, sole writer, busy_timeout

### 16.2 Execution Components

| Component | Design |
|-----------|--------|
| **Executor model** | DAG with dependency tracker --- actions dispatched based on dependency satisfaction |
| **Worker pool** | Unified lane-based pool (interactive + bulk + shared overflow). Checker pool separate. |
| **Commit model** | Per-action atomic commits (baseline + ledger status in same tx). Delta token committed separately when cycle completes. |
| **Crash recovery** | Ledger-based exact resume --- pending/claimed actions loaded from ledger, no full re-observation needed |
| **Context tree** | Persistent workers on tracker channels, surviving across planning passes |

### 16.3 Component Details

- **BaselineManager**: Same schema, same sole-writer. `CommitOutcome()` commits
  one outcome per transaction. `CommitDeltaToken()` seals cycle token separately.
  `Load()` and `GetDeltaToken()` unchanged.
- **Engine orchestration**: One-shot and watch modes both use ledger + tracker.
  Watch mode uses continuous pipeline with persistent workers.
- **Executor per-action logic**: download (`.partial` + hash + rename), upload
  (simple vs chunked), delete (hash-before-delete S4), conflict resolution ---
  each action type is self-contained. The executor dispatches by action type;
  the tracker handles scheduling.
- **Upload session persistence**: The `upload_sessions` table stores detailed
  session metadata. The `action_queue` ledger tracks `session_url` for
  execution-level state, enabling crash resume of in-flight uploads.

---

## 17. Open Questions

1. **Tracker-ledger consistency verification.** The tracker is always a subset
   of the ledger's non-done actions. This invariant should be formally verified
   in tests --- inject ledger states, rebuild tracker, verify consistency.

2. **Same vs. separate DB for ledger.** Recommended: same DB. Provides
   transactional atomicity between baseline and ledger. Separate DB would
   require two-phase reasoning about consistency.

3. **Planner deduplication strategy.** When observers detect new changes
   mid-execution, the planner runs again. It must not re-emit actions already
   in the tracker or ledger. Preferred: tracker deduplicates by path (preserves
   planner purity). Alternative: planner queries tracker state (breaks
   pure-function constraint).

4. **Reclaim timeout for stale claims.** Must be longer than the longest
   expected transfer. Default: 30 minutes, configurable. Reaper goroutine
   checks periodically.

5. **One-shot ledger usage.** For crash recovery and upload session resume,
   one-shot sync also uses the ledger infrastructure. The overhead is minimal
   (write actions to ledger before executing). One-shot skips the continuous
   observer loop but otherwise uses the same tracker and worker infrastructure.

6. **Lane size threshold.** Fixed at 10 MB. Provides predictable behavior
   without requiring tuning. Adaptive threshold (p50 of recent action sizes)
   adds complexity with marginal benefit.

7. **Refill batch size and frequency.** Refill to 80% of `maxTrackerSize` when
   tracker drops below 50%. A reasonable default that provides comfortable
   buffer without approaching memory bound.

8. **Config key reconciliation.** PRD specifies `parallel_downloads`,
   `parallel_uploads`, `parallel_checkers` as three separate configurable
   values. The lane model combines downloads and uploads into interactive/bulk
   lanes. Section 6.4 defines the mapping. The PRD keys are preserved for
   backwards compatibility; their sum determines total lane worker count.
