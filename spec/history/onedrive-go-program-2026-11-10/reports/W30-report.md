# A removed shared folder's local copy leaves by the delete rule: report (commit 552f09b6 on dc0f3558; earlier passes 03f555a3, e0ce5f7e)

## What changed
- `internal/sync/engine_shortcut_release.go` (new): the release, last step of a final drain: survey, forget-then-remove batches through `ExecuteLocalDelete`, kept findings.
- Planner (third pass): a final drain creates nothing new locally. `plannerMountContext.ReleasesLocalTree` (set from `RunOptions.ReleaseLocalTree` in `buildCurrentActionPlanFromInputs`) makes `planBuiltActions` run `leaveOutNewLocalItems` after normalization, before the mode partition: a download or local folder create whose view has no baseline row and no local entry is dropped, unless it is a folder an admitted non-delete action below it needs. Not counted in `DeferredByMode` or the follow-up; one Debug line per plan with `left_out`. A run without the option takes the old path untouched.
- Survey (third pass): the sync folder and every synced folder it walks must be writable (`isDirWritable` and not `isOnReadOnlyFilesystem`), else it is filed `notWritable` and nothing is removed; a synced file is compared by size (when known), mtime, ctime (when the filesystem has one) and identity (when both sides have one). New `Report.KeptNotWritable` / `LocalTreeHolds.KeptNotWritable` with its sentence; the sync folder itself is named "the shared folder itself" and gets no issue (an issue needs a path).
- Release batch: a refusal restores the refused row and the rest of the batch in one `restoreForgottenRows` call. `emptyReleasedRoot` reports its junk failure through `reportHousekeepingOutcome` (`Engine.releasedRootJunkFailing`): Warn once per streak, Debug repeats, Info on recovery.
- `engine_run_once.go`: `endOneShotRun` (housekeeping also for a release run, what the run left, the release). `api_types.go`: `RunOptions.ReleaseLocalTree`, `Report.Released/KeptNotOnOneDrive/KeptNotUploaded/KeptNotWritable`.
- `report_left_work.go`: `KeptEntries`, kept fields in `LocalTreeHolds`, `Reasons` (the only exported sentence builder; `keptReasons` unexported); `LocalTreeHolds` methods take a pointer and `Report.LocalTreeHolds` returns one (the type grew past the 128-byte `hugeParam` limit). `executor_delete.go`: `localContentChangedError`, `folderNotEmptyReason`/`namedWithMore` shared with the release.
- `issue_types.go`, `condition_keys.go`, `truth_status.go`, `observation_findings.go`, `watch_condition_log.go`, three `engine_runtime_*` switches: `removed_folder_kept` (not unsynced work, blocks nothing, every full scan clears it).
- `types.go`, `store_write_baseline.go`, `store_delete_ledger.go`: `ExemptFromDeleteHold`; `SyncStore.ForgetBaselineLocalCtime`; `forgetLocalCtimeProvenChanged` in the completion path.
- Parent: `shortcut_root_lifecycle.go`, `shortcut_root_planner_cleanup.go`, `shortcut_root_status.go`, `shortcut_root_transition.go`: root removed only as an empty dir; not empty goes back to the drain (`projection_not_empty`).
- `internal/synctree/synctree.go`: `RemoveTreeNoFollow` deleted with its tests.
- `internal/multisync`: final drains (one-shot and watch) set `ReleaseLocalTree`; blocked detail and kept notice use `LocalTreeHolds.Reasons`.
- `engine_current_plan.go`: the full scan's racily-clean window reads the run's clock, on top of the base's `buildLocalObserver`.
- CLI descriptor and `docs/troubleshooting.md` row name folders that cannot be written to. Specs: R-2.4.8 gains the create-nothing rule and the writable survey; sync-engine.md release section rewritten (no sentence accepts a download after a refusal or crash any more) and the clock paragraph says which clock observers and the full scan read; sync-planning.md gets "A Tree Being Released Creates Nothing New Locally"; earlier passes' edits to sync-control-plane, sync-shortcut, sync-execution, sync-store, sync-domain, sync-observation, data-model, system, non-functional stand.

