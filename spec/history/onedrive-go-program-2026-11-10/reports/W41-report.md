# Report: run report leftovers (fix/run-report-leftovers)

Worktree `~/src/onedrive-go-wt/run-report-leftovers`, branch `fix/run-report-leftovers`, made with `devtool worktree add` from origin/main `17564167` (start stage not run, as instructed). Commit: `032ec8f2` on `17564167`, not pushed. Not installed: after `git fetch origin main`, origin/main is `238bea94` (#943), so `merge-base --is-ancestor origin/main HEAD` fails; no rebase was made. DoR: `W41-dor.md`, written before any edit; it holds the facts the brief asked for and what the brief stated that no longer holds (no worktree or base existed; start stage not run; `status --json` prints a condition as `type`, the stored issue type, not as a key).

## What changed
- `internal/sync/engine_activity.go`: `Activity.ConditionKey`/`ConditionType`; `emitActivity` takes the decision; new `failureAwaitsUser` (actionable, retry work, named condition, not owned by a permission probe).
- `engine_results.go`, `engine_runtime_owners.go`, `engine_run_once_report.go`, `api_types.go`: the pass counts such failures (`Report.LastPassAwaitingUser`).
- `report_left_work.go`: `LeftWork.Failed` excludes them, `StillOpen` includes them.
- `engine_runtime_completion.go`: passes the decision to the feed; `local_mount_missing` in `retryDelayForCondition`.
- `issue_types.go`, `condition_keys.go`, `observation_findings.go`, `truth_status.go`, `engine_runtime_lifecycle.go`, `engine_runtime_permissions.go`, `watch_condition_log.go` (label): new member `issueLocalMountMissing`/`ConditionLocalMountMissing`, decided like the mount boundary everywhere.
- `scanner_unavailable.go`: a vanished crossed mount is recorded as `local_mount_missing`.
- `watch_condition_log.go`, `engine_watch_state.go`: the debug line only when the breakdown changed; a failed snapshot read logged once per distinct error (was dropped).
- `internal/cli/sync_console.go`, `sync_console_json.go`: once per path per run, stderr line in `status` words; JSON `status: still_open` + `condition`.
- `internal/cli/status_condition_descriptors.go`: "Disk not mounted" descriptor.
- Outside the brief's files: `internal/devtool/verify_stress_members.go` (two new `TestWatch...` tests declared as stress non-members, the source rule requires it); `spec/requirements/sync.md` (R-2.10.71) and `non-functional.md` (R-6.6.33) so the requirement text says what the run now reports; `engine_run_once_left_test.go` (an existing test encoded "1 failed" for the refusal); `internal/cli/sync_left_work_test.go` (the test helper now takes the activity by pointer, for the linter).
- Docs: `docs/scripting.md`, `spec/design/cli.md`, `sync-engine.md`, `sync-observation.md`, `config.md`.

## Tests added (each seen failing first)
- `TestWatchConditionSummary_DebugLineOnlyWhenTheBreakdownChanges`: same conditions write nothing; a moved count writes one debug line (was 2, then 5).
- `TestWatchConditionSummary_AFailedReadIsLoggedOnce`: a failed read logged once over three ticks (was 0).
- `TestBuildSyncStateInfo_AMountPointNotMountedSaysWhatClearsIt` (cli): title, reason, action, no `cross_filesystems` (was the unexpected-issue text).
- `TestRunOnce_AVanishedCrossedMountIsMissingNotABoundary`: recorded as `local_mount_missing`, an old `local_mount_boundary` row replaced, nothing deleted online (was `local_mount_boundary`).
- `TestRunOnce_MountMissingClearsWhenTheScanNoLongerFindsIt`: cleared by the next full scan; a delete under it is ordinary (failed on the type).
- `TestRunOnce_ARefusalOnlyTheUserCanClearIsLeftAsItsCondition`: first refusal and retry carry the condition, `LeftWork{StillOpen:1}` (was `Failed:1`, no condition).
- `TestFailureAwaitsUser_TheFeedAndThePassCountAgree`: feed and count agree; probe-owned denial, transient and conditionless failures excluded.
- `TestSyncConsole_AConditionTheUserMustClearIsSaidOnceInStatusWords` (cli): one stderr line in status words, "1 still not synced", JSON `still_open` + `condition`, summary `failed 0` (was "Failed to …" twice, "1 failed").

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test ./internal/cli/...`: pass; with `-race` (lock): pass.
- `go test ./internal/sync/...` (lock, 60m): pass (450 s).
- `-race` (lock) on every test of the 14 changed sync test files in one pattern: pass; `TestReverseDispatch_RunOnceScenariosPass/` the 5 scenarios touched: pass.
- Source rules: `go test -v -run RealRepoIsClean ./internal/devtool/` 4 PASS; `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`, `TestReverseDispatchTable_ListsEveryRunOnceScenario`, every `EveryMember`/`EveryKey`/`EveryDeclared` table, `TestReport_ProductCodeOutsideTheEngine…`: pass.
- `verify lint` (lock): first run 1 issue (gocritic hugeParam: `Activity` grew to 128 bytes, the test helper `mountActivity` took it by value; it now takes a pointer, `sync_console_test.go`, `sync_left_work_test.go`); CLI tests plain and `-race` rerun: pass; second `verify lint`: 0 issues, pass (`W41-lint.log`).
- Design-doc test citations checked by hand: all six cited names exist.

## Live proof (test account, isolated instance, branch binary; lock held)
Folder path of 397 characters (under the scanner's 400, over OneDrive's limit). First sync: exit 1; three ancestor folders created; stderr `Can't create folder <path>: Path too long. The full path is longer than OneDrive allows: 400 characters, and in practice a few dozen fewer. Shorten the path by renaming files or folders.` plus the engine's own `Warning: Not syncing <path>: … HTTP 400 pathIsTooLong …`; summary `Done in 8.1 s: created 3 folders; 1 still not synced (see 'onedrive-go status').` Second sync 35 s later (row due, retried): exit 1; stdout `Done in 3.8 s; 1 still not synced (see 'onedrive-go status').`; stderr the same one condition line and the exit error, no warning. Third sync at once (row not due): `Done in 2.1 s; 1 will be retried on the next sync.` (see Not fixed 1). `status`: `Path too long: 1 item` with its action and the path. Proof folder removed online, private tree removed. The data lived in a private sync folder under the scratchpad, not inside `~/tommi`, because `~/tommi` is the running watch client's own folder and a second client there would sync the same files twice; the account was the test account.

## Deviations
- The permission-probe-owned denials (local read/write denied, remote write denied when the probe answers) keep "failed": their condition is known only after the probe (DoR item 12), as decision (b) allows.
- JSON activity status `still_open` added: no existing value agrees with the summary count the file is in (DoR facts).
- The descriptor's action states that after `drive rebind-root` the next sync holds the deletes for confirmation (from `drive rebind-root --help`), so it does not read as a safe "forget".

## Also fixed
- `watch_condition_log.go:15`: a failed `ReadDriveStatusSnapshot` was dropped silently; now logged once per distinct error (test above).

## Not fixed (seen)
1. `internal/sync/engine_run_once_report.go:83` (`LeftForNextSync`) / `report_left_work.go:62`: a planned action held because its retry row is not due yet, whose row carries a condition only the user can clear, is counted "will be retried on the next sync" while `status` lists the condition (live run 3). Fix: count planned actions held for a row with a non-empty, unblocked `condition_type` as awaiting the user. Not done: the item A product change was at the brief's 150-line bound.
2. `internal/sync/engine_runtime_completion.go:640`: the first refusal prints two lines, the engine's `Warning: Not syncing …` (R-2.10.71's log line, raw Graph text) and the console's condition line. Fix: decide whether R-2.10.71's log line drops to Info/Debug now that the console says it.
3. `internal/sync/engine_runtime_completion.go:41-42` / `engine_runtime_permissions.go:57-103`: permission denials are reported as "Failed to …" and counted as failed although `status` lists them under a condition. Fix: emit the activity after `maybeHandlePermissionFailure` with the probe's evidence condition.
4. `internal/sync/watch_condition_log.go:191` (`conditionLabel`): every condition is counted in "file(s)", also folder conditions (mount boundary, mount missing, a read-denied folder). Fix: "item(s)", or a noun per condition.
5. `internal/cli/sync_console.go:627` (`write`): a failed console write is dropped; out of scope by the brief (another change's).

Count: 5.

## Unfinished
None. Packages a reviewer may want to test beyond these: `internal/multisync` reads `LocalTreeHolds.Reasons` (unchanged code path, `StillOpen` now larger); not run here.

## Fix round

Commit `018bb0ce` (032ec8f2 amended; still one commit on `17564167`, not rebased, not pushed, not installed: the branch does not contain origin/main). Findings from `R-W41-findings.md`.

### Blocking (1): the "Disk not mounted" action
- New action, verbatim: "Mount the disk again; that clears it and loses nothing. If the disk is gone for good, copy what you need from this folder on OneDrive first: after 'onedrive-go drive rebind-root', sync deletes the folder's content on OneDrive too, holding the deletes for confirmation only when they are many." Title and reason unchanged.
- Pinned by `TestRunOnce_ARebindAfterADiskIsGoneDeletesItsContentOnOneDrive` (sync; real planner, `ClearSyncRootIdentity` then `RunOnce`): 3 files: every file deleted on OneDrive, 0 held; `deleteSafetyMinItems+1` (51) files: 0 deleted, 51 held, then deleted after `ConfirmDeletes`. Before the rebind nothing is deleted in either. Text pinned by `TestBuildSyncStateInfo_AMountPointNotMountedSaysWhatClearsIt` (seen failing on the old text). Shared setup `syncedCrossedDiskNowUnmounted` now also serves `TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted`.
- The scan's warning for a vanished mount (`vanishedMountSkippedItem` detail, `scanner_unavailable.go`) also said only "forgets recorded mount points"; it now adds "and sync then deletes the folder's content on OneDrive too" (asserted in the same test, seen failing first). `docs/troubleshooting.md` has the row and its rebind section no longer promises a hold for every missing file.
- Rebind, planner and hold behaviour unchanged.

### Candidate for a later change: a rebind after a vanished mount
- Yes: a rebind after a vanished crossed mount deletes the OneDrive copies with no confirmation whenever the deletes stay at or under the hold's threshold, max(50, 30 % of the synced items). Branch: the test above. Base `17564167`, checked by test (the same scenario in a `git archive` copy under the scratchpad, removed afterwards): 3 files: before rebind 0 deletes, after rebind 3 remote deletes, 0 held; 51 files: after rebind 0 deletes, 51 held. Main behaves the same; this commit did not cause it.
- Fix idea: hold the deletes under the mount points a rebind forgot until `sync --confirm-deletes`, or have `drive rebind-root` list them before confirming. The text could then promise a hold.
- The same false promise is in two files this commit does not touch: `internal/cli/drive_rebind_root.go:28-30` (`--help`: "the next sync holds those deletes for confirmation") and `docs/safety.md:219-220` ("missing files are held for confirmation rather than deleted on OneDrive"). Fix: the troubleshooting wording, or the behaviour above.

### Should-fix
- 2: `failureAwaitsUser` excludes `upload_verification_failed` and `download_integrity` (test cases seen failing first). They report as before the commit: "Failed to ..." line, JSON `failed` without `condition`, counted in `failed`. The `jq` example also selects `still_open`. The other actionable kinds the review lists stay `still_open`: each one's status action names something the user does.
- 3: the console keeps, per (mount, path), the condition it last said and drops it on any activity of the path that names no condition. A path that synced and is refused again is said again, in text and JSON (`TestSyncConsole_AConditionIsSaidAgainOnceItsPathSyncedInBetween`, seen failing first). The map holds only paths whose last activity was a refusal. `docs/scripting.md` and `cli.md` say what this means under `--watch`.
- 4: wording narrowed, code unchanged. R-6.6.33, `docs/scripting.md` and `cli.md` now say the summary counts a refused file in `still_open` when the last pass refused it, and in `will_retry` when that pass only held it until its retry was due. `TestRunOnce_ARefusalOnlyTheUserCanClearIsLeftAsItsCondition` now asserts `LeftWork{WillRetry: 1}` for such a run (it passed from the start: behaviour pinned, not changed). The within-one-run form was not reproduced. Code fix left; see Left.
- 5: added the `ActionUpload` 403 case (expects not awaiting). Mutation (exclusion removed) fails it; restored.
- 6: the engine's "Not syncing <path>" line is logged at INFO when the activity feed names the condition (`Engine.activityNamesCondition`), WARN otherwise. The default console (NOTICE) prints the condition once; the log file (INFO) keeps the line. Asserted in the ARefusal test (seen failing: WARN). R-2.10.71, `sync-store.md` and `sync-engine.md` say so.
- 7: "Disk not mounted" row in `docs/troubleshooting.md`, with the finding-1 text.

### Notes
- Fixed: 9 (R-2.10.71: "a denial a permission probe owns"; also "once per path per run" became "when the condition starts"); 10 (R-6.6.28 and `sync-engine.md`: "failed, still open or left"); 11 (history wording in the five new test comments); 12 (the two mount conditions count "folder(s)"; mount-missing label "whose disk is not mounted"; `TestConditionLabel_AMountPointIsCountedAsAFolder`, seen failing first); 13 (a failed condition read warns once per streak, repeats at DEBUG, recovery once at INFO "reading the sync conditions for the watch summary works again"; test extended, seen failing first).
- 8 and 14-17: information only; nothing to change.

### User-visible differences from 032ec8f2
- Status "Disk not mounted" action (above); the vanished-mount scan warning's added clause; troubleshooting row and rebind paragraph.
- Integrity failures: back to "Failed to ..." text, JSON activity `status: failed` without `condition`, counted in summary `failed` (not `still_open`).
- A refused path is said again (text line, JSON `still_open` line) after the path's action ended another way; a watch can now show it twice in one session.
- "Not syncing <path>: ..." moves from WARN to INFO when sync's console reports the condition, so the default console no longer shows it. The log file keeps it.
- Watch: "N folder(s) whose disk is not mounted" (was "file(s) on a disk inside the sync folder that is not mounted"); "N folder(s) on another filesystem inside the sync folder" (was "file(s)"). Failed condition read: WARN once, then DEBUG, then INFO on recovery (was DEBUG once per distinct error).
- No JSON field added or removed. No exit code changed.

### Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass (vet rerun after the last edit: pass).
- `go test ./internal/cli/...`: pass; `-race` (lock): pass.
- `go test ./internal/sync/...` (lock): pass (404 s). `-race` (lock) on every test of the five changed sync test files plus `engine_activity_test.go`: pass. Label and watch-summary tests rerun after the last edit: pass.
- Source rules: `RealRepoIsClean` 4 PASS, `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` PASS. Sync `ListsEvery|Table|Governs|Exhaustive|Registered`, `Every`, `TestReport_ProductCodeOutsideTheEngine…`: pass. `TestReverseDispatch_RunOnceScenariosPass/` for the three touched scenarios: pass. The new RunOnce test is in `runOnceScenarios()`.
- `verify lint` (lock): first run 1 issue (goconst: a "folder" literal; now named console-noun constants). Second run: 0 issues, pass. Log: scratchpad `w41fix/W41F-lint2.log`. It was not written to `reports/`, which would have added an untracked file to this repo.
- Test names cited in spec files checked by hand: all exist.

### Left
- Finding 4's code fix: count a planned action that a pass holds for an unblocked retry row whose condition awaits the user as still open, not will-retry (`engine_runtime_admission.go:174`, `engine_run_once_report.go:83`). Needs a row-side predicate that agrees with `failureAwaitsUser` (the probe-owned and integrity rows also carry a condition), so it is bigger than the wording fix.
- `drive_rebind_root.go:28-30` and `docs/safety.md:219-220`: the false hold promise (candidate above).
- Note 12 remainder: other folder-level conditions (a read-denied folder) are still counted as "file(s)" (`watch_condition_log.go`, `conditionLabel`).
- The console's condition map keeps the entry of a path that never has another activity (for example the user deletes the refused folder). The map is bounded by the refused paths and is freed when the watch ends.
- Earlier "Not fixed" 3 and 5 unchanged.


## Shipped

Merged as #945 (squash `89c388f6`, 2026-11-17 14:40 PST); installed from main at 14:47 PST (`onedrive-go --version` prints 89c388f6), replacing the branch build of `fix/outer-silent-failures` at `a665e13c`. The worktree is removed.

### Rebase

- Rebased cleanly onto `6421bf98` (#943, #944). The commit prints no count of its own in `internal/cli` product code: its one count line, "N still not synced", is main's and passes the count-wording source check. No assertion needed main's new wording.
- Discovery, source-rule and stress-declaration tests passed before and after the fixes.

### Fixes of the second review's three should-fix items, test first

1. "Disk not mounted": the status action now says that `drive rebind-root` forgets every disk listed that is not mounted and that sync then deletes each such folder's content, and asks to mount every disk still in use before a rebind; the scan's warning and the troubleshooting row say the same. The rebind test gained a two-disk case (4 files deleted, 0 held). Text assertions seen failing first.
2. Remount: `TestRunOnce_ADiskMountedAgainLosesNothing` and `TestRunOnce_AnEditOnOneDriveWhileItsDiskIsNotMountedIsDownloadedOnceItIsBack` (forgetting the recorded mount point stands in for the disk's own device). They pass on the unchanged behaviour and fail with the vanished-mount check disabled.
3. A condition that starts again with no activity between: the engine marks a refusal as the condition starting (`Activity.ConditionStarts`, read from the retry ledger before the failure is recorded); the console prints every such refusal, and the engine logs INFO only when the feed says the start, WARN otherwise. Tests `TestRunOnce_ARefusalAfterItsRetryWorkWasDroppedStartsTheConditionAgain` and `TestSyncConsole_AConditionThatStartsAgainWithNoActivityBetweenIsSaidAgain`, seen failing first.

Also: the corrected sentences for `drive rebind-root --help` and `docs/safety.md`, the same in the troubleshooting rebind section, the rebind row of `spec/design/cli.md` (it also forgets every recorded mount point), R-2.10.71, `cli.md`, `sync-engine.md` and `docs/scripting.md` on when a condition is said. The gate's output-boundary check refused `slog.String("error", err.Error())` in the watch summary's failed read: now `slog.Any`.

### Proof and gates

- Live, test account, private tree, branch build: 397-character folder path. Run 1: exit 1, three folders created, one "Can't create folder …: Path too long" line, "1 still not synced", no warning. Run 2 (35 s, retry due): the same line once, "1 still not synced". Run 3 (at once): "1 will be retried on the next sync". `status`: "Path too long". Proof folder and tree removed.
- Full-tier e2e, one at a time behind the lock: `TestE2E_Status_IssueLifecycle`, `TestE2E_Status_JSON_IssueDetails`, `TestE2E_Edge_InvalidFilenameRejection`, `TestE2E_Sync_NamingCorpusRoundtrip`: pass.
- Start stage: nightly on main green, nothing claimed. Pre-pr: first run failed on the output-boundary check (fixed), second passed. Pre-merge passed; post-merge passed.
- New plan row W101: after a mount vanished, a rebind makes the next sync delete the OneDrive content of every unmounted disk with no confirmation below the hold threshold.
