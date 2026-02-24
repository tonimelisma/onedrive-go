# Concurrent Execution Redesign

## Problem

`RunOnce` is a strictly sequential pipeline: load baseline, observe remote, observe local, buffer, plan, execute (folder creates, moves, downloads, uploads, local deletes, remote deletes, conflicts, synced updates, cleanups), commit. Every phase must fully complete before the next one begins. Downloads and uploads have internal parallelism (8 workers each), but they run one after the other — downloads must all finish before the first upload starts.

This causes five user-facing problems:

1. **Downloads block uploads.** All 8 download workers must finish before the first upload starts. 1 GB down + 1 GB up runs back-to-back, doubling wall-clock time.
2. **One slow transfer starves workers.** A phase can't end until the last worker finishes. One 500 MB file holds up the transition while 7 workers sit idle.
3. **Interrupted sync loses all progress.** If the process dies after completing 99 of 100 transfers, all progress is lost. The next sync re-observes and re-transfers everything.
4. **No new changes detected during execution.** Changes that arrive while transfers are running aren't noticed until the next `RunOnce` call.
5. **No transfer resume.** A crashed chunked upload or download restarts from byte 0.

These problems are acceptable for Phase 4 one-shot sync but fundamentally unsuitable for Phase 5 watch mode, where a multi-GB upload would block change detection for hours.

## Current Sequential Flow

| Step | Phase | Parallel? | Blocks on | Duration |
|------|-------|-----------|-----------|----------|
| 1 | Load baseline | No | SQLite read | ms |
| 2 | Observe remote (delta pages) | No | Network, API rate limits | 1-10s |
| 3 | Observe local (walk + hash) | No | Filesystem I/O, hashing | 2-30s |
| 4 | Buffer and flush | No | Memory | <1ms |
| 5 | Plan (decision matrices) | No | CPU | <100ms |
| 6 | Execute: folder creates | No | Network/filesystem | 100ms-1s |
| 7 | Execute: moves | No | Network/filesystem | 100ms-1s |
| 8 | Execute: downloads | 8 workers | Network | 10s-5min |
| 9 | Execute: uploads | 8 workers | Network | 10s-10min |
| 10 | Execute: local deletes | No | Filesystem, hashing (S4) | 100ms-2s |
| 11 | Execute: remote deletes | No | Network | 100ms-1s |
| 12 | Execute: conflicts | No | Network | varies |
| 13 | Execute: synced updates | No | Memory | <1ms |
| 14 | Execute: cleanups | No | Memory | <1ms |
| 15 | Commit (all outcomes + delta token) | No | SQLite write | ms |
| 16 | Baseline cache reload | No | SQLite read | ms |

Steps 2-3 are independent but run sequentially. Steps 6-14 have per-path dependencies but are enforced via coarse phase barriers. Step 15 is all-or-nothing.

## Real Ordering Dependencies

There are surprisingly few hard constraints:

1. **Parent folder must exist before child operations.** `/A/` must exist before uploading `/A/file.txt` or creating `/A/B/`. Per-path dependency, not a global phase barrier.
2. **Children must be removed before parent folder deletion.** Delete `/A/file.txt` before `rmdir /A/`. Per-path, not global.
3. **Move target parent must exist.** Moving `/X/a.txt` to `/Y/a.txt` requires `/Y/` to exist. Per-path.
4. **Delta token advances only when all outcomes from that delta are durable.** Global, but only constrains the token commit — not individual action execution.
5. **Baseline must reflect a consistent state.** Per-action commits are fine as long as each committed outcome is self-contained.

Everything else — downloads before uploads, all folder creates before any download, all transfers before any delete — is accidental coupling from the phased execution model.

## Evaluation Criteria

Derived from the PRD, architecture doc, safety guarantees (S1-S7), and operational requirements.

### Tier 1 — Non-negotiable (safety and correctness)

1. **Data safety (S1-S7).** No data loss, ever. Hash-before-delete (S4), atomic file writes via `.partial` + rename (S3), big-delete protection (S5), no partial uploads reaching remote (S7), never delete remote without synced base (S1), never process deletions from incomplete enumeration (S2), disk space check before downloads (S6).
2. **Crash recovery.** Process can die at any point. Restart resumes without duplicating or losing work.
3. **Ordering correctness.** Parents before children, children before parent deletes, no races between dependent actions.
4. **Delta token consistency.** Token advances only when all outcomes from that delta are durable in baseline.

### Tier 2 — Critical (performance and responsiveness)

5. **Maximum parallelism.** Independent actions run concurrently. No artificial barriers.
6. **Watch mode compatibility.** Observers run continuously, transfers drain independently, new changes detected while transfers are in-flight. Three concurrent subsystems: observers, transfer queue, baseline commits. (Architecture doc section 5.3, Roadmap section 265.)
7. **Incremental progress.** Completed transfers are durable immediately, not held hostage by incomplete ones.
8. **Idle efficiency.** < 1% CPU when nothing is changing. < 100 MB for 100K synced files. (PRD section 20.)
9. **Worker fairness.** N large files must not monopolize all workers. A flood of small changes must not starve large transfers. Both must make progress simultaneously.
10. **Bandwidth control.** Per-direction limits (upload, download), combined limits, and scheduled throttling (full speed at night, throttled during work hours).
11. **Adaptive concurrency.** Scale worker count up when many small files queue (throughput-bound), down when few large files queue (bandwidth-bound), and back off when hitting 429 throttle responses.
12. **Graceful shutdown.** Ctrl-C stops cleanly with no lost progress. In-flight actions must be recoverable on restart.
13. **Action cancellation.** If a file changes while its upload is in-flight, cancel the stale upload and start a new one. Don't upload a version that's already outdated.
14. **Backpressure.** If the planner produces actions faster than workers execute (massive initial sync), the system must not accumulate unbounded memory.
15. **Progress reporting.** Users see real-time status: "downloading 3 files (2.1 GB), uploading 5 files (340 MB), 12 waiting." Not just at cycle end.

