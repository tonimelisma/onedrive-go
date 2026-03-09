# Backlog

Historical backlog from Phases 1-4v1 archived in `docs/archive/backlog-v1.md`.

## Active (In Progress)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| ~~B-331~~ | ~~Architecture A cleanup — complete token metadata removal~~ | ~~P1~~ | ~~tokenfile, config, root~~ | **DONE** — Token files pure OAuth (strict JSON, no meta). Flat data dir layout (account_*, drive_* at root). All metadata fallbacks deleted. CI/scripts updated. |

## Ready (Up Next)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| ~~B-300~~ | ~~Rename `SessionRecord` JSON tag `"remote_path"` → `"local_path"`~~ | ~~P4~~ | ~~driveops~~ | **DONE** — Bumped `currentSessionVersion` to 2. Custom `UnmarshalJSON` reads both `remote_path` (v0/v1) and `local_path` (v2+). Save writes v2. |

## CI Reliability Hardening

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-310~~ | ~~Fix flaky `TestWatch_HashFailureStillEmitsCreate`~~ | ~~P2~~ | **DONE** — Race between `os.Chmod` and observer hash computation. Fix: create file with `0o000` mode from the start so it's born unreadable. |
| ~~B-311~~ | ~~Fix E2E data race in `waitForDaemonReady`~~ | ~~P1~~ | **DONE** — `bytes.Buffer` shared between `os/exec` goroutine and test polling loop. Fix: `syncBuffer` type with `sync.Mutex` guards. Applied to all daemon E2E tests. |
| ~~B-303~~ | ~~Token file integrity enforcement~~ | ~~P1~~ | **DONE** — `tokenfile.ValidateMeta()` + `LoadAndValidate()` validate required metadata (`drive_id`, `user_id`, `display_name`, `cached_at`) on both write and read paths. `Save()` rejects incomplete non-nil meta. `TokenSourceFromPath` uses strict validation. `ReadTokenMeta` validates before consuming. Fixes "drive ID not resolved" CI failures. |
| ~~B-304~~ | ~~WAL checkpoint on BaselineManager.Close()~~ | ~~P2~~ | **DONE** — `PRAGMA wal_checkpoint(TRUNCATE)` before `db.Close()` ensures all WAL data is flushed to the main DB file. Fixes potential cross-process SQLite visibility issues in sync→conflicts command flow. |
| ~~B-305~~ | ~~E2E edit-delete conflict eventual-consistency guard~~ | ~~P2~~ | **DONE** — Added `pollCLIWithConfigNotContains` helper. `TestE2E_Sync_EditDeleteConflict` now polls for remote delete propagation before running sync. Fixes flaky conflict history assertion. |

## Phase 6.0b: Orchestrator + DriveRunner

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-295~~ | ~~Orchestrator + DriveRunner — always-on multi-drive runtime~~ | ~~P1~~ | **DONE** — Phase 6.0b. `Orchestrator` in `internal/sync/`, `DriveRunner` with panic recovery, client pooling by token path, `RunOnce`, sync command rewrite with `skipConfigAnnotation`. Watch mode bridge for 6.0c. |

## Phase 5.6: Identity Refactoring

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-271~~ | ~~Personal Vault exclusion in RemoteObserver~~ | ~~P1~~ | **DONE** — Phase 5.6.1. `SpecialFolderName` field + `isDescendantOfVault()` parent-chain walk. |
| ~~B-272~~ | ~~Add `DriveTypeShared` to `driveid` package~~ | ~~P1~~ | **DONE** — Phase 5.6.2. Fourth drive type, type-specific field routing, part-count validation, `ConstructShared()`, `Equal()`, predicates, zero-ID fix. |
| ~~B-273~~ | ~~Move token resolution to `config` package~~ | ~~P1~~ | **DONE** — Phase 5.6.3. `config.TokenCanonicalID(cid, cfg)` in `token_resolution.go`. Removed method from `driveid`. |
| ~~B-274~~ | ~~Replace `Alias` with `DisplayName` in config~~ | ~~P1~~ | **DONE** — Phase 5.6.4. Renamed Alias→DisplayName, auto-derived `DefaultDisplayName()`, wired into `buildResolvedDrive`. |
| ~~B-275~~ | ~~Update CLI for display_name~~ | ~~P2~~ | **DONE** — Phase 5.6.5. DisplayName in `drive list`, `status`, `drive add` output. `driveLabel()` helper. |
| ~~B-276~~ | ~~Delta token composite key migration~~ | ~~P2~~ | **DONE** — Phase 5.6.6. Migration 00004, composite PK `(drive_id, scope_id)`, existing tokens preserved. |

## Hardening: Identity & Types

Defensive coding and edge cases for `internal/driveid/` and `internal/graph/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-278~~ | ~~Standardize driveid test style (testify vs stdlib)~~ | ~~P4~~ | **DONE** — Converted `canonical_test.go` to testify. Foundation hardening PR. |
| ~~B-279~~ | ~~Add `OwnerEmail` field to `graph.Drive` struct~~ | ~~P3~~ | **DONE** — Added `OwnerEmail` from `owner.user.email`. Foundation hardening PR. |
| ~~B-280~~ | ~~Document `graph.User.Email` mapping to Graph API field~~ | ~~P4~~ | **DONE** — Doc comments on `User.Email` and `toUser()`. Foundation hardening PR. |
| ~~B-281~~ | ~~Vault parent-chain ordering assumption in RemoteObserver~~ | ~~P2~~ | **DONE** — Two-pass delta processing in `fetchPage()`. Safety-critical fix. Foundation hardening PR. |
| ~~B-282~~ | ~~Add `HashesComputed` counter to `ObserverStats`~~ | ~~P4~~ | **DONE** — Atomic counter in `classifyAndConvert()`. Foundation hardening PR. |

## Hardening: CLI Architecture

Code quality and architecture improvements for the root package. Root package at **46.7% coverage** (up from 39.9% after 6.0g root coverage tests).

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-223~~ | ~~Extract `DriveSession` type for per-drive resource lifecycle~~ | ~~P1~~ | **DONE** — Phase 6.0a (initial), Phase 6.0e (replaced by `driveops.SessionProvider` + `driveops.Session`). `DriveSession` deleted. All commands use `cc.Provider.Session()`. |
| ~~B-224~~ | ~~Eliminate global flag variables (`flagJSON`, `flagVerbose`, etc.)~~ | ~~P1~~ | **DONE** — Phase 6.0a. Two-phase CLIContext: `CLIFlags` struct populated for all commands (Phase 1), config resolved for data commands (Phase 2). Zero global flag variables. |
| ~~B-227~~ | ~~Deduplicate sync_dir and StatePath validation across commands~~ | ~~P3~~ | **DONE** — Post-6.0a hardening. `newSyncEngine()` helper validates syncDir/statePath and builds `EngineConfig`. Replaces boilerplate in `sync.go` and `resolve.go`. |
| ~~B-228~~ | ~~`buildLogger` silent fallthrough on unknown log level~~ | ~~P3~~ | Fixed in Phase 5.5: added `warn` case and `default` with stderr warning. |
| ~~B-232~~ | ~~Test coverage for `loadConfig` error paths~~ | ~~P3~~ | **DONE** — Phase 6.0c. Tests for invalid TOML, ambiguous drive, unknown log level in `root_test.go`. |
| ~~B-036~~ | ~~Extract CLI service layer for testability~~ | ~~P4~~ | **DONE** — Phase 6.2b. Interfaces `accountMetaReader`, `tokenStateChecker`, `syncStateQuerier` + `buildStatusAccountsWith` testable core. Root package 40.1% → 43.8%. |
| ~~B-229~~ | ~~`syncModeFromFlags` uses `Changed` instead of `GetBool`~~ | ~~P4~~ | **DONE** — Phase 6.0c. Documented as intentional Cobra pattern in comment. |
| ~~B-230~~ | ~~`printSyncReport` repetitive formatting~~ | ~~P4~~ | **DONE** — Phase 6.0c. Extracted `printNonZero` helper. |
| ~~B-233~~ | ~~`version` string concatenation in two places~~ | ~~P4~~ | **DONE** — Phase 6.0e. `userAgent` now passed to `NewSessionProvider` once; no more per-call concatenation. |

## Hardening: Graph API

Edge cases and correctness for `internal/graph/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-020~~ | ~~SharePoint lock check before upload (HTTP 423)~~ | ~~P2~~ | **DONE** — Reclassified HTTP 423 (Locked) as `errClassSkip` instead of `errClassRetryable`. SharePoint co-authoring locks last hours; retrying is pointless. Watch mode safety scan re-attempts in 5 min. |
| ~~B-021~~ | ~~Hash fallback chain for missing hashes~~ | ~~P2~~ | **DONE** — `HasHash()` helper, `HashVerified=false` when remote hash is empty. `SelectHash` covers all account types (QuickXorHash → SHA256 → SHA1). |
| ~~B-007~~ | ~~Cross-drive DriveID handling for shared/remote items~~ | ~~P3~~ | **Folded into Phase 6.4a** — remoteItem parsing and cross-drive DriveID handling is roadmap increment 6.4a. |
| ~~B-283~~ | ~~URL-encode query parameter in `SearchSites()`~~ | ~~P2~~ | **DONE** — Phase 6.0a. Added `url.QueryEscape(query)` in SearchSites URL construction. |

