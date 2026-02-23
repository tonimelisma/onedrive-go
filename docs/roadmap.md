# onedrive-go — Implementation Roadmap

> **ZERO PATH DEPENDENCY**: This document describes the Option E event-driven
> architecture, designed from first principles. The system is NOT an evolution
> of the prior batch-pipeline sync engine. Existing code is reused only where
> it is an excellent match for the new design. See
> [design/event-driven-rationale.md](design/event-driven-rationale.md) for the full rationale.

## Principles

- Each increment is completable by a single agent in one session
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

### 4v2.2: Remote Observer

**Produce typed `ChangeEvent` values from Graph API delta responses.**

- `RemoteObserver.FullDelta(ctx, driveID, deltaToken) -> ([]ChangeEvent, newDeltaToken, error)`
- `convertToChangeEvent`: graph.Item -> ChangeEvent with all normalization (driveID lowering, URL-decode, Prefer header, dedup, reorder deletions)
- Path materialization: build full paths from delta items using baseline for known parents + in-flight parent map for new parents in current delta
- Move detection at observation level: delta items with `parentReference` changes produce `ChangeMoved` events with both old path (from baseline) and new path
- No DB access — pure transformation from API response to typed events
- Tests with mock graph client returning multi-page deltas, edge cases (resync/410, cross-drive parents, deleted items without paths)
- **Acceptance**: `go test ./internal/sync/...` passes, observer produces correct events for all delta scenarios
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.1, 10 (Phase 2)

### 4v2.3: Local Observer

**Produce typed `ChangeEvent` values from local filesystem state.**

- `LocalObserver.FullScan(ctx, syncDir, baseline) -> ([]ChangeEvent, error)`
- Walk filesystem tree, compare against baseline to classify: new (no baseline entry), modified (mtime or hash differs), deleted (baseline entry exists, file gone), unchanged (skip)
- NFC/NFD dual-path threading: store both normalized and raw paths for macOS compatibility
- `.nosync` guard: detect `.nosync` file in sync dir, abort scan with clear error (S2 protection)
- Symlink handling: skip symlinks with warning (OneDrive does not support symlinks)
- OneDrive name validation: reject illegal characters, reserved names, trailing dots/spaces
- Racily-clean mtime guard: if mtime is within 1 second of scan time, force hash recomputation (file may still be being written)
- No DB access — compares against in-memory baseline snapshot
- Tests with real temp dirs, all edge cases (dotfiles, symlinks, racily-clean, Unicode)
- **Acceptance**: `go test ./internal/sync/...` passes, observer produces correct events for all local scenarios
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.2, 10 (Phase 2)

### 4v2.4: Change Buffer

**Debounce, dedup, and batch events for the planner.**

- `ChangeBuffer` struct with configurable debounce window (default 2 seconds)
- `Add(event)`: accumulate events, dedup by path (keep latest per path per side)
- Move event dual-keying: a move produces events at both old path (synthetic delete) and new path (create/modify), ensuring the planner sees both sides
- `Flush() -> []ChangeEvent`: return deduplicated, batched events after debounce window expires
- `FlushImmediate() -> []ChangeEvent`: bypass debounce for one-shot mode (RunOnce collects all events, then flushes immediately)
- Backpressure: if executor is still running, buffer continues accumulating (for watch mode)
- Tests for debounce timing, dedup correctness, move dual-keying, flush modes
- **Acceptance**: `go test ./internal/sync/...` passes
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.3, 10 (Phase 2)

### 4v2.5: Planner

**Pure-function reconciliation: events + baseline -> action plan.**

- `buildPathViews(events, baseline) -> []PathView`: merge change events with baseline entries to build the three-way view (remote state, local state, baseline) for each affected path
- `classifyFile(view PathView) -> Action`: implement all 14 file decision rows (EF1-EF14) from the reorganized decision matrix
- `classifyFolder(view PathView) -> Action`: implement all 8 folder decision rows (ED1-ED8) with existence-based reconciliation (no hash check for folders)
- `detectMoves(views []PathView, baseline) -> []PathView`: remote moves from `ChangeMoved` events; local moves via hash correlation (file disappears at path A, appears at path B with same hash)
- Filter application: `Filter.ShouldSync()` called during classification, not during observation — ensures symmetric filtering of both remote and local items
- Safety checks as pure functions: `bigDeleteTriggered(plan, baseline, config) bool` for S5, baseline-nil checks for S1, all operating on typed data structures instead of DB queries
- Action ordering: folders before files for creates, files before folders for deletes, moves before creates
- Exhaustive table-driven tests: one test case per matrix cell (14 file + 8 folder = 22 minimum), plus move detection, filter symmetry, safety check edge cases
- **Acceptance**: `go test ./internal/sync/...` passes with 100% decision matrix coverage
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 6, 7, 10 (Phase 3), [sync-algorithm.md](design/sync-algorithm.md) section 5

### 4v2.6: Executor

**Execute actions, produce outcomes — no DB writes.**

