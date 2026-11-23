# Report: bounded recursive get and put

Worktree `~/src/onedrive-go-wt/tree-transfers`, branch `refactor/tree-transfers`, base `51e554fa`. Commit: `b4b73401` (not pushed).

## What changed
- `internal/driveops/tree.go` (new): `TreeWorkers` (4), `TreeEvent`/`TreeEventKind`/`TreeSummary`, `TreeRootError`, the pool: the caller's goroutine walks, 4 workers on a channel of 4, one collector; every goroutine behind a `panicguard` boundary and waited for; a walk panic also fails the run instead of stranding the workers.
- `internal/driveops/tree_download.go` (new): `DownloadTree(ctx, TreeDownload)`, `ChildLister`, `FileDownloader`, `RefusedNameError`, `RefuseRemoteName`, `createDownloadFolder` (moved from cli).
- `internal/driveops/tree_upload.go` (new): `UploadTree(ctx, TreeUpload)`, `FolderEnsurer`, batch reads of `uploadDirBatch` (256).
- `internal/localpath/localpath.go`: `OpenDir`, `DirReader.Next`, `DirReader.Close`.
- `internal/cli/folder_transfer.go` (new): `folderTransferReport` (running count, failures, summary; the command keeps and prints its `--json` document).
- `internal/cli/get.go`, `get_shared.go`, `put.go`: folder flavors call the engines; `newDownloadManager`; `localFolderTarget` refuses an unsafe remote folder name used as the local one. Deleted (no wrapper left): `downloadState`, `countRemoteFiles`, `downloadTree`, `downloadTreeFile`, `finishFolderDownload`, `joinRemotePath`, `defaultDownloadConcurrency`, `countSharedFiles`, `downloadSharedFolder`, `uploadWalkState`, `countUploadFiles`, `walkUploadTree`, `walkUploadTreeEntry`, `appendUploadWalkError`, `isFatalUploadWalkError`, `uploadWalkEntry`, `uploadFileEntry`.
- Specs: `drive-transfers.md` (GOVERNS, Implements, folder transfers section, "Recursive Transfer Concurrency", Transfer Interfaces), `cli.md` (progress, cancellation, two Verified By rows), `system.md` (`localpath.OpenDir`), `file-operations.md` (new R-1.2.7), `developer-onboarding.md`.

## Tests added
- `TestDownloadTree_InFlightNeverExceedsWorkers` (10,000 files generated in memory, gate, peak = 4), `..._WalkBlocksWhileWorkersAreBusy` (no folder past the one holding file 9 is listed), `..._GoroutinesDoNotScaleWithFilesOrFolders` (1,000 folders × 10 files: 4 workers + 1 collector by stack dump, none after return).
- `TestDownloadTree_CancellationReportsEveryUnstartedFile`, `TestUploadTree_CancellationReportsEveryUnstartedFile`: every started or queued file fails with the context's error, every cut-short folder is reported, nothing listed is left out.
- `TestDownloadTree_RefusesUnsafeRemoteNames` (`..`, `.`, empty, `a/b`, `a\b`, a `..` folder: typed reason, nothing written, not listed, siblings done), `..._ListFailureIsReportedAndSiblingsContinue`, `..._TransferFailureIsReportedAndOthersContinue`, `..._UnlistableRootIsTheReturnedError`, `..._FolderExistsBeforeItsFilesDownload`, `..._APanickingTransferFailsOnlyThatFile`, `..._APanickingReportFailsTheRun`, `..._APanickingWalkFailsTheRun`; moved onto the engine: `..._CreatedFolderFollowsTheUmask`, `..._CountsOnlyTheFoldersItCreates`, `..._FileWhereAFolderBelongsIsReported`.
- `TestUploadTree_CreatesParentBeforeChildren`, `..._InFlightNeverExceedsWorkers`, `..._UploadsAFolderLargerThanOneBatch`, `..._ReadDirFailureReported`, `..._FolderThatCannotBeCreatedIsReported`, `..._UnreadableRootIsTheReturnedError`; `TestOpenDir_ReadsInBatches`, `TestOpenDir_NamesThePathInItsFailures`.
- Command level, fake Graph drive: `TestRunGet_FolderReportsEveryFileAsTextAndJSON`, `TestRunPut_FolderReportsEveryFileAsTextAndJSON`, `TestRunGet_FolderRefusesAnUnsafeRemoteName`, `TestRunGet_SharedFolderWithAnUnsafeNameIsRefused`, `TestRunGet_SharedFileDownloadsIntoTheGivenFolder` (single shared file, real downloader); `TestFolderTransferReport_ACanceledRunReportsThenEndsWithTheCancellation`.
- Deleted, replaced by the above: `TestJoinRemotePath`, `TestCountRemoteFiles_*` (2), the five cli `TestDownloadTree_*`, `TestCountUploadFiles_NestedTree`, `TestWalkUploadTree*` (2), `TestAppendUploadWalkError_*`, `TestIsFatalUploadWalkError`.

