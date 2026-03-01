# Test Strategy: onedrive-go

This document specifies the testing approach for onedrive-go — infrastructure, unit tests, integration tests, end-to-end tests, chaos/fault injection, performance benchmarks, regression suites, and CI pipeline configuration. It is designed to ensure that every safety invariant, API quirk, and sync algorithm decision row is covered by automated tests.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Test Infrastructure](#2-test-infrastructure)
3. [Unit Test Strategy](#3-unit-test-strategy)
4. [Property-Based Testing](#4-property-based-testing)
5. [Integration Test Strategy](#5-integration-test-strategy)
6. [E2E Test Strategy](#6-e2e-test-strategy)
7. [Chaos & Fault Injection Testing](#7-chaos--fault-injection-testing)
8. [Performance Testing](#8-performance-testing)
9. [Regression Test Suite](#9-regression-test-suite)
10. [CI Pipeline](#10-ci-pipeline)
11. [Test Organization & Conventions](#11-test-organization--conventions)
- [Appendix A: Test Scenario Matrix](#appendix-a-test-scenario-matrix)
- [Appendix B: Test Fixtures Catalog](#appendix-b-test-fixtures-catalog)
- [Appendix C: Decision Log](#appendix-c-decision-log)

---

## 1. Overview

### 1.1 Testing Philosophy

Tests are a safety net, not a checkbox. The sync engine manages user data — files that may exist nowhere else. A bug in conflict resolution, a missing guard on deletion, or a mishandled API quirk can destroy irreplaceable data. Every safety invariant ([sync-algorithm.md §1.4](sync-algorithm.md)) has at least one test that proves it holds and at least one chaos test that proves it holds under failure.

Guiding principles:

1. **Defense in depth**: Multiple test layers catch different classes of bugs. A unit test catches logic errors. An integration test catches wiring errors. An E2E test catches real-world API behavior. A chaos test catches failure-mode regressions.
2. **Every decision row is a test case**: The sync algorithm's decision matrix (EF1-EF14 files, ED1-ED8 folders) defines the complete behavior space. Every row maps to at least one table-driven test case.
3. **Every bug pattern is a regression test**: 23 known defensive patterns each have a dedicated regression test. If a class of bug happened in production OneDrive clients, we prove it cannot happen here.
4. **Tests run fast by default**: Unit tests complete in seconds. Integration tests in under a minute. Only E2E and stress tests touch the network or take significant time. A developer should be able to run the full local test suite before every commit.

### 1.2 Test Pyramid

```
              ┌─────────┐
              │  E2E    │  Live OneDrive API, real filesystem
              │ (slow)  │  Merge-to-main + nightly
             ┌┴─────────┴┐
             │ Integration │  Mock HTTP, real SQLite, real filter engine
             │  (medium)   │  Every PR
            ┌┴─────────────┴┐
            │  Chaos/Fault   │  Fault injection into integration tests
            │   Injection    │  Every PR
           ┌┴───────────────┴┐
           │    Unit Tests    │  Pure logic, mocked I/O, table-driven
           │    (fast base)   │  Every PR, every local run
          └───────────────────┘
```

### 1.3 Coverage Targets

| Scope | Target | Enforcement |
|-------|--------|-------------|
| **Overall** | ≥ 80% line coverage | CI gate: fail PR if below |
| **Sync engine** (`internal/sync/`) | ≥ 90% line coverage | CI gate: fail PR if below |
| **Graph API client** (`internal/graph/`) | ≥ 95% line coverage | CI gate: fail PR if below |
| **Config** (`internal/config/`) | ≥ 85% line coverage | CI gate: fail PR if below |

Coverage is measured with `go test -coverprofile` and enforced per-package in CI. New code must not decrease coverage in any measured package.

### 1.4 Test Tags

Build tags control which tests run in which context:

| Tag | Meaning | When Run |
|-----|---------|----------|
| (none) | Unit tests | Always (`go test ./...`) |
| `integration` | Integration tests (mock HTTP, real DB) | CI Job 2, local on demand |
| `e2e` | Fast E2E — live OneDrive API tests | Every DoD gate, CI on push to main |
| `e2e,e2e_full` | Full E2E — includes slow tests (large files, bidirectional sync, edge cases) | Nightly CI, manual, pre-release |
| `chaos` | Fault injection tests | CI Job 2 |
| `stress` | Long-running performance tests | Nightly only |
| `benchmark` | Go benchmarks | CI Job 1 (tracked, not gated) |

---

## 2. Test Infrastructure

### 2.1 Frameworks and Libraries

| Purpose | Library | Rationale |
|---------|---------|-----------|
| Test runner | `testing` (stdlib) | Standard, no dependencies, IDE integration |
| Assertions | `github.com/stretchr/testify` (assert/require) | Readable assertions, diff output on failure |
| Mock generation | `github.com/matryer/moq` | Generates mocks from interfaces, type-safe, no runtime reflection |
| Property testing | `testing.F` (Go fuzz) + `pgregory.net/rapid` | Fuzz for input discovery, rapid for property invariants |
| HTTP mocking | `net/http/httptest` (stdlib) | Real HTTP server in-process, no external deps |
| FS mocking | `testing/fstest.MapFS` (stdlib) | In-memory filesystem for observer unit tests |
| Temp dirs | `t.TempDir()` (stdlib) | Auto-cleaned, isolated per test |

### 2.2 Mock Generation

The project uses **consumer-defined interfaces** instead of provider-defined interfaces. The `internal/sync/` package defines 4 narrow interfaces over the concrete `graph.Client` struct, solely to enable mock testing. The CLI (`cmd/onedrive-go/`) uses `graph.Client` directly — no mocks needed for CLI integration tests.

Mocks are generated from these consumer-defined interfaces using `moq`:

```go
//go:generate moq -out mock_graph_test.go . DeltaFetcher ItemClient Downloader Uploader

// Consumer-defined interfaces in internal/sync/ — narrow slices of graph.Client
type DeltaFetcher interface {
    Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
}

type ItemClient interface {
    GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
    ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
    CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
    MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
    DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}

// Downloader streams a remote file by item ID.
type Downloader interface {
    Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

// Uploader uploads a local file, encapsulating the simple-vs-chunked decision
// and upload session lifecycle. content must be an io.ReaderAt for retry safety.
type Uploader interface {
    Upload(
        ctx context.Context, driveID driveid.ID, parentID, name string,
        content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
    ) (*graph.Item, error)
}
```

**Mock regeneration**: CI verifies that generated mocks are up-to-date by running `go generate ./...` and checking for uncommitted changes. A stale mock fails the build.

**Consumer-defined interfaces** (all in `internal/sync/`):

| Interface | Defined In | Mocks Over | Used By |
|-----------|-----------|------------|---------|
| `DeltaFetcher` | `internal/sync` | `graph.Client` | Remote observer |
| `ItemClient` | `internal/sync` | `graph.Client` | Executor (folder create, move, delete), observer |
| `Downloader` | `internal/sync` | `graph.Client` | Executor (downloads) |
| `Uploader` | `internal/sync` | `graph.Client` | Executor (uploads) |

Each interface exists solely to enable mock testing — the concrete `graph.Client` struct implements all of them implicitly.

### 2.3 Property-Based Testing

Property-based testing is applied broadly to algorithm core, API parsing, config validation, and path handling. Two complementary tools:

**Go fuzz (`testing.F`)**: Used for input discovery — finding unexpected inputs that break parsing or cause panics. Fuzz tests are stored alongside unit tests and run in CI with a time budget (30 seconds per fuzz target in CI, unlimited locally).

**rapid (`pgregory.net/rapid`)**: Used for property invariant checking — asserting that properties hold across many generated inputs. rapid provides better generator combinators than `testing.F` for structured data.

```go
func TestProperty_NormalizeIdempotent(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        driveID := rapid.StringMatching(`[a-zA-Z0-9!]{10,20}`).Draw(t, "driveID")
        once := graph.NormalizeDriveID(driveID)
        twice := graph.NormalizeDriveID(once)
        require.Equal(t, once, twice, "normalization must be idempotent")
    })
}
```

### 2.4 Test Database

**Unit tests**: In-memory SQLite via `file::memory:?mode=memory&cache=shared`. Fast, isolated, no cleanup needed. Each test gets a fresh database with migrations applied.

```go
func newTestStore(t *testing.T) *Store {
    t.Helper()
    db := openInMemoryDB(t)
    runMigrations(t, db)
    return NewStore(db)
}
```

**Integration tests**: File-backed SQLite in `t.TempDir()`. Tests WAL behavior, crash recovery, and concurrent access patterns that require a real file.

### 2.5 HTTP Mocking

API response simulation uses `httptest.NewServer` serving canned responses from fixture files:

```go
func newMockGraphAPI(t *testing.T) (*httptest.Server, *RequestLog) {
    t.Helper()
    log := &RequestLog{}
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        log.Record(r)
        switch {
        case strings.Contains(r.URL.Path, "/delta"):
            serveFixture(w, "testdata/delta_response_personal.json")
        case strings.Contains(r.URL.Path, "/content"):
            serveFixture(w, "testdata/file_content.bin")
        default:
            http.NotFound(w, r)
        }
    }))
    t.Cleanup(srv.Close)
    return srv, log
}
```

The `RequestLog` captures all requests for assertion: verifying correct headers, auth tokens not sent to pre-authenticated URLs, Retry-After honored, etc.

### 2.6 Filesystem Test Helpers

**Local Observer unit tests** use `testing/fstest.MapFS` for deterministic filesystem state without touching disk:

```go
fs := fstest.MapFS{
    "Documents/report.pdf": &fstest.MapFile{
        Data:    []byte("content"),
        ModTime: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
    },
    "Photos/image.jpg": &fstest.MapFile{
        Data:    []byte("jpeg-data"),
        ModTime: time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC),
    },
}
```

**Integration and E2E tests** use real temp directories via `t.TempDir()` with helper functions to create file trees:

```go
func createFileTree(t *testing.T, root string, files map[string]string) {
    t.Helper()
    for path, content := range files {
        fullPath := filepath.Join(root, path)
        require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
        require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
    }
}
```

---

## 3. Unit Test Strategy

### 3.1 Sync Engine Core (≥ 90% Coverage)

#### Planner (Pure-Function Testing)

The planner is the heart of the sync algorithm. It is a pure function: `(events + baseline) -> ActionPlan`. Every row of the decision matrix ([sync-algorithm.md §7](sync-algorithm.md)) is a table-driven test case. Because the planner has no I/O, tests are fast and deterministic — no mocks required.

**File decision matrix** — 14 test cases minimum:

```go
func TestPlannerClassifyFile(t *testing.T) {
    tests := []struct {
        name           string
        localHash      string // "" = absent
        remoteHash     string // "" = absent
        baselineLocal  string // "" = no baseline
        baselineRemote string // "" = no baseline
        localDeleted   bool
        remoteDeleted  bool
        expectedAction ActionType
    }{
        // EF1: Both unchanged
        {name: "EF1_unchanged", localHash: "abc", remoteHash: "abc",
         baselineLocal: "abc", baselineRemote: "abc", expectedAction: ActionNone},

        // EF2: Remote changed, local unchanged
        {name: "EF2_remote_changed", localHash: "abc", remoteHash: "def",
         baselineLocal: "abc", baselineRemote: "abc", expectedAction: ActionDownload},

        // EF3: Local changed, remote unchanged
        {name: "EF3_local_changed", localHash: "def", remoteHash: "abc",
         baselineLocal: "abc", baselineRemote: "abc", expectedAction: ActionUpload},

        // EF4: Both changed, same content (convergent edit)
        {name: "EF4_convergent_edit", localHash: "def", remoteHash: "def",
         baselineLocal: "abc", baselineRemote: "abc", expectedAction: ActionUpdateSynced},

        // EF5: Both changed, different content (edit-edit conflict)
        {name: "EF5_edit_edit_conflict", localHash: "def", remoteHash: "ghi",
         baselineLocal: "abc", baselineRemote: "abc", expectedAction: ActionConflict},

        // EF6: Local deleted, remote unchanged
        {name: "EF6_local_deleted", localHash: "", remoteHash: "abc",
         baselineLocal: "abc", baselineRemote: "abc", localDeleted: true, expectedAction: ActionRemoteDelete},

        // EF7: Local deleted, remote changed → re-download
        {name: "EF7_redownload", localHash: "", remoteHash: "def",
         baselineLocal: "abc", baselineRemote: "abc", localDeleted: true, expectedAction: ActionDownload},

        // EF8: Remote deleted, local unchanged
        {name: "EF8_remote_deleted", localHash: "abc", remoteHash: "",
         baselineLocal: "abc", baselineRemote: "abc", remoteDeleted: true, expectedAction: ActionLocalDelete},

        // EF9: Remote deleted, local changed → edit-delete conflict
        {name: "EF9_edit_delete_conflict", localHash: "def", remoteHash: "",
         baselineLocal: "abc", baselineRemote: "abc", remoteDeleted: true, expectedAction: ActionConflict},

        // EF10: Both deleted
        {name: "EF10_both_deleted", localHash: "", remoteHash: "",
         baselineLocal: "abc", baselineRemote: "abc", localDeleted: true, remoteDeleted: true,
         expectedAction: ActionCleanup},

        // EF11: Both created identical file (convergent create)
        {name: "EF11_convergent_create", localHash: "abc", remoteHash: "abc",
         baselineLocal: "", baselineRemote: "", expectedAction: ActionUpdateSynced},

        // EF12: Both created different file (create-create conflict)
        {name: "EF12_create_conflict", localHash: "abc", remoteHash: "def",
         baselineLocal: "", baselineRemote: "", expectedAction: ActionConflict},

        // EF13: New local file
        {name: "EF13_new_local", localHash: "abc", remoteHash: "",
         baselineLocal: "", baselineRemote: "", expectedAction: ActionUpload},

        // EF14: New remote file
        {name: "EF14_new_remote", localHash: "", remoteHash: "abc",
         baselineLocal: "", baselineRemote: "", expectedAction: ActionDownload},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            item := buildTestItem(tt)
            actions := classifyFile(item, ModeBidirectional)
            require.Equal(t, tt.expectedAction, actions[0].Type)
        })
    }
}
```

**Folder decision matrix** — 8 test cases:

| Test | Row | Scenario |
|------|-----|----------|
| `TestPlannerFolder_ED1_exists_both` | ED1 | Folder exists everywhere → no action |
| `TestPlannerFolder_ED2_new_both` | ED2 | Folder exists both sides, no base → adopt |
| `TestPlannerFolder_ED3_remote_only` | ED3 | Remote folder, not local → mkdir |
| `TestPlannerFolder_ED4_local_deleted` | ED4 | Local deleted, remote exists → recreate locally |
| `TestPlannerFolder_ED5_local_only` | ED5 | Local folder, not remote → remote mkdir |
| `TestPlannerFolder_ED6_remote_deleted` | ED6 | Remote deleted → delete locally |
| `TestPlannerFolder_ED7_both_deleted` | ED7 | Both deleted → cleanup |
| `TestPlannerFolder_ED8_remote_moved` | ED8 | Remote moved → local rename |

**Mode-specific planning**: Each sync mode (bidirectional, download-only, upload-only, dry-run) has a test suite verifying that only the correct subset of decision rows are active ([sync-algorithm.md §1.3](sync-algorithm.md)):

| Test | Mode | Verifies |
|------|------|----------|
| `TestPlanner_DownloadOnly_IgnoresLocalChanges` | download-only | EF3, EF6, EF12 produce no action |
| `TestPlanner_UploadOnly_IgnoresRemoteChanges` | upload-only | EF2, EF8, EF13 produce no action |
| `TestPlanner_DryRun_PreviewOnly` | dry-run | All rows produce actions with `DryRun=true` |

#### Conflict Detection

Every conflict type has a dedicated test:

| Test | Conflict Type | Ref |
|------|---------------|-----|
| `TestConflict_EditEdit` | File modified locally and remotely | §2.1 |
| `TestConflict_EditDelete` | File modified locally, deleted remotely | §2.2 |
| `TestConflict_DeleteEdit` | File deleted locally, modified remotely | §2.3 |
| `TestConflict_CreateCreate` | Same-name file created on both sides | §2.4 |
| `TestConflict_TypeChange` | File → folder or folder → file | — |
| `TestConflict_FalseConflict` | Both changed to same content | §2.1 (subset) |

**Conflict naming**: Verify that conflict files follow the pattern `file.conflict-20260217-143052.ext` with correct timestamp formatting.

**Conflict tracking**: Verify that conflict records are written to the conflicts table with correct fields (local_hash, remote_hash, detected_at, resolution, resolved_by).

#### Filter Engine (in `internal/sync/`)

| Test | Scenario |
|------|----------|
| `TestFilter_SkipFiles_Glob` | `*.tmp`, `~*`, `.~*` patterns match correctly |
| `TestFilter_SkipDirs_Glob` | `.git`, `node_modules` excluded |
| `TestFilter_SkipDotfiles` | Dotfile/dotdir exclusion toggle |
| `TestFilter_SkipSymlinks` | Symlink exclusion toggle |
| `TestFilter_SkipSize` | Files over threshold excluded |
| `TestFilter_SyncPaths` | Only listed paths included |
| `TestFilter_Odignore_Negation` | `!important.log` re-includes after `*.log` |
| `TestFilter_Odignore_DoubleStar` | `**/build` matches at any depth |
| `TestFilter_Odignore_DirectoryOnly` | `build/` matches only directories |
| `TestFilter_Odignore_Anchored` | `/root-only` matches only at root |
| `TestFilter_Cascade_MonotonicExclusion` | Each layer can only exclude more, never re-include |
| `TestFilter_Cascade_Order` | sync_paths → config patterns → .odignore |
| `TestFilter_OneDriveNamingRules` | Disallowed names rejected (CON, PRN, .lock, etc.) |
| `TestFilter_ShadowValidation` | Warn when sync_paths and skip patterns conflict |

#### Safety Invariant Unit Tests

Each of the 7 safety invariants ([sync-algorithm.md §1.4](sync-algorithm.md)) has a dedicated unit test proving it holds under normal operation:

| Test | Invariant | What It Proves |
|------|-----------|----------------|
| `TestSafety_S1_NeverDeleteRemoteOnLocalAbsence` | S1 | Items without a baseline entry are never treated as local deletions |
| `TestSafety_S2_NoDeletionsFromIncompleteEnumeration` | S2 | Partial delta fetch produces no deletion actions |
| `TestSafety_S3_AtomicFileWrites` | S3 | Downloads write to `.partial`, rename only after hash match |
| `TestSafety_S4_HashBeforeDelete` | S4 | Local delete checks hash against `baseline.local_hash`; backs up on mismatch |
| `TestSafety_S5_BigDeleteProtection` | S5 | Plans exceeding count OR percentage threshold are rejected |
| `TestSafety_S6_DiskSpaceCheck` | S6 | Downloads are skipped when free space is below threshold |
| `TestSafety_S7_NeverUploadPartialFiles` | S7 | `.partial`, `.tmp`, `~*` files are excluded from upload |

**Big-delete threshold tests** (S5, [sync-algorithm.md §8.1](sync-algorithm.md)):

```go
func TestBigDelete_CountThreshold(t *testing.T) {
    // 1001 deletions with default threshold of 1000 → blocked
    plan := buildPlanWithDeletions(1001)
    err := CheckBigDelete(plan, defaultConfig(), 5000)
    require.ErrorIs(t, err, ErrBigDeleteBlocked)
}

func TestBigDelete_PercentageThreshold(t *testing.T) {
    // 600 deletions out of 1000 total (60%) with default 50% → blocked
    plan := buildPlanWithDeletions(600)
    err := CheckBigDelete(plan, defaultConfig(), 1000)
    require.ErrorIs(t, err, ErrBigDeleteBlocked)
}

func TestBigDelete_MinimumItems(t *testing.T) {
    // 5 deletions out of 8 total (62%) but below min-items (10) → allowed
    plan := buildPlanWithDeletions(5)
    err := CheckBigDelete(plan, defaultConfig(), 8)
    require.NoError(t, err)
}

func TestBigDelete_ORLogic(t *testing.T) {
    // Count below threshold but percentage above → blocked
    plan := buildPlanWithDeletions(500)
    err := CheckBigDelete(plan, defaultConfig(), 800) // 62.5%
    require.ErrorIs(t, err, ErrBigDeleteBlocked)
}
```

#### API Quirk Handling in graph/ (≥ 95% Coverage)

All API quirk normalization is handled internally by `internal/graph/`. The raw `DriveItem` type (in `graph/raw.go`) is converted to clean `graph.Item` values with all quirks resolved. Every known API quirk has a test with a realistic API response fixture:

| Test | Quirk | Ref |
|------|-------|-----|
| `TestNormalize_DriveIdCasing` | Lowercase normalization | api-item-field-matrix §3.2 |
| `TestNormalize_DriveIdTruncation` | 15→16 char zero-padding (Personal) | api-item-field-matrix §3.1 |
| `TestNormalize_DeletedItemMissingName` | Look up name from DB (Business) | ref-edge-cases §1.1 |
| `TestNormalize_DeletedItemMissingSize` | Accept nil size (Personal) | ref-edge-cases §1.1 |
| `TestNormalize_DeletedItemBogusHash` | Discard AAAAAA... hash | ref-edge-cases §1.6 |
| `TestNormalize_MissingCTag` | Accept nil cTag (Business folders, delta) | ref-edge-cases §1.1 |
| `TestNormalize_MissingETag` | Accept nil eTag (Business root) | ref-edge-cases §1.1 |
| `TestNormalize_InvalidTimestamp` | Fallback to current time | ref-edge-cases §1.2 |
| `TestNormalize_FractionalSeconds` | Truncate to whole seconds | ref-edge-cases §1.2 |
| `TestNormalize_DuplicateItemInDelta` | Last occurrence wins | ref-edge-cases §1.4 |
| `TestNormalize_DeletionReordering` | Deletions before creations at same path | sync-algorithm §3.3 |
| `TestNormalize_MissingFileSystemInfo` | Fallback for shared items | ref-edge-cases §1.1 |
| `TestNormalize_OneNotePackage` | Skip package facet items | ref-edge-cases §1.6 |

#### Path Materialization

Tests for the path construction and cascade update logic ([data-model.md §10](data-model.md)):

| Test | Scenario |
|------|----------|
| `TestPath_RootChild` | `/Documents` has depth 1 |
| `TestPath_NestedFile` | `/Documents/Work/report.pdf` built from parent chain |
| `TestPath_RenameParent_CascadeChildren` | Renaming a folder updates all descendant paths |
| `TestPath_MoveFolder_CascadeChildren` | Moving a folder updates all descendant paths |
| `TestPath_CrossDriveReference` | Shared items with different drive_id |
| `TestPath_Roundtrip` | construct → split → reconstruct = identity |

#### Move/Rename Detection

| Test | Scenario | Ref |
|------|----------|-----|
| `TestMove_RemoteDetection_SameItemNewParent` | Item reappears with different parent_id | sync-algorithm §7.5 |
| `TestMove_RemoteDetection_Rename` | Item reappears with different name, same parent | sync-algorithm §7.5 |
| `TestMove_RemoteDetection_ViaBaseline` | Baseline lookup detects item moved to new location | sync-algorithm §7.5 |
| `TestMove_LocalDetection_UniqueHash` | File deleted + same-hash file created → move | sync-algorithm §7.5 |
| `TestMove_LocalDetection_AmbiguousHash` | Multiple new files with same hash → delete+create | sync-algorithm §7.5 |
| `TestMove_LocalDetection_NoBaseline` | Deleted file without baseline entry → not a move | sync-algorithm §7.5 |

### 3.2 Config System (≥ 85% Coverage)

| Test | Scenario |
|------|----------|
| `TestConfig_ValidTOML` | Complete valid config parses without error |
| `TestConfig_MinimalTOML` | Config with only required fields |
| `TestConfig_UnknownKey_Fatal` | Unknown key → fatal error with closest-match suggestion |
| `TestConfig_DriveOverride_FullReplace` | `["business:alice@contoso.com"]` drive-level settings replace globals entirely |
| `TestConfig_DriveOverride_Inherit` | Drive without overrides inherits global |
| `TestConfig_MultipleDrives` | Multiple drive sections coexist |
| `TestConfig_DefaultValues` | Every option has the documented default |
| `TestConfig_EnvOverride` | `ONEDRIVE_GO_CONFIG`, `ONEDRIVE_GO_DRIVE` |
| `TestConfig_CLIFlagOverride` | CLI flags override config file values |
| `TestConfig_Precedence` | defaults → config → env → CLI flags |
| `TestConfig_Validation_ChunkSize` | Must be 320KiB multiple |
| `TestConfig_Validation_PollInterval` | Minimum 5 minutes |
| `TestConfig_Validation_AzureRequiresTenant` | azure_ad_endpoint requires azure_tenant_id |
| `TestConfig_Validation_MutuallyExclusive` | download_only + upload_only → error |
| `TestConfig_Migration_Abraunegg` | abraunegg config.toml → our format |
| `TestConfig_Migration_Rclone` | rclone.conf OneDrive section → our format |
| `TestConfig_HotReload_FilterChange` | New filter config → stale files detected |
| `TestConfig_HotReload_BandwidthImmediate` | Bandwidth schedule change takes effect immediately |
| `TestConfig_HotReload_NonReloadable` | sync_dir change → restart required error |
| `TestConfig_MalformedTOML` | Syntax error → clear error message with line number |

### 3.3 Database Layer (in `internal/sync/`, ≥ 90% Coverage)

| Test | Scenario |
|------|----------|
| `TestBaseline_Upsert_Insert` | New baseline entry inserted |
| `TestBaseline_Upsert_Update` | Existing baseline entry updated |
| `TestBaseline_Upsert_CompositeKey` | Same item_id with different drive_id = different rows |
| `TestBaseline_Get_NotFound` | Missing entry returns nil, no error |
| `TestBaseline_Load_InMemory` | Full baseline loaded into ByPath and ByID maps |
| `TestBaseline_Delete_RemovesRow` | Deletion removes the baseline row |
| `TestBaseline_Commit_Atomic` | All outcomes + delta token committed in single transaction |
| `TestBaseline_Commit_PerSideHashes` | local_hash and remote_hash stored independently |
| `TestDB_DeltaToken_SaveLoad` | Round-trip delta token per drive |
| `TestDB_DeltaToken_Delete` | HTTP 410 → token deleted |
| `TestDB_Conflict_Create` | Conflict record with all fields |
| `TestDB_Conflict_Resolve` | Resolution updates resolution + resolved_by + resolved_at |
| `TestDB_Conflict_ListUnresolved` | Only unresolved conflicts returned |
| `TestDB_Conflict_History` | Resolution history JSON appended |
| `TestDB_StaleFiles_Create` | Stale file record after filter change |
| `TestDB_StaleFiles_Dispose` | Mark stale files as kept or deleted |
| `TestDB_UploadSession_SaveResume` | Upload session round-trip |
| `TestDB_UploadSession_Expire` | Expired sessions cleaned up |
| `TestDB_Migration_Forward` | Every migration applies forward cleanly in sequence |
| `TestDB_ForeignKeys` | Referential integrity enforced |
| `TestDB_WALCheckpoint` | Checkpoint reduces WAL file size |

### 3.4 Transfer Manager (in `internal/sync/`)

| Test | Scenario |
|------|----------|
| `TestTransfer_SimpleUpload` | File < 4MB → single PUT |
| `TestTransfer_ResumableUpload` | File ≥ 4MB → create session + fragments |
| `TestTransfer_FragmentAlignment` | Fragment size is 320KiB multiple |
| `TestTransfer_FragmentAlignment_LastChunk` | Last fragment may be smaller |
| `TestTransfer_HashVerification_Download` | QuickXorHash verified after download |
| `TestTransfer_HashVerification_Upload` | Server response hash recorded |
| `TestTransfer_HashMismatch_Download` | Hash mismatch → delete .partial, retry |
| `TestTransfer_BandwidthLimit` | Transfer rate capped at configured limit |
| `TestTransfer_BandwidthSchedule` | Rate changes by time-of-day |
| `TestTransfer_PreAuthURL_NoBearer` | Fragment uploads do NOT send Bearer token |
| `TestTransfer_SessionResume` | Resume from bytes_uploaded after restart |
| `TestTransfer_SessionExpired` | Expired session → create new session |
| `TestTransfer_AtomicDownload` | Write .partial → verify hash → rename |
| `TestMerge_EnrichmentNoAction` | After upload with enrichment (LocalHash != RemoteHash in baseline), next cycle produces no action |
| `TestMerge_EnrichmentThenLocalChange` | User modifies enriched file → local change detected → upload |
| `TestMerge_EnrichmentThenRemoteChange` | Remote change on enriched file → download |
| `TestMerge_EnrichmentThenBothChange` | Both sides change → conflict correctly detected |
| `TestUpload_EnrichmentLogsInfo` | Post-upload hash mismatch on SharePoint logs INFO, does not trigger download |

---

## 4. Property-Based Testing

### 4.1 Algorithm Properties

| Property | Generator | Invariant |
|----------|-----------|-----------|
| **Convergence** | Random item states (local_hash, remote_hash, baseline) | After plan + execute on both sides, local_hash == remote_hash |
| **Idempotence** | Random synced items (local == remote == baseline) | Planner produces zero actions |
| **Monotonic exclusion** | Random filter chains (sync_paths + skip patterns + .odignore) | Adding any filter never increases the included set |
| **Path roundtrip** | Random valid path strings | `MaterializePath(ParsePath(p)) == p` |
| **Action plan ordering** | Random action plans | Folder creates before children, folder deletes after children |

**Convergence property** (the most important):

```go
func TestProperty_Convergence(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        // Generate random initial states
        items := rapid.SliceOfN(genItem(), 1, 100).Draw(t, "items")

        // Plan
        plan := Plan(items, ModeBidirectional)

        // Simulate execution (apply actions to items)
        applied := simulateExecution(items, plan)

        // After execution, all items should be in sync
        for _, item := range applied {
            if item.LocalHash != "" && item.RemoteHash != "" {
                require.Equal(t, item.LocalHash, item.RemoteHash,
                    "item %s not converged", item.Path)
            }
        }
    })
}
```

### 4.2 Parsing Properties

| Property | Generator | Invariant |
|----------|-----------|-----------|
| **API response no-panic** | Random valid JSON objects | `graph.ParseItem(json)` never panics, returns `graph.Item` or error |
| **Timestamp parsing** | Random strings + valid ISO 8601 + edge cases | `ParseTimestamp(s)` returns valid time or fallback, never panics |
| **Config parsing** | Random valid TOML strings | `ParseConfig(toml)` returns config or clear error, never panics |
| **TOML roundtrip** | Generated valid config structs | `Parse(Marshal(config)) == config` |

**Fuzz targets** (Go native fuzzing):

```go
func FuzzParseItem(f *testing.F) {
    // Seed with real API responses from fixtures
    f.Add(loadFixture("testdata/delta_item_personal.json"))
    f.Add(loadFixture("testdata/delta_item_business.json"))
    f.Add(loadFixture("testdata/delta_item_deleted.json"))

    f.Fuzz(func(t *testing.T, data []byte) {
        // Must never panic — returns graph.Item or error
        _, _ = graph.ParseItem(data)
    })
}

func FuzzParseTimestamp(f *testing.F) {
    f.Add("2026-01-15T10:30:00Z")
    f.Add("")
    f.Add("not-a-timestamp")
    f.Add("2026-01-15T10:30:00.123456789Z")

    f.Fuzz(func(t *testing.T, s string) {
        ts := ParseTimestamp(s)
        // Must always return a valid time (fallback to now)
        require.False(t, ts.IsZero())
    })
}
```

### 4.3 Data Integrity Properties

| Property | Generator | Invariant |
|----------|-----------|-----------|
| **QuickXorHash determinism** | Random byte slices | `Hash(data) == Hash(data)` (same content → same hash) |
| **QuickXorHash streaming** | Random byte slices, random chunk sizes | `StreamHash(chunks) == Hash(concat(chunks))` |
| **Normalization idempotence** | Random driveId strings | `graph.NormalizeDriveID(graph.NormalizeDriveID(x)) == graph.NormalizeDriveID(x)` |
| **DriveId case-insensitive equality** | Pairs of same-driveId with random casing | `graph.NormalizeDriveID(upper) == graph.NormalizeDriveID(lower)` |
| **Path normalization** | Random paths with mixed separators | `NormalizePath(p)` uses `/`, no trailing slash, no `//` |

---

## 5. Integration Test Strategy

Integration tests wire together multiple real components with mock external boundaries. They use `//go:build integration` tags.

### 5.1 Sync Pipeline Integration

These tests run the full sync pipeline — delta fetch through execution — with a mock HTTP server and a real SQLite database.

**Test setup**:
- Mock Graph API server (`httptest.NewServer`) serving realistic delta response fixtures
- Real SQLite database (file-backed in `t.TempDir()`) — state management is part of `internal/sync/`
- Real filter engine with configured patterns — filtering is part of `internal/sync/`
- `graph.Client` pointed at mock server (quirk handling exercised end-to-end)
- Real planner (pure function — no mocks needed)
- Mock executor (captures actions without performing transfers)

**Core scenarios**:

| Test | Scenario | Verifies |
|------|----------|----------|
| `TestPipeline_InitialSync_EmptyLocal` | No delta token, remote has 50 files | All 50 files queued for download (EF14) |
| `TestPipeline_SteadyState_NoChanges` | Delta returns empty, local unchanged | Zero actions produced |
| `TestPipeline_RemoteEdit` | Delta returns 1 changed file | 1 download action (EF2) |
| `TestPipeline_LocalEdit` | Local file mtime changed, hash differs | 1 upload action (EF3) |
| `TestPipeline_BidirectionalChanges` | 3 remote edits + 2 local edits + 1 conflict | 3 downloads + 2 uploads + 1 conflict |
| `TestPipeline_RemoteDelete` | Delta returns deleted item | 1 local delete action (EF8), hash-before-delete verified |
| `TestPipeline_LocalDelete` | Synced file missing from filesystem | 1 remote delete action (EF6), S1 verified |
| `TestPipeline_Move_Remote` | Item reappears with different parent_id | 1 local move action |
| `TestPipeline_Move_Local` | File deleted + same-hash file at new path | 1 remote move action |
| `TestPipeline_FilterExcludesFile` | Remote file matches skip pattern | File skipped, no download |
| `TestPipeline_BatchCheckpoint` | Delta returns 1500 items | 3 batch checkpoints at 500-item boundaries |
| `TestPipeline_DeltaTokenSaved` | Complete delta response with deltaLink | Token saved only after final page |
| `TestPipeline_DeltaTokenNotSaved_PartialFetch` | Delta fetch fails mid-page | Token NOT saved |
| `TestPipeline_DryRun` | Various changes pending | All actions are preview-only, no side effects |

**Multi-file conflict scenarios**:

| Test | Scenario |
|------|----------|
| `TestPipeline_MultiConflict_EditEdit` | 3 files with edit-edit conflicts |
| `TestPipeline_MultiConflict_MixedTypes` | 1 edit-edit + 1 edit-delete + 1 create-create |
| `TestPipeline_ConflictTracking_Written` | Conflicts recorded in DB with correct hashes |

### 5.2 Config Integration

| Test | Scenario |
|------|----------|
| `TestConfigIntegration_LoadAndInitDrives` | Load config → init 3 drives → each gets its own DB |
| `TestConfigIntegration_FilterEngineFromConfig` | Config patterns → sync/ filter engine evaluates correctly |
| `TestConfigIntegration_CrossDriveValidation` | Two drives with same sync_dir → error |
| `TestConfigIntegration_HotReload_SIGHUP` | Send SIGHUP → config re-read → filter engine re-init |
| `TestConfigIntegration_HotReload_StaleFiles` | Filter change → stale files detected → table populated |
| `TestConfigIntegration_HotReload_BandwidthImmediate` | Bandwidth change → transfer manager updated immediately |
| `TestConfigIntegration_HotReload_NonReloadable` | sync_dir change → error logged, restart required |
| `TestConfigIntegration_Wizard_Interactive` | Simulated wizard flow → valid config written |
| `TestConfigIntegration_Wizard_DetectsAbraunegg` | abraunegg config exists → migration offered |

### 5.3 Database Integration

| Test | Scenario |
|------|----------|
| `TestDBIntegration_SchemaMigration_Forward` | Run all migrations forward in sequence, verify final schema |
| `TestDBIntegration_ConcurrentReaders` | 8 reader goroutines + 1 writer under WAL mode |
| `TestDBIntegration_WALCheckpoint_BoundsSize` | After 500 writes + checkpoint, WAL < 64MiB |
| `TestDBIntegration_CrashRecovery_MidBatch` | Kill writer mid-transaction → reopen → DB consistent |
| `TestDBIntegration_CrashRecovery_DeltaToken` | Kill after items applied but before token saved → items reprocessed |
| `TestDBIntegration_LargeDataset` | Insert 100K items → query performance within bounds |
| `TestDBIntegration_BaselineCommit_AtomicOutcomes` | Multiple outcomes + delta token committed atomically |
| `TestDBIntegration_PathCascade_10Levels` | Rename root folder → all 10 levels of descendants updated |

---

## 6. E2E Test Strategy

E2E tests run against a live OneDrive account. They use `//go:build e2e` tags and are skipped when credentials are unavailable.

### 6.1 Infrastructure

**Test accounts**:

| Drive Type | Environment | Frequency | Status |
|-------------|-------------|-----------|--------|
| Personal (free) | CI (GitHub Actions) | Every merge to main + nightly | Active |
| Business | CI (GitHub Actions) | Nightly | Backlog — add after core E2E is stable (~$5/month for M365 Business Basic) |
| SharePoint | CI (GitHub Actions) | Nightly | Backlog — same M365 subscription covers SharePoint |
| Shared | CI (GitHub Actions) | Nightly | Backlog — reuses primary account token, no separate auth needed |

All four drive types (Personal, Business, SharePoint, Shared) will run in CI. Personal is free and runs from day one. Business and SharePoint use the same Microsoft 365 Business Basic subscription (~$5/month) and will be added to the nightly CI job once the Personal E2E suite is stable and the core sync engine is functional. Shared drives reuse the primary account's token (no separate authentication). Until then, Business, SharePoint, and Shared drive quirks are covered by unit tests with realistic API response fixtures (§3.1 Normalizer, §9.2 API Quirk Regression Tests).

**Credential management**:

CI credentials use **Azure Key Vault + OIDC federation** for OAuth refresh token storage. OIDC means GitHub Actions authenticates to Azure without any stored credentials — the trust is federated via short-lived JWTs.

Setup:
1. Azure OIDC service principal (`onedrive-go-ci-github-oidc`) with federated credentials scoped to `repo:tonimelisma/onedrive-go:ref:refs/heads/main`
2. Azure Key Vault (`kv-onedrivego-ci`) with RBAC authorization; SP has "Key Vault Secrets Officer" role
3. GitHub repository variables: `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, `AZURE_KEY_VAULT_NAME`, `ONEDRIVE_TEST_DRIVES` (non-sensitive identifiers)
4. CI loads tokens via `az keyvault secret download --file` (token never in stdout/logs)
5. After tests, CI saves rotated tokens back via `az keyvault secret set --file` (with JSON validation)
6. Per-drive secrets follow the naming convention: canonical drive ID with `:`, `@`, and `.` replaced by `-`, prefixed with `onedrive-oauth-token-`. E.g., `personal:testitesti18@outlook.com` → `onedrive-oauth-token-personal-testitesti18-outlook-com`

**Key Vault secret naming derivation**:
```bash
# Input: canonical drive ID (e.g., "personal:testitesti18@outlook.com")
# Transform: sed 's/[:@.]/-/g'
# Result: "onedrive-oauth-token-personal-testitesti18-outlook-com"
echo "onedrive-oauth-token-$(echo 'personal:testitesti18@outlook.com' | sed 's/[:@.]/-/g')"
```

**Token file naming derivation**:
```bash
# Input: canonical drive ID (e.g., "personal:testitesti18@outlook.com")
# Transform: replace first ":" with "_"
# Result: "token_personal_testitesti18@outlook.com.json"
# Location: ~/.local/share/onedrive-go/
```

Token bootstrap (one-time per drive):
```bash
# 1. Login to get a token file
go run . login --drive personal:testitesti18@outlook.com

# 2. Upload token to Key Vault
az keyvault secret set \
  --vault-name kv-onedrivego-ci \
  --name onedrive-oauth-token-personal-testitesti18-outlook-com \
  --file ~/.local/share/onedrive-go/token_personal_testitesti18@outlook.com.json \
  --content-type application/json

# 3. Set the GitHub repository variable
gh variable set ONEDRIVE_TEST_DRIVES --body "personal:testitesti18@outlook.com"
```

When tokens expire completely (90 days of inactivity), re-bootstrap using the same steps.

**Who manages secrets**: The AI orchestrator (Claude) has `az` CLI access and **should** manage Key Vault secrets directly. This includes creating, updating, rotating, and verifying secrets — not just human-only operations. When CI changes affect token paths or secret naming, the orchestrator should update Key Vault secrets and GitHub variables as part of the same increment, then verify CI passes before declaring done. The human only needs to intervene for one-time Azure infrastructure setup (service principal, RBAC, federated credentials) and interactive `login` flows that require a browser.

**CI auth failure handling**: If the integration job fails authentication, it prints re-bootstrap instructions. Integration tests run only on push to main, nightly, and manual dispatch — never on PRs.

**Local CI validation** (before pushing changes that affect CI):

When changing token paths, secret naming, environment variables, or workflow logic, use `scripts/validate-ci-locally.sh` to validate locally before pushing:

```bash
# Mirrors integration.yml exactly: downloads token from Key Vault, runs
# integration + E2E tests, saves rotated token back.
# Prerequisites: az CLI logged in, gh CLI authenticated, go, jq.
./scripts/validate-ci-locally.sh [DRIVE]

# Examples:
./scripts/validate-ci-locally.sh                                      # auto-detects from ONEDRIVE_TEST_DRIVES or gh variable
./scripts/validate-ci-locally.sh personal:testitesti18@outlook.com    # explicit drive
```

The script performs all 9 steps of the CI workflow locally (derive names, verify Azure access, download token, validate structure, install token, whoami, integration tests, E2E tests, save rotated token back). This catches issues like wrong secret names, missing tokens, or broken token paths before they reach GitHub Actions.

**Test isolation**: Each test creates a timestamped directory on OneDrive (`/onedrive-go-e2e-test-20260217-143052-{random}/`) and cleans it up on teardown. Tests run serially to avoid rate limiting.

```go
func setupE2ETest(t *testing.T) (client *Client, remotePath string) {
    t.Helper()
    if os.Getenv("ONEDRIVE_E2E_ENABLED") == "" {
        t.Skip("E2E tests disabled (set ONEDRIVE_E2E_ENABLED=1)")
    }
    client = newAuthenticatedClient(t)
    remotePath = fmt.Sprintf("/onedrive-go-e2e-%s-%s",
        time.Now().Format("20060102-150405"),
        randomHex(4))
    t.Cleanup(func() {
        _ = client.DeleteFolder(context.Background(), remotePath)
    })
    return client, remotePath
}
```

### 6.2 Core E2E Scenarios

| Test | Scenario | Verifies |
|------|----------|----------|
| `TestE2E_InitialSync_EmptyLocal` | Create 5 files remotely → sync → verify local | Files downloaded, hashes match, DB populated |
| `TestE2E_Upload_NewFile` | Create local file → sync → verify remote exists | Upload path, hash verification, DB updated |
| `TestE2E_Download_NewFile` | Create remote file via API → sync → verify local | Download path, atomic write, hash match |
| `TestE2E_Bidirectional` | Create 2 local + 2 remote → sync → both sides complete | All 4 files present on both sides |
| `TestE2E_Conflict_EditEdit` | Edit same file locally and remotely → sync | Conflict file created, conflict tracking entry |
| `TestE2E_Delete_LocalPropagates` | Sync → delete local file → sync again → verify remote gone | Remote deletion propagated |
| `TestE2E_Delete_RemotePropagates` | Sync → delete remote file via API → sync again → verify local gone | Local deletion, hash-before-delete |
| `TestE2E_MoveRename` | Sync → rename local file → sync → verify remote renamed | Move detection, single remote move (not delete+create) |
| `TestE2E_LargeFile` | Upload 5MB file (resumable session) → sync → verify hash | Session creation, fragment upload, hash verification |
| `TestE2E_Subfolder_Create` | Create nested folder structure locally → sync → verify remote | Folder creation order (parent before child) |
| `TestE2E_DownloadOnly` | Changes on both sides → sync --download-only | Only remote changes applied, local changes preserved |
| `TestE2E_UploadOnly` | Changes on both sides → sync --upload-only | Only local changes pushed, remote changes ignored |
| `TestE2E_DryRun` | Pending changes → sync --dry-run | No actual transfers, report shows planned actions |
| `TestE2E_EmptySync` | Both sides unchanged → sync | Zero actions, fast completion |

### 6.3 Account-Type-Specific E2E

These tests run in the nightly CI job once Business/SharePoint accounts are provisioned (backlog). Until then, they can be run manually with local credentials:

| Test | Account | Scenario | Quirk Verified |
|------|---------|----------|----------------|
| `TestE2E_Business_NoCTag` | Business | Sync folder → verify cTag absence handled | cTag omitted for folders |
| `TestE2E_Business_DeletedItemNoName` | Business | Delete remote file → sync → verify name from DB | Name field missing on deletion |
| `TestE2E_SharePoint_Enrichment` | SharePoint | Upload .pptx → sync → verify per-side baselines prevent spurious actions, no infinite loop | Server-side enrichment handled via per-side hash baselines |
| `TestE2E_Personal_DriveIdNormalization` | Personal | Sync → verify 15-char driveId handled | Zero-padding normalization |
| `TestE2E_Personal_HeicHashMismatch` | Personal | Sync .heic file → verify graceful handling | Known iOS API bug |
| `TestE2E_Shared_FolderSync` | Shared | Add shared folder shortcut → sync → verify items | Shortcut-based delta, cross-drive resolution |
| `TestE2E_Personal_VaultExclusion` | Personal | Sync with vault items → verify vault skipped | `specialFolder.name == "vault"` detection |
| `TestE2E_Personal_DisplayNameMatching` | Personal | Use `--drive` with display name → verify resolution | 3-tier `--drive` matching priority |

### 6.4 Eventual Consistency Polling

Microsoft Graph API has eventual consistency gaps: items created or uploaded via the API may not be immediately visible via path-based queries (`GetItemByPath`, `ListChildrenByPath`). This manifests as 404 errors on read-after-write patterns in E2E tests.

**Polling helpers** (defined in `e2e/e2e_test.go`) replace fatal assertions in write-then-read patterns:

| Helper | Use Case |
|--------|----------|
| `pollCLIContains(t, expected, timeout, args...)` | Poll until CLI output contains `expected` string (no config file) |
| `pollCLIWithConfigContains(t, cfgPath, expected, timeout, args...)` | Same, with custom config file |
| `pollCLISuccess(t, timeout, args...)` | Poll until CLI exits successfully (no config file) |
| `pollCLIWithConfigSuccess(t, cfgPath, timeout, args...)` | Same, with custom config file |

All helpers use exponential backoff via `pollBackoff()`: 500ms, 1s, 2s, 4s (capped). Default timeout: 30s (`pollTimeout`).

**When to use polling vs direct assertion**:
- **Polling**: After any write operation (put, mkdir, sync --upload-only) when the next step reads from the API by path (ls, stat, get, rm)
- **Direct assertion**: For operations that don't involve a write-then-read gap (e.g., local file checks, sync --download-only reading locally, delta-based operations)

See also: DP-11 in [decisions.md](decisions.md) and LEARNINGS.md §6 "Microsoft Graph eventual consistency".

---

## 7. Chaos & Fault Injection Testing

Chaos tests prove that safety invariants hold under failure conditions. They use `//go:build chaos` tags and run in CI Job 2 alongside integration tests. Fault injection from day one.

### 7.1 Safety Invariant Chaos Tests

One test per safety invariant, each simulating the specific failure mode that the invariant guards against:

**S1: Failed download must not cause remote deletion**

```go
func TestChaos_S1_FailedDownloadNeverDeletesRemote(t *testing.T) {
    // Setup: remote file exists, no local copy yet (download pending)
    store := newTestStore(t)
    item := insertRemoteOnlyItem(store, "doc.pdf", "hash123")

    // Simulate: download fails (network error)
    executor := newExecutorWithFailingDownloader(t)
    plan := ActionPlan{Downloads: []Action{{Type: ActionDownload, Item: item}}}
    executor.Execute(context.Background(), plan)

    // Verify: item NOT marked as locally deleted
    // Verify: no remote delete action generated on next plan
    actions := Plan(views, ModeBidirectional)
    for _, a := range actions {
        require.NotEqual(t, ActionRemoteDelete, a.Type,
            "S1 violation: failed download must not trigger remote delete")
    }
}
```

**S2: Incomplete delta must not drive deletions**

```go
func TestChaos_S2_IncompleteDeltaNoDeletions(t *testing.T) {
    // Setup: 100 files synced, all present locally
    store := newTestStoreWith100SyncedFiles(t)

    // Simulate: delta fetch returns only 30 items then HTTP 410
    mockAPI := newMockGraphAPI(t)
    mockAPI.OnDelta(func(w http.ResponseWriter, r *http.Request) {
        serveDeltaPage(w, 30, false) // 30 items, no deltaLink
        // Next request → 410
        mockAPI.OnDelta(func(w http.ResponseWriter, r *http.Request) {
            w.WriteHeader(http.StatusGone)
        })
    })

    // Run sync (should handle 410 gracefully)
    engine := newSyncEngine(store, mockAPI.URL)
    engine.RunOnce(context.Background())

    // Verify: no local deletions for the 70 "missing" items
    deletedCount := countDeletedItems(store)
    require.Zero(t, deletedCount,
        "S2 violation: incomplete delta must not generate deletions")
}
```

**S3: Interrupted download must not corrupt existing file**

```go
func TestChaos_S3_InterruptedDownloadNoCorruption(t *testing.T) {
    syncDir := t.TempDir()
    existingContent := "original content"
    writeFile(t, syncDir, "report.pdf", existingContent)

    // Simulate: download starts, writes half the data, then fails
    mockAPI := newMockGraphAPI(t)
    mockAPI.OnDownload("report.pdf", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Length", "1000")
        w.Write([]byte("partial"))
        // Connection dies here (handler returns)
    })

    executor := newExecutor(mockAPI.URL, syncDir)
    executor.ExecuteDownload(context.Background(), downloadAction("report.pdf"))

    // Verify: original file unchanged
    content := readFile(t, syncDir, "report.pdf")
    require.Equal(t, existingContent, content,
        "S3 violation: interrupted download corrupted existing file")

    // Verify: .partial file exists (or was cleaned up)
    _, err := os.Stat(filepath.Join(syncDir, "report.pdf.partial"))
    // Either .partial exists (for resume) or was cleaned up — either is fine
    // The ONLY unacceptable state is original file being corrupted
}
```

**S4: Hash-before-delete prevents deletion of modified files**

```go
func TestChaos_S4_HashBeforeDeleteGuard(t *testing.T) {
    syncDir := t.TempDir()
    store := newTestStore(t)

    // Setup: file synced with hash "aaa"
    writeFile(t, syncDir, "notes.txt", "original")
    insertSyncedItem(store, "notes.txt", "aaa")

    // User edits the file locally (hash is now "bbb")
    writeFile(t, syncDir, "notes.txt", "user edited this")

    // Remote deletion arrives
    markRemoteDeleted(store, "notes.txt")

    // Execute local delete
    executor := newExecutor(nil, syncDir)
    plan := Plan(views, ModeBidirectional)
    executor.Execute(context.Background(), plan)

    // Verify: file NOT deleted (hash mismatch with baseline.local_hash)
    require.FileExists(t, filepath.Join(syncDir, "notes.txt"),
        "S4 violation: modified file was deleted despite hash mismatch")

    // Verify: conflict file created OR original preserved
    // Verify: conflicts table entry created
    conflicts := store.ListUnresolvedConflicts()
    require.NotEmpty(t, conflicts, "S4: conflict should be recorded")
}
```

**S5: Big-delete protection blocks mass deletion**

```go
func TestChaos_S5_BigDeleteProtection(t *testing.T) {
    store := newTestStore(t)

    // Setup: 2000 synced files
    for i := 0; i < 2000; i++ {
        insertSyncedItem(store, fmt.Sprintf("file%04d.txt", i), fmt.Sprintf("hash%d", i))
    }

    // Simulate: all files appear deleted remotely (e.g., unmounted volume)
    for i := 0; i < 2000; i++ {
        markRemoteDeleted(store, fmt.Sprintf("file%04d.txt", i))
    }

    plan := Plan(views, ModeBidirectional)

    // Verify: big-delete protection blocks execution
    err := CheckBigDelete(plan, defaultConfig(), 2000)
    require.ErrorIs(t, err, ErrBigDeleteBlocked,
        "S5 violation: mass deletion was not blocked")
}
```

**S6: Disk space check prevents download when disk is full**

```go
func TestChaos_S6_DiskSpaceCheck(t *testing.T) {
    // Mock disk space checker to report low space
    checker := &mockDiskChecker{freeBytes: 100 * 1024} // 100KB free
    executor := newExecutorWithDiskChecker(checker)

    // Try to download a 50MB file
    action := downloadAction("large.zip")
    action.Item.Size = 50 * 1024 * 1024

    result := executor.ExecuteDownload(context.Background(), action)

    // Verify: download skipped, not attempted
    require.ErrorIs(t, result, ErrInsufficientDiskSpace,
        "S6 violation: download attempted with insufficient disk space")
}
```

**S7: Partial/temporary files never uploaded**

```go
func TestChaos_S7_NeverUploadPartialFiles(t *testing.T) {
    syncDir := t.TempDir()

    // Create various temp/partial files in sync dir
    writeFile(t, syncDir, "document.pdf.partial", "incomplete download")
    writeFile(t, syncDir, "~$document.docx", "office lock file")
    writeFile(t, syncDir, ".~lock.file#", "libreoffice lock")
    writeFile(t, syncDir, "backup.tmp", "temp file")
    writeFile(t, syncDir, "legitimate.txt", "real file")

    // Observe local filesystem
    observer := newLocalObserver(syncDir, defaultFilter())
    items := observer.FullScan(context.Background(), baseline)

    // Verify: only legitimate.txt was picked up
    paths := extractPaths(items)
    require.NotContains(t, paths, "document.pdf.partial")
    require.NotContains(t, paths, "~$document.docx")
    require.NotContains(t, paths, ".~lock.file#")
    require.NotContains(t, paths, "backup.tmp")
    require.Contains(t, paths, "legitimate.txt")
}
```

### 7.2 Network Fault Injection

| Test | Fault | Expected Behavior |
|------|-------|-------------------|
| `TestChaos_Network_TimeoutMidTransfer` | Connection drops mid-download | Retry with backoff, .partial preserved for resume |
| `TestChaos_Network_HTTP429` | API returns 429 + Retry-After: 120 | Wait 120s (simulated), then retry successfully |
| `TestChaos_Network_HTTP429_NoRetryAfter` | API returns 429 without Retry-After | Exponential backoff (1s, 2s, 4s...) |
| `TestChaos_Network_HTTP500` | Server error on delta fetch | Retry up to 5 times, then fail cycle gracefully |
| `TestChaos_Network_HTTP410_ResyncApply` | Delta token expired, resync type 1 | Full re-enumeration, apply differences |
| `TestChaos_Network_HTTP410_ResyncUpload` | Delta token expired, resync type 2 | Upload local unknowns |
| `TestChaos_Network_TokenExpiry` | Access token expires mid-upload | Refresh token, retry fragment (not full upload) |
| `TestChaos_Network_CorruptJSON` | API returns malformed JSON | Parse error caught, item skipped, no panic |
| `TestChaos_Network_HTMLErrorPage` | API returns HTML instead of JSON | Detected as error, not written to disk |
| `TestChaos_Network_DNSFailure` | DNS resolution fails | Connection error, backoff, sync cycle retried |
| `TestChaos_Network_SlowResponse` | API takes 30s to respond | Data timeout triggers, retry with backoff |
| `TestChaos_Network_MaxRetriesExhausted` | 5 consecutive failures | Operation abandoned, item skipped, sync continues |

### 7.3 Filesystem Fault Injection

| Test | Fault | Expected Behavior |
|------|-------|-------------------|
| `TestChaos_FS_FileChangedDuringHash` | File modified while QuickXorHash is computed | Re-read or skip, never upload stale hash |
| `TestChaos_FS_FileDeletedBetweenObserveAndUpload` | File vanishes after observer sees it | Graceful skip, no panic, continue sync |
| `TestChaos_FS_PermissionDenied` | Target file not readable/writable | Skip with warning, continue sync |
| `TestChaos_FS_SymlinkLoop` | Circular symlink in sync directory | Detected, logged, skipped (not infinite loop) |
| `TestChaos_FS_DiskFull_MidDownload` | Disk fills during .partial write | Write error caught, .partial cleaned up, skip item |
| `TestChaos_FS_ReadOnlyDir` | Target directory is read-only | Download skipped with clear error |
| `TestChaos_FS_LongPath` | Path exceeds 255-byte component limit | Error caught, item skipped, not crashed |
| `TestChaos_FS_InvalidUTF8Filename` | File on disk with invalid UTF-8 bytes | Detected via `utf8.Valid()`, skipped with warning |
| `TestChaos_FS_SpecialCharsInName` | File with `%20`, `&amp;`, Unicode in name | Path normalized correctly, sync succeeds |
| `TestChaos_FS_AtomicSavePattern` | vim save: move old → create new → write | Debounce coalesces into single update, not delete+create |

### 7.4 Database Fault Injection

| Test | Fault | Expected Behavior |
|------|-------|-------------------|
| `TestChaos_DB_KillMidTransaction` | Process killed during batch write | WAL recovery on restart, partial batch rolled back |
| `TestChaos_DB_CorruptFile` | DB file has random bytes injected | Detected on open, offer clean re-init |
| `TestChaos_DB_DiskFullDuringWrite` | No disk space for WAL writes | Graceful error, no data corruption |
| `TestChaos_DB_WALCorrupt` | WAL file truncated | SQLite auto-recovery, DB consistent |
| `TestChaos_DB_StaleLock` | Lock file from dead process | PID-based validation, stale lock removed |
| `TestChaos_DB_ConcurrentAccess` | Two sync processes open same DB | SQLite lock prevents corruption, second process gets clear error |

---

## 8. Performance Testing

### 8.1 Benchmarks (Tracked in CI)

Benchmarks run in CI Job 1 using `go test -bench`. Results are stored and compared across commits. No hard gate — trends are reviewed manually. Hard gates will be added once baselines are established.

**NFR benchmarks** (from [prd.md §19](prd.md)):

| Benchmark | NFR Target | What It Measures |
|-----------|------------|-----------------|
| `BenchmarkMemory_100KItems` | < 100 MB RSS | Load 100K items into state DB, measure RSS |
| `BenchmarkCPU_WatchIdle` | < 1% CPU | Run watch mode with no events for 10s, measure CPU |
| `BenchmarkStartup` | < 1 second | Cold start: init config, open DB, ready state |
| `BenchmarkInitialEnum_10K` | < 10 minutes | Enumerate 10K items via mock delta API |

**Component benchmarks**:

| Benchmark | What It Measures |
|-----------|-----------------|
| `BenchmarkQuickXorHash_1MB` | Hash computation throughput (MB/s) |
| `BenchmarkQuickXorHash_100MB` | Hash computation for large files |
| `BenchmarkNormalizeItem` | Single item normalization latency |
| `BenchmarkNormalizeBatch_500` | 500-item batch normalization (reflects real batch size) |
| `BenchmarkPlan_1K` | Plan 1,000 items |
| `BenchmarkPlan_100K` | Plan 100,000 items |
| `BenchmarkFilterEvaluate_Simple` | 1 path against 5 patterns |
| `BenchmarkFilterEvaluate_Complex` | 1 path against 50 patterns + .odignore with negation |
| `BenchmarkFilterEvaluate_10KPaths` | 10,000 paths against complex filter set |
| `BenchmarkSQLite_BaselineCommit_1` | Single baseline entry commit latency |
| `BenchmarkSQLite_BaselineCommit_500` | 500 sequential per-action commits |
| `BenchmarkSQLite_BaselineGet_Indexed` | Lookup by (drive_id, item_id) primary key |
| `BenchmarkSQLite_ListByParent` | List children of a folder (indexed query) |
| `BenchmarkSQLite_PathMaterialization` | Recompute path for item at depth 10 |
| `BenchmarkSQLite_PathCascade_1KDescendants` | Cascade path update to 1,000 descendants |
| `BenchmarkPathConstruct` | Build path from parent chain (depth 10) |
| `BenchmarkTimestampParse` | Parse ISO 8601 timestamp |
| `BenchmarkDriveIdNormalize` | Normalize driveId (lowercase + zero-pad) |
| `BenchmarkObserverWalk_10KFiles` | Walk filesystem with 10K files (no hashing) |
| `BenchmarkObserverWalk_10KFiles_WithHash` | Walk + hash 10K small files |

**Benchmark execution**:

```go
func BenchmarkPlan_100K(b *testing.B) {
    views := buildBenchViews(b, 100_000) // Pre-populated with 100K PathViews
    baseline := loadBenchBaseline(b)

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        _ = Plan(views, baseline, ModeBidirectional)
    }
}

func BenchmarkMemory_100KItems(b *testing.B) {
    var m runtime.MemStats

    baseline := newFileBackedBaseline(b)
    // Insert 100K baseline entries
    for i := 0; i < 100_000; i++ {
        baseline.Commit(generateOutcomes(i))
    }

    runtime.GC()
    runtime.ReadMemStats(&m)

    // Report memory
    b.ReportMetric(float64(m.HeapAlloc)/1024/1024, "MB_heap")
    b.ReportMetric(float64(m.Sys)/1024/1024, "MB_sys")

    if m.HeapAlloc > 100*1024*1024 {
        b.Errorf("heap exceeds 100MB NFR: %d MB", m.HeapAlloc/1024/1024)
    }
}
```

### 8.2 Stress Tests (Nightly)

Stress tests use `//go:build stress` and run only in the nightly CI job. They verify behavior at scale limits.

| Test | Scenario | Target |
|------|----------|--------|
| `TestStress_100KFiles` | Sync simulation with 100K items (mock API) | Completes within 5 minutes |
| `TestStress_DeepNesting_100Levels` | 100-level nested directory tree | Path materialization works, no stack overflow |
| `TestStress_WideBranching_1000` | 1,000 items in single directory | Listing + reconciliation within 1 second |
| `TestStress_LargeUpload_250GB` | Upload session simulation (250GB, mock) | Session management handles boundary correctly |
| `TestStress_ConcurrentDrives_5` | 5 drives syncing simultaneously | No DB contention, each drive isolated |
| `TestStress_RapidChanges_1000` | 1,000 filesystem events in 1 second | Debounce coalesces, no event loss |
| `TestStress_DeltaPages_1000` | Delta response with 1,000 pages | All pages processed, token saved once |
| `TestStress_ConflictBurst_100` | 100 simultaneous conflicts | All recorded in conflicts table, no deadlock |
| `TestStress_FilterSet_1000Patterns` | 1,000 skip_files patterns | Filter evaluation < 1ms per path |
| `TestStress_WALGrowth_Bounded` | 50K writes without explicit checkpoint | WAL stays under journal_size_limit (64MiB) |

---

## 9. Regression Test Suite

### 9.1 Bug Pattern Regression Tests

Every one of the 23 known defensive patterns has a dedicated regression test. Test names reference the bug pattern for traceability.

#### Data Safety Regressions (Patterns 1-4)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_DownloadToTempRenameOnComplete` | #1 | §1.6, §3.5 | Download writes to .partial, renames after hash verification |
| `TestRegression_NeverDeleteRemoteOnLocalAbsence` | #2 | §1.1 | Items without a baseline entry generate no remote delete |
| `TestRegression_NoDestructiveOpsOnIncompleteData` | #3 | §1.8 | Partial delta → no cleanup deletions |
| `TestRegression_BigDeleteConfirmation` | #4 | §1.10 | Threshold exceeded → blocked, not executed |

#### API Resilience Regressions (Patterns 5-9)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_NormalizeAllIdentifiers` | #5 | §2.2, §2.3 | DriveId lowercase + zero-pad before DB storage |
| `TestRegression_DefensiveJSONParsing` | #6 | §3.6 | Missing fields, unexpected types, malformed values → no panic |
| `TestRegression_RespectRetryAfterHeaders` | #7 | §3.2 | 429 + Retry-After: 120 → waits 120s, not hardcoded backoff |
| `TestRegression_ProactiveTokenRefresh` | #8 | §3.3 | Token refreshed at 80% lifetime, not on expiry |
| `TestRegression_EnrichmentNoLoop` | #9 | §1.2 | Upload to SharePoint → enrichment → next cycle produces no upload/download. Run 5 cycles to prove stability. |
| `TestRegression_EditorFightingImpossible` | #9 | §1.2 | After upload with enrichment, local file is never modified by sync engine. Verify file mtime and content are unchanged. |

#### Database Design Regressions (Patterns 10-12)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_SelfHealingDB` | #10 | §2.1 | Orphaned parent reference → auto-repair, no --resync needed |
| `TestRegression_WALCheckpointing` | #11 | §2.4 | WAL file bounded after large batch writes |
| `TestRegression_DependencyOrderProcessing` | #12 | §2.3 | Parent items inserted before children, no FK violations |

#### Filesystem Handling Regressions (Patterns 13-15)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_DebounceFilesystemEvents` | #13 | §1.5, §1.9 | Create+delete within 2s window → ignored; vim save pattern → single update |
| `TestRegression_ValidateUTF8AndSpecialChars` | #14 | §4.1, §4.2 | Invalid UTF-8 skipped; URL encoding handled at API boundary |
| `TestRegression_ContentHashAsPrimaryChangeDetector` | #15 | §4.6 | Timestamp drift alone (hash matches) → no transfer |

#### Network Regressions (Patterns 16-18)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_GoNetHTTP` | #16 | §7.1 | No libcurl dependency, standard Go HTTP client |
| `TestRegression_BoundedRetryWithCircuitBreaker` | #17 | §3.1 | 5 consecutive failures → stop retrying, continue sync |
| `TestRegression_SIGPIPEIgnored` | #18 | §3.4 | Go ignores SIGPIPE by default (verify no CGo reintroduction) |

#### Shared Folder Regressions (Patterns 19-20)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_SharedFolderSafety` | #19 | §5.1, §5.2 | Shared folder metadata used for placement; extra safety on delete |
| `TestRegression_CrossDriveIdNormalization` | #20 | §5.3 | Same drive with different ID formats → normalized to canonical form |

#### Operational Regressions (Patterns 21-23)

| Test | Pattern | Bug Ref | What It Proves |
|------|---------|---------|----------------|
| `TestRegression_PIDBasedLocking` | #21 | §2.5 | Stale lock from dead PID → cleared on startup |
| `TestRegression_StructuredLoggingRateLimit` | #22 | §3.1 | Repeated errors rate-limited in log output |
| `TestRegression_IdempotentSyncOperations` | #23 | — | Interrupted + restarted sync produces same final state |

### 9.2 API Quirk Regression Tests

Each of the 12+ known API quirks has a regression test using realistic API response fixtures:

| Test | Quirk | Fixture |
|------|-------|---------|
| `TestQuirk_DriveIdCasingInconsistent` | Different casing across endpoints | `testdata/quirk_driveid_casing.json` |
| `TestQuirk_DriveIdTruncated15Chars` | Personal 15-char bug (#3072) | `testdata/quirk_driveid_truncated.json` |
| `TestQuirk_DeletedItemNoName_Business` | Name omitted on Business deletion | `testdata/quirk_deleted_no_name.json` |
| `TestQuirk_DeletedItemNoSize_Personal` | Size omitted on Personal deletion | `testdata/quirk_deleted_no_size.json` |
| `TestQuirk_DeletedItemBogusHash` | AAAAAA... hash on deleted items | `testdata/quirk_deleted_bogus_hash.json` |
| `TestQuirk_CTagAbsent_BusinessFolder` | No cTag for Business folders | `testdata/quirk_no_ctag_folder.json` |
| `TestQuirk_CTagAbsent_BusinessDelta` | No cTag in Business delta create/modify | `testdata/quirk_no_ctag_delta.json` |
| `TestQuirk_ETagAbsent_BusinessRoot` | No eTag for Business root | `testdata/quirk_no_etag_root.json` |
| `TestQuirk_DeltaDuplicateItem` | Same item appears twice in delta | `testdata/quirk_delta_duplicate.json` |
| `TestQuirk_DeltaDeletionAfterCreation` | Delete arrives after create at same path | `testdata/quirk_delta_deletion_order.json` |
| `TestQuirk_InvalidTimestamp` | Garbled timestamp string | `testdata/quirk_invalid_timestamp.json` |
| `TestQuirk_FractionalSecondsIgnored` | Sub-second precision stripped | `testdata/quirk_fractional_seconds.json` |
| `TestQuirk_MissingFileSystemInfo` | Shared item lacks fileSystemInfo | `testdata/quirk_missing_filesysteminfo.json` |
| `TestQuirk_OneNotePackage` | Package facet with type oneNote | `testdata/quirk_onenote_package.json` |
| `TestQuirk_HeicHashMismatch` | iOS .heic file hash/size mismatch | `testdata/quirk_heic_mismatch.json` |

---

## 10. CI Pipeline

### 10.1 Pipeline Overview

Three parallel jobs run on every PR push. E2E tests run only on merge to main and nightly.

```
┌─────────────────────────────────────────────────────────────────┐
│                        PR Push Trigger                           │
└──────────┬────────────────────┬──────────────────────┬──────────┘
           │                    │                      │
           ▼                    ▼                      ▼
   ┌──────────────┐   ┌─────────────────┐   ┌────────────────┐
   │   Job 1:     │   │    Job 2:       │   │   Job 3:       │
   │ Lint + Build │   │  Integration    │   │  E2E           │
   │ + Unit       │   │  + Chaos        │   │ (merge+nightly │
   │              │   │                 │   │  only)         │
   │ golangci-lint│   │ Mock HTTP       │   │ Live OneDrive  │
   │ go build     │   │ Real SQLite     │   │ Personal acct  │
   │ go test      │   │ Fault injection │   │                │
   │ benchmarks   │   │                 │   │                │
   │ coverage     │   │                 │   │                │
   └──────────────┘   └─────────────────┘   └────────────────┘
        ~2 min              ~3 min               ~10 min
```

### 10.2 E2E-First CI Strategy

Phase 2 introduces E2E CI **before** the sync engine is built. This is deliberate: the CLI commands (`ls`, `get`, `put`, `rm`) exercise `internal/graph/` against the live API, catching real-world quirks early. The sync engine builds on a Graph API client that has already been validated end-to-end.

**Phase 2 E2E scope** (pre-sync-engine):
- **Login**: OAuth device code flow completes, tokens stored, refresh works
- **ls**: List root, list subfolder, list nonexistent path (error handling)
- **get**: Download single file, verify hash; download to stdout; download nonexistent (error)
- **put**: Upload small file (single PUT), upload large file (resumable session), verify round-trip hash
- **rm**: Delete file, delete folder, delete nonexistent (error handling)
- **Round-trip**: `put` a file, `ls` to verify, `get` it back, compare content and hash, `rm` to clean up

**E2E edge cases** (exercised from Phase 2 onward):
- Large files (>4MB, triggering resumable upload sessions)
- Special characters in filenames (spaces, unicode, URL-encoded characters)
- Concurrent operations (parallel uploads/downloads via worker pool)
- Token refresh mid-operation (access token expires during upload session)

**CI infrastructure**: GitHub Actions with Azure Key Vault + OIDC federation for OneDrive API tokens (details in §6.1). Integration tests run on push to main + nightly, not on PRs.

### 10.3 Job 1: Lint + Build + Unit Tests

Runs on every PR push and every commit to main.

```yaml
job1-lint-build-unit:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4

    - uses: actions/setup-go@v5
      with:
        go-version: '1.24'

    # Lint
    - name: golangci-lint
      uses: golangci/golangci-lint-action@v6
      with:
        version: latest

    # Build
    - name: Build
      run: go build ./...

    # Verify generated mocks are up-to-date
    - name: Check generated code
      run: |
        go generate ./...
        git diff --exit-code || (echo "Generated code is stale. Run go generate ./..." && exit 1)

    # Unit tests with coverage
    - name: Unit tests
      run: go test -race -coverprofile=coverage.out -covermode=atomic ./...

    # Coverage enforcement
    - name: Check coverage
      run: |
        go tool cover -func=coverage.out | tail -1
        # Per-package coverage checks
        go tool cover -func=coverage.out | grep "internal/sync/" | awk '{ if ($3+0 < 90) { print "FAIL: " $1 " coverage " $3 " < 90%"; exit 1 } }'
        go tool cover -func=coverage.out | grep "internal/graph/" | awk '{ if ($3+0 < 95) { print "FAIL: " $1 " coverage " $3 " < 95%"; exit 1 } }'

    # Benchmarks (tracked, not gated)
    - name: Benchmarks
      run: go test -bench=. -benchmem -count=3 -run='^$' ./... > bench.txt

    - name: Store benchmark results
      uses: benchmark-action/github-action-benchmark@v1
      with:
        tool: 'go'
        output-file-path: bench.txt
        comment-on-alert: true
        alert-threshold: '150%'  # Warn if 50% regression
```

### 10.4 Job 2: Integration + Chaos Tests

Runs on every PR push. Uses a real SQLite database and mock HTTP server.

```yaml
job2-integration-chaos:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4

    - uses: actions/setup-go@v5
      with:
        go-version: '1.24'

    # Integration tests
    - name: Integration tests
      run: go test -race -tags=integration -timeout=5m ./...

    # Chaos / fault injection tests
    - name: Chaos tests
      run: go test -race -tags=chaos -timeout=5m ./...

    # Fuzz tests (time-budgeted)
    - name: Fuzz tests
      run: |
        for target in $(go test -list='Fuzz.*' ./... 2>/dev/null | grep '^Fuzz'); do
          pkg=$(grep -rl "func $target" --include='*_test.go' | head -1 | xargs dirname)
          go test -fuzz="^${target}$" -fuzztime=30s "./${pkg}" || true
        done
```

### 10.5 Job 3: E2E Tests

E2E tests run in the same workflow as integration tests (`.github/workflows/integration.yml`), not a separate job. They share the same Azure OIDC + Key Vault credential flow. Runs on push to main, nightly schedule, and manual dispatch.

```yaml
# E2E tests run after integration tests in integration.yml
# Credentials are already loaded from Key Vault in a prior step

- name: Run E2E tests
  run: |
    set -euo pipefail
    IFS=',' read -ra DRIVES <<< "$ONEDRIVE_TEST_DRIVES"
    for drive in "${DRIVES[@]}"; do
      drive=$(echo "$drive" | xargs)
      echo "=== Running E2E tests for: ${drive} ==="
      ONEDRIVE_TEST_DRIVE="$drive" \
        go test -tags=e2e -race -v -timeout=5m ./e2e/...
    done
```

See §6.1 for the full credential management flow (Key Vault download, token validation, post-test rotation).

### 10.6 Nightly Extended

The nightly schedule adds stress tests and optional Business/SharePoint E2E:

```yaml
on:
  schedule:
    - cron: '0 3 * * *'  # 3 AM UTC daily

jobs:
  nightly-extended:
    runs-on: ubuntu-latest
    steps:
      # ... (same setup as above)

      # Standard E2E (Personal)
      - name: E2E tests (Personal)
        run: go test -tags=e2e -timeout=30m -count=1 -p=1 ./e2e/...

      # Stress tests
      - name: Stress tests
        run: go test -tags=stress -timeout=30m ./...

      # Extended chaos
      - name: Extended chaos tests
        run: go test -tags=chaos -timeout=10m -count=5 ./...

      # Business E2E (if credentials available)
      - name: E2E tests (Business)
        if: env.ONEDRIVE_BUSINESS_ENABLED == '1'
        run: go test -tags=e2e -timeout=30m -count=1 -p=1 -run='TestE2E_Business' ./e2e/...

      # SharePoint E2E (if credentials available)
      - name: E2E tests (SharePoint)
        if: env.ONEDRIVE_SHAREPOINT_ENABLED == '1'
        run: go test -tags=e2e -timeout=30m -count=1 -p=1 -run='TestE2E_SharePoint' ./e2e/...
```

### 10.7 Coverage Reporting

Coverage profiles from Jobs 1 and 2 are merged and uploaded:

```yaml
    - name: Upload coverage
      uses: codecov/codecov-action@v4
      with:
        files: coverage.out
        flags: unittests
```

Per-package coverage thresholds are enforced as CI gates (§1.3). The overall 80% target is a minimum — individual critical packages have higher thresholds.

---

## 11. Test Organization & Conventions

### 11.1 Directory Structure

```
cmd/
└── onedrive-go/
    ├── main.go
    ├── ls.go, get.go, put.go, ...  # Cobra commands
    └── (no mocks — uses graph.Client directly)

internal/
├── graph/
│   ├── client.go                 # Graph API client (concrete struct)
│   ├── client_test.go
│   ├── item.go                   # graph.Item (clean type, all quirks resolved)
│   ├── raw.go                    # DriveItem (raw API type, internal to graph/)
│   ├── quirks.go                 # API quirk normalization
│   ├── quirks_test.go
│   ├── fuzz_test.go              # Fuzz targets for API parsing
│   └── testdata/
│       ├── delta_response_personal.json
│       ├── delta_response_business.json
│       ├── quirk_driveid_casing.json
│       ├── quirk_driveid_truncated.json
│       └── ...
├── sync/
│   ├── engine.go
│   ├── engine_test.go            # Unit tests
│   ├── planner.go
│   ├── planner_test.go           # Table-driven decision matrix tests (pure function)
│   ├── executor.go
│   ├── executor_test.go
│   ├── baseline.go               # Baseline manager (sole DB writer)
│   ├── baseline_test.go
│   ├── observer_remote.go        # Remote observer (delta → events)
│   ├── observer_remote_test.go
│   ├── observer_local.go         # Local observer (FS → events)
│   ├── observer_local_test.go
│   ├── buffer.go                 # Change buffer (debounce, dedup)
│   ├── buffer_test.go
│   ├── transfer_manager.go       # Unified download/upload with resume, hash verification
│   ├── transfer_manager_test.go
│   ├── worker.go                 # Lane-based worker pools
│   ├── worker_test.go
│   ├── tracker.go                # In-memory dependency graph
│   ├── tracker_test.go
│   ├── session_store.go          # File-based upload session persistence
│   ├── session_store_test.go
│   ├── mock_graph_test.go        # Generated mocks (moq) for graph interfaces
│   ├── migrations/
│   │   ├── 000001_initial_schema.sql
│   │   └── ...
│   └── testdata/
│       ├── .odignore_complex
│       └── ...
└── config/
    ├── config.go
    ├── config_test.go
    ├── migration.go              # abraunegg/rclone config migration
    ├── migration_test.go
    └── testdata/
        ├── valid_config.toml
        ├── minimal_config.toml
        ├── unknown_key.toml
        ├── abraunegg_config.toml
        └── rclone.conf

pkg/
└── quickxorhash/
    ├── quickxorhash.go
    ├── quickxorhash_test.go
    └── quickxorhash_bench_test.go

e2e/
├── e2e_test.go                   # //go:build e2e
├── helpers_test.go
├── personal_test.go
├── business_test.go              # //go:build e2e
└── sharepoint_test.go            # //go:build e2e
```

Test files live alongside source files (`*_test.go`). Test fixtures live in `testdata/` directories (ignored by `go build`). E2E tests live in a dedicated `e2e/` package. API quirk test fixtures live in `internal/graph/testdata/`. Sync-related test fixtures (filter patterns, DB fixtures) live in `internal/sync/testdata/`.

### 11.2 Test Naming Convention

```
Test{Component}_{Scenario}_{Expected}
```

Examples:
- `TestPlanner_EF5_EditEditConflict` — planner, decision row EF5, expect conflict
- `TestNormalize_DriveIdTruncation_ZeroPadded` — graph/ quirk handler, 15-char ID, expect zero-padding
- `TestChaos_S1_FailedDownloadNeverDeletesRemote` — chaos test for safety invariant S1
- `TestRegression_DownloadToTempRenameOnComplete` — regression test for pattern #1
- `TestQuirk_DeletedItemBogusHash` — API quirk regression
- `TestE2E_Conflict_EditEdit` — end-to-end conflict test
- `BenchmarkPlan_100K` — benchmark with 100K items

### 11.3 Table-Driven Test Pattern

All decision matrices, validation rules, and similar exhaustive tests use table-driven patterns:

```go
func TestValidateChunkSize(t *testing.T) {
    tests := []struct {
        name    string
        size    int64
        wantErr bool
    }{
        {"320KiB", 327680, false},
        {"10MiB", 10485760, false},
        {"not_multiple", 500000, true},
        {"zero", 0, true},
        {"negative", -1, true},
        {"too_large", 100 * 1024 * 1024, true}, // >60MiB
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := ValidateChunkSize(tt.size)
            if tt.wantErr {
                require.Error(t, err)
            } else {
                require.NoError(t, err)
            }
        })
    }
}
```

### 11.4 Test Helper Conventions

```go
// Always mark helpers
func newTestStore(t *testing.T) *Store {
    t.Helper()
    // ...
}

// Use t.TempDir() for filesystem isolation
func TestSomething(t *testing.T) {
    dir := t.TempDir() // Auto-cleaned
    // ...
}

// Use t.Setenv() for environment variable isolation
func TestConfig_EnvOverride(t *testing.T) {
    t.Setenv("ONEDRIVE_GO_DRIVE", "personal")
    // ...
}

// Use t.Cleanup() for teardown
func setupMockAPI(t *testing.T) *httptest.Server {
    t.Helper()
    srv := httptest.NewServer(handler)
    t.Cleanup(srv.Close)
    return srv
}

// Use require (not assert) for preconditions — fail fast
func TestFoo(t *testing.T) {
    baseline := newTestBaseline(t)
    require.NotNil(t, baseline) // Fail immediately if nil

    entry, err := baseline.Get("drive", "item")
    require.NoError(t, err)       // Fail immediately on error
    assert.Equal(t, "/expected/path", entry.Path) // Soft assert for actual test
}
```

### 11.5 Test Tags Usage

```go
// Unit test — no tag, runs with plain `go test`
func TestPlanner_EF1_NoAction(t *testing.T) { ... }

// Integration test — requires real SQLite, mock HTTP
//go:build integration

func TestPipeline_InitialSync(t *testing.T) { ... }

// Chaos test — fault injection
//go:build chaos

func TestChaos_S1_FailedDownloadNeverDeletesRemote(t *testing.T) { ... }

// E2E test — requires live OneDrive
//go:build e2e

func TestE2E_Upload_NewFile(t *testing.T) { ... }

// Stress test — long-running
//go:build stress

func TestStress_100KFiles(t *testing.T) { ... }
```

Run commands:
```bash
go test ./...                              # Unit tests only
go test -tags=integration ./...            # Unit + integration
go test -tags=chaos ./...                  # Unit + chaos
go test -tags="integration,chaos" ./...    # Unit + integration + chaos
go test -tags=e2e ./e2e/...               # E2E only
go test -tags=stress ./...                 # Unit + stress
go test -bench=. ./...                     # Benchmarks only
```

---

## Appendix A: Test Scenario Matrix

Complete mapping from sync algorithm decisions to test cases.

### A.1 File Decision Matrix → Test Cases

| Row | Decision | Unit Test(s) | Integration Test(s) | Chaos Test(s) |
|-----|----------|-------------|---------------------|---------------|
| EF1 | No action (in sync) | `TestPlanner_EF1_NoAction` | `TestPipeline_SteadyState_NoChanges` | — |
| EF2 | Download (remote changed) | `TestPlanner_EF2_RemoteChanged` | `TestPipeline_RemoteEdit` | `TestChaos_S3_*`, `TestChaos_S6_*` |
| EF3 | Upload (local changed) | `TestPlanner_EF3_LocalChanged` | `TestPipeline_LocalEdit` | `TestChaos_FS_FileChangedDuringHash` |
| EF4 | False conflict | `TestPlanner_EF4_FalseConflict` | `TestPipeline_FalseConflict` | — |
| EF5 | True conflict | `TestPlanner_EF5_TrueConflict` | `TestPipeline_MultiConflict_EditEdit` | — |
| EF6 | Remote delete | `TestPlanner_EF6_LocalDeleted`, `TestSafety_S1_*` | `TestPipeline_LocalDelete` | `TestChaos_S5_*` |
| EF7 | Re-download | `TestPlanner_EF7_Redownload` | — | — |
| EF8 | Local delete | `TestPlanner_EF8_RemoteDeleted`, `TestSafety_S4_*` | `TestPipeline_RemoteDelete` | `TestChaos_S4_*` |
| EF9 | Edit-delete conflict | `TestPlanner_EF9_EditDeleteConflict` | `TestPipeline_MultiConflict_MixedTypes` | — |
| EF10 | Identical new | `TestPlanner_EF10_IdenticalNew` | — | — |
| EF11 | Create-create conflict | `TestPlanner_EF11_CreateConflict` | `TestPipeline_MultiConflict_MixedTypes` | — |
| EF12 | Upload new | `TestPlanner_EF12_NewLocal` | `TestPipeline_InitialSync_*` | `TestChaos_FS_FileDeletedBetweenObserveAndUpload` |
| EF13 | Download new | `TestPlanner_EF13_NewRemote` | `TestPipeline_InitialSync_EmptyLocal` | `TestChaos_S3_*`, `TestChaos_S6_*` |
| EF14 | Cleanup | `TestPlanner_EF14_BothDeleted` | — | — |

### A.2 Folder Decision Matrix → Test Cases

| Row | Decision | Unit Test(s) | Integration Test(s) |
|-----|----------|-------------|---------------------|
| ED1 | No action | `TestPlannerFolder_ED1_exists_both` | `TestPipeline_SteadyState_NoChanges` |
| ED2 | Adopt | `TestPlannerFolder_ED2_new_both` | — |
| ED3 | Create locally | `TestPlannerFolder_ED3_remote_only` | `TestPipeline_InitialSync_EmptyLocal` |
| ED4 | Recreate locally | `TestPlannerFolder_ED4_local_deleted` | — |
| ED5 | Create remotely | `TestPlannerFolder_ED5_local_only` | — |
| ED6 | Delete locally | `TestPlannerFolder_ED6_remote_deleted` | `TestPipeline_RemoteDelete` |
| ED7 | Cleanup | `TestPlannerFolder_ED7_both_deleted` | — |
| ED8 | Move locally | `TestPlannerFolder_ED8_remote_moved` | `TestPipeline_Move_Remote` |

### A.3 Conflict Types → Test Cases

| Conflict Type | Unit Test | E2E Test | Chaos Test |
|--------------|-----------|----------|------------|
| Edit-edit | `TestConflict_EditEdit` | `TestE2E_Conflict_EditEdit` | — |
| Edit-delete (local edit, remote delete) | `TestConflict_EditDelete` | — | `TestChaos_S4_HashBeforeDeleteGuard` |
| Delete-edit (local delete, remote edit) | `TestConflict_DeleteEdit` | — | — |
| Create-create | `TestConflict_CreateCreate` | — | — |
| Type change | `TestConflict_TypeChange` | — | — |
| False conflict | `TestConflict_FalseConflict` | — | — |

### A.4 Safety Invariants → Test Cases

| Invariant | Unit Test | Chaos Test |
|-----------|-----------|------------|
| S1: Never delete remote on local absence | `TestSafety_S1_*` | `TestChaos_S1_FailedDownloadNeverDeletesRemote` |
| S2: No deletions from incomplete enumeration | `TestSafety_S2_*` | `TestChaos_S2_IncompleteDeltaNoDeletions` |
| S3: Atomic file writes | `TestSafety_S3_*` | `TestChaos_S3_InterruptedDownloadNoCorruption` |
| S4: Hash-before-delete guard | `TestSafety_S4_*` | `TestChaos_S4_HashBeforeDeleteGuard` |
| S5: Big-delete protection | `TestSafety_S5_*` | `TestChaos_S5_BigDeleteProtection` |
| S6: Disk space check | `TestSafety_S6_*` | `TestChaos_S6_DiskSpaceCheck` |
| S7: Never upload partial files | `TestSafety_S7_*` | `TestChaos_S7_NeverUploadPartialFiles` |

---

## Appendix B: Test Fixtures Catalog

### B.1 API Response Fixtures

All fixtures are stored as JSON files in `testdata/` directories (primarily `internal/graph/testdata/` for API response fixtures). Each fixture contains a realistic Graph API response captured from documentation or live API calls, then sanitized of real user data.

**Delta response fixtures**:

| Fixture | Account Type | Contents |
|---------|-------------|----------|
| `delta_response_personal.json` | Personal | 10 items: mix of files, folders, root |
| `delta_response_business.json` | Business | 10 items: missing cTag on folders |
| `delta_response_deleted.json` | Mixed | 5 deleted items: missing name, size, bogus hash |
| `delta_response_moved.json` | Personal | 3 items: 1 moved folder + 2 children |
| `delta_response_large.json` | Personal | 500 items: full batch for checkpoint testing |
| `delta_response_paginated/page1.json` | Personal | First page with nextLink |
| `delta_response_paginated/page2.json` | Personal | Last page with deltaLink |

**Individual item fixtures** (for `internal/graph/` quirk handling tests):

| Fixture | Quirk Tested |
|---------|-------------|
| `quirk_driveid_casing.json` | DriveId `B!2ID8JX` vs `b!2id8jx` |
| `quirk_driveid_truncated.json` | 15-character Personal driveId |
| `quirk_deleted_no_name.json` | Business deletion without name field |
| `quirk_deleted_no_size.json` | Personal deletion without size field |
| `quirk_deleted_bogus_hash.json` | Deleted item with AAAAAA... hash |
| `quirk_no_ctag_folder.json` | Business folder without cTag |
| `quirk_no_ctag_delta.json` | Business delta item without cTag |
| `quirk_no_etag_root.json` | Business root without eTag |
| `quirk_delta_duplicate.json` | Same item appearing twice |
| `quirk_delta_deletion_order.json` | Deletion after creation at same path |
| `quirk_invalid_timestamp.json` | Garbled timestamp string |
| `quirk_fractional_seconds.json` | Timestamp with nanosecond precision |
| `quirk_missing_filesysteminfo.json` | Shared item without fileSystemInfo |
| `quirk_onenote_package.json` | Item with package facet type oneNote |
| `quirk_heic_mismatch.json` | .heic file with metadata/content mismatch |

### B.2 Config Fixtures

| Fixture | Purpose |
|---------|---------|
| `valid_config.toml` | Complete valid config with all sections |
| `minimal_config.toml` | Only required fields |
| `unknown_key.toml` | Config with typo'd key (e.g., `sync_directory`) |
| `multiple_drives.toml` | Three drive sections with overrides |
| `invalid_chunk_size.toml` | chunk_size not a 320KiB multiple |
| `mutually_exclusive.toml` | download_only + upload_only both set |
| `abraunegg_config.toml` | Real abraunegg config for migration testing |
| `rclone.conf` | Real rclone config with OneDrive section |
| `malformed.toml` | Syntax error (missing closing bracket) |

### B.3 Filesystem Fixtures

Filesystem fixtures are created programmatically in test setup rather than stored on disk:

```go
// Standard file tree for sync tests
var standardFileTree = map[string]string{
    "Documents/report.pdf":       "pdf-content-here",
    "Documents/notes.txt":        "some notes",
    "Photos/vacation/img001.jpg": "jpeg-data",
    "Photos/vacation/img002.jpg": "jpeg-data-2",
    ".hidden/config":             "hidden-config",
}

// File tree with edge cases
var edgeCaseFileTree = map[string]string{
    "file with spaces.txt":       "content",
    "unicode-café.txt":           "content",
    "UPPERCASE.TXT":              "content",
    "deeply/nested/path/file.txt": "content",
}
```

---

## Appendix C: Decision Log

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | Generated mocks (moq) from consumer-defined interfaces | Type-safe, auto-regenerated on interface change, CI verifies freshness. 4 narrow interfaces in sync/ (DeltaFetcher, ItemClient, Downloader, Uploader) instead of 17+ provider-defined. CLI uses graph.Client directly — no mocks needed. |
| D2 | Personal E2E from day one, Business/SharePoint E2E in backlog | Personal accounts are free. Business/SharePoint share one M365 Business Basic subscription (~$5/month) and will be added to nightly CI once the core E2E suite is stable. Until then, account-specific quirks are covered by unit tests with realistic fixtures. |
| D3 | Property-based testing applied broadly | Algorithm properties (convergence, idempotence) catch classes of bugs that table-driven tests miss. Parsing properties prevent panics on unexpected input. Broad application catches more bugs. |
| D4 | 80% overall / 90%+ core coverage targets | 80% catches most regressions without encouraging test-for-coverage-sake. 90%+ for sync core reflects that this code manages user data and correctness is critical. |
| D5 | All chaos tests from day one | The worst known bugs in OneDrive sync tools (data loss, infinite loops) would have been caught by fault injection. Retrofitting chaos tests is harder than building them in. |
| D6 | Benchmarks tracked but not gated | Hard gates cause CI flake on noisy VMs. Tracking trends with alerting on 50%+ regression catches real problems without false positives. Hard gates when baselines are stable. |
| D7 | Three parallel CI jobs | Lint+build+unit is fast (~2 min) and catches most issues. Integration+chaos is medium (~3 min) and catches wiring + failure mode bugs. E2E is slow (~10 min) and only needed on merge. Parallelism keeps PR feedback fast. |
| D8 | Setup/teardown per test, not shared state | Shared test state causes flaky tests, ordering dependencies, and debugging nightmares. Timestamped directories + per-test cleanup ensures isolation and reproducibility. |
| D9 | Azure Key Vault + OIDC for CI token storage | Secure, no long-lived secrets in GitHub. OIDC federation eliminates PAT rotation. Key Vault provides encrypted storage with auditing. See §6.1. |
| D10 | E2E auth failure warns, does not block PRs | A developer pushing a code change should not be blocked by an expired OneDrive token. E2E auth issues are infrastructure problems, not code problems. |
| D11 | testify assert/require over stdlib-only | `require.Equal` with diff output saves minutes of debugging compared to `if got != want { t.Errorf(...) }`. The dependency is test-only and well-maintained. |
| D12 | Fuzz tests with 30s CI budget | 30 seconds per fuzz target catches easy panics without dominating CI time. Developers can run unlimited locally. Corpus is committed so CI builds on previous discoveries. |

---

## Architecture Constraint Traceability

Every constraint from [architecture.md](architecture.md) is traced:

| Architecture Constraint | Implementation |
|------------------------|----------------|
| `internal/sync/` defines 4 consumer-defined interfaces (DeltaFetcher, ItemClient, Downloader, Uploader) over `graph.Client` for mock testing | §2.2 Mock Generation — moq generates mocks from consumer-defined interfaces in sync/ |
| E2E tests must cover all four drive types (Personal, Business, SharePoint, Shared) | §6.2 Core E2E (Personal in CI), §6.3 Account-specific (Business + SharePoint + Shared nightly, plus vault exclusion and display name matching) |
| Must test API quirk handling in `internal/graph/` with known-bad inputs | §3.1 API quirk handling tests (13 quirk tests), §9.2 API Quirk Regression Tests (15 fixtures) |
| Must test conflict detection and resolution | §3.1 Conflict Detection (6 conflict types), §6.2 E2E conflict test, Appendix A.3 |
| Must test crash recovery from mid-sync interruption | §5.3 Database Integration (crash recovery tests), §7.4 Database Fault Injection |
| CLI (`cmd/onedrive-go/`) uses `graph.Client` directly — no mocks needed | §10.2 E2E-First CI Strategy — CLI E2E tests validate graph client against live API |
