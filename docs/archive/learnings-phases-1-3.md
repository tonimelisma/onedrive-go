# Learnings — Institutional Knowledge Base

Knowledge captured during implementation. Patterns, gotchas, and design decisions that future agents need.

---

## 1. QuickXorHash (`pkg/quickxorhash/`)

**Plan test vectors were wrong.** The test vectors specified in the plan were incorrect. Verified against rclone v1.73.1's `quickxorhash` package, which is verified against Microsoft's reference C# implementation. Lesson: always verify test vectors against a known-good reference, don't blindly trust specs.

**Code quality:** 100% coverage, pure Go, clean `hash.Hash` interface. BSD-0 attribution to rclone. The `[3]uint64` circular buffer with bit-straddling logic is well-commented.

---

## 2. State Store (`internal/state/`)

**scanner/execer interfaces:** Clean abstraction that lets the same upsert logic work for both `*sql.DB` and `*sql.Tx`. Good pattern — reuse it.

**Soft-delete tombstone strategy:** Items get `is_deleted=1` + `deleted_at` timestamp. `PurgeTombstones(cutoff)` does the actual DELETE. Correct for sync engines that need to track deletions temporarily.

**Pragmas:** WAL + FULL synchronous + foreign_keys ON + 64 MiB journal limit. Conservative and correct for a sync engine.

**DriveID in items:** Both `DriveID` and `ParentDriveID` come from the parent reference context in delta responses. For cross-drive shared items (ItemType = remote), this might produce incorrect DriveID — verify against real API data.

**MarkDeleted is a thin alias:** `MarkDeleted(driveID, itemID)` just calls `DeleteItem(driveID, itemID)`. Acceptable for semantic clarity at call sites.

---

## 3. Normalizer (`internal/normalize/`)

