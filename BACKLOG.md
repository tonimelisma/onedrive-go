# Backlog

Historical backlog from Phases 1-4v1 archived in `docs/archive/backlog-v1.md`.

## Active (In Progress)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Ready (Up Next)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Backlog (Architecture-Neutral)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-026 | Replace `ONEDRIVE_TEST_DRIVE_ID` env var with typed discovery | P3 | CI + tests | CI could use a typed Go helper instead of the bootstrap tool to discover the drive ID. |
| B-030 | Review whether `internal/graph/` should be split | P2 | `internal/graph/` | Package has ~15 files. Assess cohesion vs. size. |
| B-033 | Implement accounts.md features | P1 | all | Setup wizard, `drive add`/`drive remove`, fuzzy `--drive` matching, RPC for `sync --watch`, email change detection, `service install`/`uninstall`/`status`, `status` command, `--account` flag, text-level config manipulation, commented-out config defaults on first login. |
| B-034 | Add --json support for login, logout, drive add/remove | P2 | root | These commands output plain text only. Per accounts.md §9, login should emit JSON events. |
| B-035 | Add --quiet support for login, logout, whoami, status, drive | P2 | root | Auth/drive commands write to stdout via `fmt.Printf`. File ops use `statusf()` which respects --quiet. Standardize all commands. |
| B-036 | Extract CLI service layer for testability | P2 | root | Root package at 28.1% coverage. All RunE handlers are untested. Need to extract pure logic from I/O for mock-based testing. Target: 50%+ coverage. |
| B-020 | SharePoint lock check before upload (HTTP 423) | P2 | `internal/graph/` | Check lock status before uploading to SharePoint. Avoid overwriting co-authored documents. |
| B-021 | Hash fallback chain for missing hashes | P2 | `internal/graph/` | Some Business/SharePoint files lack any hash. Fall back: QuickXorHash -> SHA256 -> size+eTag+mtime. |
| B-023 | `/children` fallback when delta incomplete | P3 | `internal/graph/` | National Cloud Deployments don't support `/delta`. Implement `/children` traversal fallback. |
| B-007 | Cross-drive DriveID handling for shared/remote items | P3 | `internal/graph/` | Verify against real API responses in E2E CI. |
| B-008 | Spec inconsistency: chunk_size units (MB vs MiB) | P2 | `docs/design/` | configuration.md says "10MB" default but requires 320 KiB multiples. Clarify spec. |
| B-031 | Profile and optimize performance | P3 | all | After feature-complete: CPU/memory/I/O profiling with `pprof`. |
| B-032 | Like-for-like performance benchmarks vs rclone and abraunegg | P3 | benchmarks | Reproducible benchmark suite comparing sync performance. |
| B-060 | Add `ResolveDrives()` for multi-drive sync | P1 | `internal/config/` | `ResolveDrive()` returns one drive. `sync` (all enabled) and `sync --drive a --drive b` need `ResolveDrives()` returning `[]*ResolvedDrive`. Required for 4v2.7 engine wiring. |
| B-061 | Shared `graph.Client` per token file for multi-drive | P2 | `internal/sync/` | Multiple drives sharing an account (business + SharePoint) should share one `graph.Client` (same token, same rate limit tracking). Engine wiring (4v2.7) should create one Client per unique token path, not per drive. |
| B-062 | Global worker pool cap for multi-drive sync | P2 | `internal/sync/` | Per-drive pools (8 dl + 8 ul + 8 hash) multiply with concurrent drives. 5 drives = 120 I/O goroutines. Need global cap or per-drive reduction when multiple drives active. Decision for 4v2.6 (executor) or 4v2.7 (engine wiring). |
| B-063 | Per-tenant rate limit coordination | P3 | `internal/graph/` | Multiple drives under same tenant share Graph API rate limits. Current per-client 429 retry works but isn't optimal. Shared rate limiter per-tenant would be better. Not critical for MVP. |
| B-064 | Baseline memory scaling for many drives | P3 | `internal/sync/` | Each drive loads full baseline into memory (~19 MB per 100K files). Additive across drives. 5 drives × 100K = ~95 MB baselines alone. Monitor during profiling (B-031). Lazy loading if needed. |
| B-086 | Conflict notification for watch mode | P3 | `internal/sync/` | Phase 5: auto-resolved conflicts should emit events (structured log, desktop notification). |
| B-087 | Conflict retention/pruning policy | P3 | `internal/sync/` | Resolved conflicts accumulate forever. Add configurable retention (e.g., 90 days) with periodic pruning in Commit(). |
| B-071 | ConflictRecord missing Name field | P3 | `internal/sync/` | UX convenience for conflict reporting. `path.Base(Path)` suffices but Name from the PathView is cleaner. |
| B-074 | Drive identity verification at Engine startup | P2 | `internal/sync/` | Engine (4v2.7) should verify that the configured `driveid.ID` matches the API-reported drive ID at sync start. Catches stale config, renamed drives, or ID format changes. |
| B-085 | Resumable downloads (Range header) | P2 | `internal/graph/`, `internal/sync/` | Interrupted downloads restart from byte 0. Keep `.partial` file, send `Range: bytes=<size>-` on retry, append, hash-verify after completion. Matters for large files on slow connections — without this, Ctrl-C + re-sync restarts multi-GB downloads. Upload sessions already have resume via `QueryUploadSession`. |
| B-088 | Configurable auto-delete of OS junk files (.DS_Store etc.) | P3 | `internal/sync/` | Add configurable option to automatically delete OS-generated junk files during sync (e.g., `.DS_Store`, `Thumbs.db`, `desktop.ini`, `._*` resource forks). Requires research: enumerate all common OS/editor/IDE junk filenames across macOS, Windows, Linux. Consider whether to delete locally, remotely, or both. May overlap with existing always-excluded filter patterns — evaluate unifying into a single configurable exclusion/cleanup system. Reference: `.gitignore` templates, rclone `--delete-excluded`, rsync filter rules. |
| B-089 | Baseline concurrent-safe incremental cache for per-action commits | P1 | `internal/sync/` | Phase 5.3: `CommitOutcome()` is called by concurrent workers. `Baseline.ByPath`/`ByID` must be updated in-place (not full reload) under `sync.RWMutex`. Readers (`resolveParentID`, planner) take RLock; writers (`CommitOutcome`) take Lock. Replaces old `Commit()` cache invalidate+reload pattern. |
| B-090 | Eliminate `createdFolders` map — use incremental baseline instead | P1 | `internal/sync/` | Phase 5.3: `createdFolders` is per-Executor (not shared across workers). With B-089, `CommitOutcome()` updates `Baseline.ByPath` immediately, so `resolveParentID()` finds newly-created folders in its baseline fallback branch. DAG edges guarantee folder create completes before child dispatch. Delete `createdFolders` field and first branch of `resolveParentID()`. |
| B-091 | `resolveTransfer()` calls batch `Commit()` — must migrate to `CommitOutcome()` | P1 | `internal/sync/` | Phase 5.3: `engine.go:384` calls `e.baseline.Commit(ctx, []Outcome{outcome}, "", ...)` for conflict resolution. When batch `Commit()` is deleted, this breaks at compile time. Change to `CommitOutcome()` with `ledgerID = ""` (skip ledger update). `CommitOutcome()` must handle empty ledgerID gracefully. |

