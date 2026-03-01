# Backlog

Historical backlog from Phases 1-4v1 archived in `docs/archive/backlog-v1.md`.

## Active (In Progress)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Ready (Up Next)

| ID | Title | Priority | Package | Notes |
|----|-------|----------|---------|-------|

## Phase 5.6: Identity Refactoring

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| ~~B-271~~ | ~~Personal Vault exclusion in RemoteObserver~~ | ~~P1~~ | **DONE** — Phase 5.6.1. `SpecialFolderName` field + `isDescendantOfVault()` parent-chain walk. |
| ~~B-272~~ | ~~Add `DriveTypeShared` to `driveid` package~~ | ~~P1~~ | **DONE** — Phase 5.6.2. Fourth drive type, type-specific field routing, part-count validation, `ConstructShared()`, `Equal()`, predicates, zero-ID fix. |
| ~~B-273~~ | ~~Move token resolution to `config` package~~ | ~~P1~~ | **DONE** — Phase 5.6.3. `config.TokenCanonicalID(cid, cfg)` in `token_resolution.go`. Removed method from `driveid`. |
| B-274 | Replace `Alias` with `DisplayName` in config | P1 | Phase 5.6.4. Auto-derived display names: email, "site / lib", "{Name}'s {Folder}". |
| B-275 | Update CLI for display_name | P2 | Phase 5.6.5. 3-tier `--drive` matching: display_name > canonical ID > substring. |
| B-276 | Delta token composite key migration | P2 | Phase 5.6.6. `(drive_id, scope_id)` key for per-shortcut delta tokens. |

## Hardening: Identity & Types

Defensive coding and edge cases for `internal/driveid/` and `internal/graph/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-278 | Standardize driveid test style (testify vs stdlib) | P4 | `edge_test.go` uses `testify/assert`, `canonical_test.go` uses stdlib. Pick one. |
| B-279 | Add `OwnerEmail` field to `graph.Drive` struct | P3 | Needed for shared drive display name derivation (B-274). Graph API provides `shared.owner.user.email`. |
| B-280 | Document `graph.User.Email` mapping to Graph API field | P4 | Ambiguous: `mail` vs `userPrincipalName`. Business accounts can differ. |
| B-281 | Vault parent-chain ordering assumption in RemoteObserver | P2 | `isDescendantOfVault()` assumes parents processed before children in delta. Not contractually guaranteed. Safety-critical. |
| B-282 | Add `HashesComputed` counter to `ObserverStats` | P4 | Planned in B-127 but may not have been implemented. Useful for perf diagnostics. |

## Hardening: Internal Sync

Defensive coding, bug fixes, and test gaps in `internal/sync/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-207 | Document intentional `.partial` preservation on rename failure | P4 | Add clarifying comment. |
| B-211 | `resumeDownload` TOCTOU race between stat and open | P4 | Extremely unlikely. Verify size after open if fixing. |
| B-221 | Add comment explaining Go integer range in hash retry loop | P4 | `range maxRetries + 1` is Go 1.22 syntax, unfamiliar to many. |
| B-222 | Document `selectHash` cross-file reference in `transfer_manager.go` | P4 | Aid code navigation without IDE. |

## Hardening: CLI Architecture

Code quality and architecture improvements for the root package.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-223 | Extract `DriveSession` type for per-drive resource lifecycle | P1 | Replace `clientAndDrive()`. Prerequisite for multi-drive. |
| B-224 | Eliminate global flag variables (`flagJSON`, `flagVerbose`, etc.) | P1 | Move to `CLIFlags` struct in `CLIContext`. Eliminates test pollution. |
| B-227 | Deduplicate sync_dir and StatePath validation across commands | P3 | Extract `RequireSyncDir()` and `RequireStatePath()` on `CLIContext`. |
| ~~B-228~~ | ~~`buildLogger` silent fallthrough on unknown log level~~ | ~~P3~~ | Fixed in Phase 5.5: added `warn` case and `default` with stderr warning. |
| B-232 | Test coverage for `loadConfig` error paths | P3 | Invalid TOML, ambiguous drive, wrong context type, unknown log level. |
| B-036 | Extract CLI service layer for testability | P4 | Root package at 28.1% coverage. Target 50%+. |
| B-229 | `syncModeFromFlags` uses `Changed` instead of `GetBool` | P4 | Subtle Cobra invariant. Document or fix. |
| B-230 | `printSyncReport` repetitive formatting | P4 | Extract `printNonZero` helper. |
| B-233 | `version` string concatenation in two places | P4 | Minor duplication. Fixed by `DriveSession` (B-223). |

## Hardening: Graph API

Edge cases and correctness for `internal/graph/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-020 | SharePoint lock check before upload (HTTP 423) | P2 | Avoid overwriting co-authored documents. |
| B-021 | Hash fallback chain for missing hashes | P2 | Some Business/SharePoint files lack any hash. Fall back: QuickXorHash → SHA256 → size+eTag+mtime. |
| B-007 | Cross-drive DriveID handling for shared/remote items | P3 | Verify against real API responses in E2E. |

## Hardening: Watch Mode

Improvements to continuous sync reliability in `internal/sync/`.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-101 | Add timing and resource logging to safety scan | P3 | Elapsed time, files walked, directories scanned. |
| B-115 | Test: safety scan + watch producing conflicting change types | P3 | Watch sees Create, safety scan classifies same file as Modify. Planner handles it but no test. |
| B-120 | Symlinked directories get no watch and no warning | P3 | Log Warn when symlinked directory encountered during watch setup. |
| B-125 | No health or liveness signal from Watch() goroutines | P4 | Detect stuck/dead observers. Heartbeat or periodic liveness signal. |
| B-127 | No observer-level metrics or counters | P4 | Events produced, polls, hashes, dropped events. Essential for long-running daemon. |
| B-128 | Debounce semantics change under load | P4 | When consumer is busy, debounce blocks and timer stops running. |

## Hardening: Code Quality

Misc improvements across packages.

| ID | Title | Priority | Notes |
|----|-------|----------|-------|
| B-138 | Add upstream sync check for oauth2 fork | P3 | `tonimelisma/oauth2` fork may fall behind security patches. CI check or documented process. |
| B-149 | Deduplicate conflict scan logic in `baseline.go` | P3 | `scanConflictRow`/`scanConflictRowSingle` — 80 lines duplicated. |
| B-160 | Drop or document `conflicts.history` column | P3 | Unused column in schema. |
| B-198 | Periodic baseline cache consistency check in watch mode | P3 | Every N cycles, reload from DB and compare with cache. Defensive against silent corruption. |
| B-071 | ConflictRecord missing Name field | P4 | UX convenience. `path.Base(Path)` suffices. |
| B-087 | Conflict retention/pruning policy | P4 | Resolved conflicts accumulate forever. Add configurable retention (e.g., 90 days). |
| B-154 | Sort map keys in planner for reproducible action order | P4 | Non-deterministic map iteration aids debugging. |
| B-158 | `DownloadURL`: implement `slog.LogValuer` for compile-time redaction | P4 | "NEVER log" is convention-only. |

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

## CI / Infrastructure

| ID | Title | Priority | Notes |
|----|-------|----------|-------|

## Closed

| ID | Title | Resolution |
|----|-------|------------|
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
| B-121 | Delta token and baseline not atomically consistent | **DONE** — Phase 5.2. `cycleTracker`. |
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
