# onedrive-go — Implementation Roadmap

> **ZERO PATH DEPENDENCY**: This document describes the Option E event-driven
> architecture, designed from first principles. The system is NOT an evolution
> of the prior batch-pipeline sync engine. Existing code is reused only where
> it is an excellent match for the new design. See
> [design/event-driven-rationale.md](design/event-driven-rationale.md) for the full rationale.

## Principles

- Each increment is completable in one focused session
- Each increment has clear acceptance criteria (build + test + lint pass)
- Each increment is a focused, well-scoped unit of work
- Design docs in `docs/design/` are the spec — use plan mode before each increment for file-level planning
- CLI-first: build a working tool before building the sync engine

---

## Phase 1: Graph Client + Auth + CLI Basics — COMPLETE

**Build a working tool first.** After this phase, users can `login`, `ls`, `get`, `put`, `rm`, `mkdir`.

| Increment | Description | Status |
|-----------|-------------|--------|
| 1.1 | graph/ client: HTTP transport, retry, rate limiting, error mapping | **DONE** |
| 1.2 | graph/ auth: device code flow, token persistence, refresh | **DONE** |
| 1.3 | graph/ items: GetItem, ListChildren, CreateFolder, MoveItem, DeleteItem | **DONE** |
| 1.4 | graph/ delta: Delta with full normalization pipeline (all quirks) | **DONE** |
| 1.5 | graph/ transfers: Download, SimpleUpload, chunked uploads | **DONE** |
| 1.6 | graph/ drives: Me, Drives, Drive | **DONE** |
| 1.7 | cmd/ auth: login (device code), logout, whoami | **DONE** |
| 1.8 | cmd/ file ops: ls, get, put, rm, mkdir, stat | **DONE** |

Eight increments. Graph API client with HTTP transport, retry, rate limiting, auth (device code flow), items CRUD, delta with normalization pipeline, transfers (download + chunked upload), drives. CLI: login/logout/whoami, ls/get/put/rm/mkdir/stat. All tested against live OneDrive via httptest mocks and integration tests. Package coverage: ~91%.

---

## Phase 2: E2E CI — COMPLETE

**Prove the tool works against real OneDrive.** Azure Key Vault + OIDC for token management.

| Increment | Description | Status |
|-----------|-------------|--------|
| 2.1 | CI scaffold: GitHub Actions, Azure Key Vault + OIDC, integration tests | **DONE** |
| 2.2 | E2E tests: login, ls, get, put, rm round-trip against live API | **DONE** |
| 2.3 | E2E edge cases: large files, special characters, concurrent ops | **DONE** |

Three increments. Azure OIDC federation for CI, Key Vault token storage, integration tests against real Graph API. E2E test suite builds binary and exercises full round-trip (whoami, ls, mkdir, put, get, stat, rm). Edge cases: 5 MiB chunked upload, Unicode filenames, spaces, concurrent uploads.

---

## Phase 3: Config Integration — COMPLETE

| Increment | Description | Status |
|-----------|-------------|--------|
| 3.1 | config/ TOML loading + validation | **DONE** |
| 3.2 | config/ drives + path derivation | **DONE** |
| 3.3 | cmd/ config: config show + CLI integration | **DONE** |

Three increments. TOML config with all global options, unknown key detection, XDG-compliant paths, environment variable overrides. Drive sections with canonical IDs, per-drive overrides, token/state path derivation. CLI integration via `PersistentPreRunE` with four-layer override chain (defaults -> file -> env -> CLI flags). Config package coverage: 95.6%.

---

## Phase 3.5: Account/Drive System Alignment — COMPLETE

| Increment | Description | Status |
|-----------|-------------|--------|
| 3.5.1 | Documentation alignment: profiles -> drives terminology | **DONE** |
| 3.5.2 | Config + CLI + graph/auth migration to flat drive-section format | **DONE** |

Two increments. Replaced profile-based terminology with account/drive design from [accounts.md](design/accounts.md). Flat TOML with `["personal:user@example.com"]` drive sections, `ResolveDrive()`, drive matching (exact/alias/partial). Graph/auth decoupled from config (accepts tokenPath directly). Net diff: -414 lines across 32 files.

---

## Phase 4 v1: Batch-Pipeline Sync Engine — SUPERSEDED

Phase 4 v1 (increments 4.1-4.11) built a batch-pipeline sync engine with SQLite state store, delta processor, local scanner, filter engine, reconciler (14+7 decision matrix), safety checks (S1-S7), executor (9-phase dispatch), conflict handler (edit-edit/edit-delete/create-create), transfer pipeline (worker pools + bandwidth limiting), and engine wiring (RunOnce orchestration). This architecture was superseded by Option E after comprehensive E2E analysis revealed six structural fault lines: tombstone split (scanner and delta fighting over shared mutable DB rows), `local:` ID lifecycle (fake IDs for an item_id-keyed table), SQLITE_BUSY (concurrent DB writes during execution), incomplete folder lifecycle (`isSynced()` depending on hash fields folders lack), pipeline phase ordering (intermediate DB writes creating dependencies), and asymmetric filter application (filters only in the scanner, not on remote items). All six trace to one root cause: the database as the coordination mechanism between pipeline stages. See [design/event-driven-rationale.md](design/event-driven-rationale.md) for the full analysis. The old code in `internal/sync/` is deleted in Increment 0 before new engine implementation begins.

---

## Phase 4 v2: Event-Driven Sync Engine — COMPLETE

**The core architectural pivot.** Events replace the database as the coordination mechanism. Observers produce typed change events, the planner operates as a pure function on events + baseline, the executor produces outcomes, and the baseline manager commits everything atomically. Same decision matrix logic (EF1-EF14, ED1-ED8), same safety invariants (S1-S7), completely different data flow.

Estimated reuse: `internal/graph/` 100%, `internal/config/` 100%, `pkg/quickxorhash/` 100%. Reuse estimates for old sync code are historical — the old code is deleted in Increment 0 and the new engine is written from scratch. Design patterns (decision matrix, safety invariants) carry forward; code does not.

### 4v2.0: Clean Slate — DONE

**Delete old sync code. Start fresh.**

- Deleted all old `internal/sync/` files (~16,655 lines of batch-pipeline code, tests, and migrations)
- Rewrote `sync.go` CLI command to return "sync engine not yet implemented (Phase 4v2)" error stub
- Removed `tombstone_retention_days` from config package (option eliminated by Option E — tombstones are not needed)
- Removed sync integration test from `integration.yml` (re-enabled in 4v2.8 after new engine is wired)
- Pruned unused dependencies via `go mod tidy`, trimmed `.golangci.yml` depguard
- Created `clean-slate` orphan branch (fresh git history, `main` preserved as read-only safety net)
- Updated all CI workflows, scripts, and docs for `clean-slate` as the active branch
- Added Azure OIDC federated credential for `clean-slate` branch
- **Acceptance**: Build passes, all non-sync tests pass, `sync` command returns clear "not yet implemented" message

### 4v2.1: Types + Baseline Schema + BaselineManager — DONE

**Foundation types and persistence layer.**

- All type definitions: enums (ChangeSource, ChangeType, ItemType, SyncMode, ActionType, FolderCreateSide), core structs (ChangeEvent, BaselineEntry, Baseline, PathView, RemoteState, LocalState, ConflictRecord, Action, ActionPlan, Outcome), consumer-defined interfaces (DeltaFetcher, ItemClient, TransferClient)
- SQLite baseline schema via goose migrations: 4 tables (baseline, delta_tokens, conflicts, upload_sessions) + 4 indexes
- BaselineManager: sole DB writer with WAL mode via DSN pragmas, atomic Commit (outcomes + delta token), Load (ByPath + ByID maps), GetDeltaToken, injectable nowFunc for deterministic tests
- 25 tests, 82.5% coverage. Dependencies: modernc.org/sqlite, goose/v3, google/uuid
- **Acceptance**: All tests pass, baseline round-trip with real SQLite. PR #78.

### 4v2.2: Remote Observer — DONE

**Produce typed `ChangeEvent` values from Graph API delta responses.**

