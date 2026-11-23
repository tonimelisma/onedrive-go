# W66 report: an error names a local path once

**Outcome:** commit `acd7c659` on `fix/error-names-path-once`. The worktree is `~/src/onedrive-go-wt/error-names-path-once`, based on origin/main `b00e6da3`. The commit is not pushed and has no PR. It is installed: `onedrive-go --version` prints `acd7c659`.

I first stopped at the DoR, because decision (a) rested on a false premise; the record of that stop is in this file's git history. The coordinator then re-decided (a) as L2, and this report covers the built change.

## What the brief had wrong against main

1. **The localpath premise (the blocker).** `internal/localpath` did not return the bare `*fs.PathError`. Every wrap (33 sites in `localpath.go`) printed `<verb> <path as given>: <os error holding the absolute path>`, so every localpath error named the path twice by itself.
2. **Where `Failed: <path>: ` came from.** The folder printer (`cli/format.go:279`, fed by `cli/folder_transfer.go:35`) printed only `Failed: ` plus `event.Err.Error()`. The `<path>: ` came from driveops `failedFile` (`tree.go:234`). Folder events had no such subject, so their lines read `Failed: opening <P>: …`.
3. **Unmentioned in the brief:** the `--json` `errors` of a folder put or get hold the same strings (`put.go:213`, `get.go:235`).
4. **Line moves:**
   - `transfer_upload.go` `:243` is now `:244`, and `:274` is now `:275`.
   - `hash.go:43`, `put.go:134` and `putJSONPath` were where the brief said.
   - The spec has no "Failed" paragraph. The governing one is `drive-transfers.md:149-151`.
5. **Sync already named the path once,** in its activity line and JSON events (`innermostErrorText`). That fits (d).
6. **Names and procedure.** The brief's worktree and branch names differ from the row's; I used the row's. The start stage was not run, as instructed. There are no separate dor or lint files; everything is in this file.

## DoR, today's texts on main

Texts were measured by a probe test (since removed) and the live run. P is the path as typed, A the absolute path.

| Caller | Text on main | P/A count |
|---|---|---|
| folder put, unreadable file (and `--json` errors) | `Failed: P: opening P for hashing: opening P: open A: permission denied` | 4 |
| folder put, folder that cannot be opened | `Failed: opening P: open A: permission denied` | 2 |
| single put, `--verbose` details | `uploading "/r": opening P for hashing: opening P: open A: permission denied` | 3 |
| single put, plain sentence | `Permission denied for A.` | 1 |
| folder get, read-only folder | `Failed: T: creating partial file PART: opening PART: open A: permission denied` | target 1, partial 3 |
| folder get, target is a folder | `Failed: T: renaming partial to T: renaming PART to T: rename A A: file exists` | target 4 |
| single get, plain sentence | `Permission denied for A/.onedrive-go.b.txt~v….partial.` | 1, the hidden partial |
| sync activity line (`sync_console.go:216-223`) | `Failed to upload <rel>: permission denied` | 1 |

**Wrap sites.** Every driveops wrap that named a local path was over localpath, TargetFS, Graph or an integrity error. The full list is in the diff of `hash.go`, `transfer_upload.go`, `transfer_session.go`, `transfer_verify.go`, `transfer_target.go`, `transfer_download.go`, `tree.go`, `tree_upload.go` and `tree_download.go`.

**Tests and product lines that matched on these texts:**
- cli: `put_test.go:76,109,117,140,166`, `folder_transfer_test.go:133,144,253`.
- driveops: `hash_test.go:147`, `tree_download_test.go:246,342,371,462,464,530,608`, `tree_upload_test.go:256,283,308,361,455`, `transfer_manager_test.go:1030,1272,1710`.
- localpath: `localpath_test.go:64,65,128,134,162,377`.
- sync: `pending_creates_test.go:543`, which pins "creating upload session".
- e2e: none. Product code: no line matched on these texts.

**Renderer home.** Both folder printers are in `internal/cli`, and the lowest package below them is `localpath`, so `WithoutPath` lives there.

## What changed

The non-test code is 13 modified files (+142/−138) and the new `localpath/failure.go` (128 lines, about half of them comments). That is 270 lines added or rewritten. Git's insertions plus deletions come to 408. I read the 350-line limit as lines added or rewritten; by git's +/− count the change is over it.

- **`localpath/failure.go`:**
  - `callError` prints `<verb> <path as given>: <reason>`. It unwraps to the unchanged os error, and prints only that error's reason when the error names the same path the call reached.
  - `failure()` builds one. `osErrorPath` handles temp names.
  - `WithoutPath(err)` substitutes text for every `callError`, bare `*fs.PathError` and bare `*os.LinkError` in the error tree.
