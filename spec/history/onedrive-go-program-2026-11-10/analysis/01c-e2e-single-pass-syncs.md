# One-shot `sync` call sites in ~/src/onedrive-go/e2e — single-pass audit

Read-only work; nothing was modified, built, tested or run. All line numbers below were read in the files themselves.

## Result

There are **104 call sites** that run a one-shot `sync` exactly once, require exit 0, and are not behind a converge helper.

| Verdict | Count |
|---|---|
| CONVERGE (only the end state matters) | 66 |
| SINGLE-PASS (asserts on that one pass) | 37 |
| UNCLEAR | 1 |

- **Idempotence wrappers:** 17 of the 37 SINGLE-PASS sites are callers of `assertSyncLeavesLocalTreeStable` / `...ForDrive`. They only need exit 0 plus an unchanged local tree.
- **Fast tier:** only two of the 104 sites are in the `//go:build e2e` tier: `sync_e2e_test.go:642` and `sync_shortcut_e2e_test.go:23`. Everything else is `e2e && e2e_full`.
- **Strongest flake candidates:** these CONVERGE sites assert, right after the single pass, on state that is not polled or that is reached through delta:
  - `sync_full_test.go` 294, 906, 975
  - `sync_hardening_full_e2e_test.go` 328
  - `orchestrator_e2e_test.go` 66, 120
  - `cli_commands_e2e_test.go` 191, 758
  - `sync_recovery_e2e_test.go` 138

## Helper behaviour that shapes the verdicts

Abbreviations: **RSTF** = `runSyncThroughTransientFailures` (`e2e_test.go:571`), **RSEC** = `requireSyncEventuallyConverges` (`e2e_test.go:491`).

- **RSTF does nothing for delta lag.** It returns the first pass as it is when that pass logged no `msg="sync retry work"` (`e2e_test.go:590`). Otherwise it re-runs while stdout matches `\d+ failed|will be retried on the next sync` (`e2e_test.go:555`, 594). It returns stderr only, and every pass must still exit 0 (`runCLIWithConfig` at 587). Where the risk is a remote change not yet in delta, RSEC with a predicate is the fit.
- **RSTF must not wrap a pass that is meant to end in a permanent per-item failure.** `internal/sync/engine_result_classify.go:113-125` gives Forbidden, Unsupported, PathTooLong, Malware and Other server answers `Persistence: persistRetryWork`. `engine_results.go:85` and 166-174 log `sync retry work` for those. If such a pass prints "N failed", the loop runs to its timeout. I did not verify that the blocked shared-folder writes actually reach a 403 action result, but this is why `shared_items_e2e_test.go:202` and `sync_shortcut_full_e2e_test.go:208` are SINGLE-PASS.
- **Scan-time findings are safe under RSTF.** `invalid_filename`, `path_too_long` and `file_too_large` are caught at observation time (`internal/sync/issue_types.go:5-8`) and are not action completions. `edge_cases_e2e_test.go:110` already runs RSTF with an invalid name present.
- **Both helpers are default-drive only.** RSEC calls `runCLIWithConfigAllowError` (509) and RSTF calls `runCLIWithConfig` (587). Sites using `runCLIWithConfigForDrive` (`cli_runner_test.go:183`) or `runCLIWithConfigAllDrives` (`cli_runner_test.go:154`) have no drop-in helper. `syncShortcutDownloadUntilProjected` (`sync_shortcut_full_e2e_test.go:306-346`) is in an `e2e_full` file, so the fast-tier `sync_shortcut_e2e_test.go` cannot see it.

## Per-file counts

