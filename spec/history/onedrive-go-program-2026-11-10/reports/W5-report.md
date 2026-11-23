# W5 report: pending-create records end once their file can no longer receive the upload
Commit `d75982ef` on `fix/pending-create-lifetime` (one commit on `22e88c0b`), not pushed. 30 files.

## What changed
- graph `item_create.go`: `CreatedItemError.Removed`; `removeCreatedPlaceholder` returns the fact and never removes after `ErrUploadCompletedWithoutItem`.
- driveops `transfer_session.go`: `removePlaceholder` returns the fact; a failed hook returns one `CreatedItemError{Removed}` (not wrapped again by `persistedSessionUpload`); `recordItemOfFailedSession` skips a removed item. `transfer_verify.go`: `settleUncertainFinalFragment` returns `CreatedItemError{Removed}`; the lookup failure after a completion without item keeps the create mark (`keepCreateMark`, `outcomeUnknownAfter`).
- sync `store_pending_creates.go`: lifetime comment block; `ForgetRemovedPlaceholder`; `prunePendingCreates` (was `pruneAbandonedCreateIntents`) plus `dropRecordsOfSettledEmptyPlaceholdersTx`; `dropGoneCreateIntentsAt`; `dropPendingCreatesSettledByTx`; `placeholderSizeUnknown`; intent waits on an unknown-size file; `followAdoptedPlaceholderTx` moves the row on a rename.
- sync `store_write_baseline.go`: `applyOutcomeTx` (occupant read, apply, ledger, settled records; before the crash point). `store_write_observation.go`: new decision in both switches. `engine_current_plan.go`: calls `prunePendingCreates`.
- sync `executor_transfer.go`: detached bounded intent clear after success; `forgetRemovedPlaceholder`; `finishIntoPlaceholder` uses `isEmptyLiveFile` and returns a failed read as that read's failure.
- sync `observation_remote_batch.go` (`pollStartedAt`, `pollOnly`), `observer_remote.go` (stamp before the first page, poll-only batch for a zero-event poll, recovery batch stamped), `engine_observation_postprocess.go` (poll-only branch, drop after apply), new `engine_watch_create_attempts.go` (`watchCreateAttempts`, governed by `engine_watch*.go`), `engine_watch_loop.go`/`engine_watch_state.go` (dispatch and completion hooks).
- Tests: graph `item_create_test.go`, `upload_test.go` (fragment knob, `serveFragment`); driveops `transfer_create_test.go`, `transfer_manager_test.go`; sync new `pending_create_lifetime_test.go`, `observer_remote_test.go`, `pending_creates_test.go` (uploader lookups/removal), `scenario_remote_test.go` (`hashlessEmptyFiles`), `dispatch_order_test.go` (3 RunOnce scenarios).
- Docs: sync-store.md, sync-engine.md, sync-execution.md, sync-observation.md, drive-transfers.md, graph-client.md, R-5.6.15.

## Tests added (each seen red first; pins marked)
- (a) `TestUploadNewFile_ZeroByteCreateLeavesNoPendingRow`, `..._ZeroByteCreateClaimedByTheObserverLeavesNoPendingRow`, `TestRunOnce_ClaimedZeroByteFileThenRemoteMtimeChangeIsARemoteChange`, `TestCommitMutation_DeleteOutcomeDropsTheRecordOfItsItem`, `TestCommitMutation_UploadWithContentKeepsTheRecordForItsEcho` (pin).
- (b) `TestUpload_FailedContentStepReportsWhetherItRemovedItsEmptyItem`, `TestUpload_CompletedUploadIsNotRemovedAsAPlaceholder`, `TestRecordItemOfFailedSession_SkipsARemovedItem`, `TestUploadFile_RemovedPlaceholderIsReportedRemoved`, `TestUploadNewFile_RemovedPlaceholderLeavesNoRecord`.
- (c) `TestRunOnce_AdoptedPlaceholderWithAnEmptyLocalFileLosesItsRecord`, `TestPrunePendingCreates_DropsTheRecordOfASettledEmptyPlaceholder`. (d) `TestCommitObservation_UnknownSizeOfAPendingItemIsNotAStaleEcho`, `..._IsNotAdopted` (record and intent).
- (e) `TestCommitObservation_AdoptedPlaceholderRenamedWhileEmptyTakesItsRecordAlong`; `..._PlaceholderMovedBeforeItWasAdoptedLosesItsRecord` (pin, was untested).
- (f) `TestSettleUploadedItem_LookupFailureKeepsTheCreateMark`, `TestExecuteUpload_LookupFailureAfterACreateIsNotAGoneParent`, `TestRunOnce_LookupFailureAfterACreateKeepsItsParentInRemoteTruth` (sync tests red with the old driveops line restored).
- (g) `TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll` (zero-event and with events; counts store transactions and replans), `TestWatch_PollThatMayHaveMissedTheCreateKeepsTheIntent` (poll before the end; retry in flight), `TestWatch_EveryPollHandsOverTheTimeItBegan`; `TestWatch_ZeroEvents_NoTokenAdvance` updated.
- (h) `TestFinishIntoPlaceholder_RefusesAnItemThatIsNotAnEmptyLiveFile`; `TestFinishIntoPlaceholder_AReadThatFailsIsThatReadsFailure`.

