# One-shot `sync` exits 1 when work is left: report

Commit `f5b735a4` on `feat/sync-exit-left-work` (not pushed), on main `27e76cc3`. First pass `a452bcbd`, second pass `77e771f7`; the third pass amended it.

## The rule as it is now
What a run left is counted on its last pass, by the engine, in one method: `Report.LeftWork()` (`internal/sync/report_left_work.go`).
- `Failed` = the last pass's failed actions not kept for retry (`LastPassFailed - LastPassKeptForRetry`; the second is a subset of the first, counted by the same `recordError`).
- `WillRetry` = the last pass's failures kept for retry + `LeftForNextSync` (its planned actions neither completed nor failed, plus `planFollowUp.leftWhenTheRunEnds`).
- `StillOpen` = `UnsyncedIssues` (observation issues `status` lists whose type leaves a file unsynced).
- Each file once; a file an earlier pass failed and a later pass finished, or no longer needed, is in none of them. Nil and dry-run reports return zero. `Total()` and `Any()` are the helpers.
- "Kept for retry" is one predicate over the result class, `failureKeptForRetry` (transient or scope-blocking); `emitActivity`'s `WillRetry` and the pass counter both read it.
- Every end of a pass fills its counters through `recordPassCounts` (the invariant branch through `Report.setPassCounts` directly), so each holds `LeftForNextSync = actions - succeeded - failed`.
- `Report.Failed` stays the sum over passes. Production code reads `Failed` only on a single pass's report: the follow-up decision, the pass log line and the perf collector. The `Report` doc comment says which fields add up and which are the last pass's state.
- The text summary, the JSON `summary` and the exit status print the method's numbers; the console tally is display only (counts and bytes by kind).
- Retry work is still not read from the store. Checked: every retry row after the last pass is an action that pass counted. The two pause tests assert rows == `LastPassFailed + LeftForNextSync` (2 == 1 + 1 after a refused trial; 0 after a pause waited out).
- Final drain: `Report.LocalTreeHolds()` answers whether the local tree may be removed. It holds for a missing report, a dry run, any left work, held deletes of local files, and files an interrupted removal set aside (`Report.InterruptedRemovals`, new, read with `UnsyncedIssues`). Held deletes of remote files alone do not hold. `classifyShortcutChildDrainReport` asks it and renders the counts; it no longer reads `Failed` or `Errors`.

## What changed in the third pass (per item)
1a. `failureKeptForRetry`; `runResults.keptForRetry`, counted in `recordError`, reset in `resetResultStats`; `emitActivity` reads the predicate.
1b. `engineFlow.recordPassCounts` and `Report.setPassCounts`/`passCounts` replace the five hand fills; both `dispatchInitialReadyActions` exits now set `LeftForNextSync`.
1c. `Report.LastPassFailed`, `Report.LastPassKeptForRetry`; `withPass` takes them from the last pass; `Report` doc comment.
1d. `LeftWork` type and `(*Report).LeftWork()`; `cli.leftWork` and `leftWorkCounts` are gone; `runOnceResultError`, `Summary` and `jsonSummary` take `map[string]syncengine.LeftWork`.
1e. `activityTally` lost `failedPaths`, `retrying`, `failureEvents`; `record` counts successes only.
1f. `TestReport_ProductCodeOutsideTheEngineReadsWhatARunLeftOnlyThroughItsMethods` parses every non-test Go file of the module outside `internal/sync` and fails on a selector of `LeftForNextSync`, `UnsyncedIssues`, `LastPassFailed`, `LastPassKeptForRetry` or `InterruptedRemovals`. Because of it, the `LocalTreeHolds` field is named `SetAsideFiles`.
2. `LocalTreeHolds`; `classifyShortcutChildDrainReport` + `localTreeHoldsDetail` ("child sync: 1 failed; 2 to be retried on the next sync; 3 local deletes held for confirmation"). Added beyond the asked rule: interrupted-removal leftovers block the drain (see the list below).
3. `store_inspect_moment_test.go` comment says "would show".
Docs and spec: R-6.6.19, R-6.6.28, R-6.6.33 (last pass, each file once, earlier-pass failures a later pass finished are in none); R-2.4.8 one sentence for the clean drain; `cli.md` (Verified By row, Console Output, Exit Status, JSON), `sync-engine.md` (pass counters, `LeftWork`, `InterruptedRemovals`, final drain), `sync-store.md` (`interruptedRemovalCount`), `sync-control-plane.md` one sentence for the clean drain; `docs/scripting.md` (summary row and fields).
Also: the CLI test comments that told history were reworded in the second pass; `TestSyncConsole_FailedCountsFilesNotFailureEvents` is removed (it tested the path sets).

