# e2e wait audit: remote name-only waits (Task A) and local download waits (Task B)

Read-only; nothing was modified, built or run. All e2e files below are under `~/src/onedrive-go/e2e/`; product files under `~/src/onedrive-go/internal/`. Every line cited was read.

**Result:** 80 name-only remote wait call sites: **18 RACY-DAEMON, 62 SAFE, 0 UNCLEAR**. No racy local (download-side) wait was found.

## 0. Product facts the classification rests on

| Fact | Citation |
|---|---|
| New non-empty file = two requests: empty item with final name, then content by item ID | `graph/item_create.go:18-19` ("Creating a file is two requests: create the empty item with its name in a JSON body and conflictBehavior=fail, then send the content by item ID."); `CreateFile` :59-94; `UploadCreating` :152-179 |
| Zero-byte file has no content step | `graph/item_create.go:161-163` (`if size == 0 { return c.finalizeSimpleUpload(...) }`) |
| A leftover placeholder is a real 0-byte remote file | `graph/item_create.go:204-212`, `removeCreatedPlaceholder` :213-239 (deletes only while `current.Size == 0`, :229) |
| Sync upload of a new file takes this path | `sync/executor_transfer.go:252` → `uploadNewFile` :487-505 → `UploadFile` :496; `driveops/transfer_upload.go:178-182` (`UploadCreating`), :162 (session for > 4 MiB, threshold :56) |
| Overwrite of a known remote item goes by item ID, no placeholder; old content stays until commit | `sync/executor_transfer.go:213`, :218-220 (`IfMatch`), `shouldOverwriteKnownRemoteItem` :462-472; `driveops/transfer_upload.go:322-408`, :432, :445 |
| No visibility probe after upload | `sync/executor_transfer.go:272-275` |
| `waitForRemoteReadContains` is name-only | `remote_read_helpers_test.go:204-206` (`err == nil && strings.Contains(stdout, expected)`) |
| `pollForDrive2File` is name-only, with driveID | `sync_watch_e2e_test.go:393-394` |
| `waitForRemoteExactStatVisible` = stat success only | `remote_read_helpers_test.go:247-269` |
| `waitForRemoteFixtureSeedVisible` = stat success OR name in parent listing | `remote_read_helpers_test.go:325-391` |
| `waitForRemoteStatSize` proves size, but **has no driveID parameter** | `remote_read_helpers_test.go:214-243`; driveID hard-coded `""` at :228; ready at :239 (`stat.Size == size`) |
| CLI `cp` waits for the async copy to finish before exiting | `cli/cp.go:151` (`wait(ctx, cc, session.Meta, copyResult.MonitorURL)`), `awaitCopy` :190-214, "Copied" printed only at :173 |

Only six e2e files start a long-lived process (`cmd.Start`): `sync_watch_e2e_test.go` (:80, :144, :219, :326), `sync_watch_full_test.go` (:69 via `startDaemonWithStderr`), `orchestrator_e2e_test.go` (:281, :341, :425), `socketio_e2e_test.go` (:50 via `startFastDaemon`), `sync_shortcut_full_e2e_test.go` (:379 via `withShortcutWatchDaemon`), `sync_kill_mid_transfer_full_e2e_test.go` (:193). No other file can hold a RACY-DAEMON site.

## A1. RACY-DAEMON (18)

Rows 1-3 are the same pattern as the known failure (daemon stop plus content assertion). "SS" = `waitForRemoteStatSize`.

