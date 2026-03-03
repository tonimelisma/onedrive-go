# Remote State Separation

## The Problem

A bidirectional sync engine has two independent data flows. Each starts with an observation, requires an action to reconcile, and ends with the baseline recording success. The fundamental question is: what happens when the action fails?

### Remote → Local (downloads)

```
Observation          Action              Baseline
───────────          ──────              ────────
Delta API tells us   Download the file   Record "local now
"file X has hash R"  to local disk       matches remote"
```

**The observation is ephemeral.** We learn about remote changes by calling the delta API. The API returns changed items and a new cursor token. Once we advance the token, the API will not re-tell us about those items. The knowledge exists only as a `ChangeEvent` struct flowing through the buffer and planner.

If the download succeeds: `CommitOutcome` updates the baseline. Done.

If the download fails: the `ChangeEvent` is gone. `CommitOutcome` is a no-op for failures. The baseline has no record of the remote change. The only way to re-learn about it is to re-poll the delta API with the old token.

**This is where the current design breaks.** The delta token advances (see [failures.md](failures.md) for the full bug analysis), so the API won't re-deliver the item. The knowledge is permanently lost. No local trace exists — the file simply wasn't downloaded, and neither the baseline nor the filesystem records that anything is missing.

### Local → Remote (uploads)

```
Observation          Action              Baseline
───────────          ──────              ────────
Filesystem has       Upload the file     Record "remote now
"file X with hash L" to OneDrive        matches local"
```

**The observation is inherent.** The local filesystem IS the durable record. We can re-observe it at any time — via inotify, the safety scan (every 5 minutes), or by the planner comparing local files against baseline on every cycle.

If the upload fails: the file is still on disk. The planner compares local file hash against baseline on the next cycle. They still differ. The planner regenerates the upload action. Natural infinite retry with no special mechanism.

### The fundamental asymmetry

| | Remote → Local | Local → Remote |
|---|---|---|
| **Source of truth** | Graph API (external) | Local filesystem (internal) |
| **Observation durability** | Ephemeral — API won't re-tell us | Inherent — file persists on disk |
| **Re-observation on failure** | Requires token replay OR persistent storage | Free — re-reads filesystem every cycle |
| **Current retry mechanism** | None (token bug loses the item) | Natural (planner regenerates) |

Uploads work because the planner reconciles from **state** (filesystem vs baseline). Downloads fail because the planner reconciles from **events** (ephemeral ChangeEvents). The fix is to give downloads the same property: reconcile from persistent state, not ephemeral events.

### Three conflated concerns

The sync engine conflates three things that should be independent:

| Concern | Meaning | When it should update | Where it currently lives |
|---------|---------|----------------------|--------------------------|
| **Observation cursor** | "What have we been told about?" | After every successful API poll | In-memory token + DB token (conflated with sync success) |
| **Remote knowledge** | "What does the remote look like?" | When we learn about a change | Nowhere persistent. Ephemeral ChangeEvent structs |
| **Synced state** | "What have we successfully synced?" | On action success | `baseline` table |

The delta token currently means both "we've been told about everything up to here" AND "we've synced everything up to here." It should mean only the first. This conflation is the root cause of the delta token advancement bug, the need for the in-memory failure tracker, and the entire "persistent failure tracker" design that was being considered.

### How the in-memory failure tracker makes it worse

The in-memory failure tracker (`failure_tracker.go`) was introduced to prevent "delta token starvation" — a permanently-failing item blocking the token forever. It suppresses items after 3 failures, excluding them from the plan. But this *accelerates* item loss: suppressed items don't count as cycle failures, so cycles containing them "succeed," committing the token past the suppressed items even within the same cycle. The tracker was solving a real problem (starvation) but creating a worse one (faster item loss). See [failures.md §The failure tracker makes it worse](failures.md) for the detailed mechanism.

### What this design does NOT solve

This document addresses the download/remote-change side of failure handling. Two upload-side gaps remain:

1. **No upload backoff.** A permanently-failing upload (e.g., invalid SharePoint filename) is retried every cycle with no delay — wasting one worker's time per cycle. The planner regenerates the action because the file still differs from baseline. There's no mechanism to back off.
2. **No upload failure visibility.** The user has no way to see that a local file can't be uploaded. The `failures` CLI command (described below) queries `remote_state`, which uploads don't touch.

These are pre-existing gaps. Addressing them requires either extending `remote_state` to track upload failures (adds complexity for something the natural retry already handles) or a separate upload-failure tracking mechanism. Both are deferred — the download-side data loss is the critical bug.

## Industry Context

Every production sync engine separates "knowing about a change" from "having applied it."

**Dropbox (Nucleus)** maintains three separate state stores: Sync File Journal (what the remote looks like), local state (filesystem), and synced state (what was reconciled). The cursor tracks position in the SFJ — observation only. Processing failures do not affect the cursor.

**abraunegg** (OneDrive Linux client) uses a sequential model: poll → process all → commit token. Token only committed after the entire cycle completes. If processing fails, the token stays stale and the next run re-fetches everything. Correct but slow — one permanently-failing item blocks everything.

**Official OneDrive client** uses an event-driven queue with per-item metadata in a `.dat` file. Internal state tracks per-item sync status independently of the cursor.

**Google Drive** uses a sequential page token model similar to abraunegg.

Our engine is the only one that ties cursor advancement to sync success, which creates the two-token problem and the cascading bugs around it.

## The Solution

### Overview

Add a `remote_state` table that records what we've observed from delta, independently of whether we've synced it. The delta token becomes a pure API cursor that always advances. A dedicated reconciler goroutine watches for unreconciled items and retries them with exponential backoff.

Three principles:
1. **Record what we learn immediately.** When delta says "file X has hash R," write that to `remote_state` before attempting any action.
2. **Always advance the token.** The token is an API cursor — "don't re-send me stuff I already know about." Decouple it completely from sync success.
3. **Reconcile from state, not events.** Failed items persist in `remote_state`. The reconciler turns them back into pipeline events. No item is ever lost.

### Alternatives considered

Four alternative approaches were evaluated:

**A. Never advance the token until all items succeed (abraunegg model).** Correct but slow. One permanently-failing item (bad filename, revoked permission) blocks all new change discovery indefinitely. This is "delta token starvation" — the very problem the in-memory failure tracker was introduced to solve. The tracker's solution (suppress the item so the cycle "succeeds") then causes item loss. Starvation and item loss are two sides of the same coin when the token conflates cursor position with sync success.

**B. Periodically do full resyncs.** A full delta response with an empty token returns every item in the drive — potentially thousands. Reasonable as a safety net (e.g., every 24 hours) or as a last resort after 410 Gone, but too expensive as the primary recovery mechanism. It also doesn't help during the 5-minute window between a failure and the next full resync — items are still lost in the interim.

**C. Accept the data loss (current behavior).** Items lost from delta are recovered only if: (1) modified again remotely, (2) the daemon restarts within the 5-minute window before the token advances (almost never), or (3) the 90-day token expiration triggers a full resync. For enterprise SharePoint environments with frequent 423 locks, this means silently stale files with no user indication.