## Tests (third pass)
Red first on `77e771f7`, where the old code could express it:
- `TestSyncConsole_LeftWorkComesFromTheReportAlone` (activities do not change the numbers): old printed "1 failed; 2 will be retried", exit "4 files".
- `TestExecutePreparedPlan_APassThatEndsAtItsFirstDispatchLeavesItsWork`: old LeftForNextSync 0, want 2.
- `TestClassifyShortcutChildDrainReport_CleanOnlyWhenTheLocalTreeMayGo`: old was clean for left work, issue, dry run, held local deletes, set-aside file, and blocked for an earlier-pass failure.
- `TestRunOnce_FinalDrainChildThatLeftWorkKeepsItsLocalCopy`: old acknowledged the drain (acks 1) in both subtests.

New with the new API (the old code has no last-pass fields to express them):
- `TestRunOnce_APauseThatRefusesItsTrialAgainLeavesEachFileOnce`: two uploads refused together; trial refused again; other never dispatched; Failed sum 3, LeftWork {WillRetry: 2}, rows 2.
- `TestRunOnce_APauseWaitedOutLeavesNothing`.
- `TestRunOnce_AFailureTheNextPassNoLongerNeedsIsNotLeft`: superseded move plus a 500 on a file the user then removes.
- `TestRunOnce_AFailureALaterPassFinishesIsNotLeft`.
- `TestRunOnce_AFailedCleanupCountsOnlyWhenTheLastPassFailedIt`: an SQLite trigger refuses the cleanup; the last pass's failure counts, an earlier one a later pass redid does not.
- `TestRunOnce_AConflictCopyAtThePassBoundIsLeftWork` (pin).
- `TestRunOnce_ReportsTheFilesAnInterruptedRemovalSetAside`.
- `TestFailureKeptForRetry_TheFeedAndThePassCountAgree`.
- the source-reading test (1f).
- `TestFinalDrain_RunsBidirectionalFullReconcileInOneShotAndWatch` (pin; passed on the old code).

Extended:
- `TestExecutePreparedPlan_AMismatchedPlanFailsEveryActionLoudly`: LeftWork {Failed: 2}.
- `TestRunOnce_ReportCountsUnresolvedWork`: LeftWork {1, 1, 1}.
- Last-pass fields in the CLI and multisync report literals that meant a failure of the last pass.

The seven new `TestRunOnce_` tests are in `runOnceScenarios()`.

## Item 2: other ways a file can exist only locally while `LocalTreeHolds` holds nothing
- A conflict copy made in the last pass: always counted. The pass bound counts `Renames` (pin test). Otherwise the run passes again (the copy succeeded, so the pass made progress) unless that pass failed, which counts. A canceled run fails at the final store read.
- Files the user's filters exclude (`included_dirs`, `ignored_dirs`, `ignored_paths`, `ignore_dotfiles`, `ignore_junk_files`) and symlinks without `follow_symlinks`: `RemoveTreeNoFollow` removes them with the tree (a symlink as the link, not its target). R-2.4.1 makes filters sync-only visibility. No requirement says what a release does with them. Not changed.
- FIFOs, sockets and device files (R-2.4.13 skips them silently): no content; the release removes the nodes. No requirement governs it.
- Entries under names sync reserves for itself, excluded whatever the filters: owned download partials (remote content), restore copies (`localtrash`; the trashed original stays in the trash), and a folder or special file a user named like a detach artifact (the scanner leaves those unreported). The release removes them. A synced file left at a spelling-rename temporary name is reported (`local_unsyncable_replacement`), so it counts as still open and holds the tree.
- A file an interrupted removal set aside (`local_interrupted_removal`, "the user's content, never deleted", P-4): before this pass the release removed it. It now holds the tree (`InterruptedRemovals`), tested in sync and multisync.
- A file written between the last pass's local scan and the release: no report can see it, and the release removes it.

