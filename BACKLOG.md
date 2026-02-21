# Backlog

## Active (In Progress)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Ready (Up Next)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-026 | Replace `ONEDRIVE_TEST_DRIVE_ID` env var with typed discovery | P3 | CI + tests | CI could use a typed Go helper instead of the bootstrap tool to discover the drive ID. Lower priority since env var approach is clean. |

## Future Phase Review (Before Phase 4)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| ~~B-027~~ | ~~Decide conflict resolution UX before building sync engine~~ | ~~P1~~ | ~~`internal/sync/`~~ | **CLOSED**: Conflict resolution UX finalized in sync-algorithm.md §7.4. Interactive mode (per-conflict prompting with L/R/B/S/Q) + batch mode (--keep-local, --keep-remote, --keep-both, --all, --dry-run). prd.md §4 and §8 updated to match. |
| ~~B-028~~ | ~~Evaluate merging Phase 3 into Phase 4~~ | ~~P2~~ | ~~`internal/config/`~~ | **CLOSED**: Phase 3 completed (PRs #19, #20). Config package now at 95.6% coverage with Resolve(), CLI integration, config show. Config init wizard and migrate deferred to Phase 5. |
| ~~B-029~~ | ~~Plan Phase 4 in two waves~~ | ~~P1~~ | ~~`internal/sync/`~~ | **CLOSED**: Phase 4 wave structure added to roadmap.md. Wave 1A (4.1+4.4), Wave 1B (4.2+4.3), Wave 2 (4.5-4.12, re-plan after Wave 1). All four Wave 1 increments can potentially run as a single wave if file conflicts are zero. |
| B-030 | Review whether `internal/graph/` should be split | P2 | `internal/graph/` | After 1.4-1.6, package will have ~15 files. Assess cohesion vs. size. architecture.md already calls for this review point. |
| B-033 | Implement accounts.md features | P1 | all | New features from accounts.md that will need implementation: setup wizard, `--browser` auth flow, `drive add`/`drive remove`, fuzzy `--drive` matching, RPC for `sync --watch`, email change detection, `service install`/`uninstall`/`status`, `status` command, `--account` flag for auth commands, text-level config manipulation, commented-out config defaults on first login. |

## Backlog (Discovered During Foundation Hardening)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-034 | Add --json support for login, logout, drive add/remove | P2 | root | These commands output plain text only. `whoami` has --json but login/logout/drive don't. Per accounts.md §9, login should emit JSON events. |
| B-035 | Add --quiet support for login, logout, whoami, status, drive | P2 | root | Auth/drive commands write to stdout via `fmt.Printf`. File ops use `statusf()` which respects --quiet. Standardize all commands. |
| B-036 | Extract CLI service layer for testability | P2 | root | Root package at 28.1% coverage. All RunE handlers are untested. Need to extract pure logic from I/O and create interfaces for Graph client to enable mock-based testing. Target: 50%+ root package coverage. |
| B-037 | Add chunk upload retry for pre-auth URLs | P2 | `internal/graph/` | `UploadChunk`, `CancelUploadSession`, `QueryUploadSession`, `downloadFromURL` bypass retry. Need lightweight retry wrapper for pre-authenticated URL operations. Important for Phase 4 transfer pipeline. |
| ~~B-038~~ | ~~Document token source context lifetime~~ | ~~P3~~ | ~~`internal/graph/`~~ | **CLOSED**: Doc comments added to `Login()` and `TokenSourceFromPath()` specifying context must outlive TokenSource. PR #40. |
| ~~B-039~~ | ~~Add fsync to graph/auth.go saveToken~~ | ~~P3~~ | ~~`internal/graph/`~~ | **CLOSED**: `tmp.Sync()` added between Write and Close in `saveToken()`. PR #40. |
| ~~B-040~~ | ~~Inject logger into config package for debug observability~~ | ~~P3~~ | ~~`internal/config/`~~ | **CLOSED**: Logger parameter added to `Load()`, `ResolveDrive()`, `ReadEnvOverrides()`. PR #44. |

## Icebox (Deferred / Nice-to-have)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-007 | Cross-drive DriveID handling for shared/remote items | P3 | `internal/graph/` | Verify against real API responses in E2E CI |
| B-008 | Spec inconsistency: chunk_size units (MB vs MiB) | P2 | `docs/design/` | configuration.md says "10MB" default but requires 320 KiB multiples. 10 MB (decimal) is not a multiple. Default changed to "10MiB". Clarify spec. |
| ~~B-009~~ | ~~Extract shared size parsing to shared utility~~ | ~~P3~~ | ~~`internal/`~~ | **CLOSED**: Resolved by exporting `config.ParseSize`. sync/filter.go updated to use it. See B-041. |
| B-011 | Linux CI for diskspace.go field types | P2 | `internal/sync/` | syscall.Statfs_t field types differ between darwin and linux. Verify compilation on linux CI. |
| ~~B-014~~ | ~~Profile name validation (restrict to safe chars)~~ | ~~P3~~ | ~~`internal/config/`~~ | **CLOSED**: Obsolete. Canonical drive IDs (e.g., `personal:toni@outlook.com`) replace arbitrary profile names. Naming format is deterministic, derived from real data via the Graph API. See accounts.md §2. |
| ~~B-015~~ | ~~Upload session resume: query status endpoint~~ | ~~P1~~ | ~~`internal/graph/`~~ | **CLOSED**: `QueryUploadSession` + `ErrRangeNotSatisfiable` added. PR #35. |
| ~~B-016~~ | ~~Include fileSystemInfo in upload session creation~~ | ~~P1~~ | ~~`internal/graph/`~~ | **CLOSED**: `fileSystemInfo` included in `CreateUploadSession` when mtime is non-zero. PR #35. |
| ~~B-017~~ | ~~`Prefer: deltashowremoteitemsaliasid` header for Personal delta~~ | ~~P1~~ | ~~`internal/graph/`~~ | **CLOSED**: Prefer header sent on ALL delta requests for ALL account types. URL-decode normalization added. PR #29. |
| ~~B-018~~ | ~~`.nosync` guard file for unmounted volumes~~ | ~~P2~~ | ~~`internal/sync/`~~ | **CLOSED**: `.nosync` guard check implemented in scanner.go. Returns `ErrNosyncGuard` sentinel. PR #41. |
| ~~B-019~~ | ~~NFC/NFD Unicode normalization for macOS~~ | ~~P2~~ | ~~`internal/sync/`~~ | **CLOSED**: NFC normalization via `golang.org/x/text/unicode/norm`. Dual-path threading (fsRelPath/dbRelPath) for cross-platform correctness. PR #41. |
| B-020 | SharePoint lock check before upload (HTTP 423) | P2 | `internal/graph/` | Check lock status before uploading to SharePoint. Avoid overwriting co-authored documents. See tier1-research/issues-api-inconsistencies.md §8.1. |
| B-021 | Hash fallback chain for missing hashes | P2 | `internal/graph/` | Some Business/SharePoint files lack any hash. Fall back: QuickXorHash -> SHA256 -> size+eTag+mtime. Zero-byte files never have hashes. See tier1-research/issues-api-inconsistencies.md §2.1. |
| ~~B-022~~ | ~~Deferred FK handling for orphaned items~~ | ~~P3~~ | ~~`internal/sync/`~~ | **CLOSED**: `MaterializePath` returns empty string for orphaned items (parent not in DB). Items stored anyway, path recomputed when parent arrives. PR #45. |
| B-023 | `/children` fallback when delta incomplete | P3 | `internal/graph/` | National Cloud Deployments don't support `/delta`. Shared folders on Personal may also return incomplete delta. Implement `/children` traversal fallback. See tier1-research/ref-edge-cases.md §1.4. |
| B-031 | Profile and optimize performance | P3 | all | After feature-complete: CPU/memory/I/O profiling with `pprof`, identify hotspots, optimize critical paths (delta processing, hash computation, transfer pipeline). |
| B-032 | Like-for-like performance benchmarks vs rclone and abraunegg/onedrive | P3 | benchmarks | Create reproducible benchmark suite comparing sync performance (throughput, latency, memory, CPU) against rclone and abraunegg/onedrive on identical workloads. Publish results. |
| ~~B-041~~ | ~~Export `config.ParseSize` to eliminate duplication~~ | ~~P2~~ | ~~`internal/config/`~~ | **CLOSED**: `parseSize` exported as `ParseSize`. sync/filter.go updated to use `config.ParseSize` instead of duplicated `parseSizeFilter`. |
| B-042 | Add `context.Context` parameter to `Store.Checkpoint()` | P3 | `internal/sync/` | Currently uses `context.Background()`. Should accept caller context for cancellation. Low priority — only matters for long-running checkpoint operations. |
| B-043 | Track directories as items in scanner | P3 | `internal/sync/` | sync-algorithm.md §4.5 specifies directories should be tracked in DB for move detection, but scanner currently only tracks files. Needed for folder reconciliation (D1-D7). |