## User-visible text, before → after
- Progress (terminal only): `Downloaded N/M files` → `Downloaded N files`; `Uploaded N/M files` → `Uploaded N files`; `put -r`'s `Uploading <name>: <x> / <y>` status lines (printed even off a terminal) are gone.
- Unchanged: `Downloaded N files, M folders (size)`, `Uploaded N files, M folders (size)`, `Failed: <line>`, `N items failed to download|upload`, file lines `<local>: <err>`.
- `get -r` subfolder listing failure: `listing "<remote>": list children for "<remote>": <err>` (shared: `listing shared folder contents: <err>`) → `<local>: list children: <err>`. `put -r` unreadable folder: `<local>: reading directory <local>: <err>` → `opening <local>: <err>`.
- New lines: `<local folder>: refused the remote name "<name>": it <is empty|is . or ..|holds a path separator|is not a local name>`; `<local>: stopped before the folder was finished: context canceled`; `put -r` per queued file on cancel `<local>: context canceled`; `<local>: internal error while uploading <local>: …`. New command error: `refused the remote name "<name>": it …: name the local folder to download into`.
- Root errors: `listing "<remote>": list children for "<remote>": <err>` → `listing "<remote>": list children: <err>`; shared `listing shared folder contents: <err>` → `listing "<selector>": list children: <err>`; put `reading upload root <local>: reading directory <local>: <err>` → `uploading "<remote>": reading upload root: opening <local>: <err>`.
- Cancelled `get -r`: summary, `Failed:` lines, `N items failed to download`, exit 1 → summary, `Failed:` lines, the cancellation (Ctrl-C exit 130 silent; deadline exit 1 `folder download stopped: context deadline exceeded`). Cancelled `put -r`: `upload file "<local>": context canceled`, no summary, 130 → summary, `Failed:` lines, 130.
- `--json`: one document, same fields and order rule; entries in completion order; error strings as above.

## (g) Directory reads
`.golangci.yml:190` forbids `os.ReadDir`, `os.Open` and the rest outside `fsroot`/`synctree`/`localpath` (exception `:363`), so the batch reader is `localpath.OpenDir` + `DirReader.Next(n)`; `UploadTree` reads 256 entries at a time.