## Lifetime table (17 rows; every row has an end, one exception below)
| Way | Code | Ends by / test |
|---|---|---|
| W intent written | `uploadNewFile` `RecordPendingCreateIntent` | rows E1-E9 below; `TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly` |
| W item named (hook, failed session, retry claim) | `recordPendingCreate`, `recordItemOfFailedSession`, `claimLostCreate` | E1, E3-E6; `TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled` |
| W item named by observation's claim | `claimIntendedPlaceholderTx` | E1, E3-E5; `TestCommitObservation_IntentClaimsTheEmptyFileACrashLeftUnrecorded` |
| W row follows a rename while empty | `followAdoptedPlaceholderTx` | E1, E3, E4; `..._RenamedWhileEmptyTakesItsRecordAlong` |
| K adopted (row stays) | `adoptPlaceholderTx` | E1/E3/E4/E7; `TestCommitObservation_OwnPlaceholderIsAdoptedAsEmptySyncedContent` |
| K followed (row stays) | `followAdoptedPlaceholderTx` | same; `..._AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict` |
| K stale echo (row stays) | `placeholderStale` | E1 when content is observed; `..._PlaceholderStateSeenAfterTheCommitIsNotARemoteChange` |
| K unknown size (row stays) | `placeholderSizeUnknown`, claim waits | next known-size observation; `..._UnknownSizeOfAPendingItemIsNotAdopted` |
| E1 resolved (content or delete observed) | `processObservedItemWithPlaceholders` | `TestCommitObservation_PendingCreateIsDroppedOnceTheItemChanges` |
| E2 moved before adoption | `adoptPlaceholderTx` | `..._PlaceholderMovedBeforeItWasAdoptedLosesItsRecord` |
| E3 empty outcome committed / cleared after success | `dropPendingCreatesSettledByTx`, `clearPendingCreateIntent` | `..._ZeroByteCreateClaimedByTheObserver...`, `..._ZeroByteCreateLeavesNoPendingRow` |
| E4 outcome removes item from baseline | `dropPendingCreatesSettledByTx` | `TestCommitMutation_DeleteOutcomeDropsTheRecordOfItsItem` |
| E5 removed placeholder | `forgetRemovedPlaceholder` | `TestUploadNewFile_RemovedPlaceholderLeavesNoRecord` |
| E6 refused claim / nothing created | `claimIntendedPlaceholderTx`, `ClearPendingCreateIntent` | `..._IntentDoesNotClaimContentOrOutliveItsTTL`, `TestRunOnce_RefusedCreateWaitsOnTheDurableCurveAndClearsItsIntent` |
| E7 settled empty placeholder (prune) | `dropRecordsOfSettledEmptyPlaceholdersTx` | `TestPrunePendingCreates_DropsTheRecordOfASettledEmptyPlaceholder` |
| E8 expiry / file gone after remote-first pass (prune) | `dropAbandonedCreateIntentsTx` | `TestPruneAbandonedCreateIntents_DropsWhatNoCreateWillClaim` |
| E9 watch: poll after the attempt ended | `dropGoneCreateIntentsAfterPoll`, `dropGoneCreateIntentsAt` | `TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll` |
Finding: a row naming an item at a path the content filter hides or a shortcut root reserves, whose item stays empty, has no end while the path stays out of the scan (inert: nothing plans there). Not fixed: the prune deliberately keeps rows at unscanned paths; decision needed.