**Skipped SharePoint enrichment flag (#11):** Plan was vague — agent reasonably skipped it. Now resolved: per-side hash baselines handle enrichment naturally, no enrichment-specific flag needed. See SHAREPOINT_ENRICHMENT.md.

**DriveID from ParentReference only:** Works for delta responses (per-drive), but cross-drive shared items may need different handling. Tracked in BACKLOG.md as B-007.

**NormalizedItem has fewer fields than planned (16 vs ~26).** Missing fields were things the sync engine will compute. Struct should only contain what normalization produces.

**Deletion reordering:** `sort.SliceStable` with same-path deletions before creations. Preserves relative order for different paths.

---

## 4. Filter Engine (`internal/filter/`)

**Three-layer architecture:** sync_paths -> config patterns -> .odignore. Each layer is a separate method. `Result{Allowed bool, Reason string}` is good for debuggability.

---

## 5. Cross-Cutting Patterns

### Parallel agent execution
Four agents ran simultaneously without conflicts (true leaf packages). Scoped verification commands to own package to avoid cross-agent interference:
```bash
# Good: scoped to own package
go test ./internal/filter/...
# Bad: tests all packages, sees intermediate states from other agents
go test ./...
```

### Linter compliance
See CLAUDE.md "Linter Patterns" section for the full list. Key ones:
- `hugeParam`: DriveItem (408B), Item (240B) → pass as pointers
- `mnd`: Every number needs a named constant; tests exempt
- `funlen`: Max 100 lines → decompose
- `errcheck`: `defer tx.Rollback()` exempt, `_ = tx.Rollback()` not
- `rangeint`: `for range N` not `for i := 0; i < N; i++`

### Test infrastructure
- `OpenInMemory()` for all state tests — fast, isolated
- `ptrStr(s)`, `ptrInt64(n)` helpers for nullable fields
- `newTestItem(id, name, type)` with hardcoded driveID
- Never pass nil context — runtime panics, not caught by compiler/linter

### Design decision: SharePoint enrichment
Per-side hash baselines (not download-after-upload). See SHAREPOINT_ENRICHMENT.md for full analysis. Key insight: single synced hash can't satisfy both local and remote comparisons when server-side enrichment changes the file. Per-side baselines handle it naturally with zero enrichment-specific code.

---

## 6. Config Package (`internal/config/`)

### Pivots from plan

**Chunk size default changed from "10MB" to "10MiB".** The design spec says chunk_size default is "10MB" and must be a multiple of 320 KiB. But 10 MB (decimal, 10,000,000 bytes) is NOT a multiple of 320 KiB (327,680 bytes). 10 MiB (10,485,760) IS a multiple (32 * 327,680). Changed default to "10MiB" to maintain alignment validation. The transfer layer and user-facing docs should document this. Future increment 2.4 (uploads) should accept "10MB" and round to nearest aligned value for convenience, or the spec should be updated.

**`ApplyEnvOverrides` replaced with `ReadEnvOverrides`.** The original plan called for `ApplyEnvOverrides(cfg *Config) *Config` but since profiles are not in scope for 2.1 (no sync_dir field on Config), there is nothing to apply ONEDRIVE_GO_SYNC_DIR to. Instead, `ReadEnvOverrides()` returns a struct with the raw env var values. The profile increment (2.2) will apply sync_dir and profile overrides to the resolved config.

**No `go-humanize` dependency for size parsing.** The filter package uses `go-humanize` for size parsing, but importing `internal/filter` from `internal/config` would create a dependency direction concern. Instead, config has its own lightweight size parser supporting KB/MB/GB/TB (decimal) and KiB/MiB/GiB/TiB (binary) suffixes. If a unified parser is needed later, extract to a shared `internal/units/` package.

### Issues found

**Spec inconsistency: chunk_size units.** The configuration.md spec lists "Valid values: 10MB, 15MB, 20MB..." as multiples of 320 KiB, but none of these decimal MB values are actually multiples of 320 KiB. The spec conflates MB (decimal) with MiB (binary). This should be clarified in the spec. See BACKLOG.md B-008.

### Weird observations

**`toml.MetaData` is 96 bytes** (triggers hugeParam). Must be passed by pointer. The `toml.DecodeFile` returns it by value, so we take its address when passing to helper functions.

**`replace_all` in Edit tool replaces string literals inside const blocks too.** When replacing a string with a constant name using `replace_all`, the const declaration itself gets mangled (`invalidSizeStr = invalidSizeStr`). Always define constants BEFORE doing replace_all, or do targeted replacements.

**misspell linter catches intentional typos in test TOML strings.** Test data for unknown-key-detection uses deliberate misspellings. Must use `//nolint:misspell` on those lines. Inline TOML strings (not raw string literals with backticks) make the nolint placement cleaner.

### Suggested improvements

- Extract size parsing to `internal/units/` to share between config and filter packages.
- Add config file generation (`WriteDefault`) for the `config init` wizard (Phase 4).
- Bandwidth limit/schedule validation (currently validates schedule times and sort order, but does not validate the limit string format like "5MB/s"). This is a future concern for increment 2.5.
- Consider adding `config.ParseDuration()` wrapper that adds user-friendly error messages for common mistakes (e.g., "5min" instead of "5m").

### Cross-package concerns

- **Chunk size units**: The transfer package (2.4) must be aware that chunk_size is stored as a human-readable string and needs parsing + alignment rounding at runtime. Use `MiB` suffix in docs.
- **Config ↔ Filter**: The filter package has its own `Config` struct. When the config package resolves profiles (2.2), it will need to map `config.FilterConfig` to `filter.Config`. Keep field names aligned.
- **Env overrides**: ONEDRIVE_GO_SYNC_DIR applies to profiles (2.2 scope), not global config. The `ReadEnvOverrides()` function is intentionally decoupled from `Config` for this reason.

---

## 7. Transfer Package (`internal/transfer/`)

### Pivots from plan
- **Disk space check injectable:** `CheckDiskSpace` uses `syscall.Statfs` which requires real filesystem paths. Made the disk space checker a function field (`SpaceCheckFunc`) on Downloader so tests can set it to nil. This is cleaner than trying to mock syscall.
- **No `DownloadToWriter` on existing client:** The plan correctly noted that `pkg/onedrive/Client` doesn't have a `DownloadToWriter` method — it writes directly to files. The `APIClient` interface defines the signature the transfer package needs; a real adapter will be built in Phase 3.
- **`Filesystem.Chtimes` added to interface:** The plan's Filesystem interface didn't include `Chtimes`, but downloads need to set mtime after rename. Added it.
- **`HashingWriter.inner` field removed:** Plan spec had an `inner` field on HashingWriter for the original target writer. It was unused — the `io.MultiWriter` handles both targets. Removed to avoid unused field lint.

### Issues found
- **Mock filesystem uses real temp files:** The mock Create/OpenFile implementations create real temp files (via `os.CreateTemp`) to return `*os.File`. This means mock rename/remove operations don't perfectly track content through the mock's map. For the tests we have this is fine, but a more sophisticated mock (or `afero`) might be needed for complex integration tests.

### Weird observations
- **`hugeParam` thresholds:** DownloadJob (112B) and UploadJob (104B) both trigger `hugeParam` on gocritic. Pass by pointer.
- **`gosec` G115 integer overflow:** Converting `syscall.Statfs_t.Bavail` (uint64) * `Bsize` (uint32) to int64 triggers G115. Fixed with `clampToInt64` helper that caps at `math.MaxInt64`.
- **`unconvert` on darwin:** `stat.Bavail` is already uint64 on darwin, so `uint64(stat.Bavail)` is flagged as unnecessary conversion. The explicit cast is not needed.
- **`nolintlint` strictness:** An `//nolint:gosec` on a line that gosec doesn't flag (after the overflow was fixed via clamping) is flagged by `nolintlint` as unused. Always verify nolint directives are still needed after fixing the underlying issue.
- **Time zone in test assertions:** Chtimes mock stores `time.Time` values, but comparing `time.Date(2025, ..., time.UTC)` with the stored value fails if the local timezone differs from UTC. Compare using `.UnixNano()` instead of direct equality.

### Suggested improvements
- **Bandwidth limiting:** The plan mentions token-bucket bandwidth scheduling. Not in scope for 2.3/2.4 but should be added to the transfer package later (likely as a `io.Reader`/`io.Writer` wrapper).
- **Retry logic:** Neither downloader nor uploader retries failed operations. The worker pool layer (2.5) or the sync engine should add retry with exponential backoff.
- **Resumable upload recovery:** Current `RecoverSessions` cancels stale sessions rather than resuming them. Full resume (seek to BytesUploaded, re-upload remaining chunks) is deferred to later — requires re-opening the local file and verifying it hasn't changed.
- **Linux portability:** `diskspace.go` uses `syscall.Statfs` which works on both darwin and linux, but field types may differ (e.g., `Bsize` is `int64` on linux vs `uint32` on darwin). The current code compiles on darwin; verify on linux CI.

### Cross-package concerns
- **No adapter needed:** After the 3.8 clean-slate rewrite, `*onedrive.Client` satisfies `transfer.APIClient` directly via duck typing. Methods were renamed to match: `DownloadByID`, `SimpleUpload`, `CreateUploadSession`, `UploadChunk`, `GetUploadSessionStatus`, `CancelUploadSession`.
- **`SessionStore` implemented by `state.Store`:** The `SessionStore` interface matches `state.Store`'s upload session methods exactly, so `*state.Store` satisfies `SessionStore` with no adapter needed.

---

## 8. Config Profiles (`internal/config/` increment 2.2)

### Pivots from plan

**No pivots.** Implementation followed the plan closely. The `resolveSection` helper uses Go generics (`resolveSection[T any]`) to avoid repeating the "profile override or global fallback" logic for each of the 6 config sections.

### Issues found

**None.** Existing test suite remained green throughout. The `toml:"profile"` tag on `map[string]Profile` correctly handles `[profile.NAME]` sections in TOML automatically, as documented.

### Weird observations

**`toml.Key.String()` uses dot separators.** When the TOML library reports undecoded keys, it joins the key path with dots. For profile keys like `[profile.work]` with field `sync_dir`, the key string is `profile.work.sync_dir`. The unknown key classifier must split on dots and count parts to distinguish between `profile.NAME.field` (3 parts) and `profile.NAME.section.field` (4 parts).

**misspell linter does not flag all misspellings.** `acount_type` (missing 'c') is not flagged by misspell because it only knows common English word misspellings, not domain-specific identifiers. No `//nolint:misspell` is needed for test strings containing such typos (unlike `parralel_downloads` which contains the English word "parallel").

**Duplicate sync_dir detection uses tilde-expanded paths.** Both `~/OneDrive` and `/Users/name/OneDrive` resolve to the same path, so the duplicate detection expands tildes before comparison.

### Suggested improvements

- **Profile-scoped error messages:** Per-profile section override validation reuses the same validators as global sections, so error messages say "filter.ignore_marker: must not be empty" rather than "profile.work.filter.ignore_marker: must not be empty". A future improvement could prefix error messages with the profile name for clarity.
- **Profile name validation:** Currently any string is accepted as a profile name. Consider restricting to alphanumeric + hyphens + underscores to avoid filesystem issues in DB/token paths.

### Cross-package concerns

- **CLI integration (Phase 4):** `--profile` flag should pass the profile name to `ResolveProfile`. `--sync-dir` flag should override `resolved.SyncDir` after resolution (same pattern as ONEDRIVE_GO_SYNC_DIR env var).
- **State store (Phase 3):** `ProfileDBPath(profileName)` returns the per-profile database path. The sync engine should call this when opening the state store for a profile.
- **Auth (Phase 4):** `ProfileTokenPath(profileName)` returns the per-profile token file path. The login/logout commands should use this.

---

## 9. Worker Pools + Bandwidth (`internal/transfer/` increment 2.5)

### Pivots from plan
- **Manager is a concrete struct, not an interface:** The architecture spec defines Manager as an interface, but the implementation uses a concrete struct. This is simpler for now; an interface can be extracted when the sync engine needs to mock it (Phase 3).
- **Bandwidth limiter not wired into download/upload I/O streams:** The BandwidthLimiter exists as a standalone rate limiter but is not yet integrated into the Downloader/Uploader's I/O path. The Manager exposes the limiter, and future integration can wrap the io.Reader/io.Writer passed to the API client. This was deferred because modifying the Downloader/Uploader would break the 2.3/2.4 code boundary.
- **No retry logic added:** Retry with exponential backoff belongs in the sync engine (Phase 3), not the transfer manager, since retry policies are operation-type-specific.

### Issues found
- **Token bucket polling:** The bandwidth limiter uses polling (`time.After(5ms)`) to check token availability rather than a condition variable. This is simple and correct but wastes CPU cycles under heavy throttling. A `sync.Cond`-based approach would be more efficient but harder to make cancellation-safe. Acceptable for the expected workload.

### Weird observations
- **`time.After` in tight loops:** Using `time.After` in the `waitForTokens` loop creates a new timer per iteration. Under heavy load this could cause GC pressure. The `time.NewTimer` + `Reset` pattern would be more efficient, but for bandwidth limiting (where waits are milliseconds to seconds), the simpler approach is fine.
- **`clampPoolSize` returns DefaultPoolSize for zero/negative:** The spec said "Zero/negative pool size uses default" which is what we do. But the spec also said `MinPoolSize = 1`, so strictly clamping to MinPoolSize would give 1 instead of 8. We follow the spec's behavioral description (use default) rather than the mathematical interpretation (clamp to min).
- **Schedule entry order matters:** `findActiveEntry` assumes entries are sorted chronologically. No sort is applied -- the config package (2.1) already validates and sorts schedule entries. If entries are out of order, the wrong entry may be selected.

### Suggested improvements
- **Integrate bandwidth limiter into I/O streams:** Wrap the `io.Reader`/`io.Writer` in the Downloader/Uploader with a bandwidth-limited wrapper that calls `limiter.Wait()` before each chunk. This gives per-byte throttling rather than per-operation throttling.
- **Use `sync.Cond` for bandwidth limiter:** Replace polling loop with condition variable signaled on token refill. More efficient under heavy throttling.
- **Manager interface extraction:** When the sync engine (Phase 3) needs to mock the Manager, extract a `TransferManager` interface with `Download`, `Upload`, `CheckHash`, `Shutdown` methods.
- **Pool metrics:** Add counters for total operations, in-flight operations, and wait times per pool. Useful for monitoring and debugging.

### Cross-package concerns
- **Config integration:** The config package defines max workers as 16, but the Manager accepts up to 64 (MaxPoolSize). The config validates the limit, the transfer package just enforces a sane upper bound. When wiring config to Manager, use the config's validated values directly.
- **Schedule entries from config:** `ScheduleEntry.Limit` is `int64` (bytes/sec). The config package stores bandwidth limits as human-readable strings ("5MB/s"). The conversion from string to int64 happens in the config package, not the transfer package.
- **Shutdown ordering:** The sync engine must call `Manager.Shutdown()` before closing the state store, since in-flight uploads may still write session progress to the store.

---

## 10. Local Scanner (`internal/sync/scanner.go` increment 3.2)

### Pivots from plan

**ItemID for new local files uses `local:` + UUID instead of empty string.** The spec said `ItemID = ""` for new files, but the items table has `PRIMARY KEY (drive_id, item_id)`. Multiple new files with the same driveID and empty ItemID would collide on upsert. Used `local:` prefix + UUID to generate unique IDs that are clearly identifiable as local-only items. The reconciler or upload logic should recognize this prefix and replace with the server-assigned ID after upload.

**Scan count semantics: new/modified only.** The spec said "returns the number of files processed" but didn't define whether unchanged (fast-path) files count. Implemented as: only new or modified items increment the count. Unchanged files (fast path hit) return 0. Directories always count as 1.

### Issues found

**Pre-existing gofmt issue in `delta.go`.** The `convertNormalizedToState` struct literal had extra alignment spaces that gofumpt normalized. Fixed as part of the scanner work since it's in the same package.

**Test name collisions with `delta_test.go`.** The existing delta tests define `testDriveID` and `newMockStore()`. Scanner tests needed distinct names (`scannerDriveID`, `newScannerMockStore`, etc.) to avoid redeclaration errors. All test files in the same package share a namespace.

### Weird observations

**`govet` shadow detection is aggressive.** In `processEntry`, an `if err := f(); err != nil { }` block inside a function that already has `err` from a prior call triggers the shadow warning, even though the scopes don't overlap in a confusing way. Fixed by extracting into separate helper methods (`processDirectory`, `processAndCountFile`).

**`os.ReadDir` returns `DirEntry` which has `Type()` for symlink detection.** This is more efficient than calling `Lstat` separately. The `entry.Type()&os.ModeSymlink != 0` check works correctly on both macOS and Linux.

### Suggested improvements

- **Batch upserts:** The scanner currently upserts items one at a time. For large sync roots (100K+ files), this could be slow. Consider batching upserts using `UpsertItems` (already exists on `state.Store`) with configurable batch size.
- **Progress reporting:** The scanner has no callback mechanism for reporting scan progress. The sync engine UI (Phase 4) will want progress updates. Consider adding a `ScanProgress` callback field on `Scanner`.
- **Parallel hashing:** For the slow path, hash computation is sequential. The transfer package already has worker pools that could be reused for parallel hash computation during scanning.

### Cross-package concerns

- **`local:` prefix convention:** The reconciler (increment 3.4) and upload logic (Phase 4) must recognize `local:` prefixed ItemIDs as locally-created items that need to be uploaded and have their ItemID replaced with the server-assigned value. This is a convention, not enforced by the type system.
- **`ListSyncedItems` query added to `state.Store`:** This new method queries `WHERE is_deleted = 0 AND synced_hash IS NOT NULL`. If the state schema changes (e.g., a dedicated `sync_status` column), this query needs updating.
- **Filter path format:** The filter engine expects paths without a leading slash (e.g., `Documents/file.txt`), while the scanner stores paths with a leading slash (`/Documents/file.txt`). The scanner strips the leading slash before calling `ShouldSync`. This mismatch is worth documenting.

---

## 11. Delta Processor (`internal/sync/delta.go` increment 3.1)

### Pivots from plan

**Drive ID normalization affects test lookups.** The `NormalizeDriveID` function lowercases and zero-pads personal drive IDs to 16 characters. Tests that use short drive IDs (e.g., `"drive-001"`) will find items stored under the normalized ID (e.g., `"0000000drive-001"`). Fixed by using a 16-char lowercase drive ID in tests so normalization is a no-op.

**Scanner already exists in `internal/sync/`.** The `scanner.go` file defines `ptrStr` and `ptrInt64` helpers plus `mockStore`/`testDriveID` test constants. Delta test types use distinct names (`deltaStoreMock`, `deltaAPIMock`, `deltaDriveID`, etc.) to avoid redeclaration conflicts within the same test package namespace.

**HTTP 410 now uses `ErrGone` sentinel.** As of increment 3.8, `pkg/onedrive` defines `ErrGone` for HTTP 410 Gone responses. The delta processor detects 410 via `errors.Is(err, onedrive.ErrGone)` instead of the previous string-matching approach.

### Issues found

**Migration test version hardcoded.** `TestSchemaVersionIsCorrect` and `TestMigrationIsIdempotent` in `migrate_test.go` had the expected schema version hardcoded to `1`. Adding migration `002_delta_complete.sql` broke these tests. Updated to expect version `2`.

### Weird observations

**`SaveDeltaToken` does not touch `complete` column.** The ON CONFLICT clause only updates `token` and `updated_at`, which is correct -- saving a new token should not reset the complete flag. This was verified with a dedicated test.

### Suggested improvements

- ~~**Add `ErrGone` sentinel error to `pkg/onedrive`:**~~ Done in increment 3.8. Delta processor now uses `errors.Is(err, onedrive.ErrGone)`.
- **Transactional page application:** Currently, each page's items are applied individually. For true atomicity, all pages could be applied in a single transaction, with the delta token saved at the end. This would prevent partial state on crash between pages. However, for large delta responses this could hold the DB lock too long.
- **Batch size configuration:** The batch size (100) is hardcoded as `defaultBatchSize`. Consider making it configurable via the `DeltaProcessor` constructor for tuning.

### Cross-package concerns

- **`DeltaStore` interface vs `*state.Store`:** The `DeltaStore` interface matches `*state.Store` methods exactly, so `*state.Store` satisfies `DeltaStore` with no adapter needed. The new `SetDeltaComplete`/`GetDeltaComplete` methods were added to `checkpoints.go` with migration `002_delta_complete.sql`.
- **Normalizer dependency:** The delta processor takes a `*normalize.Normalizer` which requires an optional `ItemLookup` for deleted item name recovery. When wiring up the real processor, pass a state store adapter that implements `normalize.ItemLookup` for better deleted item handling.
- **Reconciler (next increment):** The reconciler will need to check `GetDeltaComplete` to know whether a full delta enumeration has been done before reconciling. The `complete` flag prevents reconciling against a partially synced remote state.

---

## 12. Conflict Handler (`internal/sync/conflict.go` increment 3.6)

### Pivots from plan

**`ConflictStore.RecordConflict` signature mismatch.** The spec defined the `ConflictStore` interface with `RecordConflict(rec *state.ConflictRecord) error`, but `state.Store.RecordConflict` returns `(string, error)` (the generated UUID). The `ConflictStore` interface uses the simpler signature since the handler generates its own UUID before calling the store. The real `*state.Store` will need a thin adapter or the interface can be updated to match. Alternatively, a wrapper method on `*state.Store` that discards the returned string could satisfy the interface.

**`insertCollisionSuffix` uses conflict marker awareness, not `filepath.Ext`.** The original plan's `insertSuffix` used generic "insert before extension" logic, but `filepath.Ext` misidentifies the conflict timestamp as an extension for files without original extensions (e.g., `Makefile.conflict-20260218-143052`). The implementation locates the `.conflict-` marker and inserts the collision number after the fixed-length timestamp portion. This is more robust and correctly handles all naming patterns.

**Exhaustive switch required.** The `exhaustive` linter requires all `ConflictType` values to be handled in the switch statement. Added explicit cases for `DeleteEdit`, `TypeChange`, and `CaseConflict` that return unsupported errors. These types will be implemented as the sync engine matures.

### Issues found

**Pre-existing reconciler test failures.** The `reconciler.go` and `reconciler_test.go` files are untracked (work in progress from another increment). Two tests (`TestLocalMoveDetectionUniqueMatch`, `TestLocalMoveDetectionAmbiguous`) fail. Also has 4 lint issues (1 exhaustive, 3 gocyclo). These are not related to the conflict handler.

### Weird observations

**`filepath.Ext` on conflict-named files.** For `Makefile.conflict-20260218-143052`, `filepath.Ext` returns `.conflict-20260218-143052` since it treats everything after the last dot-preceded segment as the extension. This is technically correct per Go's definition but semantically wrong for our naming convention. The `insertCollisionSuffix` function works around this by using string search for the `.conflict-` marker instead.

**`gochecknoglobals` catches test time variables.** The `conflictTestTime` variable in tests is flagged by `gochecknoglobals`. Suppressed with `//nolint:gochecknoglobals` since test fixtures are a legitimate use of package-level variables.

### Suggested improvements

- **DeleteEdit handler:** Currently returns an error. A reasonable resolution would be: download the remote modified file to the original path (remote wins), since the local user deleted it. Or keep as-is and let the user decide.
- **TypeChange and CaseConflict handlers:** These are complex edge cases that will need careful design. TypeChange (file became folder or vice versa) likely needs both sides preserved. CaseConflict needs OS-specific handling.
- **Conflict history JSON:** The `ConflictRecord.History` field is always nil. Future work could append resolution history entries as JSON for audit trails.
- **Sync root prefix for rename:** The `Resolve` method receives relative paths and the filesystem mock handles them. When integrated with the real executor, the sync root prefix must be prepended to paths before calling `fs.Rename`.

### Cross-package concerns

- **`ConflictStore` adapter needed:** `state.Store.RecordConflict` returns `(string, error)` but `ConflictStore` expects `error`. A thin adapter or interface wrapper will be needed when wiring the conflict handler to the real state store.
- **Executor integration:** The executor (increment 3.5) will call `ConflictHandler.Resolve()` for each `Conflict` action in the plan, then execute the replacement actions. The executor needs to handle the case where `Resolve` returns an error (e.g., rename fails) and record it as a `SyncError`.
- **ConflictInfo on reconciler actions:** The reconciler must populate `ConflictInfo` on `Conflict` actions with accurate `LocalHash` and `RemoteHash` values. These come from `state.Item.LocalHash` and `state.Item.QuickXorHash` respectively.

---

## 13. Reconciler + Safety (`internal/sync/reconciler.go` increments 3.3+3.4)

### Pivots from plan

**Decision functions decomposed aggressively to satisfy `gocyclo` (max 15).** The original design had two large switch statements (`decideFileAction` with 9 cases, `decideFolderAction` with 7 cases) that each exceeded the cyclomatic complexity limit. Decomposed into focused helper functions: `decideFileSynced`, `decideBothChanged`, `decideLocalAbsent`, `decideRemoteAbsent`, `decideOneSideChanged`, `decideFileUnsynced`, `decideFileNewBothPresent`, `decideFolderSynced`, `decideFolderUnsynced`. This is more verbose but each function has clear single responsibility and stays within complexity bounds.

**Folder `localPresent` detection simplified.** The spec had extensive discussion about how to detect whether a folder exists locally (scanner doesn't set LocalHash/LocalMTime for folders). Used `LocalMTime != nil || LocalHash != nil` for folder local presence. This works because the scanner would need to be enhanced to set LocalMTime for folders (it currently only sets Path). For now, folder presence detection relies on the executor/scanner setting these fields after initial sync.

**`ListAllItems` instead of `ListAllActiveItems`.** The spec initially proposed `ListAllActiveItems` (non-deleted only), but the reconciler needs to see tombstoned items to handle F8 (remote deleted -> local delete), F9 (edit-delete conflict), F14 (both deleted -> cleanup), D3 (folder remote deleted), and D4 (folder both deleted). Changed to `ListAllItems` which returns all items including tombstones.

**Move detection matches synced hash from delete against local hash from upload.** The delete's `SyncedHash` (the baseline from last sync) is matched against the upload's `LocalHash` (the current local content). This correctly detects "file was renamed locally" because the content hash is the same but the path changed. Marking consumed uploads by clearing `ItemID` to empty string is a simple sentinel approach.

### Issues found

**`exhaustive` linter caught missing `Bidirectional` case in `isSuppressed`.** The switch on `SyncMode` used a `default` case, but the `exhaustive` linter requires all enum values to be explicit. Added explicit `Bidirectional` case that returns false. The `exhaustive` linter also caught missing cases in the pre-existing `conflict.go` `resolveByType` switch -- fixed by adding `DeleteEdit`, `TypeChange`, `CaseConflict` cases.

**Move detection test data initially incorrect.** First attempt used `QuickXorHash=nil` and `IsDeleted=false` for the delete item, which made it fall into F14 (both absent cleanup) instead of F6 (local absent, remote unchanged -> RemoteDelete). The delete item must have `QuickXorHash` matching `SyncedHash` (remote still present and unchanged) for the reconciler to produce a RemoteDelete action.

### Weird observations

**`_ = it` pattern for unused parameters.** The `decideOneSideChanged` and `decideFolderAction` functions receive `*state.Item` for signature consistency with the decision table pattern, but some branches don't use it directly. The `_ = it` assignment suppresses the `unparam` linter. This is a deliberate trade-off: uniform function signatures make the decision table easier to read and extend.

**`percentDivisor` constant for `100`.** The `mnd` linter requires named constants for all numbers except 0-3 and HTTP status codes. Using `100` in `pct := totalDeletes * 100 / totalItems` requires a named constant `percentDivisor`.

### Suggested improvements

- **Folder local presence from scanner.** The scanner currently does not set `LocalMTime` for folders, so folder local presence detection is unreliable. The scanner should be enhanced to set `LocalMTime` for folders (from `os.Stat` of the directory). This would make D1-D7 work correctly.
- **Move detection for folders.** Current move detection only handles files (RemoteDelete + Upload pairs). Folder moves are more complex and would need path-based detection rather than hash-based.
- **Stale item detection.** Items with no local state, no remote state, and no synced state should be cleaned up. The current code returns nil for these (no action), but they could accumulate in the DB.

### Cross-package concerns

- **`ReconcilerStore` interface vs `*state.Store`.** Two new methods added to `state.Store`: `ListAllItems(driveID)` and `CountItems(driveID)`. Combined with the existing `GetDeltaComplete`, these satisfy the `ReconcilerStore` interface. No adapter needed.
- **`local:` prefix items.** Items created by the scanner with `local:` prefix ItemIDs will have `LocalHash` set but no `QuickXorHash` or `SyncedHash`. They correctly fall into F12 (Upload new) in the decision table.
- **Safety checks integration.** The sync engine should call `Reconcile` first, then `ApplySafetyChecks` on the resulting plan. If safety checks fail, the engine should abort the sync with a clear error message and suggest `--force`.

---

## 14. Executor + Engine (`internal/sync/executor.go`, `engine.go` increment 3.5+3.7)

### Pivots from plan

**`EngineConfig` passed by pointer, not value.** The `EngineConfig` struct is 80 bytes due to the embedded `SafetyConfig`. The `hugeParam` gocritic check flagged it, so `NewEngine` accepts `*EngineConfig` instead of `EngineConfig`. This is consistent with the pattern used for other heavy structs like `DriveItem` and `Item`.

**`executeReplacement` exhaustive switch.** The `exhaustive` linter requires all `ActionType` cases to be present in switch statements. The replacement action handler only supports `Download`, `Upload`, and `UpdateSynced`, but needed explicit cases for the remaining action types (`LocalDelete`, `RemoteDelete`, `LocalMove`, `RemoteMove`, `FolderCreate`, `Conflict`, `Cleanup`) returning unsupported errors. This is the pattern used elsewhere in the codebase (e.g., `conflict.go` `resolveByType`).

**Cleanups are no-ops, just counted.** The spec suggested various approaches (hard delete, clear fields, etc.) but the simplest is correct: both sides are already deleted/tombstoned. The tombstone purger will handle actual DB cleanup. The executor just counts them for the report.

**Remote folder creation skipped implicitly.** The spec noted that `FolderCreate` for local-only folders (D5/D6) would need API calls not yet available. However, `MkdirAll` is a no-op for directories that already exist, so we just call `MkdirAll` for all `FolderCreate` actions. If the folder already exists locally (D5/D6 case), nothing happens. If it needs to be created locally (D2/D7 case), it gets created.

### Issues found

**No issues found in existing code.** All existing tests remained green. The executor and engine integrate cleanly with the existing types and interfaces.

### Weird observations

**`ConflictHandler.Resolve` uses relative paths for rename.** The conflict handler receives action paths as relative paths (e.g., `/documents/report.docx`), but the executor needs to prepend the sync root before calling filesystem operations. The conflict handler was designed to work with relative paths, so the executor prepends the sync root for its own filesystem calls but passes the original relative path to the conflict handler. The conflict handler's `fs.Rename` mock in tests works with relative paths. When wiring to a real filesystem, the executor should ensure the conflict handler's filesystem implementation handles absolute paths. This is a potential integration concern for Phase 4.

**`executorMockFS.Stat` returns `nil` `os.FileInfo` for existing files.** The mock returns `(nil, nil)` when a file exists, which is sufficient because the executor only checks for `os.ErrNotExist` in the stat error, not the `FileInfo` itself. However, if future code needs FileInfo details, the mock will need to be enhanced.

### Suggested improvements

- **Parallel download/upload execution.** The executor processes downloads and uploads sequentially. The transfer.Manager already has worker pools for concurrency. A future enhancement could dispatch all downloads concurrently and collect results, leveraging the existing pool infrastructure.
- **Retry logic.** The executor classifies errors as Skip or Fatal but does not retry. A Retryable tier exists in types.go but is unused. Adding exponential backoff for transient API errors (5xx, network timeouts) would improve reliability.
- **Progress callbacks.** The executor has no mechanism for reporting progress during execution. The CLI (Phase 4) will need progress updates for long sync operations.
- **ConflictHandler sync root awareness.** The conflict handler receives relative paths and renames them. When integrated with a real filesystem, the handler needs to know the sync root. Consider passing the sync root to the ConflictHandler or wrapping the filesystem to prepend it automatically.

### Cross-package concerns

- **Engine component interfaces.** The Engine accepts `DeltaFetcher`, `LocalScanner`, `PlanReconciler`, and `PlanExecutor` interfaces. The concrete `*DeltaProcessor`, `*Scanner`, `*Reconciler`, and `*Executor` all satisfy these interfaces naturally. No adapters needed.
- **`ExecutorConflictStore` adapter for `state.Store`.** The `state.Store.RecordConflict` returns `(string, error)` but `ExecutorConflictStore` expects just `error`. A thin adapter wrapping `state.Store` is needed:
  ```go
  type conflictStoreAdapter struct{ store *state.Store }
  func (a *conflictStoreAdapter) RecordConflict(rec *state.ConflictRecord) error {
      _, err := a.store.RecordConflict(rec)
      return err
  }
  ```
- **Upload ItemID update (FIXED).** After successful upload of a `local:xxx` item, the executor updates the item's ItemID to the server-assigned value. Since the state store key is `(driveID, itemID)`, changing the ItemID would create a new row with the server ID while orphaning the old `local:xxx` row. Fixed by deleting the old row before upserting with the new ItemID in `updateItemAfterUpload`.
- **Transfer Manager interface extraction confirmed.** The `TransferManager` interface defined in the executor (`Download`, `Upload`, `CheckHash`) is satisfied by `*transfer.Manager`. The LEARNINGS entry from increment 2.5 suggested this extraction; now it is implemented.
