# A16 Definition of Ready: a create intent nobody will retry is dropped; expired intents are pruned

Worktree `~/src/onedrive-go-wt/orphan-create-intent`, branch `fix/orphan-create-intent`, base `e686ae2e`.

## What I found (checked in code)

- Reproduced on this tree: seed 9 fails in 222 s on "invariant: no pending creates after convergence", leftover row `docs/50%.txt` with no item ID (log `scratchpad/a16-seed9-before.log`); seed 11 run separately (`a16-seed11-before.log`).
- Intent-only rows are deleted in exactly two places: `ClearPendingCreateIntent` (the executor, after an attempt known to have created nothing) and `claimIntendedPlaceholderTx` (observation, only when an item is observed at the intent's exact path). An intent kept by `uploadNewFile` (unknown outcome, `executor_transfer.go:533`), by a canceled attempt (`clearPendingCreateIntent`, `:644`), by a 409 with nothing listed (`resolveCreateConflict`, `:722`), or left by a crash between the intent and the request, is never deleted when no create at that path is retried and no item ever appears there. `pendingCreateIntentTTL` only stops claims (`store_pending_creates.go:387`, `executor_transfer.go:567`); nothing deletes an expired row.
- The TTL test is written twice (`claimIntendedPlaceholderTx` and `priorCreateIntent`); `ClearPendingCreate` has no caller.
- Plan stages: one-shot (`runLiveCurrentPlan`), watch bootstrap and watch steady state all end in `reconcileRuntimeStateStage` -> `reconcileRuntimeState`, which already prunes `retry_work` and block scopes after the plan is built and before anything runs; the dry run does not (`keepBuiltCurrentPlan`). In one-shot and bootstrap the remote observation (and so every claim) is committed before the local scan and before that stage. Watch-mode remote commits claim as they land, so at that stage an unclaimed row is one with `item_id = ''`.
- "Complete local observation" exists as a durable fact: `observation_state.local_truth_complete`, set by a full snapshot (`markLocalTruthCompleteTx`) and cleared by `MarkLocalTruthSuspect`. Unavailable local truth at a path is `newTruthAvailabilityIndex(observation issues)` (read boundaries, case collisions, invalid names), the same index the planner uses. The scanner never records a path the content filter hides or a shortcut root reserves (`shouldObserveWithFilter`), so absence there says nothing.
- Kill-mid-create check: the item lands after the kill, the user deletes the local file, the process restarts. The remote observation runs first and claims the empty item (`claimIntendedPlaceholderTx`), adopts it, and the row then names the item; the prune never touches it, and the planner deletes the placeholder with If-Match. The decision holds there.

## What I will change

- `store_pending_creates.go`: `pruneAbandonedCreateIntents(ctx, scanned func(path) bool)`: one transaction reads the intent-only rows, deletes each one past the TTL, and, when local truth is complete, each one at a path the scan covers, whose local truth is available and where `local_state` holds nothing. Read, decision and delete share one transaction (the store has one connection), and the delete still requires `item_id = ''`. One freshness method on `PendingCreate` replaces both TTL copies. Remove the dead `ClearPendingCreate`. Rewrite the top comment block.
- `engine_current_plan.go`: `reconcileRuntimeState` calls the prune with the engine's scan-coverage predicate (content filter plus protected roots).
- `executor_transfer.go`: `priorCreateIntent` uses the shared freshness method.
- Docs: `spec/design/sync-execution.md` (pending creates paragraph), `spec/design/sync-store.md` (Pending creates), `spec/design/sync-engine.md` (one-shot step 6), `spec/requirements/transfers.md` R-5.6.15 text.

Refresh (found while checking the class): a zero-byte create never runs the create hook, so when its intent outlives a create that completed (the attempt was canceled as it succeeded, which keeps a canceled attempt's intent, or the clear failed) the next observation claimed the synced file as a placeholder, and the row then named an empty file for good. `claimIntendedPlaceholderTx` drops the intent instead when the baseline already holds the observed item. Test: `TestCommitObservation_IntentDoesNotClaimAFileTheBaselineHolds`.

## Tests (each seen failing first)

- Store: `TestPruneAbandonedCreateIntents_DropsWhatNoCreateWillClaim` (expired dropped whatever local says; absent path dropped; file present, unavailable truth, unscanned path, suspect local truth and a row naming an item kept).
- Engine: `TestRunOnce_CreateIntentOfAFileRenamedAwayIsDropped` (unknown outcome that never landed, rename, row gone after the next pass, invariants hold).
- Engine: `TestRunOnce_EmptyItemOfAKilledCreateWhoseFileWasDeletedIsRemoved` (claim before prune, placeholder deleted). This one passes on the base tree (observation already claims first); I will show it fails against a prune run before observation.
- Seeds 9 and 11 pass; seeds 1 to 20 run one at a time.

## Definition of Ready

1. PASS **Task intent**: an intent-only pending-create row that no create will retry is dropped when the plan is built from complete local truth with no file at its path; intent-only rows past the TTL are deleted at the same point.
2. PASS **Non-goals**: no change to how intents are recorded, claimed or kept by the executor; no store generation bump; no live e2e (the caller runs it); rows that name an item are not pruned.
3. PASS **Governing docs**: `spec/design/sync-store.md` (Pending creates), `spec/design/sync-execution.md` (create uploads), `spec/design/sync-engine.md` (one-shot pass, runtime state), `spec/requirements/transfers.md` R-5.6.15, repo `CLAUDE.md` lines 55-236.
4. PASS **Authority owner**: `pending_creates` is owned by the sync store (`store_pending_creates.go`); the decision to prune runs at the engine's runtime-state reconcile (`engine_current_plan.go`), which already owns pruning durable work to the current plan; local truth completeness is `observation_state`; truth availability is `truth_status.go`.
5. PASS **Code scope**: `internal/sync/store_pending_creates.go`, `engine_current_plan.go`, `executor_transfer.go`, tests in `pending_creates_test.go`, `engine_pending_create_intent_test.go`, `dispatch_order_test.go`; the four spec files above. Nothing outside `internal/sync` and `spec/`.
6. PASS **Regression plan**: seeds 9 and 11 of `TestModel_ChaosGraphConverges`, plus the three tests above.
7. PASS **Verification plan**: focused `-run` tests with and without `-race`; the list tests (`ListsEvery`, `Table`, `Governs`, `Exhaustive`, `Registered`); seeds 1-20 one at a time; `go build ./...`, `go vet ./...`, full non-race `go test ./internal/sync/...` once at the end; lint `./internal/sync/...` alone.
8. PASS **Data safety**: the prune deletes only rows with no item ID; it never deletes a file. It acts only on complete local truth, available truth at the path, a scanned path and no local entry, so a file the scan could not see keeps its intent. A create that lands after the drop shows up as a remote empty file and is downloaded, which is the accepted lesser harm. The read and the delete share one transaction on the store's single connection, so no claim or fresh intent can land between them.
9. PASS **Secret and credential safety**: no tokens, `.env` or account names involved.
10. PASS **Live provider and destructive operations**: none; no live account, no reset, no dependency change, no history rewrite.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: can watch-mode `local_state` be complete-but-stale so that a file present on disk is missing from it? Current assumption: no; it is the same truth the planner deletes remote files on, and suspect truth clears `local_truth_complete`. Validation step: the prune requires `local_truth_complete`; covered by the store test's suspect case. Owner if wrong: `store_pending_creates.go`.
    - Question: do other chaos seeds in 1-20 fail on another invariant? Current assumption: unknown. Validation step: run each seed alone and report. Owner if wrong: report "Not fixed".