| # | file:line | Test (line) | Wait call | Producer (live daemon) | What follows | Replacement |
|---|---|---|---|---|---|---|
| 1 | sync_watch_e2e_test.go:103 | `TestE2E_SyncWatch_BasicRoundTrip` (:56) | `waitForRemoteReadContains(t, opsCfgPath, nil, "", "watch-test.txt", 3*time.Minute, "stat", remotePath)` | `sync --watch --upload-only` (args :71, `cmd.Start` :80); local write :95-99 | SIGTERM :106, `cmd.Wait` :109, `getRemoteFile` :120, `assert.Equal` :121 | `SS(t, opsCfgPath, nil, remotePath, int64(len("created during watch mode\n")), 3*time.Minute)` |
| 2 | sync_watch_e2e_test.go:261 | `TestE2E_SyncWatch_PauseResume` (:195) — the known failure | `waitForRemoteReadContains(t, opsCfgPath, nil, "", "paused-file.txt", 3*time.Minute, "stat", remotePath)` | daemon `cmd.Start` :219; local write :239-243; `resume` :258 | SIGTERM :264, `waitForDaemonExit` :265, `getRemoteFile` :268, `assert.Equal` :269 | `SS(..., remotePath, int64(len("created while paused\n")), 3*time.Minute)` |
| 3 | sync_watch_e2e_test.go:348 | `TestE2E_SyncWatch_ControlSocketReload` (:275) | `waitForRemoteReadContains(t, opsCfgPath, nil, "", "before-reload.txt", 3*time.Minute, "stat", remotePath1)` | all-drives `sync --watch --upload-only` (args :314-318, `cmd.Start` :326); local write :340-344 | config rewrite :353, reload :357, `waitForControlSocketMount` :358, drive2 write :363-367, drive2 wait :371, SIGTERM :374, `waitForDaemonExit` :375, `getRemoteFile` :378, `assert.Equal` :379. More slack than rows 1-2, same race | `SS(t, opsCfgPath, nil, remotePath1, int64(len("before reload\n")), 3*time.Minute)` |
| 4 | sync_watch_e2e_test.go:371 | same test | `pollForDrive2File(t, cfgPath, env, drive2, "after-reload.txt", 3*time.Minute, "stat", remotePath2)` | same daemon; drive2 local write :363-367 | SIGTERM :374; no content assertion on this file (latent: leaves a 0-byte item, test still passes) | Drive-aware helper needed (SS has no driveID): content `"after reload\n"` |
| 5 | sync_watch_full_test.go:142 | `TestE2E_SyncWatch_Bidirectional` (:119) | `waitForRemoteReadContains(t, cfgPath, env, "", "local-to-remote.txt", daemonPollTimeout, "stat", remotePath)` | `startDaemon` :128 (`sync --watch`); local write `"from local"` :134-138 | `putRemoteFile` of another file :145, `pollLocalFileContent` :148, SIGTERM :151 (latent) | `SS(t, cfgPath, env, remotePath, int64(len("from local")), daemonPollTimeout)` |
| 6 | sync_watch_full_test.go:185 | `TestE2E_SyncWatch_ConflictDuringWatch` (:161) | `... "conflict-watch.txt", daemonPollTimeout, "stat", remotePath)` | `startDaemonWithStderr` :171; local write `"baseline"` :181 | `putRemoteFile` to the SAME path :190, sleep :193, local write :197. The remote mutation assumes the upload finished | `SS(..., remotePath, int64(len("baseline")), daemonPollTimeout)` |
| 7 | sync_watch_full_test.go:254 | `TestE2E_SyncWatch_FileModification` (:230) | `... "modifiable.txt", daemonPollTimeout, "stat", remotePath)` | daemon :240; local write `"version 1"` :250 | local modify `"version 2 modified"` :257, content loop via `getRemoteFile` :260-273, SIGTERM :276. Converges anyway, but the scenario (modify of an uploaded file) is weakened | `SS(..., remotePath, int64(len("version 1")), daemonPollTimeout)` |
| 8 | sync_watch_full_test.go:310 | `TestE2E_SyncWatch_FileDeletion` (:286) | `... "deleteme.txt", daemonPollTimeout, "stat", remotePath)` | daemon :296; local write `"will be deleted"` :306 | `os.Remove` :313, `waitForRemoteDeleteDisappearance` :316, SIGTERM :319. Removing the source mid-upload can leave a placeholder the disappearance wait then waits on | `SS(..., remotePath, int64(len("will be deleted")), daemonPollTimeout)` |
| 9 | sync_watch_full_test.go:347 | `TestE2E_SyncWatch_FolderCreation` (:325) | `... "deep.txt", daemonPollTimeout, "ls", "/"+testFolder+"/a/b/c")` | daemon :334; local write `"deeply nested content"` :340-344 | SIGTERM :350 (latent) | `SS(t, cfgPath, env, "/"+testFolder+"/a/b/c/deep.txt", int64(len("deeply nested content")), daemonPollTimeout)` |
| 10 | sync_watch_full_test.go:384 | `TestE2E_SyncWatch_MultipleFiles` (:356); one call site, looped over 5 names | `... name, daemonPollTimeout, "ls", "/"+testFolder)` | daemon :365; writes `fmt.Sprintf("multi content %d", i)` :372-379 | SIGTERM :388 (latent) | per file: `SS(t, cfgPath, env, "/"+testFolder+"/"+name, int64(len(fmt.Sprintf("multi content %d", i))), daemonPollTimeout)` |
| 11 | sync_watch_full_test.go:508 | `TestE2E_SyncWatch_GracefulShutdown` (:486) | `... "before-shutdown.txt", daemonPollTimeout, "stat", remotePath)` | daemon :495; local write `"synced by daemon"` :501-505 | SIGTERM :511, `cmd.Wait` :512, new local file :518-522, one-shot `sync --upload-only` :525, `assert.Contains(out, "Uploaded ")` :528 | `SS(..., remotePath, int64(len("synced by daemon")), daemonPollTimeout)` |
| 12 | sync_watch_full_test.go:583 | `TestE2E_SyncWatch_TimedPauseExpiry` (:538) | `... "paused-file.txt", daemonPollTimeout, "stat", remotePath)` | daemon :547; local write `"created during pause"` :559-563 | SIGTERM :586 (latent) | `SS(..., remotePath, int64(len("created during pause")), daemonPollTimeout)` |
| 13 | orchestrator_e2e_test.go:304 | `TestE2E_Orchestrator_WatchSimultaneous` (:251) | `waitForRemoteReadContains(t, opsCfgPath, nil, "", "watch-d1.txt", 3*time.Minute, "stat", remotePath1)` | `sync --watch --upload-only` (args :272, `cmd.Start` :281); local write `"watch drive1"` :295 | drive2 wait :307, SIGTERM :310, `waitForDaemonExit` :311 (latent) | `SS(t, opsCfgPath, nil, remotePath1, int64(len("watch drive1")), 3*time.Minute)` |
| 14 | orchestrator_e2e_test.go:307 | same test | `pollForDrive2File(t, cfgPath, env, drive2, "watch-d2.txt", 3*time.Minute, "stat", remotePath2)` | same daemon; local write `"watch drive2"` :300 | SIGTERM :310 (latent) | Drive-aware helper needed; content `"watch drive2"` |
| 15 | orchestrator_e2e_test.go:359 | `TestE2E_Orchestrator_WatchDriveIsolation` (:316) | `... "isolated.txt", 3*time.Minute, "stat", remotePath)` | args :332, `cmd.Start` :341; local write `"drive1 only"` :355 | drive2 negative stat :362-365, SIGTERM :368 (latent) | `SS(t, opsCfgPath, nil, remotePath, int64(len("drive1 only")), 3*time.Minute)` |
| 16 | orchestrator_e2e_test.go:448 | `TestE2E_Orchestrator_WatchPausedDrive` (:374) | `... "active.txt", 3*time.Minute, "stat", remotePath1)` | args :416, `cmd.Start` :425; local write `"active drive"` :439 | bounded negative check :451-466, SIGTERM :469 (latent) | `SS(t, opsCfgPath, nil, remotePath1, int64(len("active drive")), 3*time.Minute)` |
| 17 | socketio_e2e_test.go:84 (FAST tier, tag `e2e`) | `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression` (:62) | `waitForRemoteReadContains(t, opsCfgPath, nil, "", "upload.txt", 3*time.Minute, "stat", "/"+testFolder+"/upload.txt")` | `startFastDaemon` :73-74 (`sync --watch --upload-only`, `cmd.Start` :50); local write `"websocket disabled regression\n"` :78-82 | SIGTERM :86, `waitForDaemonExit` :87, `assertNoSocketIOConnected` :88 (latent) | `SS(t, opsCfgPath, nil, "/"+testFolder+"/upload.txt", int64(len("websocket disabled regression\n")), 3*time.Minute)` |
| 18 | sync_shortcut_full_e2e_test.go:271 | `TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget` (:248) | `waitForRemoteReadContains(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, remoteName, 3*time.Minute, "stat", remotePath)` | `withShortcutWatchDaemon(..., "--upload-only")` :265 (`cmd.Start` :379); local write `"watch local shortcut upload\n"` :269 | test ends: cleanup `stopDaemon` :381 runs first (LIFO), then the `rm` cleanup registered at :261-263 (latent) | Drive-aware helper needed (non-empty driveID); length `int64(len("watch local shortcut upload\n"))` |