## Checks (final tree)
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/driveops/... ./internal/cli/... ./internal/localpath/...`: pass; the same with `-race` behind the lock: pass. A middle `-race` run failed once on `TestDownloadTree_GoroutinesDoNotScaleWithFilesOrFolders` (4 of 5: the collector not yet scheduled); the test now also waits for the first delivered event; tree tests `-race -count=10`: pass.
- `internal/panicguard` (goroutine boundary scan) and `internal/devtool` `RealRepoIsClean|Governs|ListsEvery|Registered` (test conventions, requirements, mutex docs): pass. By hand: every test and requirement the three design docs cite exists (0 missing).
- Lint (`H7-lint.log`): `0 issues.` (the first run's 8 findings fixed).

## Deviations (behaviour, scope or interface)
- (j) No downloader seam: a loopback Graph base URL already admits loopback pre-authenticated URLs (`internal/graph/client_construction.go:78-87`, `loopbackTolerant`), so the shared-file test runs the real downloader; product code and R-6.8.22's check unchanged.
- (h) The parse was already one helper (`configuredMinFreeSpace`); the four manager constructions around it became `newDownloadManager` (folder downloads no longer build an unused session store).
- Engines are functions taking `TreeDownload`/`TreeUpload` with narrow seams (`ChildLister`, `FileDownloader`, `FolderEnsurer`, a `PutFile` func), not `TransferManager` methods: put's overwrite rule (`putFile`, R-1.3.8) stays in cli and the 10,000-file test writes nothing. No `Workers` option.
- A drive-path folder `get` lists every folder by item ID, as the shared flavor did, not by path.
- A cancelled folder `get` ends with the cancellation (130 on Ctrl-C), not exit 1, like folder `put`.
- A remote folder that cannot be listed is no longer created locally (or counted); a local folder that cannot be opened is no longer created on OneDrive.
- The default local folder of a shared folder or a drive root (its remote name) is refused when unsafe; the single-file case is not touched (stop rule).
- The analysis cited R-6.7.1 (delta reordering) for the bound; the tests cite R-1.2.1/R-1.3.1. R-1.2.7 is new.
- Command-level cli tests were written after the cli change; the engine tests were seen failing (no compile) first.

## Also fixed
- `uploadFolder` took a `*cobra.Command` only for its context; folder put took size and mtime from the entry's lstat (wrong for a symlink): now the transfer's own stat.
- History comments in `shared_test.go`, `TestConfiguredMinFreeSpace`; serialization tests without `t.Parallel`/`Validates`; `drive-transfers.md` named 2 of 8 optional seams; `cli.md` cited a deleted test.

## Not fixed
1. `internal/cli/get.go:73`, `internal/cli/get_shared.go:31`: a single-file `get` with no local path writes to the remote name as given, so a sharer's `../x` lands outside the working directory. Fix: the same `localFolderTarget`/`RefuseRemoteName` check for the file name (left: single-file transfers were out of scope).
2. `internal/driveops/session.go:993`: `EnsureFolder` finds the existing folder after a 409 by exact name, so a folder OneDrive holds in another case fails "reported as existing but not found". Fix: `namekey.Equal`, as `putFile` does.
3. `internal/driveops/transfer_upload.go:361-364`, `:401-405`: `UploadFileToItem` repeats `uploadMtime` and `recordUploadPerf` inline. Fix: call the helpers.
4. `internal/cli/format.go:31-35`, `:307-309`, `:326-328`: `writeWarningf`, `Progressf` and `ProgressDone` drop write errors that `Statusf` records. Fix: record them through `recordStatusError`, outside `statusMu`.
5. `internal/graph/items_fetch.go:175`: `ListChildren` returns every page of a folder as one slice, so `DownloadTree` holds a remote folder of 100,000 children whole (one listing per depth level). Fix: a page-at-a-time lister the walk drains.
6. `internal/cli/put_shared.go:18`: `fmt.Errorf` without a verb. Fix: `errors.New`.
7. `internal/cli/json_contract_test.go:439-441`: the comment blames the fake's pre-authenticated URL for zero-byte uploads; loopback URLs are admitted, the fake just serves no upload session. Fix: correct the comment.
8. `internal/driveops/interfaces.go:91`: cites a backlog id `(B-085)`. Fix: cite the requirement or drop it.
9. `internal/cli/format.go:115`, `internal/driveops/transfer_manager.go:118`: two `formatSize` with the same units. Fix: one helper.
10. `internal/cli/put.go` (`remoteRoot + "/" + RelPath`): folder put's `--json` paths drop the leading slash (`Backup/a.txt`) while a single put prints the path as given (`/a.txt`). Fix: one form (kept: the `--json` contract was to stay).

## Unfinished / for the gate
Nothing unfinished. Live e2e of folder `get`/`put` is the gate's.

## Second pass

Rebased onto `3a80f900` without a conflict: git merged `localpath.go`, its test, `system.md` and `developer-onboarding.md` on its own; the diff against main adds only (main's functions, tests and sentences unchanged). `OpenDir` added to `localpathReadingFunctions` in `internal/sync/executor_rooted_mutation_test.go` (the test reads functions only, not methods, so `Next` is not listed).

### Items
1. `TestRunGet_DriveRootWithNoLocalPathTakesTheRootsName` (fake drive, `get /`, root reported as `..`, then as `Drive Root`). Holding test: the unchanged code already refused. The unsafe half holds `item.IsRoot ||` in `get.go:63` (taken out, the test fails: no error, the root's files land in the working directory); the safe half holds `return remoteName, nil` in `localFolderTarget`.
2. `localFileTarget(args, remoteName)` now returns an error and applies `RefuseRemoteName` wherever the remote name becomes the file's name: no local path, and also a local folder to download into (R-1.2.6), since `Join(dir, "")`, `Join(dir, ".")` write a file named after the folder and `Join(dir, "..")` targets its parent. A named local file is not checked. Shared: `TestRunGet_SharedFileWithAnUnsafeNameIsRefused` (failed before: "target path must not be empty"). Drive path: a server cannot get such a name through, because `graph.GetItemByPath` (`internal/graph/items_fetch.go:105-121`) refuses an item whose name is not the requested leaf; `TestRunGet_FileReportedUnderAnUnsafeNameIsNotWritten` holds that end to end, and `TestLocalFileTarget_RefusesAnUnsafeRemoteName` holds the helper. The first report's `../x` example is wrong: OneDrive refuses `/` in names, so a name holding a slash is not one a server sends; the names that matter are empty, `.` and `..`.
3. `TreeUpload.FolderID` became `ParentID` and `Name`: `UploadTree` opens the local root, then makes sure of the remote root, reported as the first `TreeFolderCreated` (RelPath ""), so `put.go` no longer calls `EnsureFolder` and `folderTransferReport.finish` lost its `extraFolders` parameter. Root ensure failure is a `TreeRootError` (`creating the remote folder: …`). `TestRunPut_FolderThatCannotBeOpenedCreatesNothing` (mode-000 folder; failed before: the remote folder existed), `TestUploadTree_UnopenableRootCreatesNothingOnOneDrive` (missing, regular file; replaces `…_UnreadableRootIsTheReturnedError`), `TestUploadTree_RootThatCannotBeCreatedIsTheReturnedError`. At command level a missing root or a file never reaches the folder path (`runPut` stats first), so the command test uses an unreadable folder.
4. Download: `assertFailedAreAPrefixOfTheListing` (5 to 9 files, exactly the first of folder-0000's listing). Upload: rewritten on the walk-blocks layout (20 folders, one file each, cancel at the 9th folder made sure of) with `assertEveryLocalFileAccounted`. With `work` dropping queued jobs after a cancel, both fail.
5. `TestUploadTree_TransferFailureIsReportedAndOthersContinue`; holds `return err` in the upload transfer (with `return nil` it fails).
6. `TestTreeWorkers_FourFilesTransferAtOnce`: literal 4, `require.Equal(4, TreeWorkers)`, then exactly 4 started and peak 4 in a 20-file tree with the walk at the full queue. R-1.2.1 and R-1.3.1 now state "four files at a time". With `TreeWorkers = 5` it fails.
7. `OpenDir` opens with `O_RDONLY|O_DIRECTORY|O_NONBLOCK` (`opendir_unix.go`, `//go:build unix`); `Next(n<1)` is refused. `localpath` also builds for Windows, which has no directory-only flag: `opendir_other.go` keeps the plain read-only open there (checked with `GOOS=windows go build`). New `TestOpenDir_RefusesAFIFOWithoutBlocking` (own 5 s deadline; blocked before), `TestOpenDir_NextRefusesACountBelowOne`; `TestOpenDir_NamesThePathInItsFailures` now expects ENOTDIR at the open. All three failed on the old code.
8. Before: `<local>: list children: <cause>`. After: `listing the OneDrive folder "<RemoteRoot>/<rel>" to fill <local>: <cause>`, with a new `TreeDownload.RemoteRoot` (the path or shared selector the user gave; `downloadFolder` passes `target`).
9. Seam: unexported `TreeUpload.readBatch` (nil is `readUploadBatch`), about 10 lines. `TestUploadTree_ReadFailurePartwayThroughAFolderIsReported`: one entry read, then a failure; the entry uploads, one failed event, the next folder uploads. Without the `reportWalked` on a read error it fails.
10. R-1.2.7 extended (folder's own name, single file's name; a named local path is not checked); items 1 and 2 tests carry it.
11. Kept exit 130; `cli.md` says so. `TestRunPut_CanceledFolderReportsThenEndsWithTheCancellation` (holds `return treeErr` in `finish`) and `TestRunPut_FolderWithAFailingFileUploadsTheOthers` (holds the failures append). Empty files: the fake serves no upload session. The fake now creates a file, not a folder, for a create with a file facet, takes an `onFileWrite` hook and `reportedNames`, and calls `assert.Fail` instead of `t.Errorf`.
12. `countOf` gives "1 file"/"1 folder" in the progress line and the summary; `TestCountOf_OneIsSingular` and the summary assertions.
13. `EnsureFolder` uses `namekey.Equal`; `TestEnsureFolder_ConflictFindsTheFolderInAnotherCase` failed before. The three conflict tests share `conflictingFolderSession`.
14. `put_shared.go:18` uses `errors.New`; the `json_contract_test.go` comment now says an empty file is a create plus a time update, which the fake serves. `interfaces.go:91` no longer cites `B-085`; `internal/graph/download.go:117` still does, so its row in `retired-backlog-ids.md` stays.
15. Commit message rewritten; every sentence checked against the tree.