## Hardening: Config

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-284~~ | ~~`write.go` config writer uses fragile line-based TOML editing~~ | ~~P3~~ | **DONE** — Phase 6.2b. Structured `parsedLine` model replaces prefix matching with exact key comparison. Inline comments preserved. `paused` no longer matches `paused_until`. |
| ~~B-288~~ | ~~`quiet` parameter threading across 11 functions~~ | ~~P4~~ | **DONE** — Phase 6.0c. Added `CLIContext.Statusf()` method. Refactored resume.go, pause.go, resolve.go, sync.go to pass `cc *CLIContext` instead of `quiet bool`. |
| ~~B-287~~ | ~~Symlink-aware sync_dir overlap warning~~ | ~~P4~~ | **DONE** — `checkDriveSyncDirUniqueness` now resolves symlinks via `filepath.EvalSymlinks` before comparing. Falls back to lexical if path doesn't exist yet. |

## Hardening: Test Infrastructure

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-285~~ | ~~Standardize `baseline_test.go` to testify style~~ | ~~Done~~ | Converted in fix/b285-testify-baseline. Net -637 lines. |
| ~~B-286~~ | ~~No shared/business drive in E2E test matrix~~ | ~~P3~~ | **DONE** — 6.4c topup: added `sync_shared_e2e_test.go` with 3 E2E tests (owner upload → recipient download, drive list --shared, idempotent re-sync). Uses testitesti18 → kikkelimies123 shared folder fixture. |
| ~~B-306~~ | ~~Exhaustive E2E test hardening~~ | ~~P2~~ | **DONE** — 42 new `e2e_full` tests across 5 new files + 2 modified. Covers daemon watch (11), CLI commands (13), edge cases (8), recovery (3), output validation (4), multi-drive watch (3). Total E2E: 86 tests (44 existing + 42 new). |

## Deferred from Phase 6.0c

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-299~~ | ~~E2E tests for daemon mode (Orchestrator.RunWatch)~~ | ~~P3~~ | **DONE** (6.0f+6.0g) — `e2e/sync_watch_e2e_test.go`: `TestE2E_SyncWatch_BasicRoundTrip`, `TestE2E_SyncWatch_PauseResume`, `TestE2E_SyncWatch_SIGHUPReload`. Build tag `e2e,e2e_full`. |

## Hardening: Watch Mode