## Rebase
`git rebase --onto dc0f3558 f5b735a4`: four conflicts. `engine_current_plan.go`: the base builds the observer with `buildLocalObserver` (wall clock); kept that and set the run's clock on top. `orchestrator_run_once_test.go`: the base's `synctest.CapturedRecord` field names plus my added assertion. sync-engine.md housekeeping paragraph: merged by hand (the base's streak text plus the release-run sentence). sync-execution.md: the base had already repaired the spliced trash paragraph; took the base. Build passed before going on. After the rebase the base's "equal content is a baseline update" made the old unseen-change simulation (rewrite the row's hash) plan a baseline update instead of an upload; the tests now change the file to same-size content with its mtime put back and the row given the new ctime (`changeContentUnseen`).

## How the release runs
After the passes (which create nothing new locally), housekeeping and the left-work read: if nothing holds the tree (or the shared folder is unreadable), a readable folder is surveyed first; anything filed (unsynced content, changed stat facts, a folder that cannot be written to) means nothing is removed. Otherwise rows are forgotten in batches of 256 (hold-exempt), the WAL checkpointed, entries go through `ExecuteLocalDelete` deepest first; the first refusal restores its row (without ctime on a proven content change) and the rest of the batch in one call, and the release stops. What left before stays gone: no later drain downloads it. Unreadable folder: no pre-survey, the release goes on past refusals. The parent then `RemoveEmptyDirNoFollow`s the root.

## Choice for the moved file
A tracked file OneDrive moved into a folder new to the drain: the folder is kept in the plan because the admitted local move below it needs it (the same "anything but a delete or cleanup" test as a preserved parent). Simpler than leaving the move out: dropping the move would also mean dropping whatever is planned below a moved folder, and would leave the comparison's move for every pass; keeping the folder plans nothing new beyond the move's own destination. End state tested: one move, one folder create, no download, nothing holds, the moved file in the trash, root empty.

## Tests added or changed (third pass)
- `TestRunOnce_FinalDrainCreatesNothingNewLocally` (a remote-only file; a remote-only folder with a file): no download or folder create, one pass, nothing holds, release runs, no entry, one Debug line with the count.
- `TestRunOnce_AnOrdinaryRunStillDownloadsANewRemoteFile`: a run without the option downloads it and logs no left-out line.
- `TestRunOnce_FinalDrainMovesASyncedFileIntoANewFolderAndReleasesBoth`: the moved-file choice above.
- `TestRunOnce_ARefusalThatPersistsNeverDownloadsWhatTheReleaseTook` (Docs/a, b, c; c refused): trash refusing after two moves (trash directory made unwritable at the second move's crash point; the housekeeping test's file-in-place would have discarded the two entries already in it) and a content change no stat fact shows. First drain: two released, c kept and reported, rows c and Docs stay. Second drain: no download; trash case: no trash move, c still kept and reported, rows unchanged; content case: c uploaded and released (one trash move), nothing held.
- `TestRunOnce_FinalDrainKeepsATreeWithAFolderThatCannotBeWrittenTo` (Docs 0555 with Docs/sub/b.txt sorting before Docs/a.txt; the sync folder 0555): two drains each remove nothing, name the folder, record the issue for Docs, download nothing.
- `TestReleaseLocalTree_ARowWithoutAModificationTimeWaitsForTheNextDrain`: mtime 0 holds as left work; the next drain records it (baseline update) and releases.
- `TestReleaseLocalTree_ARestoreThatFailsStillPutsBackTheRestOfTheBatch`: the refused row's restore fails (store crash hook); Docs/sub, Docs and top.txt still get their rows; the error names the failed restore.
- `TestObserveLocal_MeasuresTheRacilyCleanWindowOnTheRunsClock`: mtime an hour ahead of the wall clock, run clock two hours ahead: the scan trusts the recorded hash.
- Changed: the junk test calls twice and asserts one Warn and a Debug repeat; `TestReleaseLocalTree_ANewOrChangedFileIsLeftForTheNextDrain`'s in-place edit with its mtime put back is now caught by the survey (ctime), nothing removed; `TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain` asserts no download in either drain and the new content online; the crash test asserts no download on recovery; `TestLocalTreeHolds_ReasonsNameEveryPartInOrder` covers the not-writable sentence including the sync folder itself; the CLI descriptor test names folders that cannot be written to.
- Each new test fails with its fix switched off (one mutation at a time, restored after): leave-out off fails the create-nothing and loop tests; the needed-folder rule off fails the moved-file test; the writable check off, the split restore, a Warn on every call, the scan on the wall clock, an mtime of 0 trusted and the survey without ctime each fail their test.

## Findings checked
- 1 to 8 and 10: as described in the code; fixed or done as asked. 6: compares size, mtime, ctime and identity like `canReuseBaselineHash`, except (a) no racily-clean window (it guards trusting a stored hash; the survey trusts none, the delete reads every file), and (b) a size the row does not know and an identity either side lacks are not compared. Corrected in the fourth pass: metadata drift reads an unknown size as zero, so an empty file would never get one recorded and the tree would stay held for good; an identity is different, since a full scan backfills it onto rows of unchanged items (`TestReplaceLocalState_BackfillsBaselineIdentityForUnchangedItems`), but a missing one proves nothing about the content and a filesystem that gives none would hold the tree for good. A missing mtime or ctime does hold (drift records both).
- 9 is half wrong: after the rebase the watch observers do not read the engine's (replaceable) clock but its real clock, `wallClock`, which equals it only when no test replaced it. The sentence now says exactly that.
- The loop test's premise for the content variant is wrong: a content change no stat fact shows is not a persisting refusal. The refusal proves it, the row loses its ctime, and the next drain uploads and releases the file (one trash move). The variant asserts that; the persisting variant is the trash.

## Checks
On the final tree: `go build ./...` pass; `go vet ./...` pass; `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...` pass; `go test -count=1 -timeout 40m ./internal/sync/... ./internal/multisync/... ./internal/cli/...` pass (sync 791 s, multisync 18 s, cli 19 s). The same command without `-timeout` failed once on the 10-minute default: `TestModel_ChaosGraphConverges/seed=2` was still running with the load average near 10 from the other writers; with 40 minutes (the unit-test deadline the repository's own gate uses on this host) the whole package passed twice. Lint last (`golangci-lint run --allow-parallel-runners ./internal/sync/... ./internal/multisync/...`, pinned toolchain): 0 issues (`reports/W30-lint.log`); an earlier lint found five `hugeParam` hits once `LocalTreeHolds` grew past 128 bytes, fixed by pointer receivers and `Report.LocalTreeHolds` returning a pointer. No `devtool verify`, client or e2e run.

## Deviations
- Order: the row leaves before the entry (same commit, checkpointed): after would leave "row present, file gone" across a crash or lost WAL, a remote delete.
- Symlinks are never removed, followed or not. Visibility is a child-store issue type, not a parent state; no protocol or schema change.
- Housekeeping runs in every release run. A kept-locally folder (ignored content) keeps its row in a release.
- The sync folder that cannot be written to has no `removed_folder_kept` issue (an issue needs a non-empty path); the report and the drain's detail name it.

## Also fixed
- `engine.go` (touched for the junk streak flag): its two principle labels (P-4) cite R-6.4.9. `engine_run_once.go`: a doc line joined past 120 columns is rewrapped. `executor_delete.go`: its pointer to `KeptReasons` names `Reasons`. sync-control-plane.md's blocked-detail sentence names the not-writable sentence.
- Earlier passes' fixes stand (GOVERNS entry for `report_left_work.go`, principle labels in `issue_types.go` and `executor_delete.go`, `ExecuteLocalDelete`'s doc, the stale comment in `engine_shortcut_topology.go`, one module walk for the source tests); the spliced sync-execution.md paragraph is now the base's repair.
- The commit ends with the attribution line this session's harness sets (`Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`), not the shorter line the task named; one `git commit --amend` changes it if the short form is wanted.

## Seen and not fixed
1. The child's trash: `internal/cli/trash.go:99` (`openDriveTrash` opens only a configured drive's trash, so a child's cannot be listed or restored), `internal/multisync/shortcut_child_artifacts.go:413` (the purge removes the child's state database family and leaves its trash), `internal/sync/executor_delete.go:315` (a release files its entries with reason `deleted`): one separate change.
2. `internal/sync/executor.go:64`, `internal/sync/content_filter.go:25` and `:249`: principle labels (P-4, P-11, P-2, P-3).

## Review (first pass, 2026-11-13, on `03f555a3`)

Second pass asked for. Verdicts on what the first pass handed back:

- The four deviations are accepted as written.
- The equal-metadata finding (a file whose content changed while size and time stayed equal was released) was real; second-pass items 1 and 2 fix it (the verified delete's typed refusal and the survey).
- Not fixed (1) to (3) belong to the plan row for the parent's alias-root states (W32); (4) is the file-mode rider on W29.
- Not fixed (5), a re-download after a content refusal, was accepted as a cost at the time. The second-pass review withdraws that: see finding 1 below.

## Review (second pass, 2026-11-13 14:50, on `e0ce5f7e`)

Third pass asked for. A read-only reviewer read the whole diff (`03f555a3..e0ce5f7e`); its findings are in the session scratchpad (`R-W30-findings.md`). I read `engine_shortcut_release.go:20-107` and `engine_run_once.go:109-141` myself; the rest of each finding stands on the reviewer's reading until the third pass's tests prove or disprove it.

Sound, no change: the typed error `localContentChangedError`; `ForgetBaselineLocalCtime`; a partial release cannot travel to OneDrive as a delete; every tested item fails without its fix.

Must fix:

1. A refusal that persists loops. The release forgets rows and removes entries until it meets the refused entry; the next final drain sees every entry it removed as a new remote item, downloads it, and the release puts it in the trash again, on every drain. Triggers: a synced folder or the root that is not writable while its subfolders are; junk at the root that `emptyReleasedRoot` cannot remove; a trash that is full or on another filesystem; an immutable file. Decision (mine):
   - the survey refuses the tree when its root or any synced folder is not writable, with the existing `isDirWritable` and `isOnReadOnlyFilesystem`, so the usual case is found before anything is removed;
   - a final drain creates nothing new locally: in a run with `RunOptions.ReleaseLocalTree`, a remote item with no baseline row and no local entry is not downloaded, and no local folder is made for it unless an admitted action needs that folder. What is left out this way is not follow-up work and not left work. This closes the class (a partial release is never downloaded back, after a refusal or after a crash) and stops the first final drain from downloading files only to put them in the trash.
2. The doc comment on `releaseLocalTree` (`engine_shortcut_release.go:35-37`) still says "all at once or not at all … never makes the next drain download the folder again". It must say what the code and `sync-engine.md` say.

Should fix:

3. The junk warning (`:538-542`) repeats on every drain; the requirement its test cites says once, repeats at Debug.
4. The survey's `LocalMtime == 0` change (`:258-268`) has no test.
5. The commit message and the report still describe the first pass's rule (only a content refusal stops; all or nothing).

Notes taken into the third pass: 6 (the survey compares like `canReuseBaselineHash`, change time and identity included), 7 (a failed restore restores the refused row and the rest of the batch in one call and joins the errors), 8 (`KeptReasons` has no caller outside `internal/sync`: use it or unexport it). Note 9 (the sentence in `sync-engine.md:122-124` about the observers' clock) is checked against the rebased tree: it stays only if the watch observers read the engine clock there, otherwise it says what is true. Note 10: the full scan reading the run's clock (`engine_current_plan.go:372-376`) gets a direct test. Note 12 (the `.detach` sibling in the unreachable case) stays with the alias-root row (W32).


## Review (third pass, 2026-11-13 17:19, on `552f09b6`)

A read-only reviewer answered ten questions about the commit; I checked its
four should-fix findings against the code. Verdict: accepted, no must-fix.
The fourth pass takes the remainder:

1. A delete refusal that is not a content change (the trash refuses, an
   immutable file, a junk folder that cannot be emptied) is logged at Debug
   only and reported as "to be retried on the next sync" on every drain,
   with no path and no cause (`engine_shortcut_release.go` 439 to 444 and
   637 to 638). It becomes a kept kind of its own, named through
   `removed_folder_kept`, with a Warn once per streak.
2. The `removed_folder_kept` descriptions name two kinds while the code
   records more (`issue_types.go` 80 and 155, `sync-domain.md` 76).
3. The `wallClock` field comment in `engine.go` 124 says every observer
   runs on it; a pass's full scan uses the run's clock.
4. `planner_actions.go` 673 and the commit message say the final drain
   never fetches what the release would put in the trash; a remote edit of
   a synced file is still downloaded and then released.
5. Wording: "the next drain adopts" a row-less entry (it adopts it only
   when the content is equal); a test comment that still gives the second
   pass's reason for keeping the tree; the report's reason about identity
   backfill.
6. `accountsFor` allocates per call; a planner-level test of the
   needed-folder rule over three levels.
7. The commit trailer carried a model name; it becomes the plain line.

Not checked by the reviewer and still open: whether this commit changes the
duration of the chaos model's seed 2 (the model's clock now runs ahead of
every racily-clean window); whether `status` shows the drain detail when
the kept entry is the shared folder itself. Smells outside the diff:
`P-n` labels at `executor.go` 64 and `content_filter.go` 25 and 249 are a
rider for the next change to either file; the child's trash is plan row
W32.

## Fourth pass (commit 2122d67d on 8d6f7d46; was 36a070a3)

Per item:
1. A refusal now carries its cause (`releaseRefusal{path, err, contentChanged}`, `contentChanged` by `errors.As` on `localContentChangedError`). `releaseSurvey` has a fourth kind, `notRemoved`: `addRefused` files a proven content change as `unsynced` (work for the next drain) and any other unaccounted refusal as `notRemoved`. `Report.KeptNotRemoved` and `LocalTreeHolds.KeptNotRemoved` with the sentence "N synced item(s) could not be moved to the trash or deleted (paths); remove it/them by hand or fix what keeps it/them, then sync again"; `Any` includes it; `recordReleaseKept` records it as `removed_folder_kept` readable or not. The cause goes through `reportReleaseRefusals` (`reportHousekeepingOutcome`, new `Engine.releaseRefusalFailing`): one Warn per streak with the first cause and the entry count, repeats at Debug, Info on recovery; the per-entry Debug line stays as detail. The persisting-refusal test asserts, on both drains of the trash variant, `KeptNotRemoved == {1, [Docs/c.txt]}`, the sentence, the issue row and `WillRetry == 0`, plus exactly one Warn over the two drains; the content variant still asserts `WillRetry == 1` and no issue. Followed in api_types.go, report_left_work.go and its test, the multisync classifier test (a not-writable and a not-removed case), the CLI descriptor and its test, docs/troubleshooting.md, sync-engine.md and R-2.4.8.
2. `issue_types.go` (the constant's comment and `issueTypeLeavesFileUnsynced`'s reason) and sync-domain.md name all four kinds, with "a change that can no longer be sent to the shared folder"; the answer stays false (the not-writable and not-removed entries are synced already).
3. `engine.go` `wallClock` comment: the full scan's racily-clean window is measured on the run's clock (observeLocal).
4. `planner_actions.go` comment and the commit message: "never fetches an item it has not synced".
5. sync-engine.md, the commit message and the crash test's comment: "the next run adopts or keeps as a conflict copy".
6. The kept-tree test's comment drops the old download reason.
7. Report line 40 corrected (above); the same wrong reason sat in `releaseFileLooksUnchanged`'s comment, corrected there too: an unknown size reads as zero to metadata drift, so an empty file would never get one; a missing identity proves nothing, a filesystem without identities would hold the tree for good, and a full scan backfills a row's identity anyway.
8. `addRefused` builds one lookup per call (`releaseSurveyNames`: every listed path with its folders, plus the unwritable folders) and adds each newly filed path to it; `releaseSurvey.accountsFor` is gone.
9. `TestLeaveOutNewLocalItems_KeepsTheNewFoldersAnAdmittedMoveNeeds` (planner level): three new nested folders, a moved synced file in the deepest, a new download beside it; only the download is left out, one Debug line with `left_out=1`.
10. The trailer is `Co-Authored-By: Claude <noreply@anthropic.com>`; no model name in the message.

Mutation checks (one at a time, restored): filing every refusal as `unsynced`, warning on every drain, and ignoring the needed-folder rule each fail their test.

Commands and results:
- `go build ./...`, `go vet` of sync, multisync and cli: pass.
- `go test -count=1 -run '<release, planner, left-work, issue-type tests>' ./internal/sync/`: pass.
- `go test -count=1 -timeout 40m ./internal/sync/... ./internal/multisync/... ./internal/cli/...` (started before the host lock rule): pass (sync 959 s, multisync 9 s, cli 11 s).
- `go run ./cmd/devtool verify lint` (before the lock rule): golangci-lint reported 0 issues and then "Timeout exceeded" (22 min, exit 4) while other heavy runs were active: a load failure. Rerun once under `flock …/heavy.lock`: pass (golangci-lint 0 issues, actionlint pass).
- `flock …/heavy.lock go run ./cmd/devtool verify quick`: pass (10m52s; format, boundaries, conventions, spec governs, layering, traceability, build, tagged vet, short unit tests). The format step changed no file.

Seen and not fixed:
1. The child's trash (`internal/cli/trash.go:99`, `internal/multisync/shortcut_child_artifacts.go:413`, `internal/sync/executor_delete.go:315`), as before.
2. Principle labels in `internal/sync/executor.go:64` and `internal/sync/content_filter.go:25`, `:249`, as before.
3. `internal/sync/engine_housekeeping.go:27-41`: `housekeepingReport`/`reportHousekeepingOutcome` are documented as housekeeping only, but the release's junk and refusal reports use them too; a neutral name and comment belong with that file's next change.


## Review (fourth pass, lead, 2026-11-13 19:36 Pacific)

Read: `engine_shortcut_release.go`, `engine.go`, `api_types.go`, `report_left_work.go` and its test, the drain tests, `planner_actions.go`, `issue_types.go`, the status descriptor and its test, the multisync notice test, `docs/troubleshooting.md`, the commit message. Accepted with two fixes made in review; the commit is now `e6d9d0e8` on `8d6f7d46`.

1. Defect, fixed. `reportReleaseRefusals` ran on every refusal before the survey. When the shared folder cannot be read, a folder that still holds a kept entry is refused by the delete as not empty, and the release warned "Could not move a synced entry … to the trash or delete it" for a folder nothing was wrong with. `TestRunOnce_UnreadableSharedFolderReleasesWhatMatchesAndTheReAddPlansNoDelete` now captures the log and asserts no such Warn and a zero `KeptNotRemoved`; it failed on `2122d67d` and passes now. The warning is given only for the refusals the survey files as not removed: `addRefused` returns them, and a refusal with no error gets a stand-in cause (`errReleaseKeptWithoutCause`).
2. Smell, fixed (the writer's "seen and not fixed"). The once-per-streak helper was named for housekeeping and the release uses it twice. It is `Engine.reportStreakOutcome` with `streakReport` now, in `engine_housekeeping.go`, `engine_shortcut_release.go` and `spec/design/sync-engine.md`.

Checks on the fixes: `go vet ./internal/sync/` clean; `gofumpt -l` on the three Go files prints nothing; the release, drain and housekeeping tests pass (`ok … 46.5s`). The full gate runs when the branch is rebased for its PR.

Still open: the chaos model's seed-2 duration, whether `status` shows the drain detail for the shared folder itself, and the riders already recorded (principle labels at `executor.go:64` and `content_filter.go:25, 249`; the child's trash is W32).
