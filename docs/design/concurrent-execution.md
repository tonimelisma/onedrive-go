# Execution Architecture

This document is the definitive specification for the execution layer of
onedrive-go's sync engine. It covers how planned actions are scheduled,
dispatched to workers, committed to the database, and recovered after crashes.

The execution architecture uses a two-layer design: an **in-memory dependency
tracker** (`DepTracker`) provides instant dispatch, lane-based fairness, and
action cancellation; the **SQLite baseline** provides durability through
per-action atomic commits. Crash recovery relies on the idempotent planner:
if the process crashes mid-cycle, the delta token has not advanced, so the same
delta is re-fetched and the planner detects already-committed actions as
convergent and skips them.

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
    and why, replay failures.
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
| PRD SS10 | Resumable transfers (upload sessions, Range headers, state persisted to disk) | Section 14 --- graceful shutdown, SessionStore |
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
| **DepTracker** | In-memory data structure | Scheduling, dispatch, lane fairness, cancellation, dependency resolution | Zero latency, bounded memory, channel-based |
| **Baseline** | SQLite `baseline` table | Durability, crash recovery via idempotent planner, per-action atomic commits | Survives process exit, queryable |

The DepTracker is the scheduling engine that determines when actions are ready
and dispatches them to workers. The baseline is the durable record of what has
been synced. Crash recovery works through idempotency: uncommitted actions are
re-planned and re-executed on restart.

### 2.2 Component Diagram

```
Planner --> ActionPlan with dependency DAG
               |
               v
     +--------------------+
     | DepTracker          |
     | (in-memory)         |
     |                     |
     | ready channels:     |
     |   interactive []    |
     |   bulk        []    |
     |                     |
     | * lane dispatch     |
     | * cancellation      |
     | * dep counting      |
     | * cycle tracking    |
     +----------+----------+
                |
     +----------+----------+----------+
     v          v          v          v
 Worker 1   Worker 2   Worker 3   Worker N
 (interactive) (bulk)  (shared)   (shared)
     |          |          |          |
     +----------+----------+----------+
                |
         WorkerResult channel
                |
                v
     +--------------------+
     | Engine              |
     | (drainWorkerResults)|
     |                     |
     | per-action commit:  |
     |   baseline upsert   |
     |   tracker.Complete() |
     +--------------------+
```

### 2.3 Startup Sequence

1. Load baseline from database (`BaselineManager.Load()`)
2. Load SessionStore for any persisted upload sessions
3. Start workers (interactive lane, bulk lane, shared pool)
4. Start observers (remote, local)
5. Workers begin draining tracker immediately

### 2.4 Execution Flow

1. Observers detect changes, emit `ChangeEvent` values to the change buffer
2. Buffer debounces (2s) and flushes `[]PathChanges`
3. Planner produces `ActionPlan` with dependency DAG (pure function)
4. Actions loaded into DepTracker with dependency edges
5. Tracker dispatches ready actions to worker lanes via channels
6. Workers execute actions (same per-action logic: downloads, uploads, deletes, moves)
7. Workers send `WorkerResult` back through the results channel
8. Engine's `drainWorkerResults` loop processes each result: per-action atomic commit (baseline upsert) + `tracker.Complete(id)` to unblock dependents
9. Tracker dispatches newly ready actions as dependencies are satisfied
10. When all actions for a cycle complete: commit delta token

---

## 3. DepTracker

### 3.1 Purpose

The DepTracker provides:

- **Zero-latency dispatch**: When action X completes and unblocks action Y,
  Y is dispatched to the ready channel immediately (in the same `Complete()`
  call). No polling.
- **Lane-based fairness**: Ready actions are routed to interactive or bulk
  channels based on file size and action type.
- **Action cancellation**: Each in-flight action has a `context.CancelFunc`.
  Cancellation is instant.
- **Cycle tracking**: Tracks which actions belong to which planning cycle.
  Signals cycle completion via `CycleDone()` channels.
- **Progress snapshots**: `InFlightCount()` returns live counts with a single
  mutex acquisition.

### 3.2 Data Structures