e2e search: "Downloaded ", "Uploaded " match only sync output in the e2e suite, "Uploading " matches nothing; nothing to change, no e2e run.

### Checks
`go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l`, `goimports -l`: clean. `go test ./internal/cli/... ./internal/driveops/... ./internal/localpath/...`: pass; the same with `-race` behind the lock: pass (before the test-only lint fixes; the tree tests again with `-race -count=3` after them: pass). `TestLocalpathFunctions_AreEachDeclaredReadingOrChanging`: pass. `RealRepoIsClean` (4): pass. Spec citations by script: 23 cited tests exist, every cited requirement exists. Lint (`devtool verify lint`, `H7-lint2.log`): `0 issues.` on the third run (the first found 6 in test code, the second an unused nolint; all fixed).

### Corrections to the first report
- "Not fixed" 1: the `../x` example cannot happen (see item 2).
- "Not fixed" 7 said loopback URLs are admitted and the fake serves no upload session; true, and the fake also created every uploaded empty file as a folder (fixed).
- "A local folder that cannot be opened is no longer created on OneDrive" held for subfolders only until item 3.

### Seen and not fixed
1. `internal/driveops/tree.go:233` (`failedFile`) with `internal/driveops/transfer_upload.go:243,274` and `hash.go:43`: an upload failure names the local path two or three times (`<local>: opening <local> for hashing: opening <local>: …`). Not fixed here: download errors do not carry the path, so the tree's prefix is the one rule that names it; the fix is in the upload's own error texts (or a per-direction prefix), outside this change.
2. `internal/cli/trash.go:258`: "Permanently deleted 1 files". Fix: `countOf`.
3. `internal/cli/drive_rebind_root.go:125`: "holds 1 files now". Fix: `countOf`.
4. `internal/graph/download.go:117`: cites backlog id `B-085`. Fix: drop it, then its row in `spec/reference/retired-backlog-ids.md`.


