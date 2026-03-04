# onedrive-go — Implementation Roadmap

> **ZERO PATH DEPENDENCY**: This document describes the Option E event-driven
> architecture, designed from first principles. See
> [design/event-driven-rationale.md](design/event-driven-rationale.md) for the full rationale.

## Principles

- Each increment is completable in one focused session
- Each increment has clear acceptance criteria (build + test + lint pass)
- Design docs in `docs/design/` are the spec — use plan mode before each increment for file-level planning
- CLI-first: build a working tool before building the sync engine

---

## Phase 1: Graph Client + Auth + CLI Basics — COMPLETE

Graph API client (HTTP transport, retry, rate limiting, auth, items CRUD, delta with normalization, transfers, drives) and CLI (login/logout/whoami, ls/get/put/rm/mkdir/stat). 8 increments. ~91% package coverage.

---

## Phase 2: E2E CI — COMPLETE

Azure OIDC federation, Key Vault token storage, E2E tests against live Graph API (whoami, ls, mkdir, put, get, stat, rm, chunked upload, Unicode, concurrent ops). 3 increments.

---

## Phase 3: Config Integration — COMPLETE

TOML config (XDG paths, env overrides, four-layer override chain), drive sections with canonical IDs, `PersistentPreRunE` integration. 3 increments. 95.6% config coverage.

---

## Phase 3.5: Account/Drive System Alignment — COMPLETE

Profile-based → flat drive-section config. `["personal:user@example.com"]` format, `ResolveDrive()`, drive matching. Graph/auth decoupled from config. 2 increments, -414 lines net.

---

## Phase 4 v1: Batch-Pipeline Sync Engine — SUPERSEDED

Built and then deleted. Six structural fault lines (tombstone split, SQLITE_BUSY, incomplete folder lifecycle, pipeline phase ordering, asymmetric filtering, local:ID lifecycle) all traced to one root cause: database as coordination mechanism. See [event-driven-rationale.md](design/event-driven-rationale.md). All code deleted in 4v2.0.

---

## Phase 4 v2: Event-Driven Sync Engine — COMPLETE

**The core architectural pivot.** Observers → typed events → buffer → planner (pure function) → executor → outcomes → atomic baseline commit. Same decision matrix (EF1-EF14, ED1-ED8), same safety invariants (S1-S7), completely different data flow.

| Increment | Description |
|-----------|-------------|
| 4v2.0 | Clean slate — deleted ~16,655 lines of old sync code |
| 4v2.1 | Types + baseline schema + BaselineManager (SQLite, WAL mode, goose migrations) |
| 4v2.2 | Remote observer — `FullDelta()` with path materialization, NFC normalization, move detection |
| 4v2.3 | Local observer — `FullScan()` with baseline comparison, `.nosync` guard, OneDrive name validation, QuickXorHash |
| 4v2.4 | Change buffer — thread-safe `Add/AddAll/FlushImmediate`, move dual-keying |
| 4v2.5 | Planner — 5-step pipeline, 14 file + 8 folder decision matrix, move detection, big-delete safety |
| 4v2.6 | Executor — 9-phase execution, parallel workers, `.partial` downloads, chunked uploads, S4 hash-before-delete |
| 4v2.7 | Engine wiring — `RunOnce()` with full pipeline, CLI `sync` command |
| 4v2.8 | CLI integration — `conflicts`, `resolve`, `verify` commands, sync E2E tests |

---

## Phase 5: Concurrent Execution + Watch Mode

> **CLEAN SLATE INVARIANT**: When complete, the codebase must appear as if written from scratch for the concurrent execution architecture. No vestiges of sequential phase execution, batch commits, or phase-ordered action plans. See `docs/design/concurrent-execution.md`.

### 5.0: DAG-based Concurrent Execution Engine — DONE

**THE ARCHITECTURAL PIVOT.** Replaced 9-phase sequential executor with flat action list + dependency DAG + concurrent workers. `ActionPlan` restructured (9 slices → flat `Actions []Action` + `Deps [][]int`), `DepTracker` for dispatch, `WorkerPool` with per-action atomic commits, `Baseline` gains `sync.RWMutex` + locked accessors. Net -416 lines. See [legacy-sequential-architecture.md](design/legacy-sequential-architecture.md) for removal verification patterns.

### 5.1: Continuous Observer Watch() + Debounced Buffer — DONE

`RemoteObserver.Watch()` (delta polling with backoff), `LocalObserver.Watch()` (fsnotify + 5-min safety scan), `Buffer.FlushDebounced()` (timer-based batching). Added `fsnotify/fsnotify` v1.9.0. 17 new tests.

### 5.2: RunWatch + Parallel Observation — DONE

| Sub-increment | What |
|---------------|------|
| 5.2.0 | Parallel remote + local observation in `RunOnce()` via `errgroup` |
| 5.2.1 | Parallel `FullScan` hashing (`errgroup.SetLimit(NumCPU)`) — 8× speedup for initial sync |
| 5.2.2 | `Engine.RunWatch()` — continuous pipeline, per-path dedup (B-122), delta token per cycle (B-121) |

### 5.3: Graceful Shutdown + Crash Recovery — DONE

Two-signal shutdown (SIGINT = drain, second = force), failure tracker for watch mode (B-123), drive identity verification (B-074), resumable downloads (`.partial` + `DownloadRange`), resumable uploads (`ResumeUpload` + `SessionStore`), stable hash detection (B-119).

### 5.4: Universal Transfer Resume — DONE

| Sub-increment | What |
|---------------|------|
| 5.4 | Unified `TransferManager` (download/upload with resume), shared between CLI and sync engine. File-based `SessionStore` (JSON, SHA256-keyed, 7-day TTL). Dropped `upload_sessions` table. |
| 5.4.2 | 18 hardening fixes (3 critical: `.partial` preserve on Ctrl-C, `withRetry` for transfers, worker panic recovery; 4 high: `UploadResult` fields, nil-check, deferred construction, Close error check) |

### 5.5: Pause/Resume + Config Reload — DONE

Config migration (`Enabled` → `Paused`), `drive remove` deletes config section, `pause`/`resume` CLI commands (with timed pause via duration arg), SIGHUP config reload in `sync --watch`, PID file with flock. Legacy sweep verified against [legacy-sequential-architecture.md](design/legacy-sequential-architecture.md).

### 5.6: Identity Refactoring + Personal Vault Exclusion — DONE

6 sub-increments: Personal Vault exclusion (`sync_vault` config), `DriveTypeShared` in `driveid`, token resolution moved to `config.TokenCanonicalID()`, `Alias` → auto-derived `DisplayName` + `Owner`, CLI display_name integration, delta token composite key `(drive_id, scope_id)`. Net: 26 files, ~1500 lines.

### 5.7: Remote State Separation

**Goal**: Fully implement the remote-state-separation architecture described in [remote-state-separation.md](design/remote-state-separation.md). This replaces the fragile "baseline-only + in-memory failure tracker" model with durable `remote_state` tracking, an explicit 9-value state machine, a dedicated reconciler goroutine for retry, and persistent upload failure tracking via `local_issues`. Eliminates the delta token advancement bug (silent data loss on download failure) and the cycle concept entirely.

