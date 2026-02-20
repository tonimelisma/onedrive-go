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
| B-027 | Decide conflict resolution UX before building sync engine | P1 | `internal/sync/` | Interactive vs. batch resolution affects schema (4.1), reconciler (4.5), executor (4.7), and conflict handler (4.8). Write a design doc or extend sync-algorithm.md. |
| ~~B-028~~ | ~~Evaluate merging Phase 3 into Phase 4~~ | ~~P2~~ | ~~`internal/config/`~~ | **CLOSED**: Phase 3 completed (PRs #19, #20). Config package now at 95.6% coverage with Resolve(), CLI integration, config show. Config init wizard and migrate deferred to Phase 5. |
| B-029 | Plan Phase 4 in two waves | P1 | `internal/sync/` | Wave 1 (4.1-4.4): state store, delta processor, scanner, filters — independent. Wave 2 (4.5-4.12): re-plan after Wave 1 based on lessons learned. Reconciler, safety, executor, conflict handler are deeply interconnected. |
| B-030 | Review whether `internal/graph/` should be split | P2 | `internal/graph/` | After 1.4-1.6, package will have ~15 files. Assess cohesion vs. size. architecture.md already calls for this review point. |

## Icebox (Deferred / Nice-to-have)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-007 | Cross-drive DriveID handling for shared/remote items | P3 | `internal/graph/` | Verify against real API responses in E2E CI |
| B-008 | Spec inconsistency: chunk_size units (MB vs MiB) | P2 | `docs/design/` | configuration.md says "10MB" default but requires 320 KiB multiples. 10 MB (decimal) is not a multiple. Default changed to "10MiB". Clarify spec. |
| B-009 | Extract shared size parsing to shared utility | P3 | `internal/` | Both config and filter have size parsers. Could share code. |
| B-011 | Linux CI for diskspace.go field types | P2 | `internal/sync/` | syscall.Statfs_t field types differ between darwin and linux. Verify compilation on linux CI. |
| B-014 | Profile name validation (restrict to safe chars) | P3 | `internal/config/` | Currently any string accepted as profile name. Consider restricting to `[a-zA-Z0-9_-]` to avoid filesystem issues in DB/token paths. |
| B-015 | Upload session resume: query status endpoint | P1 | `internal/graph/` | On resume after interruption, query upload session status for accepted byte ranges. Never blindly retry from last local position (HTTP 416). See tier1-research/issues-api-inconsistencies.md §7.1. |
| B-016 | Include fileSystemInfo in upload session creation | P1 | `internal/graph/` | Avoid double-versioning on Business/SharePoint. Never PATCH timestamps separately after upload. See tier1-research/issues-api-inconsistencies.md §7.2. |
| B-017 | `Prefer: deltashowremoteitemsaliasid` header for Personal delta | P1 | `internal/graph/` | Required for shared folder items to appear in delta responses for Personal accounts. See tier1-research/issues-graph-api-bugs.md §11. |
| B-018 | `.nosync` guard file for unmounted volumes | P2 | `internal/sync/` | Check for `.nosync` file in sync dir root before each sync. If found, halt. Prevents "empty mount = delete everything" disaster. Complements big-delete protection. See tier1-research/ref-edge-cases.md §2.10. |
| B-019 | NFC/NFD Unicode normalization for macOS | P2 | `internal/graph/` | macOS APFS uses NFD; Linux uses NFC. Normalize to NFC before all path comparisons. Use `golang.org/x/text/unicode/norm`. See tier1-research/ref-edge-cases.md §7.2. |
| B-020 | SharePoint lock check before upload (HTTP 423) | P2 | `internal/graph/` | Check lock status before uploading to SharePoint. Skip with warning if locked. Prevents overwriting co-authored documents. See tier1-research/issues-api-inconsistencies.md §8.1. |
| B-021 | Hash fallback chain for missing hashes | P2 | `internal/graph/` | Some Business/SharePoint files lack any hash. Fall back: QuickXorHash → SHA256 → size+eTag+mtime. Zero-byte files never have hashes. See tier1-research/issues-api-inconsistencies.md §2.1. |
| B-022 | Deferred FK handling for orphaned items | P3 | `internal/sync/` | Insert items with unknown parents as orphans, reconcile in later pass. Prevents cascading FK constraint failures from API inconsistencies. See tier1-research/issues-api-inconsistencies.md §4.2. |
| B-023 | `/children` fallback when delta incomplete | P3 | `internal/graph/` | National Cloud Deployments don't support `/delta`. Shared folders on Personal may also return incomplete delta. Implement `/children` traversal fallback. See tier1-research/ref-edge-cases.md §1.4. |
