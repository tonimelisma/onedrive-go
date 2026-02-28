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
- SQLite baseline schema via goose migrations: 7 tables (baseline, delta_tokens, conflicts, stale_files, upload_sessions, change_journal, config_snapshots) + 4 indexes
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
| 5.4 | Universal transfer resume + hardening | 2: Operational Polish |
| 5.5 | Pause/resume + SIGHUP config reload + final cleanup | 2: Operational Polish |

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

#### 5.5: Pause/resume + SIGHUP config reload + final cleanup

**Goal**: Complete Phase 5 feature set. Ensure clean slate.

**1. New Code:**
- `engine.go`: `Pause()` / `Resume()` — pause workers, continue collecting events, resume drains buffer
- SIGHUP handler: reload config

**2. Code Retirement:**
- Final sweep — run ALL grep patterns from [`docs/design/legacy-sequential-architecture.md`](design/legacy-sequential-architecture.md) §9
- Doc comment audit: no production `.go` file should reference "9 phases", "9 slices", "sequential execution", or "batch commit" except in historical/explanatory context.

**3. CI and Testing:**
- Pause/resume test, SIGHUP test
- Docs updated: CLAUDE.md (current phase → Phase 6), BACKLOG.md (close items), LEARNINGS.md
- Both CI workflows green. Full DOD checklist.

---

## Phase 6: Packaging + Release — FUTURE

| Increment | Description |
|-----------|-------------|
| 6.1 | CI: full pipeline (unit + integration + E2E + coverage enforcement) |
| 6.2 | goreleaser: Linux + macOS binaries (amd64 + arm64) |
| 6.3 | Homebrew tap formula + AUR PKGBUILD |
| 6.4 | Man page generation from Cobra + README update |
| 6.5 | `service install/uninstall/status` — systemd/launchd service management |

---

## Future (Post-v1)

- WebSocket remote observer (replace polling with Graph API change notifications)
- Parallel initial enumeration (`/children` enumeration for drives without delta support)
- Change journal (append-only debugging aid — optional, not the source of truth)
- Setup wizard (`setup` command for interactive configuration)
- `migrate` command (abraunegg/rclone config migration)
- Email change detection (stable user GUID, auto-rename files)
- FUSE filesystem mount
- National cloud support (US Gov, Germany, China — `/children` fallback for missing delta API)
- Sub-drive sync paths (sync a subfolder instead of the entire drive)
- Performance profiling and optimization (CPU, memory, I/O hotspots)
- Like-for-like performance benchmarks against rclone and abraunegg/onedrive
- Case-insensitive collision detection on Linux (two local files differing only in case)
- **Sync package decomposition (Option B)** — extract `model/` + `logic/` sub-packages as the sync package grows
- **Filter engine pre-filter** — shared pre-filter at observer outputs for skip_files/skip_dirs/etc.
- **Performance**: batched SQLite commits (B-172, deferred until profiling shows bottleneck), concurrent folder creates via $batch API (B-173), streaming delta processing (B-171), adaptive concurrency/AIMD (B-174)

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
| 5 | 7 | Concurrent execution + watch mode | FUTURE |
| 6 | 5 | Packaging + release | FUTURE |
| **Total** | **48** | | |

Each increment: independently testable, completable in one focused session. Phase 4 v2 count (9, including Increment 0) replaces the original Phase 4 v1 count (11). Phase 5 count (7, increments 5.0-5.4 with 5.2 split into 5.2.0/5.2.1/5.2.2) reflects the decision to ship parallel observation and parallel hashing as independent increments before the full RunWatch pipeline.

**Key architectural difference**: Phase 4 v1 used the database as the coordination mechanism (scanner and delta write eagerly, reconciler reads). Phase 4 v2 uses typed events as the coordination mechanism (observers produce events, planner operates as a pure function, executor produces outcomes, baseline manager commits atomically). Phase 5 replaces the sequential executor with a DAG-based concurrent execution model (in-memory dependency tracker + lane-based workers + per-action commits). Same decision matrix, same safety invariants, fundamentally different execution model.