## Checks
- `go build ./...` pass; `go vet ./...` pass; `go vet -tags 'e2e e2e_full' ./e2e/...` pass.
- `go test -count=1 ./internal/cli/... ./internal/multisync/... ./internal/devtool/...` pass.
- `go test -count=1 -timeout 60m ./internal/sync/...`: pass (310 s; 322 s again on the final tree after the lint fixes).
- `-race -run` on the added and changed sync tests (38.8 s), cli and multisync tests: pass.
- Lint (`./internal/sync/... ./internal/cli/... ./internal/multisync/...`, `B2-lint.log`): 0 issues. The first run found 5 in the new tests (a package-level map, a JSON-named constant compared with `Equal`, three unwrapped errors), all fixed. No e2e file changed in this pass, so `B2-lint-e2e.log` is the second pass's (0 issues).
- One `go test` run of mine overlapped another branch's `verify --stage pre-pr` (I checked `pgrep` in the same batch as the test instead of before it). Every later run waited.

## Observed, not fixed
- Release-time race (last item of the list above). What I need from you: whether the release should check the tree against the drain's baseline before removing it. That check belongs in the parent's release code (`removeShortcutRootProjection`), not in the report rule, so I did not add it.
- Filter-excluded files, special files and user entries under reserved names (list above). What I need from you: whether a release may remove them as it does now. No requirement governs it.

## e2e single-pass sites (second pass, unchanged)
`runSyncPass` / `ForDrive` / `AllDrives` (`e2e_test.go`) run one pass and require exit non-zero exactly when the summary (or, under `--quiet`, the command error) says it left work; each site's comment says what the pass may leave.
| Site | Became |
| --- | --- |
| `sync_e2e_test.go:671` `TestE2E_Sync_UploadOnly` | `runSyncPass`; may leave transient work |
| `output_validation_e2e_test.go:110` quiet pass | `runSyncPass` (stderr error pattern) |
| `output_validation_e2e_test.go:159` multi-drive pass | `runSyncPassAllDrives` |
| `sync_full_test.go:523` `runSyncWithMode` (directional conflict tests) | `runSyncPass` with the mode flag |
| `sync_full_test.go:1070` convergent edit | `runSyncPass`; must leave nothing ("Up to date") |
| `assertSyncLeavesLocalTreeStable` (`sync_e2e_test.go:339`) and `...ForDrive` (`shared_helpers_test.go:55`), 16 call sites | the pairing check instead of exit 0; the tree is the proof |
| `sync_demoted_full_e2e_test.go:77` conflict pass | `runSyncPass`; a conflict copy is not work left |
| `sync_kill_mid_transfer_full_e2e_test.go:90, 146` resume passes | `runSyncPass`; may leave transient work |
| `sync_kill_mid_transfer_full_e2e_test.go:100` idle pass | `runSyncPass`; must leave nothing |
| `sync_recovery_e2e_test.go:157, 258` idle passes | `runSyncPass`; must leave nothing |
| `sync_recovery_e2e_test.go:304` re-download pass | `runSyncPass`; may leave transient work |
| `sync_safety_full_e2e_test.go:121` pass after confirmed deletes | `runSyncPass`; may leave transient work |
| `sync_watch_full_test.go:523` pass after the daemon | `runSyncPass`; may leave transient work |
| `sync_shortcut_full_e2e_test.go:73` restart pass | `runSyncPassForDrive`; may leave transient work |
Dry-run sites are unchanged. The download-only pass in the read-only shared folder test now says in its comment that it prunes the held upload and its write block.

## Kept from the first pass
- Deviation: the summary text is "N still not synced (see 'onedrive-go status')", not "...from earlier runs": a run's own scan can find the issue (a reserved name on a first sync). Error texts are the requested ones.
- Also fixed: dead `mountReportsError` removed (its cases test `runOnceResultError`); the plan-invariant violation logged at Error, not Debug; the `engineInputs` comment; `runRunOnceStartup` inlined; the drop in `emitJSON` documented; R-6.8.15 names the command `onedrive-go`; `docs/scripting.md` no longer says sync's "Syncing ..." line goes to stderr; `TestE2E_Sync_NestedDeletion`'s setup predicate retries a left-work pass.

