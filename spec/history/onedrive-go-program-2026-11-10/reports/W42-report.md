# W42 report

First pass stopped at the DoR on "answer (ii) is yes"; the decision (option A)
came back and the whole brief was done in one commit, `d71e3456`. DoR with the five answers and the refresh: `W42-dor.md`.

## What changed
- `internal/config/validate_drive.go`: `checkSyncDirsHoldNoManagedFolder` refuses a sync_dir equal to or above the data or config directory (resolved like sync_dirs; one line on macOS where they are one folder); `pathContains` compares whole components, so `checkSyncDirOverlap` now also sees `/` as overlapping every other sync_dir; overlap errors come in sorted order.
- `internal/synctree/detach_unix.go`: `DetachRegularFile`/`PreserveRegularFile` refuse a dest that `Rel` places inside the root (`refuseDestinationInside`); `openVerifiedRegular` opens the parent with `openParentFolder` (`O_DIRECTORY|O_NONBLOCK`); history wording in the doc comment rewritten.
- `internal/synctree/rename_noreplace_unix.go`: `MoveInNoReplace` decides "inside the root" by device and inode (`refuseFolderWithinRoot`: fstatat of `.`, `..`, `../..` on the opened source folder against the root folder), not by `Rel`.
- `internal/sync/executor.go`: `renameLocal` uses `RenameNoReplace` for every non-spelling move (`ErrSourceNotRemoved` is a move that happened, logged); a taken destination ends as `errActionPreconditionChanged` (superseded, existing outcome). Spelling-only path unchanged.
- `internal/sync/executor_preconditions.go`: `clearReplacedLocalMoveTarget` takes the verified destination out by the local delete rule, then the rename; verifier and metadata check renamed `existingTargetVerifier`/`validateExistingTargetMetadata` with a noun, so a move's refusal no longer says "download target"; misplaced doc comment of `validateLocalMoveSourceAgainstBaseline` moved back to it.
- `internal/sync/executor_delete.go`: `takeOutVerifiedLocalFile` (reason and crash point as parameters) shared by the local delete and the replacing move.
- `internal/sync/crash_points.go`: `crashPointLocalMoveDestinationTrashed`. `internal/sync/actions.go`: `ReplaceLocalTarget` comment.
- Docs: R-4.8.3, R-6.4.8, R-2.17.9; `spec/design/config.md` (validation), `system.md` (rooted mutation, row 32), `sync-execution.md` (local move, crash row, two stale sentences); `docs/configuration.md` (sync_dir rules).

Today's replacing move vs now: it hard-linked (or copied) the destination into the trash while leaving it in place, then a plain rename replaced it; without a trash it verified by path and the rename replaced it. Now the destination is moved once (detached into the trash, or removed through its verified descriptor) and the rename replaces nothing. The copy helper (`preserveReplacedDownloadTarget`, `PreserveRegularFile`) is still called by the download path (`validateDownloadTargetBeforeReplace`), so it stays.