| File | Tag | CONVERGE | SINGLE-PASS | UNCLEAR | Sites |
|---|---|---|---|---|---|
| `sync_e2e_test.go` | e2e | 0 | 1 | 0 | 1 |
| `sync_full_test.go` | full | 19 | 8 | 0 | 27 |
| `sync_demoted_full_e2e_test.go` | full | 4 | 3 | 0 | 7 |
| `sync_edge_cases_full_test.go` | full | 5 | 2 | 0 | 7 |
| `sync_recovery_e2e_test.go` | full | 6 | 4 | 0 | 10 |
| `sync_kill_mid_transfer_full_e2e_test.go` | full | 0 | 3 | 0 | 3 |
| `sync_safety_full_e2e_test.go` | full | 2 | 1 | 0 | 3 |
| `sync_hardening_full_e2e_test.go` | full | 6 | 8 | 0 | 14 |
| `sync_shortcut_full_e2e_test.go` | full | 9 | 2 | 0 | 11 |
| `sync_shortcut_e2e_test.go` | e2e | 1 | 0 | 0 | 1 |
| `sync_shared_e2e_test.go` | full | 0 | 1 | 0 | 1 |
| `cli_commands_e2e_test.go` | full | 8 | 0 | 0 | 8 |
| `drive_list_full_test.go` | full | 1 | 0 | 0 | 1 |
| `output_validation_e2e_test.go` | full | 1 | 2 | 0 | 3 |
| `orchestrator_e2e_test.go` | full | 3 | 0 | 0 | 3 |
| `shared_items_e2e_test.go` | full | 1 | 1 | 1 | 3 |
| `sync_watch_full_test.go` | full | 0 | 1 | 0 | 1 |
| **Total** | | **66** | **37** | **1** | **104** |

## Per-file tables

All files are under `~/src/onedrive-go/e2e/`. Unless stated, the runner is `runCLIWithConfig(t, cfgPath, env, <args>)`. "Stable helper" means `assertSyncLeavesLocalTreeStable`, whose assertion is `assert.Equal(t, before, after, "sync should not mutate the test-owned local tree")` at `sync_e2e_test.go:349` (its run is at 347).

### ~/src/onedrive-go/e2e/sync_e2e_test.go — 0 / 1 / 0

| Line | Test | Args | Verdict | Evidence |
|---|---|---|---|---|
| 642 | TestE2E_Sync_UploadOnly | `"sync","--upload-only"` | SINGLE-PASS | `assert.Contains(t, out, "Uploaded "+testFolder+"/upload-test.txt", "each uploaded file is named")` (644); banner (643); baseline check (649) |

### ~/src/onedrive-go/e2e/sync_full_test.go — 19 / 8 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 278 | TestE2E_Sync_BidirectionalMerge | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` follows (281) |
| 294 | same | `"sync"` | CONVERGE | RSEC with predicate "data/info.txt present locally"; remote put at 291, unpolled `mustReadFile` at 302, unpolled `ls` at 298 |
| 316 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence, `sync_e2e_test.go:349` |
| 336 | TestE2E_Sync_EditDeleteConflict | `"sync","--upload-only"` | CONVERGE | RSTF; baseline setup |
| 340 | same | `"sync","--download-only"` | CONVERGE | RSTF; delta-token advance, no assertion (comment 338-339) |
| 408 | TestE2E_Sync_DeletePropagation | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 415-420 |
| 412 | same | `"sync","--download-only"` | CONVERGE | RSTF; delta-token advance (comment 410-411) |
| 487 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 549 | TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 550 |
| 591 | same | `"sync","--upload-only"` | CONVERGE | RSEC, because stdout is needed for the banner `assert.Contains(t, out, "(upload only)")` (592); end state at 593-594 |
| 615 | TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 616 |
| 699 | TestE2E_Sync_DirectionalModes_PreserveEditEditConflict | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 700 |
| 705 | same | `runSyncWithMode` → `"sync", mode.flag` (def 503-508) | SINGLE-PASS | `assert.Contains(t, out, "Conflict, kept both versions of")` (722); `assert.Contains(t, out, "Held back by --upload-only:")` (711); `assert.NotContains(t, out, "kept both versions of")` (710) |
| 751 | TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict | `"sync","--upload-only"` | CONVERGE | RSTF; setup |
| 752 | same | `"sync","--download-only"` | CONVERGE | RSTF; delta-token advance, polled `ls` at 753 |
| 836 | TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict | `runSyncWithMode` → `"sync", mode.flag` | SINGLE-PASS | `assert.Contains(t, out, "Conflict, kept both versions of")` (853); `assert.Contains(t, out, "Held back by --upload-only:")` (842) |
| 885 | TestE2E_Sync_NestedFolderHierarchy | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 888 |
| 906 | same | `"sync"` | CONVERGE | RSEC with predicate "deeper.txt present locally"; unpolled `mustReadFile` at 910, unpolled `ls` at 914-919 |
| 932 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 949 | TestE2E_Sync_DryRunNonDestructive | `"sync","--upload-only"` | CONVERGE | RSTF; baseline setup |
| 957 | same | `"sync","--dry-run"` | SINGLE-PASS | `assert.Contains(t, out, "Dry run, nothing was changed.")` (958); "Would upload" (959); "Would download" (960) |
| 975 | same | `"sync"` | CONVERGE | RSEC with predicate "pending-download.txt present locally"; unpolled `mustReadFile` at 982; remote side is polled (978-979) |
| 1007 | TestE2E_Sync_ConvergentEdit | `"sync","--upload-only"` | CONVERGE | RSTF; baseline setup |
| 1011 | same | `"sync","--download-only"` | CONVERGE | RSTF; delta-token advance (comment 1009-1010) |
| 1024 | same | `"sync"` | SINGLE-PASS | `assert.Contains(t, out, "Up to date")` (1028); `assert.NotContains(t, out, "kept both versions of")` (1029) |
| 1040 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 1065 | TestE2E_Sync_InternalBaselineVerificationDetectsTampering | `"sync","--upload-only"` | CONVERGE | RSTF; `require.Len(t, report.Mismatches, 2)` (1079) and `assert.Equal(t, 1, report.Verified)` (1080) need all three uploads in the baseline |

