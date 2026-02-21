# Archived Learnings — Phase 4 Per-Increment Details

Per-increment bullet-point summaries (Pivots/Issues/Linter/Suggested/Cross-package/Code smells) that duplicate what the narrative sections already cover. Also includes section 6 (just a pointer to tier1-research/) and section 13 (all "None" entries).

The active `LEARNINGS.md` preserves all substantive content in a topic-organized format.

---

## Section 6 (Tier 1 Research pointer)

16 research documents in `docs/tier1-research/` covering Graph API bugs, reference implementation analysis, and tool surveys. Consult these before implementing any API interaction — they contain critical gotchas (upload session resume, delta headers, hash fallbacks, etc.) tracked as B-015 through B-023 in BACKLOG.md.

## Section 13 (Pre-Phase 4 Docs — B-027, B-029)

- **Pivots**: None (doc-only increment)
- **Issues found**: None
- **Linter surprises**: N/A (no code changes)
- **Suggested improvements**: None
- **Cross-package concerns**: None. The conflict resolution UX design (interactive + batch) is consistent with existing CLI patterns (--json for machine output, --dry-run for previewing). The resolution actions table in sync-algorithm.md §7.4 maps cleanly to the executor (4.7) and conflict handler (4.8) interfaces that will be implemented in Phase 4.
- **Code smells noticed**: None

## Section 17 (Graph Hardening — bullet summary)

- **Pivots**: Extracted `terminalError` helper (not in plan) to satisfy `funlen` lint after `rewindBody` extraction pushed `doRetry` over the limit. Changed test from `r.URL.RawPath` to `r.RequestURI` for URL encoding verification.
- **Issues found**: `driveItemResponse.MimeType` was a dead field with a non-functional JSON tag (dot notation). Removed as planned.
- **Linter surprises**: Adding 4 lines to `doRetry` (already at 100 lines) triggered both `gocyclo` and `funlen`. Required extracting two helpers instead of one.
- **Suggested improvements**: None specific to graph package.
- **Cross-package concerns**: URL encoding fix affects CLI callers that construct paths for `GetItemByPath`/`ListChildrenByPath` — they should not pre-encode paths now (double-encoding). Currently CLI passes raw user input which is correct.
- **Code smells noticed**: The `doRetry` function was already at the lint limit before this change — any future additions will require further decomposition. The `rewindBody` seek-error branch is practically untestable (no easy way to make `bytes.NewReader.Seek` fail).

## Section 18 (CLI Hardening — bullet summary)

- **Pivots**: None. All seven fixes (C1-C7) were implemented as planned. C8 was correctly skipped per plan.
- **Issues found**: The `//nolint:mnd` directive on `< 2` comparisons was unnecessary because `2` is in the ignored-numbers list. Caught by `nolintlint`.
- **Linter surprises**: `nolintlint` catching unnecessary `//nolint:mnd` — reminder to check the ignored-numbers list before adding nolint directives.
- **Suggested improvements**: None outside scope.
- **Cross-package concerns**: C8 (DefaultSyncDir signature change) depends on Agent B. The orchestrator will handle the call site update in top-up work after Agent B merges.
- **Code smells noticed**: (1) `drive.go` previously imported `errors` and `os` only for the duplicated purge logic — removing the duplication also cleaned up imports. (2) The `findTokenFallback` function probes the filesystem, which makes it harder to test in isolation without creating actual files. An interface-based approach would be cleaner but over-engineered for this use case.

## Section 19 (Config Hardening — bullet summary)

- **Pivots**: None. All six fixes implemented exactly as planned.
- **Issues found**: `DriveStatePath` accepted empty strings and strings without colons, producing invalid state file paths like `state_.db`. This could cause issues if a caller passed an invalid canonical ID. Now matches `DriveTokenPath` validation.
- **Linter surprises**: None. All fixes passed lint on first try.
- **Suggested improvements**: The `atomicWriteFile` fsync error path and close error path are not covered by tests (64% function coverage). OS-level error injection would be needed to test these. Consider an interface-based approach for the file handle if this becomes a concern.
- **Cross-package concerns**: The `DefaultSyncDir` signature change (removing unused email parameter) requires Agent C to update the call site in `auth.go:277`. The build will fail at the repo level until Agent C makes the corresponding change.
- **Code smells noticed**: (1) `parseSizeNumber` uses `int64(n * float64(multiplier))` which loses precision for very large sizes near the int64 boundary — acceptable for practical file sizes but technically imprecise. (2) The `knownGlobalKeys`/`knownDriveKeys` maps and their list counterparts are package-level mutable state (slices), though they are initialized once and never modified after init.

## Section 22 (Scanner — bullet summary)

