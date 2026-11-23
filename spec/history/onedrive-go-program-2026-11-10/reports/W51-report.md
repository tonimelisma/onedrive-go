# W51 report

Commit `1de94ac7` "Record the file that replaced a folder in its baseline update" on
`fix/crash-recovery-converges`, one commit on `f82a5f84`, not pushed. 4 files, +219 -18.

## Cause (answer i, product defect)

Seed 318011, batch 4 after a crash that lost the WAL: passes 2 to 8 each plan one
`baseline_update` at `q%20r` with nothing observed in between. The baseline holds a folder where
both sides hold the same file; the planner rightly plans a baseline update (`equal_again`), and
`publicationMutationFromAction` wrote the remote's item ID and hashes but put the baseline's
folder type back whenever the observed type was `ItemTypeFile` (the zero value). The row stayed a
folder, so every pass planned the same update. The same state arises without a crash when both
sides replace a synced folder with the same file. Per-pass table: `W51-dor.md`.

## What changed

- `internal/sync/store_write_baseline.go`: the override that replaced an observed remote's file type with the baseline's is gone; `fillMutationFromBaselineDefaults` fills hashes and sizes only from a baseline of the same item type (new helper `fillMutationContentFromBaseline`).
- `internal/sync/folder_replaced_by_file_test.go` (new): the three tests below.
- `internal/sync/dispatch_order_test.go`: the new `TestRunOnce_` registered in `runOnceScenarios`.
- `spec/design/sync-store.md`: the publication-only actions list states both rules, citing R-2.17.4 and the tests.

## Tests added