### ~/src/onedrive-go/e2e/sync_demoted_full_e2e_test.go — 4 / 3 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 27 | TestE2E_Sync_DryRun | `"sync","--upload-only","--dry-run"` | SINGLE-PASS | `assert.Contains(t, out, "Dry run, nothing was changed.")` (28); "Would upload" (29) |
| 46 | TestE2E_Sync_InternalBaselineVerification | `"sync","--upload-only"` | CONVERGE | RSTF; `assert.GreaterOrEqual(t, report.Verified, 1)` (51) |
| 68 | TestE2E_Sync_Conflicts | `"sync","--upload-only"` | CONVERGE | RSTF; seed-visible wait at 69 |
| 73 | same | `"sync"` | SINGLE-PASS | `assert.Contains(t, out, "Conflict, kept both versions of")` (74) |
| 80 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 121 | TestE2E_Sync_DriveRemoveAndReAdd | `"sync","--upload-only"` | CONVERGE | RSEC (banner at 122 needs stdout); baseline path at 125 |
| 135 | same (cfgPath2) | `"sync","--upload-only"` | CONVERGE | RSEC (banner at 136); baseline paths at 139-140 |

### ~/src/onedrive-go/e2e/sync_edge_cases_full_test.go — 5 / 2 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 34 | TestE2E_Sync_EmptyDirectory | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 37 |
| 42 | same | `"sync","--download-only"` | CONVERGE | RSTF; delta-token advance (comment 39-41) |
| 112 | TestE2E_Sync_NestedDeletion | `"sync","--download-only"` | CONVERGE | RSTF; delta-token advance (comment 111) |
| 171 | TestE2E_Sync_MtimeOnlyChange | `"sync","--upload-only"` | CONVERGE | RSTF; baseline for the no-re-upload check at 180-182 |
| 180 | same | stable helper, `"sync","--upload-only"` | SINGLE-PASS | `assert.NotContains(t, out, "Uploaded "+testFolder+"/", ...)` (181-182) |
| 207 | TestE2E_Sync_IdempotentReSync | `"sync"` | CONVERGE | RSTF; first pass before the stable check (211) and `ls` (214-221) |
| 211 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence |

