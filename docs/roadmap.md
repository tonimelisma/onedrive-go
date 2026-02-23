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

## Phase 4 v2: Event-Driven Sync Engine — CURRENT

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

### 4v2.8: CLI Integration + Sync E2E

**Prove the sync engine works end-to-end and add remaining CLI commands.**

- CLI `sync` wiring done in 4v2.7; this increment adds E2E tests and remaining commands
- E2E test updates: write new sync E2E tests against new engine, re-enable sync tests in CI (`integration.yml`)
- `conflicts` command: list unresolved conflicts from baseline (table or JSON)
- `resolve` command: interactive conflict resolution (keep local, keep remote, keep both)
- `verify` command: full-tree hash verification (compare local vs remote vs baseline)
- All commands support `--drive` and `--json` flags
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: sync E2E tests pass against live OneDrive, CI green.
- **Inputs**: [prd.md](design/prd.md) section 4, [event-driven-rationale.md](design/event-driven-rationale.md) Part 10 (Phase 5)

### Wave Structure

**Wave 0**: 4v2.0 (clean slate) — prerequisite for everything. Delete old code, create stubs.

**Wave 1**: 4v2.1 (types + baseline) — foundation types that everything depends on.

**Wave 2**: 4v2.2 (remote observer) + 4v2.3 (local observer) — DONE. Independent of each other, both depend on types from 4v2.1.

**Wave 3**: 4v2.4 (change buffer) + 4v2.5 (planner) — DONE. Implemented in parallel (zero file conflicts). Buffer groups events by path; Planner converts events + baseline into ActionPlan.

**Wave 4**: 4v2.6 (executor) — DONE. Depends on planner output (action plan).

**Wave 5**: 4v2.7 (engine wiring) + 4v2.8 (CLI + sync E2E) — sequential, wires everything together.

---

## Phase 5: Watch Mode + Polish — FUTURE

| Increment | Description |
|-----------|-------------|
| 5.1 | `RemoteObserver.Watch()` — polling-based (delta every N seconds) |
| 5.2 | `LocalObserver.Watch()` — rjeczalik/notify for cross-platform FS events |
| 5.3 | `Engine.RunWatch()` — event loop with change buffer |
| 5.4 | Pause/resume — buffer accumulates while executor is paused |
| 5.5 | SIGHUP config reload + stale file detection |
| 5.6 | Graceful shutdown — two-signal protocol (first: drain + checkpoint, second: immediate exit) |

### 5.1: RemoteObserver.Watch()

- Polling-based delta fetch at configurable interval (default 5 minutes)
- Produces `ChangeEvent` stream into change buffer
- Automatic reconnection with exponential backoff on errors
- WebSocket upgrade deferred to post-v1 (requires Graph API subscription management)
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: integration test — start watch, create remote file, verify event produced.

### 5.2: LocalObserver.Watch()

- rjeczalik/notify integration for cross-platform FS events (inotify on Linux, FSEvents on macOS)
- Filter: ignore `.partial` files, temp files, OS metadata
- Produces `ChangeEvent` stream into change buffer
- Network filesystem detection: fall back to periodic full scan if inotify unreliable
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: test with real temp dir — create/modify/delete files, verify events produced.

### 5.3: Engine.RunWatch()

- Event loop: wait for change buffer flush, run planner + executor, commit baseline
- Same code path as RunOnce (one-shot = "observe everything, flush immediately"; watch = "observe incrementally, flush on debounce")
- Periodic full scan (configurable, default every 12 hours) as safety net
- Time-of-day bandwidth schedule: adjust rate limiter based on config and wall-clock time
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: integration test — start watch, create file, verify sync cycle triggered.

### 5.4: Pause/Resume

- `Engine.Pause()`: stop executor, continue accumulating events in buffer
- `Engine.Resume()`: flush buffer, process accumulated events
- RPC via Unix domain socket for runtime control
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: test — pause, create files, resume, verify all changes synced.

### 5.5: SIGHUP Config Reload + Stale File Detection

- SIGHUP handler: re-read config file, apply changes to running engine
- Detect stale `.partial` files from interrupted previous runs, clean up
- Filter changes applied immediately (hot reload without restart)
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: test — change config, send SIGHUP, verify new settings active.

### 5.6: Graceful Shutdown

- Two-signal protocol: first SIGINT/SIGTERM drains current batch and commits checkpoint; second signal exits immediately
- WAL mode ensures SQLite consistency even on immediate exit
- **Acceptance**: All DOD gates (CLAUDE.md §Quality Gates). Additionally: test — start sync, send signal mid-cycle, verify clean shutdown and baseline consistency.

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

---

## Summary

| Phase | Increments | Focus | Status |
|-------|-----------|-------|--------|
| 1 | 8 | Graph API client + auth + CLI basics | **COMPLETE** |
| 2 | 3 | E2E CI against real OneDrive | **COMPLETE** |
| 3 | 3 | Config (TOML, drives, CLI integration) | **COMPLETE** |
| 3.5 | 2 | Account/drive system alignment | **COMPLETE** |
| 4 v1 | 11 | Batch-pipeline sync engine | **SUPERSEDED** |
| 4 v2 | 9 | Event-driven sync engine | **CURRENT** |
| 5 | 6 | Watch mode + polish | FUTURE |
| 6 | 5 | Packaging + release | FUTURE |
| **Total** | **47** | | |

Each increment: independently testable, completable in one focused session. Phase 4 v2 count (9, including Increment 0) replaces the original Phase 4 v1 count (11).

**Key architectural difference**: Phase 4 v1 used the database as the coordination mechanism (scanner and delta write eagerly, reconciler reads). Phase 4 v2 uses typed events as the coordination mechanism (observers produce events, planner operates as a pure function, executor produces outcomes, baseline manager commits atomically). Same decision matrix, same safety invariants, fundamentally different data flow.