## Appendix A: issue-type verdicts (`issueTypeLeavesFileUnsynced`)
"yes" = leaves a file unsynced (fails a one-shot sync). Unsure rows marked (?). Only the types an observation issue can hold reach `UnsyncedIssues` (see "The rule as it is now"); the switch decides every member.
| Type | Verdict | Reason |
| --- | --- | --- |
| `deletes_held` | no | deliberate hold for `--confirm-deletes` |
| `local_interrupted_removal` | no (?) | a report about a leftover at an internal name sync never syncs; keeps no file from syncing |
| `invalid_filename` | yes | OneDrive refuses the name; the file is not uploaded |
| `path_too_long` | yes | OneDrive refuses the path |
| `file_too_large` | yes | over the upload limit |
| `hash_panic` | yes | content could not be read or checked |
| `case_collision` | yes | both names are held back |
| `local_read_denied` | yes | the file cannot be read to upload |
| `local_unsyncable_replacement` | yes | the replaced item cannot sync; the user's change is not applied |
| `local_mount_boundary` | yes (?) | the content of the crossed filesystem does not sync (default policy, `cross_filesystems` opts in) |
| `remote_name_twin` | yes | neither twin syncs |
| `remote_read_denied` | yes | remote content cannot be downloaded |
| `unauthorized` | yes | nothing syncs until sign-in |
| `remote_write_denied` | yes | local changes are not uploaded |
| `quota_exceeded` | yes | uploads wait for space |
| `rate_limited` | yes | work waits for the throttle |
| `local_permission_denied` | yes | downloads cannot be written |
| `disk_full` | yes | downloads wait for space |
| `service_outage` | yes | work waits for the service |
| `file_too_large_for_space` | yes | the download cannot fit |
| `local_name_too_long` | yes | the name cannot be stored here |
| `local_read_only` | yes | the filesystem refuses writes |
| `remote_folder_delete_unverifiable` | yes (?) | the local delete is not applied online; a safety refusal, but not a `--confirm-deletes` hold |
| `local_file_too_large` | yes | the filesystem cannot store it |
| `local_busy` | yes | waits for the file |
| `local_cross_device` | yes | the move cannot be applied |
| `unsupported_operation` | yes | OneDrive refuses the operation |
| `malware_detected` | yes | OneDrive refuses the download |
| `local_database_open` | yes | the download waits |
| `upload_verification_failed` | yes | the upload is retried |
| `download_integrity` | yes | the download is retried |
| `request_timeout`, `transient_conflict`, `transient_not_found`, `resource_locked`, `transfer_url_rejected` | yes (?) | a transient failure of work not done; no store keeps one as an observation issue, so this only decides the case |

## Appendix B: first-pass e2e audit (one-shot `sync` call sites; watch-only sites untouched)
direct = `"sync"` argv sites; helper = `runSyncThroughTransientFailures*`; conv = `requireSyncEventuallyConverges*`/`syncUntil`. The single-pass sites of the second pass are in the table above.
| File | direct / helper / conv | changed | why | already converging |
| --- | --- | --- | --- | --- |
| `e2e_test.go` | helpers | 3 helpers + 2 new | pairing in the transient helper; left work => non-zero in the converge loop; `runSyncUntilOnlyConditionsRemain`, `requireSyncLeftWork`; `syncRecordedRetryWork` removed | n/a |
| `cli_commands_e2e_test.go` | 0 / 8 / 0 | 2 | CON.txt and the >400-char path are scan conditions: steady state exits 1 | 8 |
| `edge_cases_e2e_test.go` | 0 / 4 / 0 | 1 | CON stays unsynced: exits 1 | 4 |
| `shared_items_e2e_test.go` | 2 / 1 / 0 | 2 (+1 pass added) | read-only folder: upload-only exits 1 twice; download-only pinned 0 | 1 |
| `sync_shortcut_full_e2e_test.go` | 3 / 1 / 0 | 1 (+1 pass) | read-only shortcut upload exits 1 twice | 1 + projection helper |
| `sync_safety_full_e2e_test.go` | 5 / 2 / 1 | 3 | delete-hold passes (2) and post-rebind hold pass pinned to 0; trash test already converges | 3 |
| `sync_hardening_full_e2e_test.go` | 10 / 13 / 9 | 1 | naming corpus: client 1 keeps rejected names and case variants, stable pass exits 1 | 22 |
| `sync_edge_cases_full_test.go` | 2 / 5 / 4 | 1 | NestedDeletion setup predicate retries a left-work pass | 9 |
| `sync_full_test.go` | 7 / 16 / 9 | 0 | deferred-by-mode and conflict copies exit 0 by rule; predicates need `Err==nil` | 25 |
| `sync_recovery_e2e_test.go` | 4 / 5 / 4 | 0 | convergence and idempotence only | 9 |
| `sync_demoted_full_e2e_test.go` | 3 / 4 / 0 | 0 | dry run and conflict copies exit 0 | 4 |
| `sync_kill_mid_transfer_full_e2e_test.go` | 5 / 0 / 0 | 0 | resume passes (single pass by design) | 0 |
| `orchestrator_e2e_test.go` | 1 / 2 / 1 | 0 | the direct pass already expects an error (drive without token) | 3 |
| `output_validation_e2e_test.go` | 2 / 1 / 0 | 0 | single pass by design, clean folders | 1 |
| `sync_e2e_test.go` | 1 / 0 / 1 | 0 (+1 helper) | fast-lane upload pass, clean folder; new stable-pass helper | 1 |
| `sync_shared_e2e_test.go` | 1 / 0 / 2 | 0 | download-only, clean | 2 |
| `drive_list_full_test.go` | 0 / 1 / 0 | 0 | clean upload | 1 |
| `sync_watch_full_test.go`, `sync_watch_e2e_test.go` | 1 + 1 / 0 / 0 | 0 | one post-watch upload pass; one competing-owner pass expects an error | 0 |
| `shared_items_helpers_test.go` | 0 / 0 / 1 | 0 | download-only projection | 1 |