### ~/src/onedrive-go/e2e/sync_recovery_e2e_test.go — 6 / 4 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 51 | TestE2E_Sync_IncrementalDeltaToken | `"sync","--upload-only"` | CONVERGE | RSTF; baseline for 56-58 |
| 56 | same | stable helper, `"sync","--upload-only"` | SINGLE-PASS | `assert.NotContains(t, out, "Uploaded "+testFolder+"/", ...)` (57-58) |
| 121 | TestE2E_Sync_CrashRecoveryIdempotent | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 124 |
| 138 | same | `"sync"` | CONVERGE | RSTF, so that 141 is not failed by leftover retry work. Remote deletes at 127-128 are not waited for, so delta lag can still disturb 141; only RSEC with a predicate on the two deleted files would cover that |
| 141 | same | `"sync"` | SINGLE-PASS | `assert.Contains(t, out, "Up to date", "immediate re-sync after mixed changes should be idempotent")` (142-143) |
| 168 | TestE2E_Sync_ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 169 |
| 173 | same | `"sync","--download-only"` | CONVERGE | RSTF; delta-token setup (comment 171-172) |
| 240 | same | `"sync","--download-only"` | SINGLE-PASS | `assert.Contains(t, out, "Up to date", ...)` (241-242); snapshot equality (243) |
| 266 | TestE2E_Sync_DriveRemovePurgeResetsState | `"sync","--upload-only"` | CONVERGE | RSTF; polled `ls` at 269 |
| 284 | same | `"sync","--download-only"` | SINGLE-PASS | `assert.Contains(t, out, "Done in", ...)` (285-286); `assert.Contains(t, out, "Downloaded "+testFolder+"/purge-a.txt", ...)` (287-288) |

### ~/src/onedrive-go/e2e/sync_kill_mid_transfer_full_e2e_test.go — 0 / 3 / 0

| Line | Test | Args | Verdict | Evidence |
|---|---|---|---|---|
| 88 | TestE2E_Sync_KillMidUploadResumesSession | `"sync","--upload-only"` | SINGLE-PASS | `assert.Contains(t, stderr, stderrUploadSessionResumed, ...)` (89); `assert.NotContains(t, stderr, stderrUploadSessionExpired)` (90); recovery of exactly the next pass |
| 96 | same | `"sync","--upload-only"` | SINGLE-PASS | `assert.Contains(t, out, "Up to date", "a resumed upload should leave nothing for the next sync")` (97) |
| 140 | TestE2E_Sync_KillMidDownloadResumesPartial | `"sync","--download-only"` | SINGLE-PASS | `assert.Contains(t, stderr, stderrDownloadPartialResume, ...)` (141) |

### ~/src/onedrive-go/e2e/sync_safety_full_e2e_test.go — 2 / 1 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 37 | helper `seedSafetySyncRoot` (def 26; called at 76, 129, 180) | `"sync"` | CONVERGE | RSTF; polled remote reads at 38-40. One site, three tests |
| 109 | TestE2E_SyncFull_DeleteHoldKeepsOneDriveUntilConfirmed | `"sync","--confirm-deletes"` | CONVERGE | RSTF; `assert.Eventually` on the remote listing at 110-112. A retry repeats a pass carrying the confirmation flag; the root is isolated (74), so that is contained |
| 114 | same | `"sync"` | SINGLE-PASS | `assert.NotContains(t, stdout+stderr, "deletes for confirmation", "nothing is held once applied")` (115) |

### ~/src/onedrive-go/e2e/sync_hardening_full_e2e_test.go — 6 / 8 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 137 | TestE2E_Sync_FolderMovesBothWays | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 150 | same | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 177 | TestE2E_Sync_CrossMoves | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 247 | TestE2E_Sync_NamingCorpusRoundtrip | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 248 | same (otherCfg) | stable helper, `"sync"` | SINGLE-PASS | idempotence |
| 267 | TestE2E_Sync_TwoWriters | `(cfgA, envA, "sync")` | CONVERGE | RSTF; `syncUntil` on B follows (268) |
| 276 | same | `(cfgA, envA, "sync")` | CONVERGE | RSTF; `syncUntil` on B (277) |
| 284 | same | `(cfgA, envA, "sync")` | CONVERGE | RSTF; `syncUntil` on B (285) |
| 295 | same | stable helper `(cfgA, envA, dirA, "sync")` | SINGLE-PASS | idempotence |
| 296 | same | stable helper `(cfgB, envB, dirB, "sync")` | SINGLE-PASS | idempotence |
| 323 | TestE2E_Sync_LargeFileBetweenClients | `(cfgA, envA, "sync","--upload-only")` | CONVERGE | RSTF; polled remote size at 324; 25 MiB multi-fragment upload |
| 328 | same | `(cfgB, envB, "sync","--download-only")` | CONVERGE | `syncUntil` (107-112) with predicate "large.bin present and equal"; unpolled `mustReadFile` (329) and hash equality (330) |
| 335 | same | `(cfgB, envB, "sync")` | CONVERGE | RSTF; `syncUntil` on A follows (336) |
| 375 | TestE2E_Sync_TypeChangesBothWays | stable helper, `"sync"` | SINGLE-PASS | idempotence |