## Review of the second version

A read-only reviewer read the second version: 0 must fix, 6 should fix, 10 notes (`R-H7b-findings.md` in the session scratch; the dispositions are mine and were made in the worktree, without a third writer pass).

- Fixed in the commit: the `B-085` citation is gone from `internal/graph/download.go` and `internal/driveops/interfaces.go` and its row from `spec/reference/retired-backlog-ids.md` (finding 3, "Seen and not fixed" 4); the commit message names the `mkdir` change (finding 4); the canceled-put test accepts the cut-short folder line the engine may print (finding 5); `countItems` is `countOf(n, "item")` (note 12); `TestRunPut_SymbolicLinksInAFolder` pins what a folder put does with symbolic links (note 15); the spec and doc-comment corrections of findings 1 and 2 and note 14.
- Moved, not fixed here: the "Failed:" line of a file that cannot be uploaded names its local path three times (finding 6: a rider on the row that changes the lookup after a conflict); "1 files" in `trash empty`, `drive rebind-root` and the recycle-bin flow (note 16: a rider on a later CLI row); the paged listing is a row of its own.
- Targeted lint of the touched packages: 0 issues after the review fixes, on the tree before the rebase; the pre-pr stage lints the rebased tree.

## Live proof

Run by me on 2026-11-15 from 03:04 to 03:10 PST against the test account, with an isolated build of `9f5ae79e` (the tree before two wording corrections: a test comment and the commit message) in a private data and config tree, about six minutes in all. The proof folder and the private tree were removed at the end.