**Helper gap.** Rows 4, 14 and 18 cannot use `waitForRemoteStatSize` as it stands (`remote_read_helpers_test.go:228` hard-codes driveID `""`). Either add a driveID parameter, or add `waitForRemoteFileContent(t, cfgPath, env, driveID, remotePath, want string, timeout)` polling `get` until the content equals `want`. The content helper is also the only correct choice for a same-length modification. Existing code to build it from: `eventuallySharedContentEquals` (`shared_items_helpers_test.go:1433-1459`: one-shot `get` :1447, `os.ReadFile` :1452, `string(data) == expected` :1457), `getRemoteFileForDrive` (`remote_read_helpers_test.go:438-471`, single download, no content loop), and the inline loops at `sync_watch_full_test.go:260-273` and :463-476.

## A2. SAFE (62)

All use `waitForRemoteReadContains` unless noted. "Opt." = a size or content wait would serve the following assertion better, although it is not the daemon race.

| file:line | Test / helper (line) | Name, command | Producer (already exited) | What follows | Note |
|---|---|---|---|---|---|
| sync_watch_full_test.go:460 | `TestE2E_SyncWatch_RapidChurn` (:437) | `"churn-watch.txt"`, stat | live daemon :446 | content loop :463-476, then SIGTERM :479 | Safe only because the content loop precedes the stop; the name wait is redundant |
| sync_watch_full_test.go:531 | `TestE2E_SyncWatch_GracefulShutdown` (:486) | `"after-shutdown.txt"`, stat | one-shot `sync --upload-only` :525 | :532 | |
| sync_watch_full_test.go:532 | same | `"before-shutdown.txt"`, stat | same | end | |
| orchestrator_e2e_test.go:70 | `TestE2E_Orchestrator_SimultaneousSync` (:36) | `"drive1-file.txt"`, stat | one-shot all-drives `sync --upload-only` :66 | drive2 stat :74, `assert.Contains` :75 | |
| orchestrator_e2e_test.go:196 | `..._OneDriveFails` (:136) | `"survive.txt"`, stat | one-shot sync :174 | nothing | |
| orchestrator_e2e_test.go:234 | `..._SelectiveDrive` (:201) | `"selected.txt"`, stat | one-shot sync :230 | negative drive2 stat :239-242 | |
| sync_shortcut_full_e2e_test.go:190 | `TestE2E_Shortcut_WritableUploadSyncsToSharedTarget` (:166) | `remoteName`, stat, shared selector as driveID | one-shot `sync --upload-only` :187 | nothing | Content proof (`expectedContent` :178) would need the drive-aware helper |
| sync_shortcut_full_e2e_test.go:244 | `..._LocalRootCollisionSkipsChildButParentCompletes` (:218) | `parentName`, stat, `fixture.ParentDrive` | one-shot sync :235 | nothing | |
| sync_shortcut_full_e2e_test.go:458 | helper `restoreRemoteShortcutPath` (:433) | cleanup name wait | one-shot `mv` :447 | nothing | |
| sync_e2e_test.go:676 | `TestE2E_Sync_DownloadOnly` (:652) | `waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, remotePath)` | one-shot `put` :675 | converge content wait :684 | |
| sync_full_test.go:126 | `testUnicodeFilename` (:106) | `remoteName`, ls | one-shot `put` :122 | `get` :132, `mustReadFile` :135, `assert.Equal` :136 | Opt.: SS with `int64(len(content))` |
| sync_full_test.go:165 | `testSpacesInFilename` (:145) | `remoteName`, stat | `put` :161 | `get` :171, :174, `assert.Equal` :175 | Opt.: SS with `int64(len(content))` |
| sync_full_test.go:247 | `testConcurrentUploads` (:199) | last file name, ls | three parallel `put` (`cmd.Run` :230), joined :240-243 | `assert.Contains` :249 | |
| sync_full_test.go:281 | `TestE2E_Sync_BidirectionalMerge` (:264) | `"readme.txt"`, ls `/docs` | `sync --upload-only` :278 | `assert.Contains` :282 | |
| sync_full_test.go:415 | `..._DeletePropagation` (:389) | `"keep.txt"`, ls | syncs :408, :412 | `assert.Contains` :416-420, deletes | |
| sync_full_test.go:550 | `..._DownloadOnlyDefersLocalOnlyChanges` (:535) | `"remote-owned.txt"`, ls | `sync --upload-only` :549 | `putRemoteFile` :553 | |
| sync_full_test.go:593 | same | `"local-new.txt"`, ls | `sync --upload-only` :591 | `assert.Equal(..., getRemoteFile(".../local-deferred.txt"))` :594 | Opt.: `SS(t, cfgPath, env, "/"+testFolder+"/local-deferred.txt", int64(len("local deferred modification")), pollTimeout)` |
| sync_full_test.go:616 | `..._UploadOnlyDefersRemoteOnlyChanges` (:602) | `"remote-deferred.txt"`, ls | sync :615 | local write :619, `putRemoteFile` :620-621 | |
| sync_full_test.go:647 | same | `"local-owned.txt"`, ls | completed sync | `getRemoteFile` asserts :648-649 | Vacuous: MODIFIED file that has existed since :615. Opt.: `SS(..., "/"+testFolder+"/local-owned.txt", int64(len("local modification")), pollTimeout)` (lengths differ) |
| sync_full_test.go:700 | `..._PreserveEditEditConflict` (:685) | `"shared.txt"`, ls | sync :699 | local write :702, `putRemoteFile` :703 | |
| sync_full_test.go:753 | `..._PreserveEditDeleteConflict` (:737) | `"fragile.txt"`, ls | syncs :751-752 | local write :755, `rm` :756 | |
| sync_full_test.go:811 | same | `"fragile.txt"`, ls | after content poll :795 | `assert.Equal(preciousLocalContent, getRemoteFile(...))` :812 | Redundant |
| sync_full_test.go:888 | `..._NestedFolderHierarchy` (:869) | `"deep.txt"`, ls `/a/b/c` | sync :885 | `mkdirRemoteFolder` :892 | |
| sync_full_test.go:964 | `..._DryRunNonDestructive` (:937) | `"existing.txt"`, ls | uploaded by sync :949; `sync --dry-run` :957 | `assert.NotContains` :965 | |
| sync_full_test.go:978 | same | `"pending-upload.txt"`, ls | sync :975 | `assert.Contains` :980 | |
| sync_hardening_full_e2e_test.go:127 | `TestE2E_Sync_FolderMovesBothWays` (:119) | `"b.txt"`, ls `/proj/sub` | `runSyncThroughTransientFailures(--upload-only)` :126 | next step | |
| sync_hardening_full_e2e_test.go:134 | same | `"b.txt"`, ls `/proj-local/sub` | sync :133 | | |
| sync_hardening_full_e2e_test.go:166 | `..._CrossMoves` (:158) | `"inner.txt"`, ls `/box` | sync :165 | | |
| sync_hardening_full_e2e_test.go:175 | same | `"top.txt"`, ls `/box` | `syncUntil` :172 | | |
| sync_hardening_full_e2e_test.go:216 | `..._NamingCorpusRoundtrip` (:201) | `"children"`, ls `/corpus` | `runCLIBestEffort(sync --upload-only)` :215 | | |
| sync_hardening_full_e2e_test.go:355 | `..._TypeChangesBothWays` (:347) | `"child.txt"`, ls `/was-folder` | sync :354 | | |
| sync_hardening_full_e2e_test.go:362 | same | `"inside.txt"`, ls `/was-file` | sync :361 | | |
| sync_demoted_full_e2e_test.go:69 | `TestE2E_Sync_Conflicts` (:55) | `waitForRemoteFixtureSeedVisible(t, cfgPath, env, drive, "/"+testFolder+"/conflict.txt")` | `sync --upload-only` :68 | local write :71, `putRemoteFile` :72, sync :73 | |
| sync_safety_full_e2e_test.go:39 | helper `seedSafetySyncRoot` (:26), loop; callers :76, :129, :180 | `name`, ls | one-shot `sync` :37 | | |
| cli_commands_e2e_test.go:427 | `TestE2E_Mv_Rename` (:415) | `"original.txt"`, ls | `putRemoteFile` :426 | `mv` :430 | |
| cli_commands_e2e_test.go:453 | `TestE2E_Mv_MoveToFolder` (:441) | `"moveme.txt"`, ls | `putRemoteFile` :452 | `mv` :456 | |
| cli_commands_e2e_test.go:488 | helper `assertMoveOrCopyJSON` (:478); callers :473, :576 | `srcName`, ls | `putRemoteFile` :487 | mv/cp | |
| cli_commands_e2e_test.go:529 | `TestE2E_Cp_File` (:518) | `"source.txt"`, ls | `putRemoteFile` :528 | `cp` :532 | |
| cli_commands_e2e_test.go:536 | same | `"copy.txt"`, `remoteWritePropagationTimeout`, ls | one-shot `cp` :532 ("Copied" asserted :533); `cp` polls the copy monitor to completion before exit (`cli/cp.go:151`, :190-214) | `assert.Contains` :537-538, `getRemoteFile` :541, `assert.Equal("cp file content")` :542 | Opt.: `SS(t, cfgPath, nil, "/"+testFolder+"/copy.txt", int64(len("cp file content")), remoteWritePropagationTimeout)` |
| cli_commands_e2e_test.go:557 | `TestE2E_Cp_IntoFolder` (:547) | `"dest"` (folder), ls | `mkdir` :556 | | Folder wait |
| cli_commands_e2e_test.go:559 | same | `"src.txt"`, ls | `putRemoteFile` :558 | `cp` :562 | |
| cli_commands_e2e_test.go:566 | same | `"src.txt"`, ls `/dest` | one-shot `cp` :562 | `assert.Contains` :567 | |
| cli_commands_e2e_test.go:646 | helper `runForceOverwrite` (:636); callers :603, :622 | `"src.txt"`, ls | `putRemoteFile` :644 | | |
| cli_commands_e2e_test.go:647 | same | `"dst.txt"`, ls | `putRemoteFile` :645 | `--force` command :654 | |
| cli_commands_e2e_test.go:680 | `TestE2E_Mv_Folder` (:667) | `"source-dir"` (folder), ls | one-shot | | Folder wait |
| edge_cases_e2e_test.go:44 | `TestE2E_ZeroByteFileSync` (:25) | `"empty.txt"`, ls | `runSyncThroughTransientFailures(--upload-only)` :41 | `get` :48, `os.Stat` :50, `assert.Equal(int64(0), info.Size())` :52 | Intentionally 0 bytes |
| edge_cases_e2e_test.go:78 | `TestE2E_UnicodeFilenameRoundtrip` (:57) | `"caf"`, ls | sync :75 | `get` :82, `mustReadFile` :84, `assert.Equal` :85 | Opt.: SS with `int64(len(content))` |
| edge_cases_e2e_test.go:117 | `TestE2E_InvalidFilenameRejection` (:90) | `"valid.txt"`, ls | sync :110 | `assert.NotContains` :118 | |
| edge_cases_e2e_test.go:153 | `TestE2E_RapidFileChurn` (:125) | `"churn.txt"`, ls | sync :150 | `getRemoteFile` :154, `assert.Equal` :155 | Opt.: `SS(t, opsCfgPath, nil, "/"+testFolder+"/churn.txt", int64(len(finalContent)), pollTimeout)` |
| sync_recovery_e2e_test.go:124 | `TestE2E_Sync_CrashRecoveryIdempotent` (:96) | `"crash-5.txt"`, ls | `sync --upload-only` :121 | `rm` :127-128 | |
| sync_recovery_e2e_test.go:169 | `..._ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta` (:147) | `"remote-delete.txt"`, ls | sync :168 | sync :173, `putRemoteFile` :175, `rm` :176 | |
| sync_recovery_e2e_test.go:269 | `..._DriveRemovePurgeResetsState` (:249) | `"purge-a.txt"`, ls | sync :266 | | |
| sync_edge_cases_full_test.go:37 | `TestE2E_Sync_EmptyDirectory` (:23) | `"emptyFolder"` (folder), ls | sync :34 | | Folder wait |
| sync_edge_cases_full_test.go:109 | `..._NestedDeletion` (:76) | `"deep.txt"`, ls `/a/b/c` | converge loop :91 | | |
| sync_edge_cases_full_test.go:249 | `..._TransferWorkersConfig` (:227) | converge loop; ready checks 5 names in `ls` :260-265 | each attempt is a completed one-shot sync | | |
| fileops_full_e2e_test.go:55 | `TestE2E_RoundTrip` (:19) | `waitForRemoteParentListingContains(t, cfgPath, nil, drive, "/"+testFolder, "test.txt")` | one-shot `put` :50 | `assert.Contains "subfolder"` :56 | |
| fileops_full_e2e_test.go:60 | same | `waitForRemoteExactStatVisible(t, cfgPath, nil, drive, "/"+testFile)` | `put` :50 | size assertion `assert.Contains(stdout, fmt.Sprintf("%d bytes", len(testContent)))` :61 | Opt.: a size wait (needs driveID, see helper gap) |
| fileops_full_e2e_test.go:76 | same | `waitForRemoteExactStatVisible(... "/"+testFile)` | earlier `put` | `rm` :77 | |
| fileops_full_e2e_test.go:83 | same | `waitForRemoteExactStatVisible(... "/"+testSubfolder)` | folder | `rm -r` :84 | Folder wait |
| fileops_full_e2e_test.go:96 | same | `waitForRemoteExactStatVisible(... "/"+permFile)` | `put` :93 | `rm --permanent` :98 | |
| fileops_smoke_e2e_test.go:44 | `TestE2E_FileOpsSmokeCRUD` (:17) | `waitForRemoteFixtureSeedVisible(t, cfgPath, nil, drive, remotePath)` | one-shot `put` :42 | `get` :47, `mustReadFile` :50, `assert.Equal` :51 | |
| drive_list_full_test.go:130 | `TestE2E_DriveList_StaleStateDB` (:109) | `"stale-test.txt"`, stat | `sync --upload-only` :126 | `drive remove` :133 | |

