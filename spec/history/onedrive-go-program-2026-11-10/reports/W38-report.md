# Report: a gone or unreadable shared folder says so; a run counts shared folders as shared folders

Commit `df8f4f91`, one commit on a6f6fbf3, not pushed; 46 files. This replaces the earlier stop report: the decisions of 2026-11-14 replaced decision (a).

## What changed (one line per file)
- `internal/sync/issue_types.go`: new member `remote_root_not_found`; same arm as `remote_read_denied` in `isMember` and `issueTypeLeavesFileUnsynced`.
- `internal/sync/condition_keys.go`: condition key `remote_root_not_found`, ranked after `remote_read_denied`, not self-clearing.
- `internal/sync/observation_findings.go`: `isObservationRemoteReadDenied` is replaced by `rootReadFailureIssueType` (404 -> new type, 403 -> `remote_read_denied`); `rootReadFailureObservationFindingsBatch(driveID, type)`; the remote batch manages both types.
- `internal/sync/engine_primary_root.go`: the three root paths write the answered type, through one `rootReadFailureBatch` helper.
- `engine_shortcut_release.go` (`sharedFolderUnreadable` accepts both), `truth_status.go`, `engine_runtime_lifecycle.go`, `engine_runtime_completion.go`, `engine_runtime_permissions.go`: the new type takes `remote_read_denied`'s arm.
- `internal/sync/watch_condition_log.go`: watch label for the new key.
- `internal/multisync/report.go`: `SelectedCount` is gone; `SelectedDriveCount` (distinct standalone mount IDs) and `StartupResultCount`.
- `internal/cli/sync_render.go`: closing sentences count drives and shared folders apart (`selectionTally`).
- `internal/cli/sync_console.go`, `sync_startup_messages.go`: one line each, `StartupResultCount`.
- `internal/cli/status_condition_descriptors.go`: `statusStoreRoot`; shared-folder-root texts; drive-root 404 text; the unexpected arms call the helper.
- `internal/cli/status_sync_state.go`, `status_snapshot.go`: the store's root kind is passed down; a shortcut child is read as a shared folder.
- `testutil/standing_conditions.go`: the e2e settle parser reads all three forms of the closing count. `e2e/e2e_test.go`: the left-work error pattern too.
- Docs: `docs/troubleshooting.md`, `docs/scripting.md`; spec: `cli.md`, `sync-observation.md`, `sync-engine.md`, `sync-domain.md`, `sync-store.md`, `sync-control-plane.md`.