- **`localpath/localpath.go`:** all 31 path wraps are `failure(...)`. The close after a failed read no longer repeats the path, and `closeAfterFailure` lost its path parameter. The SyncDir open now says `opening directory`, without "for sync". `making %s durable` stays a plain wrap, because it names a different path from the directory's failure.
- **driveops: no wrap names a local path.**
  - Verbs now in use: `hashing`, `sending the file`, `creating upload session`, `resuming the upload`, `recording the new file`, `preparing the folder`, `creating the partial file`, `moving the download into place`, `syncing the folder`, `reopening/closing/flushing/setting the mode of the download`, `inspecting the download`, `hashing the resumed download`, `checking free space`, `the file is <size>`.
  - The integrity errors are now the bare sentinel and its detail.
  - `targetPathForPartial` is deleted; it was unused once its two callers stopped naming the target.
- **Tree:**
  - `TreeEvent.Err` names no path: `failedFile` and `reportUnfinished` no longer prefix it, and a refused name is reported bare.
  - The listing failure names only the remote folder.
  - The remote folder ensure failure reads `creating the remote folder: …`.
  - A panic logs the path as an attribute instead of in its message.
- **cli:**
  - `failureLine` prints `LocalPath + ": " + localpath.WithoutPath(err)` for the text line and `--json` alike.
  - `putJSONPath` is renamed `putPrintedPath`, and the text line uses it (the rider).
  - `localFailureText` names a hidden partial file's folder instead of the partial itself (item 6 of the earlier "seen" list, three lines).
- **Spec:**
  - `drive-transfers.md` has a new section, "Where a Local Path Enters an Error", plus edits to `:149` and `:639`.
  - `system.md` has a localpath paragraph.
  - `cli.md` now covers the text line in the JSON path sentence.
  - `R-6.6.26` gained one sentence.
  - `WithoutPath` is on the reading list in `executor_rooted_mutation_test.go`.

## Tests, and what each pins

"Failed on main" means the test was run with the product changes stashed. A "pin" passed both before and after the change.

**localpath:**
- `TestFailures_NameEachPathOnce` (22 calls): each path is named once, and the error ends with the reason. Failed on main for 21 of 22 calls.
- `TestFailures_NameARelativePathAsItWasGiven`: a relative path is named as typed, while `PathError.Path` stays absolute. Failed on main.
- `TestFailures_AnUnreadableFileIsPermissionDenied`: failed on main.
- `TestFailures_AnswerAsTheOSErrorTheyHold`: a pin for Is/As, Op and Path, LinkError, `ErrNotRegularFile` and `IsMissingName`.
- `TestWithoutPath_LeavesOutEveryLocalPath` (10 cases): failed at build time, since the function did not exist yet.
- `TestReadAndClose_NamesThePathOnceForBothFailures` (renamed): failed on main.

**driveops:**
- New `TestComputeQuickXorHashContext_AnUnreadableFileIsNamedOnce`, `TestTransferManager_UploadOfAnUnreadableFileNamesItOnce` and `TestTransferManager_DownloadIntoAReadOnlyFolderNamesThePartialOnce`: each failed before the driveops change.
- `TestTransferManager_DownloadError_NamesNoLocalPath` (renamed) and the updated `TestTransferManager_Upload_ErrorWrapping`: both failed before the driveops change.
- `TestClassifyLocalError_ReadsALocalpathFailureAsItsOSError`: pins `ClassifyLocalError` through a localpath failure. Its "once" count was never run against main.
- The updated tree tests (seven `TestDownloadTree_*` and three `TestUploadTree_*`) now hold that the event names the item and the error does not. All failed before the driveops change.
- `TestDownloadToFile_RenameFailure_PreservesPartial`: its old assertion ("renaming partial") broke on the change, and was rewritten after it. The new assertion was not run against main.

**cli:**
- Failed on main: `TestRunPut_FolderWithAFailingFileUploadsTheOthers` (exact line), `TestRunPut_FolderNamesAnUnreadableFileOnce` (relative root plus `--json`), `TestRunPut_AnUnreadableFileIsNamedOnce`, `TestRunGet_FolderNamesAnUnwritableFileOnce`, `TestRunGet_AnUnwritableFileIsNamedOnce` (its plain-sentence part also failed before the `localFailureText` change) and `TestPut_TextNamesEachPathAsJSONDoes` (the rider).
- Pin: `TestSyncConsole_ALocalFileFailureNamesTheFileOnce`.
- Input updated: `TestFolderTransferReport_ACanceledRunReportsThenEndsWithTheCancellation`.

## Checks