Changed tests, what each expects: read-only shared folder: upload-only exits 1 twice with left-work text, download-only exits 0. Read-only shortcut: upload-only exits 1 twice. `TestE2E_Status_JSON_IssueDetails`, `TestE2E_InvalidFilenameRejection`: a pass leaving only the reserved name exits 1. `TestE2E_Status_IssueLifecycle`: exits 1 with only the long path left, then converges to 0 after the fix. `TestE2E_Sync_NamingCorpusRoundtrip`: client 1's stable pass exits 1 and changes nothing. Delete-hold (both holding passes) and rebind (post-rebind pass): exit 0 with held deletes. `TestE2E_Sync_NestedDeletion`: setup retries a left-work pass.

## Review (first pass, 2026-11-12 18:25, on `a452bcbd`)
- Read: the whole commit (product, tests, e2e, specs, docs), the report, and around it `engine_run_once.go` in full, admission and the one-shot result loop, `withPass`, `reconcileRuntimeState` and `PruneRetryWorkToCurrentActions`, the mount runner and `runOnceDrivesError`.
- Ran: sync (578 s), cli, multisync, devtool: all ok. Plan-id, backlog-id and principle-label checks against the base: clean.
- Accepted: exit 1 with no new code, the exhaustive issue-type switch and its table (the "(?)" rows answer "unsynced"), "N still not synced", JSON `incomplete` and `still_open`, the plan-invariant violation at Error, the "Also fixed" items.
- Checked and fine: a retry row with fewer than three attempts whose action is not yet due is held at admission and counted as left for the next sync; the early return of `dispatchInitialReadyActions` means nothing is left in the graph; a failed last read reaches the command as a drive error.
- Found: the count subtracts (`still open = unresolved - failed - will retry`), which undercounts (three bad names plus two fresh failures print 1 and total 3). The overlap it corrects is systematic: every live pass prunes retry work to the plan's actions, so the store's retry rows are always inside `Failed` and `LeftForNextSync`. The exact rule needs no arithmetic: still open = the observation issues that leave a file unsynced.
- Found: follow-up work a run ends on is counted nowhere, and the zero-action branch drops the plan's follow-up (the writer's "Not fixed" 1).
- Second pass (sent 18:25), ten items: the rule without subtraction and the tests of the fact it rests on; follow-up work counted when the run ends; failed files keyed by path; one read transaction for the status snapshot; one definition of the "Retrying" filter; the exported function with the unexported return type; the bench's handling of a sample that left work; the e2e single-pass sites; the last read's error path; the `Validates` line's place.
- Outside this change: the unused `history` parameter in `status_snapshot.go` (rides on the status model change); that a download-only pass prunes a standing write block, so `status` stops listing it until the next pass that uploads (existing behaviour; it explains the e2e expectation; not changed).
- Still mine: the whole sync suite on the final hash; the four live tests once `/me` answers; on the rebase, the exit-status order against the panic-boundary branch.

## Review (second pass, 2026-11-12 21:08, on `77e771f7`)

Read: the whole diff against `a452bcbd` (product, tests, e2e) and this report.

Accepted: the ten items, with two deviations. The follow-up work a run ends on is counted with the mode in mind (`Unfinished`). The one-transaction snapshot test uses a hook field (`betweenSnapshotReads`), which is this package's convention for test hooks.