Side observation (not a name wait): `cli_commands_e2e_test.go:606-607` (mv --force) and :625-626 (cp --force) assert `getRemoteFile(".../dst.txt")` with no wait after the completed `--force` command at :654. `dst.txt` pre-exists with old content, so only a content wait would guard against read-after-write lag there.

## A3. Counts

| File | RACY-DAEMON | SAFE | UNCLEAR |
|---|---|---|---|
| sync_watch_e2e_test.go | 4 | 0 | 0 |
| sync_watch_full_test.go | 8 | 3 | 0 |
| orchestrator_e2e_test.go | 4 | 3 | 0 |
| socketio_e2e_test.go | 1 | 0 | 0 |
| sync_shortcut_full_e2e_test.go | 1 | 3 | 0 |
| sync_e2e_test.go | 0 | 1 | 0 |
| sync_full_test.go | 0 | 15 | 0 |
| sync_hardening_full_e2e_test.go | 0 | 7 | 0 |
| sync_demoted_full_e2e_test.go | 0 | 1 | 0 |
| sync_safety_full_e2e_test.go | 0 | 1 | 0 |
| cli_commands_e2e_test.go | 0 | 11 | 0 |
| edge_cases_e2e_test.go | 0 | 4 | 0 |
| sync_recovery_e2e_test.go | 0 | 3 | 0 |
| sync_edge_cases_full_test.go | 0 | 3 | 0 |
| fileops_full_e2e_test.go | 0 | 5 | 0 |
| fileops_smoke_e2e_test.go | 0 | 1 | 0 |
| drive_list_full_test.go | 0 | 1 | 0 |
| **Total (80)** | **18** | **62** | **0** |

