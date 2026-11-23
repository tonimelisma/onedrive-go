# A16 report: a create intent nobody will retry is dropped; expired intents are pruned

Worktree `~/src/onedrive-go-wt/orphan-create-intent`, branch `fix/orphan-create-intent`, on `142b194b`. One commit `f5de518b` (16 files), amended in the second pass, not pushed.

## What changed

- `store_pending_creates.go`: `pruneAbandonedCreateIntents(ctx, passObservation, scanned)`, one transaction. It drops intent-only rows past the TTL after any plan. After a pass that observed OneDrive and then the local tree, it also drops rows whose path complete local truth visits and finds empty, with local and remote truth available there. `passObservation` (`passRefreshedLocalOnly`, the zero value; `passObservedRemoteThenLocal`) with an exhaustive `claimedLandedCreates`. `PendingCreate.intentIsFresh` is the one TTL test. The claim skips an empty file the baseline already holds. Dead `ClearPendingCreate` and its interface removed. Comments rewritten.
- `engine_current_plan.go`: `reconcileRuntimeStateStage`/`reconcileRuntimeState` take `passObservation`. One-shot live and watch bootstrap pass `passObservedRemoteThenLocal`; watch steady state passes `passRefreshedLocalOnly`. The dry run does not prune. `Engine.localScanCoverage` is the scan-coverage predicate.
- `store_write_observation.go`: `scanRemoteStateRow` (swallowed every error) became `remoteStateRowByIDTx` returning `(row, found, err)`; only `sql.ErrNoRows` is absent. Every caller (park, restore, `restoredParkPathTx`, `processObservedItem`, `store_remote_twins.go` `storedPathForObservedItem`) returns the error. `ForgetRemoteItem` fails on an unreadable item type instead of taking it for a file. The duplicate doc comment is removed and the contents list updated.
- `executor_transfer.go`: `priorCreateIntent` uses `intentIsFresh`; kept-intent comments updated. `doc.go`: reconcile phase line.
- Docs: `sync-store.md` (Pending creates: the two rules, what a long-running watch does), `sync-execution.md`, `sync-engine.md` (one-shot step 6, steady-state replan paragraph), R-5.6.15 (stays `[verified]`). No GOVERNS change, no generation bump.

## Tests added (each seen red first)

- `TestPruneAbandonedCreateIntents_DropsWhatNoCreateWillClaim` (10 cases). Dropped: no file; expired with file present and truth suspect; expired in a local-only pass. Kept: file present; local read boundary; remote read boundary; local-only pass; suspect truth; unscanned path; row naming an item.
- `TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile`: a steady-state replan with the file gone keeps the intent. The remote batch with the empty item claims it, the next replan plans only the delete of that item, and the pass that runs it deletes the placeholder with no download.
- `TestCommitObservation_ARemoteRowThatCannotBeReadFailsTheWindow` (delete, change): a scan error fails the window and names the read; the cursor does not advance.
- `TestForgetRemoteItem_RemovesTheSubtreeAndFailsOnAnUnreadableItemType`.
- From the first pass: `TestCommitObservation_IntentDoesNotClaimAFileTheBaselineHolds`, `TestRunOnce_CreateIntentOfAFileRenamedAwayIsDropped`, `TestRunOnce_EmptyItemOfAKilledCreateWhoseFileWasDeletedIsRemoved` (both in `runOnceScenarios()`).

## Checks

- Base tree: seeds 9 and 11 failed on "no pending creates after convergence". First pass: seeds 1 to 20 each pass, run one at a time. Second pass: seeds 9 and 11 pass again.
- `go build ./...`, `go vet ./...`: ok.
- `-race -run` over the new and changed tests (pending creates, observation commit, park and twins, ForgetRemoteItem, reconcile, the watch test, uploads): ok.
- List tests (`ListsEvery|Table|Governs|Exhaustive|Registered`): ok.
- Full non-race `go test -count=1 -timeout 60m ./internal/sync/...`: ok (324 s). After it, lint asked for `err :=` in place of six `err =` in `store_write_observation.go` (same behaviour); the `-race` run over the changed tests, seeds 9 and 11, build and vet were all rerun after that edit and pass.
- Lint `./internal/sync/...` (`GOTOOLCHAIN=go1.26.11`) on the final tree: `0 issues.` (`reports/A16-lint.log`).

## Deviations

- Second pass, as decided: the file-gone rule needs a pass that observed OneDrive before planning, and available remote truth at the path. A watch steady-state replan applies expiry only.
- `remoteStateRowByIDTx` returns `(row, found, err)` rather than `(row, error)`: `nilnil` refuses `nil, nil`, and the same three-value shape is what `getRemoteStateRow` already uses.
- First pass, same class: the claim guard for a file the baseline holds.

## Also fixed

- `store_pending_creates.go`: dead `ClearPendingCreate`; TTL rule written twice; run-together `classifyObservedPlaceholderTx` comment.
- `store_write_observation.go`: swallowed scan errors (above); `ForgetRemoteItem` parse error taken as "file"; duplicate doc comment.
- `store_write_observation.go:1` package-comment entry: already a file comment on `142b194b`; only its contents list needed the rename.
- `executor_transfer.go`, `doc.go`: comments that no longer matched.

## Not fixed

- `internal/sync/store_pending_creates.go:483` (`followAdoptedPlaceholderTx`): a placeholder renamed online while still empty keeps its record at the old path for as long as it stays empty. Fix: drop the record once the item leaves the record's path. Planned: pending-create lifetime.
- `internal/sync/executor_transfer.go:545` (`uploadNewFile`): in watch mode a zero-byte create's file can be claimed between the create and `clearPendingCreateIntent`, which clears only `item_id = ''`, leaving a row that names an empty file for good. Fix: on success without the hook, clear the row at the path whether it names nothing or `result.Item.ID`, on a detached bounded context. Planned: pending-create lifetime.

## Unfinished

none

## Packages the caller should test

Only `internal/sync` changed.