```go
type DepTracker struct {
    mu          sync.Mutex
    actions     map[int64]*trackedAction
    byPath      map[string]*trackedAction      // for cancellation by path
    interactive chan *trackedAction             // small files, folder ops, deletes
    bulk        chan *trackedAction             // large file transfers
    cycles      map[string]*cycleState         // cycle completion tracking
    persistent  bool                           // watch mode: channels stay open
    logger      *slog.Logger
}

type trackedAction struct {
    action     *Action
    id         int64
    cancel     context.CancelFunc              // set when worker claims the action
    depsLeft   int32                           // atomic counter
    dependents []*trackedAction                // actions waiting on this one
    cycleID    string
}
```

### 3.3 Operations

**Add(action, id, depIDs, cycleID)**: Insert an action into the tracker. If
`depsLeft == 0`, dispatch to the appropriate ready channel immediately.
Otherwise, register as a dependent of each dependency.

**Complete(id)**: Mark action done. For each dependent action, atomically
decrement `depsLeft`. If any dependent reaches zero, dispatch it to the ready
channel. Track cycle completion.

**CancelByPath(path)**: Look up by path. If the action is in-flight, call
its `context.CancelFunc` to cancel the worker's context immediately.

**CycleDone(cycleID)**: Returns a channel that closes when all actions in the
given cycle have completed. Used by the engine to know when to commit the delta
token.

### 3.4 Ready Channel Dispatch

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

## 4. WorkerResult Channel

### 4.1 Purpose

Workers report outcomes back to the engine through a `WorkerResult` channel:

```go
type WorkerResult struct {
    ActionID int64
    CycleID  string
    Outcome  *Outcome
    Err      error
}
```

The engine's `drainWorkerResults` goroutine reads from this channel and
processes each result: committing the outcome to the baseline and notifying
the tracker via `Complete(id)` to unblock dependent actions.

This design keeps workers simple (execute action, send result) and centralizes
commit logic in the engine. Workers never interact with the baseline or tracker
directly.

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
Total workers: N (runtime.NumCPU() or user-configured cap, minimum 4)

Lane: interactive (files < 10 MB, folder ops, deletes, moves, conflicts)
  Reserved workers: max(2, N/8) (always available for small ops)

Lane: bulk (files >= 10 MB)
  Reserved workers: max(2, N/8) (always available for large transfers)

Shared pool: N - reserved_interactive - reserved_bulk workers
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

| Config Parameter | Default | Lane Mapping |
|----------------|---------|-------------|
| Total lane workers | `runtime.NumCPU()` | Split into interactive, bulk, and shared lanes |
| `parallel_checkers` | 8 | Separate checker pool (unchanged --- not in lanes) |

Total lane workers = `runtime.NumCPU()` (minimum 4).
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
    action   *Action
    cancel   context.CancelFunc  // set when worker claims the action
    depsLeft int32
    dependents []*trackedAction
}
```

### 9.2 Cancellation Flow

When the observer detects a new change to a file currently being uploaded:

1. New events flow through the buffer and planner
2. Pre-dispatch deduplication checks the tracker for in-flight actions on the
   same path
3. Tracker calls `CancelByPath(path)`:
   - Calls `ta.cancel()` to cancel the worker's context immediately
   - Removes action from tracker
4. Worker detects context cancellation, stops transfer, returns partial outcome
5. Replacement action is added to tracker with the new cycle

---

## 10. Backpressure

### 10.1 Channel-Based Backpressure

The DepTracker uses buffered channels for the interactive and bulk lanes.
When channels are full, the `Add()` call blocks until a worker drains an
action, providing natural backpressure from workers to the planner.

For one-shot mode, channels are sized to the plan size. For watch mode,
`NewPersistentDepTracker` uses default buffer sizes.

### 10.2 Memory Analysis

| Component | Count | Memory |
|-----------|-------|--------|
| Tracker | Actions in current cycle * ~500 bytes | Bounded by plan size |
| Baseline | 100K items * ~200 bytes | ~19 MB |
| **Total overhead** | | **Proportional to cycle size** |

The planner produces actions for one cycle at a time. In watch mode, cycles
are small (only changed files). In one-shot mode, the entire plan is loaded
into memory, but this is bounded by the number of changed files.

For initial syncs of very large drives, delta pages are processed in batches
(see section 15.6), keeping each cycle's action count manageable.

---

## 11. Progress Reporting

### 11.1 Live Display from Tracker

The tracker provides real-time progress with zero DB overhead:

```go
func (dt *DepTracker) InFlightCount() int {
    dt.mu.Lock()
    defer dt.mu.Unlock()
    return len(dt.actions)
}
```

The `WorkerResult` channel provides a stream of completed actions that the
engine can use to update progress displays in real time.

---

## 12. Commit Model

### 12.1 Per-Action Atomic Commits

Each completed action is committed in a single SQLite transaction against the
baseline:

```sql
BEGIN;
  -- Update baseline
  INSERT OR REPLACE INTO baseline (...) VALUES (...);
