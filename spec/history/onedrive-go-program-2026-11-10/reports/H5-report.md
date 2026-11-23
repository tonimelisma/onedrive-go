# Strict upload-session records and outer-package dead code

Branch `chore/outer-dead-code`, commit `f14b91ea` on `91113542`, not pushed. 75 files.

## What changed
- `driveops/session_store.go`: `UnmarshalJSON` shim and version history gone; `decodeSessionRecord` (unknown fields, trailing data, other versions refused); `removeInvalidRecord` removes and warns once (file + reason, never the URL); `DeleteForScope` removes an invalid record the same way and reads only `*.json` record files; `ErrCorruptSession` renamed `ErrInvalidSessionRecord`; `ExpiresAt` comment fixed.
- `driveops/transfer_session.go`: sentinel rename and its Debug line only. `driveops/hash.go`: `HasHash` deleted, `SelectHash` comment no longer narrates history.
- `graph`: `buildDeltaPath`/`buildFolderDeltaPath`, `HasWriteAccess` deleted; `permissionApplicabilityLegacy` renamed `permissionApplicabilityUnattributed` (a row with no link, no grantee identity, no owner role, counted as the caller's: the name says that case); `ChunkAlignment` unexported, `ChunkedUploadChunkSize` now 32 x `chunkAlignment`.
- `cli`: deleted `printSyncStateStoreLines`, `mountReportsError`, `buildConfiguredAuthRequirements`, `resumeSingleDrive`, `clearAccountAuthRequirement`, `printStatText`, `newRootCmd`, `buildLogger`/`buildHandler`/`buildLoggerDual` (docs moved to the `WithStatusWriter` forms), `statusMount` alias; `hasPersistedAccountAuthRequirement` moved into `auth_health_test.go` (now fails the test on a catalog read error instead of returning false).
- `config`: `decodeDriveSections*` wrappers deleted, `platformLinux` moved to `paths_test.go`, comments fixed (`cidFileSuffix` "legacy", `withMutationLock` "historical", and it wrongly said fn never runs unlocked).
- `driveid/itemkey.go` + test deleted (no production user); `devtool`: unregistered `sync-partial-local-catchup-100m` scenario, its fixture JSON and tests deleted, `LookupBenchSubject/Scenario`, `hitDeadline`, `readDir`, `fullE2EStandaloneTests` deleted, e2e tag discovery moved into `verify_runner_test.go`, `fullE2EPreflightPattern` built from `fullE2EFixturePreflight`; `errclass` `Class.Valid`, `loglevel.Name`, `retry.WithLogTarget`, `localpath.Symlink` deleted.
- Specs: `drive-transfers.md` (strict records, CleanStale sentence without history), `drive-identity.md` (GOVERNS, ItemKey section), `system.md`, `error-model.md`, `retry.md`, `cli.md` (renamed test), `retired-backlog-ids.md` (B-300 row gone, 9 stale file lists corrected).

## Tests added
- `TestSessionStore_SaveAndLoadRoundTripTheCurrentRecord` (R-5.2.3): replaces the five version/migration tests.
- `TestSessionStore_Load_RemovesARecordInAnyOtherFormat`: not JSON, unversioned `remote_path`, v1 `remote_path`, v1 today's shape, unknown field, version 99, trailing data: refused, removed, one warning with file and reason, no URL.
- `TestSessionStore_DeleteForScope_RemovesAnUnreadableRecord`, `..._LeavesASaveInProgressAlone`.
- `TestTransferManager_SessionUpload_RecordInAnOlderFormatStartsOver`: matching-hash v1 record is never resumed; fresh session.
- `TestResolveDeltaToken`, `TestEvaluateWriteAccess_WithoutAccountEmail`, `TestFinalizeBenchLiveSampleRuntimeReportsARootItCannotRemove` (R-6.10.14) replace deleted-helper tests; `TestClassString` (R-6.8.16) replaces `TestClassStringAndValidity`.
- No requirement covers strict loading; the refusal tests carry no `Validates:` line.

## Unreadable record
Before: Load deleted a non-JSON record, logged Info (no warning), returned the sentinel (Debug in caller), upload restarted; old shapes and unknown fields were accepted; `DeleteForScope` returned an undecodable record as an error every pass and left it. Now: any record not in Save's format is removed with one Warn (file, reason), upload restarts from zero; `DeleteForScope` likewise, not counted, not an error.

## Checks
build, vet, vet `-tags 'e2e e2e_full'` (e2e, graph, devtool), vet `-tags integration` graph, `GOOS=darwin` vet of touched packages: pass. `go test -race -count=1` whole packages: errclass, loglevel, localpath, retry, driveid, config, driveops, graph, devtool, cli: pass (cli failed once, see Not fixed, then passed); driveops and graph rerun after the lint fixes: pass. Lint: first run 4 issues (dupl in two upload tests, now `newFreshSessionUploader`; gosec G101 on a `token` field; testifylint require), fixed; second run `0 issues.` (H5-lint.log). Spec citations and GOVERNS checked by grep; backlog registry checked by script.

## Deviations
- (c) `CleanStale` no longer skips silently: #887 already returns an `Info()` failure joined with the count; engine housekeeping warns once per streak (R-6.6.30) and Info-logs the count, engine `Close` warns with the count. Per-entry warnings would break `TestCleanTransferArtifacts_ReturnsEachCleanupsFailureAndLogsNone` and R-6.6.30, so no change.
- `DeleteForScope` behaviour changed with (a) (removes invalid records, skips temp files).
- Test names `TestBuildLogger_*`, `TestBuildHandler_*`, `TestBuildLoggerDual_*`, `TestNewRootCmd_*`, `TestPrintStatText*` renamed after the functions they now call.

## Table (d)
aliases: none (grep empty) | buildDeltaPath x2: deleted | HasWriteAccess: deleted | permissionApplicabilityLegacy: renamed Unattributed | applyDriveOverrides: already gone | status_snapshot history: already gone | status_render_sections rows: already gone | printSyncStateStoreLines: deleted | cidFileSuffix comment: fixed | sync_dir backfill: still exists, now `config.plannedConfigText` (drive_enroll.go:300), left | itemkey.go: deleted (no production user) | bench_live.go:406/431: dead scenario deleted.

## (e) search: 129 hits, 58 removed or fixed, 71 kept
How: `deadcode` (golang.org/x/tools v0.42.0, main packages as roots, linux and GOOS=darwin, same list); a scratch go/packages scan (`-tags 'e2e e2e_full integration stress'`, tests loaded) for package-level declarations no non-test file references, bodies that do nothing, parameters assigned to `_`; `grep -rniE 'legacy|compat|kept for|deprecated|no longer|historical'` over non-test Go outside the excluded packages; `go vet` with tags plus the scan's tagged-reference column as proof.
- Functions/methods only tests reach (58). Removed (41): cli 10 (listed above), config 2, devtool 18 (11 dead-scenario funcs, 2 lookups, hitDeadline, readDir, fullE2EStandaloneTests, discoverTaggedE2ETests and hasBuildExpression moved to test), driveid 3, driveops HasHash, errclass Valid, graph 3, localpath Symlink, loglevel Name, retry WithLogTarget. Kept (17): reached by tagged tests (stop rule) `config.Load`, `loadWithIO`, `UpdateCatalog`, `DefaultCacheDir`, `graph.IsProviderUnavailable`, `MostSpecificErrorCode`, `IsProviderReadOnlyCode`, `syncverify` x4; documented seams `AppendDriveSection`, `MustCanonicalID`, `MustNewClient`, `sharedref.MustParse`, `WithContentHasher`; out of scope `printStatJSON`.
- Consts/types only tests reference (3): `statusMount` alias deleted, `platformLinux` moved to test, `ChunkAlignment` unexported and used by the chunk size.
- Bodies doing nothing (3) and `_` params (1): `printSyncStateStoreLines` deleted; kept `synctree/birthtime_linux.go:16` (platform stub) and `synctree/detach_unix.go:194` (closes descriptors; scan false positive).
- Words (64 lines): fixed or deleted 12 (discovery.go:16, mutation_lock.go:74, delta.go:120/128, items_permissions.go:74/102/123/152, bench_live.go:406/431, itemkey.go:19, sync_render.go:21). Kept 52: `verify_e2e.go:96` e2e test name (Not fixed), `verify_backlog_ids.go:16/76` (registry is historical by design), `namekey.go:51/222` (Unicode "compatibility variants"), `perf.go:82`, `drive_cleanup.go:27`, `localtrash.go:596`, `sync_console.go:400` and 41 "no longer"/"Incompatible" lines that describe behaviour, not code age (email_reconcile, toml_lines, download, upload_session, redaction, errors, auth_token, nightly, verify_dod_ci_wait, verify_governs, detach_unix, sync_render 73/99, status_condition_descriptors, replace_destination, auth_logout, drive_catalog, failure_class, purge, auth_health, authstate, sync_startup_messages x10, drive_reset_sync_state, drive_cleanup:64, transfer_verify).

## Packages the reviewer should test
`internal/sync` (CleanStale/session store users, `WithContentHasher`), `internal/multisync` (`DeleteForScope` via shortcut cleanup).

## Not fixed
- `internal/cli/status_live_test.go:306` `TestStatus_StorageCutShortByTheBudgetIsNoted` failed once in a full `-race` package run (live name not fetched inside `statusLiveTestShortBudget`), 5/5 alone and green on rerun: timing-bound; fix with an injected clock or a gated `/me` answer.
- `e2e/cli_commands_e2e_test.go:202` `TestE2E_Status_NoLegacyHistorySurface` names a removed surface "legacy"; rename and update `internal/devtool/verify_e2e.go`, `spec/design/cli.md:40`.
- `internal/devtool/verify_backlog_ids.go:53` checks ids only, so registry file lists drift (9 rows were stale); check the listed files too.
- Kept test-only exports (see (e) list in the commit report): `config.Load`/`loadWithIO` (e2e), `config.UpdateCatalog` (e2e helper), `config.DefaultCacheDir` (integration), `graph.IsProviderUnavailable` (integration), `graph.MostSpecificErrorCode`, `graph.IsProviderReadOnlyCode` (e2e), `syncverify` package (e2e only), `cli.printStatJSON` (out of scope), seams `config.AppendDriveSection`, `driveid.MustCanonicalID`, `graph.MustNewClient`, `sharedref.MustParse`, `driveops.WithContentHasher`.
- Out of scope, deadcode in `internal/sync`/`internal/multisync` (base lines): multisync `mount_spec.go:178` buildRuntimeWork, `:741` expectedChildRootIdentity; sync `active_scopes.go:117,139,156,174`, `core_types.go:331` NewBaselineForTest, `engine_run_once.go:521`, `engine_runtime_lifecycle.go:530`, `engine_watch_state.go:519`, `executor_conflict.go:186`, `observed_items.go:36`, `observer_local.go:415`, `scanner.go:1326`, `shortcut_root_publication.go:188`, `sqlite_compare_kinds.go:90,192`, `store.go:208`, `store_inspect.go:175`, `inotify_other.go:18` (darwin).


## Review (first pass, 2026-11-13 17:15, on `f14b91ea`)

Verdict: handed back. The deletions are sound (no deleted symbol has a use left under any build tag, no behaviour changed without a test) and the strict session-record rule is implemented as asked. What follows must be done before this is accepted. The reviewer's full evidence is in `/tmp/scratch/scratchpad/R-H5-findings.md` (read sections by line range; section numbers below refer to it). Do not rebase: stay on `91113542`; I restack the branch.

### Must fix

1. **An invalid record that cannot be removed is reported as success** (`internal/driveops/session_store.go` `removeInvalidRecord`, called from `DeleteForScope`). I read it: the removal error is logged and dropped, so shortcut cleanup reports success while the file, which may hold a pre-authenticated URL, stays, and every later pass warns again (against `DeleteForScope`'s own contract for a matching record it cannot delete, and R-6.6.30). `removeInvalidRecord` returns the removal error; `DeleteForScope` joins it into `errs` and the store does not also warn about that failure there (the cleanup's caller reports it once); `Load` keeps its one warning. A test covers the removal failure in both callers (the read-only-directory pattern of `cleanup_test.go` is there).
2. **R-5.6.13 and the new drop path** (`spec/requirements/transfers.md` line 42). The requirement says every dropped record's server session is cancelled; a file that holds no readable record is removed without a cancel. Decision (mine, reported to the owner): the client reads nothing from a file that is not a record, its URL included, so there is no cancel. Amend R-5.6.13 to say so in one clause (a record the client can read is cancelled when dropped; a file that holds no readable record names no session the client can trust, and Graph expires it), say the same in `spec/design/drive-transfers.md` where the removal is described, and list the amendment in the report under "Deviations". Add the sibling sentence under R-5.2.3 that the strict format rule needs (a session file that is not a record in the format the client writes is removed with one warning and its upload starts over), so the new tests have a requirement to cite.

### Should fix (all of it, in this commit)

3. Tests (section 2): add the truncated record and the empty file as cases of `TestSessionStore_Load_RemovesARecordInAnyOtherFormat`; the `DeleteForScope` invalid-record test also asserts the reason and that the URL is absent; every new test gets its `// Validates:` line (R-5.2.3 as amended, R-5.2 and R-6.6.30 where the neighbours cite them).
4. The warning in `Load` names the local path whose upload starts over (it is known there and is no secret); the cleanup's message does not claim an upload starts over, which it cannot know (section 1, S-1.2).
5. The temporary-file pattern is a named constant beside `sessionRecordSuffix`, and the test builds its in-progress name from it (S-1.1).
6. `permissionApplicabilityUnattributed`: the comment says what the code tests (a row whose grantees carry no e-mail address the client can compare: no sharing link, no granted address, no owner role), and `EvaluateWriteAccess`'s doc comment stops saying that grants for another principal are ignored where a group, an application or an address-less user is counted as the caller's. No behaviour change: counting such a row is the right default, because the client cannot know group membership and a wrong guess ends in a 403 that is handled. The commit message's "names no grantee" gets the same correction.
7. Devtool coverage lost with deleted tests (section 5, S-5.1): the bidirectional runtime test asserts the credential copy, and one test covers the missing `ONEDRIVE_TEST_DRIVE` prerequisite turning into a fixture failure (R-6.10.14).
8. Names that refer to removed things, in files this commit touches: `TestE2E_Status_NoLegacyHistorySurface` (e2e file, `internal/devtool/verify_e2e.go`, `spec/design/cli.md`) and `TestPrintStatusJSON_SyncStateOmitsLegacyHistoryKeys` (`internal/cli/status_test.go`). For each: if another test already pins the exact key set of the document, the test only pins that a removed surface stays removed and is deleted with its citations; otherwise it is renamed to what it holds today. Also `TestLookupBenchRegistriesAreSortedAndIncludeBuiltins` and `TestLookupBenchSubjectAndScenarioRejectUnknownIDs` (named after the deleted API; `spec/design/performance-benchmarking.md` cites the first), and the "partial-local catch-up" headline metric in that document, which no scenario measures now: remove it or say it is not measured.

### Smells outside the diff: fixed here too

9. `config.DefaultCacheDir`: no product code calls it, and `docs/configuration.md` and `docs/scripting.md` promise a cache location nothing uses. Decision: delete the function and its tests, correct both documents (the client has no cache directory), and adapt the integration isolation check that asserts it. Check first with your scan that nothing under any build tag depends on the directory existing.
10. `config.Load` with `loadWithIO`: a strict loader no product path calls. Delete it if the product's loader covers the same rules: tests that pin a rule the product loader shares move onto the product loader, tests that only pin the dead loader go, the one e2e use reads through the product loader. If this turns out to change more than about 300 lines or to remove a rule no product path enforces, stop and report the list instead; I then make it a plan row.
11. `graph.IsProviderUnavailable` is reachable only under the integration tag: move it beside its only caller or say so in its doc comment.
12. `parseBenchSyncCountLine` in `internal/devtool/bench_live_bidirectional.go` reads a count-line dry-run format. Check what `sync --dry-run` prints today (read the product code that prints it); if nothing prints count lines, delete the parser branch and rename its test.
13. `e2e/sync_kill_mid_transfer_full_e2e_test.go` globs `upload-sessions/*`, so a leftover temporary file alone would count as a persisted session: glob the record suffix.
14. The doc comment of `classifyPostMergeRuns` (`internal/devtool/verify_dod_ci_wait.go`) narrates history ("not the old squash-merge skip rule"): say what the function does.

### Checks

Run and report: `go build ./...`, `go vet ./...`, `go vet -tags "e2e e2e_full" ./e2e/...`, `go vet -tags integration ./...`, `GOOS=darwin go vet ./...`, `go test -count=1 ./internal/driveops/... ./internal/config/... ./internal/devtool/... ./internal/graph/... ./internal/cli/...` (give `-timeout 40m` if another heavy run is on the host), and lint once at the end when `pgrep -fa "stage pre-p[r]"` prints nothing. One commit, amended; attribution line `Co-Authored-By: Claude <noreply@anthropic.com>` and no other. Append "## Second pass" to this report: what changed per number above, each check with its result, and anything seen and not fixed with the reason.

## Second pass

Commit `463a9b1b` (amended, still one commit on `91113542`, not pushed). 96 files.

1. `removeInvalidRecord` is now a plain function that removes and returns the failure. `Load` reports once through `warnInvalidRecordOfUpload`, with the removal error when there is one. `DeleteForScope`'s per-file work moved into `deleteRecordForScope`: a removal that fails is joined into its errors and not warned; a removal that works is warned once (file, reason). New tests: `TestSessionStore_Load_ReportsAnInvalidRecordItCannotRemove`, `TestSessionStore_DeleteForScope_ReturnsAnInvalidRecordItCannotRemove` (read-only directory).
2. R-5.2.3 has the strict-format sentence. R-5.6.13 limits the cancel to a record the client can read, and says that a file holding no readable record has nothing read from it, URL included, so Graph expires the session. `drive-transfers.md` says the same. Deviation (reviewer's decision): an invalid record's server session is not cancelled. The transfer test now asserts the recorded URL is never cancelled.
3. The load table has cases for a cut-short record (`unexpected EOF`) and an empty file (`EOF`). The scope test now checks the reason, that the URL is absent, and that the message does not say "starts over". `Validates:` lines added: R-5.2.3 and R-6.6.30 on the load tests; R-5.2 and R-6.6.30 on the scope tests (the temp-file test cites R-5.2 only); R-5.2.3, R-5.6.13 and R-6.6.30 on the transfer test.
4. The `Load` warning carries `local_path` (the key Load was given; an overwrite's key puts the item in front). The cleanup message drops "its upload starts over".
5. `sessionTempPattern` sits beside `sessionRecordSuffix`; `Save` and the test use it.
6. The comment and the `EvaluateWriteAccess` doc now say a row whose grantees carry no comparable address (group, application, user without an address) counts as the caller's. The commit message says the same; no behaviour change.
7. The bidirectional runtime test asserts the copied sign-in file. `TestBidirectionalCatchupScenarioMissingPrerequisitesReturnsFixtureFailure` (R-6.10.14) covers the missing drive variable.
8. `TestE2E_Status_NoLegacyHistorySurface` is deleted, with its manifest entry and the `cli.md` citation. `TestE2E_Status_PerDrive_NoIssuesOrRetries` already holds the clean-status part, and the only other thing it checked was that the removed flag stayed removed. `live-incidents.md` gets a row for it, and row 33 no longer names it. `TestPrintStatusJSON_SyncStateOmitsLegacyHistoryKeys` is deleted, because `TestStatusOutputGoldenJSON` pins the exact `sync_state` keys. The bench tests are renamed `TestBenchRegistriesAreSortedAndIncludeBuiltins` (citation updated) and `TestBenchDefinitionLookupsRejectUnknownIDs`. The partial-local headline metric is gone.
9. `DefaultCacheDir` and its four tests are deleted, and the integration isolation check no longer lists it. `docs/configuration.md` and `docs/scripting.md` now say there is no cache directory. Nothing under any tag creates or reads that directory.
10. `config.Load` and `loadWithIO` are deleted. 69 config test calls, 3 cli test calls and the one e2e call read through `LoadOrDefault`, which runs the same strict pipeline (`configLoader.load`), so no rule is lost. Two cli tests now assert the file still exists before checking for empty drives. `TestLoad_FileNotFound` went: it pinned only the dead loader's error on a missing file. `TestLoadOrDefault_ReadManagedFileError` replaces the injected-IO test. `config.md` is updated. Size: +92/-163 across 14 files.
11. The doc comment says only the integration tests call it; product code branches on `ErrProviderReadOnly`.
12. The dry run prints no count lines today (`sync_console.go` prints "Would ..." lines and the summary). So the count-line parser and `BaselineUpdates` are deleted, the test is renamed `TestParseBenchSyncPlanOutput_CountsOneWouldLinePerAction`, and three test fixtures that fed the old format now print the current one.
13. The e2e glob now matches `*.json` only (`uploadSessionRecordGlob`).
14. `classifyPostMergeRuns` now says what it returns.

Checks: `go build ./...` pass. `go vet ./...` pass. `go vet -tags "e2e e2e_full" ./e2e/...` pass. `go vet -tags integration ./...` pass. `GOOS=darwin go vet ./...` pass. `go test -count=1 -timeout 40m` on driveops, config, graph and cli: pass. devtool failed first: two bench tests fed the old count format and timed out at 180 s. After the fixtures were fixed it passes. Lint on the ten touched packages plus e2e: one misspelling ("cancelling") fixed, then `0 issues.`

Seen and not fixed:
- `config.CheckDevSafe` (`internal/config/paths.go`) still requires `XDG_CACHE_HOME` for a dev build although the client keeps no cache. Dropping it changes the refusal message, `README.md:88` and `config.md:69`, so it is the owner's call. Its comment now says so.
- `internal/config` tests are still named `TestLoad_*` while calling `LoadOrDefault`. They name the act of loading, and renaming about 30 tests is churn.


## Review (second pass, 2026-11-13 18:22, on `c787e72b`)

The second pass (`463a9b1b` on the old base) was rebased by the lead onto main `8d6f7d46` as `c787e72b`; four files conflicted and were resolved by keeping main's code and applying this commit's deletions. An independent read-only reviewer read the commit against 10 questions (brief and findings in the session scratchpad: `R-H5-2.md`, `R-H5-2-findings.md`); the lead checked M1 at `load_test.go:827` and M2 in `paths.go` against the code. All 14 first-pass items are done; the rebase lost nothing (`git range-diff` shows context changes and the two hunks main had already made). Sent back for a short third pass:

1. **Must fix.** Six converted config tests now pass if the code under test deletes the config file, because `LoadOrDefault` answers a missing file with defaults that also satisfy the assertions: `load_test.go:827`, `write_test.go:612, 670, 1315, 1367, 1402`. One test helper that requires the file and then loads it, used at every reload of a file the code under test wrote.
2. **Must fix.** `CheckDevSafe` still requires `XDG_CACHE_HOME`, which nothing reads once `DefaultCacheDir` is gone. The requirement, its message, its test, README, CONTRIBUTING and `spec/design/config.md` (lines 69 to 71 and 79) go; so does every place that sets the variable only to satisfy the check.
3. The commit message names `mountReportsError`, which main had already removed.
4. The 23 tests named `TestLoad_*` become `TestLoadOrDefault_*` (the writer's "about 30" is 23), with the citation in `spec/design/config.md`.
5. Two comments say an invalid session record "has been removed", which is false when the removal failed (`session_store.go:21`, `transfer_session.go:113`).
6. `transfer_manager_test.go:3143` asserts the path anywhere in the log, which the caller's Debug line satisfies; it asserts `local_path=` plus the path.
7. `spec/design/performance-benchmarking.md:154` gives a "partially-local catch-up" claim as its example; the scenario is gone.

Left to the lead: the "removed in" cell of the new `live-incidents.md` row takes the PR number when the PR is opened. Noted and not changed: a record `Load` cannot remove warns on every upload attempt for that path (the documented per-attempt warning for an unreadable record; a read-only session directory makes `Save` fail too). Not checked by the reviewer: build, vet and tests (the host was at load 14), and whether multisync's handling of the returned cleanup error matches R-6.6.30 (that code is older than this commit).

## Third pass

Commit `7c4ee352` (amended; still one commit, on main `8d6f7d46`; not pushed).

1. A new test helper, `loadWrittenConfig` (`internal/config/load_test.go`), requires the file to exist, loads it with `LoadOrDefault` and requires no error. I went through every `LoadOrDefault(` call in `internal/config/*_test.go` and use the helper where the code under test wrote or rewrote the file:
   - all 32 loads in `write_test.go`;
   - the `ClearExpiredPauses` reload (`load_test.go`), which now also requires the drive section to stay;
   - the three loads in `drive_enroll_test.go`;
   - the two `email_reconcile_test.go` tests.
   `assertEmailReconcileFixtureRenamed` keeps its logger argument, because four callers in `email_change_journal_test.go` pass one, so it requires the file inline. The other calls load a file the test itself wrote, with `LoadOrDefault` as the code under test, and stay as they are.
   In the cli tests, `require.FileExists` now comes before every converted load. The exception is `TestAddOwnDrive_SyncDirCreateFailureLeavesConfigAndCatalogUnchanged`: there the add must not create the config, so it now asserts `NoFileExists` (the run caught the wrong guard).
2. `CheckDevSafe` asks only for `XDG_DATA_HOME` and `XDG_CONFIG_HOME`; the message and the comment changed with it. In `paths_test.go`, the passing test now runs with `XDG_CACHE_HOME` empty to show it is not needed. `README.md`, `CONTRIBUTING.md` and `config.md` (dev-build paragraph and "config and data roots") are updated, and so is `performance-benchmarking.md` (the startup scenario's roots).
   The setting is removed from:
   - `scripts/dev-env.sh` (variable and mkdir);
   - `scripts/bootstrap-test-credentials.sh` (variable and its `rm` path);
   - `bench.go` and `bench_live_bidirectional.go` (variable and directory);
   - `e2e/testenv_test.go` (directory, setting, isolation-check entry and `TestIsolation_XDGCacheDir`);
   - `integration_testenv_test.go` (setting and check entry);
   - the cli tests (`setTestDriveHome`, two `t.Setenv` calls, the signal helper, and the side-effect roots list, which would otherwise have walked whatever `XDG_CACHE_HOME` the shell holds).
   **No setting is kept** except the deliberate empty one in `paths_test.go`. The only other reader is the Go toolchain's build cache (`os.UserCacheDir`), in two places:
   - `bootstrap-test-credentials.sh` also redirects `HOME`, so `go run`'s cache stays inside `.testdata/home` and is deleted with it.
   - After `dev-env.sh`, `go run` returns to the developer's own build cache.
   - e2e sets `GOCACHE` from the real cache it captured before redirecting `HOME`.
3. The commit message no longer names `mountReportsError`. It now names the file guard and the dropped `XDG_CACHE_HOME` requirement, and still ends with `Co-Authored-By: Claude <noreply@anthropic.com>`.
4. The 23 `TestLoad_*` tests are now `TestLoadOrDefault_*` (no collisions), with the `config.md` citation. Also renamed: `TestDecodeDriveSections_StrictAndLenient_IdenticalOutput` → `TestDriveSectionDecoder_StrictAndLenient_IdenticalOutput` (named after a deleted function, cited nowhere).
5. The `ErrInvalidSessionRecord` doc and the `transfer_session.go` comment now say Load tried to remove the file and warned once, with the failure if it failed.
6. The transfer test asserts `local_path=` plus the path.
7. The example claim is now catching up on a 100 MiB tree changed on both sides (the bidirectional scenario).

Checks:
- `go build ./...`: pass.
- `go vet ./...`: first failed on a shadowed `err` the helper conversion left in `write_test.go` (fixed); passes now.
- `go vet -tags "e2e e2e_full" ./e2e/... ./internal/devtool/...`: pass.
- `go vet -tags integration ./internal/graph/...`: pass.
- `go test -count=1 ./internal/config/ ./internal/driveops/ ./internal/devtool/ ./internal/cli/`: config, driveops and devtool pass. cli failed on the wrong guard in item 1; after the fix, cli and config were rerun and pass.
- Lint (under `heavy.lock`) on cli, config, devtool, driveops, graph and e2e: one `unparam` (the reconcile helper's logger, fixed by keeping it used), then `0 issues.`

Seen and not fixed: none.


## Review (third pass, lead, 2026-11-13 19:15 Pacific)

Accepted, no findings. Read: `CheckDevSafe` and every place that set `XDG_CACHE_HOME` (only `internal/config/paths_test.go` names it now), the `loadWrittenConfig` helper and each converted load, the cli `drive_test.go` assertions, the two driveops comments and the `local_path` assertion, the `TestLoadOrDefault_*` renames with their citation in `spec/design/config.md`, the user docs, and the whole commit message. The lead filled the "removed in" cell of incident row 34 with the pull request number (#899); the commit is `f498e84b` on `8d6f7d46`, and its pre-pr gate started at 19:18.
