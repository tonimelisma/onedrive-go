# Remote State Separation — Architectural Design

> **Scope**: Design document only. No code changes. This document is the single
> self-contained reference for the remote state separation architecture —
> incorporating all analysis, risk assessment, and decisions that previously
> lived in separate review files.

## Table of Contents

1. [Founding Principle](#1-founding-principle)
2. [The Problem](#2-the-problem)
3. [Industry Comparison](#3-industry-comparison)
4. [Architectural Decisions](#4-architectural-decisions)
5. [Full Remote Mirror](#5-full-remote-mirror)
6. [ID-Based Primary Key](#6-id-based-primary-key)
7. [Explicit State Machine](#7-explicit-state-machine)
8. [Data Flows](#8-data-flows)
9. [Conflict Scenarios](#9-conflict-scenarios)
10. [Concurrency Safety Model](#10-concurrency-safety-model)
11. [CommitObservation Logic](#11-commitobservation-logic)
12. [Database Access Pattern](#12-database-access-pattern)
13. [Unified Filtering Architecture](#13-unified-filtering-architecture)
14. [Upload Failure Tracking](#14-upload-failure-tracking)
15. [Consolidated Schema](#15-consolidated-schema)
16. [Single-Timer Reconciler](#16-single-timer-reconciler)
17. [Error Handling](#17-error-handling)
18. [Compatibility](#18-compatibility)
19. [Crash Recovery](#19-crash-recovery)
20. [Critical Design Invariants](#20-critical-design-invariants)
21. [Risk Analysis](#21-risk-analysis)
22. [Assessment Against Existing Architecture](#22-assessment-against-existing-architecture)
23. [Tier 1 Research Alignment](#23-tier-1-research-alignment)
24. [Testing Strategy](#24-testing-strategy)
25. [Migration Path](#25-migration-path)
26. [Verdict](#26-verdict)

---

## 1. Founding Principle

We are designing from a blank slate. Zero path dependency. If the right answer
requires rewriting every file in `internal/sync/`, we rewrite every file.
Engineering effort is free. The only thing that matters is getting the
architecture right, because this is the foundation everything else builds on —
multi-drive, WebSocket notifications, filtering, shared content. A wrong
foundation compounds forever.

---

## 2. The Problem

A bidirectional sync engine has two independent data flows. Each starts with an
observation, requires an action to reconcile, and ends with the baseline
recording success. The fundamental question is: what happens when the action
fails?

### Remote → Local (downloads)

```
Observation          Action              Baseline
───────────          ──────              ────────
Delta API tells us   Download the file   Record "local now
"file X has hash R"  to local disk       matches remote"
```

**The observation is ephemeral.** We learn about remote changes by calling the
delta API. The API returns changed items and a new cursor token. Once we advance
the token, the API will not re-tell us about those items. The knowledge exists
only as a `ChangeEvent` struct flowing through the buffer and planner.

If the download succeeds: `CommitOutcome` updates the baseline. Done.

If the download fails: the `ChangeEvent` is gone. `CommitOutcome` is a no-op
for failures. The baseline has no record of the remote change. The only way to
re-learn about it is to re-poll the delta API with the old token.

**This is where the current design breaks.** The delta token advances, so the
API won't re-deliver the item. The knowledge is permanently lost. No local trace
exists — the file simply wasn't downloaded, and neither the baseline nor the
filesystem records that anything is missing.

### The delta token advancement bug

This is not a corner case — it is a **fundamental design flaw** that silently
loses data in the most common failure scenario. The mechanism:

1. Poll returns items [A,B,C,D,E] + token T'. Observer stores T' in memory.
2. D fails (423 lock — routine in SharePoint). Cycle has failures. DB token stays at T.
3. Next poll uses T' (in-memory). Returns [F,G] + token T''. F,G succeed.
4. Cycle has zero failures. Commits T''. **D is permanently lost.**

The failure tracker (`failure_tracker.go`) was introduced to prevent token
starvation but *accelerates* the bug: suppressed items don't count as failures,
so cycles containing them "succeed" and commit the token past the suppressed
items within the same cycle.

This is not hypothetical. In any SharePoint environment with co-authoring, 423
locks are routine. Files locked during download that are never modified again
become silently stale — permanently. The user has no indication anything is
wrong.

The reference OneDrive client (abraunegg) had the exact same class of bug
(#3344 — pending download flag cleared on failure, causing the engine to
interpret the absent file as an intentional local deletion). Every production
sync engine that has shipped has encountered and solved this problem.

The PRD requires "zero data loss crash recovery." The current architecture
**violates this requirement.**

### Local → Remote (uploads)

```
Observation          Action              Baseline
───────────          ──────              ────────
Filesystem has       Upload the file     Record "remote now
"file X with hash L" to OneDrive        matches local"
```

**The observation is inherent.** The local filesystem IS the durable record. We
can re-observe it at any time — via inotify, the safety scan, or by the planner
comparing local files against baseline on every cycle. Upload failures retry
naturally.

### The fundamental asymmetry

| | Remote → Local | Local → Remote |
|---|---|---|
| **Source of truth** | Graph API (external) | Local filesystem (internal) |
| **Observation durability** | Ephemeral — API won't re-tell us | Inherent — file persists on disk |
| **Re-observation on failure** | Requires token replay OR persistent storage | Free — re-reads filesystem every cycle |
| **Current retry mechanism** | None (token bug loses the item) | Natural (planner regenerates) |

Uploads work because the planner reconciles from **state** (filesystem vs
baseline). Downloads fail because the planner reconciles from **events**
(ephemeral ChangeEvents). The fix is to give downloads the same property:
reconcile from persistent state, not ephemeral events.

### Three conflated concerns

The sync engine conflates three things that should be independent:

| Concern | Meaning | When it should update | Where it currently lives |
|---------|---------|----------------------|--------------------------|
| **Observation cursor** | "What have we been told about?" | After every successful API poll | In-memory token + DB token (conflated with sync success) |
| **Remote knowledge** | "What does the remote look like?" | When we learn about a change | Nowhere persistent. Ephemeral ChangeEvent structs |
| **Synced state** | "What have we successfully synced?" | On action success | `baseline` table |

The delta token currently means both "we've been told about everything up to
here" AND "we've synced everything up to here." It should mean only the first.

### What this design does NOT solve

This document addresses the download/remote-change side of failure handling. Two
upload-side gaps remain as pre-existing limitations:

1. **No upload backoff.** A permanently-failing upload (e.g., invalid SharePoint
   filename) is retried every cycle with no delay — wasting one worker's time per
   cycle. The planner regenerates the action because the file still differs from
   baseline. The `local_issues` table (§14) addresses this gap.

2. **No upload failure visibility.** The user has no way to see that a local file
   can't be uploaded. The `issues` CLI command queries both `remote_state` and
   `local_issues` to surface all stuck items.

These were pre-existing gaps. The `local_issues` table designed in §14 resolves
both.

---

## 3. Industry Comparison

| Engine | Observation Persistence | Token Advancement | Failure Recovery |
|--------|------------------------|-------------------|------------------|
| **Dropbox** | Full remote mirror (SFJ) | Always (cursor only) | State discrepancy |
| **Syncthing** | Sequence numbers per device | N/A (no cursor) | Version vectors |
| **abraunegg** | Database (partial) | Only on full success | Token replay (starvation) |
| **rclone bisync** | Listing snapshots | N/A (no cursor) | Full rescan |
| **Google Drive** | Sequential token | Only on full success | Token replay (starvation) |
| **Current onedrive-go** | None (ephemeral events) | Broken (advances past failures) | None (data loss) |
| **Proposed onedrive-go** | Full remote mirror | Always (cursor only) | State discrepancy + reconciler |

### Dropbox Nucleus — The Gold Standard

Dropbox maintains **three separate trees**: Remote Tree (SFJ — what the server
says), Local Tree (filesystem), Synced Tree (last committed state / merge base).
The SFJ cursor is a pure observation cursor — it tracks position in the journal,
not sync success. Processing failures do not affect the cursor.

The `remote_state` table in this design is architecturally equivalent to
Dropbox's Remote Tree. The proposed design follows Dropbox's core principle
(separate observation from application) and goes further than the original
proposal by adopting a full remote mirror rather than a work queue.

### Syncthing — Version Vectors + Sequence Numbers

Syncthing uses monotonically increasing sequence numbers per device. On restart,
it knows exactly where it was. Syncthing doesn't have our problem because it
doesn't use a cursor-based delta API. Each device maintains its own version
vector and sequence counter.

**Relevance: Moderate.** Different model, but the principle is the same:
observation state is durable and independent of sync success.

### Reference OneDrive Client (abraunegg)

Sequential model: poll → process all → commit token. Token only committed after
the entire cycle completes. Correct but slow — one permanently-failing item
blocks everything. This is the "starvation" problem.

Bug #3344 showed that clearing the pending flag on failure caused data loss (the
engine interpreted the missing local file as an intentional deletion).

**The proposed design solves the starvation problem** that the reference client
suffers from. The reference client's approach (don't advance until everything
succeeds) is correct but operationally unacceptable.

### rclone bisync — Snapshot Comparison

rclone bisync saves full listing snapshots before acting. If interrupted, the
next run detects inconsistency and can either resync or resume.

**Relevance: Low.** Different model (full listing comparison, not cursor-based
delta). But the principle holds: the observation is persisted durably before
action.

### Google Drive — Page Token Model

Sequential page token, similar to abraunegg. Token only advances after complete
processing. Same starvation risk.

**The proposed design is strictly better** than Google Drive's model for the same
reason it's better than abraunegg's.

---

## 4. Architectural Decisions

Six foundational decisions shape the entire design:

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **Full remote mirror** (not work queue) | Rows persist for ALL remote items. Enables offline verify, filter-change preview, diagnostic completeness. |
| 2 | **`(drive_id, item_id)` as PK** (not path) | Matches API identity. Atomic moves. Eliminates path-collision bugs. Dropbox proved this. |
| 3 | **Explicit `sync_status` state machine** | Prevents abraunegg bug #3344. Every state is named, never inferred. |
| 4 | **Partitioned interfaces + optimistic concurrency** | Compile-time safety (typed interfaces per writer) + runtime safety (WHERE clause guards). |
| 5 | **`sync_status` authoritative, DepTracker as read-through cache** | Single source of truth in DB. DepTracker projects DB state into memory for fast lookups + dependency DAG. |
| 6 | **`computeNewStatus()` pure function** | All 30 CommitObservation state-transition cells handled in Go code (not SQL CASE), fully unit-testable. |

### Alternatives considered

**A. Never advance the token until all items succeed (abraunegg model).** Correct
but slow. One permanently-failing item blocks all new change discovery
indefinitely. This is "delta token starvation."

**B. Periodically do full resyncs.** A full delta response with an empty token
returns every item in the drive — potentially thousands. Too expensive as the
primary recovery mechanism.

**C. Accept the data loss (current behavior).** Items lost from delta are
recovered only if modified again remotely, or on 90-day token expiration.
Unacceptable.

**D. Persistent failure tracker with separate `failure_records` table.** Works,
but duplicates information: the failure record stores the same data we'd need in
`remote_state`. The failure tracker becomes a shadow copy.

**E. Persist observed remote state (this design).** The Dropbox model. Records
what we learn when we learn it. The delta token is a pure API cursor. Failed
items persist as state discrepancies. This is the only approach that provides
both continuous sync (no starvation) and data integrity (no loss).

We chose E.

---

## 5. Full Remote Mirror

### What changed from original proposal

The original design used a **work queue**: rows existed only for unsynced items
and were deleted on sync success. The revised design uses a **full remote
mirror**: rows persist for ALL remote items. On sync success, the row is marked
`synced` rather than deleted.

### Why (5 reasons)

1. **Offline verification**: Compare baseline against `remote_state` without API
   call.
2. **Filter-change impact preview**: Diff `remote_state` vs baseline without API
   round trip.
3. **Conflict detection completeness**: Planner always sees `Remote != nil` for
   synced files.
4. **Diagnostic completeness**: `status --verbose` shows "X remote, Y synced, Z
   pending, W filtered."
5. **Future migration safety**: No backfill needed — we just don't delete rows.

### Steady-state characteristics

- **Table size**: Same as baseline (~100K rows).
- **Write amplification**: 1 write (UPDATE) vs 2 writes (INSERT + DELETE) — less
  I/O than work queue.
- **Reconciler query**: `WHERE sync_status NOT IN ('synced', 'filtered')` —
  index on `sync_status`.

### Risks addressed

- **R6** (100K scan): Index on `sync_status` makes reconciler query fast.
- **R7** (WAL growth): Periodic checkpoint after initial sync, every 30 min, and
  on shutdown. Hard requirement. New `SyncStore.Checkpoint()` method.
- **R8** (UPDATE vs DELETE): Conditional UPDATE has identical semantics to
  conditional DELETE.

---

## 6. ID-Based Primary Key

### What changed from original proposal

The original design used `path TEXT PRIMARY KEY` on both tables, consistent with
the existing baseline. The revised design uses
`PRIMARY KEY (drive_id, item_id)` with `path TEXT NOT NULL UNIQUE`.

### Why (5 reasons)

1. **Dropbox proved it**: Path-based caused their worst data loss bugs. IDs are
   immutable; paths are derived.
2. **Move atomicity**: Path-as-PK requires DELETE+INSERT (crash window).
   ID-as-PK is single UPDATE — atomic.
3. **Folder rename cascade**: Only renamed folder's row changes — children's
   `parent_id` unchanged.
4. **API identity mismatch**: Delta API identifies by ID. Path materialization is
   lossy — two items can transiently share a path.
5. **Shared drive identity**: `(drive_id, item_id)` is globally unique; paths
   may collide across shared drives.

### Code impact analysis

- Buffer, Planner, Executor, Scanner, LocalObserver: all use `GetByPath()` —
  unchanged (path is UNIQUE).
- Remote observer: `GetByID()` now uses PRIMARY KEY — faster.
- CommitOutcome: upsert by ID. Moves become single UPDATE.

### In-memory changes

`ByID` becomes canonical map. `ByPath` remains as secondary index. New
`DeleteByID(key)`.

### Risks addressed

- **R9** (folder rename cascade): Eager — O(N) writes in same transaction.
  Stale paths confuse every path-based lookup.
- **R10** (transient path collision): Atomic transaction + B-281 ordering. Two
  items transiently sharing a path are handled within the transaction.
- **R11** (`local_issues` asymmetry): By design — local files have no item ID
  until uploaded.

---

## 7. Explicit State Machine

### Why

abraunegg bug #3344 — implicit state inference leads to data loss. Explicit
states are unambiguous.

### Download state machine

```
UNKNOWN → PENDING_DOWNLOAD → DOWNLOADING → SYNCED
                  ↑               |
                  |          (on failure)
                  |               ↓
                  ←── DOWNLOAD_FAILED
```

### `sync_status` values

| Value | Meaning | Transitions to |
|-------|---------|---------------|
| `pending_download` | Delta observed, download needed | `downloading`, `synced`, `filtered` |
| `downloading` | Worker started download | `synced`, `download_failed` |
| `download_failed` | Failed after retries | `pending_download` (reconciler) |
| `synced` | Baseline matches remote_state | `pending_download` (new delta), `filtered` |
| `pending_delete` | Delta observed deletion | `deleting` |
| `deleting` | Worker started delete | `deleted`, `delete_failed` |
| `delete_failed` | Delete failed | `pending_delete` (reconciler) |
| `deleted` | Deletion complete, row kept | (terminal; new delta can resurrect) |
| `filtered` | Excluded by filter rules | `pending_download` (SIGHUP), `synced` |

### State transition ownership (7 writers)

| Writer | Goroutine | Transition |
|--------|-----------|------------|
| CommitObservation | Remote observer → engine | `* → pending_download`, `* → pending_delete` |
| DepTracker.Add() | Engine (processBatch) | `pending_download → downloading`, `pending_delete → deleting` |
| CommitOutcome | Worker goroutines | `downloading → synced`, `deleting → deleted` |
| RecordFailure | Worker or drain goroutine | `downloading → download_failed`, `deleting → delete_failed` |
| Filter marking | Engine | `pending_download → filtered` |
| Reconciler | Reconciler goroutine | `download_failed → pending_download`, `delete_failed → pending_delete` |
| SIGHUP | Signal handler | `filtered → pending_download` |

### Critical invariant

> A `remote_state` row with `sync_status` in (`pending_download`, `downloading`,
> `download_failed`) means "the remote has this file and we haven't synced it
> yet." The absence of the corresponding local file means "we haven't downloaded
> it yet," NOT "the user deleted it locally." No code path may interpret a
> missing local file + existing pending row as local deletion intent.

---

## 8. Data Flows

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
  │         (UPSERT by (drive_id, item_id) — latest state wins)
  │         For moves: also write pending_delete row for old path
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

The `remote_state` write and token commit are in the same transaction. This is
the critical atomicity guarantee: we never advance the token without recording
what we learned. If the daemon crashes after the transaction but before events
reach the buffer, the events are lost from the pipeline but the `remote_state`
rows persist. The reconciler picks them up on restart.

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

No `remote_state` involvement. Local changes retry naturally because the planner
compares filesystem against baseline every cycle.

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

Catches local changes inotify missed. Does NOT detect remote discrepancies —
that's the reconciler's job.

### Flow 4: Reconciler retry

```
Reconciler goroutine (dedicated, long-lived, level-triggered)
  │
  ├─ On startup: reconcile() — read DB, schedule timer for earliest retry
  │
  ├─ On Kick() (from drainWorkerResults after any action completion):
  │     reconcile() — read DB, find items where next_retry_at <= now:
  │       Dispatch ready items via buf.Add()
  │       Arm timer for next earliest retry
  │       Skip in-flight items
  │
  ├─ On 2-minute safety sweep:
  │     reconcile() — same as Kick, catches anything missed
  │
  └─ When timer fires:
        1. Re-read remote_state row from DB (may have changed)
        2. Skip if in-flight (tracker.HasInFlight)
        3. Synthesize ChangeEvent from row
        4. Feed into buffer via buf.Add()
        5. Buffer debounces → planner → workers (normal pipeline)
```

### Flow 5: Action success

```
Worker completes action with Success=true
  │
  ├─ 1. store.CommitOutcome(outcome) → returns error
  │     → BEGIN TRANSACTION
  │     → Upsert baseline row (or delete for deletions, or move)
  │     → UPDATE remote_state SET sync_status = 'synced'
  │       WHERE drive_id = ? AND item_id = ? AND sync_status = 'downloading'
  │         AND hash IS ?
  │       (conditional — preserves row if a newer delta updated it)
  │     → COMMIT
  │
  ├─ 2. Update in-memory baseline cache
  │
  └─ 3. WorkerResult{Success: true} → drainWorkerResults
        → reconciler.Kick()  // "something changed, check the DB"
```

The reconciler discovers the outcome on its next `reconcile()` pass:
- If CommitOutcome's conditional UPDATE matched (`synced`): reconciler sees
  `synced` status, no action needed.
- If it didn't match (newer version exists): reconciler sees row still pending,
  keeps/reschedules timer.

#### Stale success with newer remote_state

When a worker downloads R1 successfully but `remote_state` has R2 (from a newer
delta), CommitOutcome's conditional update doesn't match — the row stays
`downloading` with R2's hash. RecordFailure isn't called (the action succeeded
from the worker's perspective). The reconciler's next pass sees the row still
needs work and reschedules. R2's retry lifecycle is completely unaffected by R1's
stale success.

### Flow 6: Action failure

```
Worker completes action with Success=false
  │
  ├─ 1. CommitOutcome is a no-op (baseline unchanged)
  │
  └─ 2. WorkerResult{Success: false, HTTPStatus: N, ErrMsg: "..."}
        → drainWorkerResults
        → UPDATE remote_state SET
            sync_status = 'download_failed',
            failure_count = failure_count + 1,
            next_retry_at = ?, last_error = ?, http_status = ?
          WHERE drive_id = ? AND item_id = ? AND sync_status = 'downloading'
        → reconciler.Kick()
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
  │     → Queries rows with non-terminal sync_status
  │     → Resets 'downloading' → 'pending_download' (crash recovery)
  │     → Schedules retries per backoff state
  │
  ├─ 3. RemoteObserver.FullDelta(token)
  │     → Returns only changes SINCE the stored token
  │     → Previously-failed items are NOT in this response
  │
  ├─ 4. CommitObservation writes fresh delta events to remote_state
  │     → Fresh events overwrite stale rows (UPSERT)
  │
  └─ 5. Planner receives both fresh delta events and reconciler retries
        → Normal pipeline: planner → workers → outcomes
```

Previously-failed items are recovered from `remote_state`, not from delta
replay. No items are ever lost, regardless of how many times the daemon crashes.

### Flow 8: WebSocket notification (future, Phase 8)

```
WebSocket push → trigger immediate delta poll (Flow 1)
```

WebSocket changes exactly one thing: the trigger for Flow 1. Everything
downstream is unchanged.

---

## 9. Conflict Scenarios

Ten scenarios proving the design handles every edge case.

### Scenario 1: Simple remote edit, download fails, later succeeds

```
Time 0: Delta delivers file X with hash R_new.
        remote_state: {item_id: X, hash: R_new, sync_status: pending_download}
        baseline:     {item_id: X, remote_hash: R_old, local_hash: R_old}

        Planner: remoteChanged=true → EF2: download.
        Download fails (423 locked). sync_status → download_failed. failure_count → 1.

Time 1: Reconciler dispatches retry (~5s later).
        Same PathView. Same decision. Download succeeds.

        CommitOutcome: baseline updated. sync_status → synced.
        Reconciled.
```

### Scenario 2: Remote edit fails, then user edits locally

```
Time 0: Delta delivers X with hash R_new. Download fails.
        remote_state: {hash: R_new, sync_status: download_failed}

Time 1: User edits X locally to hash L_new. inotify fires.

Time 2: Reconciler retry + inotify event merge in buffer.
        Remote=R_new, Local=L_new, Baseline=R_old.
        remoteChanged=true, localChanged=true, L_new ≠ R_new
        → EF5: edit-edit conflict.

        Conflict copy preserves local edit. Remote version downloaded.
        No data loss.
```

### Scenario 3: Remote edit fails, remote edits again

```
Time 0: Delta delivers X with hash R1. Download fails.
Time 1: Delta delivers X with hash R2.
        remote_state updated to {hash: R2, failure_count: 0}.
        New action planned for R2. R1 is superseded — correct.
```

### Scenario 4: Download succeeds for R1 but remote_state already has R2

```
Time 0: Delta delivers X with hash R1. Download planned.
Time 1: Delta delivers X with hash R2. remote_state updated to R2.
Time 2: Download of R1 completes.
        CommitOutcome: baseline=R1.
        UPDATE ... SET sync_status='synced' WHERE hash IS R1
          → R1 ≠ R2 → 0 rows affected → row NOT updated.
Time 3: Reconciler retries. Downloads R2. Fully reconciled.
```

The conditional update prevents losing knowledge of R2.

### Scenario 5: Upload fails, then remote changes the same file

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

### Scenario 6: Remote delete fails, user creates new file at same path

```
Time 0: Delta delivers delete for X. sync_status → pending_delete.
        Local delete fails (permission denied).

Time 1: User creates new file at X with hash L_new.

Time 2: Remote=deleted, Local=L_new, Baseline=exists.
        remoteDeleted=true, localChanged=true → EF9: edit-delete conflict.
        User's file preserved as conflict copy. Correct.
```

### Scenario 7: Remote move fails, then file is edited

```
Time 0: Delta delivers move X → Y.
        remote_state: {path: Y, pending_download} + {path: X, pending_delete}.
        Local move fails.

Time 1: User edits local file at X.

Time 2: Reconciler dispatches retries.
        Path X: Remote=deleted, Local=modified → EF9 edit-delete conflict.
        Path Y: Remote=exists, Local=absent → EF14 download.
        Both versions preserved. No data loss.
```

### Scenario 8: Multiple failures compound, then resolve

```
Time 0: Delta delivers A, B, C, D, E. A, B succeed. C, D, E fail.
Time 1: Delta delivers F, G. Token advanced. F, G succeed.
Time 2: Reconciler retries C, D, E (~5s). C succeeds. D, E fail (count → 2).
Time 3: Reconciler retries D, E (~5s). Fail again. count → 3. Backoff: 5 min.
Time 4: Delta delivers new version of D. remote_state reset to count=0.
Time 5: D retried immediately. Succeeds. E backoff expires. Succeeds.
Final: everything synced. Token advanced freely throughout.
```

### Scenario 9: inotify missed a local edit

```
Time 0: Reconciler retries X. Planner derives Local from baseline. Plans download.
Time 1: User edits X locally. inotify hasn't fired yet.
Time 2: Worker starts download. S4 safety check: hash local file.
        Expected=R_old, actual=L_new. Mismatch! → conflict copy.
        Remote version downloaded. No data loss.
```

The S4 safety check protects against any race between planning and execution,
regardless of event source.

### Scenario 10: Remote delete succeeds

```
Time 0: Delta delivers delete for X.
        sync_status → pending_delete.
        Planner: EF3 remote-only delete. Local file deleted.

Time 1: CommitOutcome: baseline row deleted.
        UPDATE remote_state SET sync_status = 'deleted'
        WHERE drive_id = ? AND item_id = ? AND sync_status = 'deleting'
        → Row updated to 'deleted'. Clean state.
```

---

## 10. Concurrency Safety Model

With 7 goroutines writing `sync_status`, the concurrency model must be explicit.
This section resolves risks R1-R5.

### 8.1 State Ownership — Partitioned Interfaces + Optimistic Concurrency (R1)

**Three real races traced through code:**

**Race 1** (CommitObservation vs CommitOutcome): Worker finishes download
(hash=abc), CommitObservation writes new delta (hash=xyz) before CommitOutcome
runs. Without guards: item marked `synced` with stale hash.

**Race 2** (RecordFailure scope): `drainWorkerResults` calls RecordFailure on a
different goroutine than the worker's CommitOutcome — potential race.

**Race 3** (Add vs Reconciler): Reconciler resets
`download_failed → pending_download` while Add() sets
`pending_download → downloading`. Without guards: reconciler overwrites
`downloading → pending_download`.

**Solution: Compile-time + runtime safety**

Typed interfaces enforce transition ownership:

```go
type ObservationWriter interface { SetPendingDownload(...); SetPendingDelete(...) }
type DispatchWriter interface { SetDownloading(...); SetDeleting(...) }
type OutcomeWriter interface { SetSynced(...); SetDeleted(...) }
type FailureWriter interface { SetDownloadFailed(...); SetDeleteFailed(...) }
```

Every method uses optimistic concurrency WHERE clauses:

```sql
-- CommitOutcome
UPDATE remote_state SET sync_status = 'synced'
WHERE drive_id = ? AND item_id = ? AND sync_status = 'downloading' AND hash IS ?

-- Add() dispatch
UPDATE remote_state SET sync_status = 'downloading'
WHERE ... AND sync_status IN ('pending_download', 'download_failed')

-- RecordFailure
UPDATE remote_state SET sync_status = 'download_failed'
WHERE ... AND sync_status = 'downloading'
```

Every method returns `(rowsAffected int64, err error)`. `rowsAffected == 0`
means lost race — log at Debug, not an error.

**Why both layers**: SQLite serialization (`SetMaxOpenConns(1)`) is necessary
but NOT sufficient. Two serialized UPDATEs can still produce wrong results if
the second doesn't check what the first wrote. Consider Race 1: even with
serialized writes, CommitObservation writes `pending_download` (hash=xyz), then
CommitOutcome writes `synced` — result is wrong. The WHERE clause is essential.

### 8.2 DepTracker as Read-Through Cache (R2)

**Problem**: DepTracker (in-memory) and `sync_status` (DB) overlap for in-flight
tracking.

**Solution**: `sync_status` is authoritative. DepTracker becomes a projection of
DB state:

1. `Add()` sets `downloading` in DB AND adds to in-memory map.
2. `Complete()` triggers CommitOutcome (`synced` in DB) AND removes from map.
3. On crash: DepTracker rebuilt from
   `remote_state WHERE sync_status = 'downloading'`.
4. B-122 dedup: check in-memory map (fast) — always consistent.

The DB write in Add() is ~0.1ms in WAL mode. Dependency ordering stays purely
in-memory.

### 8.3 CancelByPath Safety (R3)

CancelByPath doesn't update DB. Three scenarios all handled by optimistic
concurrency:

1. **Cancel before dispatch**: CommitOutcome's hash check discards stale result.
2. **Cancel during dispatch**: RecordFailure sets `download_failed` (WHERE
   matches). New Add() picks up from there.
3. **Cancel after dispatch**: CommitOutcome's hash check discards stale result
   (if hash changed) or succeeds (if same).

CancelByPath stays pure in-memory. It's a performance optimization, not a
correctness mechanism.

### 8.4 Buffer Re-Dispatch (R5)

Reconciler query `WHERE sync_status IN ('pending_download', 'download_failed')`
naturally excludes `downloading` items. B-122 dedup catches the sub-millisecond
race. No action needed.

---

## 11. CommitObservation Logic

### The problem (R4 — highest severity)

Delta API re-delivers unchanged items (page-level tokens, sharing changes, 410
full resync). Naive CommitObservation regresses `synced` items to
`pending_download`, triggering unnecessary re-downloads.

### Full decision matrix (10 states × 3 conditions = 30 cells)

| Current state | Same hash, not deleted | Different hash, not deleted | Deleted |
|---|---|---|---|
| `pending_download` | No change | Update hash (still pending) | → `pending_delete` |
| `downloading` | No change (let worker finish) | → `pending_download` + cancel | → `pending_delete` + cancel |
| `download_failed` | → `pending_download` (retry) | → `pending_download` (new version) | → `pending_delete` |
| `synced` | **No change (critical!)** | → `pending_download` | → `pending_delete` |
| `pending_delete` | → `pending_download` (restored) | → `pending_download` (restored+changed) | No change |
| `deleting` | → `pending_download` (restored) | → `pending_download` (restored) | No change (let worker finish) |
| `delete_failed` | → `pending_download` (restored) | → `pending_download` (restored) | → `pending_delete` (retry) |
| `deleted` | → `pending_download` (recreated) | → `pending_download` (recreated) | No change |
| `filtered` | No change | Update hash (stay `filtered`) | → `deleted` |
| (no row) | → `pending_download` (new) | → `pending_download` (new) | No-op |

**Why filtered items update their hash.** When a filtered item receives a delta
with a different hash, the status stays `filtered` but the hash, size, mtime,
and etag columns are updated. This ensures that when SIGHUP later resets
`filtered → pending_download`, the row contains the current remote hash. Without
this, CommitOutcome's `WHERE hash IS ?` guard would fail after a successful
download (the downloaded file's hash wouldn't match the stale hash in
`remote_state`), orphaning the row in `downloading` state permanently.

### Implementation: `computeNewStatus()` pure function

```go
func (s *SyncStore) CommitObservation(ctx context.Context, item ObservedItem) error {
    tx, _ := s.db.BeginTx(ctx, nil)
    var currentStatus, currentHash string
    err := tx.QueryRow(
        "SELECT sync_status, hash FROM remote_state WHERE drive_id=? AND item_id=?",
        item.DriveID, item.ItemID).Scan(&currentStatus, &currentHash)
    if err == sql.ErrNoRows {
        tx.Exec("INSERT INTO remote_state (...) VALUES (..., 'pending_download')")
    } else {
        newStatus := computeNewStatus(currentStatus, currentHash, item.Hash, item.IsDeleted)
        tx.Exec("UPDATE ... SET sync_status=?, hash=?, ... WHERE drive_id=? AND item_id=?",
            newStatus, item.Hash, ...)
    }
    return tx.Commit()
}
```

**Why Go code over SQL CASE**: 30 cells. Pure function → table-driven unit test.
Readable, debuggable, extensible. The extra SELECT per item is negligible
(~0.05ms in WAL mode).

**Failure count reset**: When `excluded.hash != remote_state.hash`, reset
`failure_count = 0` and `next_retry_at = NULL`. The file may have changed —
give it a fresh start.

### CommitObservation transaction details

**Why the observer calls it (not the engine).** In watch mode, the observer runs
in a goroutine sending events via channel. The engine would need to intercept
events, batch them, and write to DB before forwarding — this changes the event
pipeline and requires a side channel for batch boundaries. Unnecessary
complexity. The observer already depends on `*Baseline` for reads; adding an
`ObservationWriter` interface is a minor extension.

**Why not batch across multiple delta polls?** Each poll returns a discrete set
of events and a new token. Writing them in one transaction per poll is the
natural boundary — it matches the atomicity guarantee we need (never advance the
token without recording what we learned). Batching across polls would require
tracking which events go with which token. The per-poll transaction is typically
<10ms for a normal delta response (10-100 items). For initial sync (50K items),
it's ~200-500ms — acceptable.

**FullDelta is all-or-nothing.** `FullDelta()` accumulates all pages in memory
before returning. If the connection drops mid-pagination, no partial result is
returned — the entire fetch fails, no events are emitted, and the delta token is
not advanced. The next poll retries from the same token. This means
`CommitObservation` always receives a complete batch: every item from the delta
response, plus the new token. There is no risk of committing a partial
observation. The new token only appears on the final page (`@odata.deltaLink`),
so it's impossible to obtain a token without having fetched all preceding items.

**Crash window analysis.** If we crash between the delta poll and
`CommitObservation`, both the events and the new token are lost. The old token is
replayed on restart, delta re-delivers the same items. `remote_state` writes are
idempotent (same data rewritten via UPSERT). No data loss. If we crash after
`CommitObservation` but before events reach the buffer, the events are lost from
the pipeline but the `remote_state` rows persist. The reconciler picks them up on
restart.

---

## 12. Database Access Pattern

### The problem with "sole writer"

The current codebase has a `BaselineManager` type that owns all database
methods. Today, multiple goroutines already call write methods concurrently:
worker goroutines (N concurrent) call `CommitOutcome()`, the engine goroutine
calls `CommitDeltaToken()`, the daemon goroutine calls
`PruneResolvedConflicts()`. This design adds more: `CommitObservation()` from
the observer, `RecordFailure()` from drain. Correctness comes from SQLite WAL
mode with busy timeout, not single-writer.

### Alternatives considered for access pattern

**A. Status quo + documentation fix.** Keep one type, clarify "sole writer"
means "sole module." Rejected: grows to ~23 methods — a god object. No
compile-time restriction on who writes what.

**B. Split by table ownership.** Three types (`BaselineStore`,
`RemoteStateStore`, `DeltaTokenStore`). Rejected: some operations need
cross-table atomicity (`CommitOutcome` writes baseline + updates remote_state;
`CommitObservation` writes remote_state + delta_tokens). A coordinator type
becomes the new god object.

**C. Split by read vs. write.** `SyncStateReader` for queries,
`SyncStateWriter` for mutations. Rejected: splits related operations.
`CommitOutcome` (write) and "is this item in baseline?" (read) are conceptually
coupled but live in different types.

**D. Sub-interfaces on one concrete type (chosen).** One implementation type,
five interfaces grouped by caller identity. Callers receive the narrowest
interface they need. Cross-table transactions stay in one place. Type system
enforces capability restriction at compile time.

**E. Event-sourced single-writer goroutine.** All state changes flow through a
channel to a single writer goroutine. Rejected: adds latency to every write,
requires back-pressure design, complicates shutdown/drain. Write volume (~tens of
items per 5-minute cycle) doesn't justify the complexity.

We chose D.

### Alternatives considered for reconciler mechanism

**A. Poll from the observer.** RemoteObserver queries `remote_state` for
unreconciled rows on each delta poll. Rejected: mixes concerns (observer reads
internal DB state). Couples retry timing to poll interval.

**B. Timer in the engine's select loop.** Add `time.Ticker` to engine's main
select. Rejected: breaks the pattern where each long-lived concern owns its
goroutine. Adds latency (up to N minutes) and wastes work scanning when
`remote_state` is empty.

**C. Extend the local safety scan.** Walk filesystem + query `remote_state`.
Rejected: different concern with different timing requirements. Coupling means
you can't change one without affecting the other.

**D. Fire-and-forget per-item timers.** `time.AfterFunc` on failure. Rejected:
no cancel on shutdown (goroutine leak), no bootstrap from DB on startup, no
centralized rate limiting, no way to skip in-flight items.

**E. Dedicated reconciler goroutine (chosen).** Single long-lived goroutine.
Receives `Kick()` hints via 1-buffered channel, reads authoritative state from
DB, maintains single timer, synthesizes ChangeEvents when retries fire.
Level-triggered: every `reconcile()` pass reads DB and adjusts timer. 2-minute
safety sweep catches anything missed.

We chose E.

### SyncStore sub-interface signatures

```go
// ObservationWriter — called by RemoteObserver goroutine (single caller).
// Writes observed remote state and advances the delta token atomically.
type ObservationWriter interface {
    CommitObservation(ctx context.Context, events []ChangeEvent, newToken string, driveID string) error
    GetDeltaToken(ctx context.Context, driveID, scopeID string) (string, error)
}

// OutcomeWriter — called by worker goroutines (N concurrent callers).
// Commits action results to baseline and updates remote_state on success.
type OutcomeWriter interface {
    CommitOutcome(ctx context.Context, outcome *Outcome) error
    Load(ctx context.Context) (*Baseline, error)
}

// FailureRecorder — called by drainWorkerResults goroutine (single caller).
// Records failure metadata on remote_state rows.
type FailureRecorder interface {
    RecordFailure(ctx context.Context, path, errMsg string, httpStatus int) error
}

// StateReader — called by reconciler, planner, status, CLI (read-only).
// All methods are pure reads. Multiple goroutines call concurrently.
// WAL mode guarantees readers never block.
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
// Write operations that don't fit the hot path.
type StateAdmin interface {
    ResolveConflict(ctx context.Context, id, resolution string) error
    PruneResolvedConflicts(ctx context.Context, retention time.Duration) (int, error)
    WriteSyncMetadata(ctx context.Context, report *SyncReport) error
    ResetFailure(ctx context.Context, path string) error     // reset failure_count, not delete
    ResetAllFailures(ctx context.Context) error              // reset all, not delete
}
```

### Component → interface distribution

| Component | Receives | Why |
|-----------|----------|-----|
| RemoteObserver | `ObservationWriter` | Writes observations + reads delta token |
| Worker pool | `OutcomeWriter` | Commits outcomes + reads baseline cache |
| drainWorkerResults | `FailureRecorder` | Records failures on action failure |
| Reconciler | `StateReader` | Reads unreconciled rows for retry scheduling |
| CLI commands | `StateReader` + `StateAdmin` | Reads for display + admin writes |
| Engine | all interfaces (constructs + distributes) | Orchestrator |

**The `DB()` escape hatch is removed.** Today, `BaselineManager.DB()` exposes
raw `*sql.DB` so `status.go` can run ad-hoc queries. Under this design, those
queries become methods on `StateReader`. No component receives raw database
access.

### Concurrency model

Sub-interfaces restrict *capability* (what each caller may do). They do not
restrict *concurrency* (multiple goroutines still call concurrently).

Concurrency safety comes from SQLite WAL mode with a 5-second busy timeout
(`_busy_timeout=5000`). Under WAL, readers never block — they read from a
consistent snapshot. Writers serialize: if two goroutines call write methods
simultaneously, one waits (up to the busy timeout). With the system's write
volume, contention is negligible.

---

## 13. Unified Filtering Architecture

Single unified `Filter` struct at ALL boundaries. Every competitor uses a single
mechanism.

```go
type Filter struct {
    builtIn   *BuiltInRules     // .partial, .tmp, ~$*, invalid OneDrive names
    userRules *UserRules         // skip_files, skip_dirs, skip_dotfiles, max_file_size, sync_paths
    odIgnore  *OdIgnoreCache     // per-directory .odignore (Phase 10)
}

func (f *Filter) ShouldSync(path string, itemType ItemType, size int64) FilterResult
```

### Call sites (defense in depth)

| Call site | Purpose | On exclude |
|-----------|---------|-----------|
| Remote observer `classifyItem()` | Primary gate | Dropped. Never in `remote_state`. Fixes FC-1. |
| Local observer (scanner, inotify) | Primary gate | Dropped. Never enters buffer. |
| Planner | Belt-and-suspenders | Excluded. `remote_state` → `filtered`. |
| `CommitObservation` pre-check | Defense in depth | Prevents polluting `remote_state`. |

### FC-1 through FC-12 resolutions

All filtering conflicts from [filtering-conflicts.md](filtering-conflicts.md)
are addressed:

| ID | Issue | Resolution |
|----|-------|-----------|
| FC-1 | Remote observer missing built-in filtering | Symmetric filtering in `classifyItem()` |
| FC-2 | `.db` exclusion too aggressive | Narrow to sync engine DB path |
| FC-3 | `desktop.ini` dual-system | Document overlap, remove from 10.5 junk list |
| FC-4 | E20 vs. observer built-ins | Two-tier model, amend E20 |
| FC-5 | Built-in vs. `skip_files` overlap | Document Layer 0, fix examples |
| FC-6 | `.odignore` killed by `skip_dotfiles` | Exempt `ignore_marker` from `skip_dotfiles` |
| FC-7 | `.odignore` sync behavior | Never sync (add to built-ins) |
| FC-8 | `auto_clean_junk` deletion wars | Exclude junk, don't auto-clean |
| FC-9 | Stale files after filter changes | Warn and freeze, no delete propagation |
| FC-10 | `sync_paths` parent traversal | Lightweight baseline entries for parents |
| FC-11 | `.nosync` vs. remote observer | Document; require remote-side exclusion for future per-dir |
| FC-12 | Non-empty directory delete | Two-tier disposable + fail-safe |

### Risks addressed

- **R12** (invisible filtered): Correct behavior — `filtered` state deliberate.
- **R13** (stale local): Correct — warn/freeze (FC-9).
- **R14** (two-stage filtering): Defense in depth, same `Filter.ShouldSync()`.
- **R21** (filter performance): Deferred to Phase 10.

---

## 14. Upload Failure Tracking

### Design: `local_issues` table

Upload failure tracking is separate from `remote_state` because uploads are
locally-driven. Local files have no item ID until uploaded.

### Error classification (17 types)

- **PERMANENT**: invalid filename, path too long, file too large, permission
  denied.
- **TRANSIENT**: locked, rate limited, network, eTag, range, server, name
  conflict.
- **FATAL**: quota, unauthorized, context canceled.
- **TRANSPARENT**: token expiry, session expiry (handled by existing retry
  layers).
- **WARNING**: hash mismatch (post-upload verification).

### Pre-upload validation (in planner)

1. `Filter.ShouldSync()` — invalid filenames, reserved names.
2. Path length: `<= 400` bytes (Business) or runes (Personal).
3. File size: `<= 250 GB`.

Items failing validation → `local_issues` with `permanently_failed`. Surfaced
via `onedrive-go issues`.

### Upload-side retries

Planner checks `local_issues` for `upload_failed` items where
`next_retry_at <= now`. Same backoff curve (5s → 5min → 10min → 20min → 40min →
1hr cap).

---

## 15. Consolidated Schema

### `remote_state` (full mirror + ID PK + explicit state)

```sql
CREATE TABLE IF NOT EXISTS remote_state (
    drive_id      TEXT    NOT NULL,
    item_id       TEXT    NOT NULL,
    path          TEXT    NOT NULL UNIQUE,
    parent_id     TEXT,
    item_type     TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash          TEXT,
    size          INTEGER,
    mtime         INTEGER,
    etag          TEXT,
    sync_status   TEXT    NOT NULL DEFAULT 'pending_download'
                  CHECK(sync_status IN (
                      'pending_download', 'downloading', 'download_failed',
                      'synced',
                      'pending_delete', 'deleting', 'delete_failed', 'deleted',
                      'filtered')),
    observed_at   INTEGER NOT NULL CHECK(observed_at > 0),
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    PRIMARY KEY (drive_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_remote_state_status
    ON remote_state(sync_status);
CREATE INDEX IF NOT EXISTS idx_remote_state_parent
    ON remote_state(parent_id);
```

### `baseline` (ID PK)

```sql
CREATE TABLE IF NOT EXISTS baseline (
    drive_id    TEXT    NOT NULL,
    item_id     TEXT    NOT NULL,
    path        TEXT    NOT NULL UNIQUE,
    parent_id   TEXT,
    item_type   TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash  TEXT,
    remote_hash TEXT,
    size        INTEGER,
    mtime       INTEGER,
    synced_at   INTEGER NOT NULL,
    etag        TEXT,
    PRIMARY KEY (drive_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_baseline_parent
    ON baseline(parent_id);
```

### `local_issues` (upload tracking)

```sql
CREATE TABLE IF NOT EXISTS local_issues (
    path          TEXT    PRIMARY KEY,
    issue_type    TEXT    NOT NULL
                  CHECK(issue_type IN (
                      'invalid_filename', 'path_too_long', 'file_too_large',
                      'permission_denied', 'upload_failed', 'quota_exceeded',
                      'locked', 'sharepoint_restriction')),
    sync_status   TEXT    NOT NULL DEFAULT 'pending_upload'
                  CHECK(sync_status IN (
                      'pending_upload', 'uploading', 'upload_failed',
                      'permanently_failed', 'resolved')),
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    file_size     INTEGER,
    local_hash    TEXT
);
```

### CommitOutcome SQL

- **Download success**:
  ```sql
  UPDATE remote_state SET sync_status = 'synced'
  WHERE drive_id = ? AND item_id = ?
    AND sync_status = 'downloading' AND hash IS ?
  ```
- **Delete success**:
  ```sql
  UPDATE remote_state SET sync_status = 'deleted'
  WHERE drive_id = ? AND item_id = ?
    AND sync_status = 'deleting'
  ```
- **Upload success**:
  ```sql
  DELETE FROM local_issues WHERE path = ?
  ```

---

## 16. Single-Timer Reconciler

### What changed from original proposal

The original design used `map[string]*time.Timer` (per-item, edge-triggered).
The revised design uses a single `*time.Timer` (level-triggered). Per-item
timers → 1000 goroutines thundering on `buf.Add()` mutex. Single timer +
reconcile loop is truly level-triggered.

### Design

```go
type Reconciler struct {
    state  StateReader
    buf    *ChangeBuffer
    tracker *InFlightTracker
    logger *slog.Logger
    mu     sync.Mutex
    timer  *time.Timer    // single timer; nil when idle
    kickCh chan struct{}   // 1-buffered, coalesces kicks
    cancel context.CancelFunc
}
```

### How it works

1. `reconcile()` reads all unreconciled rows.
2. Rows where `next_retry_at <= now` and not in-flight: dispatch via
   `buf.Add()`.
3. Remaining rows: arm timer to minimum `next_retry_at`.
4. Timer callback writes to `kickCh`.

### Three mechanisms

- **`Kick()`** (event-driven): Called after every worker completion.
  Non-blocking write to 1-buffered channel. Multiple rapid kicks coalesce.
- **Single timer** (event-driven): Armed to the earliest `next_retry_at`.
  Callback writes to `kickCh`.
- **2-minute safety sweep** (backup): Catches anything missed by kicks or
  timer. Level-triggered — reads DB ground truth.

### Main loop

```go
func (r *Reconciler) run(ctx context.Context) {
    safety := time.NewTicker(2 * time.Minute)
    defer safety.Stop()

    r.reconcile(ctx) // bootstrap

    for {
        select {
        case <-r.kickCh:
            r.reconcile(ctx)
        case <-safety.C:
            r.reconcile(ctx)
        case <-ctx.Done():
            r.mu.Lock()
            if r.timer != nil { r.timer.Stop() }
            r.mu.Unlock()
            return
        }
    }
}
```

### Retry backoff

| After N failures | Retry delay |
|------------------|-------------|
| 1-2 | ~5 seconds (immediate via reconciler) |
| 3 | 5 min |
| 4 | 10 min |
| 5 | 20 min |
| 6 | 40 min |
| 7+ | 1 hour (cap) |

Formula: `next_retry_at = last_failed_at + min(5min × 2^(failure_count - 3), 1 hour)`

No error classification drives retry strategy. The HTTP status and error message
are metadata for user visibility, not retry logic.

### Re-read cases when timer fires

When a retry timer fires, the reconciler re-reads the `remote_state` row from
DB. The row may have changed since the timer was scheduled. Five cases:

| DB state at re-read time | Action | Why |
|--------------------------|--------|-----|
| Row `synced` (CommitOutcome updated it) | Skip, no-op | Item synced successfully. Timer was stale |
| Hash changed (fresh delta overwrote) | Synthesize event with **new** hash | Old version irrelevant. Download current version |
| `pending_delete` (was `pending_download`) | Synthesize delete event | Delta delivered deletion while retrying download |
| Item in-flight (`tracker.HasInFlight`) | Skip, reschedule at same backoff | Already being processed. Avoid duplicate work |
| Unchanged | Synthesize event, dispatch | Normal retry |

All five follow from "use current DB state at timer-fire time." The reconciler
never caches — it always re-reads. Correct by construction regardless of
intervening events.

### Synthesized ChangeEvent fields

```go
ChangeEvent{
    Source:    SourceRemote,
    Type:      ChangeModify,    // or ChangeDelete if pending_delete
    Path:      row.Path,
    ItemID:    row.ItemID,
    ParentID:  row.ParentID,
    DriveID:   row.DriveID,
    ItemType:  row.ItemType,
    Name:      filepath.Base(row.Path),  // derived, not stored
    Size:      row.Size,
    Hash:      row.Hash,
    Mtime:     row.Mtime,
    ETag:      row.ETag,
    CTag:      "",             // not stored, not used by planner
}
```

- `pending_delete` → `ChangeDelete`. The planner sees a remote deletion.
- All other pending states → `ChangeModify`. Planner checks hash vs baseline.
- Never `ChangeMove` — moves decomposed into delete + create at observation
  time. Each side retried independently.

### Permanently-failing items

Items that never self-heal are retried at the 1-hour cap. Each attempt costs ~1
second. At steady state, 10 such items cost 10 seconds per hour — 0.07% of one
worker's capacity. The `issues` CLI command makes stuck items visible.

### Risks addressed

- **R15** (timer re-arm): Re-arm after pass, Kick catches new items.
- **R16** (timer vs shutdown): Channel + `ctx.Done()` guard.
- **R17** (thundering herd): Buffer debounce + worker backpressure.

---

## 17. Error Handling

Every error that survives the two existing retry layers (graph client: 5 retries,
executor: 3 retries) is handled uniformly by the state discrepancy mechanism. No
error classification drives retry strategy.

### Error table

| Error | Where state lives | Recovery |
|-------|------------------|----------|
| 423 Locked (download) | `remote_state` row persists | Self-heals when lock released. Reconciler retries |
| 403 Forbidden (download) | `remote_state` row persists | User must fix permissions. Visible in `issues` CLI |
| Persistent 5xx (download) | `remote_state` row persists | Self-heals when outage ends. Reconciler retries |
| FS permission (download) | `remote_state` row persists | User must fix local permissions. Visible in `issues` CLI |
| Non-empty directory delete | `remote_state` `pending_delete` row | Escalated to conflict after threshold. See below |
| 401 Unauthorized | `remote_state` row persists | Auth must be refreshed externally. See below |
| 507 Insufficient Storage | `remote_state` row persists | Disk must be freed externally. Reconciler retries at 1hr cap |
| 400 Bad Request (upload) | `local_issues` row | User must rename file. Visible in `issues` CLI |
| 423 Locked (upload) | Local file differs from baseline | Self-heals when lock released. Natural planner retry |
| 403 Forbidden (upload) | Local file differs from baseline | User must fix permissions. Visible in `issues` CLI |
| FS read errors (upload) | Local file differs from baseline | User must fix permissions. Visible in `issues` CLI |

### Non-empty directory deletes

When delta reports a folder deletion, the executor checks `os.ReadDir()` and
fails immediately if the directory is non-empty — classified as `errClassSkip`.
The planner's dependency DAG ensures children are deleted before parents, but
dependencies gate on *completion*, not *success*.

In the new design, the `pending_delete` row persists. The reconciler retries
with exponential backoff, capping at 1 hour. But the directory will never become
empty through retries alone.

**Escape hatch: conflict escalation.** After `failure_count` reaches a threshold
(e.g., 10 — roughly 5 hours of retries), the reconciler escalates to a conflict.
The conflict record explains: "Remote deleted folder X, but local directory is
not empty." The user can resolve by deleting local contents or keeping local
version.

### Global auth failure

401 failures are recorded in `remote_state` like any other failure. The
reconciler retries with exponential backoff. If the token is expired globally,
every action in the batch fails with 401.

**Detection.** When `drainWorkerResults` sees a cluster of 401 failures (>50% of
a batch), it logs: `slog.Error("auth failure: most actions in batch failed with
401, refresh token may be expired")`. The user runs `onedrive-go login` to
re-authenticate.

**Why not shut down on 401?** A single 401 doesn't mean global auth failure — it
could be a permission issue on a specific SharePoint resource. The cluster
detection provides signal without a hard stop.

**Why not auto-refresh?** Token refresh requires the OAuth refresh token. The
graph client already handles automatic refresh via `oauth2.TokenSource`. If the
refresh token itself is expired (90-day idle), interactive re-authentication is
required.

---

## 18. Compatibility

### Dry-run behavior

During `sync --dry-run`, the observer runs and produces in-memory ChangeEvents
(needed for the report), but `CommitObservation` is not called — no
`remote_state` writes, no token advancement. This matches rclone's precedent and
user expectation: "dry-run is read-only." The dry-run gate already exists at
`engine.go:266`.

### RunOnce (one-shot sync)

RunOnce writes `remote_state` at observation time, processes events, updates
`remote_state` rows on success. After a successful one-shot sync, all rows are
`synced`.

**No reconciler goroutine.** The reconciler is a watch-mode construct
(long-lived, timer-based). RunOnce is a blocking single-cycle call. Instead,
RunOnce queries unreconciled `remote_state` rows directly at startup and merges
them into the planning pass:

```
RunOnce(ctx, mode, opts)
  ├─ baseline.Load()
  ├─ observeChanges()
  │   ├─ observeRemote() + FullDelta → fresh delta events
  │   ├─ store.ListUnreconciled() → orphaned rows from previous crash/failure
  │   ├─ Synthesize ChangeEvents from orphaned rows
  │   ├─ observeLocal() + FullScan
  │   └─ buf.AddAll(fresh + orphaned + local).FlushImmediate()
  ├─ planner.Plan()  [one pass, sees everything]
  ├─ executePlan()
  └─ return SyncReport
```

The buffer deduplicates by path: if a fresh delta event and an orphaned row
exist for the same path, the buffer takes the latest (fresh delta wins). This is
a single query + event synthesis in `observeChanges()`, not a new goroutine.

**Why no reconciler for RunOnce?** RunOnce might finish and exit before any
timer-based retries could fire. The next RunOnce invocation serves the same
purpose — it queries unreconciled rows on startup. Failed items persist in
`remote_state` and are retried on the next RunOnce.

### RunWatch (daemon mode)

Events arrive at any rate. `remote_state` gets updated to the latest observed
values (Graph coalesces intermediate states). Multiple planning passes can
overlap. Path dedup (B-122) cancels stale in-flight actions. No cycles exist —
batches are debounce windows.

### WebSocket (Phase 8)

WebSocket notifications trigger immediate delta polls (Flow 1). Everything
downstream — `remote_state` write, token commit, buffer, planner, workers,
reconciler — is unchanged.

### Initial sync

On initial sync (empty token), FullDelta returns all remote items — potentially
tens of thousands. All are written to `remote_state` in one transaction
(~200-500ms for 50K items with WAL mode). As actions succeed, rows transition to
`synced`. No special case needed.

### Status command

The `status` command gains a failure count by querying `remote_state`:

```sql
SELECT COUNT(*) FROM remote_state
WHERE sync_status NOT IN ('synced', 'filtered', 'deleted')
```

Displayed as "X items pending sync" alongside existing "Y unresolved conflicts."

---

## 19. Crash Recovery

Explicit state enables unambiguous recovery:

| DB state at restart | Recovery action |
|----|-----|
| `downloading` | Check `.partial`. Valid session → resume. Else → reset to `pending_download`. **(R18)** |
| `deleting` | Check if local file exists. If yes → `pending_delete`. If no → `deleted`. **(R19)** |
| `pending_download` | Reconciler reschedules. |
| `download_failed` | Reconciler reschedules per backoff. |
| `synced` | No action. |
| `filtered` | No action. |
| `deleted` | No action. |

**R18 detail** (corrupt `.partial`): On crash recovery with `downloading` state,
query TransferManager for session state. If session is valid, resume. If session
expired or invalid, delete `.partial` and reset to `pending_download`. The
TransferManager already handles session expiry.

**R19 detail** (partial folder delete): The planner's dependency DAG ensures
children delete before parents. On restart, the reconciler resets the parent to
`pending_delete`. The planner regenerates the delete action. If remaining
children still exist, the DAG re-orders correctly. Idempotent.

---

## 20. Critical Design Invariants

1. **Every DB state write uses a WHERE clause checking expected current state.**
   No blind UPDATEs.
2. **CommitOutcome includes `AND hash IS ?`** — prevents overwriting newer
   observation with stale result.
3. **CommitObservation never regresses `synced → pending_download` when hashes
   match.**
4. **Reconciler only dispatches `pending_download` or `download_failed`.** Never
   `downloading`, `synced`, `filtered`, `deleted`.
5. **CancelByPath is a performance optimization, not a correctness mechanism.**
   System is correct even if cancel doesn't fire.

---

## 21. Risk Analysis

All 22 identified risks with severity, resolution, and where resolved:

| Risk | Sev | Category | Decision | §  |
|------|-----|----------|----------|----|
| R1: State ownership | Med | State machine | Partitioned interfaces + optimistic concurrency | 10.1 |
| R2: Dual tracking | Med | State machine | sync_status authoritative, DepTracker as cache | 10.2 |
| R3: B-122 cancel | Low | State machine | Do nothing — optimistic concurrency handles it | 10.3 |
| R4: Synced regression | High | State machine | `computeNewStatus()` pure function in Go | 11 |
| R5: Buffer re-dispatch | Low | State machine | Do nothing — reconciler query + B-122 dedup | 10.4 |
| R6: 100K row scan | Low | Full mirror | Index on sync_status | 5 |
| R7: WAL growth | Med | Full mirror | Periodic checkpoint — hard requirement | 5 |
| R8: UPDATE vs DELETE | Low | Full mirror | Conditional UPDATE identical semantics | 5 |
| R9: Folder rename cascade | Med | ID-based PK | Eager: O(N) writes in same txn | 6 |
| R10: Transient path collision | Low | ID-based PK | Atomic txn + B-281 ordering | 6 |
| R11: local_issues asymmetry | None | Upload | By design — path PK for pre-upload items | 14 |
| R12: Invisible filtered items | None | Filtering | Correct — `filtered` state deliberate | 13 |
| R13: Stale local files | None | Filtering | Correct — warn/freeze (FC-9) | 13 |
| R14: Two-stage filtering | None | Filtering | Defense in depth, same Filter.ShouldSync() | 13 |
| R15: Timer re-arm | Low | Reconciler | Re-arm after pass, Kick catches new items | 16 |
| R16: Timer vs shutdown | Low | Reconciler | Channel + ctx.Done() guard | 16 |
| R17: Thundering herd | Low | Reconciler | Buffer debounce + worker backpressure | 16 |
| R18: Corrupt .partial | Med | Crash recovery | Session validation → fallback | 19 |
| R19: Partial folder delete | Low | Crash recovery | Idempotent DAG replay | 19 |
| R20: Tier 1 coverage gaps | Med | Research | Document scope boundaries | 23 |
| R21: Filter performance | Low | Filtering | Deferred to Phase 10 | 13 |
| R22: Event ordering | Med | Buffer | Buffer takes LAST event; conditional updates catch stale | 10.4 |

---

## 22. Assessment Against Existing Architecture

### What survives unchanged

- **Planner**: Pure function, same decision matrix (EF1-EF14, ED1-ED8). Same
  `PathView` types.
- **Executor**: Same retry logic, error classification, `Outcome` production.
- **Buffer**: Same debounce + dedup. Reconciler events enter via `buf.Add()`.
- **Transfer machinery**: `TransferManager`, `SessionStore`, `.partial` files —
  all unchanged.
- **Observers**: `LocalObserver` unchanged. `RemoteObserver` gains
  `CommitObservation` call.
- **Safety checks**: S1-S7 in planner, S4 in executor — all unchanged.
- **B-122 path dedup**: Works the same, now also deduplicates against reconciler
  events.

### What gets removed

- `failure_tracker.go` (97 lines) — replaced by `remote_state` + reconciler.
- `CycleID` concept (~50 lines across planner, tracker, worker, types, engine).
- `watchCycleCompletion` (~40 lines in engine.go).
- `cycleFailures` map (~15 lines in engine.go).
- Cycle tracking in DepTracker (`CycleDone`, `CleanupCycle`, `cycles`,
  `cycleLookup` — ~80 lines).
- Existing migrations (00001-00005) — replaced by single clean schema.

### What gets added

- `remote_state` table (14 columns, full mirror).
- `local_issues` table (upload tracking).
- `SyncStore` (renamed from `BaselineManager`): ~200 lines for
  CommitObservation, RecordFailure, ListUnreconciled, etc. + typed interfaces.
- `Reconciler`: ~200 lines.
- `computeNewStatus()`: ~60 lines (pure function).
- `issues` CLI command: ~50 lines.

### What gets modified

- `engine.go`: Removes cycle machinery, adds reconciler lifecycle, changes
  `drainWorkerResults`.
- `baseline.go` → `sync_store.go`: Rename + new methods + ID-based PK.
- `observer_remote.go`: Add `CommitObservation` call after each delta poll.
- `tracker.go`: Removal of cycle tracking + Add() writes to DB (Option D).
- `worker.go`: `WorkerResult` gains `HTTPStatus`, `ErrorClass`. Loses
  `CycleID`.

### Interface migration

| Caller | Old type | New type | Interface |
|--------|----------|----------|-----------|
| RemoteObserver | `*Baseline` | `ObservationWriter` | CommitObservation, GetDeltaToken |
| WorkerPool | `*BaselineManager` | `OutcomeWriter` | CommitOutcome, Load |
| drainWorkerResults | N/A | `FailureRecorder` | RecordFailure |
| Reconciler | N/A (new) | `StateReader` | ListUnreconciled, etc. |
| CLI commands | `*BaselineManager` + `DB()` | `StateReader` + `StateAdmin` | Read + admin writes |
| Engine | all | all | Orchestrator |

---

## 23. Tier 1 Research Alignment

### PRD Assessment

- **"Zero data loss crash recovery"**: Currently violated. Proposed design
  fulfills it.
- **RunOnce recovery**: Token committed at observation time.
  `ListUnreconciled()` recovers orphaned rows.
- **RunWatch recovery**: `remote_state` persists across crashes. Reconciler
  bootstraps from DB.
- **Pause/resume**: Reconciler handles the gap.
- **`issues` CLI**: New capability — makes persistent failures visible in
  enterprise environments.

### Tier 1 Research Recommendations (20 items)

| # | Recommendation | Status | How Addressed |
|---|---|---|---|
| 1 | Three-state sync model | Already exists | `baseline` has `local_hash` + `remote_hash` |
| 2 | Atomic downloads | Already exists | `.partial` files + atomic rename |
| 3 | DriveId normalization | Already exists | `driveid.New()` normalizes |
| 4 | Two-pass delta processing | Already exists | `observer_remote.go` B-281 |
| 5 | Pending state machine | **Implemented** | Explicit `sync_status` with state machine |
| 6 | Delta token in same txn | **Core of this design** | `CommitObservation` writes rows + token atomically |
| 7 | Big-delete protection | Already exists | S5 safety check |
| 8 | Hash-before-delete guard | Already exists | S4 safety check |
| 9 | Retry-After compliance | Already exists | `graph.Client` retry logic |
| 10 | Upload session persistence | Already exists | File-based `SessionStore` |
| 11 | WAL checkpoint | **Implemented** | Periodic checkpoint after initial sync, every 30min, shutdown |
| 12 | Circuit breaker | **Implemented** | Per-item backoff (1hr cap) + 401 cluster detection |
| 13 | PID-based lock validation | Already exists | Daemon PID file |
| 14 | Token proactive refresh | Already exists | `oauth2.TokenSource` |
| 15 | ID-based identity as PK | **Implemented** | `(drive_id, item_id)` as PK, `path UNIQUE` |
| 16 | `deltashowremoteitemsaliasid` | Already exists | Remote observer sends header |
| 17 | 504 safety | Already exists | Executor: 504 → retryable |
| 18 | Tombstone/soft-delete | **Implemented** | `sync_status = 'deleted'` in full remote mirror |
| 19 | Nanosecond timestamps | Deferred | Future optimization phase |
| 20 | Inode number in database | Deferred | Future fast-check optimization phase |

### The blank-slate verdict

From a blank slate, this IS the right architecture:

1. Persist remote observations immediately on receipt ✓
2. Advance API cursor independently of sync success ✓
3. Reconcile from persistent state discrepancies ✓
4. Use level-triggered reconciliation with exponential backoff ✓
5. Full remote mirror (not work queue) ✓
6. ID-based primary key (not path-based) ✓
7. Explicit state machine (not implicit inference) ✓
8. Partitioned interfaces with optimistic concurrency ✓
9. Upload failure tracking with pre-validation ✓
10. Unified filtering architecture ✓

---

## 24. Testing Strategy

### Layer 1: Unit tests (real SQLite, in-memory)

- `CommitObservation`: write N events + token, verify rows and token.
- `CommitObservation` with existing rows: handle all 30 matrix cells.
- `CommitOutcome` with `remote_state` update: hash match → `synced`; hash
  mismatch → row persists; NULL hash → `IS` works.
- `ListUnreconciled` query: correct rows returned, respecting `next_retry_at`.
- `RecordFailure`: updates `failure_count`, `next_retry_at`, `last_error`,
  `http_status`.
- Reconciler level-triggered behavior: `Kick()` coalesces, reconcile reads DB
  and diffs timer map.
- Reconciler re-read cases: row gone → skip, hash changed → new hash, in-flight
  → skip + reschedule, unchanged → dispatch.

### Layer 2: Integration tests (real SQLite, mock Graph API)

- Full cycle: observe → fail → Kick → reconcile → timer fires → succeed.
- Crash recovery: write + "crash" + restart → reconciler bootstraps from DB.
- Conditional update race (Scenario 4): R1 succeeds but R2 persists.
- Backoff escalation: 3+ failures → `next_retry_at` set → respected → success
  clears.

### Layer 3: Engine tests (full pipeline with mocks)

- Token always advances regardless of failures.
- Reconciler schedules retries correctly.
- Dry-run: no DB writes.
- Planner works with reconciler-sourced events.
- DepTracker: no regressions in dispatch + HasInFlight + CancelByPath.

### Layer 4: E2E tests (live OneDrive, `e2e_full` tag)

- Reconciliation recovery: upload file, make download fail, fix, verify
  recovery.
- `issues` CLI: list, clear, JSON output.
- `status` command shows pending sync count.

### Layer 5: Property-based tests (optional, high value)

- Invariant: for any sequence of operations, every remote item is either in
  `baseline` (synced) or `remote_state` (pending).

### Layer 6: Stress tests (no Graph API, in-memory SQLite)

1. **1000 simultaneous failures → reconciler convergence**: All fail with 423.
   Assert correct `sync_status`, `failure_count`, single timer armed.
2. **Rapid kick/sweep interleaving**: 100 rows, 1000 rapid Kicks, 50% success
   rate. Assert no races.
3. **Fresh delta overwrites during retry**: 10 backed-off items, 5 overwritten.
   Assert `failure_count` reset.
4. **Conditional update race**: Hash mismatch prevents overwriting newer version.
   Assert `pending_download` preserved.
5. **Filter exclusion marking**: 10 items, 5 filtered. After SIGHUP, all
   re-evaluated.
6. **Upload pre-validation**: Invalid filenames, paths too long, too large.
   Assert `local_issues`, no upload actions.
7. **`computeNewStatus()` exhaustive matrix**: All 30 cells — table-driven test.
8. **CancelByPath + CommitOutcome race**: 3 scenarios. Assert correct outcome in
   all cases.

---

## 25. Migration Path

### Database migration

Zero users. All migrations (00001-00005) deleted. Single
`00001_initial_schema.sql` with all three tables.

### Code removals

```bash
grep -rn 'CycleID\|CycleDone\|CleanupCycle\|cycleFailures\|cycleLookup\|shouldSkip\|watchCycleCompletion\|failureTracker\|FailureTracker\|failure_tracker\|BaselineManager' \
    internal/sync/ --include='*.go' | grep -v '_test.go' | grep -v 'design'
```

Plus: `uuid` import in planner, `cycleTracker` struct, `suppressedPaths`,
`NewFailureTracker()` call.

### Verification checklist

After migration, grep the codebase for these terms. **None should appear**
(except in comments explaining the removal or in this design doc):

- `CycleID` / `cycleID` / `cycle_id`
- `CycleDone` / `CleanupCycle`
- `cycleFailures` / `cycleLookup`
- `shouldSkip` (in the failure-suppression sense)
- `watchCycleCompletion`
- `failureTracker` / `FailureTracker` / `failure_tracker`
- `NotifySuccess` / `NotifyFailure` (on reconciler)
- `remoteStateCleared` / `RemoteStateCleared`
- `BaselineManager` (renamed to `SyncStore`)

### Documents to update

| Document | Change |
|----------|--------|
| `data-model.md` | Update axiom to include `remote_state`. Add table to schema. Rename `BaselineManager` → `SyncStore` |
| `concurrent-execution.md` | Update "baseline is only durable per-item state". Document sub-interface distribution |
| `event-driven-rationale.md` | Update Alternative B notes: adopted for remote side with sub-interface solution |
| `LEARNINGS.md` | Add: "delta token is API cursor, not sync cursor" |
| `CLAUDE.md` | Add `issues` to CLI command list. Rename `BaselineManager` → `SyncStore` |

---

## 26. Verdict

**This is the right architecture. Implement it.**

The design:

1. **Solves a real, critical bug** — silent data loss in the most common failure
   scenario.
2. **Follows industry best practice** — Dropbox's separation of observation from
   application.
3. **Is validated by tier 1 research** — addresses the two most critical gaps
   (#5 pending state, #6 token atomicity).
4. **Is proportional** — ~500 new lines, ~280 removed, surgical modifications
   to existing code.
5. **Doesn't regress any existing capability** — planner, executor, buffer,
   transfer machinery unchanged.
6. **Simplifies the architecture** — removes the cycle concept, failure tracker,
   cycle-gated token commits.
7. **Enables future features** — multi-drive, WebSocket, filtering,
   observability all build naturally on this foundation.
8. **Is the blank-slate answer** — this is what you'd build from scratch.

---

## Relationship to Existing Documents

This design supersedes:

- The persistent failure tracker plan
- The in-memory failure tracker (`failure_tracker.go`, `failure_tracker_test.go`)
- The delta token hold-back logic in `watchCycleCompletion`
- The `cycleFailures` map and cycle success/failure concept
- The DepTracker cycle tracking machinery
- The `CycleID` field on `ActionPlan`, `TrackedAction`, `WorkerResult`
- The `BaselineManager` name and monolithic API surface
- The "baseline is the only durable per-item state" axiom
- The "database stores confirmed synced state and nothing else" axiom

It builds on:

- [failures.md](failures.md) — failure enumeration, delta token bug analysis
- [sync-algorithm.md](sync-algorithm.md) — planner decision matrix (unchanged)
- [event-driven-rationale.md](event-driven-rationale.md) — architectural
  decisions (extended — Alternative B adopted for remote side)
- [filtering-conflicts.md](filtering-conflicts.md) — FC-1 through FC-12
  analysis
