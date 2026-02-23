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
| B-037 | Add chunk upload retry for pre-auth URLs | P2 | `internal/graph/` | `UploadChunk`, `CancelUploadSession`, `QueryUploadSession`, `downloadFromURL` bypass retry. Need lightweight retry wrapper for pre-authenticated URL operations. |
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
| B-069 | Handle locally-deleted folder with no remote delta event | P2 | `internal/sync/` | When a folder is locally deleted but has no remote delta event (unchanged since last token), the planner falls to ED8 (cleanup) instead of propagating deletion remotely. No folder equivalent of EF6. In incremental sync, this orphans the remote folder. |
| B-071 | ConflictRecord missing Name field | P3 | `internal/sync/` | UX convenience for conflict reporting. `path.Base(Path)` suffices but Name from the PathView is cleaner. |
| B-072 | ED1-ED8 missing folder-delete-to-remote (folder EF6) | P2 | `internal/sync/` | Clarifies B-069: when `hasBaseline && !hasLocal && hasRemote && !remoteDeleted`, folders fall to ED4 (recreate locally) instead of propagating the local deletion remotely. Intentional for safety but undocumented. |
| B-074 | Drive identity verification at Engine startup | P2 | `internal/sync/` | Engine (4v2.7) should verify that the configured `driveid.ID` matches the API-reported drive ID at sync start. Catches stale config, renamed drives, or ID format changes. |

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