## Checks
- `go build ./...`, `go vet ./...`, tagged vet, `go test ./internal/graph/... ./internal/driveops/...`: pass. Lint (`W5-lint.log`): 0 issues.
- sync `-run 'PendingCreate|Placeholder|UploadNewFile|LostCreate|CommitObservation|CommitMutation'` plus every added/touched test, `TestWatch|RemoteObservationBatch|TestCrashRecovery`, `ListsEvery|Table|TestDetachedContexts`, reverse dispatch of the 3 new scenarios: pass. `-race` on all added tests: pass. Chaos seeds 2 and 9, crash seed 2: pass. Crash-point tests unchanged and passing. panicguard scan: pass.

## Live proof (e2e coverage)
- (1) `TestE2E_ZeroByteFileSync` (e2e/edge_cases_e2e_test.go) syncs a zero-byte file up and reads it back; no e2e test reads pending_creates or makes a remote mtime-only change. (2) `TestE2E_Sync_KillMidUploadResumesSession` (e2e/sync_kill_mid_transfer_full_e2e_test.go): kill, resume, one file, "Up to date"; it does not read pending_creates.

## Deviations
- (f) The final fragment's 200-without-body is a `FinalFragmentError` and already kept its mark; the lost mark was on the other branch (a completion without item reported unmarked, e.g. a fragment before the last). Same fix.
- (e) The row follows the item (a session already open is not shown refused after a rename); test named `..._TakesItsRecordAlong`. A path already holding a row keeps it.
- (d) also covers intents: a fresh intent at an unknown-size file is neither claimed nor dropped (it was dropped).
- (g) Observers run on `e.wallClock` (the engine's clock in production, not a test's manual clock); tests build poll batches. The 410 recovery batch is stamped too. Mount-root watches get no signal.

## Also fixed (beyond the Decision)
- `finishIntoPlaceholder` swallowed a failed read of the placeholder (now classified by its own answer; 404 falls through); missing test for the moved-before-adoption drop; `TestTransferManager_Upload_NilItem` asserts the typed unknown outcome.

## Not fixed
- `internal/sync/tx.go:57`: `perfTx.Rollback` records a transaction after a Commit (the deferred `finalizeTxRollback`), so `DBTransactionCount` counts each committed transaction twice. Fix: record only a real rollback (not `sql.ErrTxDone`).
- `internal/sync/engine_mount_root.go:341`: a mount-root watch skips zero-event polls and stamps no poll start, so a shared-folder engine keeps a gone file's fresh intent until restart or expiry. Fix: stamp `pollStartedAt` and send a poll-only batch.
- `internal/sync/scenario_remote_test.go:797`: the fake runs the create hook for a zero-byte create, which graph never does. Fix: skip it for size 0 and recheck chaos seeds (the chaos lost-answer fault detects the create through the hook).
- `internal/driveops/transfer_session.go:402`: session expiry decided with `time.Now()`. Fix: give `TransferManager` a clock.
- `internal/sync/executor_transfer.go:776`: `finishIntoPlaceholder` checks emptiness, not that the item still has the action's name and parent. Fix: compare name (NFC) and parent ID (resolve the `root` alias first).
- `internal/sync/store_pending_creates.go:601`: `freeBaselinePathForPlaceholderTx` drops an occupant's baseline row in observation but keeps that item's record (an outcome doing the same drops it). Fix: delete the record in the same transaction.
- `internal/sync/store_write_baseline.go:830`: `commitMove` of an adopted placeholder keeps its record at the old path until observation moves it. Fix: move the record with the baseline row.
- `internal/sync/store_pending_creates.go:723`: following an adopted placeholder needs its baseline row at the item's path, so after an online rename of a hashless empty placeholder its eTag/mtime are not followed. Fix: follow regardless of path.
- `internal/sync/store_pending_creates.go:448`: rows at unscanned paths have no end (finding above).

Reviewer: run all of `internal/sync`; also `internal/cli` and `internal/multisync` (use `graph.CreatedItemError` and the watch loop; not run here).

## Review (first pass, 2026-11-13 14:55, on `d75982ef`)

Second pass asked for. A read-only reviewer read the whole diff; its findings are in the session scratchpad (`R-W5-findings.md`). None of them is verified by me yet: each must-fix below starts with a test that fails on `d75982ef`, which proves or disproves the reading. The writer did not run the whole `./internal/sync/...` suite; the second pass does, and `-run 'TestCrashRecovery' ./internal/sync/`.

Must fix (tests first):