### ~/src/onedrive-go/e2e/sync_shortcut_full_e2e_test.go — 9 / 2 / 0

All sites are `runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", <mode>)`. For the download-only projections the fitting helper is the file's own `syncShortcutDownloadUntilProjected` (306-346, already used at 107 and 150).

| Line | Test | Mode arg | Verdict | Evidence or helper |
|---|---|---|---|---|
| 30 | TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount | `"--download-only"` | CONVERGE | shortcut loop; child-mount and file checks at 32-34 |
| 72 | TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible | `"--download-only"` | CONVERGE | shortcut loop; first pass, record read at 73 |
| 74 | same | `"--download-only"` | SINGLE-PASS | `assert.Equal(t, firstRecord.MountID, secondRecord.MountID)` (77); idempotence of the second run, needs only exit 0 |
| 91 | TestE2E_Shortcut_RenameReusesChildMountState | `"--download-only"` | CONVERGE | shortcut loop; 92-94 |
| 130 | TestE2E_Shortcut_MoveReusesChildMountState | `"--download-only"` | CONVERGE | shortcut loop; 131-133 |
| 173 | TestE2E_Shortcut_WritableUploadSyncsToSharedTarget | `"--download-only"` | CONVERGE | shortcut loop; 174 |
| 187 | same | `"--upload-only"` | CONVERGE | needs a drive-aware RSEC (banner at 188, polled remote read at 190); none exists |
| 201 | TestE2E_Shortcut_ReadOnlyBlockedUploadStatus | `"--download-only"` | CONVERGE | shortcut loop; 202 |
| 208 | same | `"--upload-only"` | SINGLE-PASS | pass must record a blocked write: `assert.Contains(t, statusOut, "Shared folder writes blocked")` (212); "Downloads continue normally." (213); blocked name (214). A left-work loop would not drain |
| 255 | TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget | `"--download-only"` | CONVERGE | shortcut loop; pre-daemon projection (256) |
| 282 | TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot | `"--download-only"` | CONVERGE | shortcut loop; pre-daemon projection (283) |

### ~/src/onedrive-go/e2e/sync_shortcut_e2e_test.go — 1 / 0 / 0

| Line | Test | Args | Verdict | Helper |
|---|---|---|---|---|
| 23 | TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount | `runCLIWithConfigForDrive(..., fixture.ParentDrive, "sync", "--download-only")` | CONVERGE | banner (24), child mount (26), dir and file (28-29). Needs a drive-aware converge helper visible to the fast tier; the shortcut loop is `e2e_full` only |

### ~/src/onedrive-go/e2e/sync_shared_e2e_test.go — 0 / 1 / 0

| Line | Test | Args | Verdict | Evidence |
|---|---|---|---|---|
| 116 (args at 122-123) | TestE2E_SharedFolder_RecipientSyncTwice_Idempotent | `assertSyncLeavesLocalTreeStableForDrive(..., "sync", "--download-only")` | SINGLE-PASS | idempotence, `shared_helpers_test.go:66`; banner at 125 |

### ~/src/onedrive-go/e2e/cli_commands_e2e_test.go — 8 / 0 / 0

All eight are `"sync","--upload-only"` and all fit RSTF.

