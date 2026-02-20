# onedrive-go — Implementation Roadmap

## Principles

- Each increment is completable by a single agent in one session
- Each increment has clear acceptance criteria (build + test + lint pass)
- Each increment is ~200-700 LOC of new code
- Design docs in `docs/design/` are the spec — use plan mode before each increment for file-level planning
- CLI-first: build a working tool before building the sync engine

---

## Phase 1: Graph Client + Auth + CLI Basics _(8 increments)_

**Build a working tool first.** After this phase, users can `login`, `ls`, `get`, `put`, `rm`, `mkdir`.

| Increment | Description | Est. LOC | Status |
|-----------|-------------|----------|--------|
| 1.1 | graph/ client: HTTP transport, retry, rate limiting, error mapping | ~350 | **DONE** |
| 1.2 | graph/ auth: device code flow, token persistence, refresh | ~250 | **DONE** |
| 1.3 | graph/ items: GetItem, ListChildren, CreateFolder, MoveItem, DeleteItem | ~200 | **DONE** |
| 1.4 | graph/ delta: Delta with full normalization pipeline (all quirks) | ~400 | **DONE** |
| 1.5 | graph/ transfers: Download, SimpleUpload, chunked uploads | ~300 | **DONE** |
| 1.6 | graph/ drives: Me, Drives, Drive | ~100 | **DONE** |
| 1.7 | cmd/ auth: login (device code), logout, whoami | ~200 | **DONE** |
| 1.8 | cmd/ file ops: ls, get, put, rm, mkdir, stat | ~400 | **DONE** |

### Pre-Phase 1 decision: Test strategy for `internal/graph/` ✅

**Decided**: `httptest` servers (real HTTP, no interfaces for mocking). Confirmed working well in 1.1. See LEARNINGS.md §4.

### 1.1: Graph client — HTTP transport — `internal/graph/client.go` ✅

- Client struct with configurable base URL, HTTP client, auth token source
- Automatic retry with exponential backoff + jitter for 429/5xx
- Rate limiting: respect Retry-After header
- Error mapping: HTTP status to sentinel errors (ErrNotFound, ErrGone, ErrThrottled, ErrUnauthorized)
- Request/response logging via slog
- **Acceptance**: `go test ./internal/graph/...` passes with httptest server
- **Inputs**: architecture.md section 7 (error handling), section 8 (quirk catalog)
- **Actual**: 690 LOC (client.go 228, errors.go 90, client_test.go 372), 88.5% coverage
- **Decision**: `httptest` servers for all tests (no mock interfaces). `sleepFunc` override for fast retry tests.

### 1.2: Graph auth — device code flow — `internal/graph/auth.go` ✅

- Device code OAuth2 flow via `oauth2` fork with `OnTokenChange` callback
- Atomic token persistence to disk (write-to-temp + rename, 0600 permissions)
- Token bridge: `oauth2.TokenSource` → `graph.TokenSource`
- Login, TokenSourceFromProfile, Logout functions
- ErrNotLoggedIn sentinel error
- **Acceptance**: `go test ./internal/graph/...` passes with httptest mock OAuth server
- **Inputs**: architecture.md section 9 (security model)
- **Actual**: 872 LOC (auth.go 257, auth_test.go 583, client_test.go +23, errors.go +1), 88.6% package coverage
- **Decision**: oauth2 fork (`github.com/tonimelisma/oauth2`) via `go.mod` replace directive for `OnTokenChange` callback. Separate `doLogin`/`tokenSourceFromPath`/`logout` internal functions for testability.

### 1.3: Graph items — CRUD operations — `internal/graph/items.go`, `internal/graph/types.go` ✅