- **Pivots**: NFC normalization approach changed from single-path to dual-path (fsRelPath/dbRelPath) after Linux CI failure. Original approach used NFC path for both I/O and DB, which failed on Linux ext4 where NFD bytes are stored as-is.
- **Issues found**: staticcheck caught an unused `segments` slice append in a test — fixed by removing the variable.
- **Linter surprises**: The `nilnil` return pattern for "skip this entry" in `resolveSymlink` required a `//nolint:nilnil` directive. This is the idiomatic pattern when a function returns `(value, error)` and both nil means "nothing to do, no error."
- **Suggested improvements**: (1) The scanner currently runs single-threaded; section 4.4 describes a checker pool for parallel hash computation that would be a future enhancement. (2) Directory tracking (section 4.5) is not yet implemented — directories are walked but not stored as items.
- **Cross-package concerns**: None. The scanner only depends on Store and Filter interfaces, which other agents are implementing concurrently.
- **Code smells noticed**: (1) The `validateEntry` function does both filtering and validation in one pass, which means the filter is consulted even for entries that would fail name validation. The order is intentional (filter first for performance) but could be surprising. (2) The `oneDriveReservedNames` map is package-level mutable state (though initialized once and never modified).

## Section 23 (Filter — bullet summary)

- **Pivots**: `config.parseSize` is unexported so had to duplicate size parsing logic locally rather than modifying config package (owned by Agent E). This duplication should be resolved post-merge by exporting `ParseSize`.
- **Issues found**: (1) "." path component was rejected by OneDrive name validation (trailing dot check). Fixed by skipping "." and ".." in component validation. (2) Path length tests used single-component paths that hit the 255-byte name limit before the 400-char path limit. Fixed by using multi-component paths. (3) `FilterConfig` is 112 bytes — gocritic `hugeParam` required passing by pointer.
- **Linter surprises**: `gocritic:emptyStringTest` prefers `name != ""` over `len(name) > 0`. `gocritic:hugeParam` triggers at 112 bytes for `FilterConfig` struct, requiring pointer parameter.
- **Suggested improvements**: Export `config.parseSize` to eliminate duplication between config and sync packages. Consider a shared `nameutil` package if name validation logic is needed by multiple consumers.
- **Cross-package concerns**: The `NewFilterEngine` constructor accepts `*config.FilterConfig` — callers must pass a pointer. The `types.go` `NewFilterConfig` helper already returns `config.FilterConfig` by value, so callers will need `&sync.NewFilterConfig(resolved)` or the helper should return a pointer.
- **Code smells noticed**: (1) Size parsing duplication between `config/size.go` and `sync/filter.go` — same algorithm, different constant names to avoid conflicts. (2) The `matchesSkipPattern` function uses package-level `slog.Warn` instead of the engine's logger, because it's a standalone function. Consider making it a method.

## Section 25 (Safety — bullet summary)

- **Pivots**: Extracted `filterBySyncedHash` to deduplicate S1/S4 (not in plan). Extracted `handleBigDeleteViolation` and `handleDiskSpaceViolation` to keep `checkS5BigDelete` and `checkS6DiskSpace` under funlen limit. Changed `SafetyConfig` from value to pointer parameter after gocritic hugeParam.
- **Issues found**: None beyond lint issues.
- **Linter surprises**: `dupl` catching S1/S4 pattern overlap, `gocritic:hugeParam` on SafetyConfig (88 bytes), `gosec:G115` on uint64->int64 conversion.
- **Suggested improvements**: None specific to safety checker.
- **Cross-package concerns**: `NewSafetyConfig` in `types.go` changed to return pointer. Callers (future reconciler/engine) must accept `*config.SafetyConfig`.
- **Code smells noticed**: None significant. The seven safety checks are well-separated into individual methods.

## Section 26 (Reconciler — bullet summary)

- **Pivots**: Changed `classifyRemoteTombstone` from returning `[]Action` to `([]Action, bool)` after test failure (F9 skip with incomplete delta was falling through to F5 conflict). Refactored folder reconciliation three times to satisfy gocyclo (enum approach, then struct approach, finally settling on `folderState` + `dispatchFolder`).
- **Issues found**: None pre-existing.
- **Linter surprises**: (1) `gocyclo` at limit 15 is strict for decision matrices — the 14-row file matrix naturally has many branches. Decomposition into `classifyLocalDeletion`, `classifyRemoteTombstone`, `classifyStandardChange`, `classifyBothChanged` was necessary. (2) `exhaustive` linter catches missing enum cases even for internal-only types. (3) `staticcheck S1011` prefers `append(slice, other...)` over range-loop append.
- **Suggested improvements**: The `NewPath` field on `FolderCreate` actions encoding "local" vs "remote" as a string is a code smell — consider a dedicated `FolderCreateSide` enum.
- **Cross-package concerns**: None. The reconciler only depends on Store and types from the same package.
- **Code smells noticed**: (1) The `folderCreateAction` using `NewPath` as a "local"/"remote" discriminator is stringly-typed. (2) `dispatchFolder` default branch is unreachable but required by exhaustive checking patterns.