RACY breakdown: 3 stop + content assertion (rows 1-3); 4 mutation or follow-up that assumes the upload finished (rows 6, 7, 8, 11); 11 latent, stop with no content assertion (rows 4, 5, 9, 10, 12-18). Only row 17 is in the fast (PR) tier; the rest are `e2e_full`.

## A4. Cross-check grep hits that are not name-only waits

| Kind | Sites |
|---|---|
| Size waits (`waitForRemoteStatSize`) | sync_watch_full_test.go:420 (live daemon, already correct); sync_full_test.go:87; sync_hardening_full_e2e_test.go:324; sync_kill_mid_transfer_full_e2e_test.go:93, :123 |
| Disappearance waits | sync_watch_full_test.go:316; sync_full_test.go:351, :442, :443, :444, :471, :757, :806, :979; sync_hardening:366; sync_edge_cases_full:48, :118; fileops_full:79, :86, :100; fileops_smoke:55; sync_safety:110 |
| Converge / content loops (one-shot sync per attempt, or content compare) | sync_e2e_test.go:684; sync_full_test.go:353, :452, :559, :625, :652, :759; sync_recovery:66, :180, :207; sync_edge_cases_full:56, :91, :125; sync_hardening `syncUntil` calls; sync_watch_full_test.go:199, :260-273, :463-476; sync_shared_e2e_test.go:78, :110; sync_shortcut_full:300, :325; shared_items_helpers_test.go:1442; sync_safety:136 |
| `pollCLIWithConfigRetryingTransientGraphFailures` (Graph 502/503/504 retry, not visibility) | fileops_full:104, :113, :127; drive_list_e2e_test.go:317, :376; shared_items_e2e_test.go:124; w7_live_caller_proof_e2e_test.go:122, :184 |
| Definitions and helper-internal calls | e2e_test.go:459, :462, :482, :491, :539; remote_read_helpers_test.go:54, :196, :224, :256, :283, :351, :414, :448, :452; sync_watch_e2e_test.go:382, :384, :399; sync_e2e_test.go:403, :467 (:577 is a string literal); sync_full_test.go:510, :520; sync_hardening:110 |