### Tier 3 — Important (engineering quality)

16. **Testability.** Scheduler, dependency resolver, and commit logic must be unit-testable without I/O or network.
17. **Debuggability.** Inspect the state of every action, see what's blocked and why, replay failures. State should survive process exit for post-mortem analysis.
18. **Extensibility.** Straightforward to add priority scheduling, pause/resume, progress bars, upload session resume (B-085).
19. **Invariant simplicity.** Fewer simultaneous invariants = safer. The system must be correct, but the proof of correctness should be as simple as possible.

### Constraints from Architecture Doc

These are existing architectural decisions that the new design must respect:

- **Event-driven pipeline.** Observers produce events (no DB writes), planner is pure function (no I/O, no DB), executor produces outcomes (no DB writes), BaselineManager is sole writer. (Architecture section 1.)
- **Baseline is the only durable per-item state.** Remote and local observations are ephemeral — rebuilt from the API and filesystem each cycle. (Architecture section 7.2.)
- **Planner must remain a pure function.** Signature: `([]PathChanges, *Baseline, SyncMode, SafetyConfig) -> *ActionPlan`. No I/O, no database access, deterministic. (Architecture section 3.5.)
- **One-shot mode keeps the sequential pipeline.** Only watch mode needs the concurrent architecture. (Roadmap section 265.)
- **Each drive is independent.** Own engine, own goroutine, own state DB. No cross-drive coordination needed. (PRD section 6.)
- **Crash recovery is idempotent.** Transfers that complete but aren't committed to baseline are safe: on next cycle, the planner sees them as "already transferred, no baseline change" and produces an update-synced action. (Architecture section 7.5.)
- **Delta token commits with baseline in the same transaction.** Prevents token-advancement-without-execution crash bug. (Architecture decision E3.)
- **Ephemeral execution context per operation.** Immutable `ExecutorConfig` + ephemeral `Executor` via `NewExecution()`. No temporal coupling. (LEARNINGS B-079.)
- **Upload lifecycle encapsulated in `graph.Client.Upload()`.** Consumers call one method; the provider handles simple-vs-chunked routing, session lifecycle, and cleanup. (LEARNINGS post-B-075.)
- **`io.ReaderAt` for retry-safe uploads.** `SectionReader`s from the same file handle make retries safe without re-opening the file. (LEARNINGS.)
- **Cache-through baseline loading.** `BaselineManager.Load()` returns cached baseline. `Commit()` invalidates and reloads. (LEARNINGS.)

## Alternatives Considered and Eliminated

### Concurrent download + upload phases (minimal change)

Run downloads and uploads as separate `errgroup`s in parallel instead of sequentially. Folder creates and deletes stay sequential.