**D. Persistent failure tracker with separate `failure_records` table.** Track failures independently, allow the token to advance, and retry from the failure table. This was the previous design (see `.claude/plans/witty-doodling-whale.md`). It works, but duplicates information: the failure record stores the same path/hash/drive_id that we'd need in `remote_state`. The failure tracker becomes a shadow copy of "what the remote looks like but we haven't synced." Persisting the observation directly in `remote_state` is simpler — the failure state (count, next_retry) lives on the observation row itself. No separate table, no shadow copy, one source of truth for "what does the remote look like."

**E. Persist observed remote state (this design).** The Dropbox model. Records what we learn when we learn it. The delta token is a pure API cursor that always advances. Failed items persist as state discrepancies between `remote_state` and `baseline`. A reconciler retries them. This is the only approach that provides both continuous sync (no starvation) and data integrity (no loss) without duplicating state.

We chose E.

### Addressing the original objections

The `event-driven-rationale.md` (Part 1.1, Alternative B) previously rejected a multi-table approach for three reasons:

1. **"Multiple tables means multiple writers competing for SQLite."** Solved: a single concrete type (`SyncStore`) owns all database access, exposing capability-restricted sub-interfaces to each caller. See [Database access pattern](#database-access-pattern).

2. **"Dry-run has side effects because observation writes to the database."** Solved: `remote_state` writes are gated on `!opts.DryRun`. The observer produces in-memory events for dry-run reports but persists nothing. See [Dry-run behavior](#dry-run-behavior).

3. **"Event-driven keeps observations ephemeral."** This is the root cause of the bug. Ephemeral observations + advancing token = permanent item loss. The fix is to make remote observations persistent.

The original Alternative B had *both* observers writing state (remote AND local), creating two concurrent writers. This design only persists the remote side — local state is inherently persistent in the filesystem. The planner stays pure: it receives synthesized ChangeEvents through the existing buffer, never reading from the database directly.

### Updated design axiom

The old axiom: "The sync database stores confirmed synced state and nothing else."

The new axiom: **"The database stores confirmed synced state (baseline) and observed remote state (remote_state). Local state is inherently persistent in the filesystem and is not stored. A single concrete type (`SyncStore`) owns all database access, exposing capability-restricted sub-interfaces to each caller."**

A corollary: the delta token is an API cursor, not a sync cursor. It means "don't re-send me stuff I already know about." It does not mean "I've synced everything up to here."

## Schema

### The remote_state table

```sql
CREATE TABLE IF NOT EXISTS remote_state (
    path            TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash            TEXT,
    size            INTEGER,
    mtime           INTEGER,
    etag            TEXT,
    is_deleted      INTEGER NOT NULL DEFAULT 0,
    observed_at     INTEGER NOT NULL CHECK(observed_at > 0),
    failure_count   INTEGER NOT NULL DEFAULT 0,
    next_retry_at   INTEGER,
    last_error      TEXT,
    http_status     INTEGER
);
```

### Column design rationale

**`path` as primary key.** Paths are globally unique within the sync root. Even with shared drive shortcuts (where items have a different `drive_id`), the path prefix distinguishes them (e.g., `SharedFolder1/file.txt` vs `SharedFolder2/file.txt`). No `scope_id` column is needed.

**`hash` is nullable.** Folders don't have hashes. The conditional delete in `CommitOutcome` uses SQLite's `IS` operator (`WHERE hash IS ?`) for NULL-safe comparison — `NULL IS NULL` returns `TRUE`.

**`is_deleted` for deletions and moves.** When delta reports a deletion, we write `{path: X, is_deleted: 1}`. For moves (X → Y), the observer writes two rows: `{path: Y, is_deleted: 0}` for the new location and `{path: X, is_deleted: 1}` for the old. This ensures the reconciler sees both sides of a move. Cleanup: when a local delete succeeds, `CommitOutcome` deletes the `is_deleted` row via the same conditional delete (`WHERE path = ? AND hash IS NULL` — deleted items have NULL hash, and `NULL IS NULL` returns TRUE).

**`failure_count` and `next_retry_at` for backoff.** These track retry state directly on the observation row. No separate failure tracking table — when the action succeeds and the row is deleted, the failure history goes with it. A fresh delta observation (INSERT OR REPLACE) resets `failure_count` to 0, giving the item a fresh start if the remote file has changed.

*Alternative considered: preserve failure_count across fresh observations.* If the same path keeps failing with the same error (e.g., 403), resetting to 0 on each fresh delta event means the item goes through the fast-retry phase (1-2 attempts at ~5s) before backing off again. This wastes 2 retries every 5 minutes (the delta poll interval). We accept this: the cost is negligible (2 seconds of work every 5 minutes) and the benefit is real — if the file *has* changed (new version, permissions fixed), we don't want to wait through a long backoff.

**`last_error` and `http_status` for user visibility.** These are metadata for the `failures` CLI command. When the user runs `onedrive-go failures`, showing "HTTP 423: locked" is far more useful than "5 failures." These columns do not affect retry logic.

**No additional indexes needed** beyond the primary key. The reconciler's bootstrap query joins `remote_state` (typically 0-100 rows in steady state) against `baseline` (potentially 100K rows) via both PKs. SQLite's optimizer starts from the smaller table. The `next_retry_at` column is queried by the reconciler at startup, but with <100 rows, a full scan is faster than an index lookup.

### What "unreconciled" means

A `remote_state` row is **unreconciled** when the corresponding `baseline` row either doesn't exist or has a different hash. This is the definition used throughout the document:

```sql
SELECT rs.*
FROM remote_state rs
LEFT JOIN baseline b ON rs.path = b.path
WHERE rs.is_deleted = 0
  AND (b.remote_hash IS NULL OR b.remote_hash != rs.hash)
```

For deletion rows (`is_deleted = 1`), "unreconciled" means the baseline row still exists (the local deletion hasn't been committed yet):

```sql
SELECT rs.*
FROM remote_state rs
INNER JOIN baseline b ON rs.path = b.path
WHERE rs.is_deleted = 1
```

The state discrepancy between `remote_state` and `baseline` IS the failure record. No separate tracking needed.

### Migration strategy

There are zero current users. All existing migrations (00001-00005) are deleted. A single `00001_initial_schema.sql` creates the final schema: `baseline`, `delta_tokens`, `conflicts`, `sync_metadata`, `remote_state`. Test environments with old databases will fail with a schema mismatch — delete the `.db` file and re-sync.

*Why not keep existing migrations and add 00006?* With zero users, there's no upgrade path to maintain. A single migration is easier to read, and the migration tooling (goose) is simpler when there's only one file. If we had users, we'd add an incremental migration instead.

## Architecture

### How the pieces fit together

```
RemoteObserver                    LocalObserver
  │ (polls delta API)                │ (inotify + safety scan)
  │                                  │
  ├─ CommitObservation()             │
  │  (writes remote_state +          │
  │   advances delta token           │
  │   via SyncStore)                 │
  │                                  │
  ├─ sends ChangeEvents ─────────────┤──→ Buffer ──→ Planner ──→ Workers
  │                                  │                              │
  │                                  │                         ┌────┴────┐
  │                                  │                      success   failure
  │                                  │                         │         │
  │                                  │                  CommitOutcome  increment
  │                                  │                  (baseline +   failure_count
  │                                  │                   delete from  on remote_state
  │                                  │                   remote_state)     │
  │                                  │                         │         │
  Reconciler ◄─────────────────────────────────────────────────┴─────────┘
  │ (dedicated goroutine)                              NotifySuccess / NotifyFailure
  │
  ├─ On startup: bootstrap from remote_state
  ├─ On failure: schedule retry (immediate or backoff)
  ├─ On success: cancel pending retry
  ├─ When retry fires: synthesize ChangeEvent → Buffer → normal pipeline
  └─ On shutdown: cancel all pending timers
```

### Database access pattern

#### The problem with "sole writer"

The current codebase has a `BaselineManager` type that owns all database methods. The design doc previously described this as a "sole-writer pattern" — implying a single goroutine writes to the database. But that's not what happens. Today, multiple goroutines already call `BaselineManager` write methods concurrently:

- **Worker goroutines** (N concurrent) call `CommitOutcome()` — one per completed action
- **Engine goroutine** calls `CommitDeltaToken()` — at cycle completion
- **Daemon goroutine** calls `PruneResolvedConflicts()` — periodic maintenance

This design adds more concurrent writers:

- **RemoteObserver goroutine** calls `CommitObservation()` — after each delta poll
- **drainWorkerResults goroutine** calls `RecordFailure()` — on each action failure

Correctness comes from SQLite WAL mode with a busy timeout, which serializes concurrent write transactions at the database level. The "sole writer" label was describing *API encapsulation* (one type contains all write methods), not *concurrency* (one goroutine does all writes). The distinction matters: someone reading "sole writer" and seeing concurrent goroutine calls would be confused.

#### Sub-interfaces by capability (Option D)

Instead of one type with ~23 methods accessed by everyone, we define sub-interfaces grouped by *who calls them*. A single concrete type (`SyncStore`, renamed from `BaselineManager` to reflect its broader scope) implements all interfaces. Each caller receives only the interface it needs.

Five alternatives were considered:

**A. Status quo + documentation fix.** Keep one type, clarify that "sole writer" means "sole module" not "single goroutine." Rejected: `BaselineManager` grows to ~23 methods — a god object. Every component depends on the full type. No compile-time restriction on who can write what.

**B. Split by table ownership.** Three types, each owning one table (`BaselineStore`, `RemoteStateStore`, `DeltaTokenStore`). Rejected: some operations genuinely need cross-table atomicity (`CommitOutcome` writes `baseline` + deletes `remote_state`; `CommitObservation` writes `remote_state` + `delta_tokens`). A coordinator type that holds all three stores becomes the new god object.

**C. Split by read vs. write.** `SyncStateReader` for all queries, `SyncStateWriter` for all mutations. Rejected: splits related operations. `CommitOutcome` (write) and "is this item in baseline?" (read) are conceptually coupled but live in different types. Harder to reason about invariants.

**D. Sub-interfaces on one concrete type (chosen).** One implementation type, five interfaces grouped by caller identity. Callers receive the narrowest interface they need. Cross-table transactions stay in one place. The type system enforces capability restriction at compile time.

**E. Event-sourced single-writer goroutine.** All state changes flow through a channel to a single writer goroutine. True single-writer — no concurrent DB access. Rejected: adds latency to every write (channel hop), requires back-pressure design, complicates shutdown/drain, and the write volume (~tens of items per 5-minute cycle) doesn't justify the complexity. SQLite WAL handles this trivially.

We chose D.

```go
// ObservationWriter — called by RemoteObserver goroutine (single caller).
// Writes observed remote state and advances the delta token atomically.
type ObservationWriter interface {
    CommitObservation(ctx context.Context, events []ChangeEvent, newToken string, driveID string) error
    GetDeltaToken(ctx context.Context, driveID, scopeID string) (string, error)
}

// OutcomeWriter — called by worker goroutines (N concurrent callers).
// Commits action results to baseline and cleans up remote_state on success.
type OutcomeWriter interface {
    CommitOutcome(ctx context.Context, outcome *Outcome) error
    Load(ctx context.Context) (*Baseline, error)
}

// FailureRecorder — called by drainWorkerResults goroutine (single caller).
// Records failure metadata on remote_state rows and checks known-bad status.
type FailureRecorder interface {
    RecordFailure(ctx context.Context, path, errMsg string, httpStatus int) error
    IsKnownBad(ctx context.Context, path string) (bool, error)
}

// StateReader — called by reconciler, planner, status, CLI (read-only).
// All methods are pure reads. Multiple goroutines call concurrently.
// WAL mode guarantees readers never block (they read from a consistent snapshot).
type StateReader interface {
    ListUnreconciled(ctx context.Context) ([]RemoteStateRow, error)
    FailureCount(ctx context.Context) (int, error)
    BaselineEntryCount(ctx context.Context) (int, error)
    UnresolvedConflictCount(ctx context.Context) (int, error)
    ReadSyncMetadata(ctx context.Context) (map[string]string, error)
    CheckCacheConsistency(ctx context.Context) (int, error)
    ListConflicts(ctx context.Context) ([]ConflictRecord, error)
    ListAllConflicts(ctx context.Context) ([]ConflictRecord, error)
    GetConflict(ctx context.Context, idOrPath string) (*ConflictRecord, error)
}

// StateAdmin — called by CLI commands and daemon maintenance.
// Write operations that don't fit the hot path (user-initiated or periodic).
type StateAdmin interface {
    ResolveConflict(ctx context.Context, id, resolution string) error
    PruneResolvedConflicts(ctx context.Context, retention time.Duration) (int, error)
    WriteSyncMetadata(ctx context.Context, report *SyncReport) error
    ClearFailure(ctx context.Context, path string) error
    ClearAllFailures(ctx context.Context) error
}
```

The engine constructs `SyncStore` and distributes sub-interfaces:

| Component | Receives | Why |
|-----------|----------|-----|
| RemoteObserver | `ObservationWriter` | Writes observations + reads delta token |
| Worker pool | `OutcomeWriter` | Commits outcomes + reads baseline cache |
| drainWorkerResults | `FailureRecorder` | Records failures + checks known-bad status |
| Reconciler | `StateReader` | Reads unreconciled rows for retry scheduling |
| CLI commands | `StateReader` + `StateAdmin` | Reads for display + admin writes |
| Engine | all interfaces (constructs + distributes) | Orchestrator |

**The `DB()` escape hatch is removed.** Today, `BaselineManager.DB()` exposes raw `*sql.DB` so `status.go` can run ad-hoc queries. Under this design, those queries become methods on `StateReader`. No component receives raw database access.

#### Concurrency model

The sub-interfaces restrict *capability* — what each caller is allowed to do. They do not restrict *concurrency* — multiple goroutines still call write methods on the same underlying `SyncStore` concurrently.

Concurrency safety comes from SQLite WAL mode with a 5-second busy timeout (`_busy_timeout=5000`). Under WAL, readers never block — they read from a consistent snapshot. Writers serialize: if two goroutines call write methods simultaneously, one completes while the other waits (up to the busy timeout). With the system's write volume (~tens of items per 5-minute cycle), contention is negligible.

This is a deliberate choice. Application-level serialization (e.g., a single-writer goroutine with a channel, as in Option E) would eliminate contention entirely but adds latency and complexity. SQLite WAL is designed for exactly this workload: low-volume concurrent writes from a handful of goroutines in the same process.

#### CommitObservation details

`CommitObservation` is the key new write method on `SyncStore` (exposed via `ObservationWriter`):

```go
func (s *SyncStore) CommitObservation(ctx context.Context, events []ChangeEvent, newToken string, driveID string) error
```

This writes `remote_state` rows and the delta token in a single transaction. The `RemoteObserver` calls it after each successful `FullDelta()`, before sending events to the channel. The observer already depends on `*Baseline` for reads; adding an `ObservationWriter` interface is a minor extension.

**Why not have the observer write directly (bypassing SyncStore)?** Two independent types writing to the same database risks SQLITE_BUSY conflicts and makes it impossible to enforce invariants (e.g., "remote_state rows always have a corresponding delta token") across types. Routing all writes through one type keeps the SQL in one place.

**Why not have the engine mediate?** In watch mode, the observer runs in a goroutine sending events via channel. The engine would need to intercept events, batch them, and write to DB before forwarding — this changes the event pipeline and requires a side channel for batch boundaries. Unnecessary complexity.

**Why not batch across multiple delta polls?** Each poll returns a discrete set of events and a new token. Writing them in one transaction per poll is the natural boundary — it matches the atomicity guarantee we need (never advance the token without recording what we learned). Batching across polls would require tracking which events go with which token, adding complexity for no correctness benefit. The per-poll transaction is typically <10ms for a normal delta response (10-100 items). For initial sync (50K items), it's ~200-500ms — acceptable.

### The reconciler

The reconciler is a dedicated long-lived goroutine that schedules retries for failed remote-side actions. It closes the gap in the current architecture: "we know the remote changed, the action failed, and now nobody remembers."

#### Why a dedicated goroutine?

Five options were considered for the retry mechanism:

**A. Poll from the observer.** The RemoteObserver queries `remote_state` for unreconciled rows on each delta poll cycle and re-emits them as ChangeEvents. Rejected: observers observe the external world (Graph API, filesystem). Having one also read internal DB state mixes concerns. It also couples retry timing to the delta poll interval — if poll is every 5 minutes, retries happen at 5-minute intervals regardless of backoff schedule.

**B. Timer in the engine's select loop.** Add a `time.Ticker` to the engine's main `select` loop that periodically scans `remote_state` for retry-ready rows. Rejected: the engine's select loop handles buffer readiness and worker results — both are event-driven. A timer-based scan would be the first poll-based mechanism in the loop, breaking the pattern where each long-lived concern owns its goroutine. It also adds up to N minutes of latency to every failure (if the scan runs every 2 minutes, a failure waits up to 2 minutes before first retry) and wastes work scanning when `remote_state` is empty.

**C. Extend the local safety scan.** The local safety scan already walks the filesystem every 5 minutes. It could additionally query `remote_state` for unreconciled rows. Rejected: the safety scan detects *local* discrepancies (missed inotify events). Remote discrepancies are a different concern with different timing requirements (backoff schedule vs fixed 5-minute interval). Coupling them means you can't change one without affecting the other.

**D. Fire-and-forget per-item timers.** On failure, schedule a `time.AfterFunc` for the retry. No central goroutine. Rejected: no way to cancel on shutdown (goroutine leak), no way to bootstrap from DB on startup (who reads the DB?), no centralized logging or rate limiting, no way to skip in-flight items.

**E. Dedicated reconciler goroutine (chosen).** A single long-lived goroutine that receives failure/success notifications via method calls, maintains a map of pending `time.Timer`s, bootstraps from DB on startup, and synthesizes ChangeEvents when retries fire. This matches the architecture: each long-lived concern (delta polling, filesystem watching, reconciliation) owns its goroutine. The reconciler is reactive — it schedules retries in response to failure events and fires them at exactly the right time, not on a poll schedule.

We chose E.

#### Lifecycle

1. **Startup**: loads all unreconciled `remote_state` rows from DB, schedules retries at their `next_retry_at` (or immediately if `next_retry_at` is NULL or in the past)
2. **Failure notification**: engine calls `NotifyFailure(path, failureCount)`. Fresh failures (count < 3) are scheduled after ~5 seconds. Established failures schedule at `next_retry_at` from the backoff formula
3. **Success notification**: engine calls `NotifySuccess(path)`. Cancels any pending retry timer
4. **Retry fires**: re-reads the `remote_state` row from DB (a fresh delta may have updated it), skips if in-flight, synthesizes a ChangeEvent, feeds it into the buffer. Normal pipeline handles the rest
5. **Shutdown**: cancels all pending timers, goroutine exits

#### Go-level interface

```go
type Reconciler struct {
    state    StateReader           // read-only view of remote_state + baseline
    buf      *ChangeBuffer
    tracker  *InFlightTracker
    logger   *slog.Logger
    timers   map[string]*time.Timer // path → pending retry
    mu       sync.Mutex
    cancel   context.CancelFunc
}

func NewReconciler(state StateReader, buf *ChangeBuffer, tracker *InFlightTracker, logger *slog.Logger) *Reconciler

func (r *Reconciler) Start(ctx context.Context) error   // bootstrap + run
func (r *Reconciler) NotifyFailure(path string, failureCount int)
func (r *Reconciler) NotifySuccess(path string)
func (r *Reconciler) Stop()
```

The reconciler receives `StateReader` — a read-only interface. It cannot write to the database. It reads `remote_state` rows to synthesize `ChangeEvent`s and checks in-flight status via the tracker. All writes (failure recording, outcome commits) happen elsewhere in the pipeline.

The engine creates the reconciler and calls `Start()` in a goroutine. Worker results flow through `drainWorkerResults` which calls `NotifyFailure`/`NotifySuccess`. The `mu` mutex protects the `timers` map — notifications and timer callbacks can race.

#### Robustness

- **Crash recovery**: on startup, bootstraps from `remote_state`. Orphaned rows from before the crash are rescheduled. No items lost
- **Buffer full**: reconciler blocks on `buf.Add()`. Other retries accumulate and fire when it unblocks. No unbounded queue growth
- **Fresh delta event for a retrying item**: buffer deduplicates by path. The conditional delete on CommitOutcome ensures correctness regardless of which version was synced
- **Initial sync with many failures**: if 1000 items fail during initial sync, the reconciler schedules 1000 retries at ~5s. They fire in rapid succession but each goes through `buf.Add()` → debounce → planner, which naturally batches them. The workers process them at their concurrency limit (4 by default). No special rate limiting needed — the pipeline's existing backpressure handles it

#### Logging

- `slog.Info("reconciler started", "pending_retries", count)` — on bootstrap
- `slog.Info("reconciler: scheduling retry", "path", path, "failure_count", n, "retry_at", t)` — on failure notification
- `slog.Debug("reconciler: dispatching retry", "path", path)` — when a retry fires
- `slog.Info("reconciler: cleared", "path", path)` — on success notification
- `slog.Warn("reconciler: skipping in-flight item", "path", path)` — when retry fires but item is already being processed

### CommitOutcome expansion

On action success, `CommitOutcome` now also deletes the `remote_state` row in the same transaction as the baseline upsert:

```sql
DELETE FROM remote_state WHERE path = ? AND hash IS ?
```

The conditional delete (using `IS` for NULL-safe comparison) is critical. It prevents a successful download of version R1 from deleting a `remote_state` row that has already been updated to R2 by a newer delta poll. If the hashes don't match, the row persists and the reconciler picks it up. See [Scenario 4](#scenario-4-download-succeeds-for-r1-but-remote_state-already-has-r2).

For deletions (`is_deleted = 1`): the hash is NULL, so the conditional delete becomes `WHERE path = ? AND hash IS NULL`. This matches the `is_deleted` row correctly. If a fresh delta event has since re-created the file at the same path (with a non-NULL hash), the conditional delete won't match — the row persists and the new version is downloaded. Correct.

`CommitOutcome` already touches `baseline` and `conflicts`. Adding `remote_state` is a natural extension — it commits the outcome of an action, which includes updating all relevant state tables.

### What happens to cycleFailures

The `cycleFailures` map and `watchCycleCompletion` token-commit logic are deleted entirely. In the current design, these exist to gate token advancement on cycle success. With the token committed at observation time (in `CommitObservation`), there is no longer a concept of "cycle success/failure" controlling the token. Worker results go directly to `CommitOutcome` (on success) or failure-count increment + reconciler notification (on failure). The engine's main loop simplifies: it receives batches from the buffer, dispatches to workers, and processes results. No cycle bookkeeping.

## Data Flows

Eight data flows through the system. Each is traced step by step.

### Flow 1: Remote delta poll

```
RemoteObserver.Watch()
  │
  ├─ 1. Call FullDelta(ctx, currentToken)
  │     → Graph API returns: []ChangeEvent + newToken
  │
  ├─ 2. Call store.CommitObservation(ctx, events, newToken, driveID)
  │     → SyncStore performs:
  │       BEGIN TRANSACTION
  │       Write each ChangeEvent to remote_state table
  │         (INSERT OR REPLACE — path is primary key, latest state wins)
  │         For moves: also write is_deleted=1 row for old path
  │       Commit newToken to delta_tokens table
  │       COMMIT
  │     ← Atomic: if we crash between poll and commit, both are lost.
  │       The old token is replayed on restart, delta re-delivers the same items.
  │       remote_state is idempotent (same data rewritten). No data loss.
  │
  ├─ 3. Send each ChangeEvent to events channel (blocking send)
  │     → Bridge goroutine reads from channel, calls buf.Add()
  │
  ├─ 4. Buffer debounces (2-second window)
  │     → Produces []PathChanges on the ready channel
  │
  └─ 5. Engine main loop receives batch from ready channel
        → Calls processBatch()
```

The `remote_state` write and token commit are in the same transaction. This is the critical atomicity guarantee: we never advance the token without recording what we learned. If the daemon crashes after the transaction but before events reach the buffer, the events are lost from the pipeline but the `remote_state` rows persist. The reconciler picks them up on restart.

### Flow 2: Local filesystem change

```
inotify/fsevents
  │
  ├─ 1. LocalObserver.watchLoop receives filesystem event
  │     → Filters, coalesces rapid events (100ms window)
  │
  ├─ 2. Hashes the file → Produces ChangeEvent with Source=SourceLocal
  │
  ├─ 3. Sends to events channel → buf.Add()
  │
  └─ 4. Buffer debounces → planner → workers
```

No `remote_state` involvement. Local changes retry naturally because the planner compares filesystem against baseline every cycle.

### Flow 3: Local safety scan

```
Timer (every 5 minutes) in LocalObserver
  │
  ├─ 1. Walk entire local directory tree
  │     Compare each file against baseline (mtime, size, hash)
  │
  ├─ 2. Produce ChangeEvent for each discrepancy
  │
  └─ 3. Events → buffer → planner → workers
```

Catches local changes inotify missed. Does NOT detect remote discrepancies — that's the reconciler's job. The safety scan only sees the local filesystem. A failed download leaves no local trace — the file simply doesn't exist locally, and the baseline has no entry for it.

### Flow 4: Reconciler retry

```
Reconciler goroutine (dedicated, long-lived)
  │
  ├─ On startup (bootstrap):
  │     Query all unreconciled remote_state rows (see "What unreconciled means")
  │     Schedule retries at next_retry_at (or immediately if NULL/past)
  │
  ├─ On failure notification (from engine):
  │     Schedule retry: ~5s for fresh failures, next_retry_at for backed-off
  │
  ├─ On success notification (from engine):
  │     Cancel pending retry for this path
  │
  └─ When a scheduled retry fires:
        1. Re-read remote_state row from DB (may have been updated by fresh delta)
        2. Skip if in-flight (tracker.HasInFlight)
        3. Synthesize ChangeEvent from row
        4. Feed into buffer via buf.Add()
        5. Buffer debounces → planner → workers (normal pipeline)
```

### Flow 5: Action success

```
Worker completes action with Success=true
  │
  ├─ 1. store.CommitOutcome(outcome)
  │     → BEGIN TRANSACTION
  │     → Upsert baseline row (or delete for deletions, or move)
  │     → DELETE FROM remote_state WHERE path = ? AND hash IS ?
  │       (conditional — preserves row if a newer delta updated it)
  │     → COMMIT
  │
  ├─ 2. Update in-memory baseline cache
  │
  └─ 3. WorkerResult{Success: true} → drainWorkerResults
        → reconciler.NotifySuccess(path) — cancels pending retry
```

### Flow 6: Action failure

```
Worker completes action with Success=false
  │
  ├─ 1. CommitOutcome is a no-op (baseline unchanged, remote_state persists)
  │
  └─ 2. WorkerResult{Success: false, HTTPStatus: N, ErrMsg: "..."} → drainWorkerResults
        → UPDATE remote_state SET failure_count = failure_count + 1,
            next_retry_at = ?, last_error = ?, http_status = ?
            WHERE path = ?
        → reconciler.NotifyFailure(path, failureCount)
        → Log: slog.Warn("action failed", "path", path,
                 "failure_count", count, "http_status", status, "next_retry", t)
```

### Flow 7: Daemon startup / restart

```
Engine.RunOnce() or Engine.RunWatch()
  │
  ├─ 1. Read delta token from delta_tokens table
  │     → Always reflects latest successful poll (not sync success)
  │
  ├─ 2. Reconciler.Start() bootstraps from remote_state
  │     → Queries unreconciled rows (items from before crash)
  │     → Schedules retries per backoff state
  │
  ├─ 3. RemoteObserver.FullDelta(token)
  │     → Returns only changes SINCE the stored token
  │     → Previously-failed items are NOT in this response
  │
  ├─ 4. CommitObservation writes fresh delta events to remote_state
  │     → Fresh events overwrite stale rows (INSERT OR REPLACE)
  │
  └─ 5. Planner receives both fresh delta events and reconciler retries
        → Normal pipeline: planner → workers → outcomes
```

Previously-failed items are recovered from `remote_state`, not from delta replay. No items are ever lost, regardless of how many times the daemon crashes.

### Flow 8: WebSocket notification (future, Phase 8)

```
WebSocket push → trigger immediate delta poll (Flow 1)
```

WebSocket changes exactly one thing: the trigger for Flow 1. Everything downstream is unchanged.

## Retry and Backoff

### Tiered retry

The `failure_count` and `next_retry_at` columns on `remote_state` enable tiered retry:

| After N failures | Retry delay |
|------------------|-------------|
| 1-2 | ~5 seconds (immediate via reconciler) |
| 3 | 5 min |
| 4 | 10 min |
| 5 | 20 min |
| 6 | 40 min |
| 7+ | 1 hour (cap) |

Formula: `next_retry_at = last_failed_at + min(5min × 2^(failure_count - 3), 1 hour)`

Items below the threshold (1-2 failures) are likely transient (423 lock released, 5xx outage ended) and retry quickly. Items above the threshold back off exponentially, capping at 1 hour — following Syncthing's model.

### No error classification

The backoff curve is the same for all error types. The HTTP status and error message (`last_error`, `http_status` columns) are metadata for user visibility, not retry logic.

*Alternative considered: different backoff curves for "transient" vs "permanent" errors.* The persistent failure tracker design had an `error_class` column (transient/permanent/unknown) with different backoff schedules: 5min base for transient, 30min base for permanent. We rejected this because:
- Classification is unreliable. A 403 could be "permanent" (permission revoked) or "transient" (propagation delay after sharing). A 423 is always transient but the duration varies from seconds to hours.
- A uniform curve that's reasonable for both cases (5min base, 1hr cap) is simpler and correct enough. The worst case is retrying a truly permanent failure at 1-hour intervals — 1 second of wasted work per hour.
- User visibility (the `failures` CLI) is more valuable than classification. The user can see "HTTP 403: forbidden" and decide whether to fix permissions or wait.

### Fresh observations reset failure state

When a fresh delta event arrives for a path that has a `remote_state` row with high `failure_count`, `CommitObservation` overwrites the row (INSERT OR REPLACE). The new observation carries `failure_count = 0`. This is correct: a new delta event may carry a new file version where the old problem (lock, permission) is gone.

The cost of resetting: 2 extra retries at ~5 seconds each before the backoff kicks in again (if the problem persists). This costs 10 seconds every 5 minutes — negligible — and the benefit is immediate success if the problem was resolved.

### Rate limit impact

**Microsoft Graph rate limits**: ~10 req/s per user, 1,250 RU/min per app per tenant.

| Stuck items | Error type | Steady-state retry rate | Impact |
|-------------|-----------|------------------------|--------|
| 1 | 423 (skip) | 1 attempt/hr (~1s) | None |
| 10 | 423 (skip) | 10 attempts/hr (~10s total) | None |
| 10 | 5xx (full retry) | 10 attempts/hr (~20 min across 4 workers) | None (spread over 1 hour) |
| 100 | Mixed | ~100 attempts/hr at cap | Minor — <1% worker capacity |

With backoff, the retry rate converges regardless of how many items are stuck. 100 items at 1-hour cap = 100 seconds of work per hour across 4 workers. Negligible.

### Priority: real events vs retries

Real events naturally take priority:
1. Real events arrive via the buffer at any time and trigger an immediate planning pass (after 2-second debounce)
2. Reconciler events also enter the buffer via `buf.Add()`
3. If both arrive in the same debounce window, the buffer merges by path — the planner sees one PathChanges with the latest state
4. B-122 path dedup cancels in-flight reconciler actions when a real event arrives for the same path

### Permanently-failing items

Items that never self-heal (bad filename, permanent permission denial) are retried at the 1-hour cap. Each attempt costs ~1 second. At steady state, 10 such items cost 10 seconds per hour — 0.07% of one worker's capacity.

The mitigation is user visibility. The `failures` CLI command shows stuck items:

```
$ onedrive-go failures
PATH                      ERROR              STATUS  COUNT  NEXT RETRY
Documents/locked.xlsx     HTTP 423: locked   423     5      in 20 min
Photos/restricted.jpg     HTTP 403: forbidden 403    8      in 1 hour

$ onedrive-go failures --clear Documents/locked.xlsx
Cleared failure record for Documents/locked.xlsx

$ onedrive-go failures --clear --all
Cleared 2 failure records
```

The underlying query:

```sql
SELECT rs.path, rs.last_error, rs.http_status, rs.failure_count,
       rs.next_retry_at, rs.observed_at
FROM remote_state rs
LEFT JOIN baseline b ON rs.path = b.path
WHERE rs.is_deleted = 0
  AND (b.remote_hash IS NULL OR b.remote_hash != rs.hash)
ORDER BY rs.failure_count DESC
```

No separate failure tracking table needed. The state discrepancy IS the failure record.

### Dependency between items

Parent folder 403 → all children fail. The planner's DAG ordering handles this — children aren't dispatched until the parent succeeds. The failure only counts once (the parent). When the parent is backed off, children aren't wasted on retries.

## Error Handling

Every error that survives the two existing retry layers (graph client: 5 retries, executor: 3 retries) is handled uniformly by the state discrepancy mechanism. No error classification drives retry strategy. The two retry layers handle transient errors within a single attempt. The state discrepancy handles everything else:

| Error | Where state lives | Recovery |
|-------|------------------|----------|
| 423 Locked (download) | `remote_state` row persists | Self-heals when lock released. Reconciler retries |
| 403 Forbidden (download) | `remote_state` row persists | User must fix permissions. Visible in `failures` CLI |
| Persistent 5xx (download) | `remote_state` row persists | Self-heals when outage ends. Reconciler retries |
| FS permission (download) | `remote_state` row persists | User must fix local permissions. Visible in `failures` CLI |
| Non-empty directory delete | `remote_state` shows deletion | Needs separate handling (may become conflict) |
| 400 Bad Request (upload) | Local file differs from baseline | User must rename file. **Not visible in `failures` CLI** (upload-side gap) |
| 423 Locked (upload) | Local file differs from baseline | Self-heals when lock released. Natural planner retry |
| 403 Forbidden (upload) | Local file differs from baseline | User must fix permissions. **Not visible in `failures` CLI** (upload-side gap) |
| FS read errors (upload) | Local file differs from baseline | User must fix permissions. **Not visible in `failures` CLI** |

The upload-side gaps (marked above) are pre-existing limitations. See [What this design does NOT solve](#what-this-design-does-not-solve).

## Correctness

### Race: reconciler retry vs fresh delta event

The reconciler dispatches a retry for path X (hash R1). Meanwhile, a fresh delta poll delivers X with hash R2. Both enter the buffer in the same debounce window.

`buildPathViews` takes the LAST remote event. If the delta event was added after the reconciler event, it wins (correct). In the pathological case where the reconciler event wins, the planner plans a download for R1. It succeeds. `CommitOutcome` tries `DELETE WHERE hash IS R1` but `remote_state` has R2 — the conditional delete doesn't match. The row persists. The reconciler schedules another retry for R2.

**No special handling needed.** The conditional delete is the safety net that makes this correct regardless of event ordering.

### Crash recovery

**Old model**: "On crash, delta token hasn't advanced → re-fetch same delta → re-observe failed items." Works for RunOnce. Broken for RunWatch (token advances past failures within 5 minutes).

**New model**: "Delta token always advances. Failed items persist in `remote_state`. On restart, the reconciler bootstraps from DB and retries them."

**RunOnce edge case**: With the uniform code path, RunOnce commits the token at observation time. If the process crashes mid-execution, the token is committed but actions aren't complete. On restart, RunOnce queries unreconciled `remote_state` rows alongside the fresh delta poll, merging both into the planner. No items lost.

The new model is strictly better for both modes.

### Conflict scenarios

The planner's three-way comparison is unchanged. `PathView` has `Remote` (from delta or `remote_state`), `Local` (from filesystem), and `Baseline` (from synced state). The decision matrix (EF1-EF14) applies exactly as today. What changes: `Remote` can now come from a stored `remote_state` row, not just a fresh delta event.

#### Scenario 1: Simple remote edit, download fails, later succeeds

```
Time 0: Delta delivers file X with hash R_new.
        remote_state: {path: X, hash: R_new}
        baseline:     {path: X, remote_hash: R_old, local_hash: R_old}

        Planner: remoteChanged=true → EF2: download.
        Download fails (423 locked). failure_count → 1.

Time 1: Reconciler dispatches retry (~5s later).
        Same PathView. Same decision. Download succeeds.

        CommitOutcome: baseline updated. remote_state deleted.
        Reconciled.
```

#### Scenario 2: Remote edit fails, then user edits locally

```
Time 0: Delta delivers X with hash R_new. Download fails.
        remote_state: {path: X, hash: R_new}

Time 1: User edits X locally to hash L_new. inotify fires.

Time 2: Reconciler retry + inotify event merge in buffer.
        Remote=R_new, Local=L_new, Baseline=R_old.
        remoteChanged=true, localChanged=true, L_new ≠ R_new
        → EF5: edit-edit conflict.

        Conflict copy preserves local edit. Remote version downloaded.
        No data loss.
```

#### Scenario 3: Remote edit fails, remote edits again

```
Time 0: Delta delivers X with hash R1. Download fails.
Time 1: Delta delivers X with hash R2.
        remote_state updated to {hash: R2, failure_count: 0}.
        New action planned for R2. R1 is superseded — correct.
```

#### Scenario 4: Download succeeds for R1 but remote_state already has R2

```
Time 0: Delta delivers X with hash R1. Download planned.
Time 1: Delta delivers X with hash R2. remote_state updated to R2.
Time 2: Download of R1 completes.
        CommitOutcome: baseline=R1.
        DELETE WHERE hash IS R1 → R1 ≠ R2 → row NOT deleted.
Time 3: Reconciler retries. Downloads R2. Fully reconciled.
```

The conditional delete prevents losing knowledge of R2.

#### Scenario 5: Upload fails, then remote changes the same file

```
Time 0: User edits X locally to hash L. Upload fails (423).
        No remote_state involvement (uploads are local-driven).

Time 1: Someone edits X remotely to hash R. Delta fires.
        remote_state: {hash: R}.

Time 2: Remote=R, Local=L, Baseline=B.
        remoteChanged=true, localChanged=true → EF5: conflict.

        Without remote_state, the remote change would have been lost.
        With remote_state, the conflict is correctly detected.
```

#### Scenario 6: Remote delete fails, user creates new file at same path

```
Time 0: Delta delivers delete for X. remote_state: {is_deleted: 1}.
        Local delete fails (permission denied).

Time 1: User creates new file at X with hash L_new.

Time 2: Remote=deleted, Local=L_new, Baseline=exists.
        remoteDeleted=true, localChanged=true → EF9: edit-delete conflict.
        User's file preserved as conflict copy. Correct.
```

#### Scenario 7: Remote move fails, then file is edited

```
Time 0: Delta delivers move X → Y.
        remote_state: {path: Y, ...} + {path: X, is_deleted: 1}.
        Local move fails.

Time 1: User edits local file at X.

Time 2: Reconciler dispatches retries.
        Path X: Remote=deleted, Local=modified → EF9 edit-delete conflict.
        Path Y: Remote=exists, Local=absent → EF11 download.
        Both versions preserved. No data loss.
```

#### Scenario 8: Multiple failures compound, then resolve

```
Time 0: Delta delivers A, B, C, D, E. A, B succeed. C, D, E fail.
Time 1: Delta delivers F, G. Token advanced. F, G succeed.
Time 2: Reconciler retries C, D, E (~5s). C succeeds. D, E fail (count → 2).
Time 3: Reconciler retries D, E (~5s). Fail again. count → 3. Backoff: 5 min.
Time 4: Delta delivers new version of D. remote_state reset to count=0.
Time 5: D retried immediately. Succeeds. E backoff expires. Succeeds.
Final: everything synced. Token advanced freely throughout.
```

#### Scenario 9: inotify missed a local edit

```
Time 0: Reconciler retries X. Planner derives Local from baseline. Plans download.
Time 1: User edits X locally. inotify hasn't fired yet.
Time 2: Worker starts download. S4 safety check: hash local file.
        Expected=R_old, actual=L_new. Mismatch! → conflict copy.
        Remote version downloaded. No data loss.
```

The S4 safety check (`executor_transfer.go`) protects against any race between planning and execution, regardless of event source.

#### Scenario 10: Remote delete succeeds

```
Time 0: Delta delivers delete for X. remote_state: {path: X, is_deleted: 1, hash: NULL}.
        Planner: EF3 remote-only delete. Local file deleted.

Time 1: CommitOutcome: baseline row deleted.
        DELETE FROM remote_state WHERE path = X AND hash IS NULL
        → NULL IS NULL → TRUE → row deleted.
        Reconciled. Clean state.
```

## Compatibility

### Dry-run behavior

During `sync --dry-run`, the observer runs and produces in-memory ChangeEvents (needed for the report), but `CommitObservation` is not called — no `remote_state` writes, no token advancement. This matches rclone's precedent and user expectation: "dry-run is read-only." The dry-run gate already exists at `engine.go:266`.

### RunOnce (one-shot sync)

RunOnce uses the same code path as RunWatch: writes `remote_state` at observation time, processes events, deletes `remote_state` rows on success. The table is empty before and after a successful one-shot sync.

On crash recovery, RunOnce queries unreconciled `remote_state` rows at startup, merging them with the fresh delta poll. One code path for both modes — no conditional logic.

### WebSocket (Phase 8)

WebSocket notifications trigger immediate delta polls (Flow 1). Everything downstream — `remote_state` write, token commit, buffer, planner, workers, reconciler — is unchanged.

### Continuous sync

Events arrive at any rate. `remote_state` gets updated to the latest observed values (Graph coalesces intermediate states). Multiple planning passes can overlap. Path dedup (B-122) cancels stale in-flight actions. No cycles needed for correctness — cycles are a batching optimization.

### Initial sync

On initial sync (empty token), FullDelta returns all remote items — potentially tens of thousands. All are written to `remote_state` in one transaction (~200-500ms for 50K items with WAL mode, ~10 MB temporary disk). As actions succeed, rows are deleted. After a successful initial sync, the table is empty. No special case needed.

### Status command

The `status` command gains a failure count by querying `remote_state`:

```sql
SELECT COUNT(*) FROM remote_state rs
LEFT JOIN baseline b ON rs.path = b.path
WHERE rs.is_deleted = 0
  AND (b.remote_hash IS NULL OR b.remote_hash != rs.hash)
```

Displayed as "X items pending sync" alongside existing "Y unresolved conflicts."

## Implementation Plan

### What gets deleted

| Component | Reason |
|-----------|--------|
| `failure_tracker.go` | Replaced by `remote_state` + reconciler |
| `failure_tracker_test.go` | Tests for deleted component |
| `cycleFailures` map in engine | Token decoupled from sync success |
| `watchCycleCompletion` token-commit logic | Token committed at observation time |
| All existing migrations (00001-00005) | Replaced by single clean schema |

### What gets added

| Component | Purpose |
|-----------|---------|
| `00001_initial_schema.sql` | Single migration with all tables including `remote_state` |
| `sync_store.go` | `SyncStore` concrete type with sub-interfaces: `ObservationWriter`, `OutcomeWriter`, `FailureRecorder`, `StateReader`, `StateAdmin` |
| `reconciler.go` | Dedicated retry scheduler goroutine (receives `StateReader`) |
| `failures.go` (root package) | CLI command: list, clear, JSON output |

### What gets renamed

| Old | New | Reason |
|-----|-----|--------|
| `BaselineManager` | `SyncStore` | Manages all sync state (baseline + remote_state + conflicts + tokens), not just baseline |
| `baseline.go` | `sync_store.go` | File name matches type |
| `baseline_test.go` | `sync_store_test.go` | File name matches type |

### What gets modified

| Component | Change |
|-----------|--------|
| `engine.go:RunWatch` | Remove failure tracker. Create and start reconciler goroutine. Distribute sub-interfaces |
| `engine.go:RunOnce` | Reconciler bootstraps from `remote_state` at startup |
| `engine.go:processBatch` | Remove `shouldSkip` suppression logic entirely |
| `engine.go:drainWorkerResults` | Receives `FailureRecorder`. On failure: `RecordFailure()` + notify reconciler. On success: notify reconciler |
| `engine.go:watchCycleCompletion` | Remove entirely (no cycle-based token gating) |
| `worker.go:WorkerResult` | Add `HTTPStatus int` and `ErrorClass string` fields for structured error info |
| `worker.go:WorkerPool` | Receives `OutcomeWriter` instead of `*BaselineManager` |
| `observer_remote.go:Watch` | Receives `ObservationWriter`. Call `CommitObservation()` after delta poll, before sending events |
| `sync_store.go:CommitOutcome` | Add conditional `DELETE FROM remote_state WHERE path = ? AND hash IS ?` in same transaction |
| `status.go` | Receives `StateReader`. Add failure count to output |
| `root.go` | Register `failures` command |

### Documents to update

| Document | Change |
|----------|--------|
| `data-model.md` | Update axiom to include `remote_state`. Add table to schema. Rename `BaselineManager` → `SyncStore` |
| `concurrent-execution.md` | Update "baseline is only durable per-item state". Document sub-interface distribution |
| `event-driven-rationale.md` | Update Alternative B notes: adopted for remote side with sub-interface solution for original objections |
| `LEARNINGS.md` | Add: "delta token is API cursor, not sync cursor" and "in-memory failure suppression + delta token advancement = silent item loss" |
| `CLAUDE.md` | Add `failures` to CLI command list. Rename `BaselineManager` → `SyncStore` in architecture diagram |

### Testing strategy

**Layer 1: Unit tests** — real SQLite (in-memory), same pattern as `baseline_test.go`.
- `CommitObservation`: write N events + token, verify rows and token
- `CommitObservation` with existing rows: INSERT OR REPLACE overwrites stale data, resets `failure_count`
- `CommitOutcome` with `remote_state` delete: hash match → deleted, hash mismatch → persists, NULL hash (folders) → `IS` works, `is_deleted` row → `NULL IS NULL` works
- Reconciler bootstrap query: correct rows returned, respecting `next_retry_at` and ordering
- `failure_count` increment: updates `next_retry_at`, `last_error`, `http_status`
- `failure_count` reset: row deleted on success via `CommitOutcome`
- Reconciler event synthesis: correct ChangeEvents, skips in-flight, respects backoff

**Layer 2: Integration tests** — real SQLite, mock Graph API.
- Full cycle: observe → fail → reconcile → succeed
- Crash recovery: write + "crash" + restart → reconciler bootstraps from DB
- Conditional delete race (Scenario 4): R1 succeeds but R2 persists
- Backoff escalation: 3+ failures → `next_retry_at` set → respected → success clears
- Reconciler + fresh delta interaction: fresh delta resets failure_count

**Layer 3: Engine tests** — full pipeline with mocks.
- Token always advances regardless of failures
- Reconciler schedules retries correctly (timing, backoff)
- Dry-run: no DB writes
- Planner works with reconciler-sourced events
- `cycleFailures` and `watchCycleCompletion` are gone — verify no regressions

**Layer 4: E2E tests** — live OneDrive, `e2e_full` tag.
- Reconciliation recovery: upload file, make download fail, fix, verify recovery
- `failures` CLI: list, clear, JSON output
- `status` command shows pending sync count

**Layer 5: Property-based tests** — optional, high value.
- Invariant: for any sequence of operations, every remote item is either in `baseline` (synced) or `remote_state` (pending)

## Relationship to Existing Documents

This design supersedes:
- The persistent failure tracker plan (`.claude/plans/witty-doodling-whale.md`)
- The in-memory failure tracker (`failure_tracker.go`, `failure_tracker_test.go`)
- The delta token hold-back logic in `watchCycleCompletion`
- The `cycleFailures` map and cycle success/failure concept
- The `BaselineManager` name and monolithic API surface
- The "baseline is the only durable per-item state" axiom
- The "database stores confirmed synced state and nothing else" axiom

It builds on:
- [failures.md](failures.md) — failure enumeration, delta token bug analysis, retry layer behavior
- [sync-algorithm.md](sync-algorithm.md) — planner decision matrix (unchanged)
- [event-driven-rationale.md](event-driven-rationale.md) — architectural decisions (extended — Alternative B adopted for remote side with sub-interface solution for original objections)