## Tests added (each seen failing first)
- `TestRootObservation_AnswerDecidesTheStoredRootIssue`: 18 cases (3 root paths x 6): 404 and 403 rows; an old 403 row then a 404 gives one new-type row; the new type then a 403 gives one 403 row; a successful read clears either.
- `TestRootReadFailureIssueType_NamesOnlyNotFoundAndForbidden`, `TestSharedFolderUnreadable_EitherRootAnswerMakesTheFolderUnreadable`.
- `TestRunOnce_DriveWhoseSharedFoldersCannotBeSetUpIsOneSelectedDrive` (multisync): two results, one selected drive.
- `TestRunOnceResultError_CountsSharedFoldersApartFromDrives`, `TestRunOnceDrivesError_CountsSharedFoldersApartFromDrives`.
- `TestBuildSyncStateInfo_RootReadFailureSaysWhatTheRootIs` (text and JSON, five cases), `TestBuildChildStatusDrive_ReadsTheStoreAsASharedFolders`.
- `TestStandingSyncConditions_SettleReadsEveryFormOfTheClosingCount` (testutil).

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test ./internal/cli/...`: pass; with `-race` (lock): pass. `./internal/multisync/` plain and `-race`: pass. `./testutil/` `-race`: pass.
- `go test ./internal/sync/...` whole (lock): pass. `-race` over the 137 tests of the touched test files (lock): pass; the new file again after the lint fix: pass.
- Lint over cli, sync, multisync, testutil (lock): `0 issues.` (W38-lint.log; a first run found one gocritic issue in the new test, fixed).

## Deviations
- A folder delta that answers 404 is read as "change tracking unavailable" (`engine_mount_root.go:82-99`), so a shared folder root's 404 is the answer of the fallback listing. The test stubs both; nothing was changed there.
- JSON: field names and order are unchanged. No JSON field carried `SelectedCount`. The issue `type` for a root that answers 404 changes to `remote_root_not_found`, as decided.
- `testutil/standing_conditions.go` and `e2e/e2e_test.go` parse the closing sentence, so they were updated (not named in the brief).
- Requirement: none covers "an issue says what the user can do next". R-2.10.4 (grouped issues) and R-6.6.28 (exit and closing count) are cited; R-6.6.26 gives next steps for command failures, not for `status` issues. No requirement was added.

## Also fixed
- `status_condition_descriptors.go`: the two arms that repeated `unexpectedStatusConditionDescriptor` now call it.
- `engine_primary_root.go`: three copies of the failure batch folded into one helper; `remoteReadDeniedObservationBatch`, which lost its only caller, is removed.
- A drive whose shared folders could not be set up no longer counts as two selected drives.

## Not fixed
- `internal/multisync/report.go:177`: `WatchStartupError.Error` says "%d drives could not start syncing" and counts shared folders as drives. Fix: the same split as the run-once sentence (out of scope: split only).
- `internal/sync/engine_mount_root.go:94`: the Info line "Shared folder change tracking is unavailable" is logged when the folder delta answers 404 for a folder that is gone. Fix: log the fallback only after the listing succeeds, or name the 404.
- `internal/cli/sync_render.go` (`selectionTally`): failures without a mount ID merge under one key. Production never builds them (`sync_runtime.go:100` always sets the identity). Fix if that changes: key them by selection index.

## Packages the reviewer should test
`./internal/sync/...` under `-race` in full (I ran it whole without race and only the touched tests with race), `./e2e` live (the CI fixture's 404 shared folder is the proof).


## Second pass

Commit 5ad65eb2 on a6f6fbf3 (the same one commit, amended). Per item, the failing message seen on df8f4f91 before the fix:

1. Removing the shortcut ends the report. The review's chain is right as stated: the new final-drain tests saw `Left:{StillOpen:1}` as the only hold, from the root row, which the release never touched. Where it falls short: its fix (subtract the count) alone would leave the root row stored, so `status` of a blocked drain would keep saying "Shared folder gone ... remove its shortcut". Changed both the stored row and the count: after a release over an unreadable folder, `forgetSharedFolderReadFailure` (engine_shortcut_release.go, called from `recordReleaseKept`) reconciles the remote observation's own batch (`newRemoteObservationFindingsBatch`) empty, then calls `readIssuesTheRunLeft` again. I used the observation's constructor rather than adding the root types to the release's batch, because sync-engine.md forbids assembling overlapping managed batch shapes ad hoc. Gated by `sharedFolderUnreadable`, now through `issueTypeIsRootReadFailure`. Failing messages: (a) and (b) "nothing holds the tree: &{... Left:{Failed:0 WillRetry:0 StillOpen:1} ...}"; (c) "expected LeftWork{} actual StillOpen:1", reasons held "1 still not synced", the store held "remote_read_denied /" or "remote_root_not_found /". (d): the whole `./internal/sync/...` tree passes. In the kept-files case `status` shows the kept change as "Kept in a removed shared folder" and no longer "Shared folder gone". Consequence to note: a one-shot run whose final drain keeps a change it cannot upload now exits 0 (removed_folder_kept leaves no file unsynced by design; the root issue was what made it exit 1). multisync: the "kept changes" case no longer carries `UnsyncedIssues: 1`, plus a clean case for a release over an unreadable folder that kept nothing. Texts: the descriptor, troubleshooting.md:33 and cli.md:659-669 are now true; sync-engine.md "the drain can never finish" corrected and the new behaviour added.
2. Done: "1 drive", "N shared folders", "N drives and M shared folders", singular for 1; a result with no mount ID counts by selection index. Failing: "expected '1 drive could not start syncing; ...' actual '2 drives could not start syncing; ...'".
3. Done (test comment and sync-observation.md; also the subtest name "... an older store holds").
4. Done: `issueTypeIsRootReadFailure` with `TestIssueTypeIsRootReadFailure_EveryMemberIsDecided` (explicit groups, read against the source's member list).
5. Done; also `sync_test.go:86` ("1 of 2 selected drives failed or was skipped"). Failing: "expected '1 of 1 selected drive failed or was skipped' actual '... were skipped'". No other sentence beside it had the slip.
6. Done: 'onedrive-go drive remove --drive <drive>' in the descriptor, troubleshooting.md and both expectations.
7. Done; both cases passed at once (`TestRootReadOfASharedFolder_TheListingDecidesAfterTheFolderDeltaAnswersNotFound`).
8. Done: the guard skips the root issue when `errors.Is(err, errDeltaExpired)` and returns the error like any other failure. The test needs a `*graph.GraphError` with status 404: a bare `graph.ErrNotFound` is not a cursor rejection. Failing: "Expected error with "OneDrive change history expired" in chain but got nil."
9. Done for the failure tally (startup results and reports carry `SelectionIndex`). Failing: "expected '2 of 2 selected drives failed or were skipped' actual '1 of 2 ...'". The left-work tally keeps its mount-ID key: its input map is keyed by mount ID.

Smaller points: issue_types.go and sync-observation.md name the condition family as the one switch that differs; test renamed to `TestBuildChildStatusDrive_ReadsTheStoreAsASharedFolder` (cli.md updated) with its twin `TestConfiguredDriveState_ReadsTheStoreAsADrive`; scripting.md paragraph rewrapped.

Outside the brief's list (deviation): `spec/design/sync-domain.md`, the governing doc of issue_types.go, said a kept change's root read issue "already counts"; corrected, and the new per-type function listed there. `sync-control-plane.md` cites the new WatchStartupError test. `engine_run_once.go`: one comment sentence.

### Also fixed
- `engine_shortcut_release_test.go`: the unreadable-folder test's comment and message no longer say the drain can never finish.
- `engine_shortcut_release.go`: `releaseMayRun`'s comment said the drain cannot finish.
- `issue_types.go`: `issueTypeLeavesFileUnsynced`'s comment leaned on the root issue counting.
- `sync_render.go`: `runOnceResultError`'s comment now names shared folders.

### Checks
build, vet, vet with all tags: pass. gofumpt: clean. `./internal/multisync/...`, `./internal/cli/...`: pass. Targeted `internal/sync` run: pass; whole `./internal/sync/...` tree (no race, behind the lock): pass. `-race` over the added and changed tests in `internal/sync` and `internal/multisync`: pass. Lint: 0 issues (after fixing goconst, rangeValCopy and gocyclo in this pass's own code). Cited tests and requirement ids of the commit's spec and docs files: all exist. `TestFindTestConventionViolations_RealRepoIsClean`: pass.

### Seen and not fixed
- `internal/cli/sync_render.go` (`runOnceLeftWorkError`): left work of a report without a mount ID merges under "" because `syncConsole.Summary` keys it by mount ID. Fix: key the summary by selection index (touches `sync_console.go`, which another branch owns).


## Step review of the second pass, fixed in review (2026-11-14, about 11:50 to 12:45 PST)

Commit `0ebdf9de`, one commit on main `2e465121` (amended from `5ad65eb2`, then rebased without a conflict). The review (`R-W38b-findings.md` in the scratch folder) had one must-fix and four should-fix items; I made the fixes myself.

- **Exit rule (decision mine, R-6.6.28)**: a final drain that keeps a change it could not upload exits 1, every run, until the change is moved away or deleted. Implemented as `LeftWork.NotUploaded`, read from the release's `Report.KeptNotUploaded.Count`, and counted inside the existing "still not synced" number (`still_open` in `--json`) through `LeftWork.NotSynced()`. `removed_folder_kept` stays out of `issueTypeLeavesFileUnsynced`: the count comes from the release's own report, not from the issue rows. The other kept kinds (entries sync does not manage, folders it cannot write to, entries it could not remove) hold the local copy and do not fail the run. A drain that keeps nothing exits 0 and the mount goes. No new JSON key.
- **The count of a result that names no mount**: product code can produce a startup result with no mount identity (a standalone drive whose resolution failed, and the one-result report of a failed process), so the id-less branch of the drive and shared-folder count is kept and has a test; it counts once by its selection index.
- **Commit message**: the exit rule is stated as the three cases above, and the sentence about `--json` says the kept changes are inside `still_open`.
- **`docs/troubleshooting.md`**: the entry for a removed shared folder says the run exits 1 while a change it could not upload is kept.
- Review notes with no fix: 7 to 11. Note 6 is a known limit, named in the PR body: when the final drain's release does not run (the hold is still in place), the root's read issue stays until the hold clears.

Checks on the rebased commit: `go build ./...`, `go vet ./...`, the tagged vet, the test-convention test, and the linter over `internal/sync`, `internal/cli` and `internal/multisync` (0 issues).

Owed at install: the CHANGELOG entry says a binary older than this one refuses a store that holds `remote_root_not_found` until `drive reset-sync-state`.



## Merged as #910 (`36f2e2b6`, 2026-11-14 about 14:26 PST)

- One commit `6528c8f3` on main at `dd0932f4`. The pre-pr gate passed on its first full run (27 minutes, lint 11 of them); pre-merge passed with no review threads once the PR's live jobs had run behind the dispatched nightly; post-merge passed and removed the `shared-folder-gone-says-so` worktree and its branch.
- Installed `36f2e2b6`; the watch client reports "Up to date, watching for changes", 19 files.
- Live proof: none on the test account, which holds no shared folder that can be taken away. The CI account's shared-folder tests in the PR's e2e job are the only live run of this code; the unit and scenario tests are the proof of the gone and unreadable cases.
- No store-generation bump. The store can now hold the issue type `remote_root_not_found`, which a binary older than this one refuses until `drive reset-sync-state`.
- A final drain that keeps a change it could not upload exits 1 (R-6.6.28): a behaviour change for scripts, named in `docs/scripting.md`.
- Free to start after this: the row that follows on the shared-folder condition files.