| Line | Test | Verdict | Evidence |
|---|---|---|---|
| 42 | TestE2E_Status_AfterSync | CONVERGE | status text checks at 46-48 |
| 191 | TestE2E_Status_PerDrive_NoIssuesOrRetries | CONVERGE | `assert.Empty(t, status.Issues)` (194); `assert.Zero(t, status.IssueCount)` (195); `assert.Zero(t, status.Retrying)` (196). One transient failure leaves `Retrying` non-zero |
| 216 | TestE2E_Status_NoLegacyHistorySurface | CONVERGE | `assert.Zero(t, current.IssueCount, ...)` (219) |
| 243 | TestE2E_Status_JSON_IssueDetails | CONVERGE | `require.Len(t, status.Issues, 1, ...)` (246); scan-time `invalid_filename`, durable in status |
| 275 | TestE2E_CLI_NoResolveCommand | CONVERGE | setup before the unknown-command check (277-278); low value |
| 300 | TestE2E_InternalBaselineVerification_AfterSync | CONVERGE | `assert.Equal(t, 3, report.Verified)` (304); `assert.Empty(t, report.Mismatches)` (305) |
| 745 | TestE2E_Status_IssueLifecycle | CONVERGE | `require.Len(t, status.Issues, 1)` (748); `assert.Equal(t, "Path too long", status.Issues[0].Title)` (749). The comment at 744 calls the pass the trigger, but the assertion is on durable status and `path_too_long` is a scan-time finding, so RSTF is behaviour-neutral; low value |
| 758 | same | CONVERGE | `assert.Empty(t, status.Issues, "status should be clean after the next sync")` (761); unpolled `ls` for fixed.txt (763-764). The message says "the next sync", but the property under test is that no manual clear is needed (728-730) |

### ~/src/onedrive-go/e2e/drive_list_full_test.go — 1 / 0 / 0

| Line | Test | Args | Verdict | Helper |
|---|---|---|---|---|
| 126 | TestE2E_DriveList_StaleStateDB | `"sync","--upload-only"` | CONVERGE | RSTF; polled remote read at 130 |

### ~/src/onedrive-go/e2e/output_validation_e2e_test.go — 1 / 2 / 0

| Line | Test | Args | Verdict | Evidence or helper |
|---|---|---|---|---|
| 40 | TestE2E_Status_JSONShape | `"sync","--upload-only"` | CONVERGE | RSTF; setup for status shape checks (42-48) |
| 108 | TestE2E_Sync_QuietMode | `"sync","--quiet","--upload-only"` | SINGLE-PASS | `assert.NotContains(t, line, "level=DEBUG", ...)` (117-118); `assert.NotContains(t, line, "level=INFO", ...)` (119-120) on that pass's stderr |
| 155 | TestE2E_Sync_MultiDriveReport | `runCLIWithConfigAllDrives(..., "sync","--upload-only")` | SINGLE-PASS | ``assert.Regexp(t, `(?m)^.+: (Done in|Up to date)`, out, ...)`` (158) |

### ~/src/onedrive-go/e2e/orchestrator_e2e_test.go — 3 / 0 / 0

| Line | Test | Args | Verdict | Helper |
|---|---|---|---|---|
| 66 | TestE2E_Orchestrator_SimultaneousSync | `runCLIWithConfigAllDrives(..., "sync","--upload-only")` | CONVERGE | needs an all-drives variant; drive 1 is polled (70), drive 2 is checked by an unpolled `stat` (74-75) |
| 120 | TestE2E_Orchestrator_DriveIsolation | `runCLIWithConfigAllDrives(..., "sync","--download-only")` | CONVERGE | needs an all-drives RSEC with predicate "file present in syncDir1"; remote put at 115 is not waited for, and `os.Stat` plus `require.NoError(..., "file should exist in drive1 sync dir")` follow at once (124-125) |
| 230 | TestE2E_Orchestrator_SelectiveDrive | `runCLIWithConfigForDrive(..., drive, "sync","--upload-only")` | CONVERGE | needs a drive-aware RSTF; polled read at 234, negative check on drive 2 at 239-242 |

### ~/src/onedrive-go/e2e/shared_items_e2e_test.go — 1 / 1 / 1

All three are in TestE2E_Shared_ReadOnlyFolder_DiscoveryDriveAddAndBlockedWriteUX (def 174) and use `runCLIWithConfigForDrive(..., readOnlyFixture.FolderItem.Selector, "sync", <mode>)`.

| Line | Mode arg | Verdict | Evidence |
|---|---|---|---|
| 195 | `"--download-only"` | CONVERGE | only `assert.Contains(t, out, "(download only)")` (196); needs a drive-aware RSEC |
| 202 | `"--upload-only"` | SINGLE-PASS | pass must record the blocked write: `assert.Contains(t, statusOut, "Shared folder writes blocked")` (208); "Downloads continue normally." (209); file name (210) |
| 212 | `"--download-only"` | UNCLEAR | only exit 0 and `assert.Contains(t, out, "(download only)")` (213), with no end state checked. It may be the proof that a download-only pass still exits 0 while the write block stands (then SINGLE-PASS), or just a closing pass. Whether a left-work loop would drain while the blocked file remains was not established. Leans SINGLE-PASS |