## Event-Driven Pivot (Phase 4 v2)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

### Closed

| ID | Title | Resolution |
|----|-------|------------|
| B-054 | Remove old `internal/sync/` code after Phase 4v2 complete | **Superseded** — old code removal moved to Increment 0 (B-055), not end of Phase 4v2. |
| B-055 | Increment 0: Delete old sync code, stub sync command, remove tombstone config, update CI | **Done** — Phase 4v2 Increment 0 complete. Old sync engine deleted, sync.go stubbed, tombstone config removed, CI updated, clean-slate orphan branch created. |
| B-056 | Remove `tombstone_retention_days` from config package | **Done** — Completed as part of Increment 0 (B-055). |
| B-057 | Remove sync integration test line from `integration.yml` | **Done** — Completed as part of Increment 0 (B-055). |
| B-053 | Implement Phase 4v2.1: Types + Baseline Schema + BaselineManager | **Done** — PR #78. types.go, migrations, baseline.go, 25 tests, 82.5% coverage. |
| B-059 | Implement Phase 4v2.2: Remote Observer | **Done** — PR #80. observer_remote.go, 23 tests, 86.4% coverage. |
| B-065 | Implement Phase 4v2.3: Local Observer | **Done** — PR #82. observer_local.go (FullScan, name validation, always-excluded, QuickXorHash), 31 tests with real temp dirs, 87.7% coverage. |
| B-066 | Implement Phase 4v2.4: Change Buffer | **Done** — PR #84. buffer.go (thread-safe Add/AddAll/FlushImmediate, move dual-keying), 14 tests with race detector, 91.2% coverage. |
| B-067 | Implement Phase 4v2.5: Planner | **Done** — PR #85. planner.go (5-step pipeline, EF1-EF14 + ED1-ED8 decision matrices, move detection, big-delete safety), 43 tests, 91.2% coverage. |
| B-073 | DriveTokenPath/DriveStatePath should accept `driveid.CanonicalID` | **Done** — Both functions now accept `driveid.CanonicalID`. All callers migrated: `auth.go` helper chain (`discoverAccount`, `findTokenFallback`, `canonicalIDForToken`, `purgeSingleDrive`), `files.go`, `status.go`, `drive.go`, `integration_test.go`. |
| B-068 | Executor must fill zero DriveID for new local items | **Done** — PR #90. `resolveDriveID()` fills zero DriveID from executor's per-drive context. |
| B-070 | Add ParentID to Action struct | **Closed (by design)** — PR #90. Executor resolves parent IDs dynamically via `resolveParentID()` chain: createdFolders → baseline → "root". No need to add ParentID to Action. |
| B-052 | Re-enable E2E tests in CI | **Done** — 4v2.8. E2E test block uncommented in `integration.yml`. |
| B-058 | Re-enable sync E2E tests in CI after Increment 8 | **Done** — 4v2.8. Sync E2E tests written (`e2e/sync_e2e_test.go`), CI re-enabled. Interactive resolve deferred to Phase 5. |
| B-075 | Upload session leak in chunkedUpload | **Done** — Hardening. `CreateUploadSession` succeeded but `os.Open`/`uploadChunks` failure paths never canceled the session. Added `CancelUploadSession` to `TransferClient`, `cancelSession` helper, regression test. |
| B-076 | Partial file leak on f.Close() failure | **Done** — Hardening. `downloadToPartial` left `.partial` file on disk if `f.Close()` failed after successful download. Added `os.Remove(partialPath)` in Close error path. |
| B-077 | resolveTransfer nil-map panic for conflicts resolved outside Execute() | **Done** — Hardening. `executor.baseline` and `executor.createdFolders` were uninitialized when `resolveTransfer` was called from `ResolveConflict` without a prior `Execute()`. Added lazy initialization guard. |
| B-078 | TransferClient leaks upload session lifecycle to consumers | **Done** — Refactor. `TransferClient` (5 methods) replaced with `Downloader` (1) + `Uploader` (1). Upload session lifecycle (create/chunk/cancel) encapsulated in `graph.Client.Upload()`. Removed ~170 lines of duplicated state machine code from Executor and CLI. |
| B-079 | Executor conflates immutable config with per-call mutable state | **Done** — Refactor. Split `Executor` into immutable `ExecutorConfig` + ephemeral `Executor` created per `Execute()` call via `NewExecution()`. Eliminates nil-map panics and temporal coupling. Phase 5 thread-safe. |
| B-080 | Upload progress callback was nil in executeUpload | **Done** — Hardening. `executeUpload` passed `nil` for progress callback. Added per-upload Debug-level closure with path context. |
| B-081 | Simple upload doesn't preserve mtime | **Done** — Hardening. Graph API simple upload (PUT /content) can't include `fileSystemInfo`. Added `UpdateFileSystemInfo()` PATCH method, called after simple upload when mtime is non-zero. |
| B-082 | Baseline Load() queries DB on every call | **Done** — Hardening. `resolveTransfer` called `Load()` per conflict, loading full baseline N times for N conflicts. Added cache-through pattern: `Load()` returns cached `*Baseline` if available, `Commit()` invalidates and refreshes. |
| B-083 | engineMockClient lacks compile-time interface checks | **Done** — Hardening. Added `var _ Interface = (*engineMockClient)(nil)` for all 4 interfaces + design comment. |
| B-084 | EF9 edit-delete conflict fails silently (404 download loop) | **Done** — Auto-resolve: local edit wins, uploaded to re-create remote. Conflict recorded as auto-resolved in history. `conflicts --history` shows resolved conflicts. E2E tests updated. |
| B-037 | Add chunk upload retry for pre-auth URLs | **Done** — `doPreAuthRetry` method added to `graph.Client`. `UploadChunk` (`io.Reader` → `io.ReaderAt`), `CancelUploadSession`, `QueryUploadSession`, `downloadFromURL` all use it. Retro follow-ups: graph coverage recovered via decode-error test; baseline cache invalidation YAGNI; upload-then-PATCH purity confirmed correct. |
| B-069 | Handle locally-deleted folder with no remote delta event | **Done** — ED8 changed from ActionCleanup to ActionRemoteDelete. Both folder classifiers restructured with upfront mode filtering parallel to file path. |
| B-072 | ED1-ED8 missing folder-delete-to-remote (folder EF6) | **Done** — Fixed as part of B-069. ED8 now propagates local folder deletes to remote. |
