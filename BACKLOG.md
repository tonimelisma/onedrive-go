# Backlog

Historical backlog from Phases 1-4v1 archived in `docs/archive/backlog-v1.md`.

## Active (In Progress)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Ready (Up Next)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-052 | Re-enable E2E tests in CI | P2 | CI | E2E tests disabled in `integration.yml` (PR #76) because local E2E requires active login. Re-enable after E2E test prerequisites are fixed. |

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

## Event-Driven Pivot (Phase 4 v2)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-059 | Implement Phase 4v2.2: Remote Observer | P1 | `internal/sync/` | Delta fetch -> ChangeEvent[]. See roadmap.md. |
| B-058 | Re-enable sync E2E tests in CI after Increment 8 | P2 | CI | Part of 4v2.8. Wire new engine to CLI, write sync E2E tests, re-enable in CI. |

### Closed

| ID | Title | Resolution |
|----|-------|------------|
| B-054 | Remove old `internal/sync/` code after Phase 4v2 complete | **Superseded** — old code removal moved to Increment 0 (B-055), not end of Phase 4v2. |
| B-055 | Increment 0: Delete old sync code, stub sync command, remove tombstone config, update CI | **Done** — Phase 4v2 Increment 0 complete. Old sync engine deleted, sync.go stubbed, tombstone config removed, CI updated, clean-slate orphan branch created. |
| B-056 | Remove `tombstone_retention_days` from config package | **Done** — Completed as part of Increment 0 (B-055). |
| B-057 | Remove sync integration test line from `integration.yml` | **Done** — Completed as part of Increment 0 (B-055). |
| B-053 | Implement Phase 4v2.1: Types + Baseline Schema + BaselineManager | **Done** — PR #78. types.go, migrations, baseline.go, 25 tests, 82.5% coverage. |