- `TestPublicationMutation_RecordsTheObservedItemType`: a baseline update records the observed sides' type in both directions of a type change, and a folder that replaced a file keeps none of its hashes.
- `TestRunOnce_AFolderBothSidesReplacedWithTheSameFileConverges`: both sides replace a synced folder (with a subfolder) by the same file; pass 1 transfers nothing and publishes the file (1 baseline update, file type, OneDrive's item ID, subtree gone); pass 2 plans nothing.
- `TestCrashRecovery_AFolderReplacedOnOneDriveWhoseDownloadWasCutConverges` (plain and lost-WAL): OneDrive replaces the folder, the crash comes at `download.renamed`; the first pass after the restart publishes the file, the second plans nothing, every invariant holds. Only the lost-WAL variant fails on the base.

Pass count asserted: 2 (publish, then quiet). Model bound stays 8.

## Checks

- Base, 3 replays: FAIL, FAIL, FAIL, identical. After the fix: 3 of 3 plain pass, 1 of 1 `-race` pass.
- New tests red on the base for the named reason (unit test also on the hashes).
- `go test -count=1 ./internal/sync/...`: ok, 473.5 s.
- New tests `-count=20` plain: ok; `-race -count=20` together with every `TestCommitMutation_`, `TestPublicationMutation_`, `TestUpsertBaselineRowTx_`, `TestSQLUpsertBaseline_` test: ok; `-race` `TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_AFolderBothSidesReplacedWithTheSameFileConverges`: ok.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: clean. `gofumpt -l`, `goimports -l`: nothing.
- `RealRepoIsClean`: four PASS. golangci-lint `./internal/sync/...`: 0 issues (`W51-lint.log`).
- Spec citations checked by hand: the three test names exist; R-2.17.4 and R-6.5.2 exist.

## Deviations

- The brief's step 4 asks for one scenario; there are two (the ordinary case as a `TestRunOnce_`, a crash case as a `TestCrashRecovery_` that cuts at `download.renamed`, the earliest crash point that leaves the state the drawn batch recovered into: a baseline folder where both sides hold the same file; the drawn batch itself lost the WAL later, at `delete.local_trashed`, after a re-created subfolder), since the defect is not crash-specific.
- Also fixed (same function, behaviour change, tested): a baseline update filled hashes and sizes from a baseline of another item type, so a folder replacing a file kept the file's hashes.

## Files of in-flight branches touched

None. `store_write_baseline.go` and `sync-store.md` are in no other branch's list; `dispatch_order_test.go` gets one line next to `TestRunOnce_AFileRedatedToTheEpochConverges`, away from the list's end.

## Declarations leaned on

`crashCapture`, `crashCaseName`, `crashPointDownloadRenamed` (`crash_points_test.go`, `crash_points.go`), `newSyncHarness`, `assertSyncInvariants`, `harnessLocalTree`, `syncStateDump` (`sync_invariants_test.go`), `assertPassPlannedNothing` (`mtime_range_scenario_test.go`), `runScenarioPass` (`engine_move_scenario_test.go`), `moveScenarioRemote` (`scenario_remote_test.go`).

## Not fixed (seen and not fixed)

1. `internal/sync/store_write_baseline.go:273` `actionItemType` reads `ItemTypeFile` (the zero value) as unknown: with no remote observed, a non-file baseline outranks an observed local file. Fix: an observed side's type decides, the baseline's only when no side was observed. Not done here: its other users are `planner_dependencies.go:597` and `executor_delete.go:420`, both in branches in flight.
2. `internal/sync/executor_test.go:3212` `TestPublicationMutation_SyncedUpdate_BaselineFallback` gives the local side a hash and leaves its `ItemType` unset (file) while asserting folder, so it encodes "file means unknown". Fix with item 1: give the local side `ItemTypeFolder` and no hash.
3. `internal/sync/engine_model_based_test.go:519` the model's message says "still changing after 8 passes" but a model pass is one `RunOnce`, which can run up to `oneShotMaxPasses` engine passes (here pass 1 ran two). Fix: name it "8 sync runs" or count engine passes.
4. `internal/sync/engine_runtime_start.go:55` the dispatch-order hook sees only frontiers of two or more and never publication-only actions, so the model's transcript leaves out single dispatches and every baseline update and cleanup. Fix: record every frontier, publication actions included, in the transcript.

## Unfinished

Nothing. Reviewer: no package outside `internal/sync` is affected.

## Review fixes

Amended commit `379b1f22` (was `86ef3f50`), one commit on `41919e44`, 7 files, +269 -30, not pushed. The stopped run's tree edits were taken as a draft: its two new rows were kept (reshaped below); its removal of the fill was the half-done "see it fail" step and is undone.

- Finding 1: row "file whose hash OneDrive omits" (`folder_replaced_by_file_test.go:54`): a file stays a file, the local side keeps its hash, OneDrive reports none, the baseline's remote hash is kept. The table now has separate local and remote hashes (`:40-43`). With the fill at `store_write_baseline.go:312` removed, `-run "^TestPublicationMutation_"` failed only this row ("the remote content"): no other `TestPublicationMutation_` test holds the fill. Fill restored.
- Note 3 applies: `sqlite_compare.go:456` is `COALESCE(b.etag, '') <> COALESCE(r.etag, '')`, so an observed empty eTag over a baseline eTag is drift. `store_write_baseline.go:328` now reads `if mutation.ETag == "" && !remoteObserved`. Row "file observed without an eTag" (`:59`) failed first (got the baseline's eTag), passes now. The comment at `:292-300` and the bullet at `sync-store.md:393-405` state the rule. Whether OneDrive sends such a row is not known; nothing claims it.
- Unit test renamed `TestPublicationMutation_RecordsTheObservedItem` (it holds hashes and the eTag too); the spec cites the new name. "Tests added" above still shows the old one.
- Finding 2, kept in full: `sync_invariants_test.go:162-166` require item-type agreement with remote_state and local_state; `crash_points_test.go:400` adds `BaselineUpdates`; `assertQuietNextPass` unchanged. Whole `go test -count=1 -timeout 60m ./internal/sync/`: ok, 454.5 s, so nothing was taken back. Base check: with `store_write_baseline.go` of `41919e44` in the tree for one run, the lost-WAL variant failed both new type invariants ("of another type or absent in remote_state", "absent from local_state or of another type there"), besides the test's own type assertions and planned-nothing pass; the plain variant passed. File restored; `git status` showed only my edits.
- Note 4: `assertPublishesTheReplacingFileInOnePass` also reads `item_type` and `remote_hash` of the stored row through `rawDB` and compares OneDrive's hash (`folder_replaced_by_file_test.go:204-212`).
- Note 5: "Deviations" above now says the crash test cuts at `download.renamed`, the earliest crash point that leaves the state the drawn batch recovered into.
- Note 8: both bullets of the list at `sync-store.md:393-407` end with a period.
- `engine_model_based_test.go:519`: says "sync runs" and prints `modelMaxSyncPasses+stormPasses` and the storm count.
- Commit message: adds the eTag rule, the invariant changes, the stored-row check, the model message and the new test name; the seed line says what the batch did instead of "after eight passes".

Checks: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...` clean; `gofumpt -l`, `goimports -l` nothing; the three commit tests plus every `TestPublicationMutation_`, plain ok and `-race` ok (72.3 s); whole package as above; `RealRepoIsClean` 4 PASS; golangci-lint `./internal/sync/...` 0 issues (`W51-lint.log`, final tree).

Not done: nothing.

Seen and not fixed: `crash_points_test.go:399-400` leaves `ConflictCopies` out of the quiet sum that `assertQuietNextPass` (`sync_invariants_test.go:114-115`) counts; whether a pass can make only a conflict copy was not checked. Fix if it can: add it to the sum.


## Merged as #935

- Merged 2026-11-16 03:43 PST as `e248aae5` (squash); installed 03:52.
- The pre-PR gate passed on `50c4e2e8` (base `ca8ecac6`); the PR's CI
  was green; pre-merge passed on its second run. The first run refused
  the PR because its body did not name the nightly claim's key.
- The live proof is mine and is in the PR body ("Live, ..." lines).
- Not known: how often OneDrive sends a sparse update in the first
  sync after the fix; whether it ever sends a remote row with an empty
  eTag for an item whose baseline holds one.
- Opened by this merge: W74, W57, and W81 once W79 has merged too.