Improvements to continuous sync reliability in `internal/sync/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-101~~ | ~~Add timing and resource logging to safety scan~~ | ~~P3~~ | **DONE** — `observer_local_handlers.go:475-481` logs elapsed time, event count, baseline entries. |
| ~~B-115~~ | ~~Test: safety scan + watch producing conflicting change types~~ | ~~P3~~ | **DONE** — `buffer_test.go:940-994` `TestBuffer_WatchAndSafetyScanConflictingTypes`. |
| B-128 | Debounce semantics change under load | P4 | Correctness done (write coalescing via B-107). Remaining: load-testing and documentation. |

## Hardening: Performance

Optimization deferred until profiling shows a bottleneck.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-063 | Per-tenant rate limit coordination | P4 | Multiple drives under same tenant share Graph API rate limits. Shared rate limiter per-tenant. |
| B-064 | Baseline memory scaling for many drives | P4 | ~19 MB per 100K files, additive across drives. Monitor during profiling (B-031). |
| B-031 | Profile and optimize performance | P4 | CPU/memory/I/O profiling with `pprof`. After feature-complete. |
| B-171 | Streaming delta processing (process pages as they arrive) | P4 | Reduces memory for large deltas. Modest win (~1ms per page vs ~100-300ms API call). |
| B-172 | SQLite batched commits for high-throughput workloads | P4 | Per-action commit is ~0.5ms. Bottleneck is network I/O, not SQLite. |
| B-173 | Concurrent folder creates via Graph API `$batch` | P4 | Sibling folders at same depth. Diminishing returns after first sync. |

## Filtering Conflicts (pre-Phase 10)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-307~~ | ~~FC-1: Remote observer symmetric filtering~~ | ~~P1~~ | **DONE** — Phase 5.7.1. Added `isAlwaysExcluded()` + `isValidOneDriveName()` to `classifyItem()` in remote observer. Remote items now filtered symmetrically with local observer. |
| ~~B-308~~ | ~~FC-2: Narrow `.db` exclusion to sync engine database~~ | ~~P2~~ | **DONE** — Phase 5.7.1. Removed `.db`/`.db-wal`/`.db-shm` from `alwaysExcludedSuffixes`. Legitimate data files no longer silently excluded. |
| ~~B-309~~ | ~~FC-12: Non-empty directory delete — Tier 1 disposable cleanup~~ | ~~P2~~ | **DONE** — `isDisposable()` classifies OS junk (`.DS_Store`, `Thumbs.db`, `._*`), editor temps (`.tmp`, `.swp`), and invalid OneDrive names. `deleteLocalFolder` auto-removes disposable files before attempting removal. 6 test cases. |

## Prerelease Robustness (from [prerelease review](docs/archive/prerelease_review.md))

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| ~~B-312~~ | ~~Path containment guard at executor write points~~ | ~~P1~~ | **DONE** — Added `containedPath()` using `filepath.IsLocal()` to reject path traversal. Replaced all 7 `filepath.Join(syncRoot, path)` calls in executor files with containment-checked version. |
| ~~B-313~~ | ~~DAG cycle detection in `buildDependencies`~~ | ~~P1~~ | **DONE** — Added `detectDependencyCycle()` with DFS white/gray/black coloring. Called after `buildDependencies()` in `Plan()`. Five test cases (no-cycle, self-loop, mutual, indirect, diamond). |
| ~~B-314~~ | ~~`io.ReadAll` unbounded on error responses~~ | ~~P2~~ | **DONE** — Wrapped all 4 `io.ReadAll(resp.Body)` calls with `io.LimitReader(resp.Body, maxErrBodySize)` (64 KiB cap). |
| ~~B-315~~ | ~~`UploadURL` lacks `slog.LogValuer` protection~~ | ~~P2~~ | **DONE** — Added `type UploadURL string` with `LogValue()` redaction. Changed `UploadSession.UploadURL` and `UploadSessionStatus.UploadURL` from `string` to `UploadURL`. Updated 30+ conversion sites. |
| ~~B-316~~ | ~~No dedicated migration tests~~ | ~~P2~~ | **DONE** — Added `TestMigration00002_SyncFailureRoundTrip` testing Up/Down round-trip of migration 00002, verifying CHECK constraint changes and data preservation. |
| ~~B-317~~ | ~~API response fuzz tests~~ | ~~P3~~ | **DONE** — 3 fuzz functions: `FuzzDriveItemUnmarshal`, `FuzzDeltaResponseUnmarshal`, `FuzzPermissionUnmarshal`. Seed corpus from representative API responses. No panics found. |
| ~~B-318~~ | ~~Fault injection test suite~~ | ~~P3~~ | **DONE** — 3 test scenarios: context cancel during worker pool, DB close during commit, partial file cleanup. All verify graceful handling without panics. |
| ~~B-319~~ | ~~Clock skew resilience audit~~ | ~~P3~~ | **DONE** — Audit doc at `docs/design/clock-skew-audit.md`. All `time.Now()` call sites classified. Targeted tests for backward clock jump (baseline, local issues, conflicts). Codebase already has excellent `nowFunc` injection coverage. |
| ~~B-320~~ | ~~`Baseline.ByPath`/`ByID` exported mutable maps~~ | ~~P3~~ | **DONE** — Renamed to `byPath`/`byID`. All access through thread-safe accessors (`GetByPath`, `GetByID`, `Put`, `Delete`, `Len`, `ForEachPath`). Added `NewBaselineForTest`. Updated 30+ test sites. |
| ~~B-321~~ | ~~`tokenfile.RequiredMetaKeys` exported mutable slice~~ | ~~P3~~ | **DONE** — Replaced with unexported `func requiredMetaKeys() []string` returning a fresh copy each call. |
| ~~B-322~~ | ~~Test cleanups: merge ENOSPC mock, doc comments, deduplicate hashBytes~~ | ~~P4~~ | **DONE** — (a) Deleted `enospcWatcherForEngine` duplicate from engine_test.go, reuse `enospcWatcher`. (b) Added mutation warning to `GetByPath`/`GetByID` doc comments. (c) Replaced `hashBytes` with `hashContent` in verify_test.go. |
| ~~B-323~~ | ~~Symlink TOCTOU fix in `containedPath`~~ | ~~P2~~ | **DONE** — `containedPath()` now resolves symlinks on parent directory via `filepath.EvalSymlinks` and verifies containment. Falls back to lexical-only when path doesn't exist yet. Three new tests. |

## CI / Infrastructure

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-324 | Cross-test contamination mitigation via `remote_path` filtering | P2 | Bidirectional sync E2E tests on shared drive pick up delta events from other parallel tests' folders. Root cause: delta returns ALL drive events, no folder-scoped delta on Business accounts. **Interim fix**: deletion-dependent tests de-parallelized (no `t.Parallel()`). **Long-term fix**: implement `remote_path` config (roadmap Phase 10) to filter delta events client-side. See ci_issues.md §19. |
| ~~B-325~~ | ~~Periodic full reconciliation scan (safety net for missed delta deletions)~~ | ~~P2~~ | **DONE** — Full reconciliation implemented: `observeRemoteFull()` + `Baseline.FindOrphans()` detect orphaned items missed by incremental delta. Three access modes: `sync --full` (manual), daemon periodic reconciliation (default 24h via `ReconcileInterval`), programmatic. See ci_issues.md §21. |
| ~~B-326~~ | ~~Investigate delta token advancement on zero-event responses~~ | ~~P3~~ | **DONE** — Zero-event guard: `observeAndCommitRemote()` and `RemoteObserver.Watch()` skip token advancement when delta returns 0 events. Replaying costs O(1); prevents advancing past still-propagating deletions. See ci_issues.md §20. |
| ~~B-327~~ | ~~`EnsureDriveInConfig` broken for shared drives~~ | ~~P3~~ | **DONE** — `DriveTokenPath(cid)` no longer requires `cfg`. Shared drive token resolution reads drive metadata files (`drive_*.json` at data root) for `account_canonical_id`. `addSharedDrive` registers drive metadata via `SaveDriveMetadata` before calling `EnsureDriveInConfig`. No more nil-cfg crashes. |

## Phase 6.4 Follow-up

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-332~~ | ~~Add tests for `reconcileShortcutScopes`~~ | ~~P2~~ | **DONE** — 6 tests: delta reconciliation + orphan detection, enumerate reconciliation, collision skipping, per-scope error isolation, no shortcuts, nil fetchers. |
| ~~B-333~~ | ~~Reduce redundant `ListShortcuts` queries per sync cycle~~ | ~~P4~~ | **DONE** — `processShortcuts` loads shortcuts once after registration and threads the list through `detectShortcutCollisionsFromList` and `observeShortcutContentFromList`. 4 queries → 1 (2 if removals present). |
| ~~B-334~~ | ~~`processShortcuts` collects ALL deletes into `removedShortcutIDs`~~ | ~~P4~~ | **DONE** — Pre-filters delete IDs against known shortcut IDs before passing to `handleRemovedShortcuts`. |
| ~~B-335~~ | ~~Remove duplicate `ObservationEnumerate` case in `observeSingleShortcut`~~ | ~~P5~~ | **DONE** — Collapsed to `case ObservationDelta:` / `default:`. |

## Retry Architecture Follow-up

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-336~~ | ~~Drop dead failure columns from remote_state schema~~ | ~~P5~~ | **DONE** — Consolidated schema rebuilds `remote_state` without `failure_count`, `next_retry_at`, `last_error`, `http_status`. Full retry architecture transition complete: removed escalation infrastructure, renamed permanent→actionable, replaced circuit breaker with throttle gate, unified `issues` CLI command, updated status/retry policy. |

## Retry Architecture — Actionable Failure Lifecycle

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-341 | 507 quota exceeded → actionable failure with time-based retry | P3 | **Design phase required.** HTTP 507 (Insufficient Storage) is currently misclassified as transient. Should be actionable with `issue_type='quota_exceeded'`, visible in `issues`, with time-based retry (server-scoped fix has no local signal). See details below. |
| B-342 | File-scoped actionable failure lifecycle — stale cleanup | P3 | **Design phase required.** When user fixes a file-scoped actionable failure (rename, move, delete), the old `sync_failures` row persists until age-based pruning. Four design options to evaluate. See details below. |
| B-343 | Scope-classified retry policies and display | P3 | **Design phase required.** Different retry backoff curves per failure scope. Scope-classified display in `status`. Implements R10.3-R10.4, R11.1-R11.4. See details below. |

### B-341: 507 Quota Exceeded → Actionable Failure with Time-Based Retry

**Problem**: HTTP 507 from Graph API during upload is misclassified. `RecordFailure()` (baseline.go:1068) hardcodes `category='transient'`. User has no visibility — 507 failures don't appear in `issues`.

**What 507 means**: File is fine locally. Problem is server-side (OneDrive quota full). User must free space on OneDrive (delete files via web UI, empty recycle bin). No local filesystem change signals the fix — only a retry against the server can detect resolution.

**Why time-based retry is correct**: Unlike file-scoped actionable failures (invalid name, path too long, file too large) where the user fixes the problem locally (scanner detects it), 507 requires a server-side fix with no local signal. The reconciler must periodically re-inject the upload attempt. When quota is freed, upload succeeds, and `CommitOutcome` (baseline.go:557-565) auto-clears the `sync_failures` row.

#### Architectural context: two failure recording paths

The codebase has **two separate code paths** that write to `sync_failures`, each with its own category logic. This is a design smell that B-341 will make worse unless addressed.

**Path 1: `RecordFailure()` (baseline.go:1068)** — Called by `processWorkerResult()` in the engine when an executor action (download, upload, delete) fails with an HTTP error. This path **always hardcodes `category='transient'`** in the INSERT statement. It stores `http_status`, `last_error`, and computes `next_retry_at` via `computeNextRetry()`. It has **no concept of issue types** — the `issue_type` column is left empty. This is the path that handles runtime HTTP errors like 429, 500, 503, 507, etc.

**Path 2: `RecordSyncFailure()` (baseline.go:1525)** — Called by `filterInvalidUploads()` in the engine when pre-upload validation fails (invalid filename, path too long, file too large). This path uses `isActionableIssue()` to determine category. Actionable failures get `next_retry_at = NULL` (never retried by the reconciler). This path **always has an issue type** (`IssueInvalidFilename`, `IssuePathTooLong`, `IssueFileTooLarge`).

**`isActionableIssue()` (baseline.go:1512)** — A switch statement that returns `true` for `IssueInvalidFilename`, `IssuePathTooLong`, `IssueFileTooLarge`. This function is **only called by Path 2** (`RecordSyncFailure`). Path 1 (`RecordFailure`) bypasses it entirely because Path 1 doesn't set issue types — it only has an HTTP status code.

**The problem for B-341**: HTTP 507 arrives via Path 1 (`RecordFailure`), which hardcodes `category='transient'`. To classify 507 as actionable, we must either:

1. **Add HTTP-status-based classification to Path 1**: Add a `classifyCategory(httpStatus)` check in `RecordFailure()` that returns `'actionable'` for 507. This keeps the two paths separate but splits the "what is actionable?" decision across two functions (`isActionableIssue` for issue types, `classifyCategory` for HTTP codes). The category decision becomes harder to audit.

2. **Unify into a single failure classifier**: Create a `classifyFailure(issueType, httpStatus) → (category, needsRetry)` function that both paths call. All category logic lives in one place. `RecordFailure` would need to derive an issue type from the HTTP status (e.g., 507 → `IssueQuotaExceeded`), then call the unified classifier. This is a larger refactor but makes the system more auditable.

3. **Merge the two recording paths**: A single `RecordSyncFailure()` that accepts both issue-type-based and HTTP-status-based failures. Callers provide what they have (issue type, HTTP status, or both), and the function determines category. This eliminates the dual-path smell entirely.

**Recommendation**: Option 2 or 3 should be evaluated during B-341's design phase. The current split means that to answer "what category does failure X get?", you must know which code path recorded it, then trace through path-specific logic. A unified classifier makes the answer a single function lookup. This becomes critical when B-343 adds scope classification — without unification, scope logic would also need to be duplicated across both paths.

**Changes required**:
- `internal/sync/upload_validation.go`: Add `IssueQuotaExceeded = "quota_exceeded"` constant
- `internal/sync/baseline.go`: Unify or extend failure classification — either add HTTP-status awareness to `isActionableIssue()`, or create a unified `classifyFailure()` function that both `RecordFailure` and `RecordSyncFailure` call (design decision)
- `internal/sync/baseline.go`: `RecordFailure()` — detect `httpStatus == 507`, set `category = 'actionable'`, `issue_type = IssueQuotaExceeded`, with `next_retry_at` (507 is the one actionable type that should have time-based retry). Currently the INSERT on line 1133 hardcodes `'transient'` — must use a variable.
- `internal/sync/baseline.go`: `ListSyncFailuresForRetry()` — change WHERE from `category = 'transient'` to `next_retry_at IS NOT NULL AND next_retry_at <= ?`
- `internal/sync/baseline.go`: `EarliestSyncFailureRetryAt()` — same category filter removal
- Schema partial index (`migrations/00001_consolidated_schema.sql`): Remove `category = 'transient'` from `idx_sync_failures_retry`. New: `WHERE next_retry_at IS NOT NULL`
- `internal/sync/engine.go`: `classifyStatusCode(507)` currently returns `errClassFatal` (line 422-423). This means 507 goes through `RecordFailure()` without retry. Verify this is correct after B-341 changes (507 should still be fatal at the transport/action level — retries happen at the reconciler level via `next_retry_at`).

**Tests**:
- `RecordFailure` with `httpStatus=507` → `category='actionable'`, `issue_type='quota_exceeded'`, `next_retry_at` set (not NULL)
- `RecordFailure` with `httpStatus=503` → `category='transient'` (unchanged behavior)
- `RecordFailure` with `httpStatus=429` → `category='transient'` (unchanged behavior)
- `ListSyncFailuresForRetry` returns 507 failures when `next_retry_at <= now`
- `ListActionableFailures` returns 507 failures (visible in `issues`)
- If unified classifier is built: table-driven test covering every known `(issueType, httpStatus)` → `(category, needsRetry)` mapping
- `isActionableIssue()` or its replacement: exhaustive test covering all issue type constants

### B-342: File-Scoped Actionable Failure Lifecycle — Stale Cleanup

**Problem**: When a user fixes a file-scoped actionable failure (`path_too_long`, `file_too_large`, `invalid_filename`) by renaming, moving, or deleting the file, the old `sync_failures` row persists indefinitely. The file was never uploaded (no baseline entry), so the scanner's deletion detection doesn't emit a `ChangeDelete`. Stale entry shows in `issues` as unresolved even though user already fixed it. Currently cleared only by age-based pruning (baseline.go:889-895) or manual `issues clear`.

**Why re-validation is unnecessary for 2 of 3 types**:
- `path_too_long`: If file still exists at same path, path is still too long by definition (path = primary key).
- `invalid_filename`: If file still exists at same path, name is still invalid.
- `file_too_large`: Could change (user compresses in place), but this case is already handled by the existing scanner → validation → upload → CommitOutcome pipeline.

**Also consider**: Suppressing redundant re-validation noise in one-shot mode. Every `sync` pass re-emits unchanged files with actionable failures (no baseline → scanner emits ChangeCreate → planner plans upload → `filterInvalidUploads` catches it → `RecordSyncFailure` UPSERT bumps `failure_count`). `filterInvalidUploads` could skip files that already have an actionable sync_failure with the same issue_type.

**Design options (to be evaluated)**:
1. **`recheckActionableFailures()` at start of pass** (403 pattern): Stat each actionable failure path. If file gone → `ClearSyncFailure`. Pro: follows established pattern. Con: arguably overkill since detection is just an `os.Stat`.
2. **Scanner deletion detection enhancement**: Extend `detectDeletions()` to compare observed set against sync_failures paths (not just baseline). Pro: uses existing mechanism. Con: scanner gains sync_failures coupling.
3. **Aggressive pruning**: Enhance `Prune()` to stat actionable failure paths and clear missing ones. Pro: minimal new code. Con: only runs on prune schedule.
4. **Do nothing**: Rely on existing age-based pruning + `issues clear`. Pro: no changes. Con: stale entries linger.

### B-343: Scope-Classified Retry Policies and Display

**Problem**: All transient failures use the same `retry.Reconcile` policy (30s base → 1h max). Suboptimal for different scopes:
- **File-scoped transient** (423 locked, hash mismatch, 412 ETag): Self-resolving. 30s→1h appropriate.
- **Service/account-wide transient** (500, 502, 503, 504, 509, 429, 401): Affects ALL items. Retrying 100 files every 30s when service is down wastes resources.
- **Actionable server-scoped** (507 quota — B-341): User must free space. Retrying every 30s is pointless.

The `status` command shows flat "Retrying: N" with no scope context.

**Prerequisite**: B-341 should be implemented first. If B-341 introduces a unified failure classifier (see B-341 architectural context), scope classification naturally extends it: `classifyFailure(issueType, httpStatus) → (category, scope, retryPolicy)`. If B-341 leaves the two recording paths separate, B-343 must add scope derivation logic to **both** paths — `RecordFailure()` would derive scope from HTTP status, `RecordSyncFailure()` would derive scope from issue type. This duplication is another argument for unifying during B-341.

**Key design decisions**:
1. **Retry policies**: Proposed three: `Reconcile` (file: 30s→1h, existing), `ReconcileWide` (service/account: 2min→1h, new), `ReconcileActionable` (actionable: 5min→6h, new).
2. **Scope derivation**: Store `scope` column in `sync_failures`? Or derive from `(category, issue_type, http_status)` in Go code? Storing makes SQL grouping trivial (`GROUP BY scope`). Deriving avoids schema change but requires Go-side grouping for display.
3. **Probe-based retry for wide-scope**: When 100 items fail with 503, retry ONE probe and kick all 100 if it succeeds? Or use longer intervals + jitter? The throttle gate (R6) already handles 429 batching — could a similar pattern work for 503?
4. **Scope-classified display**: `status` showing "47 items (503 Service Unavailable since 2h ago)" instead of "Retrying: 47". Requires either `scope` column or Go-side `GROUP BY http_status` over `ListSyncFailures()` results.
5. **`computeNextRetry()` changes**: Currently `computeNextRetry(now, failureCount)` calls `retry.Reconcile.Delay(failureCount)`. Must become scope-aware: `computeNextRetry(now, failureCount, scope)` selecting the appropriate policy. Both `RecordFailure()` and `RecordSyncFailure()` call `computeNextRetry()` — if a unified classifier exists (B-341), scope is available at the call site.
6. **Reconciler changes**: `reconcileSyncFailures()` currently re-injects all due items uniformly. With scope classification, should the reconciler prioritize file-scoped items (likely to succeed) over service-wide items (likely to fail again)?

**Scope classification from R11**: Service-wide (500, 502, 503, 504, 509, network timeout, 400 ObjectHandle), Account-wide (429, 401, 507), Folder-scoped (403 — existing permission subsystem), File-scoped (423, hash mismatch, 412).

**Where scope is determined today**: `RecordFailure()` receives `httpStatus` as a parameter from `processWorkerResult()`. `RecordSyncFailure()` receives `issueType` from `filterInvalidUploads()`. All file-scoped actionable issues come through Path 2; all HTTP-status-based failures come through Path 1. The 403 case is special — it goes through `handle403()` in the engine which calls `RecordSyncFailure()` with `issue_type='permission_denied'` and has its own recheck subsystem (`recheckPermissions()`). Scope classification must respect this existing 403 carve-out.

**Implements**: R10.3, R10.4, R11.1-R11.4 from `docs/design/retry-transition-requirements.md`.

## Prerelease Robustness — Remaining Items

From [prerelease_review.md](docs/archive/prerelease_review.md). Items B-312 to B-323 already done. Remaining:

### Inc 1: Security — Path Containment (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-344 | Remote filename validation (reject `..`, `/`, `\`, null bytes, control chars) in `classifyAndConvert` | P2 | Inc 1 deliverable 2. Scanner validates local names but remote API responses NOT validated for path traversal chars. |
| B-345 | Written threat model document | P3 | Inc 1 deliverable 5. Document all attack surfaces and guards. |

### Inc 2: Resource Bounds & DoS Resistance (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-346 | Per-transfer timeout or connection-level deadline for `transferHTTPClient()` | P3 | Inc 2 deliverable 2. `Timeout: 0` relies on context. Stalled connection without context cancellation hangs forever. |
| B-347 | Total item cap during delta enumeration | P3 | Inc 2 deliverable 3. `maxObserverPages = 10000` × items per page = unbounded memory. |
| B-348 | Per-path event cap in Buffer | P4 | Inc 2 deliverable 4. `defaultBufferMaxPaths` caps path count but not events-per-path. |
| B-349 | Documentation of resource consumption guarantees | P4 | Inc 2 deliverable 5. Per-component resource bounds. |

### Inc 3: Concurrency Correctness

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-350 | Lock ordering contract document | P3 | Inc 3 deliverable 1. Every mutex, documented hierarchy. `tracker.go:154` has nested `mu` → `cyclesMu`. |
| B-351 | Channel lifecycle document | P4 | Inc 3 deliverable 2. Every channel: who creates/closes/reads/writes. |
| B-352 | Audit `ForEachPath` callers for re-entrancy safety | P2 | Inc 3 deliverable 3. Holds read lock during callback — write from callback → deadlock. |
| B-353 | Mutex or `sync.Once` on `SyncStore.Load` | P3 | Inc 3 deliverable 4. Concurrent `Load` calls race on `m.baseline = b`. |
| B-354 | Targeted `-race` stress tests for DepTracker, Buffer, WorkerPool | P3 | Inc 3 deliverable 5. |

### Inc 4: State Machine Invariants (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-355 | Sub-second uniqueness in `conflictCopyPath` | P3 | Inc 4 deliverable 3. Second-precision timestamps — two conflicts in same second collide. |
| B-356 | Fix zero-byte NULL conflation in baseline | P2 | Inc 4 deliverable 4. Zero-byte files map to `NULL` in SQLite — can't distinguish from "size unknown". Use `sql.NullInt64{Valid: true, Int64: 0}`. |
| B-357 | Explicit error for unknown ActionType in `applySingleOutcome` | P3 | Inc 4 deliverable 5. Default case returns nil — silently drops outcomes. |
| B-358 | Property-based tests for planner with random inputs | P4 | Inc 4 deliverable 6. `testing/quick` or manual generators. |

### Inc 5: Error Handling Completeness

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-359 | Audit all `_ = ...` patterns in production code | P3 | Inc 5 deliverable 1. |
| B-360 | Add value assertions after bare `assert.NoError` calls | P4 | Inc 5 deliverable 2. 42+ bare assertions could mask incorrect results. |
| B-361 | Error return from deferred `Close` on write paths | P3 | Inc 5 deliverable 3. Deferred close errors universally ignored. |
| B-362 | Panic recovery in scanner hash phase | P3 | Inc 5 deliverable 4. Worker pool has recovery; scanner does not. |

### Inc 6: API Contract Fidelity (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-363 | Nil guards on all Item field accesses in `observer_remote.go` | P2 | Inc 6 deliverable 2. Delta items with missing `Name`, `ParentReference`, etc. |
| B-364 | Upload URL validation (HTTPS scheme, Microsoft domain) | P3 | Inc 6 deliverable 3. `UploadSession.UploadURL` received with no URL validation. |
| B-365 | NFC normalization idempotency test | P4 | Inc 6 deliverable 4. |

### Inc 7: Credential Safety (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-366 | Audit all `slog.*` calls for potential secret leakage | P2 | Inc 7 deliverable 2. |
| B-367 | Audit all error message strings for embedded secrets | P3 | Inc 7 deliverable 3. `GraphError.Message` includes API error body. |
| B-368 | Test that captures log output and verifies no tokens/pre-auth URLs appear | P3 | Inc 7 deliverable 4. |

### Inc 8: Encapsulation (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-369 | Unexport `graph.Client.Do`/`DoWithHeaders` if unused externally | P4 | Inc 8 deliverable 3. |
| B-370 | Evaluate decoupling `sync` → `graph` error dependency via interface | P4 | Inc 8 deliverable 4. |

### Inc 9: Test Completeness (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-371 | Planner property tests (random inputs, verify DAG invariant) | P3 | Inc 9 deliverable 2. Same as B-358, cross-reference. |
| B-372 | Buffer overflow test with drop metric verification | P3 | Inc 9 deliverable 3. |
| B-373 | Transfer manager resume edge case tests (corrupt partial, changed remote, oversized) | P3 | Inc 9 deliverable 4. |
| B-374 | Root package unit test expansion (target 60%+) | P4 | Inc 9 deliverable 5. Currently ~47%. |

### Inc 10: Operational Resilience (partial)

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-375 | Disk full during baseline commit — in-memory cache consistency | P3 | Inc 10 deliverable 1 (partial). |
| B-376 | Graceful shutdown test under active worker pool | P3 | Inc 10 deliverable 2. SIGTERM during active transfers. |
| B-377 | inotify partial-watch cleanup verification | P4 | Inc 10 deliverable 4. Already-added watches cleaned up on setup failure? |
| B-378 | Documentation of degraded-mode behavior guarantees | P4 | Inc 10 deliverable 5. |

### Inc 11: Architecture Re-evaluation

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-379 | Architecture re-evaluation: `internal/sync/` package splitting | P4 | Inc 11 question 1. 8k lines in one package — evaluate sub-packages. |
| B-380 | Architecture re-evaluation: `sync` → `graph` error coupling | P4 | Inc 11 question 2+7. Same as B-370. |
| B-381 | Architecture re-evaluation: baseline storage abstraction | P4 | Inc 11 question 3. `BaselineStore` interface? |
| B-382 | Architecture re-evaluation: CLI structure scaling | P5 | Inc 11 question 6. 21 files / 4k lines — group by domain? |

## Phase 7 Follow-up

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|
| B-337 | Extract `multiHandler` to `internal/slogutil/` package | P4 | root | `multiHandler` lives in `root.go` alongside CLI setup. If logging grows (structured error reporting, log sampling), a dedicated package would be cleaner. |
| B-338 | Concurrent `recycle-bin empty` deletion | P4 | root | Currently deletes items sequentially. For large recycle bins, a worker pool (similar to sync executor) would be faster. |
| B-339 | Remove `PermanentDeleteItem` 405→`DeleteItem` fallback when MS adds Personal support | P5 | root, graph | The `DeleteItem` fallback for Personal accounts is a workaround for HTTP 405 on `permanentDelete`. Monitor MS Graph API changelog. |
| B-340 | Recover coverage lost by recycle-bin CLI handlers (75.5% → 76.0%) | P4 | root | CLI `RunE` handlers in `recycle_bin.go` are untestable without live Session. Interface-based mock injection could recover those lines. |

## Phase 6.3 Follow-up

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-328~~ | ~~SharedWithMe deprecated Nov 2026 — search-based alternative~~ | ~~P2~~ | **DONE** — `SearchDriveItems` using `GET /me/drive/search(q='*')` as primary discovery. SharedWithMe as fallback. Identity enrichment via `GetItem` for search results. |
| B-329 | `drive/recent` deprecated Nov 2026 | P5 | We don't use this endpoint. No action needed. |
| B-330 | Monitor `search(q='*')` reliability on business accounts | P4 | Wildcard search for shared item discovery needs verification on non-personal accounts. |

## Closed

| ID | Title | Resolution |
|----|-------|------------|
| B-328 | SharedWithMe deprecated Nov 2026 — search-based alternative | **DONE** — `SearchDriveItems` + identity enrichment via `GetItem`. SharedWithMe as fallback. |
| B-325 | Periodic full reconciliation scan | **DONE** — `observeRemoteFull()` + `Baseline.FindOrphans()` + `sync --full` + daemon periodic reconciliation (24h default). See ci_issues.md §21. |
| B-326 | Delta token advancement on zero-event responses | **DONE** — Zero-event guard in `observeAndCommitRemote()` and `Watch()`. See ci_issues.md §20. |
| B-307 | FC-1: Remote observer symmetric filtering | **DONE** — Phase 5.7.1. `isAlwaysExcluded()` + `isValidOneDriveName()` in `classifyItem()`. Remote items filtered symmetrically with local observer. |
| B-308 | FC-2: Narrow `.db` exclusion | **DONE** — Phase 5.7.1. Removed `.db`/`.db-wal`/`.db-shm` from `alwaysExcludedSuffixes`. |
| B-310 | Fix flaky `TestWatch_HashFailureStillEmitsCreate` | **DONE** — File born unreadable (mode `0o000`) eliminates race between chmod and hash. |
| B-311 | Fix E2E data race in `waitForDaemonReady` | **DONE** — `syncBuffer` with `sync.Mutex` replaces `bytes.Buffer` in all daemon E2E tests. |
| B-300 | Rename `SessionRecord` JSON tag `"remote_path"` → `"local_path"` | **DONE** — Bumped `currentSessionVersion` to 2. Custom `UnmarshalJSON` reads both `remote_path` (v0/v1) and `local_path` (v2+). Save writes v2. |
| B-287 | Symlink-aware sync_dir overlap detection | **DONE** — `checkDriveSyncDirUniqueness` resolves symlinks via `filepath.EvalSymlinks`. Falls back to lexical if path doesn't exist yet. |
| B-101 | Add timing and resource logging to safety scan | **DONE** — `observer_local_handlers.go:475-481` logs elapsed time, event count, baseline entries. |
| B-115 | Test: safety scan + watch conflicting change types | **DONE** — `buffer_test.go:940-994` `TestBuffer_WatchAndSafetyScanConflictingTypes`. |
| B-021 | Hash fallback chain for missing hashes | **DONE** — `HasHash()` helper, `HashVerified=false` when remote hash is empty. |
| B-020 | SharePoint lock (HTTP 423) reclassification | **DONE** — HTTP 423 reclassified as `errClassSkip` (was `errClassRetryable`). |
| B-298 | Watch-mode parallel hashing | **Moved to roadmap** — Phase 8.1 alongside B-297 and AIMD. |
| B-297 | Worker budget algorithm for multi-drive allocation | **Moved to roadmap** — Phase 8.1 (adaptive concurrency + multi-drive worker budget). See MULTIDRIVE.md §11.3. |
| B-302 | Post-B-301 housekeeping hardening | **DONE** — (1) Removed `staleSessionAge` parameter from `CleanTransferArtifacts` (always `StaleSessionAge`). (2) Made `postSyncHousekeeping` synchronous — eliminates process-exit race where cleanup goroutine may not complete. (3) Added WalkDir error logging for permission-denied subdirectories. (4) Permission error test added (coverage +0.5%). |
| B-301 | Auto-delete `.partial` files after sync | **DONE** — `ReportStalePartials` (warn-only) replaced by `CleanStalePartials` (unconditional delete). After sync completes, `.partial` files are always garbage: successful downloads rename them, failed downloads delete them, Ctrl-C aborts before housekeeping runs. Threshold removed — no age check needed. |
| B-296 | Config-file `log_level` not applied by sync command | **DONE** — `runSync` rebuilds logger from `rawCfg.LoggingConfig` after loading config. CLI flags still override. Test `TestBuildLogger_FromRawConfigLogLevel` added. |
| B-207 | Document intentional `.partial` preservation on rename failure | **DONE** — Comment added in `transfer_manager.go` (B-207). PR #139. |
| B-211 | `resumeDownload` TOCTOU race between stat and open | **DONE** — Open-before-stat pattern eliminates TOCTOU (B-211). PR #139. |
| B-221 | Add comment explaining Go integer range in hash retry loop | **DONE** — Comment explaining Go 1.22 `range N` syntax (B-221). PR #139. |
| B-222 | Document `selectHash` cross-file reference in `transfer_manager.go` | **DONE** — Cross-file reference comment (B-222). PR #139. |
| B-160 | Drop or document `conflicts.history` column | **DONE** — Documented as intentionally dormant/unused (B-160). PR #139. |
| B-149 | Deduplicate conflict scan logic in `baseline.go` | **DONE** — `conflictScanner` interface, single `scanConflict()` (B-149). PR #139. |
| B-154 | Sort map keys in planner for reproducible action order | **DONE** — `sort.Strings(sortedPaths)` before classification (B-154). PR #139. |
| B-071 | ConflictRecord missing Name field | **DONE** — `Name` field derived from `path.Base(Path)` (B-071). PR #139. |
| B-120 | Symlinked directories get no watch and no warning | **DONE** — `slog.Warn` on symlinked directory in watch setup (B-120). PR #139. |
| B-125 | No health or liveness signal from Watch() goroutines | **DONE** — `LastActivity()` on both observers (B-125). PR #139. |
| B-127 | No observer-level metrics or counters | **DONE** — `ObserverStats` struct with `EventsEmitted`, `PollsCompleted`, `Errors` (B-127). PR #139. |
| B-158 | `DownloadURL`: implement `slog.LogValuer` for compile-time redaction | **DONE** — `LogValue()` returns `[REDACTED]` (B-158). PR #139. |
| B-087 | Conflict retention/pruning policy | **DONE** — `PruneResolvedConflicts()` with configurable retention (B-087). PR #139. |
| B-198 | Periodic baseline cache consistency check in watch mode | **DONE** — `CheckCacheConsistency()` report-only verification (B-198). PR #139. |
| B-138 | Add upstream sync check for oauth2 fork | **DONE** — `scripts/check-oauth2-fork.sh` (B-138). PR #139. |
| B-277 | E2E polling for Graph API eventual consistency | **DONE** — Polling helpers (`pollCLIContains`, `pollCLIWithConfigContains`, `pollCLISuccess`) replace fatal write-then-read assertions. `Drives()` 403 retry in production code. |
| B-205 | `WorkerPool.errors` slice grows unbounded in watch mode | **DONE** — Capped at 1000 with `droppedErrors` counter. PR #129. |
| B-208 | `sessionUpload` non-expired resume error creates infinite retry loop | **DONE** — Delete session on any resume failure. PR #129. |
| B-204 | Reserved worker receives on nil channel in select | **DONE** — Go nil-channel semantics documented. PR #133. |
| B-206 | Document `sendResult` lost-result edge case in panic recovery | **DONE** — B-206 comment explaining benign drop. PR #133. |
| B-209 | `DownloadToFile` doesn't validate empty `targetPath` | **DONE** — Empty-string validation for targetPath and itemID. PR #130. |
| B-210 | `UploadFile` doesn't validate empty `name` parameter | **DONE** — `validateUploadParams()` for parentID, name, localPath. PR #130. |
| B-212 | `freshDownload` uses permissive file permissions (`os.Create` = 0666) | **DONE** — `os.OpenFile` with 0o600, matching `resumeDownload`. PR #130. |
| B-214 | Test: `DownloadToFile` rename failure preserves `.partial` | **DONE** — EISDIR rename failure test. PR #133. |
| B-215 | Test: `sessionUpload` session save failure still completes upload | **DONE** — Chmod-based save injection. PR #133. |
| B-216 | Test: `UploadFile` stat failure wraps error correctly | **DONE** — `errors.Is(err, os.ErrNotExist)` verification. PR #133. |
| B-217 | Test: non-RangeDownloader with existing `.partial` starts fresh | **DONE** — `tmSimpleDownloader` overwrites old partial. PR #133. |
| B-218 | Test: worker panic recovery records error in `wp.errors` | **DONE** — Enhanced test checks "panic:" in Stats() and WorkerResult. PR #133. |
| B-219 | Inconsistent hash function usage: direct call vs `tm.hashFunc` | **DONE** — `resumeDownload` switched to `tm.hashFunc`. PR #133. |
| B-220 | `deleteSession` helper swallows errors silently | **DONE** — Improved comment documenting fire-and-forget pattern. PR #130. |
| B-225 | Defensive nil guard for `cliContextFrom` | **DONE** — `mustCLIContext()` with clear panic. 10 callers updated. PR #129. |
| B-226 | Remove `os.Exit(1)` from `runVerify` | **DONE** — Sentinel error `errVerifyMismatch`. PR #129. |
| B-231 | `loadAndVerify` separation rationale is stale | **DONE** — Comment updated when B-226 removed `os.Exit`. PR #129. |
| B-107 | Write event coalescing at observer level | **DONE** — Per-path timer coalescing (500ms cooldown). PR #129. |
| B-105 | `addWatchesRecursive` has no aggregate failure reporting | **DONE** — Summary Info log with watched/failed counters. PR #130. |
| B-238 | `hashAndEmit` retry exhaustion lacks distinct log message | **DONE** — Distinguish retry exhaustion from generic hash failure. PR #131. |
| B-239 | `findConflict` prefix matching without ambiguity check | **DONE** — Two-pass search: exact first, then prefix with ambiguity detection. PR #131. |
| B-240 | `resolveAllKeepBoth` and `resolveAllWithEngine` duplicate loop | **DONE** — `resolveEachConflict` shared helper. PR #131. |
| B-241 | `addWatchesRecursive` logs Info unconditionally even on 0 failures | **DONE** — Debug when `failed==0`, Info otherwise. PR #131. |
| B-242 | `freshDownload`/`resumeDownload` duplicate partial cleanup pattern | **DONE** — `removePartialIfNotCanceled` helper (5 call sites). PR #131. |
| B-243 | `sessionUpload` parameter named `remotePath` is actually local path | **DONE** — Renamed to `localPath` with documenting comment. PR #131. |
| B-244 | `DownloadToFile` hash exhaustion silently overrides remoteHash | **DONE** — `HashVerified` field on `DownloadResult`. PR #131. |
| B-245 | `printConflictsTable`/`printConflictsJSON` no shared field extraction | **DONE** — `formatNanoTimestamp` + `toConflictJSON`. PR #131. |
| B-246 | `conflictIDPrefixLen = 8` constant lacks "why 8" comment | **DONE** — Added entropy explanation. PR #131. |
| B-247 | `computeStableHash` double stat undocumented | **DONE** — Added comment explaining intentional pre/post stat. PR #131. |
| B-248 | `engine.go` plan invariant guard doesn't surface to SyncReport | **DONE** — Sets `report.Failed` and appends to `report.Errors`. PR #131. |
| B-249 | `transfer_manager_test.go` `fmtappendf` lint suggestion | **DONE** — `fmt.Appendf(nil, ...)` instead of `[]byte(fmt.Sprintf(...))`. PR #131. |
| B-203 | Flaky `TestWatch_NewDirectoryPreExistingFiles` | **DONE** — Emit with empty hash on `errFileChangedDuringHash`. 100/100 pass. |
| B-074 | Drive identity verification at Engine startup | **DONE** — Phase 5.3. `verifyDriveIdentity()`. |
| B-085 | Resumable downloads (Range header) | **DONE** — Phase 5.3. `DownloadRange` + `.partial` resume. |
| B-096 | Parallel hashing in FullScan | **DONE** — Phase 5.2.1. `errgroup.SetLimit(runtime.NumCPU())`. |
| B-170 | Parallel remote + local observation in RunOnce | **DONE** — Phase 5.2.0. `errgroup.Go()` for concurrent observation. |
| B-200a | Stale `.partial` file cleanup command | **DONE** — CLI prints path + resume instructions on Ctrl+C. |
| B-089 | Baseline concurrent-safe incremental cache | **DONE** — Phase 5.0. `sync.RWMutex` + locked accessors. |
| B-090 | Eliminate `createdFolders` map | **DONE** — Phase 5.0. DAG edges + incremental baseline. |
| B-091 | `resolveTransfer()` migrate to `CommitOutcome()` | **DONE** — Phase 5.0. |
| B-095 | DepTracker.byPath cleanup on completion | **DONE** — Phase 5.1. |
| B-098 | Backpressure for LocalObserver.Watch() | **DONE** — Non-blocking `trySend()` with drop-and-log. |
| B-099 | Configurable safety scan interval | **DONE** — Phase 5.3. |
| B-100 | Scan new directory contents on watch create | **DONE** — `scanNewDirectory()`. |
| B-102 | Hash failure silently drops events | **DONE** — All paths emit events with empty hash. |
| B-103 | `debounceLoop` final drain deadlock | **DONE** — Phase 5.2. Non-blocking select. |
| B-109 | RemoteObserver.Watch() interval validation | **DONE** — Clamp below `minPollInterval` (30s). |
| B-111 | Multiple `FlushDebounced()` calls break goroutine | **DONE** — Panic on double-call. |
| B-112 | `handleDelete` doesn't remove watches | **DONE** — `watcher.Remove()` for deleted dirs. |
| B-113 | `Watch()` doesn't detect sync root deletion | **DONE** — `ErrSyncRootDeleted` sentinel. |
| B-119 | Hashing actively-written files | **DONE** — Phase 5.3. `computeStableHash()`. |
| B-121 | Delta token and baseline not atomically consistent | **DONE** — Phase 5.2. Resolved via `CommitObservation` atomicity + durable failure state. cycleTracker removed as dead code. |
| B-122 | No dedup between planner and in-flight tracker | **DONE** — Phase 5.2. `HasInFlight()` + `CancelByPath()`. |
| B-123 | Repeated failure suppression for watch mode | **DONE** — Phase 5.3. `failureTracker`. |
| B-124 | Watch() error semantics don't distinguish exit reasons | **CLOSED** — Asymmetry is harmless. Comment added. |
| B-126 | Buffer has no size cap | **DONE** — `maxPaths` field (default 100K). |
| B-129 | LocalObserver.Watch() no backoff for watcher errors | **DONE** — Exponential backoff (1s→30s). |
| B-037 | Chunk upload retry for pre-auth URLs | **DONE** — `doPreAuthRetry`. |
| B-069 | Locally-deleted folder with no remote delta event | **DONE** — ED8 → ActionRemoteDelete. |
| B-072 | ED1-ED8 missing folder-delete-to-remote | **DONE** — Part of B-069. |
| B-075 | Upload session leak in chunkedUpload | **DONE** — `CancelUploadSession`. |
| B-076 | Partial file leak on f.Close() failure | **DONE** — `os.Remove` in Close error path. |
| B-077 | resolveTransfer nil-map panic | **DONE** — Lazy initialization guard. |
| B-078 | TransferClient leaks session lifecycle | **DONE** — `Downloader` + `Uploader` interfaces. |
| B-079 | Executor conflates config with mutable state | **DONE** — `ExecutorConfig` + ephemeral `Executor`. |
| B-080 | Upload progress callback was nil | **DONE** — Debug-level closure. |
| B-081 | Simple upload doesn't preserve mtime | **DONE** — `UpdateFileSystemInfo()` PATCH. |
| B-082 | Baseline Load() queries DB on every call | **DONE** — Cache-through pattern. |
| B-083 | engineMockClient lacks interface checks | **DONE** — Compile-time checks added. |
| B-084 | EF9 edit-delete conflict fails silently | **DONE** — Auto-resolve: local edit wins. |
| B-092 | Audit and clean up unused schema tables | **DONE** — Phase 5.4. Migration 00003. |
| B-097 | Action queue compaction | **SUPERSEDED** — No `action_queue` table. |
| B-104 | `FlushImmediate()` logs Info on empty buffer | **DONE** — Changed to Debug. |
| B-108 | No test for combined chmod+create event | **DONE** — `TestWatchLoop_ChmodCreateCombinedEvent`. |
| B-110 | `LocalObserver.sleepFunc` dead code | **DONE** — Removed. |
| B-114 | Event channel sizing undocumented | **DONE** — Documentation added. |
| B-116 | Document stale baseline interaction | **DONE** — Comments added. |
| B-117 | Test: transient file create+delete on macOS | **DONE** — `TestWatchLoop_TransientFileCreateDelete`. |
| B-118 | Test: local move out-of-order events | **DONE** — `TestWatchLoop_MoveOutOfOrderRenameCreate`. |
| B-131 | Fix `userAgent` to use version constant | **DONE** — `userAgent` field on `Client`. |
| B-132 | Fix download hash mismatch infinite loop | **DONE** — 3-attempt retry loop. |
| B-133 | Track conflict copies in conflicts table | **DONE** — `ActionConflict` with `ConflictEditDelete`. |
| B-134 | Populate `remote_mtime` in conflict records | **DONE** — `RemoteMtime` field on `Outcome`. |
| B-135 | Promote `fsnotify` to direct dependency | **DONE** — `go mod tidy`. |
| B-136 | Drop unused `golang.org/x/sync` | **DONE** — `go mod tidy`. |
| B-138 (partial) | Use `http.Status*` constants | **DONE** — B-139. |
| B-140 | Set `Websocket: false` default | **DONE**. |
| B-141 | Warn on unimplemented config fields | **DONE** — `WarnUnimplemented()`. |
| B-142 | Remove dead `truncateToSeconds` | **DONE**. |
| B-143 | Remove `addMoveTargetDep` dead code | **DONE**. |
| B-144 | Update stale design docs post-Phase 5.0 | **DONE**. |
| B-145 | Add `tracker.go` API documentation | **DONE**. |
| B-146 | Inject logger into `shutdownCallbackServer` | **DONE**. |
| B-147 | Merge bootstrapLogger/buildLogger | **DONE** — Single `buildLogger(cfg)`. |
| B-149 (partial) | Deduplicate conflict scan | Note: B-149 remains open. |
| B-153 | Document hash-verify skip in `resolveTransfer` | **DONE**. |
| B-156 | `rm` command: warn about recursive deletion | **DONE** — `--recursive` flag. |
| B-159 | Document implicit `Content-Type` default | **DONE**. |
| B-161 | SQL comment for `action_queue.depends_on` | **SUPERSEDED** — Table dropped. |
| B-162 | Add `created_at` to `action_queue` | **SUPERSEDED** — Table dropped. |
| B-165 | Implement local trash support | **DONE** — Injectable `trashFunc`. |
| B-175 | Bounded DepTracker with spillover | **SUPERSEDED** — No spillover target. |
| B-177 | Canceled-context race in `failAndComplete` | **DONE** — Pool-level ctx. |
| B-178 | `events` channel never closed in `startObservers` | **DONE** — WaitGroup + close. |
| B-179 | No dead-observer detection | **DONE** — Observer count tracking. |
| B-180 | Undocumented baseline/token safety invariants | **DONE** — Comments added. |
| B-181 | Missing `DownloadOnly` observer skip test | **DONE**. |
| B-182 | Integration test for crash recovery | **DONE** — Phase 5.3. 11 tests. |
| B-183 | Dropped-event counter for `trySend` | **DONE** — `atomic.Int64` + `DroppedEvents()`. |
| B-184 | Reset backoff on successful safety scan | **DONE**. |
| B-185 | Test `trySend` channel-full path | **DONE** — 3 tests. |
| B-186 | Test `scanNewDirectory` recursive depth | **DONE** — 3-level dir tree. |
| B-187 | Split `observer_local.go` into two files | **DONE** — `observer_local_handlers.go`. |
| B-188 | Split `observer_local_test.go` | **DONE** — `observer_local_handlers_test.go`. |
| B-189 | Test backoff reset on safety scan | **DONE** — 2 tests. |
| B-190 | Fix cumulative drop counter → per-cycle reset | **DONE** — `ResetDroppedEvents()`. |
| B-191 | Document blocking sends in `RemoteObserver.Watch` | **DONE**. |
| B-192 | Document `timeSleep` cross-file dependency | **DONE** — Injectable `sleepFunc`. |
| B-193 (mock) | Make `mockFsWatcher.Close()` idempotent | **DONE** — `sync.Once`. |
| B-194 | Make safety scan ticker injectable | **DONE** — `safetyTickFunc` field. |
| B-195 | Document `DroppedEvents()` and mock watcher intent | **DONE**. |
| B-196 | Fix inaccurate scheduling-yield comment | **DONE**. |
| B-197 | Fix goroutine leak in double-call test | **DONE** — Cancel + drain. |
| B-198 (cache) | Baseline cache consistency via idempotent planner | **DONE** — Phase 5.4. Note: B-198 (periodic check in watch mode) remains open as a separate item. |
| B-199 | Eliminate global resolvedCfg | **DONE** — Context-based config. |
| B-008 | Spec inconsistency: chunk_size MB vs MiB | **DONE** — Fixed to MiB. |
| B-054 | Remove old sync code after Phase 4v2 | **SUPERSEDED** — Moved to Increment 0. |
| B-055 | Increment 0: delete old sync code | **DONE**. |
| B-056 | Remove `tombstone_retention_days` | **DONE**. |
| B-057 | Remove sync integration test line | **DONE**. |
| B-053 | Phase 4v2.1: Types + Baseline | **DONE** — PR #78. |
| B-059 | Phase 4v2.2: Remote Observer | **DONE** — PR #80. |
| B-065 | Phase 4v2.3: Local Observer | **DONE** — PR #82. |
| B-066 | Phase 4v2.4: Change Buffer | **DONE** — PR #84. |
| B-067 | Phase 4v2.5: Planner | **DONE** — PR #85. |
| B-068 | Executor must fill zero DriveID | **DONE** — PR #90. |
| B-070 | Add ParentID to Action struct | **CLOSED (by design)** — Dynamic resolution. |
| B-073 | DriveTokenPath/DriveStatePath accept CanonicalID | **DONE**. |
| B-052 | Re-enable E2E tests in CI | **DONE** — 4v2.8. |
| B-058 | Re-enable sync E2E tests | **DONE** — 4v2.8. |
| B-234 | Conflict ID prefix slicing panics on short IDs | **DONE** — `truncateID()` helper, 5 call sites. PR #130. |
| B-235 | Plan Actions/Deps length invariant not validated | **DONE** — Guard in `executePlan` and `processBatch`. PR #130. |
| B-236 | `StatePath()` duplicates `DriveStatePathWithOverride` logic | **DONE** — Simplified to delegation. PR #130. |
| B-237 | `hashAndEmit` infinite retry on `errFileChangedDuringHash` | **DONE** — `maxCoalesceRetries` cap (3 retries). PR #130. |
| B-250 | `findConflict` accepts empty string without early return | **DONE** — Added `idOrPath == ""` guard. PR #132. |
| B-251 | `resolveSingleKeepBoth`/`resolveSingleWithEngine` duplicate find+resolve logic | **DONE** — Extracted `resolveSingleConflict` shared helper. PR #132. |
| B-252 | `errAmbiguousPrefix` sentinel error loses the ambiguous prefix value | **DONE** — Changed to function returning `fmt.Errorf` with `%q` prefix. PR #132. |
| B-253 | `processBatch` invariant guard lacks rationale comment | **DONE** — Added comment explaining why log-only is sufficient. PR #132. |
| B-254 | `DownloadToFile` hash retry loop overflows on `maxRetries + 1` | **DONE** — `resolveMaxRetries` helper with `maxSaneRetries = 100` cap. PR #132. |
| B-255 | `handleCreate`/`scanNewDirectory` duplicate hash-failure handling | **DONE** — Extracted `stableHashOrEmpty` method. PR #132. |
| B-256 | `UploadFile` double file-open pattern undocumented | **DONE** — Added comment explaining intentional stat + open separation. PR #132. |
| B-257 | `resolveEachConflict` uses `fmt.Println` bypassing `statusf` | **DONE** — Changed to `statusf`. PR #132. |
| B-258 | `hashAndEmit` exhaustion log lacks retry context | **DONE** — Added `slog.Int("max_retries", maxCoalesceRetries)`. PR #132. |
| B-259 | `cancelPendingTimers` delete-during-range safety undocumented | **DONE** — Added Go spec comment. PR #132. |
| B-260 | `isAlwaysExcluded` calls `strings.ToLower` allocating on every check | **DONE** — `asciiLower` allocation-free helper. PR #132. |
| B-261 | `watchLoop` select priority semantics undocumented | **DONE** — Added comment about random priority and safety scan guarantee. PR #132. |
| B-262 | `removePartialIfNotCanceled` untested | **DONE** — 3 subtests: active context, canceled context, nonexistent file. PR #132. |
| B-263 | Reserved worker nil channel select semantics undocumented | **DONE** — Added Go spec comment. PR #133. |
| B-264 | `sendResult` lost-result edge case during shutdown undocumented | **DONE** — Added B-206 comment explaining benign drop. PR #133. |
| B-265 | Test: `DownloadToFile` rename failure preserves `.partial` | **DONE** — EISDIR rename failure test. PR #133. |
| B-266 | Test: session save failure still completes upload | **DONE** — Chmod-based save injection. PR #133. |
| B-267 | Test: `UploadFile` stat failure wraps `os.ErrNotExist` | **DONE** — `errors.Is` chain verification. PR #133. |
| B-268 | Test: non-RangeDownloader with existing `.partial` starts fresh | **DONE** — `tmSimpleDownloader` overwrites old partial. PR #133. |
| B-269 | Panic recovery error message not verified in test | **DONE** — Enhanced test checks "panic:" in Stats() and WorkerResult. PR #133. |
| B-270 | `resumeDownload` uses `computeQuickXorHash` instead of `tm.hashFunc` | **DONE** — Switched to `tm.hashFunc` for consistency. PR #133. |
| B-200 | Re-bootstrap CI token for new token format | **DONE** — Token format updated, E2E tests passing in CI. |
| B-289 | Remove dead `TokenSource` field from `DriveSession` | **DONE** — Post-6.0a hardening. Set but never read by any call site. |
| B-290 | Export `ResolveConfigPath` + add `CfgPath` to `CLIContext` | **DONE** — Post-6.0a hardening. Single correct config path resolution replaces `resolveLoginConfigPath` (which ignored `ONEDRIVE_GO_CONFIG`). 11 call sites simplified to `cc.CfgPath`. |
| B-291 | `ResolveDrive` returns `*Config` to eliminate double config load | **DONE** — Post-6.0a hardening. `loadAndResolve` was calling `LoadOrDefault` twice. Now `ResolveDrive` returns both `*ResolvedDrive` and `*Config`. |
| B-292 | Extract `newSyncEngine` helper for EngineConfig dedup | **DONE** — Post-6.0a hardening. `sync.go` and `resolve.go` had identical 15-line EngineConfig blocks. Extracted to `sync_helpers.go` with validation. |
| B-293 | `ReadEnvOverrides` double call in PersistentPreRunE | **DONE** — Moved to Phase 1, stored in `CLIContext.Env`, passed to `loadAndResolve`. Post-6.0a hardening round 2. |
| B-294 | LEARNINGS.md stale `configFromContext` reference | **DONE** — Updated to `cliContextFrom`/`mustCLIContext`/`cliContextKey`. Post-6.0a hardening round 2. |
