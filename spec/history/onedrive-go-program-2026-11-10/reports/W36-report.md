# W36 report

Commit `7ecdb13f` on `fix/planner-move-recreate-cycle`, one commit on `3e5c1bd4`, not pushed.

## Reproduction on the unchanged tree (3e5c1bd4), before any product change

Test: `TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges`, one case per row, `-count=20` plain and `-count=20 -race`.

| Case | Plain runs / failures | Race runs / failures |
|------|----------------------|----------------------|
| two runs (the row's two steps, `50%.txt`, `photos/photos`) | 20 / 0 | 20 / 0 |
| one run (both moves before a single run) | 20 / 20 | 20 / 20 |
| two runs plain name (`plain.txt`) | 20 / 0 | 20 / 0 |
| one run plain name | 20 / 20 | 20 / 20 |
| two runs two levels deep (`photos/album/photos`) | 20 / 0 | 20 / 0 |
| one run two levels deep | 20 / 20 | 20 / 20 |
| two runs, lagging listings (`invisibleFor` on the first run's three results) | 20 / 0 | 20 / 0 |
| chaos seed 3 at two batches | not run: seed replay is not in this tree | - |

Every failure is the cycle and only the cycle (60 of 60 per mode: "sync never finished", OneDrive tree unchanged, "dependency cycle" logged; no data race). Each of a failing case's three syncs dropped the same three actions on its pass and reported `LeftWork{WillRetry: 3}`:

- A0 `remote_move docs/50%.txt -> docs/renamed-50%.txt` (file)
- A1 `remote_move docs -> photos/photos` (folder; `photos/album/photos` two levels deep)
- A2 `folder_create docs` (CreateRemote)

Edges, from the admitted plan logged once with `buildDependencies` (the planner's log line names no edges): A0 waits for A2 (parent folder create, `addParentFolderDep`); A2 waits for A1 (create at the path a move vacates, `createsAtVacatedSource`); A1 waits for A0 (children leave first: A0's source lies under A1's source and its destination not under A1's destination, `underMoveSource`). The row's second sync planned only `remote_move photos/photos/50%.txt -> docs/renamed-50%.txt`; its first sync planned A1 and A2 (A2 waits for A1).

The lagging case was dropped from the commit: after its runs all three `invisibleFor` counts were still 2, so no path read or listing touched them and the case exercised nothing. The planner reads delta, which the harness cannot delay; no switch was built.

## What changed

- `internal/sync/planner_dependencies.go`: the children-leave-first edge (`moveOrderDependency`) skips a same-side move into a folder the plan creates, on the moving folder's side, at the path the folder move vacates (`recreatesMoveSource`, `movesIntoRecreatedSource`); comments of `addMoveDeps` and `moveOrderDependency` state the rule.
- `spec/design/sync-planning.md`: the move-ordering paragraph states the exception and the supersede that follows; the cycle row of the evidence table adds the left-work half (R-6.6.28).
- `internal/sync/engine_move_recreate_test.go` (new), `planner_move_composition_test.go`, `engine_run_once_left_test.go`, `planner_dependencies_test.go` (oracle follows the rule), `dispatch_order_test.go` (scenario listed).

## Tests added

- `TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges` (6 cases): one sync finishes, both trees equal, the file is moved (same item, no upload), no cycle logged, next plan quiet.
- `TestBuildDependencies_MovesThroughAFolderRecreatedAtAVacatedPathHaveAnOrder`: the three plans the scenario's syncs made keep every action and have the expected edges.
- `TestBuildDependencies_ChildrenLeaveFirstUnlessTheyMoveIntoTheRecreatedSource`: without the create, with a create on the other side, or for a delete under the source, the folder move still waits.
- `TestPassWithoutActions_ActionsDroppedAsACycleAreLeftForTheNextSync`: a hand-built cycle through `planBuiltActions`, `passWithoutActions`, `reportWhatTheRunLeft` gives `LeftWork{WillRetry: 2}` (exit 1) and an Error log line.

After the change: the six cases pass 20 of 20 plain; each single-sync case finishes in one sync with one superseded action (the file's move, whose source names the pre-move path) that the next pass of the same sync replans.

## One-shot run and dropped actions

Before and after (unchanged): `planBuiltActions` counts the dropped actions in `planFollowUp.Dropped` and `Unfinished`, `reportWhatTheRunLeft` adds them to `LeftForNextSync`, `LeftWork.WillRetry` counts them, so the run exits 1 and says they will be retried. Shown by the scenario before the change (`WillRetry: 3`) and by the hand-built test.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/sync/...` (behind the lock): pass (501 s).
- `go test -race -count=1 -run '^(TestBuildDependencies_|TestPathAncestors_|TestPathNameKey_|TestIsolateDependencyCycles_|TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges|TestPassWithoutActions_|TestReverseDispatchTable_ListsEveryRunOnceScenario)'` (behind the lock): pass.
- `TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges` and the package's `ListsEvery|Table|Governs|Exhaustive|Registered` tests: pass.
- `TestFindTestConventionViolations_RealRepoIsClean`: pass.
- golangci-lint `./internal/sync/...`: first run 1 issue (gocyclo in the oracle, fixed by extracting `oracleRecreatesMoveSource`; `TestBuildDependencies_` rerun passes), second run `0 issues.` (W36-lint.log).
- Spec: every test name added to `sync-planning.md` exists; R-2.17.6, R-2.17.1, R-6.6.28, R-2.1.7 exist.

## Deviations

- The fix removes an edge instead of adding the two the row names: both already existed (A2 on A1, A0 on A2); the third edge (A1 on A0) closed the cycle.
- The lagging-listing case is not kept (see above).

Also fixed: none. Unfinished: none. Packages for the reviewer: none beyond `internal/sync`.

## Seen and not fixed (2)

1. `internal/sync/planner_move_composition.go:914-916` (`planMove`): a move leaving a moving folder keeps its pre-move source even when its target lies in the folder re-created at the vacated path, so once the folder has moved it is superseded and costs one more pass. Fix: on the remote side, rebase its source through the earlier move as the ancestor-carry branch below does, so it runs in the same pass.
2. `internal/sync/planner_move_composition_test.go:343,449` and `spec/design/sync-planning.md:90,151`: cycle isolation cites R-2.8.11, whose text is about watch-mode reporting and runner restarts, not plans. Fix: cite the requirement that covers work a plan cannot order (R-2.17.6's last sentence, or a new line), the owner's choice; not fixed here because the choice of requirement is not mine.


## Review fixes (2026-11-14, made in review, amended into the one commit)

The read-only review (`R-W36-findings.md` in the scratchpad: 3 must, 3 should, 6 notes; "accept after small fixes") was answered as follows.

- **Must 1, wrong requirement cited for cycle isolation**: R-2.8.11 is watch-mode reporting. The four sites now cite R-2.17.6 (its last sentence: a move that cannot be ordered in the current pass waits for the next one rather than run out of order): the table row and step 10 of `spec/design/sync-planning.md`, and the two test comments in `planner_move_composition_test.go`. The history sentences at those sites and in `planner_dependencies.go` are gone.
- **Must 2, the download side had no test**: the unit table gained the plan with local moves and a local create (`one run, the moves made on OneDrive`), and the scenario gained three cases with the steps made on OneDrive (two runs, one run, one run two levels deep) plus a `downloads` counter in the scenario remote: the file is moved on this computer, not downloaded again. **Fact found: the three on-OneDrive scenario cases pass on the base `3e5c1bd4` too.** The download side did not show the cycle in the scenario; why the plan differs there is unknown (not looked into). They stay as pins.
- **Must 3, nothing pinned the destination check of the exception**: new unit case `a sibling that moves elsewhere`. Mutant: with the prefix check of `movesIntoRecreatedSource` made always true, that case and only that case fails (of the `TestBuildDependencies_`, `TestIsolateDependencyCycles_` and scenario tests); restored, and the file differs from the hand-back in comments only.
- **Should 1 and 2, wording**: comments and spec say "into or below" the re-created folder, which is what the code does; new unit case `one run, into a folder made inside the new one` (`docs/new/50%.txt`, ordered through the create chain).
- **Should 3, live proof**: see "Live proof" below.
- **Note 2** (a mount-root engine moves the file in the same pass): half checked (`expectedRemotePreconditionPath` returns the empty path there), so the spec does not say it.
- **Note 4, the file put back under its old name**: run as a scenario. With a sync between the steps it ends right (kept as `two runs same name`). With one sync for all three steps **the sync moves the file back into the moved folder on both sides**; it fails the same way with the base planner files, so W36 neither causes nor fixes it. Plan row W49 holds it, with note 3 (a child folder taking the path its parent vacated: a cycle by reading, not run) and "seen and not fixed 1" (the extra pass) as its items 2 and 3.

Checks after the fixes: the unit tables and the ten scenario cases pass plain; gofumpt clean on the four Go files; the rest (race on the changed tests, build, vets, convention test, lint behind the lock) is recorded with the gate.


## Live proof (2026-11-14 about 15:45 PST, test account, private tree, mine)

Script `w36live.py BINARY LABEL` in the scratchpad: a private tree with its own HOME and XDG roots and a copy of the test account's saved sign-in, a config that syncs one new folder made for the run, three files up, then with no sync in between `docs` moved to `photos/photos`, a new `docs` made, and `50%.txt` moved from the moved folder into the new `docs` under a new name; one sync; four folder listings from OneDrive; a further sync; the folder removed from OneDrive and the tree removed. It prints exit codes, counts and yes or no only.

| | main `0ff7c754` (before) | branch `e38f9699` (after) |
|---|---|---|
| first sync (three files up) | exit 0 | exit 0 |
| the one sync after the three steps | exit 1, 3 lines naming a dependency cycle | exit 0, none |
| OneDrive folders as wanted after it | 1 of 4 (`docs` still holds both of its files; `photos/photos` is not there) | 4 of 4 |
| the file at its new path is the same item | not there | yes (moved, not sent again) |
| a further sync | exit 1, the same 3 lines | exit 0 |
| this computer's tree | as the user left it | as the user left it |

The first run of the script with the branch binary said 3 of 4: the script's own check dropped the folder's name from a listing of `photos`, which holds a child of that name. Fixed in the script and run again; the table is from the runs after that fix.

Checks after the review fixes, before the rebase: the changed tests under the race detector pass (427 s; the first attempt hit the 10-minute test limit under load inside the whole run-once rerun of the reverse-dispatch test, which is not this branch's code, so the second attempt named the one subtest and took a 30-minute limit); `go build ./...`, `go vet ./...`, the tagged vet and the test-convention check pass. The commit was amended to `e38f9699` (subject kept, trailer plain).



## Merged as #912 (`ec6b0417`, 2026-11-14 about 16:25 PST)

- One commit `b91eae58` on main at `0ff7c754`. The pre-pr gate passed on its first full run (about 34 minutes with three writers on the host); pre-merge passed in 325 s with no review threads; post-merge passed in 129 s and removed the worktree and its branch.
- Installed `ec6b0417` at 16:28; the watch client reports "Up to date, watching for changes", 19 files.
- Live proof: the table above, made on the test account before the gate. With the branch build the one sync after the three steps exits 0 where main exits 1, OneDrive holds the four folders as wanted, and the file at its new path is the same item (moved, not sent again).
- Still open after this: a file put back at its old path after its folder moved does not stay there. That defect is proven on main and has its own plan row; its mechanism is unknown.
- Free to start after this: that row (its brief is written; it needs a builder slot).