- Executor returns `[]Outcome` for each completed action
- Each action execution returns an `Outcome` (action type, path, success/failure, resulting hashes, error if any)
- Transfer pipeline produces outcomes via callback: download/upload workers call `func(Outcome)` on completion
- Worker pools reused from 4.9 transfer pipeline (errgroup, bandwidth limiting)
- Download: `.partial` + QuickXorHash verify + atomic rename + mtime restore (S3)
- Upload: SimpleUpload (<4 MiB) or chunked session with hash verification
- Local delete: S4 hash-before-delete guard using `action.View.Baseline.LocalHash`
- Remote delete: 404 treated as success
- Conflict resolution: keep-both with timestamped conflict copies (reused from 4.8)
- Retry logic inside executor: exponential backoff before producing final outcome (fixes B-048)
- Error classification: fatal vs retryable vs skip (reused from 4.7)
- Executor operates on `PathView` context — no database queries
- Tests with mock graph client, real filesystem for downloads, all error paths
- **Acceptance**: `go test ./internal/sync/...` passes
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 5.5, 10 (Phase 4)

### 4v2.7: Engine Wiring + RunOnce

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
- **Acceptance**: `go test ./internal/sync/...` passes, integration test verifies baseline state after RunOnce
- **Inputs**: [event-driven-rationale.md](design/event-driven-rationale.md) Parts 2, 10 (Phase 5)

### 4v2.8: CLI Integration + Sync E2E

**Wire the new engine to the CLI and prove it works end-to-end.**

- Wire `sync.go` to new `Engine` API (replace "not yet implemented" stub from Inc 0 with real engine constructor)
- E2E test updates: write new sync E2E tests against new engine, re-enable sync tests in CI (`integration.yml`)
- `conflicts` command: list unresolved conflicts from baseline (table or JSON)
- `resolve` command: interactive conflict resolution (keep local, keep remote, keep both)
- `verify` command: full-tree hash verification (compare local vs remote vs baseline)
- All commands support `--drive` and `--json` flags
- **Acceptance**: Build succeeds, all unit tests pass, all E2E tests pass, `golangci-lint` clean, CI green
- **Inputs**: [prd.md](design/prd.md) section 4, [event-driven-rationale.md](design/event-driven-rationale.md) Part 10 (Phase 5)

### Wave Structure

**Wave 0**: 4v2.0 (clean slate) — prerequisite for everything. Delete old code, create stubs.

**Wave 1**: 4v2.1 (types + baseline) — foundation types that everything depends on.

**Wave 2**: 4v2.2 (remote observer) + 4v2.3 (local observer) + 4v2.4 (change buffer) — independent of each other, all depend on types from 4v2.1. Can run in parallel with up to three agents.

**Wave 3**: 4v2.5 (planner) — depends on all Wave 2 outputs (events feed into planner).

**Wave 4**: 4v2.6 (executor) — depends on planner output (action plan).

**Wave 5**: 4v2.7 (engine wiring) + 4v2.8 (CLI + sync E2E) — sequential, wires everything together.

Re-plan after Wave 2 completes. Real implementation experience may shift boundaries.

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
- **Acceptance**: Integration test — start watch, create remote file, verify event produced

### 5.2: LocalObserver.Watch()

- rjeczalik/notify integration for cross-platform FS events (inotify on Linux, FSEvents on macOS)
- Filter: ignore `.partial` files, temp files, OS metadata
- Produces `ChangeEvent` stream into change buffer
- Network filesystem detection: fall back to periodic full scan if inotify unreliable
- **Acceptance**: Test with real temp dir — create/modify/delete files, verify events produced

### 5.3: Engine.RunWatch()

- Event loop: wait for change buffer flush, run planner + executor, commit baseline
- Same code path as RunOnce (one-shot = "observe everything, flush immediately"; watch = "observe incrementally, flush on debounce")
- Periodic full scan (configurable, default every 12 hours) as safety net
- Time-of-day bandwidth schedule: adjust rate limiter based on config and wall-clock time
- **Acceptance**: Integration test — start watch, create file, verify sync cycle triggered

### 5.4: Pause/Resume

- `Engine.Pause()`: stop executor, continue accumulating events in buffer
- `Engine.Resume()`: flush buffer, process accumulated events
- RPC via Unix domain socket for runtime control
- **Acceptance**: Test — pause, create files, resume, verify all changes synced

### 5.5: SIGHUP Config Reload + Stale File Detection

- SIGHUP handler: re-read config file, apply changes to running engine
- Detect stale `.partial` files from interrupted previous runs, clean up
- Filter changes applied immediately (hot reload without restart)
- **Acceptance**: Test — change config, send SIGHUP, verify new settings active

### 5.6: Graceful Shutdown

- Two-signal protocol: first SIGINT/SIGTERM drains current batch and commits checkpoint; second signal exits immediately
- WAL mode ensures SQLite consistency even on immediate exit
- **Acceptance**: Test — start sync, send signal mid-cycle, verify clean shutdown and baseline consistency

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

Each increment: independently testable, completable by a single agent. Phase 4 v2 count (9, including Increment 0) replaces the original Phase 4 v1 count (11) — the superseded increments are historical record only.

**Key architectural difference**: Phase 4 v1 used the database as the coordination mechanism (scanner and delta write eagerly, reconciler reads). Phase 4 v2 uses typed events as the coordination mechanism (observers produce events, planner operates as a pure function, executor produces outcomes, baseline manager commits atomically). Same decision matrix, same safety invariants, fundamentally different data flow.