## Tests (each failing before its fix unless noted)
- `TestValidateDrives_SyncDirHoldingAManagedFolderIsRefused` (item 1): nothing was refused before. `TestValidateDrives_FilesystemRootOverlapsEveryOtherSyncDir`: no overlap seen before.
- `TestDetachRegularFile_RefusesADestinationInsideTheSyncFolder` (item 2): the file was renamed inside the tree or linked beside itself.
- `TestRoot_MoveInNoReplace_RefusesASourceThatResolvesIntoTheSyncFolder` (item 5): the file of the tree was moved to `moved.txt`.
- `TestDetachRegularFile_AFIFOInPlaceOfTheParentFailsWithoutBlocking` (item 4): hung in `Root.Open` from `openVerifiedRegular`, test timed out.
- `TestExecutor_LocalMove_DestinationTakenAfterTheCheckIsKept` (item 3, file and folder): file replaced before; folder failed as a plain error (os.Root refused the empty folder, EEXIST), not a stale precondition.
- `TestExecutor_LocalMove_ReplacingMoveKeepsAFileThatTakesTheFreedName` (file and folder source): file replaced before; folder failed ENOTDIR.
- `TestCrashRecovery_ReplacingMoveBetweenItsTwoStepsFinishesTheMove` (crash between the two steps): the point did not exist before; after: next plan has 1 move, 0 remote deletes, 0 conflict copies, 0 uploads.
- `TestExecutor_LocalMove_OldNameThatCannotBeRemovedIsAMoveThatHappened`: new behaviour (warning) for the move.
- Gained assertions, none weakened: `TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination` (now also with a trash; moved file's identity at b.txt, nothing beside it, trash holds the old b.txt itself once), `TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting`, `TestRunOnce_RemoteMoveOverIdenticalFileConvergesInOnePass` (trash once, identities). These pass on the old code too: the end state of link-then-replace equals detach-then-rename; the no-replace proof is the two tests above.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. gofumpt: clean.
- `go test ./internal/sync/...` whole (lock): ok (702 s).
- `-race` (lock, `-timeout 30m`): `-run "TestExecut|TestCrashRecovery_|TestRunOnce_RemoteMoveOver"` ok; `TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_RemoteMoveOver(ExistingFileReplacesWithoutResurrecting|IdenticalFileConvergesInOnePass)` ok.
- config/synctree/localtrash/localpath: plain ok, `-race` (lock) ok. Two lint fixes came after the race runs (a constant for "..", a variable name in `MoveInNoReplace`); config/synctree/localtrash re-run plain: ok.
- `RealRepoIsClean` (4 checks): pass. Lint (lock): `0 issues.` (`W42-lint.log`; two earlier runs found goconst, shadow and sloppyReassign, fixed).
- Commit `d71e3456`, not pushed.

## Deviations
- R-6.4.8 keeps one narrower exception instead of none: the first try of a spelling-only local move stays a plain rename after a check (kept by the brief's rule on spelling-only renames).
- One crash point added (a test hook, not an outcome kind) to test the step between take-out and rename.
- The refusal text has no example folder ("for example ~/OneDrive" in the DoR), since the default differs per drive type.

## Live proof (for the owner)
    onedrive-go-install <this worktree>   # then let the watch wrapper restart
    mv ~/sync-test-root/<a synced file> ~/sync-test-root/<a new name>
    onedrive-go sync --drive '<test drive>'   # or wait for the watch to pick it up
    onedrive-go ls /                           # the new name is there, the old one gone
    onedrive-go status --drive '<test drive>'  # nothing left unsynced, no issue

## Packages for the reviewer
`./internal/cli/...` (config load errors, trash restore), `./internal/multisync/...` (config validation at control-plane start, synctree).

## Seen and not fixed
- `internal/config/drive.go:236-237`: a drive without sync_dir gets its default at resolve time, so the managed-folder check never sees it (an XDG data dir inside `~/OneDrive` passes). Fix: run `checkSyncDirsHoldNoManagedFolder` on the effective sync_dir in `ValidateResolvedForSync`.
- `internal/config/validate_drive.go:16`: validation does not know a `--config`/`ONEDRIVE_GO_CONFIG` file outside the config dir, so a sync_dir holding it is accepted. Fix: pass the config path into validation and treat its folder as managed.
- `internal/synctree/detach_unix.go:183-194` and `copyThroughDescriptor` (`:430-470`): the trash side is reached by path (`os.Lstat`, `os.Remove`, `os.MkdirAll`, `os.CreateTemp`, `os.Rename`). Fix: open the trash entry folder once and work on its descriptor.
- `internal/sync/crash_points_test.go:319`: `allCrashPoints` does not hold the new point, because `crashRecoveryScenario` has no replacing move, so the model-based run never draws it. Fix: add a replacing move to the scenario and the point to the list (changes seed replays).
- `internal/sync/executor.go` `renameLocal`: the spelling-only first try is a plain rename after a check, so on a case-sensitive volume a file appearing at the new spelling in between is replaced. Fix: try `RenameNoReplace` first and take the plain path only when it reports `ErrTargetExists` and `SameFile(src, dst)`.

## Second pass

Commit `6ed4fcaf` on `04983d12` (main's head after step 0's fetch). The rebase stopped nowhere: no conflict in any file. Build, both vets and config/synctree/localtrash tests passed on the rebased commit before any change.

### What each item became
1. `checkSyncDirMeetsNoManagedFolder(driveID, syncDir)` is the one-path form; `validateForSync` calls it on `canonicalSyncDirPath(rd.SyncDir)` after the absolute-path check, and the map form (`checkSyncDirsMeetNoManagedFolder`) calls it per configured sync_dir. R-4.8.3 and `config.md`'s validation paragraph state the rule without the exception.
2. `nestingOf` (same, holds, lies inside, apart) decides by text (`pathContains`) and, for absolute paths that both exist, by `os.SameFile` against the other folder and every folder above it (`hasFolderAbove`). Messages: `sync_dir X is|contains|lies inside Y, <what Y holds>; set sync_dir to a folder outside it|that does not contain it`. The search before the fix (tests, e2e helpers, scripts, docs) found no layout with a sync folder at or below a managed folder except the refusal tests. R-4.8.3, `config.md` and `docs/configuration.md` say "contain one another".
3. Every local move calls `RenameNoReplace` first; a spelling-only move takes the plain rename only on `ErrTargetExists` plus `namesOneLocalEntry` (lstat of both, `synctree.SameFile`), and the stored-spelling step (`respellLocal`, split out of `renameLocal`) runs after either. The edge is named in the comment and in `sync-execution.md`: on a case-sensitive volume two hard links named in the two spellings pass the check, and the plain rename leaves both names.
4. Not two lines: after item 3 a spelling-only move must still be superseded when its first no-replace rename is refused. `renameLocal` turns that first-step `ErrTargetExists` into `errActionPreconditionChanged` (the chain keeps `ErrTargetExists`), and `ExecuteLocalMove` branches on `errActionPreconditionChanged`. A failure of a step through the temporary sibling (`respellLocal`) stays the plain "renaming A -> B" failure at `OldPath`.
5. Stopped, see below. The failing test is not in the commit.
6. The test also asserts the outcome (`ActionLocalMove`, `Path` b.txt, `OldPath` a.txt) and that a.txt and b.txt are one inode. `sync-execution.md` says the leftover is a second name of the same file, which a later pass sees as a new local file.
7. `sync-execution.md`, the `renameLocal` comment and the commit message: a file moves without a window wherever the system has the flag or hard links, and a file on a filesystem with neither takes the check and plain rename, like a folder without the flag.
8. `sync-execution.md` and `clearReplacedLocalMoveTarget`'s comment: moved once, copied through its verified descriptor only when the trash is on another filesystem.
9. `docs/configuration.md`: "your home folder or `/`", XDG first, and the one macOS folder `~/Library/Application Support/onedrive-go` named once (names checked in `paths.go`).
10. `returnsWithoutWaitingOnFIFO` runs the call in a goroutine. After `fifoWaitBound` (30 s) it opens the FIFO `O_RDWR`, which opens without waiting and releases a reader, closes it, and fails with a message naming the hang. A second bounded wait covers a call that still does not return.
11. R-6.5.2 (incomplete operations do not corrupt state) fits; R-6.8.9 (no retry loop in the executor) does not. Now cites R-6.5.2, R-2.17.9.
12. Done on the rebased text: the replacing-move sentence follows the second exception, words unchanged. The second exception itself is reworded for item 3 (a spelling-only move becomes a plain rename only after its no-replace rename found the destination taken and a check found the source there).
13. The comment says the bound ends the walk with an error where no folder above reports itself as its own parent, and that a real tree reaches its top in far fewer steps.
14. One clause in `sync-engine.md` line 110's list (after a replacing local move took its destination into the trash and before it renamed), naming its own test.

### Item 5 (stopped)
Test kept at `/tmp/scratch/scratchpad/w42p2/replacing_move_freed_name_test.go.txt`. Copy it into `internal/sync` as `replacing_move_freed_name_test.go`; it fails three assertions. The hooked run (one pass) supersedes the move (Superseded 1, LeftWork WillRetry > 0); local a.txt is "alpha", b.txt is the saved file, and the old b.txt is in the trash once. The next pass (one pass) makes 0 moves, 1 conflict copy (the saved file becomes `b.conflict-<time>.txt`), 1 download (item-a into b.txt) and 0 deletes on either side, and a.txt stays. The pass after uploads 2 files: the conflict copy, and a.txt as a new item. End state: nothing lost, nothing deleted on OneDrive, item-a's content unchanged. But OneDrive holds a.txt again after it moved that item to b.txt, and the moved item was downloaded beside its old copy. Both contradict R-2.17.9 ("moved there too rather than downloaded beside its old copy", "shall not be uploaded again"). `composedDisplacement` (`planner_move_composition.go:1148-1163`) returns never, because item-b's local position no longer matches its baseline path (the file at b.txt is not the baseline item), so no move is composed and the remote move is planned as a download. The fix is in the planner, a file another branch changes.

### Tests added or changed
- `TestValidateDrives_SyncDirAndManagedFolderHoldingOneAnotherAreRefused` (renamed from `..._SyncDirHoldingAManagedFolderIsRefused`; adds rows for the trash inside the data folder and a folder inside the configuration folder, and expects "is" for the equal case): the two inside rows and the equal row failed before the fix.
- `TestValidateDrives_SyncDirSpelledInAnotherCaseStillHoldsTheDataFolder`: skips here (ext4 tells letter case apart) and runs on the macOS shards. It has not been seen failing, because this host has no case-folding volume.
- `TestValidateResolvedForSync_DefaultSyncDirHoldingTheDataFolderIsRefused`: failed before the fix (nil error).
- `TestExecutor_LocalMove_DestinationTakenAfterTheCheckIsKept`: adds two rows, a file and a folder moved to another spelling (skipped where the names cannot coexist). Before the fix the file was replaced (success) and the folder failed as a plain EEXIST.
- `TestExecutor_LocalMove_SpellingRenameWhoseSecondStepFindsTheNameTakenFails` models a case-folding volume: a hard link under the other spelling, hidden from listings, and a no-op case-variant rename. Before the fix the move was superseded.
- `TestExecutor_RenameLocal_SpellingRenameThatLeavesItsTemporaryNameIsDone` now calls `respellLocal` directly, with the same assertions. After item 3 the no-replace rename on ext4 renames for real, so no handle-level seam reaches the temporary-sibling step through `renameLocal`.
- `TestExecutor_LocalMove_OldNameThatCannotBeRemovedIsAMoveThatHappened`: new assertions. With the `ErrSourceNotRemoved` case of `renameLocal` taken out it failed (a plain failure); the case is back.
- `TestDetachRegularFile_AFIFOInPlaceOfTheParentFailsWithoutBlocking`: with `openVerifiedRegular` opening the parent by `r.Open` again, all three subtests failed within the bound (30 s), with the message; the line is back.

### Checks
- `go build ./...`, `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass, after the rebase and at the end. gofumpt and goimports `-l` on every changed Go file: clean.
- `go test` of config, synctree, localtrash, cli and multisync: ok. cli and multisync needed no layout change.
- `go test -count=1 -timeout 60m ./internal/sync/...` (lock): ok (470 s).
- `-race` (lock, `-timeout 30m`): `-run "TestExecut|TestCrashRecovery_|TestCaseInsensitiveShim|TestRunOnce_RemoteMoveOver" ./internal/sync/` ok (512 s); `./internal/config/... ./internal/synctree/...` ok.
- `RealRepoIsClean` (4 checks): pass, before and after the lint fixes.
- `devtool verify lint` (lock), `W42-lint2.log`: `0 issues.` and `status: pass`. The first run found goconst (`"outside it"`), gosec G304 (the FIFO open in the test, now `localpath.OpenFile`) and an unused `nolint`; all three were fixed, and the affected tests were rerun before the second lint run.
- Spec citations checked by hand: every test name put into a spec exists, and R-4.8.3, R-6.4.8, R-6.5.2, R-2.12.4 and R-2.17.9 exist.

### Where the first report is wrong or out of date
- Line 10 ("Spelling-only path unchanged") and line 38 (R-6.4.8's narrower exception): changed by item 3.
- Line 16 ("the destination is moved once"): it is copied through its descriptor when the trash is on another filesystem (item 8).
- Lines 53 and 57 (the default sync_dir, the spelling-only first try): fixed in this pass.

### Also fixed
- `internal/config/resolved_validator.go` `validateForSync`: the stat switch's last case repeated `!errors.Is(err, os.ErrNotExist)` after the case that returns on it.
- `spec/design/sync-execution.md`: the paragraph that called the spelling-only first try "the one local rename that stays a plain rename" now says when it becomes one.

### Seen and not fixed
- `internal/sync/planner_move_composition.go:1148-1163` (`composedDisplacement`) and the planning around it: after a replacing move ends with a file at the freed name, the next pass downloads the moved item beside its old copy, and the pass after uploads the old copy as a new item (item 5; R-2.17.9 is marked [verified]). Fix: when the file at the destination is not the displaced baseline item, keep it as a conflict copy and still compose the local move of the source, so nothing is downloaded or uploaded again. The scratch test above is the regression test. Not fixed here: the file belongs to another branch in flight, and the change is more than a few lines.
- `spec/design/system.md`, rooted-mutation paragraph: the sentence "Config validation keeps the client's own folders out of every sync folder in the first place" leaves out the other direction (a sync folder inside a managed folder). Not fixed here: main and another branch edit this file, and no item named the sentence. Fix: "keeps the client's own folders and the sync folders from containing one another".
- `internal/config/load.go:153` (lenient load): `status`, `drive list` and `shared` warn about a configured sync_dir that meets a managed folder, but not about a default sync folder that does, because only `ValidateResolvedForSync` sees defaults; `sync` refuses it. Fix: run the one-path check on each drive's effective sync folder when the lenient load collects warnings.


## Review fixes (second review)

New commit: `335e49ed` (one commit on main 341d9820, not pushed). The title is now "Keep sync and managed folders apart; never replace on a local move" (the old one was 74 characters). Every message line is at most 72 characters.

1. **Done.** The paragraph is deleted. Its one fact said nowhere else (both temporary-sibling steps never replace) is now in the spelling-only paragraph: "goes through a temporary sibling with two renames that never replace". That paragraph also says the two names' entries are compared and a different file is not the source, and it cites the item-3 test.
2. **Done.** `syscall.ENOSYS` is in `localpath.IsLinkUnsupported`; its doc names a FUSE filesystem without a link operation. A row was added to `TestIsLinkUnsupported_OnlyTheErrnosOfALinkThatCannotBeMade`. Each caller is right with it:
 - `synctree.renameOrLinkAt`: ENOSYS now means link refused, so the last step runs (check and plain rename, or the exclusive copy for a move-in). That is the no-hard-links case.
 - `synctest.SkipWithoutHardLinks` and synctree's `skipWithoutHardLinks`: these skip on a filesystem without hard links, which is what ENOSYS means.
 - `localpathReadingFunctions` in `executor_rooted_mutation_test.go`: it is still a function that leaves the filesystem as it is.
 - `PreserveRegularFile` keeps its own errno list; see "Seen and not fixed" 1.
3. **Done.** New test `TestExecutor_RenameLocal_SpellingRenameOntoAnotherFileKeepsBoth`, in `executor_local_spelling_rename_test.go`.
 - It calls `renameLocal` with the two spellings as different files, and skips where they cannot coexist.
 - It requires ErrTargetExists plus errActionPreconditionChanged, both contents kept and both names present.
 - It calls `renameLocal` directly for two reasons. Through ExecuteMove, the precondition finds the taken destination first. The existing row's lstat shim also blinds `namesOneLocalEntry`.
 - With the `SameFile` line replaced by `return oldInfo != nil && newInfo != nil`, it failed ("Expected error ... but got nil"). The line is back.
4. **Done.** One sentence in the `CaseInsensitive` doc. The no-replace renames (RenameNoReplace, MoveInNoReplace) rename on folder descriptors, so they meet the volume beneath, not the shim.
5. **Done.** In `nestingOf`, each identity check now needs only the folder whose identity it compares to exist, with both paths absolute. The doc is updated.
 - The case test is renamed `TestValidateDrives_SyncDirSpelledInAnotherCaseStillMeetsTheDataFolder`. Its rows are: an existing data folder, a data folder not made yet, and a sync_dir not made yet inside the data folder. The rows skip where letter case is told apart, which includes this host.
 - New `TestNestingOf_ComparesAnExistingFolderWithAPathNotMadeYet` runs on Linux; a link stands in for another spelling. It failed on the old both-exist gate.
 - config.md's sentence and citations are updated.
6. **Done**, about 20 product lines.
 - `refuseDestinationInside` opens dest as a folder (so the root itself is walked from), else the folder that would hold it, using `parentFolderFlags`. It then runs `refuseFolderWithinRoot` on that folder.
 - `refuseFolderWithinRoot` now takes the refusal wording. Its bound is renamed `maxFoldersAboveAFolder`.
 - New `TestDetachRegularFile_DecidesTheDestinationByTheFolderItResolvesTo`, for both detach and preserve. A destination below a link in the tree that leads outside is taken, and the file lands outside. A destination below a link outside that leads in is refused with ErrUnsafePath, and nothing moves. All 4 rows failed with the old `Rel` check.
 - system.md's sentence and the R-6.4.8 evidence row are updated.
7. **Done.**
 - The `hashReader` seam now takes ctx and defaults to `driveops.ComputeQuickXorHashFromReaderContext`.
 - `existingTargetVerifier`, `clearLocalMoveDestination` and `clearReplacedLocalMoveTarget` take ctx again, as on main. So do `DeleteLocalFile` and `verifyLocalDeleteContent`.
 - So the hash stops when the action's context ends for a replacing move's destination, a download target with a trash, and a local delete.
 - `driveops.ComputeQuickXorHashFromReader` had no caller left and is removed. The drive-transfers.md sentence that named it, and a `ComputeQuickXorHash` that no longer exists, is rewritten.
 - New `TestExecutor_LocalMove_DestinationHashStopsWhenTheContextEnds` failed with `context.Background()` in the verifier.
 - This is about 15 product lines in 4 files, each one passing ctx on. There is no new type and no new seam.
8. **Done**; result below. Nothing changed in the worktree, and R-2.17.9 is untouched.
9. **Done.** The replacing-move point is taken out of the snapshot sentence's list and has its own sentence. It has its own test, uses the database as committed, simulates no power loss, and the crash model does not draw it.
10. **Done.**
  - sync-execution.md is in present tense with the finding's wording.
  - "(local move, below)" is now "(local move, above)".
  - The `clearReplacedLocalMoveTarget` comment is reflowed and in present tense, and says the hash stops when ctx ends.
  - docs/configuration.md says home is refused while either managed folder lies in it, as the default ones do.
11. **Done**, with the exact sentence.

### Item 8 result

Main does not reach the same end state, and cannot reach the state at all.

I copied main 341d9820 under scratch and added the test as `internal/sync/replacing_move_freed_name_test.go`. The only adaptation was adding `requireTrashHoldsOnce`, which main lacks. Every other helper exists on main: `newMoveScenarioRemote`, `newSyncHarness`, `runScenarioPass`, `harnessLocalTree`, `requireScenarioQuietPlan`, the remote's `deleteRemotely`/`move`/`tree`/`contents`/`deletes`, `LeftWork`, `oneShotPassLimit` and `execCfg.crashAt`.

Running `go test -count=1 -timeout 20m -run TestReplacingMove_ ./internal/sync/` fails to build with one error: `replacing_move_freed_name_test.go:50:15: undefined: crashPointLocalMoveDestinationTrashed`.

Why main cannot reach the state:
- **No hook.** Main's local move reaches only `crashPointLocalMoveDone`, which comes after the rename (main `executor.go:543`). `clearLocalMoveDestination`, `clearReplacedLocalMoveTarget` (main `executor_preconditions.go:797-830`) and `preserveReplacedDownloadTarget` call no crash point.
- **No freed-name window.** With a trash, main links or copies the verified b.txt into the trash and leaves it in place. `renameLocal` (main `executor.go:600-603`) then calls `syncTree.Rename`, a plain rename that replaces b.txt. Without a trash, main verifies by path and the same rename replaces it. The name is never free, so the first run's "1 superseded" cannot happen.

By reading, not by running: on main, a file saved at b.txt between the preserve and the rename is replaced by the moved file. Its content is lost, and the trash holds only the old b.txt.

The copy and the tar are removed.

### Checks
- `gofumpt -l` and `goimports -l` on the 15 changed Go files: no output.
- Pass:
- `go build ./...`
- `go vet ./...`
- `go vet -tags=e2e,e2e_full,integration,stress ./...`
- `GOOS=darwin go vet` on synctree, sync, config and localpath
- `GOOS=freebsd go build ./internal/synctree/`
- Tests by name, one call per package, all ok:
- **config:** the 3 changed tests plus the holding-one-another test. The case rows skip here.
- **localpath:** the errno test.
- **synctree:** `TestDetachRegularFile_`, `TestPreserveRegularFile_`, `TestRoot_MoveInNoReplace_`, `TestRoot_RenameNoReplace_`.
- **driveops:** `TestComputeQuickXorHash*`.
- **sync:** `TestExecutor_RenameLocal_`, `TestExecutor_LocalMove_`, `TestDeleteLocalFile_`, `TestExecutor_Download_LocalContentDiffersFromBaselineIsSuperseded`, `TestCrashRecovery_ReplacingMoveBetweenItsTwoStepsFinishesTheMove`, `TestExecutorFiles_`, `TestLocalpathFunctions_`, `TestChangingCallsIn_`, `TestCaseInsensitiveShim_RemoteCaseOnlyRenameConverges`, and every ListsEvery, Governs and Registered test.
- **sync, `-short`:** the Table and Exhaustive tests.
- The same tests with `-race`, behind the lock, in one call over the 5 packages: ok (sync took 59 s).
- `go test -count=1 ./internal/config/... ./internal/synctree/... ./internal/localpath/...`: ok.
- `RealRepoIsClean`: 4 PASS.
- Lint of config, driveops, localpath, sync, synctest and synctree, behind the lock: `0 issues.`, in reports/W42-lint3.log.
- Every cited test and requirement in the 6 changed spec and doc files exists. The script's only hit is `TestIntegration_` in system.md, an existing prefix mention in text I did not change.
- Each of items 3, 5, 6 and 7 was seen failing on the old code before the fix.

### Seen and not fixed
1. **`internal/synctree/detach_unix.go:187` (`PreserveRegularFile`).** It falls back to a copy only on EXDEV, EPERM or EMLINK, a list kept separately from `localpath.IsLinkUnsupported`. So a link refused with ENOTSUP, EOPNOTSUPP or ENOSYS (for example a FUSE mount without links) fails the preserve. With a trash, every download over an existing file there fails. Smallest fix: `case errors.Is(linkErr, unix.EXDEV), localpath.IsLinkUnsupported(linkErr):`, plus a link seam for its test, since `linkatOut` is a plain function.
2. **`internal/sync/executor_delete.go:223`, and the `os.ErrNotExist` case of `clearReplacedLocalMoveTarget` in `internal/sync/executor_preconditions.go`.** An ENOENT from the trash side reads as "source already absent" or "the file left the name on its own". That ENOENT happens when the trash entry's folder is gone between Reserve and the move, and now also when `refuseDestinationInside`'s open fails. The delete is superseded and replanned, and the move renames into a name it finds taken. Nothing is lost, but it is misclassified. Smallest fix: wrap the trash-side failures (`refuseDestinationInside`'s open and `moveDetachedOut`) so they do not match `os.ErrNotExist`.

## Fourth pass (two leftovers)

Commit `34b0c437` (amended from `419c52a2`; still one commit on `12a48bb2`, not pushed).

### 1. A refused link copies, by the one answer
- `internal/synctree/detach_unix.go:199`: `PreserveRegularFile` copies on `errors.Is(linkErr, unix.EXDEV)` or `localpath.IsLinkUnsupported(linkErr)` instead of its own EXDEV/EPERM/EMLINK list. `:186` links through the new seam `rootOps.linkOut` (`internal/synctree/synctree.go:169-173`, default `linkatOut` at `:223`). The rootOps comment (`synctree.go:136-140`) called `lstatAbs` the single absolute-path exception while `syncDir` already took one; it now names `linkOut` and `syncDir` as taking an absolute path only for the place outside the root.
- Test `TestPreserveRegularFile_CopiesWhenTheLinkCannotBeMade` (`internal/synctree/detach_test.go:130`): ENOTSUP, EOPNOTSUPP, ENOSYS, EPERM, EMLINK and EXDEV preserve by copy (source keeps its inode and content, dest is a copy with the content); EIO returns the errno and leaves no copy. First run: the ENOTSUP, EOPNOTSUPP and ENOSYS rows failed with `preserving docs/a.txt: operation not supported` / `function not implemented`; the other rows passed.
- Sweep of link-errno decisions: `rename_noreplace_unix.go:337` (`renameOrLinkAt`) already uses `localpath.IsLinkUnsupported`; it returns EXDEV on purpose (MoveInNoReplace's contract: the restore copies). `synctest/hardlinks.go:23` uses it too. `linkBackNoReplace` decides nothing by errno. `rename_noreplace_linux.go:15` and `_darwin.go:14` decide on the rename flag, `root_identity_unix.go:186` on xattrs, `sync/engine_locks.go:197` on locks, none on a link. `internal/sync` makes no link decision.

### 2. A failed destination is not a missing source
- `detach_unix.go:31` `ErrDestinationFailed`, `:209` `destinationFailed` (sentinel by `%w`, cause as text by `%s` of `.Error()`, as `putBackFromTemporarySibling` already names its addenda). It wraps `refuseDestinationInside`'s open (`:243`), `moveDetachedOut`'s rename (`:434`), `copyVerified`'s copy (`:455`, so the cross-filesystem move and Preserve's copy fallback too) and Preserve's failed discard of a stale link (`:196`). `copyVerified`'s failed discard after a race is now text (`:459`), so it matches the race only. Preserve's other link failures keep the link's errno, which cannot tell the two sides apart (comment at `:202`). Doc comments of `DetachRegularFile`, `PreserveRegularFile`, `moveDetachedOut` and `copyVerified` say so. No product change in `internal/sync`: `DeleteLocalFile` and `clearReplacedLocalMoveTarget` take their ordinary failure path for it.
- `TestDetachRegularFile_ADestinationThatFailsIsNotAFileThatIsGone` (`detach_test.go:183`; folder missing before the check, folder removed during verify): first failed, ErrorIs not in a chain ending in `no such file or directory` (`moving out of the sync folder: no such file or directory` for the second row).
- `TestDeleteLocalFile_ATrashThatFailsIsAFailureNotAFileAlreadyGone` (`internal/sync/executor_local_trash_test.go:129`): first failed with `file changed while it was being synced: local delete source docs/a.txt is already absent`.
- `TestExecutor_LocalMove_ATrashThatFailsFailsTheReplacingMove` (`executor_local_trash_test.go:153`): first failed with `file changed while it was being synced: local move destination b.txt was taken before the move: renaming a.txt to b.txt: destination already exists`. Helper `hashRemovingTrash` (`:110`, 14 lines) removes the trash from the executor's hash hook.
- Touched: `TestCopyVerified_DirectorySyncFailureLeavesNothingAtTheDestination` asserts `ErrDestinationFailed` and the EIO text instead of `ErrorIs(EIO)`.
- Consequence: a trash-side ENOSPC, EROFS or EACCES no longer matches its errno, so it is a per-item failure, not the local disk scope or a permission issue on the sync path (the trash can be on another filesystem).

### 3. Spec
- `spec/design/sync-execution.md:720-726` (the detach paragraph: a trash that fails is `synctree.ErrDestinationFailed`, the delete fails instead of being superseded) and `:338-341` (a trash that fails fails the replacing move). Neither passage said before what a failed trash move or a refused link does; the sentences are new.
- `spec/design/system.md:725`: `ErrDestinationFailed`, Preserve's copy rule by `localpath.IsLinkUnsupported` through `rootOps.linkOut`, and the absolute-path rule of the injectable ops. The four cited tests exist; no requirement number added or changed.

### Checks
- `gofumpt -l`, `goimports -l` on the four changed Go files: no output.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `GOOS=darwin go vet ./internal/synctree/ ./internal/sync/`, `GOOS=freebsd go build ./internal/synctree/`: pass.
- Named tests, one call per package: synctree (3) ok, sync (2) ok; the same with `-race` behind the lock: ok.
- `go test ./internal/synctree/... ./internal/localpath/...`: ok. `go test ./internal/sync/...` behind the lock: ok (611 s).
- `go test -v -run RealRepoIsClean ./internal/devtool/`: 4 PASS.
- Lint of `./internal/synctree/... ./internal/sync/...` behind the lock: `0 issues.` (`reports/W42-lint4.log`).

### Seen and not fixed
1. `internal/sync/executor_delete.go:328-330` (`takeOutVerifiedLocalFile`): a `Trash.Reserve` failure is wrapped with `%w`, so an ENOENT from it (the entry folder removed between its `MkdirAll` and the entry write, as by a purge of an empty reservation) still reads as "already absent" at `executor_delete.go:223` and as "left on its own" in `clearReplacedLocalMoveTarget`. Smallest fix: name the Reserve cause as text there, with a seam in `localtrash` (Reserve's write) for the test.
2. `internal/localtrash/localtrash.go:314-316` (`heldFileIn`): a `ReadDir` that fails because the entry folder is gone reports the entry as holding its item, so `Placement.Abandon` of a vanished entry logs "it holds a file" for an entry that no longer exists (seen in the new executor tests' debug log). Smallest fix: return nil when that `ReadDir` fails with `os.ErrNotExist`, the item lstat having already found nothing.
3. `internal/synctree/detach_unix.go:402` (`linkBackNoReplace`): on a filesystem without hard links the put-back of a raced set-aside file always fails, so the file stays under `.onedrive-go.<random>.detach` (reported every full scan, never lost, but not back at its name). Smallest fix: when the link is `localpath.IsLinkUnsupported`, put it back with the checked rename the no-replace rename falls back to (`renameCheckedAt`, `rename_noreplace_unix.go:349`), plus a seam and a test. Not fixed here: it would more than double this pass's product diff.


## Merged as #934

- Merged 2026-11-16 03:33 PST as `abfe625d` (squash); installed 03:36.
- The pre-PR gate passed on `67c9d53d` (base `ca8ecac6`); the PR's CI run was green; pre-merge passed.
- The live proof is mine and is in the PR body ("Live, ..." lines).
- Not known: whether the letter-case test ran or was skipped on the macOS
  runner of the PR. The shard logs hold package lines only, no test names.
- Opened by this merge: W70, W59, W44, W67 (after W79 too: same planner
  file) and W81 (after W51 and W79).