**Dependency graph** (strictly linear):
```
5.7.0 (DONE) → 5.7.1 (IN PROGRESS) → 5.7.2 → 5.7.3 → 5.7.4
```

**Design reference**: Every increment implements sections of [remote-state-separation.md](design/remote-state-separation.md). Section numbers (§N) are cited inline.

##### 5.7.0: Schema + SyncStore Foundation + computeNewStatus() — **DONE**

Consolidated migrations into single `00001_consolidated_schema.sql` with `remote_state` (16 cols, 9-value state machine) and `local_issues` (10 cols) tables. Renamed `BaselineManager` → `SyncStore`. Added `computeNewStatus()` pure function (30-cell decision matrix, §11). Added 6 sub-interface declarations + `ObservedItem`/`RemoteStateRow` structs. Changed baseline PK from `path` to `(drive_id, item_id)` with `path UNIQUE`. Net: 6 new files, 12 modified, 33 new tests.

##### 5.7.1: Remote State Observation Layer + Filtering Symmetry — **IN PROGRESS**

**Goal**: Wire remote state persistence into the live sync path so delta observations are durable and the delta token never advances without recording what we learned. Fix filtering asymmetry before wiring to prevent junk from polluting `remote_state` from day one.

**Design doc sections**: §8 Flow 1 (remote delta poll), §11 (CommitObservation logic), §13 (filtering architecture FC-1, FC-2), §17 (error handling — RecordFailure), §25 (code removals — legacy cycle tracking).

**Preconditions**: 5.7.0 complete (schema exists, interfaces declared, `computeNewStatus()` implemented).

**What gets built** (new methods on `*SyncStore`):
1. `CommitObservation(ctx, events []ObservedItem, newToken string, driveID driveid.ID) error` — BEGIN TRANSACTION → UPSERT each event to `remote_state` using `computeNewStatus()` → upsert delta token → COMMIT. Atomic: token never advances without observations persisted. Implements §11 full decision matrix.
2. `RecordFailure(ctx, driveID driveid.ID, itemID, errMsg string, httpStatus int) error` — UPDATE `remote_state` SET `sync_status = '*_failed'`, `failure_count = failure_count + 1`, `next_retry_at = ?` (backoff formula), `last_error = ?`, `http_status = ?` WHERE `sync_status IN ('downloading', 'deleting')`. Implements §8 Flow 6.
3. `StateReader` query methods: `ListUnreconciled()`, `ListFailedForRetry()`, `FailureCount()` — read-only queries against `remote_state`. Also `ResetInProgressStates()` on `StateAdmin`.
4. `ChangeEvent` ↔ `ObservedItem` converter function — translates between the observer's `ChangeEvent` (pipeline currency) and `ObservedItem` (DB currency).
5. Remote observer filtering symmetry (B-307): `classifyItem()` in `observer_remote.go` applies the same built-in exclusion rules as the local observer (`.partial`, `.tmp`, `~$*`, invalid OneDrive names). Prevents junk from entering `remote_state` or the event pipeline.
6. Narrow `.db` exclusion (B-308): change the `.db` built-in filter from "all `.db` files" to "only the sync engine's own DB path." Legitimate `.db` files sync correctly.

**What changes** (existing code modified):
1. `RemoteObserver` — receives `ObservationWriter` interface. After `FullDelta()` returns, calls `store.CommitObservation()` before sending events to the channel. Delta token is now committed inside `CommitObservation` (atomically with observations), so the engine's separate `CommitDeltaToken()` call after plan execution is removed for the remote-observation path.
2. `Engine.RunOnce()` — `observeChanges()` updated: remote observer calls `CommitObservation` during observation. Engine no longer calls `CommitDeltaToken()` after successful execution (token already committed). On startup, queries `ListUnreconciled()` to recover orphaned rows from previous crash/failure, synthesizes `ChangeEvent`s, merges into planning pass (§18 RunOnce compatibility).
3. `Engine.RunWatch()` — `drainWorkerResults` calls `store.RecordFailure()` on action failure instead of the in-memory `failureTracker.recordFailure()`. `processBatch` removes `shouldSkip` check (replaced by DB-persisted failure state + reconciler in 5.7.3).
4. `WorkerResult` struct — gains `HTTPStatus int` and `DriveID driveid.ID` + `ItemID string` fields so `drainWorkerResults` can pass them to `RecordFailure`.

**What gets removed**:
1. `failureTracker` struct, `failure_tracker.go`, `failure_tracker_test.go` — replaced by `RecordFailure` + `remote_state` persistence.
2. `CycleID` field from `WorkerResult`, `TrackedAction` — no more cycle concept.
3. `cycleTracker` struct, `CycleDone()`, `CleanupCycle()` from `DepTracker` — cycle-gated token commit eliminated (token commits atomically with observation, not gated on action success).
4. `cycleFailures` map + `cycleFailuresMu` from `Engine` — delta token no longer held back on failure.
5. `shouldSkip` suppression logic in `processBatch`.
6. `watchCycleCompletion` / cycle-tracking goroutine in `drainWorkerResults`.

**Behavioral changes**:
- Delta token ALWAYS advances after observation (not held back on action failure). This is the critical bug fix — previously, a failed download caused the token to be held back, and if the daemon restarted, it replayed the delta including already-succeeded items.
- Failed actions are durably recorded in `remote_state` with failure metadata (count, timestamp, error, HTTP status). Previously, failures were tracked in-memory only and lost on restart.
- Junk files (`.tmp`, `~$*`, etc.) are now filtered at the remote observer, not just the local observer (B-307).

**Tests**:
- `CommitObservation`: write N events + token, verify rows and token. All 30 matrix cells via `computeNewStatus` already tested.
- `CommitObservation` idempotency: write same events twice, verify no state regression.
- `RecordFailure`: verify `failure_count` increment, `next_retry_at` calculation, `last_error`/`http_status` persistence. Verify WHERE clause only matches `downloading`/`deleting`.
- Remote observer filtering: verify `.tmp`, `~$*`, invalid names dropped. Verify legitimate `.db` files pass.
- Engine integration: verify `CommitDeltaToken` no longer called separately. Verify delta token committed atomically with observations.
- RunOnce orphan recovery: create unreconciled rows, run RunOnce, verify they're picked up.

**State of CI after 5.7.1**:
- Observations flow into `remote_state`. Delta token is durable and atomic.
- Failures are persisted to `remote_state` with metadata.
- BUT: `CommitOutcome` does not yet update `remote_state` on success → rows transition to `downloading`/`deleting` but never reach `synced`/`deleted`. The existing baseline-based sync pipeline continues to work correctly for actual file operations. `remote_state` accumulates rows that don't reach terminal state — this is expected and corrected in 5.7.2.
- No reconciler → failed items sit in `*_failed` state until next RunOnce startup query or daemon restart. Watch mode has no automatic retry for remote failures. This is corrected in 5.7.3.
- All existing unit, integration, and E2E tests pass. No behavioral regression for file operations.