- Item type with all fields needed by CLI and sync engine
- GetItem, ListChildren (with automatic pagination), CreateFolder, MoveItem, DeleteItem
- Response normalization: DriveID lowercasing, timestamp validation with fallback, nil-safe facet extraction
- Unexported JSON response types + `toItem()` normalization
- Refactored integration tests from raw `Do()` to typed methods with `/drives/{driveID}/...` paths
- Added `DefaultBaseURL` constant, `driveIDForTest` helper, CreateAndDeleteFolder round-trip test
- **Acceptance**: `go test` with mock HTTP responses + integration tests against real Graph API
- **Inputs**: architecture.md section 3
- **Actual**: 1177 LOC (items.go 384, types.go 30, items_test.go 642, integration_test.go +121), 90.8% package coverage
- **Decision**: `toItem()` normalization lives in items.go for now; extracted to dedicated files in 1.4 when delta adds more quirk handlers.

### 1.4: Graph delta — normalization pipeline — `internal/graph/delta.go`, `internal/graph/normalize.go` ✅

- `Delta(driveID, token)` — single page. `DeltaAll(driveID, token)` — all pages with automatic pagination.
- Normalization pipeline in normalize.go: `filterPackages()`, `clearDeletedHashes()`, `deduplicateItems()`, `reorderDeletions()`
- Delta token handling: empty (initial sync), full URL (strip base), relative path
- Reuses `driveItemResponse` + `toItem()` from items.go
- **Acceptance**: `go test` with 25 test cases (13 delta + 12 normalize)
- **Inputs**: architecture.md section 8 (quirk catalog)
- **Actual**: 837 LOC (delta.go 146, normalize.go 157, delta_test.go 308, normalize_test.go 226), 92.2% package coverage

### 1.5: Graph transfers — download + upload — `internal/graph/download.go`, `internal/graph/upload.go` ✅