## B1. How a sync download writes (product code)

All in `~/src/onedrive-go/internal/driveops/transfer_download.go` unless stated.

| Step | Citation |
|---|---|
| Entry from the executor | `sync/executor_transfer.go:27-28` ("ExecuteDownload downloads a remote file via TransferManager with .partial safety, hash verification with retry, and atomic rename."); `DownloadToFile` call :94 |
| Order inside `downloadToFile` | :256-318: disk space :280 → `PrepareParent` :284 → `DownloadPartialPath` :288 → `downloadToVerifiedPartial` :289 → `publishPartial` :303 |
| Partial name | :36-40 `downloadPartialPrefix = ".onedrive-go."`, `downloadPartialSuffix = ".partial"`; version tag `"~v"` + 12 hex of sha256(remote hash or eTag) :537-561; `downloadPartialName` :573-588. The e2e glob is `.onedrive-go.*.partial` (`sync_kill_mid_transfer_full_e2e_test.go:40`) |
| Same directory as the target | :532-534 `filepath.Join(filepath.Dir(targetPath), downloadPartialName(...))` |
| Bytes go to the partial only | `freshDownload` :652-699; opened `O_CREATE\|O_TRUNC\|O_WRONLY`, 0o600 at :655; `io.MultiWriter(f, h)` :660-663 |
| Hash verified BEFORE publish | `downloadToVerifiedPartial` :410-505 (`classifyDownload` :180-193, `ErrDownloadIntegrity` :504); `publishPartial` is reached only afterwards (:303) |
| mtime set on the PARTIAL, before fsync | :324-325 "Set mtime on the partial file before its fsync, so the flush makes the time durable together with the bytes."; :328 `target.Chtimes(partialPath, mtime, mtime)` |
| chmod + fsync + close on the partial | `finalizePartial` called :336; `f.Chmod(mode)` :389, `tm.syncFile(f)` :393, `f.Close()` :397 |
| Last precondition | :340 `validateDownloadTargetBeforeRename(opts)` |
| Atomic rename | :344 "Atomic rename: .partial -> target."; :347 `target.Rename(partialPath, targetPath)` |
| Directory fsync after the rename | :355 `target.SyncDir(parentDir)`; tolerated where unsupported :356-367 |
| Nothing written after the rename | `sync/executor_transfer.go`: `crashPointDownloadRenamed` :105; `downloadOutcome` :129-168 only `Stat` :161 and `observeLocalIdentity` :165. A grep of non-test `internal/sync` for `xattr`, `Xattr`, `SetMtime` has no match |
| Plumbing | `sync/executor_transfer.go:288-341` (`Rename` :327-329, `Chtimes` :331-333, `SyncDir` :335-337); `synctree/synctree.go` `Rename` :993 (`root.Rename` :104-106), `Chtimes` :1090, `SyncDir` :1098 with `dir.Sync()` :1104. No partial is created in `synctree` |