Fixes problem 1 (downloads don't block uploads). Doesn't fix 2, 3, 4, or 5. Low effort, low risk, but insufficient as a long-term architecture. The phase barrier model remains — all folder creates before any transfer, all transfers before any delete. Incremental commits and crash recovery are not addressed.

### Incremental per-transfer commits (bolt-on)

After each successful transfer, immediately commit that outcome to baseline. Next sync re-observes and the planner skips already-synced files.

Fixes problem 3 (interrupted sync preserves progress). Doesn't fix 1, 2, 4, or 5. The delta token can't be saved until all transfers complete, creating a split between "baseline updated" and "delta token saved." Worth incorporating into the final design, but insufficient alone.

### Micro-batch execution with re-observation

Split the action plan into batches of N actions. Execute a batch, commit, re-observe, re-plan, execute next batch.

Fixes problems 3 and 4. But repeated delta fetches hit the API hard, repeated full local scans are expensive for large trees, and re-planning can produce inconsistent decisions if remote state changed between batches. Highest cost, highest risk, weakest improvement.

### Priority queue with small-files-first scheduling

Replace flat `[]Action` with a priority queue sorted by size. Small files first for quick wins.

Fixes problem 2 when combined with incremental commits. But a simple priority sort fails for fairness: a trillion small files completely starve large transfers (they never reach the front of the queue). Conversely, "large files first" starves small changes. Useful as a scheduling optimization within a broader architecture, but not sufficient as the architecture itself.

### Optimistic execution with retry

No pre-ordering. Throw every action into a worker pool. If an upload fails because the parent folder doesn't exist, retry it later. Every action must be perfectly idempotent.

Violates criterion 19 (invariant simplicity). Correctness depends on every action being safe to retry including all side effects, which is fragile for moves and deletes. A move that's retried after partial completion could leave files in unexpected locations. Wasted work on retries. Non-deterministic ordering makes debugging hard.

### Level-parallel topological sort

Topologically sort the action DAG into levels. All actions at the same level run fully concurrently. Wait for each level to complete before starting the next.

Middle ground between phases and full DAG. Typically produces 3-5 levels instead of 9 phases. But still has barriers — a slow transfer at level N blocks all of level N+1, even if those level N+1 actions don't depend on it. Doesn't solve criterion 7 (incremental progress). Per-level commit is an improvement over per-cycle commit, but still coarse-grained.

### Reactive event bus (pub/sub)

Actions subscribe to completion events of their dependencies. When all required events arrive, the action executes. Completion publishes a new event.

Converges to the actor model (Architecture B below) with more indirection and ceremony. Event ordering guarantees need care. Risk of event storms when one folder create triggers hundreds of uploads simultaneously. No advantage over direct channel-based coordination.

### Two-phase scaffold (structural then data)

Split into two phases: scaffold (folder creates, moves, empty-folder deletes — fast, sequential) and transfer (downloads, uploads, file deletes — slow, fully concurrent with per-action commits).

Pragmatic 80/20 solution. Fixes the biggest pain points with minimal structural change. But still has one barrier (scaffold must complete before any transfer starts). If a folder create is slow (API throttle), it blocks all transfers. Doesn't solve crash recovery for the scaffold phase. Doesn't foreclose migration to a better architecture later, but also doesn't build toward one.

## Front-Runner Architectures

After filtering through all criteria, three architectures survive as genuinely viable long-term designs. Others either violate a Tier 1 constraint or converge into one of these.

### Architecture A: DAG Executor with Persistent Ledger

**Core idea.** The planner emits a dependency graph, not a flat list. The graph is persisted to a SQLite `action_queue` table. A worker pool pulls actions whose dependencies are satisfied via SQL queries. Each completed action is committed to baseline immediately. The delta token is committed separately, only after all actions from that cycle are done.

```
Planner --> ActionDAG --> SQLite ledger (action_queue table)
                               |
                     +---------+---------+
                     |   Ready Scanner   |
                     | (find actions     |
                     |  with all deps    |
                     |  in "done" state) |
                     +---------+---------+
                               |
                  +------------+------------+
                  v            v            v
              Worker 1     Worker 2     Worker N
                  |            |            |
                  +------------+------------+
                               v
                      Per-action commit
                      (baseline upsert +
                       mark action done)
```

**Dependency model.** Each action has explicit dependency edges:

```
mkdir /A         -> []              (no deps)
mkdir /A/B       -> [mkdir /A]      (parent must exist)
upload /A/B/f.tx -> [mkdir /A/B]    (parent must exist)
download /C/x.pd -> []              (parent /C/ in baseline)
delete /old/y.tx -> []              (independent)
rmdir /old       -> [delete /old/*] (children first)
move /X -> /A/Z  -> [mkdir /A]      (target parent must exist)
```

**Persistence schema:**

```sql
CREATE TABLE action_queue (
    id           INTEGER PRIMARY KEY,
    cycle_id     TEXT NOT NULL,
    action_type  TEXT NOT NULL,
    path         TEXT NOT NULL,
    old_path     TEXT,
    status       TEXT DEFAULT 'pending',  -- pending/claimed/done/failed
    depends_on   TEXT,                    -- JSON array of action IDs
    item_id      TEXT,
    parent_id    TEXT,
    hash         TEXT,
    size         INTEGER,
    mtime        INTEGER,
    session_url  TEXT,                    -- upload resume (B-085)
    bytes_done   INTEGER DEFAULT 0,       -- progress + resume
    claimed_at   INTEGER,
    completed_at INTEGER,
    error_msg    TEXT
);
```

**Worker scheduling via SQL.** Workers poll for ready actions:

```sql
SELECT * FROM action_queue
WHERE status = 'pending'
  AND NOT EXISTS (
    SELECT 1 FROM action_queue d
    WHERE d.id IN (SELECT value FROM json_each(a.depends_on))
      AND d.status != 'done'
  )
ORDER BY priority, size ASC
LIMIT 1
```

Then atomically claim:

```sql
UPDATE action_queue
SET status = 'claimed', claimed_at = ?
WHERE id = ? AND status = 'pending'
```

**Criterion-by-criterion assessment:**

| # | Criterion | Assessment |
|---|-----------|------------|
| 1 | Data safety | Same execution functions, same S3/S4/S7 guards. Unchanged from current model. |
| 2 | Crash recovery | Best. Read ledger on restart. Pending/claimed actions re-execute. Done actions already in baseline. Upload resume via stored session URL. |
| 3 | Ordering | DB query: worker only picks actions where all deps are in `done` state. Correct by construction. |
| 4 | Delta token | Committed when all actions with matching `cycle_id` reach `done`. Cycle-aware token management. |
| 5 | Parallelism | Maximum. Any action whose deps are met runs immediately. No phase barriers. |
| 6 | Watch mode | Observers feed new events through planner, which appends new actions to ledger with new cycle_id. Workers drain old and new concurrently. |
| 7 | Incremental | Each completed action immediately commits outcome to baseline. |
| 8 | Idle efficiency | Workers sleep on empty ready set. Scanner is a SQL query or channel-based notify. But polling adds CPU overhead unless a notification mechanism is built on top. |
| 9 | Worker fairness | SQL queries with lane filtering (WHERE size < threshold). Works but has polling latency and claim contention. Multiple workers competing for the same rows serialize through SQLite's single-writer model. |
| 10 | Bandwidth | Same as all architectures — transport-layer rate limiter. Not a differentiator. |
| 11 | Adaptive concurrency | Adjust number of workers polling. Awkward — "stop a goroutine that's mid-SQL-poll" requires cancellation. More practical: semaphore gating the claim step, resize semaphore capacity. |
| 12 | Graceful shutdown | Mark in-flight actions as `claimed` in ledger. On restart, reclaim after timeout. Clean. |
| 13 | Action cancellation | Mark row `canceled` in ledger. But the in-flight worker doesn't know — it's doing I/O, not watching the ledger. Need a separate context cancellation mechanism in addition to the ledger update. Two mechanisms for one operation. |
| 14 | Backpressure | Natural. Ledger on disk, unlimited. Workers pull at their own pace. Planner appends freely. No memory pressure from large queues. |
| 15 | Progress reporting | `UPDATE action_queue SET bytes_done = ? WHERE id = ?` during transfer. Separate goroutine queries aggregates. SQLite WAL means reads don't block writes. Works well. |
| 16 | Testability | Strong. DAG construction is a pure function (planner). Ready scanner is a SQL query. Workers are the same transfer functions. All testable in isolation. |
| 17 | Debuggability | Best. `SELECT * FROM action_queue WHERE status != 'done'`. Full audit trail survives process exit. Post-mortem analysis with standard SQLite tools. |
| 18 | Extensibility | Best. Priority: add `priority` column + ORDER BY. Bandwidth: semaphore before dispatch. Progress: `bytes_done` column. Pause: stop claiming. Resume: `session_url` column. All natural extensions of the SQL model. |
| 19 | Invariant count | Medium. Four main invariants: (a) dependency graph is acyclic, (b) claimed actions are reclaimed after timeout, (c) baseline commits are atomic per-action, (d) delta token committed only when cycle complete. |

**Risks:**

- **Stale claims.** Worker crashes mid-action, leaving it `claimed` forever. Needs a reaper goroutine (timeout + reclaim to `pending`). Must set the timeout long enough that normal transfers don't get reclaimed mid-flight.
- **DAG construction.** Planner must emit explicit dependency edges. Currently it just sorts arrays. Non-trivial refactor of planner output, but the dependency information is already implicit in the ordering logic.
- **Two tables in play.** `action_queue` and `baseline` both written during execution. If same DB: transaction can span both tables (simpler, but sole-writer pattern means no concurrent writes). If separate DBs: need to reason about consistency between them (baseline committed but ledger not updated, or vice versa).
- **Delta token lag.** If 100 actions from cycle 1 are pending and cycle 2 adds 50 more, cycle 1's token can't advance until all 100 complete, even if cycle 2 finishes first. Need cycle-aware token commit logic.
- **Polling latency.** When action X completes and unblocks action Y, no instant notification. Worker must poll again to discover Y is ready. Mitigated by shorter poll interval, but that means constant SQL queries even when idle (violates criterion 8). Mitigated by a notify channel after each completion, but that's building an in-memory notification system on top of the ledger — which converges toward the hybrid.
- **Claim contention.** Multiple workers polling the same table, competing for the same rows. SQLite's single-writer model serializes claim attempts. With 16 workers polling frequently, this becomes a throughput bottleneck for small fast actions.

### Architecture B: Actor Per Path with Persistent Journal

**Core idea.** Each changed path gets a goroutine (actor). The actor knows its dependencies via channels from parent actors. When all dependency channels close (signaling completion), the actor executes its action. A write-ahead journal records intent before execution and completion after, enabling crash recovery.

```
Planner --> []Action + dependency channels
               |
     +---------+----------+---------+
     v         v          v         v
   Actor     Actor      Actor     Actor
   /A/       /A/B/      /A/B/f   /C/x.pdf
   mkdir     mkdir      upload   download
     |         |          |
     |    wait(ch_A)  wait(ch_AB)   (no wait)
     |         |          |
   create   create     upload     download
     |         |          |          |
   close    close      close      close
   ch_A     ch_AB     (done)     (done)
     |         |          |          |
     +---------+----------+----------+
               v
       Journal: append "done /path"
       Baseline: commit per action
```

**Concurrency control via semaphore:**

```go
sem := make(chan struct{}, maxWorkers)

go func(action Action, deps []<-chan struct{}) {
    for _, dep := range deps {
        <-dep // block until all dependencies complete
    }
    sem <- struct{}{} // acquire worker slot
    defer func() { <-sem }()

    outcome := execute(action)
    commitToBaseline(outcome)
    close(done) // signal dependents
}()
```

**Journal format (append-only):**

```
INTENT  cycle=1  action=mkdir   path=/A
INTENT  cycle=1  action=upload  path=/A/B/f.txt
DONE    cycle=1  action=mkdir   path=/A           item_id=abc123
DONE    cycle=1  action=upload  path=/A/B/f.txt   item_id=def456
```

On crash recovery: scan journal. INTENT without matching DONE = needs re-execution.

**Criterion-by-criterion assessment:**

| # | Criterion | Assessment |
|---|-----------|------------|
| 1 | Data safety | Same execution functions, same S3/S4/S7 guards. |
| 2 | Crash recovery | Good. Journal replay finds incomplete actions. But journal is harder to query than a table — "show me all pending" requires computing INTENT minus DONE by scanning the entire journal. |
| 3 | Ordering | Channel-based. Actor blocks until all dependency channels close. Compile-time guarantee — can't execute without receiving from channel. |
| 4 | Delta token | A cycle supervisor goroutine waits on all actors' done channels, then commits token. |
| 5 | Parallelism | Maximum. Goroutines unblock the instant deps complete. Zero scheduling overhead. |
| 6 | Watch mode | New cycle spawns new actors. Old actors still draining. No conflict if they operate on different paths. |
| 7 | Incremental | Each actor commits its outcome to baseline on completion. |
| 8 | Idle efficiency | Excellent. Goroutines blocked on channels = zero CPU. Go runtime handles this natively. |
| 9 | Worker fairness | Weakest. Goroutines don't have priority. Need per-lane semaphores, each actor must know its lane. Awkward to implement. |
| 10 | Bandwidth | Same — transport-layer rate limiter. |
| 11 | Adaptive concurrency | Must resize semaphore (buffered channel). Can't shrink a buffered channel in Go — need a different primitive (e.g., `golang.org/x/sync/semaphore`). Non-trivial. |
| 12 | Graceful shutdown | Cancel context. Actors in-flight see cancellation. Journal records INTENT without DONE. On restart, replay. |
| 13 | Action cancellation | Cancel the actor's context. Clean for a single actor, but wiring cancellation across actor dependency chains is complex — canceling a parent shouldn't necessarily cancel all children. |
| 14 | Backpressure | Disqualifying. 1 million goroutines at ~4 KB stack each = 4 GB. A large initial sync OOMs. Must batch actor creation, which defeats the model. |
| 15 | Progress reporting | Needs a side channel. Actors report to a progress channel. Extra wiring per actor. |
| 16 | Testability | Medium. Dependency graph construction is pure. But channel-wiring logic is harder to unit test than SQL queries or in-memory data structure methods. |
| 17 | Debuggability | Worst. No central state to inspect after process exit. 10,000 goroutines blocked on channels in production — which one is stuck? Stack traces show the block point but not the dependency chain. |
| 18 | Extensibility | Medium. Priority: launch high-priority actors first. But shared semaphore doesn't differentiate lanes. Upload resume: awkward — journal is append-only, not queryable for session URLs. |
| 19 | Invariant count | Low (2 main invariants): dependency channels wired correctly (planner responsibility), journal append is crash-safe. Simplest correctness argument. |

**Risks:**

- **Goroutine leak.** If a dependency actor panics or hangs, all dependents hang forever. Needs timeout + context cancellation per actor.
- **Memory explosion.** One goroutine per action. 1M actions = 4 GB of stack memory. Disqualifying for large initial syncs.
- **Journal limitations.** Append-only log harder to query than a table. Finding pending actions requires full scan. Grows unbounded without compaction. No natural place to store upload session URLs for resume.
- **Debugging at scale.** 10,000 goroutines in production. Requires structured logging and debug endpoints to determine what's stuck.
- **Cross-cycle coordination.** If cycle 1 is uploading `/A/f.txt` and cycle 2 wants to upload a newer version, actors need cycle-awareness. Channel wiring across cycles is complex.

**Assessment.** Architecture B is eliminated by criteria 9 (worker fairness) and 14 (backpressure/memory). A million-file initial sync OOMs. The actor model is elegant for small action sets but doesn't scale.

### Architecture C: Streaming Pipeline with In-Memory Dependency Tracker

**Core idea.** Observers stream events continuously. A dependency tracker maintains a live in-memory graph of pending actions. When an action's dependencies are met, it's dispatched to a worker pool via a ready channel. Completed actions update baseline incrementally.

```
Remote Observer --(events)--> +
                              +---> Change Buffer ---> Planner ---> Dep Tracker ---> Worker Pool
Local Observer  --(events)--> +      (2s debounce)     (pure)    (live graph)       (N workers)
                                                                       ^                |
                                                                       |    outcome     |
                                                                       +----------------+
                                                                    (completed action
                                                                     unlocks dependents)
                                                                          |
                                                                          v
                                                                   Baseline Commit
                                                                   (per-action)
```

**Dependency tracker — the core data structure:**

```go
type DepTracker struct {
    mu      sync.Mutex
    actions map[ActionID]*trackedAction
    ready   chan *trackedAction
}

type trackedAction struct {
    action   Action
    deps     []ActionID
    depsLeft int32 // atomic counter
    status   actionStatus
}

func (dt *DepTracker) Add(action Action, deps []ActionID) {
    // add to map, compute initial depsLeft
    // if depsLeft == 0: send to ready channel
}

func (dt *DepTracker) Complete(id ActionID) {
    // mark done, decrement depsLeft of all dependents
    // if any dependent reaches depsLeft == 0: send to ready channel
}
```

**Criterion-by-criterion assessment:**

| # | Criterion | Assessment |
|---|-----------|------------|
| 1 | Data safety | Same execution functions, same S3/S4/S7 guards. |
| 2 | Crash recovery | Adequate. Tracker is ephemeral — in-memory only. On restart: reload baseline, re-observe, re-plan. Planner sees committed outcomes and skips them (idempotent). Slower than ledger-based resume — requires full delta re-fetch. No upload session resume without separate persistence. |
| 3 | Ordering | Atomic counter in tracker. Action dispatched to ready channel only when all deps complete. |
| 4 | Delta token | Tracker tracks cycle IDs. Token committed when all actions for a cycle reach `done`. |
| 5 | Parallelism | Maximum. Ready channel feeds workers immediately when deps are satisfied. |
| 6 | Watch mode | Best fit. Observers always running. New events flow through buffer, planner, tracker continuously. No cycle boundary needed (but debounce batches for efficiency). |
| 7 | Incremental | Per-action baseline commit on completion. |
| 8 | Idle efficiency | Excellent. Workers block on ready channel. Observers block on FS events or poll timer. Tracker is passive — woken by Add/Complete calls. Zero CPU when idle. |
| 9 | Worker fairness | Good. Multiple ready channels per lane. Tracker routes actions to the correct lane on dispatch. Natural fit for lane-based scheduling. |
| 10 | Bandwidth | Same — transport-layer rate limiter. |
| 11 | Adaptive concurrency | Clean. Controller adjusts semaphore capacity. Tracker, controller, and workers share memory — no SQL round-trips for control decisions. |
| 12 | Graceful shutdown | Cancel context. In-flight actions see cancellation. Tracker state is lost, but baseline reflects all committed outcomes. |
| 13 | Action cancellation | Clean. Tracker holds a `context.CancelFunc` per in-flight action. `tracker.Cancel(path)` cancels the context directly. One mechanism, not two. |
| 14 | Backpressure | Problematic. If 1M actions are loaded into the tracker, at ~500 bytes each that's 500 MB — exceeds memory budget. Needs bounded tracker size. |
| 15 | Progress reporting | In-memory snapshot: `tracker.Snapshot()` returns counts and bytes by status and lane. Zero DB overhead for live display. |
| 16 | Testability | Strong. Dep tracker is a standalone data structure, testable in isolation with no I/O. Planner still pure. Workers still testable. |
| 17 | Debuggability | Good during runtime via `tracker.Snapshot()`. But ephemeral — state lost after process exit. No post-mortem analysis. |
| 18 | Extensibility | Good. Priority: priority queue instead of FIFO for ready channel. Bandwidth: semaphore before dispatch. Progress: tracker knows bytes in-flight. |
| 19 | Invariant count | Medium (4 invariants): (a) deps acyclic, (b) Add/Complete safe under concurrent access, (c) planner doesn't duplicate actions already in tracker, (d) baseline reads during planning reflect committed state. |

**Risks:**

- **Planner re-entry.** When observers detect new changes mid-execution, the planner runs again on the current baseline (with incremental commits) plus new events. It must not re-emit actions already in the tracker. Either: (a) tracker deduplicates by path, or (b) planner queries tracker state (breaks pure-function constraint). Option (a) is preferred.
- **Cross-batch conflicts.** Batch 1 says "upload /A/f.txt" and while the upload runs, batch 2 says "download /A/f.txt" (remote changed). Tracker or planner needs cross-batch conflict detection.
- **No persistence.** Tracker is in-memory. Crash recovery relies entirely on baseline + re-observation. Full delta re-fetch after crash can be slow for large change sets. Upload resume (B-085) requires separate persistence — which reinvents the ledger.
- **Memory pressure.** 1M pending actions at ~500 bytes = 500 MB. Needs bounded tracker with external spillover.
- **Tracker complexity.** Concurrent graph data structure with add/complete/cancel operations. Locking must be correct. A bug here affects every action.

## Recommended Architecture: A+C Hybrid (Persistent Ledger + In-Memory Tracker)

The recommended design combines Architecture A's persistent ledger with Architecture C's in-memory dependency tracker. The ledger provides durability and resume. The tracker provides scheduling and dispatch. They serve different concerns with different performance characteristics.

### Design Overview

```
On startup:
  Ledger (SQLite) --load pending/claimed--> Dep Tracker (in-memory, bounded)

During execution:
  Planner --> new actions:
    1. Write to ledger (status=pending)
    2. Add to tracker (if capacity available; otherwise, spillover stays in ledger)

  Tracker --> ready channel --> Workers
    On action start:
      1. Update ledger (status=claimed)
    On action completion:
      2. Commit baseline (SQLite tx)
      3. Update ledger (status=done) [same tx as baseline commit]
      4. Signal tracker: Complete(actionID) --> unblocks dependents

  Refill loop:
    When tracker has capacity, pull more pending actions from ledger

On crash + restart:
  Ledger (SQLite) --load pending/claimed--> Dep Tracker (in-memory)
  (pending actions become ready; claimed actions reclaimed after timeout)
```

### Why Two Layers

The ledger is for durability, the tracker is for scheduling. These are different concerns:

- **Durability** needs disk persistence, crash recovery, audit trail, upload session URLs, bytes-transferred tracking. SQLite excels at this.
- **Scheduling** needs instant dispatch when deps are satisfied, zero polling latency, lane-based fairness, adaptive concurrency, action cancellation via context. In-memory data structures excel at this.

Trying to make SQLite serve both (pure Architecture A) means either accepting polling latency and claim contention, or building an in-memory notification layer on top — which is the tracker. Better to design for two layers from the start than to bolt the second layer on later.

### Worker Fairness: Lane-Based Scheduling

The tracker routes ready actions to lanes. Each lane has reserved workers plus access to a shared overflow pool:

```
Total workers: N (e.g., 16)

Lane: interactive (files < 10 MB, folder ops, deletes, moves)
  Reserved workers: 2 minimum (always available for small ops)

Lane: bulk (files >= 10 MB)
  Reserved workers: 2 minimum (always available for large transfers)

Shared pool: N-4 workers
  Assigned dynamically to whichever lane has work
  Interactive lane has priority for shared workers
```

Guarantees:
- If all N workers are doing 10 GB uploads, 2 workers are reserved for the interactive lane. A small file change gets picked up immediately.
- If a trillion small files flood the interactive lane, 2 workers are reserved for the bulk lane. Large transfers keep making progress.
- When one lane is empty, its reserved workers plus all shared workers serve the other lane.

Implementation in the tracker:

```go
type DepTracker struct {
    mu          sync.Mutex
    actions     map[ActionID]*trackedAction
    interactive chan *trackedAction  // small files, folder ops, deletes
    bulk        chan *trackedAction  // large files
}

func (dt *DepTracker) dispatch(ta *trackedAction) {
    if ta.action.Size < sizeThreshold || !ta.action.IsTransfer() {
        dt.interactive <- ta
    } else {
        dt.bulk <- ta
    }
}
```

Workers assigned to lanes:

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

The size threshold between interactive and bulk could be fixed (10 MB) or adaptive (p50 of recent action sizes).

### Bandwidth Limiting

Transport-layer concern, orthogonal to architecture. Standard token-bucket rate limiter wrapping the HTTP transport:

```go
type throttledTransport struct {
    base       http.RoundTripper
    uploadBw   *rate.Limiter  // e.g., 10 MB/s
    downloadBw *rate.Limiter  // e.g., 50 MB/s
}
```

Each `Read()` or `Write()` on the HTTP body acquires tokens from the limiter. Workers naturally share the bandwidth budget — 8 workers doing downloads share the download bandwidth limit. Scheduled bandwidth (time-of-day rules) adjusts the limiter's rate on a timer. Per-direction and combined limits supported by separate or shared limiters.

### Adaptive Concurrency

AIMD (additive increase, multiplicative decrease) applied to worker pool size, similar to TCP congestion control:

| Signal | Response |
|--------|----------|
| High 429 rate from Graph API | Multiplicative decrease: halve active workers |
| Low error rate + high throughput | Additive increase: add one worker |
| Many small files in queue | Increase workers (parallelism helps throughput) |
| Few large files in queue | Decrease workers (bandwidth contention hurts) |
| High latency per action | Hold steady or decrease (network saturated) |

The controller adjusts a semaphore that gates worker dispatch from the tracker's ready channels:

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

This is clean in the hybrid because the controller, tracker, and workers share memory. No SQL round-trips for control decisions.

### Action Cancellation

The tracker holds a `context.CancelFunc` per in-flight action:

```go
type trackedAction struct {
    action   Action
    cancel   context.CancelFunc  // set when worker claims the action
    deps     []ActionID
    depsLeft int32
    status   actionStatus
}

func (dt *DepTracker) Cancel(path string) {
    if ta, ok := dt.byPath[path]; ok && ta.status == claimed {
        ta.cancel()           // cancel the worker's context immediately
        ta.status = canceled
        // ledger row also updated to canceled
    }
}
```

When the observer detects a new change to a file currently being uploaded, the planner or a pre-planner dedup step tells the tracker to cancel the stale action. The worker's context is canceled, the upload aborts, and the replacement action takes its place. One mechanism, not two.

### Backpressure: Bounded Tracker with Ledger Spillover

The tracker holds a bounded working window. The ledger holds the full queue on disk:

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

// Background goroutine: refill tracker from ledger when capacity opens
func (dt *DepTracker) refillLoop(db *sql.DB) {
    for range dt.capacitySignal {
        rows := queryPendingFromLedger(db, maxTrackerSize - len(dt.actions))
        for _, row := range rows {
            dt.addFromLedger(row)
        }
    }
}
```

Memory usage with bounded tracker:
- Tracker: 10,000 actions * ~500 bytes = 5 MB (bounded)
- Ledger: on disk, unlimited
- Baseline: same as today
- Total overhead: ~5 MB, well within the 100 MB budget even for 100K synced files

A million-file initial sync stores 1M rows in the ledger (disk), loads 10K at a time into the tracker (memory), and workers drain them at full speed. No memory explosion.

### Progress Reporting

The tracker provides real-time progress with zero DB overhead:

```go
func (dt *DepTracker) Progress() ProgressSnapshot {
    dt.mu.RLock()
    defer dt.mu.RUnlock()
    // count by status, sum bytes by lane, compute rates
    return ProgressSnapshot{
        Downloading:    dt.countByStatusAndType(claimed, download),
        Uploading:      dt.countByStatusAndType(claimed, upload),
        Waiting:        dt.countByStatus(pending),
        BytesInFlight:  dt.sumBytesInFlight(),
        // ...
    }
}
```

The ledger provides durable history for post-mortem analysis:

```sql
SELECT action_type, status, COUNT(*), SUM(bytes_done)
FROM action_queue
WHERE cycle_id = ?
GROUP BY action_type, status
```

Both available — live display from tracker, audit trail from ledger.

### Graceful Shutdown

On Ctrl-C (context cancellation):
1. Stop accepting new actions from planner.
2. Cancel all in-flight worker contexts.
3. Workers detect cancellation, stop transfers, return partial outcomes.
4. In-flight actions remain `claimed` in ledger.
5. On restart: reload ledger, reclaim stale actions (claimed_at older than timeout), resume.

For uploads: the session URL is stored in the ledger. On restart, query the upload session and continue from `bytes_done`. For downloads: the `.partial` file's size indicates how far the download got. Resume via HTTP `Range` header.

### Delta Token Management

The delta token is cycle-scoped. Each planning pass produces actions tagged with a `cycle_id`. The token for that cycle is committed only when all actions with that cycle_id reach `done`:

```go
func (cm *CycleManager) OnActionComplete(cycleID string) {
    remaining := countPendingForCycle(cycleID)
    if remaining == 0 {
        commitDeltaToken(cycleID, token)
    }
}
```

Multiple cycles can overlap. Cycle 2's token might commit before cycle 1's if cycle 2 has fewer/faster actions. Each cycle's token is independent.

If the process crashes with cycle 1 half-done, the delta token for cycle 1 is not saved. On restart, the same delta is re-fetched. The planner sees that some actions from that delta are already in baseline (committed incrementally) and skips them. The remaining actions are re-planned and re-executed. Idempotent by construction.

### Commit Transaction Boundary

Each completed action commits in a single SQLite transaction that spans both tables:

```sql
BEGIN;
  -- Update baseline (same as today's commitUpsert/commitDelete/commitMove)
  INSERT OR REPLACE INTO baseline (...) VALUES (...);
  -- Mark action done in ledger
  UPDATE action_queue SET status = 'done', completed_at = ? WHERE id = ?;
COMMIT;
```

If the transaction fails, neither the baseline nor the ledger is updated. The action remains `claimed` and will be retried. If the process crashes after the transaction commits but before the tracker is notified, the restart loads the ledger and sees the action is `done` — no duplicate execution.

Both tables must be in the same SQLite database for transactional atomicity. This is compatible with the existing sole-writer pattern (`SetMaxOpenConns(1)`) since baseline writes are already serialized.

## Head-to-Head Comparison

| Criterion | A: DAG + Ledger | B: Actor + Journal | C: Streaming + Tracker | A+C Hybrid (Recommended) |
|---|---|---|---|---|
| 1. Data safety | Same | Same | Same | Same |
| 2. Crash recovery | Best (exact resume) | Good (journal replay) | Adequate (re-observe) | Best (ledger resume) |
| 3. Ordering | DB query | Channel close | Atomic counter | Atomic counter + ledger backup |
| 4. Delta token | Cycle tracking in ledger | Supervisor goroutine | Cycle ID in tracker | Cycle tracking in both |
| 5. Parallelism | Maximum | Maximum | Maximum | Maximum |
| 6. Watch mode | Good | Good | Best | Best (continuous pipeline) |
| 7. Incremental progress | Excellent | Good | Good | Excellent (ledger + baseline) |
| 8. Idle efficiency | Needs polling or notify | Excellent | Excellent | Excellent (channel block) |
| 9. Worker fairness | SQL lane queries | Weakest | Good (lane channels) | Good (lane channels) |
| 10. Bandwidth | Same | Same | Same | Same |
| 11. Adaptive concurrency | Awkward | Non-trivial | Clean | Clean |
| 12. Graceful shutdown | Clean | Clean | Adequate | Clean (ledger preserves state) |
| 13. Action cancellation | Two mechanisms | Complex chains | Clean (one mechanism) | Clean (one mechanism) |
| 14. Backpressure | Natural (disk) | Fails (4 GB goroutines) | Needs bounding | Natural (bounded tracker + disk spillover) |
| 15. Progress reporting | SQL queries | Side channel | In-memory snapshot | Both (live + durable) |
| 16. Testability | Strong | Medium | Strong | Strong |
| 17. Debuggability | Best (survives exit) | Worst | Good (runtime only) | Best (runtime + durable) |
| 18. Extensibility | Best | Medium | Good | Best |
| 19. Invariant count | Medium (4) | Low (2) | Medium (4) | Medium-high (5: add tracker-ledger consistency) |
| Upload resume (B-085) | Natural | Awkward | Needs persistence | Natural (session_url in ledger) |
| Memory (1M actions) | ~0 MB | 4 GB (disqualifying) | 500 MB (needs bounding) | ~5 MB (bounded tracker + disk) |

## Open Questions

1. **Tracker-ledger consistency.** The tracker is a cached view of the ledger. What if they diverge? The invariant is: the tracker is always a subset of the ledger's non-done actions. Refill only adds; Complete/Cancel always update both. Startup always rebuilds tracker from ledger. This should be formally verified in tests.

2. **Same DB or separate DB for ledger?** Same DB allows transactional atomicity between baseline and ledger (recommended). Separate DB provides isolation but requires two-phase reasoning about consistency.

3. **Planner re-entry and deduplication.** When observers detect new changes mid-execution, the planner runs again. It must not re-emit actions already in the tracker or ledger. Options: (a) tracker deduplicates by path (preferred — preserves planner purity), (b) planner queries tracker state (breaks pure-function constraint).

4. **Reclaim timeout for stale claims.** Must be longer than the longest expected transfer. Configurable, with a sane default (e.g., 30 minutes). Reaper goroutine checks periodically.

5. **Should one-shot sync also use the ledger?** For resume on Ctrl-C and upload session resume, yes. The overhead is minimal (write actions to ledger before executing). One-shot could use the same infrastructure but skip the continuous observer loop.

6. **Size threshold for interactive vs. bulk lanes.** Fixed at 10 MB, or adaptive based on recent action sizes? Fixed is simpler and predictable. Adaptive handles edge cases (sync of all large files, sync of all tiny files) but adds complexity.

7. **Refill batch size and frequency.** How many actions to pull from ledger into tracker per refill? Too few = starved workers. Too many = approaches memory bound. A reasonable default: refill to 80% of maxTrackerSize when tracker drops below 50%.