### ~/src/onedrive-go/e2e/sync_watch_full_test.go — 0 / 1 / 0

| Line | Test | Args | Verdict | Evidence |
|---|---|---|---|---|
| 525 | TestE2E_SyncWatch_GracefulShutdown (def 486) | `"sync","--upload-only"` | SINGLE-PASS | `assert.Contains(t, out, "Uploaded ", "one-shot should name the uploaded file")` (528); polled remote reads at 531-532 |

## (a) Single runs that tolerate or expect an error — 10

| File:line | Test | Runner and args | Note |
|---|---|---|---|
| `orchestrator_e2e_test.go:174` | TestE2E_Orchestrator_OneDriveFails | `runCLIWithConfigAllDrivesAllowError`, `"sync","--upload-only"` | `require.Error` at 177 |
| `sync_safety_full_e2e_test.go:80` | TestE2E_SyncFull_DeleteHoldKeepsOneDriveUntilConfirmed | `runCLIWithConfigAllowError`, `"sync"` | |
| `sync_safety_full_e2e_test.go:104` | same | `runCLIWithConfigAllowError`, `"sync"` | |
| `sync_safety_full_e2e_test.go:187` | TestE2E_SyncFull_ReplacedSyncFolderIsRefusedUntilRebound | `runCLIWithConfigExpectError`, `"sync"` | |
| `sync_safety_full_e2e_test.go:196` | same | `runCLIWithConfigAllowError`, `"sync"` | |
| `sync_shortcut_full_e2e_test.go:235` | TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes | `runCLICore`, `"sync","--upload-only"` | `require.Error` at 236 |
| `sync_hardening_full_e2e_test.go:215` | TestE2E_Sync_NamingCorpusRoundtrip | `runCLIBestEffort`, `"sync","--upload-only"` | |
| `sync_hardening_full_e2e_test.go:235` | same | `runCLIBestEffort`, `"sync","--download-only"` | followed by unpolled content assertions at 236-245 |
| `sync_watch_e2e_test.go:189` | TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners | closure `assertCompetingOwnerFails` (153-187), `"sync","--watch"` | `require.Error` at 176 |
| `sync_watch_e2e_test.go:190` | same | same closure, `"sync"` | `require.Error` at 176 |

### Hand-rolled retry loops around a sync — 4

| File:line | Test or helper | Shape |
|---|---|---|
| `sync_safety_full_e2e_test.go:137` | TestE2E_SyncFull_RemoteDeleteGoesToLocalTrashAndRestores | `runCLIWithConfig "sync"` inside `require.Eventually` (136-140) |
| `sync_shared_e2e_test.go:79` | TestE2E_SharedFolder_RemoteMutationSyncsToRecipient | `runCLIWithConfigForDrive "sync","--download-only"` inside `require.Eventually` (78-86) |
| `sync_shared_e2e_test.go:111` | TestE2E_SharedFolder_RecipientSyncTwice_Idempotent | same, inside `require.Eventually` (110-114) |
| `sync_shortcut_full_e2e_test.go:320` | helper `syncShortcutDownloadUntilProjected` | loop over `runCLICore` |

The three `require.Eventually` loops use requires-success runners, so a transient non-zero exit inside the loop still fails the test.

## (b) Calls already behind a converge helper — 36

| File | Count | Lines |
|---|---|---|
| `edge_cases_e2e_test.go` | 4 | RSTF 41, 75, 110, 150 |
| `sync_e2e_test.go` | 1 | RSEC 684 |
| `sync_full_test.go` | 6 | RSEC 353, 452, 559, 625, 652; `requireSyncWithModeEventuallyConverges` 759 |
| `sync_recovery_e2e_test.go` | 3 | RSEC 66, 180, 207 |
| `sync_edge_cases_full_test.go` | 4 | RSEC 56, 91, 125, 249 |
| `sync_hardening_full_e2e_test.go` | 16 | RSTF 126, 129, 133, 165, 167, 354, 356, 361; `syncUntil` 141, 172, 268, 277, 285, 289, 336, 368 |
| `sync_shortcut_full_e2e_test.go` | 2 | `syncShortcutDownloadUntilProjected` 107, 150 |