1. A record with no end (`store_pending_creates.go:448`, `:431`): a record that names an item stays forever when its path leaves the scan (the filter hides it, or it was never adopted). In `prunePendingCreates`, in the same transaction, delete the rows with `item_id <> ''` whose path the passed-in `scanned` function reports false. One RunOnce test where the filter hides an adopted placeholder and the cleanup ends its record; one store test for the unadopted case. The lifetime table in the design doc is corrected.
2. `finishIntoPlaceholder` (`executor_transfer.go:776-795`) checks neither the name nor the parent of the item it finishes into. Compare `nfcNormalize(item.Name)` with `filepath.Base(action.Path)` and the item's parent ID with `parentID` (the `root` alias resolved); on a mismatch fall through as "not finished". A case in `TestFinishIntoPlaceholder_RefusesAnItemThatIsNotAnEmptyLiveFile`.
3. Freeing the baseline path keeps the occupant's record (`store_pending_creates.go:617`, `freeBaselinePathForPlaceholderTx`): `DELETE FROM pending_creates WHERE item_id = ?` for the occupant, beside the baseline delete. Store test.
4. A move commit leaves the record at the old path (`store_write_baseline.go:830`, `store_pending_creates.go:480-482`): for `baselineMutationMove`, `UPDATE OR IGNORE pending_creates SET path = ? WHERE item_id = ?` in the same transaction, and the descendants are re-pathed on a folder path move (`isFolderPathMove`) as `commitMove` does. A `TestCommitMutation_` case.
5. An online rename skips the eTag follow (`store_pending_creates.go:711-716`, `:723`): the moved-path branch returns before the baseline row follows the item's eTag and mtime. Follow them by item ID before returning, or disprove it with a scenario test (`hashlessEmptyFiles`, local content, the placeholder renamed online: one upload by ID, no conflict copy).
6. The test fake runs the create hook for zero-byte creates (`scenario_remote_test.go:796-800`): skip `onCreated` when `data == ""`, then run again the chaos seeds the report names.

Should fix:

7. After the `UPDATE OR IGNORE` at `store_pending_creates.go:712`: `DELETE FROM pending_creates WHERE item_id = ? AND path <> ?`, so an ignored update cannot leave two records for one item. A case in the rename test.
8. `endedAt` never ends in a mount-root engine (`engine_watch_create_attempts.go:36`, `58-75`, `120-125`), and the reviewer's verdict on the report's "Not fixed" entry for `engine_mount_root.go:341` is that it is a real fault of the same rule: mount-root batches carry no `pollStartedAt`, and a poll that saw no events sends nothing. Decision (mine): fixed here, not in a row of its own: mount-root polls are stamped and a zero-event poll sends a poll-only batch, so a create attempt ends the same way in every engine. A watch test in a mount-root engine. Stop and tell me if stamping changes what a mount-root engine does with a batch beyond ending create attempts.

Notes taken into the second pass:

9. The test for row (g) asserts neither "judged and kept" nor that exactly one record was dropped.
10. E7 (`store_pending_creates.go:448-462`) against an upload by ID in flight in watch mode: a watch test, or a guard.
11. `TestSessionUpload_RemovedPlaceholderClearsItsRecord` was planned and never written; the report says what replaced it, or it is written.
12. The report says cli and multisync use `graph.CreatedItemError`; the reviewer found they do not. The report says what is true.

Verdicts on the first pass's "Not fixed" entries:

- `internal/sync/tx.go:57` (a committed transaction is counted twice in `status --perf`): a real fault, outside this row. Plan row W34.
- `internal/sync/engine_mount_root.go:341`: a real fault; fixed in this row's second pass (should fix 8).
- `internal/driveops/transfer_session.go:402` (the expiry reads the wall clock): not a fault, a testability limit. Rider on W33, which opens that file.

After the second pass: rebase onto the stack tip, my own whole-package runs with and without the race detector, lint, then the stack. Live proof owed (the lost-answer create against the test account) when the service answers.

## Second pass (2026-11-13, on `91113542`)