COMMIT;
```

If the transaction fails, the baseline is not updated. On restart, the
idempotent planner will re-plan the action.

If the process crashes after the transaction commits but before the tracker is
notified, the restart re-fetches the same delta (token not yet advanced), and
the planner sees the already-committed item as convergent --- no duplicate
execution.

### 12.2 Per-Action Commit Operations

The per-action commit operation depends on the action type:

| Action Type | Baseline Operation |
|-------------|-------------------|
| Download, Upload, UpdateSynced, FolderCreate | `INSERT ... ON CONFLICT(path) DO UPDATE` |
| LocalMove, RemoteMove | `DELETE` old path + `INSERT` new path |
| LocalDelete, RemoteDelete, Cleanup | `DELETE FROM baseline WHERE path = ?` |
| Conflict | `INSERT INTO conflicts` + baseline upsert if applicable |

---

## 13. Delta Token Management

### 13.1 Cycle-Scoped Tokens

The delta token is cycle-scoped. Each planning pass produces actions tagged
with a `cycle_id`. The token for that cycle is committed only when all actions
with that `cycle_id` complete. The DepTracker's `CycleDone(cycleID)` channel
signals when this occurs:

```go
func (e *Engine) waitForCycleCompletion(cycleID string) {
    <-e.tracker.CycleDone(cycleID)
    e.baseline.CommitDeltaToken(cycleID, token)
    e.tracker.CleanupCycle(cycleID)
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

## 14. Graceful Shutdown and Crash Recovery

### 14.1 Two-Signal Protocol

| Signal | Action |
|--------|--------|
| **First SIGINT/SIGTERM** | Stop accepting new actions from planner. Cancel all in-flight worker contexts. Workers detect cancellation, stop transfers, return partial outcomes. Exit cleanly. |
| **Second SIGINT/SIGTERM** | Immediate cancellation. No cleanup. SQLite WAL ensures DB consistency. |
| **SIGHUP** | Reload configuration. Re-initialize filter engine and bandwidth limiter. Continue running. |

### 14.2 State Preservation on Shutdown

On first signal:
1. Stop accepting new actions from planner
2. Cancel all in-flight worker contexts
3. Workers detect cancellation, stop transfers
4. Upload sessions are persisted to SessionStore (file-based) for cross-crash
   resume
5. Download `.partial` files record download progress for `Range` header resume
6. Delta token is NOT committed (cycle incomplete), ensuring idempotent
   re-planning on restart

### 14.3 Crash Recovery via Idempotent Planner

Crash recovery does not require a persistent action queue. Instead:

1. On restart, `BaselineManager.Load()` loads the baseline (reflects all
   committed actions)
2. `GetDeltaToken()` returns the last committed token (pre-crash cycle's token
   was never committed)
3. Delta fetch returns the same changes as the crashed cycle
4. Planner compares changes against the baseline:
   - Actions that completed before the crash are already in the baseline ---
     the planner detects them as convergent (EF4/EF11) and skips them
   - Actions that did not complete are re-planned and re-executed
5. No duplicate work, no lost work

### 14.4 Upload Resume via SessionStore

The `SessionStore` is a file-based store that persists upload session URLs
for cross-crash resume:

1. When a chunked upload session is created, the session URL is saved to the
   SessionStore
2. On restart, the SessionStore is loaded and checked for valid sessions
3. Expired sessions (typically 48 hours from creation): discarded, re-upload
   from scratch
4. Valid sessions: verify local file hash matches expected hash (detect
   mutation during crash window). If match: query upload session endpoint for
   accepted byte ranges, resume from last offset. If hash differs: discard
   session, re-upload from scratch.
5. After successful upload or discard, the session entry is removed from the
   SessionStore

### 14.5 Download Resume

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
5. Load actions into DepTracker with dependency edges
6. Workers execute concurrently (uploads, folder creates, remote deletes) ---
   per-action baseline commits as each completes
7. All actions complete --- commit delta token (none for upload-only, but
   cycle tracking still applies)
8. Done. Baseline reflects all uploaded files.

### 15.2 One-Off Download-Only

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline
2. `RemoteObserver.FullDelta()` --- fetch all remote changes since last delta
   token, produce `[]ChangeEvent`. Local observer is skipped entirely.
3. `ChangeBuffer.AddAll() + FlushImmediate()` --- batch events by path
4. `Planner.Plan(changes, baseline, SyncDownloadOnly, config)` --- produce
   `ActionPlan` with dependency DAG. Only download-direction actions emitted
   (downloads, local folder creates, local deletes). Uploads suppressed.
5. Load actions into DepTracker with dependency edges
6. Workers execute concurrently (downloads, folder creates, local deletes) ---
   per-action baseline commits
7. All actions complete --- commit delta token
8. Done. Baseline reflects all downloaded files.

### 15.3 One-Off Bidirectional Sync

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline
2. `RemoteObserver.FullDelta()` and `LocalObserver.FullScan()` run
   **concurrently** --- both produce `[]ChangeEvent`
3. `ChangeBuffer.AddAll() + FlushImmediate()` --- merge and batch by path
4. `Planner.Plan(changes, baseline, SyncBidirectional, config)` --- produce
   `ActionPlan` with dependency DAG. All action types: downloads, uploads,
   folder creates, deletes, moves, conflicts.
5. Load actions into DepTracker with dependency edges
6. Workers execute concurrently --- **downloads and uploads run
   simultaneously**, no phase barriers. A download to `/A/x.txt` and an upload
   from `/B/y.txt` run in parallel if they have no dependency relationship.
7. Per-action baseline commits as each action completes
8. All actions complete --- commit delta token
9. Done. Baseline reflects full bidirectional sync state.

### 15.4 Watch Mode (Continuous)

End-to-end walkthrough:

1. `BaselineManager.Load()` --- load baseline
2. Load SessionStore for any persisted upload sessions (crash recovery)
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
9. Add new actions to tracker
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

Steps 1-4 only (observe, buffer, plan). Print `ActionPlan`. No execution, no
commit. Zero side effects.

```
1. BaselineManager.Load()
2. Observe (remote and/or local, per direction mode)
3. ChangeBuffer flush
4. Planner.Plan() -> ActionPlan
5. Print plan summary to stdout
6. STOP. No tracker, no workers, no commit.
```

Same as PRD SS11 dry-run requirement.

### 15.6 Initial Sync (Large Drive)

No delta token exists. The delta API returns every item. Batch processing
bounds memory:

1. Fetch delta page by page
2. Every 50K items:
   a. Flush buffer
   b. Plan (only these items)
   c. Load actions into DepTracker
   d. Workers execute concurrently
   e. Per-action baseline commits as transfers complete
   f. Commit intermediate delta token (next-link)
3. After all pages: commit final delta token

Batching keeps each cycle's action count manageable. The tracker holds one
cycle at a time. Workers drain at full speed.

### 15.7 Pause/Resume (Watch Mode Only)

**Pause**:
1. Observers continue running (collecting events)
2. Buffer accumulates events
3. Workers stop pulling from tracker (paused flag)
4. In-flight actions complete normally (graceful pause, not hard stop)

**Resume**:
1. Flush buffer (potentially large batch of accumulated events)
2. Plan (incremental)
3. Add actions to tracker
4. Unpause workers --- they resume pulling from tracker

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
| **Executor model** | DAG with DepTracker --- actions dispatched based on dependency satisfaction |
| **Worker pool** | Unified lane-based pool (interactive + bulk + shared overflow). Checker pool separate. |
| **Commit model** | Per-action atomic commits (baseline upsert). Delta token committed separately when cycle completes. |
| **Crash recovery** | Idempotent planner --- uncommitted delta token causes same delta to be re-fetched, planner skips already-committed actions |
| **Context tree** | Persistent workers on tracker channels, surviving across planning passes |

### 16.3 Component Details

- **BaselineManager**: Same schema, same sole-writer. `CommitOutcome()` commits
  one outcome per transaction. `CommitDeltaToken()` seals cycle token separately.
  `Load()` and `GetDeltaToken()` unchanged.
- **Engine orchestration**: One-shot and watch modes both use DepTracker +
  WorkerResult channel. Watch mode uses continuous pipeline with persistent
  workers.
- **Executor per-action logic**: download (`.partial` + hash + rename), upload
  (simple vs chunked), delete (hash-before-delete S4), conflict resolution ---
  each action type is self-contained. The executor dispatches by action type;
  the tracker handles scheduling.
- **Upload session persistence**: The `SessionStore` (file-based) stores upload
  session metadata for cross-crash resume. Session URLs are saved when chunked
  uploads begin and removed when they complete or are discarded.

---

## 17. Open Questions

1. **Planner deduplication strategy.** When observers detect new changes
   mid-execution, the planner runs again. It must not re-emit actions already
   in the tracker. Preferred: tracker deduplicates by path (preserves
   planner purity). Alternative: planner queries tracker state (breaks
   pure-function constraint).

2. **Lane size threshold.** Fixed at 10 MB. Provides predictable behavior
   without requiring tuning. Adaptive threshold (p50 of recent action sizes)
   adds complexity with marginal benefit.

3. **Config key reconciliation.** PRD specifies `parallel_downloads`,
   `parallel_uploads`, `parallel_checkers` as three separate configurable
   values. The lane model combines downloads and uploads into interactive/bulk
   lanes. Section 6.4 defines the mapping. The PRD keys are preserved for
   backwards compatibility; their sum determines total lane worker count.

---

## 18. Observation-Layer Parallelization

Sections 1--17 cover the **execution** layer (workers, tracker, lanes).
This section covers parallelization opportunities in the **observation** layer
— the pipeline stages that run before the planner.

### 18.1 Parallel Remote + Local Observation (B-170)

Remote observation is network-bound (Graph API delta pagination). Local
observation is disk-bound (readdir, Lstat, file hashing). These use different
I/O resources and share no mutable state — both read the baseline in read-only
mode via `sync.RWMutex`-protected accessors.

In `RunOnce`, steps 2 (remote) and 3 (local) can run concurrently via
`errgroup.Go()`. This halves observation time for bidirectional sync. In
`RunWatch`, both observers already run in separate goroutines.

**Implementation:** Wrap `observeRemote` and `observeLocal` in an errgroup.
Each returns its events + error. Assemble after `g.Wait()`. ~10 lines of
change, zero architectural impact.

**Why this is safe:** Both methods are pure readers of baseline. Neither writes
to any shared state. The errgroup provides clean error propagation — if either
observer fails, the other is canceled via the derived context.

### 18.2 Parallel Hashing in FullScan (B-096)

`FullScan` walks the filesystem and hashes files inline in the `WalkDir`
callback. For initial syncs with no baseline (mtime+size fast-path cannot skip
any files), hashing is the dominant cost:

| Operation | Time (100K files, 1MB avg, SSD) | Share |
|-----------|-------------------------------|-------|
| Walk (readdir + Lstat) | ~200ms | 0.02% |
| Baseline lookup + fast-path | ~10ms | <0.01% |
| Sequential hashing | ~15 min | 99.98% |
| Parallel hashing (8 cores) | ~2 min | — |

**Design decision: walk stays sequential, hashing fans out.**

The walk must remain sequential because:

1. **Disk metadata serialization.** readdir and Lstat are I/O-bound against the
   filesystem metadata layer. On a single SSD, parallel readdir calls contend
   on the same NVMe queue. On NFS, parallel stats actively hurt due to IOPS
   limits and round-trip latency.
2. **Consistent `observed` map.** The walk populates a `map[string]bool` of
   observed paths. Single-writer means no mutex, no races, no bugs.
3. **Walk is 0.02% of total time.** Even a 10× speedup of the walk saves 180ms
   on a 15-minute operation.

The hash pool processes files that the walk identified as needing hashes (new
files, mtime/size changed, racily clean). Hash jobs are embarrassingly
parallel — each file is independent, and the QuickXorHash computation uses
`io.Copy` with constant memory.

**Implementation:**

```
FullScan:
  Phase 1: Walk (sequential)
    - readdir + Lstat each entry
    - classify: skip / folder-event / fast-path-skip / needs-hash
    - populate observed[path] = true (always, for all non-skipped)
    - append folder events directly
    - append hashJob{fsPath, metadata} for files needing hash

  Phase 2: Hash (parallel, errgroup.SetLimit(runtime.NumCPU()))
    - each goroutine: computeQuickXorHash(job.fsPath)
    - if hash != baseline.LocalHash → append ChangeEvent
    - if hash error → log warn, skip file

  Phase 3: Deletion detection (sequential)
    - observed map vs baseline → delete events
```

~30 lines of new code. Walk logic, classification logic, deletion detection —
all unchanged. The only structural change is splitting the hash call out of
`processEntry` into a deferred parallel phase.

**What we explicitly do NOT do:**

- **Shared long-lived hash pool across FullScan and Watch.** This was
  considered and rejected. A shared pool adds lifecycle management, channel
  orchestration, result routing, backpressure, and error propagation complexity
  across three integration points (FullScan, watch events, safety scan). The
  FullScan parallel hash is ~30 lines with an errgroup. A shared pool is ~200
  lines with channels, context management, and shutdown coordination. The
  complexity is not justified when the simpler approach captures the same
  performance benefit for FullScan. Watch-mode hashing improvements (B-107) are
  a separate concern.
- **Parallel walk (concurrent directory subtree traversal).** Doesn't help for
  flat directories. Goroutine explosion for deep trees. Concurrent `observed`
  map writes need mutex. `filepath.WalkDir` is designed for single-threaded
  use. Disk I/O serializes anyway.
- **Streaming walk→hash pipeline.** Over-engineered. Overlapping walk and hash
  saves ~200ms on a 15-minute operation. Not worth the channel orchestration.

### 18.3 Watch-Mode Observation

Watch-mode hashing is a different problem from FullScan hashing. In FullScan,
you have a batch of files to hash after a walk. In watch mode, you have a
stream of individual file events arriving asynchronously.

The current watch event loop hashes files inline in `handleCreate` and
`handleWrite`, blocking the fsnotify event loop. During a burst (e.g., `git
checkout` producing 500 write events), each blocks for 5–15ms → 2.5–7.5s total
during which new events queue up in fsnotify's kernel buffer.

Write event coalescing (B-107) addresses this by adding a per-path debounce
timer before hashing. This is a watch-mode-only concern — it does not affect
FullScan. The design and implementation of watch-mode hashing improvements will
be determined when `RunWatch()` is profiled under realistic load.

### 18.4 Deferred Optimizations

The following observation-layer optimizations are explicitly deferred until
profiling demonstrates a bottleneck:

- **Streaming delta pages (B-171).** Processing each page as it arrives
  overlaps page processing (~1ms) with the next API call (~100–300ms). The
  savings are ~1ms per page. The memory benefit is real but modest for typical
  deltas. P3.
- **Pipelined observation → execution.** Starting execution before observation
  completes requires partial planning, which can produce incorrect results
  (e.g., move detection needs to see both the delete and the create). Only
  viable in watch mode where observation is continuous. Not applicable to
  RunOnce.

---

## 19. Multi-Drive Worker Budget (DESIGN GAP)

> **STATUS: UNRESOLVED DESIGN GAP** — The worker budget algorithm for multi-drive sync must be specified before Phase 7.0 implementation begins.

When multiple drives sync simultaneously, the total worker count across all drives must be bounded to prevent resource exhaustion (5 drives x 16 workers = 80 I/O goroutines). The current single-drive worker model (section 6) allocates interactive/bulk/shared lanes based on a single drive's config. Multi-drive sync needs a global allocation strategy.

### Constraints

- **Global cap needed**: A `max_total_workers` config option (or computed default) must limit the total worker goroutines across all drives.
- **Minimum per drive**: Each active drive needs a minimum viable allocation (at least 1 interactive + 1 bulk worker) to make progress. If 10 drives are enabled and the global cap is 16, some drives may need to wait.
- **Checker pool scoping**: The hash checker pool (CPU-bound, `runtime.NumCPU()` workers) should likely be global rather than per-drive, since it competes for CPU cores. Per-drive checker pools would oversubscribe on machines with many drives.
- **Dynamic reallocation**: When drives complete their sync cycles (one-shot) or enter idle (watch mode, nothing to do), their workers should be available for other drives.

### Open Questions

See [MULTIDRIVE.md §11](MULTIDRIVE.md#11-multi-drive-orchestrator-design-gap) for the full list of orchestrator-level open questions, including worker budget, rate limit coordination, bandwidth limiting, and error isolation.