## Watch daemons, not analysed — 23 lines

- `orchestrator_e2e_test.go`: 272, 332, 416
- `socketio_e2e_test.go`: 74
- `socketio_full_e2e_test.go`: 34, 64
- `socketio_demoted_full_e2e_test.go`: 22
- `sync_watch_e2e_test.go`: 71, 135, 210, 317
- `sync_watch_full_test.go`: 98, 129, 172, 241, 297, 335, 366, 404, 447, 496, 548
- `sync_shortcut_full_e2e_test.go`: 370 (inside `withShortcutWatchDaemon`, 357-387)

## Grep accounting

I cross-checked the list against `grep -n '"sync"' ~/src/onedrive-go/e2e/*.go`. It returns 155 lines (per-file `grep -c` sums to 155), and every line is assigned to exactly one category:

| Category | Lines |
|---|---|
| Single-pass, requires-success call sites | 102 |
| Error-tolerant or error-expected runs (list a) | 10 |
| Hand-rolled retry loops | 4 |
| Watch-daemon argument lists | 23 |
| Subprocess killed mid-transfer (`startKillableSync`, `sync_kill_mid_transfer_full_e2e_test.go:78`, 128) | 2 |
| Helper definitions (`e2e_test.go:503`, 580; `sync_full_test.go:506`) | 3 |
| Go `"sync"` package imports (`quirk_helpers_test.go:12`, `shared_items_helpers_test.go:12`, `socketio_e2e_test.go:10`, `sync_watch_e2e_test.go:13`, `timing_helpers_test.go:13`) | 5 |
| Strings that are not a sync run | 6 |
| **Total** | **155** |

The six non-run strings are:
- `quirk_helpers_test.go:467` — argument fixture in a unit test
- `shared_items_helpers_test.go:1079` — `filepath.Join(perTestHome, "sync")`
- `timing_helpers_test.go:342`, 368, 382 — fixture events
- `sync_safety_full_e2e_test.go:176` — `filepath.Join(parent, "sync")`

Two further call sites do not appear in that grep because the literal sits in a wrapper: `sync_full_test.go:705` and 836 (`runSyncWithMode`). I found them with a second grep for the wrapper and helper names. That gives 102 + 2 = 104. A third grep for argument slices carrying `"sync"` found nothing else.

## What was read

- **Read in full:** `sync_recovery_e2e_test.go`, `sync_demoted_full_e2e_test.go`, `sync_kill_mid_transfer_full_e2e_test.go`, `sync_safety_full_e2e_test.go`, `sync_hardening_full_e2e_test.go`, `sync_shortcut_full_e2e_test.go`, `sync_shortcut_e2e_test.go`, `sync_shared_e2e_test.go`, `output_validation_e2e_test.go`, `edge_cases_e2e_test.go`.
- **Read in part:**
  - `sync_full_test.go` 250-1089
  - `sync_e2e_test.go` 325-364 and 610-714
  - `e2e_test.go` 440-610
  - `sync_edge_cases_full_test.go` 1-260
  - `cli_commands_e2e_test.go` 20-319 and 722-766
  - `drive_list_full_test.go` 100-176
  - `orchestrator_e2e_test.go` 28-257
  - `shared_items_e2e_test.go` 168-215
  - `sync_watch_full_test.go` 480-549
  - `sync_watch_e2e_test.go` 120-199
  - `cli_runner_test.go` runner definitions (63-202)
  - `shared_helpers_test.go` 53-69
  - snippets around each non-run string
- **Production code read for helper semantics:** `internal/cli/sync_console.go` 290-364, `internal/sync/engine_results.go` 60-189, `internal/sync/engine_result_classify.go` 20-189, `internal/sync/issue_types.go` 1-20.
- **Build tags** were taken from a `^//go:build` grep of the fifteen files it was run on; the two not in that grep (`sync_demoted_full_e2e_test.go`, `sync_edge_cases_full_test.go`) were read at line 1.