- `RemoteObserver` struct with `FullDelta(ctx, savedToken) -> ([]ChangeEvent, newDeltaToken, error)`
- Internal pagination loop with max-pages guard (10000)
- Path materialization via in-flight parent map + baseline lookup with depth guard (256)
- Change classification: create/modify/delete/move using baseline comparison
- NFC normalization (`golang.org/x/text/unicode/norm`), driveID zero-padding (16-char), hash selection (QuickXorHash preferred, SHA256 fallback)
- Move detection: materialized path vs baseline path comparison
- Business API: deleted items with missing Name recovered from baseline
- Root items registered in inflight (for children's path materialization) but skipped as events
- `ErrDeltaExpired` sentinel for HTTP 410 (delta token expired)
- 23 test cases with mock DeltaFetcher, 86.4% coverage. PR #80.
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.1, 10 (Phase 2)
- **DOD**: All gates passed. 86.4% coverage (up from 82.5%).

### 4v2.3: Local Observer — DONE

**Produce typed `ChangeEvent` values from local filesystem state.**

- `LocalObserver` struct with `FullScan(ctx, syncRoot) -> ([]ChangeEvent, error)`
- `filepath.WalkDir` with baseline comparison to classify: create (no baseline entry), modify (hash differs), delete (baseline entry exists, not on disk), unchanged (skip)
- NFC normalization via `nfcNormalize()` (shared with RemoteObserver) applied to paths and names
- `.nosync` guard: `ErrNosyncGuard` sentinel error when `.nosync` file present (S2 protection)
- Symlink handling: skip symlinks silently (OneDrive does not support symlinks)
- OneDrive name validation: reserved names (CON/PRN/AUX/NUL/COM0-9/LPT0-9), `.lock`, `desktop.ini`, `~$` prefix, `_vti_` substring, invalid chars (`"*:<>?/\|`), trailing dot/space, leading space, >255 chars
- Always-excluded patterns: `.partial`, `.tmp`, `.swp`, `.crdownload`, `.db`/`.db-wal`/`.db-shm` (SQLite corruption safety)
- QuickXorHash content hashing via streaming `io.Copy` (constant memory), base64-encoded
- mtime+size fast path: skip hashing when both match baseline (industry standard: rsync, rclone, Syncthing, Git). Racily-clean guard forces hash when mtime is within 1 second of scan start. PR #83.
- Folder mtime changes ignored (noise — contained files generate their own events)
- No DB access — compares against in-memory `*Baseline` snapshot
- 34 tests with real temp dirs (`t.TempDir()`), 88.0% sync coverage (up from 86.4%). PR #82, #83.
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.2, 10 (Phase 2)
- **DOD**: All gates passed. 88.0% coverage (up from 86.4%).

### 4v2.4: Change Buffer — DONE

**Debounce, dedup, and batch events for the planner.**

- `Buffer` struct with thread-safe `Add(*ChangeEvent)`, `AddAll([]ChangeEvent)`, `FlushImmediate() []PathChanges`, `Len() int`
- Move event dual-keying: a move produces events at both old path (synthetic delete) and new path, ensuring the planner sees both sides
- `FlushImmediate()` returns `[]PathChanges` sorted by path (deterministic), clears buffer. Timer-based debounce deferred to Phase 5.
- `Add` takes `*ChangeEvent` (not value) due to gocritic hugeParam (~192 bytes)
- `AddAll` takes single lock for entire batch (performance for one-shot mode with thousands of events)
- 14 test cases including thread safety with race detector (20 goroutines × 50 events). PR #84.
- **Acceptance**: Build passes, all tests pass (race detector), lint clean, 91.2% sync coverage
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.3, 10 (Phase 2)

### 4v2.5: Planner — DONE

**Pure-function reconciliation: events + baseline -> action plan.**

- `Planner.Plan(changes []PathChanges, baseline *Baseline, mode SyncMode, config *SafetyConfig) (*ActionPlan, error)`
- 5-step pipeline: `buildPathViews` → `detectMoves` (remote + local hash correlation) → `classifyPathView` (EF1-EF14 file, ED1-ED8 folder) → `orderPlan` (folder creates top-down, deletes bottom-up) → `bigDeleteTriggered` (S5 safety)
- `SafetyConfig` + `DefaultSafetyConfig()` + `ErrBigDeleteTriggered` defined in planner.go (avoids types.go contention during parallel development)
- File classification split into sub-functions (`classifyFileWithBaseline`/`classifyFileNoBaseline`) to stay under funlen/gocyclo limits
- Move detection: remote moves from `ChangeMove` events, local moves via hash correlation with unique-match constraint (ambiguous cases fall through to delete+create)
- SyncMode filtering: download-only suppresses uploads, upload-only suppresses downloads
- When no local events but baseline exists, derives `LocalState` from baseline (unchanged file)
- 43 test cases: 14 file matrix, 8 folder matrix, 4 move detection, 4 big-delete safety, 4 mode filtering, 2 ordering, 3 integration, plus helper tests. PR #85.
- **Acceptance**: Build passes, all tests pass (race detector), lint clean, 91.2% sync coverage, 100% decision matrix coverage
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 6, 7, 10 (Phase 3), [sync-algorithm.md](design/sync-algorithm.md) section 5

### 4v2.6: Executor — DONE

**Execute actions, produce outcomes — no DB writes.**

- `Executor` struct with `Execute(ctx, plan, baseline) -> ([]Outcome, error)` — nine-phase execution
- Nine phases in order: folder creates → moves → downloads → uploads → local deletes → remote deletes → conflicts → synced updates → cleanups
- Parallel worker pool (`errgroup`, 8 workers) for downloads and uploads
- Download: `.partial` + QuickXorHash verify + atomic rename + mtime restore (S3)
- Upload: SimpleUpload (<4 MiB) or chunked session (10 MiB chunks) with hash verification
- Local delete: S4 hash-before-delete guard using `action.View.Baseline.LocalHash`, conflict copy on mismatch
- Remote delete: 404 treated as success, retry with backoff on transient errors
- Conflict resolution: keep_both with timestamped conflict copies, restore on download failure
- Error classification: fatal (401, 507, context.Canceled) / retryable (429, 5xx, 408, 412, 509) / skip (everything else)
- Executor-level retry: exponential backoff (1s base, 2x, max 3 retries, 25% jitter)
- B-068: fills zero DriveID from per-drive context for new local items
- `resolveParentID`: createdFolders → baseline → "root" chain
- 35+ tests with mock graph client, real filesystem, all action types. PR #90.
- **Acceptance**: All DOD gates passed. 77.2% total coverage (up from 76.3%), sync package at 88.8%.
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.5, 10 (Phase 4)

### 4v2.7: Engine Wiring + RunOnce — DONE

**Wire all components into the full sync pipeline.**

- `Engine.RunOnce(ctx, mode, opts) -> SyncReport`:
  1. `BaselineManager.Load()` — read baseline into memory
  2. `RemoteObserver.FullDelta()` — fetch remote changes (skip in upload-only mode)
  3. `LocalObserver.FullScan()` — scan local changes (skip in download-only mode)
  4. `ChangeBuffer.FlushImmediate()` — collect all events
  5. `Planner.Plan()` — build action plan (pure function)
  6. Safety check gate — abort if big-delete triggered without `--force`
  7. Dry-run gate — return plan preview without executing
  8. `Executor.Execute()` — produce outcomes
  9. `BaselineManager.Commit(outcomes, newDeltaToken)` — atomic persistence
- Mode dispatch: bidirectional, download-only (skip local scan), upload-only (skip delta)
- Dry-run: stops at step 7, returns action counts without side effects (genuinely zero side effects)
- Context-based cancellation at every stage
- Integration tests with real SQLite: full round-trip from delta events through baseline commit
- CLI `sync` command wired to real Engine (replaced Phase 4v2 stub)
- **Multi-drive orchestration** (B-060, B-061, B-062) deferred to Phase 5
- **Acceptance**: All DOD gates passed. 76.6% total coverage, sync package at 90.7%.
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 2, 10 (Phase 5), [accounts.md](design/accounts.md) §13

### 4v2.8: CLI Integration + Sync E2E — DONE

**Prove the sync engine works end-to-end and add remaining CLI commands.**

- `conflicts` command: list unresolved conflicts from baseline (table or `--json`)
- `resolve` command: batch conflict resolution (`--keep-local`, `--keep-remote`, `--keep-both`, `--all`, `--dry-run`). Interactive mode deferred to Phase 5.
- `verify` command: full-tree hash verification (local files vs baseline DB)
- BaselineManager API: `ListConflicts`, `GetConflict`, `ResolveConflict` methods
- Engine API: `ListConflicts`, `ResolveConflict` with keep_local/keep_remote/keep_both strategies
- `VerifyBaseline`: read-only hash verification against baseline entries
- Sync E2E tests: upload-only, download-only, dry-run, verify, conflicts
- CI re-enablement: E2E test block uncommented in `integration.yml` (closes B-052, B-058)
- **Acceptance**: All DOD gates passed. 72.5% total coverage, sync package at 88.8%.
- **Inputs**: [prd.md](design/prd.md) section 4, [event-driven-rationale.md](design/event-driven-rationale.md) Part 10 (Phase 5)

### Wave Structure

**Wave 0**: 4v2.0 (clean slate) — prerequisite for everything. Delete old code, create stubs.

**Wave 1**: 4v2.1 (types + baseline) — foundation types that everything depends on.

**Wave 2**: 4v2.2 (remote observer) + 4v2.3 (local observer) — DONE. Independent of each other, both depend on types from 4v2.1.

**Wave 3**: 4v2.4 (change buffer) + 4v2.5 (planner) — DONE. Implemented in parallel (zero file conflicts). Buffer groups events by path; Planner converts events + baseline into ActionPlan.

**Wave 4**: 4v2.6 (executor) — DONE. Depends on planner output (action plan).

**Wave 5**: 4v2.7 (engine wiring) + 4v2.8 (CLI + sync E2E) — sequential, wires everything together.

---

## Phase 5: Concurrent Execution + Watch Mode

> **CLEAN SLATE INVARIANT**: When Phase 5 is complete, the codebase must appear as if it was written from scratch for the concurrent execution architecture. No vestiges of sequential phase execution, batch commits, or phase-ordered action plans will remain in the code. The ActionPlan contains actions with explicit dependency edges. The executor dispatches via a dependency tracker, not phase loops. Per-action atomic commits preserve incremental progress. The ONLY execution model visible in the code is the DAG-based concurrent model described in `docs/design/concurrent-execution.md`.

| Increment | Description | Wave |
|-----------|-------------|------|
| 5.0 | DAG-based concurrent execution engine | 0: The Pivot — **DONE** |
| 5.1 | Continuous observer `Watch()` methods + debounced buffer | 1: Watch Mode — **DONE** |
| 5.2 | `Engine.RunWatch()` + continuous pipeline | 1: Watch Mode — **DONE** |
| 5.3 | Graceful shutdown + crash recovery | 2: Operational Polish — **DONE** |
| 5.4 | Universal transfer resume + hardening | 2: Operational Polish — **DONE** |
| 5.5 | Pause/resume + config reload + final cleanup | 2: Operational Polish |

> **Ordering note (from architectural review, 2026-02-24):** Crash recovery
> (5.3) should land before or alongside watch mode (5.1/5.2). Without crash
> recovery, a process death loses in-flight transfer progress. The increment
> numbering reflects logical grouping, not strict execution order.

### 5.0: DAG-based Concurrent Execution Engine — DONE

**Replace 9-phase sequential executor with flat action list + dependency DAG + concurrent workers.**

- `ActionPlan` struct: 9 named slices → flat `Actions []Action` + `Deps [][]int` + `CycleID string`
- `buildDependencies()`: explicit DAG edges (parent-folder-before-child, child-delete-before-parent, move-target-parent)
- `DepTracker` (new): in-memory dependency graph, dispatches ready actions to interactive/bulk channels
- `WorkerPool` (new): lane-based workers (reserved interactive + reserved bulk + shared overflow), per-action commits
- `Baseline` gains `sync.RWMutex` + locked accessors (`GetByPath`, `GetByID`, `Put`, `Delete`, `Len`, `ForEachPath`) — B-089
- `CommitOutcome()` + `CommitDeltaToken()`: per-action atomic baseline commit, replacing batch `Commit()` — B-091
- `createdFolders` eliminated: DAG edges guarantee folder create committed to baseline before child dispatched — B-090
- `resolveParentID()`: baseline-only lookup (dropped `createdFolders` branch)
- Deleted: `Execute()`, `executeParallel()`, `Commit()`, `applyOutcomes()`, `executeAndCommit()`, `buildReport()`, `classifyOutcomes()`, `orderPlan()`, `appendActions()`, `createdFolders`, `workerPoolSize` constant
- Net -416 lines. All E2E tests pass unchanged. 74.2% total coverage, 88.9% sync package.
- Closes B-089, B-090, B-091.
- **Acceptance**: All DOD gates passed. Both CI workflows green.

### Codebase Analysis: Keep / Adapt / Delete

This analysis categorizes every part of the codebase by its relationship to the new architecture. Future agents must consult this section to understand what to touch and what to leave alone.

#### KEEP AS-IS (architecture-neutral, no changes needed)

| Package | Files | Reason |
|---------|-------|--------|
| `internal/graph/` | 19 .go files | Pure HTTP client, auth, retry. Zero sync dependencies. |
| `internal/config/` | 21 .go files | TOML config, XDG paths, drive sections. Zero sync dependencies. |
| `internal/driveid/` | 6 .go files | Type-safe IDs. Leaf package (stdlib only). |
| `pkg/quickxorhash/` | 2 .go files | Hash algorithm. Zero dependencies. |
| CLI commands | `files.go`, `auth.go`, `drive.go`, `format.go`, `root.go`, `main.go` | Call graph.Client directly, no sync engine coupling. |

#### KEEP AS-IS (sync package, per-action logic is architecture-neutral)

| File | What it does | Why it stays |
|------|-------------|--------------|
| `observer_remote.go` | `RemoteObserver.FullDelta()` → `[]ChangeEvent` | Pure observation. No execution model coupling. `Watch()` added in 5.1. |
| `observer_local.go` | `LocalObserver.FullScan()` → `[]ChangeEvent` | Pure observation. No execution model coupling. `Watch()` added in 5.1. |
| `buffer.go` | `Buffer` with `Add/AddAll/FlushImmediate` | Thread-safe event grouping. Debounce added in 5.1. |
| `verify.go` | `VerifyBaseline` — read-only hash check | Pure utility. Reads baseline via locked accessors after B-089. |
| `migrations.go` | Goose migration infrastructure + embed | Schema management unchanged. New migration added in 5.0. |
| `executor_transfer.go` | `executeDownload()`, `executeUpload()` | Self-contained per-action functions. Called by workers instead of phase loops. |
| `executor_delete.go` | `executeLocalDelete()`, `executeRemoteDelete()` | Self-contained per-action functions. |
| `executor_conflict.go` | `executeConflict()`, `executeEditDeleteConflict()` | Self-contained per-action functions. |
| `executor.go` (helpers) | `executeMove()`, `executeLocalMove()`, `executeRemoteMove()`, `createLocalFolder()`, `executeSyncedUpdate()`, `executeCleanup()`, `resolveActionItemType()`, `resolveDriveID()`, `withRetry()`, `classifyError()`, `calcExecBackoff()`, `failedOutcome()`, `folderOutcome()`, `moveOutcome()`, `downloadOutcome()`, `timeSleepExec()` | Architecture-neutral per-action functions and helpers. Called by workers. |
| `types.go` (most types) | `ChangeEvent`, `BaselineEntry`, `PathChanges`, `RemoteState`, `LocalState`, `PathView`, `ConflictRecord`, `VerifyResult`, `VerifyReport`, `Action`, `Outcome`, all enums, all interfaces | Architecture-neutral types. Only `ActionPlan` and `Baseline` change. |
| `engine.go` (conflict resolution) | `ListConflicts()`, `ListAllConflicts()`, `ResolveConflict()`, `resolveKeepLocal()`, `resolveKeepRemote()` | CLI-facing conflict operations. `resolveTransfer()` adapted (B-091). |
| `engine.go` (observers) | `observeRemote()`, `observeLocal()` | Pure observation wrappers. No execution coupling. |
| `engine.go` (safety) | `resolveSafetyConfig()` | Config helper. No execution coupling. |
| `SyncReport` struct | 9 named plan count fields + execution result fields | Architecture-neutral user-facing display counters populated by `countByType()`. Fields stay; population method changes. |
| CLI `sync.go` (`printSyncReport`) | Formats report for user display | Reads `SyncReport` fields. No internal coupling. |

#### ADAPT (keep logic, change structure)

| File | Current state | What changes |
|------|--------------|--------------|
| `types.go` (`ActionPlan`) | 9 ordered slices (`FolderCreates`, `Moves`, `Downloads`, etc.) | Replace with flat `Actions []Action` + `Deps [][]int` dependency adjacency list + `CycleID`. |
| `types.go` (`Baseline`) | Plain struct with public `ByPath`/`ByID` maps, no synchronization | Add `sync.RWMutex` field. All direct map access (`baseline.ByPath[x]`) replaced with locked accessor methods: `GetByPath(path) (*BaselineEntry, bool)` (RLock), `GetByID(key) (*BaselineEntry, bool)` (RLock), `Put(entry)` (Lock), `Delete(path)` (Lock). **This is a cross-cutting change** — touches every file that reads baseline maps: `planner.go` (`buildPathViews`), `executor.go` (`resolveParentID`), `verify.go` (`VerifyBaseline`), `observer_local.go`/`observer_remote.go`, `engine.go` (`resolveTransfer`). |
| `planner.go` | `appendActions()` routes to 9 slices. `orderPlan()` sorts slices. Logging uses `len(plan.FolderCreates)` etc. `bigDeleteTriggered()` uses `len(plan.LocalDeletes) + len(plan.RemoteDeletes)`. | Replace `appendActions()` with flat append + `buildDependencies()` for DAG edges. Replace `orderPlan()` (deleted). Logging switches to `countByType()`. `bigDeleteTriggered()` counts deletes from `plan.Actions` by type. Decision matrix logic (EF1-EF14, ED1-ED8) completely unchanged. All baseline reads use locked accessors. |
| `executor.go` | 9-phase `Execute()` method. `executeParallel()` for downloads/uploads. `createdFolders` per-Executor map. `resolveParentID()` checks `createdFolders` first. | DELETE `Execute()`, `executeParallel()`, `createdFolders`. KEEP `ExecutorConfig`/`Executor`/`NewExecution` pattern, retry/error classification. `resolveParentID()` drops `createdFolders` branch, uses only locked baseline accessor (B-090). `createRemoteFolder()` adapted: remove `e.createdFolders[action.Path] = item.ID` write — the worker's `CommitOutcome()` updates baseline incrementally instead. |
| `baseline.go` | `Commit(ctx, []Outcome, deltaToken, driveID)` — batch model. Cache invalidated and fully reloaded after commit. `NewBaselineManager()` creates and owns `*sql.DB`. | Replace batch `Commit()` with `CommitOutcome(ctx, outcome)` + `CommitDeltaToken(ctx, token, driveID)`. `CommitOutcome()` writes to DB then calls `baseline.Put()`/`baseline.Delete()` under write lock. No cache invalidation/reload — incremental updates only (B-089). |
| `engine.go` | `RunOnce()` — 9-step sequential pipeline. `executeAndCommit()` glue. `buildReport()` uses `len(plan.*)`. `resolveTransfer()` calls batch `Commit()`. | Rewrite `RunOnce()` for tracker→worker pipeline. DELETE `executeAndCommit()`, `buildReport()`. `resolveTransfer()` adapted to call `CommitOutcome()` (B-091). |
| CLI `sync.go` | Creates `Engine`, calls `RunOnce()`. `--watch` returns "not implemented". | Wire `--watch` to `RunWatch()` (5.2). `SyncReport` populated from `countByType()` and worker pool atomic counters. |

#### ADD (new components)

| File | Purpose |
|------|---------|
| *(removed)* | *(action_queue table dropped)* |
| `tracker.go` | In-memory dependency tracker: DAG, ready channel dispatch, interactive/bulk lane routing, bounded capacity, cancellation, refill loop. |
| `worker.go` | Lane-based worker pool: interactive + bulk + shared overflow workers, per-action commits, atomic success/failure/error counters. |
| `migrations/00002_action_queue.sql` | `CREATE TABLE action_queue` (later dropped in migration 00003). |

#### DELETE (old architecture artifacts removed during Phase 5)

| What | Where | Why |
|------|-------|-----|
| `Execute(ctx, plan *ActionPlan) ([]Outcome, error)` | `executor.go` | 9-phase dispatch loop → workers pull from tracker |
| `executeParallel()` | `executor.go` | errgroup pool for phases 3-4 → lane-based workers |
| `Commit(ctx, []Outcome, deltaToken, driveID)` | `baseline.go` | Batch commit → per-action `CommitOutcome()` |
| `executeAndCommit()` | `engine.go` | Sequential execute-then-commit glue |
| `buildReport()` | `engine.go` | `len(plan.FolderCreates)` etc. → `countByType(plan)` |
| `classifyOutcomes()` | `engine.go` | Batch outcome classification → worker pool atomic counters |
| `workerPoolSize = 8` constant | `executor.go` | Fixed pool → configurable lane workers |
| `golang.org/x/sync/errgroup` import | `executor.go` | errgroup only used by `executeParallel()` |
| 9-slice `ActionPlan` struct | `types.go` | Phase-ordered slices → flat list + dependency DAG |
| `appendActions()`, `orderPlan()` | `planner.go` | Slice routing/sorting → flat append + `buildDependencies()` |
| `createdFolders` map + first branch of `resolveParentID()` | `executor.go` | Per-executor map → incremental baseline updates (B-090) |
| `e.createdFolders[action.Path] = item.ID` write | `executor.go` (`createRemoteFolder`) | Outcome committed to baseline by worker instead |
| `upload_sessions` table | `00001_initial_schema.sql` | Replaced by file-based SessionStore |

**Legacy Architecture Reference**: See [`docs/design/legacy-sequential-architecture.md`](design/legacy-sequential-architecture.md) for detailed documentation of every old-architecture pattern, its rationale, and grep commands to verify removal. This document is the definitive reference for the clean-slate invariant.

---

### CI and Testing Strategy

> **CI GREEN WHEN EACH INCREMENT MERGES.** Both `ci.yml` and `integration.yml` pass. No temporary CI disablement, no skipped E2E tests.
>
> **E2E sync tests are the safety net for 5.0.** `sync_e2e_test.go` and `sync_full_test.go` test external CLI behavior — they run `onedrive-go sync` and check files appear locally/remotely. Internal execution model changes completely; external behavior does not. These tests verify the pivot didn't break anything.
>
> **Unit tests rewritten alongside the code they test.** No increment introduces code without tests. Dead code verification via grep patterns at each increment.
>
> See [`docs/design/legacy-sequential-architecture.md`](design/legacy-sequential-architecture.md) for the full pattern detection reference.

---

### Wave 0 — The Pivot

#### 5.0: DAG-based concurrent execution engine

**THE ARCHITECTURAL PIVOT.** This single increment replaces the entire sequential execution model with the DAG-based concurrent architecture described in `concurrent-execution.md`. No bridge code, no intermediate states. The old execution model is deleted and the new one takes its place.

**Scope**: ActionPlan restructure + dependency tracker + lane-based workers + engine rewrite. Everything that touches the execution model changes in this one increment.

**What does NOT change**: Observers (`FullDelta`, `FullScan`), buffer (`FlushImmediate`), planner decision matrix (EF1-EF14, ED1-ED8), per-action executor functions (`executeDownload`, `executeUpload`, `executeLocalDelete`, `executeRemoteDelete`, `executeConflict`, `executeEditDeleteConflict`, `executeFolderCreate`/`createLocalFolder`, `executeMove`/`executeLocalMove`/`executeRemoteMove`, `executeSyncedUpdate`, `executeCleanup`), all helper functions (`resolveActionItemType`, `resolveDriveID`, `withRetry`, `classifyError`, `classifyStatusCode`, `calcExecBackoff`, `failedOutcome`, `folderOutcome`, `moveOutcome`, `downloadOutcome`, `conflictCopyPath`, `conflictStemExt`, `timeSleepExec`, `deleteOutcome`, `downloadToPartial`), all types except `ActionPlan` and `Baseline`, all non-sync packages, CLI `printSyncReport()`, `SyncReport` struct fields.

**1. New Code:**

*Action plan + dependencies (planner layer):*
- `buildDependencies()` in `planner.go` — constructs `[][]int` DAG edges:
  - Parent-before-child edges for folder creates
  - Children-before-parent-delete edges
  - Move-target-parent edges
- `actionsOfType(plan, ActionType) []Action` helper for filtering flat list
- `countByType(plan) map[ActionType]int` helper for report building

*Baseline locked accessors (B-089, cross-cutting):*
- `Baseline` struct gains `mu sync.RWMutex` field
- `GetByPath(path string) (*BaselineEntry, bool)` — acquires RLock
- `GetByID(key driveid.ItemKey) (*BaselineEntry, bool)` — acquires RLock
- `Put(entry *BaselineEntry)` — acquires Lock, updates both `ByPath` and `ByID` maps
- `Delete(path string)` — acquires Lock, removes from both maps
- `Len() int` — acquires RLock, returns `len(ByPath)` (used by planner logging and big-delete check)
- All callers that do `baseline.ByPath[x]` migrated to `baseline.GetByPath(x)`. **Affected files**: `planner.go` (`buildPathViews`, `bigDeleteTriggered`), `executor.go` (`resolveParentID`), `verify.go` (`VerifyBaseline`), `observer_local.go` (baseline lookups), `observer_remote.go` (baseline lookups), `engine.go` (`resolveTransfer`). The maps remain public for backward compatibility in tests but production code uses only the locked accessors.

*Migration:*
- `migrations/00002_action_queue.sql` creates `action_queue` table (later dropped in migration 00003).

*Per-action commit (replaces batch commit):*
- `baseline.go`: `CommitOutcome(ctx, outcome) error` — per-action atomic baseline upsert (B-091).
- `baseline.go`: `CommitDeltaToken(ctx, token, driveID) error` — separate delta token commit

*Dependency tracker (dispatch layer):*
- `tracker.go`: `DepTracker` struct per `concurrent-execution.md` §4
  - `Add(action, id, deps)` — insert, dispatch if no deps
  - `Complete(id) error` — mark done, decrement dependents' counters, dispatch newly ready
  - `Cancel(path)` — cancel in-flight action by path
  - `Interactive() <-chan *trackedAction` — interactive lane channel
  - `Bulk() <-chan *trackedAction` — bulk lane channel
  - Lane routing: files < 10 MB + folder ops + deletes → interactive; files ≥ 10 MB → bulk
  - Bounded capacity (configurable, default 10K), signaling refill when below threshold

*Worker pool (execution layer):*
- `worker.go`: `WorkerPool` struct
  - `NewWorkerPool(cfg, tracker, baseline, workerCounts) *WorkerPool`
  - `Start(ctx)` — spawn interactive, bulk, and shared overflow workers
  - `Wait()` — block until all actions complete
  - `Stop()` — cancel workers, drain
  - Each worker: pull from tracker channel → `NewExecution()` → dispatch per-action function → `CommitOutcome()` → `tracker.Complete()`

**2. Code Adaptation:**

- `types.go`: `ActionPlan` → flat `Actions []Action` + `Deps [][]int` + `CycleID`. `Baseline` struct gains `sync.RWMutex` + locked accessors. All other types unchanged.
- `planner.go`: classification functions unchanged. `appendActions()` → direct `append(plan.Actions, action)`. `orderPlan()` → `buildDependencies()`. Logging at `Plan()` entry/exit switches from `len(plan.FolderCreates)` to `countByType()`. `bigDeleteTriggered()` counts deletes from `plan.Actions` by type instead of `len(plan.LocalDeletes) + len(plan.RemoteDeletes)`. `buildPathViews()` uses `baseline.GetByPath()` instead of `baseline.ByPath[x]`.
- `baseline.go` — **concurrent-safe incremental cache** (B-089): `CommitOutcome()` writes to DB then calls `baseline.Put()` or `baseline.Delete()` under the write lock. No cache invalidation/reload. `Load()` still does full DB load on first call (or after explicit invalidation for crash recovery). Expose `DB() *sql.DB` accessor for shared connection.
- `executor.go` — **eliminate `createdFolders` map** (B-090): `resolveParentID()` drops its first branch (the `createdFolders` lookup). Since `CommitOutcome()` now updates the baseline incrementally, newly-created folders appear in baseline immediately after their action completes. `resolveParentID()` uses `baseline.GetByPath()` locked accessor. DAG edges guarantee a folder create completes before any child action is dispatched. `createRemoteFolder()` adapted: remove `e.createdFolders[action.Path] = item.ID` write (line 242) — the worker's `CommitOutcome()` handles this. `Executor` struct field `createdFolders` deleted; `NewExecution()` no longer initializes it.
- `engine.go` — `RunOnce()` rewritten for tracker→worker pipeline:
  1. Load baseline
  2. Observe remote/local (unchanged)
  3. Buffer and flush (unchanged)
  4. Plan (unchanged — returns flat Actions + Deps)
  5. Early return if dry-run (use `countByType()` for report, no tracker/workers)
  6. Populate tracker with actions and dependency edges
  7. Start worker pool → per-action commits happen inside workers
  8. Wait for all actions to complete
  9. Commit delta token
  10. Populate SyncReport from worker pool atomic counters
- `engine.go` — **`resolveTransfer()` adaptation** (B-091): calls `CommitOutcome(ctx, outcome)` for per-action baseline commit.
- `migrations.go`: embed includes new migration file.
- CLI `sync.go`: `SyncReport` plan counts populated from `countByType()`. Execution counts (`Succeeded`, `Failed`, `Errors`) populated from `WorkerPool` atomic counters instead of `classifyOutcomes()`.
- `observer_local.go`, `observer_remote.go`: baseline reads use locked accessors (`baseline.GetByPath()`, `baseline.GetByID()`). Logic unchanged.
- `verify.go`: `VerifyBaseline()` iterates using `baseline.GetByPath()` or a `baseline.Entries()` iterator method. Logic unchanged.

**3. Code Retirement:**
- DELETE `Execute()`, `executeParallel()`, `workerPoolSize` from `executor.go`
- DELETE `golang.org/x/sync/errgroup` import from `executor.go` (only used by `executeParallel()`)
- DELETE `executeAndCommit()` from `engine.go`
- DELETE `buildReport()` from `engine.go` (replaced by `countByType`)
- DELETE `classifyOutcomes()` from `engine.go` (replaced by worker pool atomic counters)
- DELETE batch `Commit()` from `baseline.go` (replaced by per-action `CommitOutcome()`)
- DELETE `applyOutcomes()` from `baseline.go` (internal to batch `Commit()`)
- DELETE `appendActions()`, `orderPlan()`, `pathDepth()` from `planner.go` (`pathDepth` is only used by `orderPlan`)
- DELETE 9 slice fields from `ActionPlan` in `types.go` (`FolderCreates`, `Moves`, `Downloads`, `Uploads`, `LocalDeletes`, `RemoteDeletes`, `Conflicts`, `SyncedUpdates`, `Cleanups`)
- DELETE `ActionPlan` doc comment referencing "9 ordered slices" from `types.go`
- DELETE `createdFolders` field from `Executor` struct, first branch in `resolveParentID()`, `createdFolders` write in `createRemoteFolder()`
- DELETE `upload_sessions` table via migration (replaced by file-based SessionStore)
- Verify — run full sweep from [`legacy-sequential-architecture.md`](design/legacy-sequential-architecture.md) §9:
  ```
  grep -rn "plan\.FolderCreates\|plan\.Downloads\|plan\.Uploads\|plan\.Moves\|plan\.LocalDeletes\|plan\.RemoteDeletes\|plan\.Conflicts\|plan\.SyncedUpdates\|plan\.Cleanups\|appendActions\|orderPlan\|executeParallel\|workerPoolSize\|executeAndCommit\|\.Commit(ctx.*\[\]Outcome\|createdFolders\|len(plan\.\|classifyOutcomes\|applyOutcomes\|buildReport" internal/sync/ --include="*.go" --exclude="*_test.go"
  ```
  → 0 hits. Also verify no stale doc comments reference "9 phases", "9 slices", or "sequential" in non-legacy-doc production code.

**4. CI and Testing:**
- `planner_test.go`: all 43 tests rewritten — `len(plan.Downloads)` → `actionsOfType(plan, ActionDownload)`. All `baseline.ByPath[x]` direct accesses → `baseline.GetByPath(x)` or direct map access in test-only setup code. New dependency edge tests: parent→child for folder creates, child→parent for deletes, move-target-parent, independent actions get no edges.
- `executor_test.go`: DELETE tests calling `Execute(plan)`. KEEP per-action function tests (they use `NewExecution()` which still works). Update `resolveParentID` tests to use locked baseline accessors. Verify `createRemoteFolder` no longer writes to `createdFolders`. Add concurrency test with `-race`: two workers calling per-action functions sharing a baseline.
- `engine_test.go`: REWRITE for tracker→worker pipeline. Same scenarios (bidirectional sync, download-only, upload-only, dry-run, big-delete), different internal flow.
- `baseline_test.go`: DELETE batch `Commit()` and `applyOutcomes()` tests. ADD `CommitOutcome()` tests: single action, concurrent access under `-race`, upsert + delete + move + conflict outcome types. ADD `CommitDeltaToken()` test. ADD locked accessor tests (`GetByPath`, `GetByID`, `Put`, `Delete` under concurrent access).
- New `tracker_test.go`: dependency chains (parent→child dispatch ordering), lane routing (small file → interactive, large file → bulk), concurrent access with `-race`, cancellation (Cancel(path) triggers context cancel on in-flight action), Complete unblocks dependents.
- New `worker_test.go`: lifecycle (Start/Wait/Stop), per-action commit (verify baseline updated after worker completes folder create, subsequent worker sees it via `resolveParentID`), error handling (failed action doesn't block dependents), lane assignment (interactive vs bulk workers pull from correct channels), atomic counters (Succeeded/Failed incremented correctly).
- **E2E sync tests: MUST PASS UNCHANGED.** Same actions execute (same per-action functions), same baseline entries written, same files on disk/remote. Execution order is more parallel but results are identical. The `sync_e2e_test.go` and `sync_full_test.go` tests exercise the CLI binary end-to-end — they are the primary safety net for the pivot.
- Both CI workflows green (`ci.yml` and `integration.yml`).

---

### Wave 1 — Watch Mode

#### 5.1: Continuous observer Watch() methods + debounced buffer — DONE

**Goal**: Add `Watch()` to both observers and debounce to buffer.

- `RemoteObserver.Watch()`: continuous delta polling loop with exponential backoff (5s initial, 2× multiplier, capped at poll interval). `CurrentDeltaToken()` thread-safe accessor for engine integration. `ErrDeltaExpired` (410) resets token for full resync. Injectable `sleepFunc` for test control.
- `LocalObserver.Watch()`: fsnotify-based filesystem event monitoring + periodic safety scan (5 min). `FsWatcher` interface for testability. Recursive directory watch setup via `addWatchesRecursive()`. Classify events vs baseline for change type (create/modify/delete). New directory watches added dynamically.
- `Buffer.FlushDebounced()`: debounce-timer-based batching via output channel. Timer resets on each `Add()`/`AddAll()`. Final drain on context cancellation. Non-blocking `signalNew()` notification from `addLocked()`.
- B-095 fixed: `DepTracker.byPath` cleaned up in `Complete()` and `CancelByPath()`.
- Dependency: `github.com/fsnotify/fsnotify` v1.9.0 (chosen over `rjeczalik/notify` — actively maintained, de facto standard, used by Hugo/Docker/Kubernetes).
- `FullDelta()`/`FullScan()`/`FlushImmediate()` remain unchanged for one-shot mode.
- 17 new tests: 5 remote watch, 7 local watch, 5 buffer debounce. All pass with `-race`.
- **Acceptance**: All DOD gates passed. Both CI workflows green.

---

#### 5.2.0: Parallel remote + local observation in RunOnce (B-170)

**Goal**: Overlap network-bound remote observation with disk-bound local observation. Immediate performance win for bidirectional sync.

**Rationale**: Remote observation is network-bound (Graph API). Local observation is disk-bound (readdir + hash). Different I/O resources, zero shared mutable state (both read baseline in read-only mode). Running them concurrently is free parallelism that halves observation time.

**1. New Code:**
- `engine.go`: `RunOnce` steps 2-3 wrapped in `errgroup.Go()` (both goroutines return events + error, assembled after `g.Wait()`).

**2. Code Adaptation:**
- None — `observeRemote` and `observeLocal` are already independent methods with no shared mutable state.

**3. Code Retirement:**
- None

**4. CI and Testing:**
- Existing tests pass unchanged (observation order is irrelevant to correctness).
- Both CI workflows green.

---

#### 5.2.1: Parallel FullScan hashing (B-096)

**Goal**: Parallelize the #1 performance bottleneck — sequential file hashing during initial sync.

**Rationale**: `FullScan` walks the filesystem sequentially and hashes each file inline. For initial syncs with no baseline (every file is new → needs hash), hashing is 99.98% of total time (100K files × 1MB avg: walk ~200ms, sequential hash ~15 min, parallel hash with 8 cores ~2 min). The walk itself must stay sequential because disk metadata operations (readdir, Lstat) are I/O-serialized at the hardware level — parallel readdir contends on the same disk queue, and on NFS it actively hurts due to IOPS limits.

**Design**: Walk runs to completion exactly as today, populating the `observed` map and collecting a `[]hashJob` slice for files that need hashing (new files, mtime/size changed, racily clean). Then hash jobs are fanned out to `errgroup.SetLimit(runtime.NumCPU())`. Results are collected into `[]ChangeEvent`. Deletion detection runs after the pool drains. ~30 lines of new code, zero architectural change.

**1. New Code:**
- `observer_local.go`: `hashJob` struct, parallel hash fan-out in `FullScan` after walk completes.

**2. Code Adaptation:**
- `go.mod`: re-add `golang.org/x/sync` (for `errgroup.SetLimit`).

**3. Code Retirement:**
- None

**4. CI and Testing:**
- New test: `TestFullScan_ParallelHashing` — verify correct results with concurrent hashing.
- Existing FullScan tests pass unchanged.
- Both CI workflows green.

---

#### 5.2.2: RunWatch() + continuous pipeline

**Goal**: Wire continuous observers into the engine. `sync --watch` works.

**1. New Code:**
- `engine.go`: `RunWatch(ctx, mode, opts) error` —
  1. Load baseline
  2. Start worker pool (persistent — survives across planning passes)
  3. Start remote observer `Watch` (if not upload-only)
  4. Start local observer `Watch` (if not download-only)
  5. Loop: wait for buffer flush → plan → deduplicate against tracker (B-122) → add to tracker
  6. Delta token management per cycle (B-121)
  7. Action cancellation for stale in-flight actions when new events arrive
- Write event coalescing in watch event loop (B-107): per-path debounce timer before hashing, so rapid saves produce one hash job, not ten. This is a watch-mode-only concern — FullScan parallel hashing (5.2.1) is separate and simpler.

**2. Code Adaptation:**
- CLI `sync.go`: `--watch` wired to `RunWatch()`

**3. Code Retirement:**
- DELETE "not implemented" watch stub
- Verify: `grep -rn "not.*implemented" sync.go` → 0 hits for watch

**4. CI and Testing:**
- New engine watch tests with mock observers
- New E2E watch test (`e2e` tag): start watch, make change, verify sync, stop (short timeout)
- Both CI workflows green.

---

### Wave 2 — Operational Polish

#### 5.3: Graceful shutdown + crash recovery — DONE

**Goal**: Two-signal shutdown, crash recovery via idempotent planner, P2 hardening.

**1. New Code:**
- `signal.go`: Two-signal shutdown handler (first SIGINT = graceful drain, second = force exit)
- `failure_tracker.go`: Repeated failure suppression for watch mode (B-123)
- `engine.go`: Crash recovery via idempotent planner, drive identity verification (B-074), configurable safety scan interval (B-099)
- `executor_transfer.go`: Resumable downloads from `.partial` files (B-085)
- `observer_local.go`: Stable hash detection for actively-written files (B-119)
- `graph/download.go`: `DownloadRange` with HTTP Range header (B-085)
- `graph/upload.go`: `ResumeUpload` for interrupted chunked uploads (B-037)
- `session_store.go`: `SessionStore` for file-based upload session persistence
- `types.go`: `DriveVerifier`, `RangeDownloader`, `SessionResumer` interfaces

**2. Code Adaptation:**
- CLI `sync.go`: Signal handler wired before engine creation
- `engine.go`: `processBatch` properly handles suppressed actions

**3. Code Retirement:**
- None

**4. CI and Testing:**
- `engine_recovery_test.go`: 11 tests (crash recovery, drive verification, cycle results, synthetic view)
- `failure_tracker_test.go`: 4 tests (threshold, cooldown, success clearing, path independence)
- `signal_test.go`: 2 tests (signal cancellation, parent cancel)
- `download_test.go`: 2 tests (range download, no-URL error)
- `upload_test.go`: 2 tests (resume upload, expired session)
- `session_store_test.go`: concurrent Save/Load/Delete tests
- E2E: unchanged, all pass. 75.2% coverage maintained.

---

#### 5.4: Universal transfer resume — DONE

**Goal**: Add file-based transfer resume shared between CLI and sync engine. Unified TransferManager.

**1. New Code:**
- `session_store.go`: File-based upload session persistence (JSON files, SHA256-keyed by driveID:remotePath, 7-day TTL)
- `transfer_manager.go`: Unified download/upload with resume, shared between CLI and sync engine
- `graph/upload.go`: `UploadFromSession()` method — uploads all chunks for an existing session
- `config/paths.go`: `UploadSessionDir()` helper
- `files.go`: Download resume via `.partial` files for CLI `get`, upload session resume for CLI `put`
- `executor_transfer.go`: Delegates to `TransferManager` for download/upload with resume
- `engine.go`: Post-sync stale `.partial` file reporting and stale session cleanup
- `migrations/00003_drop_action_queue.sql`: Drops unused tables (`action_queue`, `stale_files`, `config_snapshots`, `change_journal`)

**2. Key Design Decisions:**
- **Idempotent planner as crash recovery**: Delta re-observation on restart produces same actions. Items completed before crash are in baseline (EF1 no-ops). Transfer resume is served by file-based storage shared between CLI and sync engine.
- **Remote-scoped session keys**: `sha256(driveID + ":" + remotePath)` — server invalidates old sessions on new creation, so stale records just produce 404.
- **Optimistic download resume**: Graph API provides only full-file hashes. Resume appends via `DownloadRange`, then verifies full-file hash. Same approach as `wget -c`.

**3. CI and Testing:**
- `session_store_test.go`: 10 tests (save/load/delete, corrupt file, overwrite, different keys, clean stale, permissions, deterministic keys, stale partials)
- All existing tests pass. E2E pass. Lint clean.
- Backlog: B-092 done, B-097/B-162/B-175 superseded, B-200/B-201/B-202 created

#### 5.4.2: TransferManager hardening + defensive fixes — DONE

**Goal**: 18 concrete fixes (3 critical, 4 high, 4 medium, 7 small) found in post-5.4 code review. Purely robustness, correctness, and test coverage — no new features.

**Critical fixes:**
- Preserve `.partial` files on context cancellation (Ctrl-C) — guard `os.Remove` with `ctx.Err() == nil` at 4 locations
- Restore `withRetry` wrapping for TransferManager download/upload calls — operation-level retry for mid-stream TCP resets
- Worker goroutine panic recovery — `safeExecuteAction` with `recover()` prevents single-action panic from crashing the process

**High fixes:**
- Add `Size`/`Mtime` to `UploadResult` — eliminate redundant `os.Stat` TOCTOU race in `executeUpload`
- Nil-check returned `Item` after upload — prevent panic on `(nil, nil)` return
- Defer TransferManager construction to `NewEngine` — immutable after creation, no field mutation
- Check `f.Close()` error in `resumeDownload` — corrupt partial falls back to fresh download

**Medium fixes:**
- Document `sendResult` blocking invariant (buffer sizing contract)
- Panic recovery for async `reportStalePartials` goroutine
- Log `f.Close()` failure in `freshDownload` error path
- Add `driveID` to TransferManager debug logs

**Small fixes:**
- Wrap simple-upload error with local path context
- Parent dir perms `0o755` → `0o700` (owner-only)
- Expand `MaxHashRetries` comment (3 total attempts)
- Document hash waste on resume+mismatch as acceptable
- Enhanced session save failure log message

**CI and Testing:**
- `transfer_manager_test.go`: 16 tests covering all download/upload paths, hash retry/exhaustion, session resume, nil-item, drive ID logging, parent dir perms
- `worker_test.go`: `TestWorkerPool_PanicRecovery` — panicking action completes without process crash
- All gates pass. 74.8% total coverage, 86.4% sync package.

#### 5.5: Pause/resume + config reload + final cleanup

**Goal**: Complete Phase 5 feature set. Ensure clean slate. The `paused` field is the sole drive lifecycle mechanism.

**1. New Code:**
- `pause` CLI command: sets `paused = true` in config section (+ optional `paused_until` for timed pause via duration argument, e.g., `pause 2h`). Sends SIGHUP to daemon via PID file.
- `resume` CLI command: removes `paused`/`paused_until` from config section. Without `--drive`, resumes all drives. Sends SIGHUP to daemon via PID file.
- SIGHUP-based config reload: `sync --watch` reloads config on SIGHUP. CLI commands write config and send SIGHUP to the daemon. PID file with flock prevents multiple daemons.

**2. Config Migration (`Enabled` → `Paused`) — DONE:**
- ~~`config.Drive` struct: replace `Enabled *bool` with `Paused *bool`~~ ✓
- ~~Refactor `drive remove`: delete config section instead of setting `enabled = false`~~ ✓
- ~~Refactor `drive add` (re-add): reports "already configured" if drive exists~~ ✓
- ~~Refactor `drive remove --purge`: delete config section + state DB~~ ✓ (unchanged, already correct)
- ~~Refactor `logout`: delete config sections for all account drives~~ ✓
- `PausedUntil *string` field: deferred to when `pause`/`resume` commands are implemented
- Timed pause expiry: deferred to when `pause`/`resume` commands are implemented
- The `paused` field is the only drive lifecycle mechanism
- See [MULTIDRIVE.md §11.10](design/MULTIDRIVE.md#1110-drive-lifecycle) for full spec.

**3. Code Retirement:**
- ~~Delete `Drive.Enabled` field and all references~~ ✓
- Final sweep — run ALL grep patterns from [`docs/design/legacy-sequential-architecture.md`](design/legacy-sequential-architecture.md) §9
- Doc comment audit: no production `.go` file should reference "9 phases", "9 slices", "sequential execution", or "batch commit" except in historical/explanatory context.

**4. CI and Testing:**
- Pause/resume tests (config-based + CLI-level)
- SIGHUP config reload test
- Timed pause expiry test
- Docs updated: CLAUDE.md, BACKLOG.md, LEARNINGS.md
- Both CI workflows green. Full DOD checklist.

#### 5.6: Identity Refactoring + Personal Vault Exclusion — **DONE**

**Goal**: Prepare the identity and config system for multi-drive and shared folder sync. Add Personal Vault exclusion as a safety requirement. All sub-tasks are code changes — identity refactoring must land before shared folder sync (Phase 7).

**Completed**: All 6 sub-increments shipped. `DriveTypeShared` added to `driveid`, token resolution moved to `config`, `Alias` replaced with auto-derived `DisplayName`, delta tokens upgraded to composite key `(drive_id, scope_id)`, Personal Vault items excluded from sync. Net: 26 files changed, ~1500 lines added.

##### 5.6.1: Personal Vault exclusion

- Detect `specialFolder.name == "vault"` in RemoteObserver, skip items
- Add `sync_vault` config option (default `false`) with auto-lock warning log
- Log at INFO when vault items are skipped
- Must land before sync is used in production
- **Acceptance**: Vault items never appear in baseline, planning, or execution. `sync_vault = true` overrides with warning.

##### 5.6.2: Add `DriveTypeShared` to `driveid` package

- New constant `DriveTypeShared = "shared"`
- New struct fields: `sourceDriveID`, `sourceItemID`
- New constructor: `ConstructShared(email, sourceDriveID, sourceItemID)`
- New accessors: `IsShared()`, `SourceDriveID()`, `SourceItemID()`
- Remove `TokenCanonicalID()` method (token resolution is business logic, not identity)
- Update `validDriveTypes` map, `canonicalIDMaxParts` stays at 4
- Update `NewCanonicalID()` parser for shared format
- Update `String()`, `MarshalText()`, `UnmarshalText()`
- **Acceptance**: Shared drive canonical IDs can be constructed and round-tripped. `grep -rn "TokenCanonicalID()" internal/driveid/` → 0 hits.

##### 5.6.3: Move token resolution to `config` package

- New function: `config.TokenCanonicalID(cid driveid.CanonicalID, cfg *Config) (driveid.CanonicalID, error)`
- Logic: personal/business → return self; sharepoint → business with same email; shared → find primary drive for email in `cfg.Drives`
- Update call sites: `drive.go:addNewDrive`, `config/drive.go:DriveTokenPath`, `config/drive.go:ReadTokenMeta`
- **Acceptance**: All existing tests pass. Token resolution works for all four drive types.

##### 5.6.4: Replace `Alias` with `DisplayName` in config

- `config.Drive` struct: remove `Alias string`, add `DisplayName string` and `Owner string`
- `config.ResolvedDrive` struct: remove `Alias string`, add `DisplayName string` and `Owner string`
- Update `MatchDrive()` matching priority: exact canonical → exact display_name (case-insensitive) → substring on canonical, display_name, owner
- Update `matchBySelector()`: replace alias check with display_name check
- Update `DefaultSyncDir()` for shared drives: `~/OneDrive-Shared/{display_name}`
- Update `AppendDriveSection()` to write `display_name` and `owner` TOML fields
- Display name auto-derivation at drive add:
  - Personal: email
  - Business: email
  - SharePoint: `"site / lib"` with uniqueness escalation to `"site / lib (email)"`
  - Shared: `"{FirstName}'s {FolderName}"` with escalation
- Update all test fixtures from alias to display_name
- **Acceptance**: `grep -rn "\.Alias\b" --include="*.go"` → 0 hits in non-test code. `grep -rn "alias" internal/config/ --include="*.go"` → 0 hits.

##### 5.6.5: Update CLI for display_name

- `drive list` (`drive.go`): show display_name column for configured drives, derive display_name for available drives
- `drive add` (`drive.go`): substring match against derived display_name, auto-fill display_name/owner/sync_dir
- `drive remove` (`drive.go`): use `--drive` with display_name matching
- `status` (`status.go`): show display_name in output
- `--drive` help text (`root.go`): update to mention display_name matching
- Error messages: use display_name not canonical ID in user-facing errors
- **Acceptance**: `--drive "me@outlook.com"` matches personal drive by display_name. All user-facing output shows display_name.

##### 5.6.6: Delta token schema update

- New migration `00004_delta_token_composite_key.sql`
- `delta_tokens` table: `PRIMARY KEY (drive_id, scope_id)` with `scope_drive TEXT NOT NULL`
- Primary delta: `scope_id = ""`, `scope_drive = drive_id`
- Shortcuts: `scope_id = remoteItem.id`, `scope_drive = remoteItem.driveId`
- **Acceptance**: Migration applies cleanly. Existing single-drive delta tokens preserved with `scope_id = ""`.

---

#### 5.7: Remote State Separation — Schema + SyncStore Foundation

**Goal**: Lay the schema and code foundation for the remote-state-separation architecture (see [remote-state-separation.md](design/remote-state-separation.md)). No behavioral changes — the sync pipeline still uses the existing baseline-only flow. This increment adds the new tables, renames, and pure functions that 5.7.1+ will wire into the live sync path.

##### 5.7.0: Schema + SyncStore Foundation + computeNewStatus() — **DONE**

**5.7.0a** (additive, mechanical):
1. Consolidated 5 migration files into single `00001_consolidated_schema.sql` with `remote_state` (16 cols, 9-value state machine) and `local_issues` (10 cols) tables
2. Renamed `BaselineManager` → `SyncStore` across 12 files
3. Removed `CycleID` from `ActionPlan` + `planner.go` (moved to engine-local generation)
4. Added `computeNewStatus()` pure function implementing 30-cell decision matrix (§11)
5. Added `SyncStore.Checkpoint()` method (WAL checkpoint + pruning)
6. Added 6 sub-interface declarations (`ObservationWriter`, `OutcomeWriter`, `FailureRecorder`, `ConflictEscalator`, `StateReader`, `StateAdmin`) + `ObservedItem`/`RemoteStateRow` structs

**5.7.0b** (baseline PK change):
1. Changed baseline table PK from `path` to `(drive_id, item_id)` with `path UNIQUE`
2. Updated SQL: `ON CONFLICT(drive_id, item_id)`, path in UPDATE SET, stale-path clearing
3. `Baseline.Put()` removes stale ByID entries on path reassignment
4. `commitMove()` simplified to single UPSERT (not DELETE+INSERT)

Net: 6 new files, 12 modified. 33 new tests. Coverage: 86.6% sync package.

---

## Phase 6: Multi-Drive Orchestration + Shared Content Sync

**Single-process multi-drive sync.** After this phase, `sync --watch` syncs all non-paused drives simultaneously from a single process. Each drive has its own goroutine, state DB, and sync cycle. Identity refactoring (four drive types, display_name, token resolution in config) was completed in Phase 5.6.

> **Architecture resolved**: Architecture A (per-drive goroutine with isolated engines). See [MULTIDRIVE.md §11](design/MULTIDRIVE.md#11-multi-drive-orchestrator) for full specification. The Orchestrator is ALWAYS used, even for a single drive — no separate single-drive code path.

### Dependency Graph

```
6.0a ──→ 6.0b ──→ 6.0c ──→ 6.0d
  │                          │
  ├── 6.2b                   │
  │                          │
  └── 6.4a ─────────────→ 6.4b

6.1 (DONE)    6.2a (DONE)    6.3 (after 6.0a)
```

### 6.0a: DriveSession + ResolveDrives + shared drive foundations — DONE

Prerequisite refactoring that unblocks all other Phase 6 work.

1. **DriveSession type** (B-223): **DONE** — `DriveSession` struct with Client (30s timeout), Transfer (no timeout), TokenSource, DriveID, Resolved. `NewDriveSession()` constructor. Replaced 9 `clientAndDrive()` call sites.
2. **`config.ResolveDrives()`**: **DONE** — Returns `[]*ResolvedDrive` for all non-paused drives or specified subset. Sorted by canonical ID.
3. **`BaseSyncDir` for `DriveTypeShared`**: **DONE** — Returns `~/OneDrive-Shared/{displayName}`. Signature: `BaseSyncDir(cid, orgName, displayName)`.
4. **SyncRoot overlap validation**: **DONE** — `checkSyncDirOverlap(cfg)` + `isAncestorOrDescendant()`. Called from `validateDrives()`.
5. **`OwnerEmail` on `graph.Drive`** (B-279): **DONE** — Added in foundation hardening PR.
6. **`DriveTokenPath` shared case**: **DONE** — Signature: `DriveTokenPath(cid, cfg)`. Passes cfg to `TokenCanonicalID` for shared drive resolution.
7. **B-224: Eliminate global flag variables**: **DONE** — Two-phase CLIContext: `CLIFlags` struct + `Provider`. Zero global mutable flag state.
8. **B-283: URL-encode SearchSites query**: **DONE** — `url.QueryEscape(query)` in `SearchSites()`.

Acceptance: all criteria met. `clientAndDrive` eliminated. `ResolveDrives` resolves N drives. Shared drives get correct defaults. Overlapping sync_dirs rejected. Coverage 75.1% → 76.3%.

### 6.0b: Orchestrator + DriveRunner (always-on) — DONE

Core multi-drive runtime. The Orchestrator is ALWAYS used, even for a single drive.

1. **Orchestrator struct** in `internal/sync/orchestrator.go`: `OrchestratorConfig`, `clientPair`, injectable `engineFactory` + `tokenSourceFn`. **DONE** — `NewOrchestrator(cfg)` constructor.
2. **DriveRunner** in `internal/sync/drive_runner.go`: `DriveReport`, `DriveRunner.run()` with `defer recover()`, `backoffDuration()` (3 consecutive failures → 1m, 5m, 15m, 1h cap). **DONE**.
3. **Shared `graph.Client` per token path**: `getOrCreateClient(tokenPath)` with `map[string]*clientPair` caching. **DONE**.
4. **`Orchestrator.RunOnce(ctx, mode, opts)`**: resolve tokens, create clients, create engines via factory, launch DriveRunners concurrently, collect reports. **DONE**.
5. **sync command rewrite**: `skipConfigAnnotation`, loads raw config via `LoadOrDefault`, resolves drives via `ResolveDrives`, creates Orchestrator → `RunOnce` → `printDriveReports`. Watch mode bridge (`runSyncWatchBridge`) routes to existing single-drive path — **temporary**, eliminated in 6.0c when `Orchestrator.RunWatch` replaces it. **DONE**.
6. **Annotation tests**: `sync` moved to skipConfig list in `TestAnnotationBasedSkipConfig`. **DONE**.

Acceptance: all criteria met. `sync` and `sync --drive X` run through Orchestrator. One drive failure/panic doesn't affect others. Single-drive output identical to previous path. E2E tests pass. Coverage 76.1%.

### 6.0c: Worker budget + daemon mode + config reload — DONE

1. **`transfer_workers` config key**: integer, default 8, range 4-64. Sync action workers (downloads, uploads, renames, deletes, mkdirs). Replaces hardcoded `runtime.NumCPU()` in `engine.go`. **DONE**.
2. **`check_workers` config key**: integer, default 4, range 1-16. Controls `errgroup.SetLimit` for concurrent QuickXorHash in LocalObserver FullScan. **DONE**.
3. **Deprecate old keys**: `parallel_downloads`, `parallel_uploads`, `parallel_checkers`. Log warning via `WarnDeprecatedKeys()` if found in config, ignore values. **DONE**.
4. **Lanes removed**: DepTracker and WorkerPool simplified from lane-based (interactive/bulk/shared) to single flat pool. Single `Ready()` channel. **DONE**.
5. **`Orchestrator.RunWatch(ctx)`**: daemon mode. Starts all drive runners in watch mode. `runSyncWatchBridge` eliminated — watch mode routes through the Orchestrator via `runSyncDaemon`. **DONE**.
6. **PID file**: flock-based single-instance guard in `runSyncDaemon`. **DONE**.
7. **SIGHUP config reload**: re-read config → clear expired timed pauses → diff drives → stop removed → start added. **DONE**.
8. **`--drive` repeatable**: `StringArrayVar` with `SingleDrive()` helper. File-op commands validate `len <= 1`. sync accepts multiple. **DONE**.
9. **Backlog fixes**: B-288 (quiet→cc.Statusf), B-232 (loadConfig error path tests), B-229 (document Changed vs GetBool), B-230 (printNonZero helper), CI dedup. **DONE**.

Acceptance: `transfer_workers` + `check_workers` config respected. Lanes removed. `runSyncWatchBridge` eliminated. SIGHUP reload implemented. `--drive` repeatable. Worker budget algorithm and per-drive allocation deferred to future increment. `DriveSession` eliminated in 6.0e (replaced by `driveops.SessionProvider` + `driveops.Session`).

### 6.0e: `internal/driveops/` package — DONE

Extract authenticated drive access, token caching, and transfer operations into `internal/driveops/` package. See [design/driveops.md](design/driveops.md) for full design.

1. **`driveops.SessionProvider`**: caches TokenSources by token file path. Created once (CLI PersistentPreRunE), shared with Orchestrator. Config accessed via shared `*config.Holder` (RWMutex) — SIGHUP reload updates config in one place.
2. **`driveops.Session`**: replaces `DriveSession`. Wraps Meta + Transfer `*graph.Client` pair with `ResolveItem()`, `ListChildren()`, `CleanRemotePath()`.
3. **Transfer types moved**: `TransferManager`, `SessionStore`, `Downloader`/`Uploader`/`RangeDownloader`/`SessionUploader` interfaces, `SelectHash`, `ComputeQuickXorHash` — all moved from `internal/sync/` to `internal/driveops/`.
4. **CLI migration**: all file-op commands use `cc.Provider.Session(ctx, cc.Cfg)`. `newSyncEngine` stays in root (takes `*driveops.Session`).
5. **Orchestrator migration**: deleted `clientPair`, `getOrCreateClient`, `tokenSourceFn`. Uses `o.cfg.Provider.Session(ctx, rd)`.
6. **Deleted**: `drive_session.go`, `drive_session_test.go`, `newTransferGraphClient()`.

Acceptance: `grep -rn 'NewDriveSession\|type DriveSession' *.go` → 0 hits. `grep -rn 'clientPair\|getOrCreateClient' internal/sync/orchestrator.go` → 0 hits. `TransferManager` and `SessionStore` live in `internal/driveops/`. Root package has one `graph.NewClient` call (in `newGraphClient`, used by auth/drive commands).

### 6.0f: Remove zero-config, extract scanner, daemon E2E — DONE

1. **Remove zero-config runtime path**: Deleted `matchNoDrives` zero-config branch. Config is mandatory for all drive operations (`login` creates it via `EnsureDriveInConfig`). Updated tests and comments. **DONE**.
2. **Remove no-config E2E test mode**: Deleted `e2eMode`, `fileOpModes()`, and 5 no-config CLI helpers from `e2e_test.go`. Refactored 4 parametrized tests to config-only mode. **DONE**.
3. **Extract `scanner.go`**: Split `observer_local.go` (911→313 lines) by extracting scan/walk/hash/filter logic into `scanner.go` (~480 lines). No API change. **DONE**.
4. **Walk error counting**: Added `skippedEntries` atomic counter in `FullScan`/`makeWalkFunc` with summary log after `WalkDir`. **DONE**.
5. **Dead code cleanup**: Moved `itemTypeFromDirEntry` from production code to test file. **DONE**.
6. **B-299 daemon E2E**: New `e2e/sync_watch_e2e_test.go` (build tag `e2e,e2e_full`) with `TestE2E_SyncWatch_BasicRoundTrip` and `TestE2E_SyncWatch_PauseResume`. **DONE**.

Acceptance: `go vet -tags=e2e,e2e_full ./e2e/...` clean. All unit tests pass. Fast E2E tests pass. No per-package coverage regression.

### 6.0h: CI reliability hardening — DONE

1. **Token file integrity enforcement** (B-303): `tokenfile.ValidateMeta()` validates required metadata keys (`drive_id`, `user_id`, `display_name`, `cached_at`) on both write and read paths. `LoadAndValidate()` for strict operational loading. `Save()` rejects incomplete non-nil meta. `TokenSourceFromPath` uses `LoadAndValidate`. `ReadTokenMeta` validates before consuming. Improved `driveops/session.go` error message for missing drive ID. **DONE**.
2. **WAL checkpoint hardening** (B-304): `PRAGMA wal_checkpoint(TRUNCATE)` added to `BaselineManager.Close()`. Ensures all WAL data is flushed to main DB before closing, fixing potential cross-process SQLite visibility issues. **DONE**.
3. **E2E eventual-consistency guard** (B-305): Added `pollCLIWithConfigNotContains` helper. `TestE2E_Sync_EditDeleteConflict` now polls for remote delete propagation before running sync. **DONE**.

Acceptance: All unit tests pass. Lint clean. Coverage ≥77.9%. E2E compile clean.

### 6.0g: Migrate E2E helpers to explicit config + top-ups — DONE

1. **Migrate all E2E callers to explicit `*WithConfig` variants**: Deleted `runCLI`, `runCLIExpectError`, `pollCLIContains` auto-config wrappers from `sync_e2e_test.go`. All ~50 call sites across 5 E2E test files migrated to `runCLIWithConfig`, `runCLIWithConfigExpectError`, `pollCLIWithConfigContains` with explicit `cfgPath`/`env`. `putRemoteFile` and `getRemoteFile` gained `cfgPath string` and `env map[string]string` parameters. **DONE**.
2. **SIGHUP reload E2E test**: `TestE2E_SyncWatch_SIGHUPReload` (build tag `e2e,e2e_full`). Starts daemon with drive1 only, rewrites config to add drive2, sends SIGHUP, verifies drive2 starts syncing. Requires `ONEDRIVE_TEST_DRIVE_2`. Helpers: `pollForDrive2File`, `waitForStderrContains`. **DONE**.
3. **Observer file header comments**: Added file-level doc comments with cross-references to `observer_local.go`, `observer_local_handlers.go`, and `scanner.go`. **DONE**.
4. **Root package coverage**: 21 new test functions across 7 files covering pure-logic, output formatting, and command structure functions. Root package 39.3% → 46.7%, total coverage 76.3% → 77.9%. **DONE**.

Acceptance: Zero `runCLI`/`runCLIExpectError`/`pollCLIContains` references. `go vet -tags=e2e,e2e_full ./e2e/...` clean. All unit tests pass. Total coverage ≥77%.

### 6.0d: inotify + E2E + second test account — DONE

1. **inotify watch limit detection** (Linux only): `inotify_linux.go` reads `/proc/sys/fs/inotify/max_user_watches`. Warns at 80% threshold via `checkInotifyCapacity()`. On ENOSPC: `ErrWatchLimitExhausted` sentinel returned from `addWatchesRecursive()`, engine falls back to `runPeriodicFullScan()` at `poll_interval`. Other drives retain inotify. No-op stubs on macOS (`inotify_other.go`). **DONE**.
2. **Second test account**: Both `personal:testitesti18@outlook.com` and `personal:kikkelimies123@outlook.com` bootstrapped in `.testdata/`. CI updated to download/save both tokens. **DONE**.
3. **Multi-drive E2E tests**: `e2e/orchestrator_e2e_test.go` with build tag `e2e,e2e_full`. 5 scenarios: SimultaneousSync, Status, DriveIsolation, OneDriveFails, SelectiveDrive. Helpers: `writeMultiDriveConfig`, `runCLIWithConfigAllDrives`, `runCLIWithConfigForDrive`, `cleanupRemoteFolderForDrive`. All skip gracefully when `ONEDRIVE_TEST_DRIVE_2` is unset. **DONE**.
4. **CI `ci.yml` update**: E2E job downloads/saves tokens for both `ONEDRIVE_TEST_DRIVE` and `ONEDRIVE_TEST_DRIVE_2` via loop. **DONE**.

Acceptance: all 5 orchestrator E2E tests pass locally. inotify unit tests pass cross-platform. CI pipeline supports dual-token download/save.

### E2E Test Hardening — DONE (B-306)

42 new `e2e_full` tests across 5 new files + 2 modified files, making the E2E suite exhaustive:

1. **`sync_watch_full_test.go`** (11 tests): Daemon watch mode — remote→local, bidirectional, conflict detection, file modification/deletion, folder creation, multiple files, large file (5 MiB), rapid churn, graceful shutdown, timed pause expiry. `startDaemon` helper. **DONE**.
2. **`cli_commands_e2e_test.go`** (13 tests): status (after sync, JSON, paused), pause (duration, indefinite), resume (not-paused, all-drives), conflicts (empty, JSON), resolve (keep-both, multiple strategies, not-found), verify (after sync). **DONE**.
3. **`sync_edge_cases_full_test.go`** (8 tests): Empty dirs, nested deletion, resolve-then-sync (keep-local, keep-remote), .nosync guard, mtime-only change, idempotent re-sync, transfer_workers config. **DONE**.
4. **`sync_recovery_e2e_test.go`** (3 tests): Delta token persistence, crash recovery idempotency, purge resets state. **DONE**.
5. **`output_validation_e2e_test.go`** (4 tests): Verify JSON schema, status with no drives, quiet mode, multi-drive report format. **DONE**.
6. **`orchestrator_e2e_test.go`** (3 new tests): Multi-drive watch simultaneous, drive isolation, paused drive. **DONE**.
7. **`sync_e2e_test.go`** (5 new helpers): `pollLocalFileExists`, `pollLocalFileContent`, `pollLocalDirGone`, `writeSyncConfigWithOptions`, `writeSyncConfigNoDrive`. **DONE**.

Total E2E test count: 86 (44 existing + 42 new).

Acceptance: `go vet -tags=e2e,e2e_full ./e2e/...` clean. `golangci-lint run` 0 issues. All unit tests pass. Fast E2E pass. Coverage ≥77.9%.

### 6.1: Drive removal — DONE (refactored in Phase 5.5)

1. `drive remove <drive>` — **DONE**: deletes config section. State DB and token preserved for fast re-add.
2. `drive remove --purge <drive>` — **DONE**: permanently deletes state DB and removes config section. Token preserved if shared with other drives.
3. Text-level config manipulation — **DONE**: `config/write.go` `DeleteDriveSection()` uses line-based text edits preserving all user comments.

### 6.2a: Status command (basic) — DONE

1. `status` command: show account/drive hierarchy — **DONE**: `status.go` shows account email, display name, org name, token state (valid/expired/missing), per-drive canonical ID, sync dir, and state (ready/paused/no token/needs setup).
2. Support `--json` output — **DONE**: `flagJSON` wired, produces JSON array of account objects.

### 6.2b: Status command (sync state) — DONE

1. Per-drive sync state in `status` output: last sync time, files synced, errors, unresolved conflicts. Read from per-drive state DBs. — **DONE**: `querySyncState()` opens state DB read-only, queries `sync_metadata` table + baseline/conflict counts. `syncStateInfo` struct in `statusDrive`. Migration 00005 adds `sync_metadata` table. Engine writes metadata after each `RunOnce`.
2. Overall health summary: total drives, ready/paused/error counts, aggregate unresolved conflicts. — **DONE**: `computeSummary()` aggregates across all drives. Text output prints summary line. JSON wraps accounts in `statusOutput` with `summary` field.
3. In multi-drive mode: `DriveRunner.lastErr` and failures exposed via `Orchestrator.States()` for live display. — Deferred to Phase 8 (WebSocket/daemon IPC).

Acceptance: status shows per-drive sync state and aggregate health. Also bundled B-284 (structured TOML line model) and B-036 (CLI service layer interfaces).

### 6.Xa: Remote observer symmetric filtering (FC-1) — FUTURE

> **Active bug.** See [filtering-conflicts.md FC-1](design/filtering-conflicts.md#fc-1-remote-observer-has-no-built-in-exclusion-filtering) and B-307.

1. Apply `isAlwaysExcluded()` in `observer_remote.go:classifyItem()` after vault check. Items failing: log at Debug, return nil (skip item).
2. Apply `isValidOneDriveName()` in `observer_remote.go:classifyItem()` after vault check. Same treatment.
3. Unit tests: remote observer rejects `.tmp`, `.partial`, `~$doc`, `desktop.ini`, `CON` items — assert no ChangeEvents emitted.
4. Unit tests: parameterized symmetry test confirming both observers agree on all excluded patterns.
5. E2E test (`e2e_full`): upload `.tmp` via Graph API, sync, assert not downloaded.

Acceptance: `isAlwaysExcluded` and `isValidOneDriveName` called in remote observer. S7 enforced symmetrically.

### 6.Xb: Narrow `.db` exclusion to sync engine database (FC-2) — FUTURE

> **Active bug (false positives).** See [filtering-conflicts.md FC-2](design/filtering-conflicts.md#fc-2-built-in-db-exclusion-is-too-aggressive) and B-308.

1. Remove `.db`, `.db-wal`, `.db-shm` from `alwaysExcludedSuffixes`.
2. Add path-based exclusion for the sync engine's own baseline database file (check against known database path).
3. Unit tests: `test-data.db` is NOT excluded; sync engine's database IS excluded.
4. Unit tests: `.sqlite`/`.sqlite3` files are NOT excluded (documenting the behavior).
5. E2E test (`e2e_full`): create `test-data.db` locally, sync, verify appears on remote.

Acceptance: Legitimate `.db` files sync. Only the sync engine's own database is excluded.

### 6.Xc: Non-empty directory delete — Tier 1 disposable cleanup (FC-12) — FUTURE

> **Active limitation.** See [filtering-conflicts.md FC-12](design/filtering-conflicts.md#fc-12-non-empty-directory-delete) and B-309.

1. Add `isBuiltinDisposable(name)` function covering: `isAlwaysExcluded`, `!isValidOneDriveName`, plus OS junk list (`.DS_Store`, `Thumbs.db`, `._*` prefix, `__MACOSX`).
2. Update `deleteLocalFolder` in `executor_delete.go`: when `os.ReadDir` returns entries, classify each. If all are built-in disposable, remove them and retry rmdir. If any are unknown, fail with file names in error message (upgrade from Debug to Warn).
3. Unit tests: directory with only `.DS_Store` → delete succeeds.
4. Unit tests: directory with only `.tmp` files → delete succeeds.
5. Unit tests: directory with mix of disposable + unknown → delete fails, error names blocking files.
6. Unit tests: directory with `desktop.ini` only → delete succeeds (caught by `!isValidOneDriveName`).
7. E2E test (`e2e_full`): create folder+file on remote, sync, create `.DS_Store` locally, delete folder on remote, sync, assert folder removed.

Acceptance: Non-empty directories with only disposable files are cleaned up. Unknown files block deletion with named warnings.

### 6.3: Shared drive enumeration — FUTURE

1. **sharedWithMe API**: New `graph.Client` method `SharedWithMe(ctx)` returning shared drive items with owner name, email, folder name, permissions.
2. **`drive list` integration**: Show shared-with-me folders alongside personal/business/SharePoint. First 10, then "... and N more".
3. **`drive add` for shared folders**: Substring match against derived display names (`{FirstName}'s {FolderName}` with uniqueness escalation). Construct `shared:email:sourceDriveID:sourceItemID` canonical ID. Auto-fill display_name, owner, sync_dir.
4. **New graph response types**: `sharedFacet`, `sharedOwner`, `sharedUser` for parsing `owner.user.displayName` and `owner.user.email`.

Acceptance: `drive list` shows shared folders. `drive add` creates shared drive config section with correct canonical ID and display name.

### 6.4a: Folder-scoped delta + remoteItem parsing — FUTURE

1. **`graph.Item` new fields**: `RemoteDriveID driveid.ID` (from `remoteItem.parentReference.driveId`), `RemoteItemID string` (from `remoteItem.id`). Populated during delta normalization.
2. **`remoteItemFacet` type**: Unexported struct for JSON parsing. Fields: ID, ParentReference (*parentRef), Folder (*folderFacet). Parsed in `toItem()` when remoteItem != nil.
3. **`graph.DeltaFolder()`**: New method for `/drives/{driveID}/items/{folderID}/delta`. Same normalization pipeline as `Delta()`. New `buildFolderDeltaPath()`.
4. **`graph.DeltaFolderAll()`**: Paginated wrapper like `DeltaAll()`.
5. **`DeltaFetcher` interface extension**: Add `DeltaFolder(ctx, driveID, folderID, token)` method.
6. **RemoteObserver shortcut detection**: During primary FullDelta, detect items where `RemoteItemID != "" && IsFolder` → shortcutRef struct.
7. **Sub-delta orchestration**: For each shortcut, load scope token from `delta_tokens` (`drive_id`, `scope_id=remoteItemID`), call `DeltaFolderAll` on source drive. Non-fatal on failure (other shortcuts sync fine).
8. **Path mapping**: Sub-delta item paths prefixed with shortcut's local position. E.g., shortcut "Family Photos" + item "2024/vacation.jpg" → "Family Photos/2024/vacation.jpg".
9. **Scope token management**: Pending scope tokens committed per-scope after cycle completes. Uses existing composite key schema (migration 00004).

Acceptance: folder-scoped delta returns correct ChangeEvents. Delta tokens saved/loaded per scope. Path mapping produces correct local paths. Shortcut detection identifies remoteItem folders.

### 6.4b: Shared folder sync (shortcuts + lifecycle) — FUTURE

1. **Shortcut lifecycle**: New shortcut → initial sub-delta enumeration + create local dir. Removed shortcut → delete local dir recursively + clean scope token + remove baseline entries (DP-2). Moved shortcut → `os.Rename` local dir + update path prefix.
2. **Cross-drive executor operations**: Sub-delta events have DriveID = source drive. Executor already uses `action.View.Remote.DriveID` for API calls. Same OAuth token grants access to shared content. No executor changes needed for the common case.
3. **Read-only content handling**: Auto-detect via 403 on upload/delete. Summarized errors, not per-file (DP-3). Mark shortcut as read-only in observer state to avoid repeated 403s.
4. **Shared-with-me drives**: Full drive infrastructure for standalone shared folders. `drive add`/`remove` by display_name (DP-4).
5. **Personal and Business account support**: Both account types can have shared content. SharePoint libraries already handled as separate drives.

Acceptance: Shortcuts detected, content synced. Share revocation deletes local copies. Read-only content produces summarized errors. `drive add`/`remove` works for shared drives.

---

## Phase 7: CLI Completeness

**Make every command work properly.** Global flags, recursive operations, and user-facing polish. After this phase, every CLI command specified in the PRD works correctly for single-drive use.

### 7.0: Global output flags

1. `--verbose` / `-v` flag — **DONE**: wired as persistent flag, sets log level to Info.
2. `--debug` flag — **DONE**: wired as persistent flag, sets log level to Debug.
3. `--quiet` / `-q` flag — **DONE**: wired as persistent flag, sets log level to Error. Mutually exclusive with `--verbose` and `--debug`.
4. `--json` flag — **DONE**: wired as persistent flag, used by `ls`, `stat`, `drive list`, `drive remove`, `status`, `verify`, `conflicts`, `whoami`. Not yet wired to `login`, `sync` summary, or `logout`.
5. Refactor output layer — **FUTURE**: replace direct `fmt.Printf` calls with a `CLIOutput` abstraction that respects `--quiet`, `--verbose`, `--json`. All commands use the same output path.

### 7.1: Recursive file operations

1. `ls` pagination — **DONE**: `ListChildren` supports `@odata.nextLink` pagination (200 items per page).
2. Recursive `get <remote-folder> [local]` — **FUTURE**: download an entire remote directory tree. Walk remote children recursively via `ListChildren`, create local directory structure, download each file. Respect `--verbose` for per-file progress.
3. Recursive `put <local-folder> [remote]` — **FUTURE**: upload an entire local directory tree. Walk local filesystem, create remote folders via `CreateFolder`, upload each file. Skip symlinks.
4. Recursive `rm <remote-folder>` — **DONE**: `--recursive` flag implemented (B-156).

### 7.2: Server-side move and copy — FUTURE

1. `mv <source> <destination>`: server-side move/rename via Graph API `MoveItem`. Supports both rename (same parent, new name) and move (different parent). No data transfer — instant for any file size.
2. `cp <source> <destination>`: server-side copy via Graph API copy endpoint (`POST /items/{id}/copy`). Returns `Location` header for async monitoring. Poll until complete for large files.
3. Both commands work on files and folders. Both respect `--drive` flag. Both produce `--json` output when requested.

### 7.3: Auth flow improvements — DONE

1. `login --browser` — **DONE**: authorization code flow with PKCE + localhost callback. Opens system browser, starts local HTTP server on `http://localhost:<port>/callback`. Falls back to device code if browser can't open.
2. `logout --purge` — **DONE**: deletes token file, state DB, and removes drive section from `config.toml`.

### 7.4: Drive selection and account disambiguation

1. `--drive` fuzzy matching — **DONE**: `MatchDrive()` in `config/drive.go` matches by exact canonical ID → exact display_name (case-insensitive) → substring on canonical ID, display_name, or owner. On ambiguity, shows all matching drives sorted.
2. `--account <email>` flag — **DONE**: persistent flag in `root.go`, used for `drive search` and auth commands to restrict operations to a specific account.
3. `--drive` repeatable — **FUTURE**: `sync --drive "me@contoso.com" --drive "me@outlook.com"` syncs only those two drives. Without `--drive`, sync all non-paused drives.

### 7.5: Transfer progress display — FUTURE

1. Interactive progress bars for `get` and `put`: show filename, size, progress percentage, transfer speed. Use a terminal-aware library (e.g., `github.com/vbauerster/mpb`). Disable when stdout is not a TTY or `--quiet` is set.
2. `sync` progress output: show per-file progress during sync transfers. Format: `↓ report.pdf  2.3 MB [======>  ] 62%  3.2 MB/s`. Summary line at completion.
3. Multi-file progress: when multiple transfers are in flight (parallel workers), show concurrent progress lines. Update in place using terminal control sequences.

### 7.6: Structured JSON logging — FUTURE

1. `log_format` config option: `"text"` (default, human-readable) or `"json"` (structured, machine-parseable). JSON format uses `slog.JSONHandler`. Text format uses current `slog.TextHandler`.
2. When `--quiet` is set and `log_format = "json"`, all output goes to the log file in JSON format. No stderr output except fatal errors. This is what you configure for systemd/launchd service mode.
3. Log output includes structured fields: `drive`, `action`, `path`, `size`, `duration`, `error` as applicable. No string interpolation in log messages.

### 7.7: Recycle bin commands — FUTURE

1. `recycle-bin list`: show all items in the OneDrive recycle bin for the selected drive. Display name, original location, size, deletion date. Support `--json` output. Uses `GET /me/drive/items/{item-id}/children` on the recycle bin special folder (or `GET /drives/{drive-id}/special/recyclebin/children`).
2. `recycle-bin empty`: permanently delete all items in the recycle bin. Confirmation prompt. `--force` to skip.
3. `recycle-bin restore <id|path>`: restore a specific item from the recycle bin to its original location. Uses `POST /drives/{drive-id}/items/{item-id}/restore`.

### 7.8: Conflict path filtering — FUTURE

1. `conflicts --path <path>`: filter conflict list to a specific path or subtree. `conflicts --path /Documents` shows only conflicts under `/Documents`.
2. `resolve --path <path>`: batch-resolve conflicts only within a path subtree. Combines with existing `--keep-local`, `--keep-remote`, `--keep-both` flags.

---

## Phase 8: WebSocket + Advanced Sync

**Real-time remote observation and advanced sync features.** After this phase, remote changes arrive in near-real-time instead of polling, and the system handles extreme scale gracefully.

### 8.0: WebSocket remote observer — FUTURE

1. Subscribe to Graph API change notifications via WebSocket: `POST /subscriptions` with `changeType: "updated"` on the drive root. Receive push notifications when remote files change.
2. On notification, trigger immediate delta query (instead of waiting for poll interval). WebSocket is a trigger mechanism, not a data channel — the delta API remains the source of truth.
3. Automatic fallback to polling when WebSocket connection fails (network issues, unsupported account type). Reconnect with exponential backoff.
4. `websocket` config option (default `true`): disable WebSocket and use polling only. Some corporate firewalls block WebSocket connections.

### 8.1: Adaptive concurrency + multi-drive worker budget — FUTURE

1. **Multi-drive worker budget** (B-297): Proportional allocation of `transfer_workers` across active drives by baseline file count. Global cap (default 8), minimum 4 per drive, rebalanced on SIGHUP. Currently each drive gets the full global budget — N drives = N × 8 workers. See MULTIDRIVE.md §11.3 for the allocation algorithm spec.
2. **Watch-mode parallel hashing** (B-298): `hashAndEmit` in watchLoop runs sequentially in the watch goroutine. Needs a hash worker pool for parallelism. FullScan already parallelized (three-phase pattern).
3. **AIMD auto-tuning**: Additive increase, multiplicative decrease for worker count. Monitor 429 response rate and throughput. High 429 rate (>5% of requests): halve active workers (multiplicative decrease). Low error rate + high throughput: add one worker (additive increase).
4. Adapt to workload type: many small files benefit from more workers; few large files benefit from fewer workers with more bandwidth each.
5. Per-tenant coordination: when multiple drives share the same Microsoft tenant, share the AIMD state so one drive's 429s throttle all drives on that tenant.

### 8.2: Observer backpressure — FUTURE

1. High-water mark on `ChangeBuffer` (default 100K paths). When buffer exceeds threshold, pause observers (stop polling delta API, stop processing fsnotify events).
2. Resume observers when buffer drains below low-water mark (e.g., 50K paths). Hysteresis prevents rapid pause/resume oscillation.
3. During pause, fsnotify events queue in the kernel buffer. Safety scan on resume catches anything missed.

### 8.3: Initial sync batching — FUTURE

1. For very large initial syncs (>50K items), process the delta response in batches of 50K items. Plan and execute each batch before loading the next.
2. Reduces peak memory usage: instead of loading 500K change events into memory at once, process 10 batches of 50K.
3. Each batch commits its outcomes to baseline before the next batch loads. Crash recovery picks up from the last committed batch.

### 8.4: Action cancellation — FUTURE

1. When a file changes while it's being uploaded (fsnotify Write event for an in-flight upload path), cancel the current upload via `CancelByPath()` and re-queue the file for a new upload with the updated content.
2. Avoid wasting bandwidth on uploads that will immediately be invalidated. The cancellation triggers context cancellation on the in-flight HTTP request.
3. Upload session cleanup: canceled chunked uploads should cancel the server-side upload session to free resources.

---

## Phase 9: Operational Hardening

**Make it reliable for always-on deployment.** After this phase, the tool can run as a system service for weeks/months without intervention.

### 9.0: Bandwidth limiting — FUTURE

1. `bandwidth_limit` config option: global bandwidth cap in bytes/sec (e.g., `"10MB/s"`, `"0"` for unlimited). Implemented as a token-bucket rate limiter wrapping all HTTP transfer bodies.
2. `bandwidth_schedule` config option: time-of-day rules for variable bandwidth. Format: `[{time = "08:00", limit = "5MB/s"}, {time = "18:00", limit = "50MB/s"}, {time = "23:00", limit = "0"}]`. Evaluated on each transfer start.
3. Bandwidth limit applies to both uploads and downloads combined. Separate per-direction limits are a future enhancement if needed.

### 9.1: Disk space reservation — FUTURE

1. `min_free_space` config option (e.g., `"1GB"`): before starting any download, check available disk space on the target filesystem. Skip the download with a warning if it would leave less than `min_free_space` free.
2. Check is per-file, not aggregate. A 500 MB download is allowed if 2 GB is free and `min_free_space = "1GB"`, even if 50 more downloads are queued.
3. Periodic check during watch mode: if disk fills up from non-sync activity, pause downloads until space is available.

### 9.2: Trash integration — FUTURE

1. `use_recycle_bin` config option (default `true`): when sync deletes a remote file, use the OneDrive recycle bin (not permanent delete). When `false`, call `PermanentDeleteItem` API (Business/SharePoint only; Personal always uses recycle bin).
2. `use_local_trash` config option (default `false`): when sync deletes a local file (because it was deleted remotely), move to OS trash instead of `os.Remove`. macOS: `~/.Trash/`. Linux: FreeDesktop.org Trash spec (`$XDG_DATA_HOME/Trash/`).
3. Fallback: if trash move fails (permissions, cross-device), fall back to `os.Remove` with a warning.

### 9.3: Conflict reminder notifications — FUTURE

1. In `--watch` mode, periodically remind the user about unresolved conflicts. Default interval: every 6 hours. `conflict_reminder_interval` config option (e.g., `"6h"`, `"0"` to disable).
2. Reminder format: structured log message with conflict count and `run 'onedrive-go conflicts' to view` guidance.
3. First reminder fires immediately after a sync cycle that produces new conflicts.

### 9.4: Configurable parallelism — DONE (moved to Phase 6.0c)

Moved to Phase 6.0c as `transfer_workers` (default 8, range 4-64) and `check_workers` (default 4, range 1-16). The old `parallel_downloads`, `parallel_uploads`, `parallel_checkers` keys are deprecated.

### 9.5: Configurable timeouts — FUTURE

1. `connect_timeout` config option (default `"30s"`): TCP connection timeout for Graph API requests. Currently hardcoded in `http.Client`.
2. `data_timeout` config option (default `"5m"`): per-request timeout for data transfer. Applies to download/upload HTTP bodies. Currently uses context timeout.
3. `shutdown_timeout` config option (default `"30s"`): grace period for in-flight transfers when SIGTERM is received. After timeout, force-cancel remaining transfers.

### 9.6: Config validation and log management

1. Unknown config key detection — **DONE**: `checkUnknownKeys()` in `config/unknown.go` validates both global and per-drive keys on startup. Levenshtein-based "did you mean?" suggestions. Unknown keys are fatal errors. Full test coverage.
2. `log_retention_days` config option — **FUTURE**: (default 30) automatically delete log files older than N days. Checked once per day in watch mode, or on each one-shot sync start.

### 9.7: Configurable file permissions — FUTURE

1. `sync_file_permissions` config option (default `"0644"`): file mode for downloaded files after atomic rename. Applied via `os.Chmod` after the `.partial` → final rename.
2. `sync_dir_permissions` config option (default `"0755"`): directory mode for newly created sync directories.
3. Consistent with the fix in B-212 (freshDownload permissions). These config options override the default.

---

## Phase 10: Filtering

**The tool can sync, but can't filter.** After this phase, users can exclude files and directories from sync using config patterns, per-directory marker files, and selective sync paths.

### 10.0: Config-based filtering — FUTURE

> **Prerequisites:** 6.Xa (FC-1: remote observer symmetric filtering) and 6.Xb (FC-2: narrow `.db` exclusion) must be completed first.

1. `skip_files` config option: glob patterns for files to exclude from sync. Example: `["*.log", "*.pyc", "*.o"]`. Note: `*.tmp`, `*.partial`, `~*`, `.DS_Store`, `Thumbs.db` are already in the built-in exclusion list (Layer 0) and don't need to be in `skip_files`. See [filtering-conflicts.md FC-5](design/filtering-conflicts.md#fc-5-built-in-exclusions-vs-future-skip_files-overlap). Matched against filename only (not full path). Applied in the planner (Layer 2).
2. `skip_dirs` config option: glob patterns for directories to exclude. Example: `["node_modules", ".git", "__pycache__"]`. When a directory matches, skip it and all its contents. Applied in the planner (Layer 2).
3. `skip_dotfiles` config option (default `false`): when `true`, exclude all files and directories starting with `.`, **except** the configured `ignore_marker` filename (default `.odignore`). The `ignore_marker` exemption prevents Layer 2 from killing `.odignore` before Layer 3 processes it. See [filtering-conflicts.md FC-6](design/filtering-conflicts.md#fc-6-odignore-killed-by-skip_dotfiles).
4. `max_file_size` config option (e.g., `"50GB"`): skip files larger than N bytes. Log a warning for each skipped file. Checked in both upload and download paths.
5. Per-drive overrides: each drive section can override filter settings. Drive-level `skip_dirs` replaces (not merges with) other drive settings.
6. Filter evaluation as a `Filter` type consumed by the planner. Built-in exclusions (Layer 0) remain in observers.
7. **Stale file handling** (FC-9): When a filter change excludes a previously-synced file (baseline entry exists), log a warning and freeze the baseline entry. No sync operations generated for stale files — no downloads, no uploads, no deletes. Optional `stale_action = "untrack"` to remove from baseline. See [filtering-conflicts.md FC-9](design/filtering-conflicts.md#fc-9-stale-files-after-filter-changes).
8. Log effective built-in exclusions at startup so users see what's always excluded (FC-5).
9. Test: removing a `skip_files` pattern for a built-in-excluded file does NOT cause it to sync.

### 10.1: Per-directory ignore files — FUTURE

1. `.odignore` marker file support: drop a file named `.odignore` (configurable via `ignore_marker` config key) in any directory to control exclusion.
2. Empty `.odignore` or `.odignore` containing `*`: exclude the entire directory and all contents from sync.
3. `.odignore` with patterns: gitignore-style pattern matching within that directory. Supports `*.log`, `build/`, `!important.log` (negation). Patterns apply to the directory containing the marker file and its descendants.
4. **`.odignore` never synced** (FC-7): The configured `ignore_marker` value is added to the built-in exclusion list (Layer 0) and filtered in both observers. Ignore rules are per-device. See [filtering-conflicts.md FC-7](design/filtering-conflicts.md#fc-7-odignore-files--sync-or-not).
5. Changes to `.odignore` take effect on the next sync cycle in watch mode.
6. Test: `.odignore` not synced locally→remote. Test: `.odignore` not downloaded remote→local.

### 10.2: Selective sync paths — FUTURE

1. `sync_paths` per-drive config option: list of remote paths to sync. Example: `["/Documents", "/Photos/Camera Roll", "/Work"]`. Only these paths and their children are synced. Everything else is ignored.
2. When `sync_paths` is set, the local sync directory mirrors only the specified subtrees. Remote changes outside `sync_paths` are ignored in delta processing.
3. `sync_paths` interacts with `skip_dirs`/`skip_files`: both filters apply. A file must be within a `sync_path` AND not match any skip pattern to be synced.
4. **Lightweight baseline entries for traversal parents** (FC-10): Parent directories of `sync_paths` entries get baseline entries with a `sync_paths_traversal` flag. The planner processes renames and deletes for these parents but doesn't sync their direct children unless they're within a sync_path. See [filtering-conflicts.md FC-10](design/filtering-conflicts.md#fc-10-sync_paths-parent-traversal-gaps).
5. Test: parent of `sync_path` renamed remotely → local structure updated correctly.

### 10.3: Symlink handling — FUTURE

Industry context: the official OneDrive client follows symlinks (syncs target content). rclone and Resilio skip by default. Dropbox removed symlink-following after infinite sync loops in production. Syncthing refuses to follow (security). The abraunegg Linux OneDrive client follows by default with a `skip_symlinks` option. We match the official client's behavior with safety guards.

1. Default behavior: follow symlinks during local scan. Sync the target file/directory as if it were a regular file/directory. OneDrive has no concept of symlinks.
2. `skip_symlinks` config option (default `false`): when `true`, skip all symlinks silently during local scan.
3. Circular symlink detection: track visited inodes (`os.Stat` → `sys.Ino`) during directory walk. If a symlink points to an ancestor directory or creates a cycle, skip it with `slog.Warn`. Prevent infinite recursion. Do not rely solely on OS ELOOP limits (rclone's known bug).
4. Broken symlink handling: if a symlink target does not exist (`os.Stat` returns `os.ErrNotExist`), skip the symlink with `slog.Warn`. Do not crash or propagate the error (abraunegg crash bug precedent).
5. Cross-device symlinks: symlinks pointing outside the sync directory are followed. The target content is synced under the symlink's path within the sync tree.
6. Watch mode: `fsnotify` does not deliver events for changes inside symlinked directories. After following a symlinked directory, add an explicit watch on the resolved target path. Log a warning if the watch cannot be added.
7. Resolves B-120 (symlinked directories get no watch and no warning).

### 10.4: Application-specific exclusions — FUTURE

1. OneNote auto-exclusion: automatically exclude `.one` and `.onetoc2` files from sync. OneNote files can only be edited through the OneNote application and synced through its own mechanism. Syncing them causes corruption. Always excluded regardless of config.
2. SharePoint enrichment known-type list: maintain a list of file types that SharePoint modifies server-side after upload (PDF metadata, Office document properties, HTML). When a post-upload hash mismatch occurs for these types, accept the server version without flagging a conflict. See [design/sharepoint-enrichment.md](design/sharepoint-enrichment.md).

### 10.5: OS junk handling + perishable files — FUTURE

> **Rewritten from "auto-clean junk" to "built-in junk exclusion + perishable files."** The original auto-clean design would cause infinite deletion loops in cross-platform shared folders (macOS recreates `.DS_Store` after every Finder visit). See [filtering-conflicts.md FC-8](design/filtering-conflicts.md#fc-8-auto_clean_junk-deletion-wars).

1. **Built-in junk exclusion** (core junk files already in Layer 0 from 6.Xc): `.DS_Store`, `Thumbs.db`, `._*` (macOS resource forks), `__MACOSX/` directories. Note: `desktop.ini` is already covered by `isValidOneDriveName` (FC-3). These files are never synced in either direction.
2. **`perishable_files` config option** (FC-12 Tier 2): per-drive list of glob patterns for files that can be cleaned during directory deletion. Example: `perishable_files = ["*.pyc", "*.o", "__pycache__/", "node_modules/"]`. See [filtering-conflicts.md FC-12](design/filtering-conflicts.md#fc-12-non-empty-directory-delete).
3. **Optional CLI command**: `onedrive-go clean-remote-junk` for one-time removal of junk files that were uploaded to remote by other clients. Not automated — manual, user-initiated. Avoids deletion wars.

---

## Phase 11: Packaging + Release

**Ship it.** After this phase, users can install onedrive-go via their platform's package manager and run it as a system service.

### 11.0: goreleaser — FUTURE

1. Configure goreleaser for automated binary builds: Linux (amd64, arm64), macOS (amd64, arm64).
2. Static Go binaries with no runtime dependencies. Single file, copy-and-run.
3. GitHub Releases: automatic release creation on tag push. Checksums file, changelog from commits.
4. CI integration: goreleaser runs in GitHub Actions on version tags (`v*`).

### 11.1: Homebrew + AUR — FUTURE

1. Homebrew tap: create `homebrew-onedrive-go` tap repository with a formula. `brew install tonimelisma/onedrive-go/onedrive-go`. Auto-updated on release via goreleaser.
2. AUR PKGBUILD: Arch Linux user repository package. Build from source (Go required) or download pre-built binary.

### 11.2: .deb + .rpm packages — FUTURE

1. Debian/Ubuntu `.deb` package via goreleaser nfpm integration. Install with `dpkg -i` or from a PPA.
2. Fedora/RHEL `.rpm` package via goreleaser nfpm integration. Install with `rpm -i` or from a COPR repository.
3. Both packages include systemd unit file, man page, and default config directory setup.

### 11.3: Docker image — FUTURE

1. Alpine-based multi-arch Docker image (amd64, arm64). Minimal footprint.
2. Config and data as volumes: `-v ~/.config/onedrive-go:/config -v ~/OneDrive:/data`.
3. Default entrypoint: `onedrive-go sync --watch --quiet`.
4. Published to Docker Hub and GitHub Container Registry.

### 11.4: Service management — FUTURE

1. `service install`: generate and install the appropriate service file for the current platform. Linux: systemd user unit (`~/.config/systemd/user/onedrive-go.service`). macOS: launchd plist (`~/Library/LaunchAgents/com.tonimelisma.onedrive-go.plist`).
2. `service uninstall`: remove the installed service file. Does not delete data or config.
3. `service status`: show whether the service file is installed, whether the service is enabled, and whether it's currently running. Print native commands for enable/disable/start/stop.
4. `service install` never auto-enables. It generates the file and prints instructions. The user decides when to enable.

### 11.5: Man page + README — FUTURE

1. Man page generation from Cobra command tree. `onedrive-go(1)` with all commands, flags, and examples. Installed by .deb/.rpm packages.
2. README update: installation instructions for all package managers, quick start guide, feature overview, comparison with alternatives.

---

## Phase 12: Post-Release

**Interactive CLI, advanced features, and polish.** Added based on user demand after the initial release.

### 12.0: Setup wizard — FUTURE

1. `setup` command: interactive menu-driven configuration. Covers: viewing drives/settings, changing sync directories, configuring exclusions, setting sync interval, log level, per-drive overrides, and aliases.
2. Everything `setup` does can also be done by editing `config.toml` directly. `setup` is for users who prefer guided configuration.
3. Text-level config manipulation: edits preserve all user comments in `config.toml`.

### 12.1: Migration tool — FUTURE

1. `migrate` command: auto-detect and import configuration from abraunegg/onedrive or rclone.
2. abraunegg migration: map `sync_dir`, `skip_dir`/`skip_file`, `skip_dotfiles`, `rate_limit` → `bandwidth_limit`, `threads` → `parallel_downloads`/`parallel_uploads`, `monitor_interval` → `poll_interval`, `sync_list` → `sync_paths`, `classify_as_big_delete` → `big_delete_threshold`.
3. rclone migration: map remote name → drive display_name, `drive_id` → drive section, `drive_type` → auto-detected. Token NOT migrated (different OAuth app ID).
4. Detect if abraunegg or rclone is currently running/configured and warn about conflicts.

### 12.2: Interactive conflict resolution — FUTURE

1. `resolve` with no batch flags enters interactive mode. Prompts per conflict: `[L]ocal / [R]emote / [B]oth / [S]kip / [Q]uit`. Shows diff information (sizes, dates, hashes) for each conflict.
2. Interactive mode is the default. Batch flags (`--keep-local`, `--keep-remote`, `--keep-both`, `--all`) bypass interactive mode.

### 12.3: Interactive drive add

1. `drive add` interactive flow — **FUTURE**: enumerate available SharePoint libraries and shared folders. Present a numbered list. User selects by number. Auto-configure sync directory with collision handling.
2. Non-interactive `drive add` — **DONE**: `drive add <canonical-id>` adds a new drive with a fresh config section. If state DB exists from a prior removal, sync resumes from last delta token. Without arguments, lists available drives. **Not yet done**: `--site`/`--library` shorthand flags.

### 12.4: SharePoint site search — DONE

1. `drive search <term>` — **DONE**: searches SharePoint sites by name via `SearchSites()`. Displays matching sites with document libraries and canonical IDs. Supports `--json` output and `--account` filter. Cap of 50 results per search.

### 12.5: Share command — FUTURE

1. `share <path>`: generate a shareable link for a remote file or folder. Options: `--type view` (read-only, default), `--type edit` (read-write), `--expiry 7d` (link expiration).
2. Uses Graph API `POST /drives/{drive-id}/items/{item-id}/createLink`.

### 12.6: Daemon observability — FUTURE

> **Design doc**: [observability.md](design/observability.md). Covers metrics
> registry, Unix socket transport, Prometheus exposition, and status command
> integration.

**Layer 1: Metrics registry** (no transport)

1. `MetricsRegistry` struct in `internal/sync/` with `sync/atomic` fields. Per-drive `DriveMetrics` sub-struct. `Snapshot()` returns a plain struct for JSON.
2. Instrument `Engine.RunOnce()` (phase enum, cycle accumulation), `WorkerPool` (busy/total gauge), `graph.Client.do()` (request/retry/429 counters), upload/download paths (bytes transferred).
3. Collect `runtime/metrics` (goroutines, heap, GC) in snapshot.

**Layer 2: Unix domain socket**

4. `sync --watch` opens `$XDG_RUNTIME_DIR/onedrive-go.sock` (Linux) or `/tmp/onedrive-go-$(id -u).sock` (macOS). JSON-over-HTTP-over-UDS. Same pattern as Docker, gopls, Syncthing.
5. `GET /status` — full snapshot (application + runtime metrics). `GET /health` — liveness probe. `GET /metrics` — hand-written Prometheus exposition format (no `prometheus/client_golang` dependency).
6. Socket lifecycle: create on daemon start, remove on shutdown, stale cleanup on startup.

**Layer 3: Status command integration**

7. `status` tries socket first for live data, falls back to state DB (existing behavior). `--json` gains `daemon` key. Daemon-running detection via socket connectivity.

### 12.7: RPC-based live sync trigger — FUTURE

> Builds on 12.6 socket. Pause/resume stays config-as-IPC (Phase 5.5/7.0).

1. `sync` while `--watch` is running: delegate to the running daemon via RPC to trigger an immediate sync cycle instead of failing with "database is locked".
2. `POST /sync` triggers an immediate delta check for all drives (or `POST /sync?drive=X` for a specific drive) without waiting for `poll_interval`.
3. `GET /events` — SSE (Server-Sent Events) stream for real-time push (transfer progress, sync complete, conflict detected). For TUI and GUI clients.

### 12.8: TUI interface — FUTURE

1. Interactive terminal UI (like lazygit/lazydocker): real-time sync status across all drives, transfer progress bars, conflict resolution interface, log viewer.
2. Built on a TUI framework (e.g., `github.com/charmbracelet/bubbletea`). Connects to the 12.6 socket for real-time updates via 12.7 SSE stream.

### 12.9: Prometheus metrics — FUTURE (optional)

> The 12.6 socket already serves `/metrics` in Prometheus text format. This
> increment adds an optional TCP HTTP listener for standard Prometheus scraping.

1. `metrics_listen` config option (e.g., `"localhost:9182"`). Disabled by default.
2. Same hand-written exposition format as the socket `/metrics` endpoint. No `prometheus/client_golang` dependency.

### 12.10: FUSE mount — FUTURE

1. Read-only FUSE mount: `onedrive-go mount <mountpoint>`. Browse OneDrive as a local filesystem. Files downloaded on demand (lazy fetch). Directory listing via `ListChildren` API.
2. Read-write FUSE mount (later): writes create local cache files that are uploaded asynchronously. Conflict detection on write.
3. On-demand files: placeholder stubs that fetch content on first read. Saves disk space for large drives.

### 12.11: National cloud support — FUTURE

1. Support Microsoft national cloud deployments: US Government (GCC, GCC High), Germany, China (21Vianet). Different Graph API endpoints and auth endpoints.
2. `cloud` config option: `"global"` (default), `"us_gov"`, `"us_gov_high"`, `"germany"`, `"china"`.
3. `/children` fallback: national clouds may not support the delta API. Implement full `/children` traversal as a fallback for change detection. Slower but functional.

### 12.12: Desktop integration — FUTURE

1. Desktop notifications: notify on sync completion, new conflicts, and errors. Linux: libnotify (`notify-send`). macOS: Notification Center via `osascript`.
2. File manager integration: Nautilus/Dolphin emblems for sync status (synced, syncing, conflict, error). macOS Finder badges via extension.

### 12.13: Advanced sync features — FUTURE

1. Email change detection: Microsoft accounts can change their email address. Detect via stable user GUID (immutable). Auto-rename token files, state DBs, and config sections when email changes.
2. Sub-drive sync paths: sync a remote subfolder instead of the entire drive root. `sync_root = "/Documents/Work"` in drive config. Only that subtree is synced.
3. Case-insensitive collision detection on Linux: two local files differing only in case (e.g., `README.md` and `Readme.md`) create a conflict on OneDrive (case-insensitive). Detect and warn before upload.

### 12.14: Testing and benchmarks — FUTURE

1. Like-for-like performance benchmarks against rclone and abraunegg/onedrive. Reproducible benchmark suite: initial sync time, incremental sync time, memory usage, CPU usage at idle.
2. Property-based tests using `pgregory.net/rapid` for planner invariants (decision matrix completeness, safety guard correctness).
3. Fuzz targets (`go test -fuzz`) for hash algorithm, filter pattern parsing, TOML config parsing.
4. Chaos tests (fault injection): network partitions, disk full, permission errors mid-sync. Tagged `chaos` build tag.

---

## Summary

| Phase | Increments | Focus | Status |
|-------|-----------|-------|--------|
| 1 | 8 | Graph API client + auth + CLI basics | **COMPLETE** |
| 2 | 3 | E2E CI against real OneDrive | **COMPLETE** |
| 3 | 3 | Config (TOML, drives, CLI integration) | **COMPLETE** |
| 3.5 | 2 | Account/drive system alignment | **COMPLETE** |
| 4 v1 | 11 | Batch-pipeline sync engine | **SUPERSEDED** |
| 4 v2 | 9 | Event-driven sync engine | **COMPLETE** |
| 5 | 8 | Concurrent execution + watch mode | **COMPLETE** |
| 6 | 10 | Multi-drive orchestration + CLI | IN PROGRESS (6.0a-e, 6.1, 6.2a-b done; 6.3, 6.4 future) |
| 7 | 5 | Multi-drive + account management | IN PROGRESS (7.1 done; 7.2, 7.3 have done items) |
| 8 | 5 | WebSocket + advanced sync | FUTURE |
| 9 | 8 | Operational hardening | IN PROGRESS (9.6 item 1 done) |
| 10 | 6 | Filtering | FUTURE |
| 11 | 6 | Packaging + release | FUTURE |
| 12 | 15 | Post-release | IN PROGRESS (12.4 done; 12.3 item 2 done) |
| **Total** | **98** | | |

Each increment is independently testable and completable in one focused session. Hardening backlog items (defensive coding, test gaps, documentation) are tracked separately in BACKLOG.md and addressed alongside feature work.