Two defects, sent back for a third pass (`scratchpad/B2-msg3.txt`):

1. The counts mix passes. `Report.Failed` is summed over passes and the console tally's path sets and failure-event count live for the whole run, while `LeftForNextSync` and the plan are the last pass's. A file that reported "will retry" in pass 1 and is held without an event in pass 2 counts twice. A file that failed in pass 1 and has no action in pass 2 still makes the run exit 1. A failed bookkeeping action of an earlier pass that a later pass redid still counts. Decision: the engine counts what a run left on its last pass; one exported method on the report returns the three numbers and nothing else adds the fields up; the console tally is display only.
2. A removed shared folder's final drain reads "clean" from `Failed == 0` and no errors. A clean drain releases the shortcut root, which removes its whole local tree, and purges the child's state. A child run that left work without a failed action therefore loses local files that were never uploaded. This is on main. Decision: the drain is clean only when the run left nothing; a dry-run report is never clean; held deletions of local files block it; the classifier stops reading the summed `Failed` and `Errors`.

Also sent back: a pass's counters are filled in five places and two of them (both in `dispatchInitialReadyActions`) skip the left-for-next-sync formula: one function fills them. The history sentence in `store_inspect_moment_test.go`'s doc comment is reworded.

Open question for the third pass's report: what a release does with local files the user's filters exclude from sync. Not changed here; judged when the list arrives.

## Review (third pass, 2026-11-12 23:08, on `f5b735a4`)

Read: the whole product-code diff against the second pass, the four new test files, the requirement word-diffs (`non-functional.md` R-6.6.28, `sync.md` R-2.4.8), the report's item 2 and its "Observed, not fixed" section.

Accepted, all of it:

- What a run left is added up in one place. `Report.LeftWork()` and `Report.LocalTreeHolds()` are the only readers of the last-pass fields outside the engine, and `TestReport_ProductCodeOutsideTheEngineReadsWhatARunLeftOnlyThroughItsMethods` holds that by reading the source. The console summary, the JSON summary, the exit status and the final-drain classifier all take the same value.
- The counts describe the last pass, each file once (decision (h)). The pass counts are set from five places through one function; `LeftForNextSync` is the planned actions minus the completed and the failed, never negative. A failure an earlier pass had and a later pass redid is not left; a pause waited out leaves nothing; a refused trial leaves each file once and matches the retry rows in the store.
- One predicate, `failureKeptForRetry`, decides "will retry" for the activity feed and for the pass count, held equal for every failure class by a test.
- The final drain (decision (i)): clean only when `LocalTreeHolds().Any()` is false. A dry run, a held delete of a local file, work the last pass left and a file an interrupted removal set aside each keep the local copy, with the reason in the blocked detail. Sums over the run's passes decide nothing. Held deletes of files on OneDrive alone do not block, which is right: nothing local is lost.
- The plan-invariant branch has its test (`TestExecutePreparedPlan_AMismatchedPlanFailsEveryActionLoudly`): every action failed, none kept for retry, one error, logged at Error.
- A pass that ends at its first dispatch reports its planned work as left (`TestExecutePreparedPlan_APassThatEndsAtItsFirstDispatchLeavesItsWork`).

Checked myself: the seven new `TestRunOnce_` tests of `internal/sync` are in `runOnceScenarios()`; no plan ids, process words, principle labels or retired backlog ids in the diff (the eleven hits of "the plan" all mean the sync plan); the requirement texts say what the code does.

Corrected: item 2 says of FIFOs, sockets and device files that "the release removes the nodes". It does not. `RemoveTreeNoFollow` stops with an error at a symlink or a special file, after it has already removed the regular files it met before it, and the root then stays in cleanup-blocked. That is worse than the report says, not better, and it is not B2's to fix.

The "Observed, not fixed" entries (filter-excluded files, special files, user entries under reserved names: may a release remove them as it does now?) are answered by the planned change that moves the release into the child's final drain through the ordinary verified delete: nothing is removed that the baseline does not hold with matching content, leftovers block visibly, and the parent removes only an empty root. That change starts on this branch's tip. Nothing of this pass is sent back.

Suites on `f5b735a4`, non-race: `internal/sync` 396 s, `internal/cli`, `internal/multisync`, `internal/devtool` ok. Still mine before the PR merges: the rebase onto the stack (it shares `commandExitCode`, `runOnceResultError` and `engine_run_once.go` with the panic-boundary change), the gate, and the live tests named in the first-pass review.