Commit `17cf3b4e` (the first pass amended; one commit on `91113542`, which is not an ancestor of main's `bb0d76fd`), not pushed. 32 files.

### Conflicts resolved

None. `git rebase --onto 91113542 22e88c0b` applied the first-pass commit without a conflict; nothing was resolved by hand.

### Findings, test first

Every test below was run on the rebased tree before its fix (`W5-p2-red.log`, `W5-p2-red-runonce.log`, `W5-p2-red-mountroot.log` in the session scratchpad).

1. Record with no end. `TestPrunePendingCreates_DropsTheRecordOfAnItemAtAPathTheScanDoesNotVisit` (store, never-adopted item at a path `scanned` reports false): **failed** before (record kept). `TestRunOnce_PlaceholderAtAPathTheFilterHidesLosesItsRecord` (filter hides an adopted placeholder): **passed** before, so that half is disproved: observation adopted the placeholder, the plan made a cleanup of the hidden baseline row, and that outcome's commit dropped the record (seen in the test's log: 2 baseline entries, `cleanups=1`). Test kept, added to `runOnceScenarios()`. Fix: `dropRecordsAtUnscannedPathsTx` in `prunePendingCreates` (same transaction) deletes rows with an item ID whose path `scanned` reports false; intents there are kept (their TTL ends them).
2. `finishIntoPlaceholder` location. New cases in `TestFinishIntoPlaceholder_RefusesAnItemThatIsNotAnEmptyLiveFile` (now a table): `renamed online`, `moved to another folder`, `moved out of the drive root` **failed** before (finished, one upload by ID each); `still in its folder` and `still at the drive root` pass. Fix: `placeholderIsAtCreate`: NFC name equal to the create's base name, and parent ID equal to the create's parent; for the `root` alias (no item ID to compare) `remoteItemIsAt` places the item by its parent path, and only a location known to be elsewhere refuses. Mount-root engines never pass `root` (their top-level parent is the mount root's item ID), so the parent-path coordinate question does not arise. On a mismatch the 409 falls through to the folder listing as before.
3. Freed path keeps the occupant's record. `TestCommitObservation_APlaceholderTakingAFreedPathEndsTheRecordOfTheItemThatLeftIt`: **failed** before. Fix: `freeBaselinePathForPlaceholderTx` deletes the occupant's record beside its baseline row.
4. Move commit leaves the record behind. `TestCommitMutation_MoveOutcomeTakesTheRecordOfItsItemAlong`, subtests `the item itself` and `a folder above it` (an item record and an intent below the folder): both **failed** before. Fix: `applyOutcomeTx` reads `baselineBeforeOutcome` (occupant, and for a move the item's previous baseline path) before applying; `settlePendingCreatesTx` (renamed from `dropPendingCreatesSettledByTx`) calls `movePendingCreatesTx` for every move kind: the item's own row by item ID (`movePendingCreateOfItemTx`), and for a folder path move every row below the previous baseline path (rows whose new path already holds a row are deleted first, then `UPDATE OR IGNORE`, so a pathological overlap leaves a row rather than deleting a wrong one). Not a stored-format change: no column, no schema.
5. Online rename skips the eTag follow. `TestRunOnce_AdoptedHashlessPlaceholderRenamedOnlineIsFinishedWithoutAConflict` (`hashlessEmptyFiles`, local content, the adopted placeholder renamed online with a new eTag and mtime): **passed** before. **Disproved**: no conflict copy, `renamed.txt` holds the user's content in the placeholder's own item ID, sync invariants hold. Nothing changed in `followAdoptedPlaceholderTx`'s follow; test kept and added to `runOnceScenarios()`.
6. Fake runs the hook for zero-byte creates. `TestMoveScenarioRemote_ZeroByteCreateRunsNoCreateHook`: **failed** before (hook ran for the empty file). Fix: `uploadCreating` returns the created item at once for size 0, as `graph.UploadCreating` does (no hook, no content step to interrupt); a lost answer still applies. Chaos seeds rerun below.
7. Ignored `UPDATE OR IGNORE` leaves two rows. `TestCommitObservation_AdoptedPlaceholderRenamedOntoARecordedPathLeavesNoRecordBehind`: **failed** before (record left at the old path). Fix: `movePendingCreateOfItemTx` (`UPDATE OR IGNORE`, then `DELETE ... WHERE item_id = ? AND path <> ?`), used by `followAdoptedPlaceholderTx` and by the move commit.
8. Mount-root polls. `TestWatch_MountRootPollDropsTheFreshIntentOfAGoneFile` (a mount-root engine's real `watchMountRootRemote` goroutine; subtests `zero-event poll` and `poll with events`): both **failed** before (`pollStartedAt` zero, intent kept). Fact found while testing: a successful mount-root poll is never skipped. `executeMountRootObservation` always builds findings that manage the remote read-denied issue type (`newRemoteObservationFindingsBatch`), so `hasObservationFindings()` is true and `shouldSkipMountRootWatchBatch` returns false for every batch it builds; a zero-event poll already dispatches a whole batch with an apply ack. Fix: stamp `pollStartedAt` from `rt.deps.now()` before the poll. No poll-only batch was added: it would sit in a branch no successful poll reaches. Stop condition checked, not met: the batch's handling is unchanged except that `dropGoneCreateIntentsAfterPoll`, which already ran after every applied batch and returned at once on a zero time, now judges the paths whose attempt ended before the poll (a store transaction only when such a path exists). No replan, commit or ack changes.

Notes:

9. `TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll` now also asserts exactly one row dropped (count before minus one), an intent at a path no attempt of this process ended at is not judged, and the dropped path is forgotten. New `TestWatch_PollKeepsTheIntentOfAFileThatIsStillThere`: the poll judges (a store transaction), keeps the intent while `local_state` holds the file, keeps the path in memory, and a later poll drops it once the file is gone. Both passed before this pass's changes (pins of the first pass).
10. `TestWatch_ReplanWaitsWhileAnUploadRunsBeforeItPrunesPendingCreates`: an adopted placeholder whose local file reads empty (the E7 condition) with an upload by ID dispatched and running; a queued replan does not run (`canPrepareNow` is false), the replan stays pending, the record stays; after the action finishes, the same prune drops it. Passed before (pin). No guard added; the invariant is stated on `prunePendingCreates` and in sync-store.md / sync-engine.md.

### Corrected lifetime table

It lives here (replacing the first pass's 17-row table) and, as prose, in `spec/design/sync-store.md` (Pending creates) and the comment block of `store_pending_creates.go`. 21 rows, every row has an end; the first pass's "no end" finding is closed by E11.

| Way | Code | Ends by / test |
|---|---|---|
| W1 intent written | `uploadNewFile` `RecordPendingCreateIntent` | E1-E9; `TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly` |
| W2 item named (hook, failed session, retry claim) | `recordPendingCreate`, `recordItemOfFailedSession`, `claimLostCreate` | E1, E3-E6, E10-E12; `TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled` |
| W3 item named by observation's claim | `claimIntendedPlaceholderTx` | same; `TestCommitObservation_IntentClaimsTheEmptyFileACrashLeftUnrecorded` |
| W4 row follows an online rename while empty | `followAdoptedPlaceholderTx` → `movePendingCreateOfItemTx` | E12 on a held path; `..._RenamedWhileEmptyTakesItsRecordAlong` |
| W5 row moves with a committed move (item, or rows below a folder) | `settlePendingCreatesTx` → `movePendingCreatesTx` | E12 on a held path; `TestCommitMutation_MoveOutcomeTakesTheRecordOfItsItemAlong` |
| K1 adopted | `adoptPlaceholderTx` | E1/E3/E4/E7/E10/E11; `TestCommitObservation_OwnPlaceholderIsAdoptedAsEmptySyncedContent` |
| K2 followed | `followAdoptedPlaceholderTx` | same; `..._AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict` |
| K3 stale echo | `placeholderStale` | E1 when content is observed; `..._PlaceholderStateSeenAfterTheCommitIsNotARemoteChange` |
| K4 unknown size | `placeholderSizeUnknown` | next known-size observation; `..._UnknownSizeOfAPendingItemIsNotAdopted` |
| E1 content or delete observed | `processObservedItemWithPlaceholders` | `TestCommitObservation_PendingCreateIsDroppedOnceTheItemChanges` |
| E2 moved before adoption | `adoptPlaceholderTx` | `..._PlaceholderMovedBeforeItWasAdoptedLosesItsRecord` |
| E3 empty outcome / cleared after success | `settlePendingCreatesTx`, `clearPendingCreateIntent` | `..._ZeroByteCreateClaimedByTheObserverLeavesNoPendingRow`, `..._ZeroByteCreateLeavesNoPendingRow` |
| E4 outcome removes the item from the baseline | `settlePendingCreatesTx` | `TestCommitMutation_DeleteOutcomeDropsTheRecordOfItsItem` |
| E5 removed placeholder | `forgetRemovedPlaceholder` | `TestUploadNewFile_RemovedPlaceholderLeavesNoRecord` |
| E6 refused claim / nothing created | `claimIntendedPlaceholderTx`, `ClearPendingCreateIntent` | `..._IntentDoesNotClaimContentOrOutliveItsTTL`, `TestRunOnce_RefusedCreateWaitsOnTheDurableCurveAndClearsItsIntent` |
| E7 settled empty placeholder (prune) | `dropRecordsOfSettledEmptyPlaceholdersTx` | `TestPrunePendingCreates_DropsTheRecordOfASettledEmptyPlaceholder`; never with an upload running: `TestWatch_ReplanWaitsWhileAnUploadRunsBeforeItPrunesPendingCreates` |
| E8 expiry / file gone after a remote-first pass (prune) | `dropAbandonedCreateIntentsTx` | `TestPruneAbandonedCreateIntents_DropsWhatNoCreateWillClaim` |
| E9 watch: poll after the attempt ended (primary and mount-root) | `dropGoneCreateIntentsAfterPoll`, `dropGoneCreateIntentsAt` | `TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll`, `TestWatch_PollKeepsTheIntentOfAFileThatIsStillThere`, `TestWatch_MountRootPollDropsTheFreshIntentOfAGoneFile` |
| E10 a new placeholder takes the path the item left | `freeBaselinePathForPlaceholderTx` | `TestCommitObservation_APlaceholderTakingAFreedPathEndsTheRecordOfTheItemThatLeftIt` |
| E11 item at a path the scan does not visit (prune) | `dropRecordsAtUnscannedPathsTx` | `TestPrunePendingCreates_DropsTheRecordOfAnItemAtAPathTheScanDoesNotVisit`; adopted case through E4: `TestRunOnce_PlaceholderAtAPathTheFilterHidesLosesItsRecord` |
| E12 moved onto a path that holds a row | `movePendingCreateOfItemTx`, `movePendingCreatesTx` | `..._AdoptedPlaceholderRenamedOntoARecordedPathLeavesNoRecordBehind` |

Guard, not an end: `finishIntoPlaceholder` writes only into an empty live file at the create's own name and parent (`placeholderIsAtCreate`).

### Corrections to the first report

- Note 11: `TestSessionUpload_RemovedPlaceholderClearsItsRecord` was never written. What replaced it: driveops `TestUploadFile_RemovedPlaceholderIsReportedRemoved` (the session path whose record of the created item fails, and the final fragment that did not land, each with the removal working and refused) and sync `TestUploadNewFile_RemovedPlaceholderLeavesNoRecord` (the record ends when the cleanup reports the item removed).
- Note 12: wrong. `graph.CreatedItemError` is used only in `internal/graph`, `internal/driveops` and `internal/sync` (checked by grep over `internal`, `cmd`, `e2e`). `internal/cli` and `internal/multisync` reach the changed code only through driveops and sync (the watch loop is in sync).
- The first report's deviation "(g) Mount-root watches get no signal" and its "Not fixed" entries for `engine_mount_root.go:341`, `scenario_remote_test.go:797`, `executor_transfer.go:776`, `store_pending_creates.go:601`, `store_write_baseline.go:830`, `store_pending_creates.go:448` are fixed above; the entry for `store_pending_creates.go:723` is disproved (finding 5). "A mount-root watch skips zero-event polls" was wrong too: it never skips a successful poll (finding 8).

### Commands run (second pass)

Logs in the session scratchpad (`W5-p2-*.log`) and `reports/W5-lint.log`.

- Red runs on the rebased tree before the fixes: as listed per finding above.
- `go build ./...`: pass. `go vet ./...`: pass. `go vet -tags "e2e e2e_full integration" ./...`: pass.
- `go test -count=1 ./internal/sync/...` (whole tree): pass, 711 s.
- `go test -race -count=1 -timeout 100m ./internal/sync/...`: the first run, started before the host lock existed and beside another builder's race suite, hit the 100-minute deadline with no failed test and no race report before it (the two tests running at the deadline had run 20 s; the rest were queued for a parallel slot). Load failure; rerun once under the host lock: pass, 3370 s.
- `go test -count=1 -run TestCrashRecovery ./internal/sync/`: pass (2 tests, 40 s); with `-race`: pass, 965 s.
- `go test ./internal/graph/... ./internal/driveops/...`: pass; with `-race` under the lock: pass (graph 98 s, driveops 63 s). The first `-race` attempt waited in the lock queue past its tool time limit and was stopped before it started; rerun.
- Chaos seed 2 with crash seed 2 (`ONEDRIVE_GO_MODEL_SEED=2`, `TestModel_ChaosGraphConverges|TestModel_CrashAtSeededPointsRecovers`): pass, 153 s. Chaos seed 9: pass, 247 s. Both after the fake's zero-byte change.
- Cited-test check over the five spec files: 640 cited, 0 missing.
- Lint (`golangci-lint run ./internal/sync/... ./internal/graph/... ./internal/driveops/...`, under the lock): first run 2 `goconst` issues in the new tests (`"renamed.txt"`, `"folder-token"`); named as constants; rerun: 0 issues (`W5-lint.log`). The rename touched only those two test files after the whole-tree runs; the pending-create, placeholder, commit, finish and watch tests were rerun on it: pass.
- gofumpt and goimports: clean.

### Seen and not fixed

- `internal/sync/engine_mount_root.go` `shouldSkipMountRootWatchBatch` and its skip branch in `handleMountRootObservationResult`: unreachable for every batch `executeMountRootObservation` builds (findings always manage the read-denied type). Making findings-only batches skippable, or removing the branch, changes what a mount-root engine does with a zero-event poll, which this pass was told not to do without asking. Left as is.
- `internal/sync/store_pending_creates.go` `followAdoptedPlaceholderTx`: after an online rename the baseline row still does not take the item's new eTag and mtime (it needs the row at the item's path). Finding 5's scenario shows no harm (the rename is planned and the content goes in by ID); left as is.
- `internal/sync/tx.go:57` (plan row W34) and `internal/driveops/transfer_session.go:402` (rides on W33): left, as told.

### For the coordinator

- Finding 8's decision said a zero-event mount-root poll sends a poll-only batch. It already sends a whole batch (above), so only the stamp was added. Say if you want the poll-only path anyway.
- The amended commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`: this session's system attribution reminder now names that line, while SUBAGENT-RULES.md says no model names. Swap the line on amend if the program rule wins.


## Third review (2026-11-14, by the coordinator)

The commit is now `fb5f9ca1` on `bd2711b0`. Three points from the third review, all fixed in the commit:

- **A move onto the path of an item the move displaces lost the moved item's record.** `settlePendingCreatesTx` moved the records first and dropped the displaced item's record afterwards, so the moved row met the displaced item's row at the new path, was the one to end, and the placeholder at the new path was then unrecorded. Proven with a failing subtest ("onto the path of an item that has a record of its own", log `W5-s2-red.log`), then fixed by dropping the displaced item's record before the switch. The subtest uses a local move: a remote-move commit with the same shape fails earlier, on the unique path in `remote_state`.
- **The folder cases of the move had no test that fails when the folder's delete is removed.** Two subtests added ("a folder above it" and "a folder above it, onto a path that holds a row"). Mutation check: with the folder's `DELETE` disabled the second fails with "nothing is left below the old path" (`W5-m1-mutant.log`); the line was restored.
- **The spec and one comment said an adopted placeholder's hidden row ends with the baseline row the plan removes.** It ends in the prune step, which runs before the plan executes. Both texts now say so.

Checks on the tree of `fb5f9ca1`: `go build ./...`, `go vet ./internal/sync/...`, gofumpt on both Go files, and `go test -count=1 -run "TestCommitMutation_|PendingCreate|TestRunOnce_PlaceholderAtAPathTheFilterHidesLosesItsRecord" ./internal/sync/` pass. The whole gate runs when the branch takes its place in the merge pipeline.



## Merged as #906 (`3e5c1bd4`, 2026-11-14 about 11:30 PST)

- One commit `1b501ba5` on main at `2b1c5afa`. The first pre-pr gate failed in the lint on one test function of 101 lines (the limit is 100); the function was split. The second gate passed (8m40s in the verifier, classified reruns: none).
- Live proof on `1b501ba5` (the merged content), test account, one at a time: `TestE2E_ZeroByteFileSync` (45.5 s) and `TestE2E_Sync_KillMidUploadResumesSession` (71.5 s) passed.
- Pre-merge passed in 235 s while the scheduled nightly was in progress, so a pull request does not queue behind the nightly's live jobs. The merge waited about two hours for a reason outside the change: the session stood still from about 09:25, when a compaction failed on the account's weekly limit, until 11:23.
- Post-merge passed; installed `3e5c1bd4`; the watch client reports "Up to date, watching for changes", 19 files.
- No schema change and no store-generation bump.