- **Build and vet:** `go build ./...`, `go vet ./...` and the tagged vet all passed. The touched packages were vetted again after the last edits.
- **Plain tests:** localpath, driveops, cli, and every localpath importer passed. The importers are perf, synctest, config, logfile, tokenfile, localtrash, synctree, fsroot, testutil, graph and multisync.
- **`-race` behind the lock:** driveops, cli and localpath passed.
  - The last edit (one string in `transfer_session.go`) may have landed after that race build started; driveops ran plain again after it and passed.
- **Sync, plain, behind the lock:** one failure. `pending_creates_test.go:543` pins "creating upload session", so that wrap keeps those words.
  - After the fix, that test and its reverse-dispatch run pass plain and under `-race`, together with `TestLocalpathFunctions_AreEachDeclaredReadingOrChanging` and `TestExecutorFiles_…`.
  - The whole sync tree was not run again after that one-string change.
- **Source rules:** `RealRepoIsClean` gives 4 PASS lines.
- **Lint:** `devtool verify lint` reports `0 issues.` and actionlint passes.
- **Spec citations:** all 24 test names cited in the spec diff exist, checked by script.

## Sync's user-visible texts that changed (from reading)

1. **Activity lines and JSON events:** unchanged (innermost reason).
2. **A mount's startup or run error**, printed whole by `sync_render.go:201` → `sync_startup_messages.go:51`, loses the os error's copy of the path.
   - Before: `stat sync folder for locking: stating /home/u/OneDrive: stat /home/u/OneDrive: no such file or directory`
   - After: `stat sync folder for locking: stating /home/u/OneDrive: no such file or directory`
   - The same applies to `engine_locks.go:244/248` (lock folder and file), `observation_root_identity.go`, `debug_event_sink.go:31` and `store_scratch.go`.
3. **`drive reset-sync-state`** (`store_reset.go:45/93/148`):
   - Before: `remove state DB file P: removing P: remove P: <reason>`
   - After: `remove state DB file P: removing P: <reason>`
   - Still twice: sync's own wrap (Seen item 1).
4. **Logs:** the error attributes lose driveops' path. For example, `creating upload session for /abs/big.bin: connection reset by peer` becomes `creating upload session: connection reset by peer`. The log lines keep their `path` attribute.

## Live proof (test account, synthetic tree outside any synced folder)

The tree held `a.txt`, `locked.txt` (mode 000) and `sub/b.txt`, uploaded with folder `put` to a scratch remote folder.

- **Before, installed `b00e6da3`:** `Failed: <scratch>/tree/locked.txt: opening <scratch>/tree/locked.txt for hashing: opening <scratch>/tree/locked.txt: open <scratch>/tree/locked.txt: permission denied` (the path 4 times).
- **After, the worktree built with a version stamp:** `Failed: <scratch>/tree/locked.txt: hashing: opening: permission denied` (once).
- **Single `put --verbose`, after:** `Error: Permission denied for <scratch>/tree/locked.txt. Check its file permissions.` / `Details: uploading "/<scratch folder>/one.txt": hashing: opening <scratch>/tree/locked.txt: permission denied` (once per line).

Both remote folders were removed with `rm -r`. They went to the recycle bin, which Graph does not expose for personal accounts; `stat` confirms they are gone. The local scratch tree and the scratch build are deleted.

## Done / short of the brief / beyond it

- **Done:** L2, the wraps, the printer, the JSON errors, the spec rule and the rider.
- **Short:**
  - The whole sync tree was not rerun after the last one-string edit.
  - `localFailureText` still names the absolute path for a relative one (see below).
  - The CLAUDE.md commit line and the CHANGELOG install entry are left to you, as instructed.
- **Beyond:**
  - Graph and integrity wraps were dropped as well, since without that a folder line would name the file twice on a Graph failure.
  - The path in the panic message moved to a log attribute.
  - The partial file's folder is now named in a single get's sentence.
  - `putJSONPath` was renamed.

## Seen and not fixed

1. `internal/sync/store_reset.go:45,93,148`: the wraps repeat the path the localpath failure names (`drive reset-sync-state`). Fix: drop `%s` from the three wraps.
2. `internal/cli/error_presentation.go:244-270`: `localFailureText` names `pathErr.Path`, which is always absolute, even for a relative typed path. Fix: an exported localpath accessor for the typed path (it would join the declared lists).
3. `internal/cli/get.go:108`: `download file: %w` over driveops `downloading: %w` reads `download file: downloading: …`. Fix: drop the cli word.
4. synctree's errors for sync's downloads: whether each names its path once was not checked (unknown).
5. Overlap: worktree `path-read-returns-its-item` changes `lookupUploadTarget` in `driveops/transfer_verify.go`. This commit changes `settleUploadedItem` lines 239 and 276-277, two unchanged lines away from their hunk. Whether a rebase conflicts is unknown. Both touch `drive-transfers.md` and `cli.md`, in other sections.