- `Download(driveID, itemID, w io.Writer)` — streams via pre-authenticated URL (bypasses Graph API base URL)
- `SimpleUpload` for <4MB, `CreateUploadSession`/`UploadChunk`/`CancelUploadSession` for resumable uploads
- Private `doRawUpload()` helper for auth + custom content type, no retry (can't safely replay io.Reader)
- 320 KiB chunk alignment validation
- **Acceptance**: `go test` with 27 test cases (7 download + 20 upload)
- **Inputs**: architecture.md section 3
- **Actual**: 1004 LOC (download.go 94, upload.go 294, download_test.go 194, upload_test.go 422), 91.2% package coverage

### 1.6: Graph drives — `internal/graph/drives.go` ✅

- `Me()` -> *User, `Drives()` -> []Drive, `Drive(driveID)` -> *Drive
- Email fallback: uses `userPrincipalName` when `mail` is empty (Personal account quirk)
- **Cleanup**: B-024 done — replaced raw `Do()` in bootstrap tool with typed `Drives()` method
- Integration tests: TestIntegration_Me, TestIntegration_Drives added
- **Acceptance**: `go test` with mock responses + integration tests
- **Inputs**: architecture.md section 3
- **Actual**: 521 LOC (drives.go 165, drives_test.go 289, integration_test.go +36, bootstrap -25), 90.9% package coverage

### 1.7: CLI auth commands — `auth.go` ✅

- `login` — device code flow (display URL, wait for auth, save token)
- `logout` — delete token file
- `whoami` — display authenticated user + account type (text or JSON)
- All support `--profile` and `--json` flags
- **Cleanup**: Deleted `cmd/integration-bootstrap` entirely (B-025), updated CI to use `whoami --json`
- **Acceptance**: Build succeeds, `--help` works, format_test.go covers formatters
- **Inputs**: prd.md section 4
- **Actual**: ~430 LOC (root.go 73, auth.go 166, format.go 80, format_test.go 111)
- **Decision**: Constructor pattern (`newRootCmd()`) instead of `init()` — `gochecknoinits` linter requires it. Root package (not `cmd/onedrive-go/`) — single binary via `go install`.

### 1.8: CLI file operations — `files.go` ✅

- `ls [path]` — list directory (table or JSON, folders first)
- `get <remote> [local]` — download file
- `put <local> [remote]` — upload file (simple <4MB, chunked >=4MB)
- `rm <path>` — delete file or folder
- `mkdir <path>` — create folder (recursive, handles 409 Conflict)
- `stat <path>` — show item metadata (text or JSON)
- All support `--profile` and `--json` flags
- **Acceptance**: Build succeeds, E2E tests pass against live OneDrive
- **Inputs**: prd.md section 4
- **Actual**: ~493 LOC (files.go 493)
- **Decision**: Path-based methods (`GetItemByPath`, `ListChildrenByPath`) added to graph client. `clientAndDrive()` discovers primary drive via `Drives()` call.

---

## Phase 2: E2E CI _(3 increments)_

**Prove the tool works against real OneDrive.** Azure Key Vault + OIDC for token management.

| Increment | Description | Est. LOC | Status |
|-----------|-------------|----------|--------|
| 2.1 | CI scaffold: GitHub Actions, Azure Key Vault + OIDC, integration tests | ~200 YAML + Go | **DONE** |
| 2.2 | E2E tests: login, ls, get, put, rm round-trip against live API | ~400 | **DONE** |
| 2.3 | E2E edge cases: large files, special characters, concurrent ops | ~300 |

### 2.1: CI scaffold — GitHub Actions + Azure Key Vault ✅

- Azure OIDC federation: GitHub Actions authenticates to Azure via federated identity (no stored credentials)
- Azure Key Vault stores OAuth token JSON per profile (`onedrive-oauth-token-{profile}`)
- Integration tests (`//go:build integration`) in `internal/graph/integration_test.go` validate full stack against real Graph API
- CI workflow (`.github/workflows/integration.yml`) runs on push to main + nightly + manual dispatch
- Token bootstrap tool (`cmd/integration-bootstrap/main.go`) for initial auth before CLI `login` exists
- Multi-profile support via `ONEDRIVE_TEST_PROFILES` env var (comma-separated)
- Corrupted token writeback protection: validates JSON structure before uploading to Key Vault
- **Acceptance**: Workflow runs, authenticates with OneDrive, completes without error
- **Inputs**: test-strategy.md section 10
- **Actual**: ~200 LOC (integration.yml ~100, integration_test.go ~80, bootstrap ~30)

### 2.2: E2E tests — round-trip file operations ✅

- Build tag `//go:build e2e` to separate from unit tests
- `TestMain` builds binary to temp dir, `runCLI` subprocess executor
- Timestamped test directories on OneDrive (auto-cleanup via `t.Cleanup`)
- Serial subtests: whoami, ls root, mkdir (with subfolder), put, ls folder, stat, get (content verification), rm file, rm subfolder
- CI step added to integration.yml
- **Acceptance**: `go test -tags e2e ./e2e/...` passes against live OneDrive
- **Inputs**: test-strategy.md section 7
- **Actual**: ~188 LOC (e2e/e2e_test.go 188)

### 2.3: E2E edge cases — large files, special characters, concurrent ops

- Large file upload/download via resumable upload session (>4MB)
- Unicode filenames, special characters, spaces, long paths
- Concurrent upload + download of multiple files
- **Acceptance**: `go test -tags e2e ./e2e/...` passes including edge case scenarios
- **Inputs**: test-strategy.md section 7, architecture.md section 8 (quirk catalog)
- **Size**: ~300 LOC

---

## Phase 3: Config _(3 increments)_

| Increment | Description | Est. LOC |
|-----------|-------------|----------|
| 3.1 | config/ TOML loading + validation (reuse existing internal/config/ logic) | ~550 |
| 3.2 | config/ profiles + path derivation | ~300 |
| 3.3 | cmd/ config: config init wizard, config show, migrate | ~300 |

### 3.1: Config — TOML loading + validation — `internal/config/`

- Much of this exists already in internal/config/. Validate, refine, ensure all options from configuration.md are covered.
- Config struct with all global options (sync, filter, transfers, safety, logging, network)
- Unknown key detection -> fatal error with "did you mean X?" suggestion
- Validation: type checks, range checks, cross-field constraints
- XDG-compliant default paths (Linux + macOS)
- Environment variable overrides (ONEDRIVE_GO_CONFIG, ONEDRIVE_GO_PROFILE, ONEDRIVE_GO_SYNC_DIR)
- Override precedence: defaults -> file -> env -> CLI flags
- **Acceptance**: `go test ./internal/config/...` passes with valid + invalid + malformed config fixtures
- **Inputs**: configuration.md sections 1-2, 9-10, 13
- **Size**: ~550 LOC

### 3.2: Config — profiles + path derivation — `internal/config/`

- Multi-profile support with `[profile.NAME]` sections in TOML
- Per-profile fields: account_type, sync_dir, remote_path, drive_id
- Per-profile section overrides: `[profile.work.filter]` replaces global `[filter]`
- Profile path derivation: DB path, token path
- Default profile when --profile omitted
- **Acceptance**: `go test ./internal/config/...` passes with multi-profile scenarios
- **Inputs**: configuration.md sections 3-5
- **Size**: ~300 LOC

### 3.3: CLI config commands — `cmd/onedrive-go/config.go`

- `config init` — interactive setup wizard (authenticate -> account type -> sync dir -> filters -> write TOML)
- `config show` — display effective config with overrides highlighted
- `migrate [--from abraunegg|rclone]` — detect + convert existing configuration
- All support `--profile` and `--json` flags
- **Acceptance**: Build succeeds, unit tests for config generation and migration
- **Inputs**: prd.md section 4, configuration.md sections 4, 12
- **Size**: ~300 LOC

---

## Phase 4: Sync Engine _(12 increments)_

**Now build sync.** All domain logic from sync-algorithm.md, data-model.md, safety invariants.

| Increment | Description | Est. LOC |
|-----------|-------------|----------|
| 4.1 | sync/ SQLite state store (schema, migrations, items CRUD, checkpoints) | ~800 |
| 4.2 | sync/ delta processor (fetches graph.Delta, stores as sync.Records) | ~400 |
| 4.3 | sync/ local scanner (filesystem walk, hash computation, filter evaluation) | ~500 |
| 4.4 | sync/ filter engine (three-layer cascade, .odignore, reuse existing logic) | ~400 |
| 4.5 | sync/ reconciler (F1-F14, D1-D7 decision matrix, move detection) | ~550 |
| 4.6 | sync/ safety checks (S1-S7, big-delete protection, dry-run) | ~300 |
| 4.7 | sync/ executor (dispatch actions, update state, error classification) | ~450 |
| 4.8 | sync/ conflict handler (detect, classify, keep-both resolution, ledger) | ~300 |
| 4.9 | sync/ transfer (download pipeline, upload pipeline, worker pools, bandwidth) | ~500 |
| 4.10 | sync/ engine (RunOnce: wire delta->scan->reconcile->safety->execute) | ~300 |
| 4.11 | cmd/ sync: sync command (one-shot, --download-only, --upload-only, --dry-run) | ~300 |
| 4.12 | cmd/ conflicts: conflicts list, resolve, verify | ~300 |

### 4.1: SQLite state store — `internal/sync/state.go`

- Schema from data-model.md: items table, checkpoints table, conflicts table, stale_files table, upload_sessions table
- SQLite open with pragmas (WAL mode, FULL synchronous, foreign keys)
- Migration framework (version table, up/down migrations)
- Items CRUD: GetItem, UpsertItem, DeleteItem, ListChildren, GetItemByPath, batch upsert
- Checkpoints: Get/Save delta token per driveID
- Conflicts: Record, List, Resolve, count
- Stale files: Record, List, Remove
- Type definitions: Item, ConflictRecord, StaleRecord, SyncStatus enum, ItemType enum
- **Acceptance**: `go test ./internal/sync/...` passes (in-memory SQLite)
- **Inputs**: data-model.md sections 1-9, architecture.md section 3.2
- **Size**: ~800 LOC

### 4.2: Delta processor — `internal/sync/delta.go`

- Calls graph.Delta() to fetch delta pages from Graph API
- Converts graph.Item to sync.Record (internal representation)
- Batch processing: configurable batch size (default 500)
- Save delta token checkpoint after each batch
- Handle HTTP 410: detect resync type, clear state, restart with fresh delta
- Handle pagination (nextLink -> deltaLink)
- **Acceptance**: `go test ./internal/sync/...` passes with mock graph client returning multi-page deltas
- **Inputs**: sync-algorithm.md section 3, data-model.md section 7
- **Size**: ~400 LOC

### 4.3: Local scanner — `internal/sync/scanner.go`

- Walk local filesystem tree under sync_dir
- Apply filter engine to each path (skip excluded)
- Collect file metadata: size, mtime, path
- Enqueue hash computation jobs to checker pool
- Compare scan results with last-known state from DB
- Detect: new files, modified files, deleted files, moved files (by matching content hash)
- **Acceptance**: `go test ./internal/sync/...` passes with temp dir scenarios
- **Inputs**: sync-algorithm.md section 4
- **Size**: ~500 LOC

### 4.4: Filter engine — `internal/sync/filter.go`

- Three-layer cascade: sync_paths -> config skip patterns -> .odignore
- Glob pattern matching (Go gitignore library for .odignore)
- ShouldSync(path, isDir, size) -> (bool, reason)
- OneDrive naming restriction validation (illegal chars, reserved names, trailing dots/spaces)
- Load .odignore files from filesystem (walked during scan)
- Reuse existing filter logic where applicable
- **Acceptance**: `go test ./internal/sync/...` passes, including property-based tests (monotonic exclusion)
- **Inputs**: configuration.md section 6, sync-algorithm.md section 6, architecture.md section 3.4
- **Size**: ~400 LOC

### 4.5: Reconciler — three-way merge — `internal/sync/reconciler.go`

- Implement all 14 file decision matrix rows (F1-F14) from sync-algorithm.md section 5
- Implement all 7 folder decision matrix rows (D1-D7)
- Table-driven implementation: decision lookup by (local_state, remote_state, base_state)
- Move detection: match by content hash when file disappears at one path and appears at another
- Output: ordered action plan (list of typed actions: download, upload, delete_local, delete_remote, rename, mkdir, conflict)
- **Acceptance**: `go test` passes with one table-driven test case per decision row (21 cases minimum)
- **Inputs**: sync-algorithm.md section 5 (decision matrices)
- **Size**: ~550 LOC

### 4.6: Safety checks — `internal/sync/safety.go`

- S1: Never delete remote based on local absence (check synced_hash set)
- S2: Never process deletions from incomplete delta (check delta completed flag)
- S3: Verify .partial files, never overwrite target before hash match
- S4: Hash-before-delete (verify file matches expected content before removing)
- S5: Big-delete protection (count OR percentage threshold, configurable)
- S6: Disk space check before downloads
- S7: Never upload .partial or temp files
- Dry-run mode: generate action plan but skip execution
- **Acceptance**: `go test` passes with one test per invariant (7 tests minimum)
- **Inputs**: sync-algorithm.md section 8
- **Size**: ~300 LOC

### 4.7: Executor — `internal/sync/executor.go`

- Process ordered action plan from reconciler
- Dispatch downloads/uploads to transfer pipeline
- Local filesystem operations: create dirs, rename, delete (to OS trash by default)
- Update state DB after each successful action
- Error handling per four-tier model: Fatal (abort), Retryable (queue retry), Skip (log + continue), Deferred (save for next cycle)
- Generate SyncReport with counts and errors
- **Acceptance**: `go test` passes with mock graph client + mock filesystem
- **Inputs**: sync-algorithm.md section 9, architecture.md section 7
- **Size**: ~450 LOC

### 4.8: Conflict handler — `internal/sync/conflict.go`

- Detect conflict types: edit-edit, edit-delete, delete-edit, create-create, type change
- False conflict detection: both sides modified but content identical -> silent resolve
- Keep-both resolution: rename loser with `.conflict-{timestamp}` suffix, download remote as original name
- Record in conflict ledger with full context (both versions, timestamps, hashes)
- **Acceptance**: `go test` passes with one test per conflict type
- **Inputs**: sync-algorithm.md section 7
- **Size**: ~300 LOC

### 4.9: Transfer pipeline — `internal/sync/transfer.go`

- Worker pools: downloads, uploads, hash checkers (configurable pool sizes, default 8 each)
- Download pipeline: fetch -> write to `.partial` file -> hash verify (QuickXorHash) -> atomic rename
- Upload pipeline: simple upload (<4MB) or resumable upload (>=4MB) -> verify response hash
- Streaming hash via io.TeeReader (no double read)
- Token bucket bandwidth limiter with time-of-day scheduling
- Disk space check before download
- Graceful shutdown: drain queues, wait for in-flight
- **Acceptance**: `go test ./internal/sync/...` passes including concurrency tests
- **Inputs**: architecture.md section 3.3, configuration.md section 7, sync-algorithm.md section 9
- **Size**: ~500 LOC

### 4.10: Engine — RunOnce — `internal/sync/engine.go`

- Wire the full pipeline: delta fetch -> scan -> reconcile -> safety check -> execute
- Mode dispatch: bidirectional (default), download-only, upload-only
- Sync report aggregation
- Context-based cancellation
- Interface-only dependencies (all injected)
- **Acceptance**: Integration test with mock graph client + real SQLite + real temp dir passes end-to-end
- **Inputs**: sync-algorithm.md sections 1-2, architecture.md section 3.1
- **Size**: ~300 LOC

### 4.11: CLI sync command — `cmd/onedrive-go/sync.go`

- `sync` — one-shot bidirectional sync
- `sync --watch` — continuous mode (placeholder, wired in Phase 5)
- `sync --download-only`, `sync --upload-only` — directional modes
- `sync --dry-run` — show action plan without executing
- `sync --profile NAME` — sync specific profile
- `sync status` — show current sync state, pending actions, last sync time
- Cobra command with proper flag definitions and validation
- **Acceptance**: Build succeeds, `--help` works, unit tests for flag parsing
- **Inputs**: prd.md section 4 (CLI design), architecture.md section 2
- **Size**: ~300 LOC

### 4.12: CLI conflict + verify commands — `cmd/onedrive-go/conflicts.go`

- `conflicts` — list unresolved conflicts from ledger (table or JSON)
- `resolve <id|path>` — interactive conflict resolution (keep local, keep remote, keep both)
- `verify` — full-tree hash verification (compare local vs remote vs DB)
- All support `--profile` and `--json` flags
- **Acceptance**: Build succeeds, unit tests for ledger display and resolution flow
- **Inputs**: prd.md section 4, sync-algorithm.md section 7
- **Size**: ~300 LOC

---

## Phase 5: Real-Time + Polish _(6 increments)_

| Increment | Description | Est. LOC |
|-----------|-------------|----------|
| 5.1 | sync/ local monitor (inotify/FSEvents, debounce) | ~250 |
| 5.2 | sync/ remote monitor (WebSocket + polling fallback) | ~350 |
| 5.3 | sync/ RunWatch (continuous mode, SIGINT/SIGTERM, SIGHUP reload) | ~300 |
| 5.4 | cmd/ sync --watch | ~100 |
| 5.5 | CI: full pipeline (unit + integration + E2E + chaos) | ~300 YAML |
| 5.6 | Packaging: goreleaser, Homebrew, man pages | ~300 |

### 5.1: Local FS monitor — `internal/sync/monitor_local.go`

- rjeczalik/notify integration for cross-platform FS events (inotify on Linux, FSEvents on macOS)
- 2-second batch debounce window
- Filter: ignore .partial files, temp files, OS metadata
- Output: channel of batched change events
- **Acceptance**: `go test` passes with real temp dir + file creation/modification/deletion
- **Inputs**: architecture.md section 3.7, sync-algorithm.md section 11
- **Size**: ~250 LOC

### 5.2: Remote monitor — `internal/sync/monitor_remote.go`

- WebSocket subscription to Graph API change notifications
- 5-minute polling fallback (for when WebSocket unavailable)
- Automatic reconnection with backoff
- Output: channel of "remote changed" signals
- **Acceptance**: `go test` passes with mock WebSocket server
- **Inputs**: architecture.md section 3.7, sync-algorithm.md section 11
- **Size**: ~350 LOC

### 5.3: Continuous mode — RunWatch — `internal/sync/engine.go`

- RunWatch: event loop combining local + remote monitors
- Trigger sync cycle on change events (debounced)
- Periodic full scan (configurable frequency, default every 12 hours)
- Graceful shutdown: SIGINT/SIGTERM -> finish current cycle -> exit
- SIGHUP -> reload config
- **Acceptance**: Integration test: start watch -> create file -> verify sync triggered -> stop
- **Inputs**: sync-algorithm.md section 11, configuration.md section 14
- **Size**: ~300 LOC

### 5.4: CLI sync --watch — `cmd/onedrive-go/sync.go`

- Wire `sync --watch` flag to sync.Engine.RunWatch
- Signal handling: forward SIGINT/SIGTERM/SIGHUP to engine
- Status output: periodic summary of sync activity
- **Acceptance**: Build succeeds, `--help` documents --watch, integration test with mock engine
- **Inputs**: prd.md section 4
- **Size**: ~100 LOC

### 5.5: CI — full pipeline — `.github/workflows/`

- Job 1 (every PR): lint + build + unit tests (~2 min)
- Job 2 (every PR): integration tests with build tags (~3 min)
- Job 3 (merge to main + nightly): E2E against live OneDrive Personal (~10 min)
- Coverage enforcement: fail if below 80% overall
- Benchmark tracking: run benchmarks, store results, comment on PR with trends
- Nightly: extended chaos + stress tests
- Credential management: private GitHub Gist for OAuth tokens
- **Acceptance**: CI green on push. All 3 jobs defined and functional.
- **Inputs**: test-strategy.md section 10
- **Size**: ~300 lines YAML

### 5.6: Packaging + release

- goreleaser config for Linux + macOS binaries (amd64 + arm64)
- Homebrew tap formula
- AUR PKGBUILD (best-effort)
- Man page generation from Cobra
- README update with installation instructions
- **Acceptance**: `goreleaser build --snapshot` produces binaries for all targets
- **Inputs**: prd.md section 3 (platforms)
- **Size**: ~300 LOC config

---

## Future (post-v1)

- RPC interface for pause/resume/status
- FUSE filesystem mount
- National cloud support

---

## Summary

| Phase | Increments | Focus |
|-------|-----------|-------|
| 1 | 8 | Graph API client + auth + CLI basics |
| 2 | 3 | E2E CI against real OneDrive |
| 3 | 3 | Config (TOML, profiles, wizard) |
| 4 | 12 | Sync engine (state, delta, reconciler, executor, conflicts) |
| 5 | 6 | Real-time monitoring + polish + packaging |
| **Total** | **32** | |

Each increment: ~200-700 LOC new code, independently testable, completable by a single agent.

Phase 1 increments (1.1-1.6) have no cross-dependencies within the graph/ package and can be parallelized. Increments 1.7-1.8 depend on 1.1-1.6.

**Review point after Phase 1**: Evaluate whether `internal/graph/` has grown too large and should be split (e.g., separate `auth/` package). Decide based on actual LOC and cohesion, not upfront speculation.