**Acceptance**:
- `grep -rn 'failureTracker\|FailureTracker\|failure_tracker\|shouldSkip\|cycleFailures\|cycleLookup\|CycleDone\|CleanupCycle\|watchCycleCompletion' internal/sync/ --include='*.go' | grep -v '_test.go' | grep -v 'design'` → 0 hits.
- `grep -rn 'CycleID' internal/sync/ --include='*.go' | grep -v '_test.go'` → 0 hits.
- CommitObservation and RecordFailure have compile-time interface guards: `var _ ObservationWriter = (*SyncStore)(nil)`, `var _ FailureRecorder = (*SyncStore)(nil)`.

##### 5.7.2: Close the State Machine Loop — CommitOutcome + Dispatch Transitions + Crash Recovery

**Goal**: Complete the `remote_state` lifecycle so rows transition through the full state machine: `pending_download → downloading → synced` (or `*_failed`). After this increment, `remote_state` accurately reflects the sync status of every remote item at all times, and crash recovery is unambiguous.

**Design doc sections**: §7 (state transition ownership — CommitOutcome, DepTracker.Add), §8 Flow 5 (action success), §10 (concurrency safety — optimistic WHERE clauses, DepTracker as read-through cache), §15 (CommitOutcome SQL), §19 (crash recovery), §20 (critical design invariants 1-4).

**Preconditions**: 5.7.1 complete (CommitObservation wired, RecordFailure wired, legacy cycle tracking removed, WorkerResult has HTTPStatus/DriveID/ItemID).

**What gets built** (new code):

1. **Dispatch state transitions in engine** — When the engine dispatches an action to the worker pool (during `processBatch` or `executePlan`), it writes the dispatch transition to `remote_state` before calling `tracker.Add()`:
   - For download actions: `UPDATE remote_state SET sync_status = 'downloading' WHERE drive_id = ? AND item_id = ? AND sync_status IN ('pending_download', 'download_failed')`
   - For delete actions: `UPDATE remote_state SET sync_status = 'deleting' WHERE drive_id = ? AND item_id = ? AND sync_status IN ('pending_delete', 'delete_failed')`
   - Uses optimistic WHERE clause (§10 Race 3) — if the row has already transitioned (e.g., a fresh CommitObservation overwrote it), the UPDATE affects 0 rows. This is not an error — it means the action is stale and its eventual CommitOutcome will be a no-op too.
   - The engine performs this write, not DepTracker. DepTracker stays pure in-memory (no DB dependency). The engine sequence is: write dispatch transition → `tracker.Add()` → action enters ready channel.
   - **Batch optimization** (§10.2): during `executePlan` in RunOnce (which dispatches all actions at once), wrap all dispatch transitions in a single BEGIN/COMMIT transaction. For 100K items this takes ~0.5s vs ~10s for individual writes.