**Consequence:** a download in progress never exposes a partial file at the final path. When the final path appears (new file) or changes (modified file) it already carries full, hash-verified content, final mode and final mtime. For a MODIFIED file the old content stays at the final path until the rename, so existence proves nothing there.

## B2. Local waits

| file:line | Helper | Waits for | Producer | What follows | Verdict |
|---|---|---|---|---|---|
| sync_watch_full_test.go:106 (`..._RemoteToLocal` :88) | `pollLocalFileContent(t, localPath, "from remote", daemonPollTimeout)` (def sync_e2e_test.go:722-748; `string(data) == expected` :729) | content | live download-only daemon :97 | SIGTERM :109, `mustReadFile` :113, `assert.Equal` :114 | Safe |
| sync_watch_full_test.go:148 | `pollLocalFileContent` | content | live daemon :128 | SIGTERM :151 | Safe |
| sync_watch_full_test.go:199 | `require.Eventually` (`os.ReadFile` :205, :210; `getRemoteFile` :215) | content of conflict copy, local and remote | live daemon :171 | stop | Safe |
| socketio_full_e2e_test.go:38 | `pollLocalFileContent(... "baseline before websocket watch", daemonPollTimeout)` | content | live daemon :33 | | Safe |
| socketio_full_e2e_test.go:49, :73 | `waitForSocketIOWakeAndLocalFileAfter` (socketio_helpers_test.go:182-232; `os.ReadFile` :208, compare :209) | content | live daemons :33, :63 | SIGTERM :59 / :83 | Safe |
| sync_shortcut_full_e2e_test.go:300 | `require.Eventually` (`os.ReadFile` :301, compare :302) | content | live download-only daemon :294; one-shot `put` :297 | end | Safe |
| sync_e2e_test.go:684 | converge loop (`os.ReadFile` :695, `bytes.Equal` :701) | content | one-shot sync per attempt | | Safe |
| sync_full_test.go:353, :559, :625, :652, :759 | converge loops (reads :364-365; :570-571 and :575-576; :636-637, `os.Stat` :641 absence; :663-664, :668-669; :771, :776) | content, including MODIFIED files (:571 `!= "remote modification"`) | one-shot sync per attempt | | Safe |
| sync_recovery_e2e_test.go:66, :207 | converge loops (`os.ReadFile` :77, `== "new from remote"` :83; `os.ReadFile` :218, `!= "updated from remote"` :219, `os.Stat` :223 absence) | content (new and MODIFIED) | one-shot sync per attempt | | Safe |
| sync_hardening_full_e2e_test.go:172-173, :268-269, :277-278, :285-286, :289-290, :368-369; :336-338 | `syncUntil` with `contentTree` (:66-95, `os.ReadFile` :86); :337-338 `os.ReadFile` + `bytes.Equal` on a MODIFIED large file | content | one-shot sync per attempt | | Safe |
| sync_shared_e2e_test.go:78, :110 | `require.Eventually`; one-shot `sync --download-only` :79 / :111 each attempt; `os.ReadFile` :84 / :112; `err == nil && string(data) == expectedContent` :85 | content | one-shot sync per attempt | | Safe |
| sync_shortcut_full_e2e_test.go:325 (`syncShortcutDownloadUntilProjected` :306-346) | `os.Stat(sentinelPath)`, `err == nil` | existence only | completed one-shot sync :320 | `require.FileExists` :340; no content read | Safe (process exited; atomic rename) |
| sync_hardening_full_e2e_test.go:141-143 | `syncUntil` with `os.Stat(.../proj-online/sub/b.txt)` | existence only (local folder MOVE, not a download) | completed one-shot sync | `assert.NoDirExists` :145, `contentTree` equality :146-148 | Safe |
| shared_items_helpers_test.go:473 (`shortcutSentinelReachable` :454-478) | `os.Stat(sentinelPath)` | existence only | completed one-shot `get` :467 | | Safe |
| sync_kill_mid_transfer_full_e2e_test.go:129 | `waitForDownloadPartial` (:231-246) | the PARTIAL with size > 0, not the final path | live killable sync :128 | kill :130; `os.IsNotExist` on the final path :133-134; resumed sync :140; `mustReadFile` :143 | Safe; :133-134 relies on the atomic publish |
| Absence waits/asserts | sync_full_test.go:463, :464, :474, :641, :968; sync_edge_cases_full:67, :136, :143, :146, :149; sync_safety:136-139; sync_recovery:202, :223, :236 | absence | | | Not applicable |
| Plain reads with no wait, after a completed one-shot `sync`/`get` | sync_watch_full_test.go:113, :427; sync_full_test.go:96, :135, :174, :302, :306, :478, :482, :715, :727, :730, :800, :846, :858, :861, :910, :982, :1031, :1034; sync_demoted:76; sync_hardening:236, :329; sync_recovery:199, :233, :291, :294; sync_safety:141, :158-159; sync_kill_mid:143, :261; fileops_full:71; fileops_smoke:50; edge_cases:50, :84; orchestrator:124, :129; sync_shortcut_full:33-34, :94, :111-113, :133, :154-156; sync_shortcut_e2e_test.go:28-29, :42 (grep hit only, file not read); shared_items_helpers:1418, :1452; remote_read_helpers:468 | none | process exited | | Safe |

## B3. Conclusion (local side)

- Every polling wait on a file downloaded by a live daemon compares CONTENT; none waits for existence and then reads.
- Every MODIFIED-file case already uses a content compare, so "existence proves nothing" does not bite anywhere.
- The three existence-only waits follow a completed one-shot command and read no in-flight download.
- No e2e test asserts the mtime of a downloaded file right after an existence wait; and mtime, mode and fsync are applied to the partial before the rename, with nothing written afterwards.
- **No RACY local wait found.** The exposure is on the upload side only.