- A tree of 200 files (14 folders by the summary's count): put exit 0 in 109 s, "Uploaded 200 files, 14 folders (8.5 MB)"; get into a named folder exit 0 in 41 s; same paths, contents and folders. Put again onto itself exit 0 in 96 s; get with no local path exit 0 in 37 s; same again.
- One file: put 4 s; get to a named file, into a named folder and with no local path, 2 s each; the content arrived at all three places.
- A one-file tree: "Uploaded 1 file, 1 folder (1.2 KB)" and "Downloaded 1 file, 1 folder (1.2 KB)". No line anywhere in the run held "1 files" or "1 folders".
- A folder holding a file and a FIFO: exit 1 in 4 s, the file is on OneDrive, the FIFO is not, one "Failed:" line, "Error: 1 item failed to upload"; the put ended by itself. A put of a FIFO alone: exit 1 in 1 s, ended by itself.
- `mkdir` of a folder, then `mkdir` below it spelled in lower case: both exit 0, one folder of that name on OneDrive, holding both children. A folder put onto the other spelling: exit 0.
- A folder put interrupted after 4 s: exit 130, a summary line and 11 "Failed:" lines. A folder get interrupted after 3 s: exit 130, a summary line and 11 "Failed:" lines.
- A get of the drive root with no local path was not refused: the root's name is one plain local name, so the download began into the private tree and the script ended it at its 30 s limit. That is the designed behavior (`TestRunGet_DriveRootWithNoLocalPathTakesTheRootsName`); the script's expectation was wrong, not the product.

Two wording defects the proof showed, both fixed before the gate: the commit message said a canceled folder put prints a "Failed:" line for every file it did not upload (it prints one for every file it had queued and one for each folder it stopped in), and one test comment said the same. The message also said `put -r`, a flag that does not exist.

The commit after these corrections and a rebase onto `04983d12` is `4e70f5bf`.



## Merged as #918 (`341d9820`, 2026-11-15 03:56 PST)

- One commit, `4e70f5bf` on `04983d12`, 34 files. The pre-pr gate passed (1847 s with the wait for the lock; the verifier itself 23m10s: lint 8m36s, build 3m41s, short unit tests 9m35s, no quirk events, no reruns). The PR was opened at 03:51 PST, the pre-merge stage passed in 264 s, and the post-merge stage merged it at 03:56 PST and passed after 979 s, which includes the push CI of main with the race job.
- Installed `341d9820` at 04:21 PST; `onedrive-go --version` prints it.
- A leftover of the live proof showed after the install: `status` reported 215 deletes held for confirmation, every one under the folder the proof uploaded and removed between 03:04 and 03:10. The proof tree was more than 30 percent of the test folder, so the delete protection held its removal, as designed. A one-shot `sync --confirm-deletes` is refused while the watch runs, so the watch client was stopped with SIGTERM, the confirm run deleted the 215 items locally in 11 s, the wrapper started the watch again, and `status` reads "Up to date" with 19 files. A live proof that uploads a large tree should use a private sync folder or remove the tree before the watch downloads it.
- Seen on the way and written down as plan work, not fixed here: the issue text tells a watch user to run `onedrive-go sync --confirm-deletes`, and that command is refused while the watch runs, so a watch user has to stop the watch to confirm (new plan row W63). The confirm run's summary says "deleted 215 files locally" for a count that includes folders (rider on H8, beside the "1 files" rider).
- Follow-up rows from the review: the upload failure line that names the local path three times is rider (c) on W54; paged folder listing is W55; both may start now.