2. **CommitOutcome remote_state updates** — Extend each `applySingleOutcome` handler to also update `remote_state` in the same transaction as the baseline write. All SQL uses WHERE clauses checking expected current state (invariant §20.1):

   **Download success** (`commitUpsert` for ActionDownload):
   ```sql
   UPDATE remote_state SET sync_status = 'synced'
   WHERE drive_id = ? AND item_id = ?
     AND sync_status = 'downloading' AND hash IS ?
   ```
   The `AND hash IS ?` guard (invariant §20.2) prevents a stale download result from overwriting a newer observation. If `remote_state` now has hash R2 but the worker downloaded R1, the WHERE doesn't match → 0 rows affected → row stays in `downloading` with R2's metadata → reconciler (5.7.3) picks it up. This is §8 Flow 5 "stale success" scenario (conflict scenario 4).

   **Delete success** (`commitDelete` for ActionDelete):
   ```sql
   UPDATE remote_state SET sync_status = 'deleted'
   WHERE drive_id = ? AND item_id = ? AND sync_status = 'deleting'
   ```

   **Upload success** (`commitUpsert` for ActionUpload) (invariant §20.3):
   ```sql
   INSERT INTO remote_state (drive_id, item_id, path, parent_id, item_type,
     hash, size, mtime, etag, sync_status, observed_at)
   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
   ON CONFLICT(drive_id, item_id) DO UPDATE SET
     sync_status = 'synced', hash = excluded.hash, size = excluded.size,
     mtime = excluded.mtime, etag = excluded.etag, path = excluded.path
   ```
   This is an UPSERT, not a conditional UPDATE, because:
   - New uploads (create): no `remote_state` row exists yet → INSERT as `synced`.
   - Existing items (edit): row may be in any state → unconditionally set `synced` with server-returned metadata.
   - Without this, the delta echo after upload creates an infinite `pending_download` loop (§15 — the hash in `remote_state` wouldn't match the uploaded hash, so `computeNewStatus` would trigger re-download).
   - The `Outcome` struct already carries `ItemID`, `DriveID`, `RemoteHash`, `ETag`, `Size`, `Mtime` from the server response.

   **Move success** (`commitMove`):
   ```sql
   UPDATE remote_state SET sync_status = 'synced', path = ?
   WHERE drive_id = ? AND item_id = ?
     AND sync_status IN ('downloading', 'pending_download')
   ```
   Moves don't change content, so no hash guard needed. The item's `remote_state` row was updated to the new path by CommitObservation; here we just mark it synced.

3. **Crash recovery — `ResetInProgressStates(ctx)`** on `StateAdmin` (§19):
   - Query all rows with `sync_status = 'downloading'` → reset to `pending_download`.
   - Query all rows with `sync_status = 'deleting'` → check if local file exists at `row.Path`:
     - File exists → reset to `pending_delete`.
     - File absent → set to `deleted` (the delete completed but the DB update didn't persist).
   - Called at engine startup (`RunOnce` and `RunWatch`) BEFORE any observer runs, BEFORE reconciler starts.
   - The `syncRoot` path is passed so the method can check `os.Stat(filepath.Join(syncRoot, row.Path))` for the delete recovery case.

4. **Compile-time interface satisfaction guards** — Add to `baseline.go` (or a dedicated `store_guards.go`):
   ```go
   var _ ObservationWriter = (*SyncStore)(nil)
   var _ OutcomeWriter     = (*SyncStore)(nil)
   var _ FailureRecorder   = (*SyncStore)(nil)
   var _ ConflictEscalator = (*SyncStore)(nil)
   var _ StateReader       = (*SyncStore)(nil)
   var _ StateAdmin        = (*SyncStore)(nil)
   ```
   These will fail to compile if any interface method is missing from `*SyncStore`. Some of these may already be added in 5.7.1 — add any that are missing.

**What changes** (existing code modified):

1. `baseline.go` — `commitUpsert()`, `commitDelete()`, `commitMove()`, `commitConflict()` each gain remote_state SQL within their existing transactions. The baseline write and remote_state write are in the same BEGIN/COMMIT — atomic.
2. `engine.go` — `executePlan()` and `processBatch()` gain dispatch transition writes before `tracker.Add()`. `RunOnce()` and `RunWatch()` call `ResetInProgressStates()` at startup before observers start.
3. `worker.go` — `WorkerPool` receives `OutcomeWriter` interface instead of `*SyncStore`. The type signature changes from `baseline *SyncStore` to `outcomes OutcomeWriter`. `CommitOutcome` and `Load` are on `OutcomeWriter`, so no behavioral change — just narrower type.

**What gets removed**: Nothing. This increment is purely additive to the state machine.

**Behavioral changes**:
- After a successful download, the `remote_state` row transitions from `downloading` → `synced`. Previously it stayed in `downloading` forever (accumulated since 5.7.1).
- After a successful delete, the row transitions from `deleting` → `deleted`.
- After a successful upload, the row is created as or updated to `synced` with server-returned metadata. The delta echo no longer triggers spurious re-downloads.
- On daemon restart, `downloading`/`deleting` rows are reset to their pending states. Previously, crash recovery was implicit (delta replay from held-back token). Now it's explicit and unambiguous.
- Dispatch actions (`pending_* → downloading/deleting`) are visible in the DB — `remote_state` accurately reflects what's in-flight at all times.

**Tests**:

1. **CommitOutcome download success**: Create `remote_state` row at `downloading` with hash H. Call `CommitOutcome` with matching hash. Assert row is `synced`. Assert baseline row also upserted.
2. **CommitOutcome download stale success (scenario 4)**: Create row at `downloading` with hash H2. Call `CommitOutcome` with hash H1 (stale). Assert row is still `downloading` with hash H2 (0 rows affected). Assert baseline IS updated (baseline doesn't have the hash guard — it records what was actually written to disk).
3. **CommitOutcome delete success**: Create row at `deleting`. Call `CommitOutcome` for delete. Assert row is `deleted`. Assert baseline row deleted.
4. **CommitOutcome upload success — new item**: No `remote_state` row exists. Call `CommitOutcome` for upload with server-returned ItemID. Assert row created as `synced` with correct metadata.
5. **CommitOutcome upload success — existing item**: Create row at `download_failed`. Call `CommitOutcome` for upload. Assert row is `synced` (unconditional update).
6. **CommitOutcome move success**: Create row at `downloading` with old path. Call `CommitOutcome` for move with new path. Assert row is `synced` with new path.
7. **Dispatch transitions**: Call engine dispatch logic. Assert `remote_state` transitions from `pending_download` → `downloading`. Assert 0 rows affected if row is already `synced` (stale dispatch).
8. **Dispatch batch optimization**: Dispatch 1000 actions in RunOnce. Assert single transaction wraps all transitions. Assert performance < 1s.
9. **Crash recovery — downloading**: Insert rows at `downloading`. Call `ResetInProgressStates`. Assert all reset to `pending_download`.
10. **Crash recovery — deleting with file present**: Insert row at `deleting`. Create local file at that path. Call `ResetInProgressStates`. Assert reset to `pending_delete`.
11. **Crash recovery — deleting with file absent**: Insert row at `deleting`. No local file. Call `ResetInProgressStates`. Assert set to `deleted`.
12. **Full cycle integration test**: CommitObservation → dispatch → CommitOutcome → verify `remote_state` is `synced` and baseline is correct.

**State of CI after 5.7.2**:
- The full observe → dispatch → execute → outcome cycle works through `remote_state`. Every `remote_state` row reaches a terminal state (`synced`, `deleted`, `filtered`) or a failure state (`*_failed`).
- Crash recovery is explicit and unambiguous.
- One-shot sync (`RunOnce`) fully uses `remote_state`: observations persisted, orphans recovered on startup, outcomes update state.
- BUT: Watch mode has no automatic retry for `*_failed` rows. Failed items persist with correct backoff metadata but nothing reads them until the next `RunOnce` startup query or daemon restart. The reconciler (5.7.3) adds automatic retry.
- All existing tests pass. The delta echo after upload no longer triggers spurious re-downloads.

**Acceptance**:
- After a full sync cycle, `SELECT COUNT(*) FROM remote_state WHERE sync_status NOT IN ('synced', 'filtered', 'deleted')` → 0 (no orphaned pending/downloading rows).
- After a crash simulation (kill + restart), `SELECT COUNT(*) FROM remote_state WHERE sync_status IN ('downloading', 'deleting')` → 0 (all reset by crash recovery).
- `var _ OutcomeWriter = (*SyncStore)(nil)` compiles. All 6 interface guards compile.

##### 5.7.3: Reconciler Goroutine + Conflict Escalation

**Goal**: Add the dedicated reconciler goroutine that automatically retries failed remote actions with exponential backoff and escalates permanently-failing items to conflicts. After this increment, watch mode is self-healing — no manual intervention needed for transient failures.

**Design doc sections**: §16 (single-timer reconciler — architecture, mechanisms, backoff, re-read cases, synthesized events), §17 (error handling — non-empty directory deletes, global auth failure), §8 Flow 4 (reconciler retry), §8 Flow 7 (daemon startup — reconciler bootstrap), §9 scenarios 1-3, 8 (reconciler-involved scenarios).

**Preconditions**: 5.7.2 complete (CommitOutcome updates remote_state, dispatch transitions work, crash recovery resets in-progress states, `ListFailedForRetry` and `ListUnreconciled` queries exist).

**What gets built** (new files and methods):

1. **`internal/sync/reconciler.go`** — New file, ~200 lines. The reconciler is a dedicated long-lived goroutine (§16).

   **Reconciler struct**:
   ```go
   type Reconciler struct {
       state     StateReader
       escalator ConflictEscalator
       buf       EventAdder          // interface wrapping buf.Add()
       tracker   InFlightChecker     // interface wrapping tracker.HasInFlight()
       logger    *slog.Logger
       kickCh    chan struct{}        // 1-buffered, coalesces kicks
       cancel    context.CancelFunc
       wg        sync.WaitGroup
   }
   ```

   **Three trigger mechanisms** (§16):
   - `Kick()` — non-blocking write to 1-buffered channel. Called by `drainWorkerResults` after every worker completion (success or failure). Multiple rapid kicks coalesce into one `reconcile()` pass.
   - Single `*time.Timer` — armed to the earliest `next_retry_at` across all `*_failed` rows. Timer callback writes to `kickCh`. Re-armed after every `reconcile()` pass.
   - 2-minute safety sweep `*time.Ticker` — catches anything missed by kicks or timer drift. Level-triggered.

   **`reconcile(ctx)` method** — the core logic, called from all three triggers:
   1. `state.ListFailedForRetry(ctx, time.Now())` → rows where `sync_status IN ('download_failed', 'delete_failed') AND next_retry_at <= now`.
   2. For each row:
      - Skip if `tracker.HasInFlight(row.Path)` — already being processed.
      - If `row.FailureCount >= escalationThreshold` (10): call `escalator.EscalateToConflict(ctx, row.DriveID, row.ItemID, row.Path, reason)`. Skip dispatch. The conflict record explains the situation; the user resolves via `conflicts` CLI.
      - Else: synthesize a `ChangeEvent` from the row (§16 "Synthesized ChangeEvent fields") and feed into `buf.Add()`. The buffer debounces → planner → workers (normal pipeline).
   3. Query remaining unreconciled rows to find the earliest `next_retry_at` that's in the future. Arm the single timer to that time.
   4. If no pending retries, disarm timer (nil it out).

   **Re-read semantics** (§16): The reconciler always reads current DB state. Five cases on re-read:
   - Row `synced` → skip (item synced since timer was scheduled).
   - Hash changed (fresh delta overwrote) → synthesize event with new hash.
   - State changed to `pending_delete` → synthesize delete event.
   - In-flight → skip, keep timer for next pass.
   - Unchanged → synthesize event, normal retry.

   **Synthesized ChangeEvent fields** (§16):
   - `pending_delete` → `ChangeDelete`. Planner sees remote deletion.
   - All other pending/failed states → `ChangeModify`. Planner checks hash vs baseline.
   - Never `ChangeMove` — moves decomposed at observation time. Each side retried independently.

2. **`EscalateToConflict(ctx, driveID, itemID, path, reason)` on `*SyncStore`** — Implements `ConflictEscalator` interface. Writes a conflict record to the `conflicts` table with `conflict_type = 'sync_failure'` (new enum value, requires schema addition) and the reason string. Also updates the `remote_state` row's `sync_status` to `synced` to stop further retries (the conflict record takes ownership of the problem). Example reason: `"remote deleted folder, but local directory is not empty after 10 retry attempts"`.

3. **Backoff calculation function** — Pure function, testable independently:
   ```go
   func retryBackoff(failureCount int) time.Duration
   ```
   Formula (§16): failures 1-2: ~5 seconds. Failure 3+: `min(5min × 2^(failureCount - 3), 1 hour)`. Returns the delay to add to `time.Now()` for `next_retry_at`.

4. **`InFlightChecker` interface** — Narrow interface for the reconciler to check if a path is already being processed:
   ```go
   type InFlightChecker interface {
       HasInFlight(path string) bool
   }
   ```
   Satisfied by `*DepTracker`. Keeps the reconciler decoupled from tracker internals.

5. **`EventAdder` interface** — Narrow interface for the reconciler to inject events into the buffer:
   ```go
   type EventAdder interface {
       Add(events ...ChangeEvent)
   }
   ```
   Satisfied by `*Buffer`. Keeps the reconciler decoupled from buffer internals.

6. **Schema migration** — Add `'sync_failure'` to the `conflict_type` CHECK constraint on the `conflicts` table. This is a new conflict type for escalated failures (distinct from `edit_edit`, `edit_delete`, `create_create`).

**What changes** (existing code modified):

1. `engine.go` — `RunWatch()`:
   - Creates `Reconciler` with `StateReader` (from `*SyncStore`), `ConflictEscalator` (from `*SyncStore`), buffer, tracker, logger.
   - Calls `reconciler.Start(ctx)` after observers start but before the main select loop.
   - `drainWorkerResults` calls `reconciler.Kick()` after every worker result (success or failure).
   - Engine shutdown (`ctx.Done()`) triggers `reconciler.Stop()` (cancels context, waits for goroutine).
2. `engine.go` — `RunOnce()`: No reconciler goroutine (§18). RunOnce's startup orphan recovery (added in 5.7.1) already handles the same case — query unreconciled rows, merge into planning pass.
3. `migrations/00001_consolidated_schema.sql` — Add `'sync_failure'` to conflicts table `conflict_type` CHECK constraint.

**What gets removed**: Nothing. This increment is purely additive.

**Behavioral changes**:
- Watch mode is now self-healing. A download that fails due to a transient error (423 locked, 5xx, network) is automatically retried with exponential backoff: ~5s, ~5s, 5min, 10min, 20min, 40min, 1hr cap.
- After 10 consecutive failures, items are escalated to user-visible conflicts instead of retrying forever. The `conflicts` CLI shows the reason.
- The reconciler is level-triggered — it always reads current DB state, never caches. If a delta delivers a new version of a failed item while it's backed off, the `failure_count` resets to 0 (done by `CommitObservation` in 5.7.1) and the reconciler picks up the new version on its next pass.
- Worker completion triggers an immediate reconciler check (`Kick()`), so retries happen as fast as the backoff allows — not on a fixed schedule.

**Tests**:

1. **Reconciler unit — basic retry**: Insert row at `download_failed` with `next_retry_at` in the past. Create reconciler. Assert it synthesizes a `ChangeEvent` and calls `buf.Add()`.
2. **Reconciler unit — backoff respected**: Insert row at `download_failed` with `next_retry_at` in the future. Assert reconciler does NOT dispatch it. Assert timer armed to the correct time.
3. **Reconciler unit — in-flight skip**: Insert ready-to-retry row. Set `tracker.HasInFlight(path) = true`. Assert reconciler skips it.
4. **Reconciler unit — escalation threshold**: Insert row with `failure_count = 10`. Assert `EscalateToConflict` called. Assert no `ChangeEvent` dispatched.
5. **Reconciler unit — Kick coalescing**: Send 100 rapid `Kick()` calls. Assert `reconcile()` called a small number of times (not 100).
6. **Reconciler unit — re-read: row synced**: Insert row, arm timer. Before timer fires, update row to `synced`. Assert reconciler skips it.
7. **Reconciler unit — re-read: hash changed**: Insert row with hash H1, arm timer. Before timer fires, CommitObservation writes hash H2 (resets failure_count). Assert reconciler dispatches event with H2.
8. **Reconciler unit — safety sweep**: Disable Kick and timer. Assert the 2-minute sweep still triggers `reconcile()`.
9. **Reconciler unit — shutdown**: Start reconciler, cancel context. Assert goroutine exits cleanly (wg.Wait returns). Assert timer stopped.
10. **Backoff function**: Table-driven test: `{failureCount: 1, expected: 5s}`, `{3, 5m}`, `{4, 10m}`, `{5, 20m}`, `{6, 40m}`, `{7, 1h}`, `{100, 1h}`.
11. **EscalateToConflict**: Call method. Assert conflict record with `type = 'sync_failure'` and reason string. Assert `remote_state` row updated to stop retries.
12. **Integration — full failure-retry-success cycle**: CommitObservation → dispatch → worker fails → RecordFailure → Kick → reconcile → re-dispatch → worker succeeds → CommitOutcome → verify `synced`.
13. **Integration — crash recovery + reconciler bootstrap**: Insert `downloading` rows. Call `ResetInProgressStates`. Start reconciler. Assert it picks up the reset `pending_download` rows.

**State of CI after 5.7.3**:
- The **complete download/delete remote state lifecycle** is operational: observe → dispatch → execute → succeed/fail → retry/escalate → synced/conflict.
- Watch mode is self-healing with exponential backoff.
- One-shot mode recovers orphans on startup.
- Permanently-failing items surface as conflicts.
- The only remaining gap: upload failure tracking (`local_issues` table) is unused. Upload failures are still handled by the existing planner retry (local file differs from baseline → re-plan upload). This works but loses failure metadata on restart and doesn't surface permanently-failing uploads to the user.
- All existing tests pass. No behavioral regression.

**Acceptance**:
- After a simulated transient failure (mock 423 response), the item is automatically retried and eventually succeeds without manual intervention.
- After 10+ failures on one item, a conflict record appears in `SELECT * FROM conflicts WHERE conflict_type = 'sync_failure'`.
- `var _ ConflictEscalator = (*SyncStore)(nil)` compiles.
- `grep -rn 'reconcil' internal/sync/ --include='*.go' | grep -v '_test.go'` shows `reconciler.go` and engine wiring.

##### 5.7.4: Upload Failure Tracking + Status Integration + Interface Narrowing + Cleanup

**Goal**: Complete the remote state separation architecture: add persistent upload failure tracking via `local_issues`, surface pending/failed items to the user via `status` and `issues` commands, narrow all component interfaces to their sub-interface, remove the `DB()` escape hatch, and verify the full architecture against the design doc checklist.

**Design doc sections**: §14 (upload failure tracking — error classification, pre-upload validation, upload-side retries), §12 (sub-interface wiring — component → interface distribution table), §18 (status command), §24 (testing strategy layers 1-4), §25 (migration path — verification checklist, documents to update).

**Preconditions**: 5.7.3 complete (reconciler operational, EscalateToConflict implemented, full download/delete lifecycle works).

**What gets built** (new code):

1. **`local_issues` CRUD methods on `*SyncStore`**:
   - `RecordLocalIssue(ctx, path, issueType, errMsg string, httpStatus int, fileSize int64, localHash string) error` — UPSERT to `local_issues`. On conflict (same path): increment `failure_count`, update `last_seen_at`, `last_error`, `http_status`. Set `sync_status` based on issue type: `permanently_failed` for validation failures (invalid filename, path too long, file too large), `upload_failed` for transient failures.
   - `ListLocalIssues(ctx) ([]LocalIssueRow, error)` — read all `local_issues` rows. Returns structured rows for CLI display.
   - `ClearLocalIssue(ctx, path string) error` — DELETE from `local_issues`. Used when user fixes the issue or for manual reset.
   - `ClearResolvedLocalIssues(ctx, retention time.Duration) (int, error)` — DELETE `resolved` rows older than retention. Called from `Checkpoint()`.
   - `LocalIssueRow` struct with all `local_issues` columns.

2. **Pre-upload validation in planner** (§14):
   - New function `validateUpload(path string, size int64, driveType driveid.DriveType) *validationError` called in the planner when classifying upload actions.
   - Checks:
     - Invalid filenames: reserved names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`), trailing dots/spaces, invalid characters.
     - Path length: `<= 400` bytes for Business, `<= 400` runes for Personal.
     - File size: `<= 250 GB`.
   - Items failing validation are excluded from the action plan. Instead, the planner calls `store.RecordLocalIssue(path, issueType, reason, 0, size, hash)` with `permanently_failed` status.
   - The planner needs a `LocalIssueRecorder` interface (new sub-interface) to avoid importing `*SyncStore` directly:
     ```go
     type LocalIssueRecorder interface {
         RecordLocalIssue(ctx context.Context, path, issueType, errMsg string,
             httpStatus int, fileSize int64, localHash string) error
     }
     ```

3. **Upload failure recording in workers** — When a worker's upload action fails (after the executor's own retries are exhausted), the worker calls `store.RecordLocalIssue()` with the failure details. The `Outcome.Error` message and HTTP status are passed through.

4. **CommitOutcome upload success clears `local_issues`** — In the upload success handler (already extended in 5.7.2 for `remote_state`), add:
   ```sql
   DELETE FROM local_issues WHERE path = ?
   ```
   This clears any prior upload failure record when the upload eventually succeeds.

5. **`issues` CLI command** — New command `onedrive-go issues` in the root package:
   - `issues list` (default): table of all `local_issues` rows — path, issue_type, sync_status, failure_count, last_error, first/last seen timestamps.
   - `issues clear <path>`: remove a specific issue.
   - `issues clear --all`: remove all resolved issues.
   - `--json` flag for machine-readable output.
   - Opens the state DB read-only (same pattern as `status.go:querySyncState`), queries `local_issues`.

6. **`status` command remote_state integration** — Extend `querySyncState()` in `status.go` to query:
   ```sql
   SELECT COUNT(*) FROM remote_state
   WHERE sync_status NOT IN ('synced', 'filtered', 'deleted')
   ```
   Display as "N items pending sync" in status output. Also query:
   ```sql
   SELECT COUNT(*) FROM local_issues
   WHERE sync_status != 'resolved'
   ```
   Display as "N upload issues" in status output. Both queries use read-only DB access (existing `mode=ro` pattern in `status.go`).

**What changes** (existing code modified):

1. **Sub-interface narrowing across all consumers** (§12 component → interface distribution table):
   - `RemoteObserver` — field type changes from `*SyncStore` (or `*Baseline`) to `ObservationWriter`. Already partially done in 5.7.1 if the observer received `ObservationWriter` there. Verify and complete.
   - `WorkerPool` — field type changes from `*SyncStore` to `OutcomeWriter`. Already changed in 5.7.2. Verify.
   - `drainWorkerResults` — receives `FailureRecorder` interface, not `*SyncStore`. The engine casts `*SyncStore` to `FailureRecorder` before passing.
   - `Reconciler` — already receives `StateReader` + `ConflictEscalator` (built in 5.7.3). Verify.
   - `Planner` — receives `LocalIssueRecorder` for pre-upload validation (new in this increment).
   - `Engine` — holds `*SyncStore` (the concrete type) and distributes sub-interfaces to all consumers at construction time.

2. **`DB()` method on `*SyncStore`** — Remove it. The only production caller is test code (`baseline_test.go:1967`). Change that test to use a SyncStore method instead of raw SQL, or keep `DB()` as a test-only helper (unexported `db()` or accessed via a test helper function).

3. **`Checkpoint()` in `baseline.go`** — Add pruning of `local_issues` resolved rows (already exists in schema, verify the `Checkpoint()` method handles it).

4. **`status.go`** — Add `PendingSyncCount` and `UploadIssueCount` fields to `syncStateInfo`. Add SQL queries in `querySyncState()`. Update display formatting.

**What gets removed**:
- `DB()` public method on `*SyncStore` (or downgraded to unexported for test use only).

**Behavioral changes**:
- Uploads that fail validation (invalid filename, path too long, file too large) are immediately recorded as `permanently_failed` in `local_issues` and excluded from the action plan. Previously, these would be attempted and fail repeatedly.
- Upload failures from transient errors (423, 5xx, network) are persisted to `local_issues`. Previously, upload failures were only tracked in-memory via the planner's "local file differs from baseline" re-detection, losing failure metadata on restart.
- `onedrive-go status` shows pending sync count and upload issue count.
- `onedrive-go issues` surfaces all upload problems with actionable information (path, error, HTTP status, timestamps).
- Successful uploads clear their `local_issues` records.

**Tests**:

1. **RecordLocalIssue — new entry**: Record issue. Assert row created with correct fields. Assert `failure_count = 1`.
2. **RecordLocalIssue — repeat failure**: Record same path twice. Assert `failure_count = 2`, `last_seen_at` updated.
3. **RecordLocalIssue — permanently_failed**: Record invalid filename. Assert `sync_status = 'permanently_failed'`.
4. **ClearLocalIssue**: Record then clear. Assert row deleted.
5. **ListLocalIssues**: Record 5 issues, list. Assert 5 rows with correct fields.
6. **Pre-upload validation — invalid filename**: Plan an upload of `CON.txt`. Assert excluded from plan. Assert `local_issues` row.
7. **Pre-upload validation — path too long**: Plan upload with 500-char path. Assert excluded. Assert `local_issues` row.
8. **Pre-upload validation — file too large**: Plan upload of 300GB file. Assert excluded. Assert `local_issues` row.
9. **Pre-upload validation — valid file passes**: Plan upload of normal file. Assert included in plan. No `local_issues` row.
10. **CommitOutcome upload success clears issue**: Record upload failure → retry → succeed → assert `local_issues` row deleted.
11. **Status command integration**: Create state DB with pending remote_state rows and local_issues rows. Call `querySyncState`. Assert counts correct.
12. **Issues CLI**: Create state DB with issues. Run `issues list`. Assert table output. Run `issues clear <path>`. Assert cleared.
13. **Interface narrowing**: Verify each consumer only has access to its sub-interface methods. Test that `RemoteObserver` cannot call `CommitOutcome` (compile-time check via type assertion in test).
14. **DB() removal**: Assert `DB()` is not exported (or doesn't exist). `grep -rn '\.DB()' internal/sync/ --include='*.go' | grep -v '_test.go'` → 0 hits.

**State of CI after 5.7.4**:
- **The full remote state separation architecture is complete.** Every feature described in [remote-state-separation.md](design/remote-state-separation.md) is implemented:
  - `remote_state` tracks every remote item through its full lifecycle.
  - `local_issues` tracks upload failures persistently.
  - Delta token advances atomically with observations.
  - Crash recovery is explicit and unambiguous.
  - The reconciler retries failed downloads/deletes with exponential backoff.
  - Permanently-failing items escalate to conflicts.
  - Pre-upload validation catches invalid files before they're attempted.
  - `status` and `issues` commands surface all problems to the user.
  - Sub-interfaces enforce capability restriction at compile time.
- All unit, integration, and E2E tests pass.
- The in-memory failure tracker, cycle concept, and cycle-gated token commit are fully removed.

**Acceptance — design doc §25 verification checklist**:
```bash
# None of these should appear (except in comments, design docs, or test fixtures):
grep -rn 'failureTracker\|FailureTracker\|failure_tracker' internal/sync/ --include='*.go' | grep -v '_test.go' | grep -v 'design'
grep -rn 'CycleID\|cycleID\|cycle_id' internal/sync/ --include='*.go' | grep -v '_test.go'
grep -rn 'CycleDone\|CleanupCycle' internal/sync/ --include='*.go' | grep -v '_test.go'
grep -rn 'cycleFailures\|cycleLookup' internal/sync/ --include='*.go' | grep -v '_test.go'
grep -rn 'shouldSkip' internal/sync/ --include='*.go' | grep -v '_test.go'
grep -rn 'watchCycleCompletion' internal/sync/ --include='*.go' | grep -v '_test.go'
grep -rn 'BaselineManager' internal/sync/ --include='*.go' | grep -v '_test.go'
# DB() should not be exported:
grep -rn 'func.*SyncStore.*DB()' internal/sync/ --include='*.go'
```

**Documents to update** (§25):
- `docs/design/data-model.md` — add `remote_state` and `local_issues` table documentation. Update axiom: "baseline + remote_state + local_issues are the three durable per-item state stores."
- `docs/design/concurrent-execution.md` — update "baseline is only durable per-item state." Document sub-interface distribution table.
- `docs/design/event-driven-rationale.md` — update Alternative B notes: adopted for remote side with sub-interface solution.
- `LEARNINGS.md` — add: "delta token is API cursor, not sync cursor" and "remote_state is the authoritative record of what the server has; baseline is the authoritative record of what's synced locally."
- `CLAUDE.md` — add `issues` to CLI command list. Update architecture diagram if `reconciler` becomes a visible component. Update package description for `internal/sync/` to mention reconciler.
- `docs/roadmap.md` — mark 5.7.4 as DONE. Update "Current Phase" to reflect completion.

##### 5.7.1: Remote State Observation Layer + Filtering Symmetry — **DONE**

**5.7.1a** (filtering symmetry — B-307, B-308):
1. Remote observer filtering symmetry: added `isAlwaysExcluded()` + `isValidOneDriveName()` checks in `classifyItem()` — remote items now filtered symmetrically with local observer (B-307)
2. Narrowed `.db` exclusion: removed `.db`/`.db-wal`/`.db-shm` from `alwaysExcludedSuffixes` — legitimate data files no longer silently excluded (B-308)

**5.7.1b** (SyncStore observation + failure layer):
1. `CommitObservation()`: atomically persists `[]ObservedItem` + delta token in a single transaction
2. `RecordFailure()`: durable failure tracking in `remote_state` with exponential backoff (`next_retry` scheduling)
3. `CommitOutcome` extension: updates `remote_state` on action completion (download/upload/delete)
4. `ChangeEvent`-to-`ObservedItem` converter for bridging observer output to SyncStore input

**5.7.1c** (query layer + engine wiring):
1. `StateReader`: `ListUnreconciled()`, `ListFailedForRetry()`
2. `StateAdmin`: `ResetFailure()`, `ResetAllFailures()`, `ResetInProgressStates()`
3. Engine wiring — `RunOnce`: crash recovery via `ResetInProgressStates()` + `observeAndCommitRemote()` pipeline
4. Engine wiring — `RunWatch`: `obsWriter` + `RecordFailure` integration
5. Removed legacy cycle tracking (`failureTracker`, `cycleFailures`, `watchCycleCompletion`)

New file: `commit_observation_test.go`. Deleted files: `failure_tracker.go`, `failure_tracker_test.go`. Modified: `scanner.go`, `observer_remote.go`, `baseline.go`, `engine.go`, `worker.go` + their tests.

---

## Phase 6: Multi-Drive Orchestration + Shared Content Sync — PARTIALLY COMPLETE

**Single-process multi-drive sync.** After this phase, `sync --watch` syncs all non-paused drives simultaneously. Each drive has its own goroutine, state DB, and sync cycle.

> **Architecture**: Architecture A (per-drive goroutine with isolated engines). See [MULTIDRIVE.md §11](design/MULTIDRIVE.md#11-multi-drive-orchestrator). The Orchestrator is ALWAYS used, even for a single drive.

### Dependency Graph

```
6.0a ──→ 6.0b ──→ 6.0c ──→ 6.0d
  │                          │
  ├── 6.2b                   │
  │                          │
  └── 6.4a ─────────────→ 6.4b

6.1 (DONE)    6.2a (DONE)    6.3 (after 6.0a)
```

### Completed increments

| Increment | What | Key deliverables |
|-----------|------|------------------|
| 6.0a | DriveSession + ResolveDrives + shared drive foundations | `DriveSession` type, `config.ResolveDrives()`, `BaseSyncDir` for shared, sync root overlap validation, `CLIFlags` struct (B-224) |
| 6.0b | Orchestrator + DriveRunner (always-on) | `Orchestrator.RunOnce()`, `DriveRunner` with panic recovery + backoff, shared `graph.Client` per token path, sync command rewrite |
| 6.0c | Worker budget + daemon mode + config reload | `transfer_workers`/`check_workers` config, lanes removed (flat pool), `Orchestrator.RunWatch()`, PID file, SIGHUP reload, `--drive` repeatable |
| 6.0d | inotify + E2E + second test account | inotify watch limit detection (Linux), second test account bootstrapped, 5 multi-drive E2E scenarios |
| 6.0e | `internal/driveops/` package | `SessionProvider` (token caching), `Session` (Meta + Transfer clients), `TransferManager`/`SessionStore` moved from sync to driveops |
| 6.0f | Zero-config removal + scanner extraction + daemon E2E | Config mandatory for all ops, `scanner.go` extracted from `observer_local.go`, daemon E2E tests |
| 6.0g | Explicit E2E config + SIGHUP E2E + root coverage | All E2E helpers migrated to explicit config, SIGHUP reload E2E, root package 39.3% → 46.7% |
| 6.0h | CI reliability hardening | Token file integrity enforcement (B-303), WAL checkpoint hardening (B-304), eventual-consistency guard (B-305) |
| 6.1 | Drive removal | `drive remove` (config section), `drive remove --purge` (+ state DB) |
| 6.2a | Status command (basic) | Account/drive hierarchy, token state, `--json` |
| 6.2b | Status command (sync state) | Per-drive sync metadata, baseline/conflict counts, health summary |
| E2E hardening | 42 new `e2e_full` tests (B-306) | 86 total E2E tests across 7 files |

### Future increments

#### 6.3: Shared drive enumeration — FUTURE

1. `graph.SharedWithMe(ctx)` — shared drive items with owner, permissions.
2. `drive list` shows shared folders alongside personal/business/SharePoint.
3. `drive add` for shared folders — substring match, construct `shared:email:sourceDriveID:sourceItemID` canonical ID.

#### 6.4a: Folder-scoped delta + remoteItem parsing — FUTURE

1. `graph.Item` gains `RemoteDriveID`/`RemoteItemID` from `remoteItem` facet.
2. `graph.DeltaFolder()` for `/drives/{driveID}/items/{folderID}/delta`.
3. RemoteObserver shortcut detection + sub-delta orchestration.
4. Path mapping: shortcut position prefix + sub-delta item path.
5. Scope token management via composite key schema (migration 00004).

#### 6.4b: Shared folder sync (shortcuts + lifecycle) — FUTURE

1. Shortcut lifecycle: new → enumerate + local dir, removed → delete + clean tokens, moved → rename.
2. Cross-drive executor operations (same OAuth token grants shared access).
3. Read-only content: auto-detect via 403, mark shortcut read-only.
4. Shared-with-me drives: full drive infrastructure for standalone shared folders.

---

## Phase 7: CLI Completeness

**Make every command work properly.** After this phase, every CLI command in the PRD works correctly.

| Increment | Status | Description |
|-----------|--------|-------------|
| 7.0 | Mostly DONE | Global output flags (`--verbose`, `--debug`, `--quiet`, `--json`). Output layer refactor FUTURE. |
| 7.1 | Partially DONE | `ls` pagination DONE, recursive `rm` DONE. Recursive `get`/`put` FUTURE. |
| 7.2 | FUTURE | Server-side `mv` and `cp` commands |
| 7.3 | DONE | `login --browser` (PKCE + localhost callback), `logout --purge` |
| 7.4 | Mostly DONE | `--drive` fuzzy matching DONE, `--account` flag DONE. `--drive` repeatable FUTURE. |
| 7.5 | FUTURE | Transfer progress bars (mpb library) |
| 7.6 | FUTURE | Structured JSON logging (`log_format` config) |
| 7.7 | FUTURE | Recycle bin commands (`recycle-bin list/empty/restore`) |
| 7.8 | FUTURE | Conflict path filtering (`conflicts --path`, `resolve --path`) |

---

## Phase 8: WebSocket + Advanced Sync

| Increment | Description |
|-----------|-------------|
| 8.0 | WebSocket remote observer — push notifications trigger immediate delta poll |
| 8.1 | Adaptive concurrency — multi-drive worker budget (B-297), watch-mode parallel hashing (B-298), AIMD auto-tuning |
| 8.2 | Observer backpressure — high/low water marks on ChangeBuffer |
| 8.3 | Initial sync batching — process 50K-item batches to bound memory |
| 8.4 | Action cancellation — cancel stale in-flight uploads when file changes |

---

## Phase 9: Operational Hardening

| Increment | Status | Description |
|-----------|--------|-------------|
| 9.0 | FUTURE | Bandwidth limiting (`bandwidth_limit`, `bandwidth_schedule`) |
| 9.1 | FUTURE | Disk space reservation (`min_free_space`) |
| 9.2 | FUTURE | Trash integration (`use_recycle_bin`, `use_local_trash`) |
| 9.3 | FUTURE | Conflict reminder notifications |
| 9.4 | DONE | Configurable parallelism (moved to 6.0c) |
| 9.5 | FUTURE | Configurable timeouts (`connect_timeout`, `data_timeout`, `shutdown_timeout`) |
| 9.6 | Partial | Unknown config key detection DONE. Log retention FUTURE. |
| 9.7 | FUTURE | Configurable file permissions (`sync_file_permissions`, `sync_dir_permissions`) |

---

## Phase 10: Filtering

> **Prerequisites**: B-307 (FC-1) and B-308 (FC-2) are addressed in Phase 5.7.1.

| Increment | Description |
|-----------|-------------|
| 10.0 | Config-based filtering — `skip_files`, `skip_dirs`, `skip_dotfiles`, `max_file_size`. Stale file handling (FC-9: warn + freeze). |
| 10.1 | Per-directory `.odignore` marker files (gitignore-style patterns, never synced) |
| 10.2 | Selective sync paths (`sync_paths` per-drive config) with lightweight parent traversal (FC-10) |
| 10.3 | Symlink handling — follow by default with cycle detection, `skip_symlinks` option |
| 10.4 | Application-specific exclusions — OneNote auto-exclude, SharePoint enrichment known-type list |
| 10.5 | OS junk exclusion (`.DS_Store`, `Thumbs.db`, `._*`) + `perishable_files` config for directory deletion cleanup |

---

## Phase 11: Packaging + Release

| Increment | Description |
|-----------|-------------|
| 11.0 | goreleaser — Linux/macOS binaries, GitHub Releases |
| 11.1 | Homebrew tap + AUR PKGBUILD |
| 11.2 | `.deb` + `.rpm` packages (nfpm, systemd unit) |
| 11.3 | Docker image (Alpine, multi-arch) |
| 11.4 | `service install/uninstall/status` (systemd + launchd) |
| 11.5 | Man page + README |

---

## Phase 12: Post-Release

| Increment | Status | Description |
|-----------|--------|-------------|
| 12.0 | FUTURE | Setup wizard — interactive menu-driven configuration |
| 12.1 | FUTURE | Migration tool — import from abraunegg/onedrive or rclone |
| 12.2 | FUTURE | Interactive conflict resolution (`[L]ocal / [R]emote / [B]oth / [S]kip`) |
| 12.3 | Partial | Interactive `drive add` FUTURE. Non-interactive DONE. |
| 12.4 | DONE | SharePoint site search (`drive search`) |
| 12.5 | FUTURE | Share command — generate shareable links |
| 12.6 | FUTURE | Daemon observability — metrics registry, Unix socket, Prometheus exposition. See [observability.md](design/observability.md). |
| 12.7 | FUTURE | RPC-based live sync trigger — delegate to running daemon via socket |
| 12.8 | FUTURE | TUI interface (bubbletea) — real-time status, progress, conflict resolution |
