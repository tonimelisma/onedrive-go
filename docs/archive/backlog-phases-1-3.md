# Backlog

## Active (In Progress)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Ready (Up Next)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Done (Recently Completed)

| ID | Title | Priority | Package | Resolved |
|----|-------|----------|---------|----------|
| B-010 | Build APIClient adapter for pkg/onedrive.Client | P1 | `internal/transfer/` | Redesigned: `*onedrive.Client` satisfies `transfer.APIClient` directly via duck typing. No adapter needed. |
| B-015 | Add ErrGone sentinel error to pkg/onedrive | P2 | `pkg/onedrive/` | Done in increment 3.8. `ErrGone` added to client.go, delta processor uses `errors.Is`. |
| B-022 | pkg/onedrive/ clean-slate rewrite | P1 | `pkg/onedrive/` | Done in increment 3.8. Gutted from ~5,785 to ~1,500 LOC. Deleted 9 files, ~20 unused types, ~38 unused methods. Removed globals, renamed ByID methods, added ErrGone. |

## Icebox (Deferred / Nice-to-have)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-007 | Cross-drive DriveID handling for shared/remote items | P3 | `internal/normalize/` | Verify against real API responses in Phase 2 |
| B-008 | Spec inconsistency: chunk_size units (MB vs MiB) | P2 | `docs/design/` | configuration.md says "10MB" default but requires 320 KiB multiples. 10 MB (decimal) is not a multiple. Default changed to "10MiB". Clarify spec. |
| B-009 | Extract shared size parsing to `internal/units/` | P3 | `internal/` | Both `internal/config/` and `internal/filter/` have size parsers. Could share code. |
| B-011 | Linux CI for diskspace.go field types | P2 | `internal/transfer/` | syscall.Statfs_t field types differ between darwin and linux. Verify compilation on linux CI. |
| B-012 | RecoverSessions should resume rather than cancel | P3 | `internal/transfer/` | Currently cancels stale sessions; full resume needs file unchanged verification. |
| B-013 | Profile-scoped validation error messages | P3 | `internal/config/` | Per-profile override validation reuses global validators; errors say "filter.ignore_marker" not "profile.work.filter.ignore_marker". Low priority cosmetic improvement. |
| B-014 | Profile name validation (restrict to safe chars) | P3 | `internal/config/` | Currently any string accepted as profile name. Consider restricting to `[a-zA-Z0-9_-]` to avoid filesystem issues in DB/token paths. |
| B-016 | ConflictStore adapter for state.Store | P2 | `internal/sync/` | `state.Store.RecordConflict` returns `(string, error)` but `ExecutorConflictStore` expects just `error`. Thin adapter needed for integration. |
| B-017 | Parallel download/upload execution in Executor | P3 | `internal/sync/` | Executor processes downloads/uploads sequentially. Transfer.Manager has worker pools. Could dispatch concurrently for better throughput. |
| B-018 | ConflictHandler sync root awareness | P2 | `internal/sync/` | Conflict handler receives relative paths for rename. When integrated with real filesystem, needs sync root prepended or filesystem wrapper. |
| B-019 | Scanner batch upserts for large sync roots | P3 | `internal/sync/` | Scanner upserts items one at a time. For 100K+ files, batch upserts via `UpsertItems` would improve performance. |
| B-020 | Folder move detection in reconciler | P3 | `internal/sync/` | Move detection only handles files (hash-based). Folder moves need path-based detection. |
| B-021 | DeleteEdit and TypeChange conflict handlers | P2 | `internal/sync/` | ConflictHandler returns "unsupported" for DeleteEdit, TypeChange, CaseConflict. Need real implementations. |
