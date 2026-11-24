# W79 report

Commit: `658fb595` on `fix/remote-move-into-recreated-folder` (on 0104b8f2), not pushed; 6 files.

## What changed
- `internal/sync/planner_move_composition.go`: `folderHolder` walks up a chain of new folders to the first path a synced item holds (both sides; OneDrive paths through `snapshotPath`); `localFolderHolder` uses it; `waitsForAFolderHolder` replaces `waitsForALocalFolderHolder`; new `remoteLandingWaits`, `remoteLandingLoops`, `plannedRemoteMove`; header comment; `foldMovePath` removed for `pathNameKey`.
- `internal/sync/planner_dependencies.go`: index `movesByOldNameKey`; `moveDepCandidates` adds the moves under each ancestor name key of a move's path; `movesIntoVacatedPath` compares name keys; comments.
- `internal/sync/planner_move_composition_test.go`: new OneDrive rows, chain row, two dependency rows, `pathNameKey` in a helper.
- `internal/sync/engine_move_recreate_test.go`: three RunOnce scenarios, `newFolderPairScenarioRemote`, `assertOneSyncLeavesTree` returns superseded.
- `internal/sync/dispatch_order_test.go`: three lines in `runOnceScenarios()`.
- `spec/design/sync-planning.md`: Move Composition (OneDrive hold, two-folder shape, chain), dependency paragraph and Dependency Build index.

## Tests added (each seen failing on the base, via an overlay of the base product files)
- `TestComposeMoves_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsAnItemAtItsPath`: 8 rows; on the base 5 fail: x kept (f->x/f planned), chain kept (f->x/sub/f), frozen x and file x (f->x/f planned), two folders (both moves planned); 3 guard rows (x moved away, chain moved away, x decided after f) pass on both.
- `TestComposeMoves_FolderMovedIntoAFolderMadeWhereAnotherFolderLeftWaits/a_chain_of_new_folders`: base plans f->x/sub/f with x->x2.
- `TestBuildDependencies_MoveIntoThePathAnotherMoveVacatesWaitsForIt/into_a_new_X`: base `{nil, nil}`; row "named X" (rename inside x) guards the folded source compare, passes on both.
- `TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsTheOldFolderThere`: base moves item-f into the old x during the collision and moves it twice after.
- `TestRunOnce_TwoFoldersMovedHereIntoNewFoldersAtEachOthersPathsConverge`: base never finishes, four actions dropped as a cycle every pass.
- `TestRunOnce_FolderMovedOnOneDriveIntoAChainOfNewFoldersConverges`: base supersedes f's move once.
`TestComposeMoves_FolderAMoveLandsInIsNotParkedToBreakASwap` is unchanged: the walk finds z at Q/z first.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 -timeout 60m ./internal/sync/...` (lock): ok, 1012 s.
- `go test -race -count=1 -timeout 90m ./internal/sync/` (lock): ok, 4266 s.
- Property and model tests in those runs, all pass: `TestPlannerModel_RandomTriplesConverge`, `TestBuildDependencies_MatchesScanningOracle`, `TestRunOnce_RandomizedBidirectionalEditsConverge`, `TestModel_SameSeedReplaysIdentically`, `TestModel_CrashAtSeededPointsRecovers`.
- 200x plain (lock), own `-run`: first shape ok 120 s, two folders ok 134 s, chain ok 99 s.
- `RealRepoIsClean`: 4 PASS.
- Lint `./internal/sync/...` (lock, last): `0 issues.` (W79-lint.log), on the committed tree; one test comment was reworded after the test runs, before lint.

## Deviations
- First shape reproduced with `X2` (a name folding to x2) made on OneDrive: an exact `x2` is reconciled by SQLite in the same pass (merged folder, or conflict copy for a file) and the base converges there, leaving item-x at x.
- Two-folder shape fixed without a park: decision (d) on OneDrive (the second decided waits when its holder's move lands at its own path), 24 product lines (`remoteLandingLoops` and one case). It converges in 3 working passes plus the quiet one, exactly `oneShotMaxPasses`.
- On OneDrive a folder holder still pending when the move into its path is decided does not make the move wait (unlike `holdsALocalLanding` here): when the holder then moves on OneDrive, its move and the create follow in the same pass; when it ends the composition without an OneDrive move, the move is taken back (Review fixes, finding 2). Making the move wait at once would change what `TestComposeMoves_EntryBelowARenamedSubfolderKeepsItsSource` and `TestComposeMoves_FileMovedBackWaitsForAFolderDecidedAfterIt` plan and add passes.

## Also fixed
- `foldMovePath` duplicated `pathNameKey` line for line; removed.
- `assertOneSyncLeavesTree` dropped the superseded count; it returns it.

## Not fixed (4)
1. `planner_move_composition.go:1761` `dropCreatesOntoComposedItems`: with a chain made here while OneDrive keeps x (first shape with x/sub), SQLite's create of x/sub lands in the old x on OneDrive; after the collision x carries it to x2/sub and it is moved back (probe: moves item-x->x2, sub->sub, item-f->f). Fix: drop a remote create below a path OneDrive keeps a composed item at that is elsewhere here.
2. `planner_dependencies.go:572` `underMoveSource` with SQLite's local create: in the chain made on OneDrive, the local create of x/sub runs inside the old x before x's move (children first), is carried to x2/sub and moved back next pass. Fix: drop a local create below a path whose local occupant OneDrive no longer has there; it is made the pass after.
3. `planner_move_composition.go:681` `parkOneOccupancyCycle` keeps only local holders of a planned landing; an item parked on OneDrive leaves no create after it (`recreatedSourceCreates` skips deferred items), so a remote move planned into its path fails when run (by reading). Fix: do not park on OneDrive an item a planned remote move's `folderHolder` names.
4. `planner_move_composition.go:1094` `remoteLandingWaits`: a folder moved here into a new folder made at its own path (k to k/k made here) has itself as holder; its move and the create of k wait for each other (by reading, twin of the planned OneDrive-side k to k/k). Fix with that change.

## Unfinished
Nothing. Packages for the reviewer: `internal/sync` only.

## Review fixes

Commit `cdb02def` on `0e31e63c` (rebased, one commit, 7 files), installed. Product code against `9cb1d599`: +60 −21 lines in `planner_move_composition.go` (comments +66 −39, with the header reflow); `planner_dependencies.go` comment only.

- **Finding 1** (`case !e.movedOnOneDrive(holder)` of `remoteLandingWaits`, `planner_move_composition.go:1180`, had no row). New row "x held in a new b, decided first" (`planner_move_composition_test.go:840`): x moved into a new b whose rename collides with B2, so x is deferred before f is decided. Proof, `-run "TestComposeMoves_|TestRunOnce_"`: with that case answering false and the take-back below switched off, the row fails, and so does the older `TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsTheOldFolderThere` (in the engine x is deferred before f is decided; only the composition row "x kept" reaches case 1). With the case answering false and the take-back on, nothing fails: the take-back also holds a holder decided first. The case stays and keeps such a move out of the run instead of planning and taking it back. Product line restored.
- **Finding 2** (a holder still pending when the move is planned, deferred later): made true. `emit` (`:608`) runs `emitRun` (`:636`, the old body) and then `takeBackHeldLandings` (`:667`): `recordLanding` (`:1080`) records the `folderHolder` of each planned OneDrive move into a folder made here (`landings`); a move whose holder ends the run without an OneDrive move goes into `takenBack`, and the run is repeated with those keys deferred in `resolve` (`:955`), so `destinationFree`, the parent checks and the vacated-path creates see them stay. Bounded: a taken-back key is deferred before `planMove` and never recorded again, so each run adds new keys, at most one run per key; every run uses the same sorted order; `emitRun` writes only its own emitter, so a run is a function of the composer and `takenBack`. The partner moves of decision (d) keep their landing: `loopHeld` now maps the second of a loop to the first (`:1183`), and `isLoopPartner` (`:683`) spares only the moves on that loop, so the pair and the ring plan exactly as before. Rows: "x waiting for the delete of x2, decided after f" (`:845`, To run 2: no move of f, x and f deferred, no create of x), "x held in a new y, decided after f" (`:850`, the compound shape), "each into a new folder at the other's path, and g into the new f" (`:863`, a move into the folder that waits that is not on its loop). Proofs: take-back off (the rerun loop not entered), the first two rows and `TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileTheOldFolderWaitsInANewFolder` fail (f moved into the old x during the collision, then again: `item-f->f` twice); with every move into a loop-held folder spared (the state before this round), only the g row fails (`g->f/g` planned into the old f). No existing expectation changed; the STOP rule was not reached.
- **Finding 2, RunOnce for To run 2**: `TestRunOnce_MoveIntoAFolderMadeHereAfterARenameOntoADeletedFileConverges` (`engine_move_recreate_test.go:532`) runs those steps and never reaches that composition shape: the sync deletes the file x2 on OneDrive, makes a new folder x2 there and moves keep.txt into it, and x stays at x on OneDrive holding f (keep.txt and f moved once each, zero superseded). Why observation does not report x's rename as a move here: unknown. The engine proof of the take-back is the compound scenario above (`:568`), which fails without it.
- **Finding 3**: `TestRunOnce_TwoFoldersMovedHereIntoNewFoldersAtEachOthersPathsConverge` (`:648`) asserts zero superseded and the moves `item-x->x`, `item-f->f`, `item-x->x`. To run 4 (`-count=200`, own `-run`, no race): ok, 111 s.
- **Finding 4**: the comment of `TestRunOnce_FolderMovedOnOneDriveIntoAChainOfNewFoldersConverges` (`:397`) says x/sub is made in the first pass inside the old x, carried to x2/sub and moved back the next pass.
- **Note 12**: the header of `planner_move_composition.go`, the comment at `planner_move_composition_test.go:689` and `planner_dependencies.go:26` reflowed; the history sentence at the last is gone.
- **Note 6**: the comment at `planner_dependencies_test.go:17` says the oracle proves the indexes find every pair the predicates accept, not what a predicate accepts.
- **Notes 8, 10, 11**: `TestRunOnce_FolderMovedOnOneDriveIntoNewFoldersWhereAFolderMovedDeeperLeftConverges` (`:425`); `TestRunOnce_ThreeFoldersMovedHereInARingOfNewFoldersConverge` (`:684`; a moved twice on OneDrive, b and c once, a, b and c created); `TestRunOnce_MoveIntoAFolderMadeHereWhileOneDriveMakesTheRenamedPathConverges` (`:491`): afterwards both sides hold x2/keep.txt and x/f/c.txt; on OneDrive x2 is the folder OneDrive made, keep.txt was moved into it, x is still the item at x and holds f; nothing is created there.
- **Passes per new scenario** (one sync each; counted with a temporary debug-log probe, reverted): two folders 4, ring 4, x moved deeper with x/sub 4 (each at `oneShotMaxPasses`); chain 3; first shape after the collision 3; compound after the collision 3; exact x2 2; deleted file 2.
- **Spec** (`sync-planning.md`, Move Composition): the take-back sentence, "Only those moves land in the folder that waits", the deleted-file sentence, the new test cited. Deviation 3 above corrected to the code.
- **Lint fix**: `verify lint` flagged `planMove` at cyclomatic complexity 17 (the landing record); it moved into `recordLanding`.
- **Checks**: build, `vet`, `vet -tags=e2e,e2e_full,integration,stress` on `cdb02def`: pass. `gofumpt -l internal/sync`: nothing. `go test -count=1 -timeout 60m ./internal/sync/`: ok on `cdb02def` (454 s) and on `b0bb03df` (414 s). `go test -race -count=1 -timeout 90m ./internal/sync/`: ok on `b0bb03df` (4176 s; `cdb02def` differs only by the `recordLanding` extraction); `-race -run "TestComposeMoves_|TestBuildDependencies_|TestRunOnce_"` on `cdb02def`: ok (1730 s). `go run ./cmd/devtool verify lint` on `cdb02def`: pass (lint and workflow-lint).
- **Live proof** (`w79live.py`, test account, installed `cdb02def`; the same four on `b0bb03df` before): `pair`, `kept`, and two new cases, `nested` (the compound shape) and `ring`. `kept` and `nested` exit 1 while the collision lasts, f still at f and not inside the old x on OneDrive, then exit 0 in one sync after it; `pair` and `ring` exit 0 in one sync. Every case: trees equal, every folder still the item it was, a quiet sync after, no dependency-cycle lines.
- **Seen, not fixed**: with the take-back on, case 3 of `remoteLandingWaits` (`:1180`) is no longer the only guard of its shape, so no test fails with it alone removed. The four "Not fixed" items above and notes 5, 7, 9 and 13 stand.

## Second review fixes

Commit `46dba1f7` on `0e31e63c` (amended, one commit, 7 files), installed. Against `cdb02def`: 5 files; `planner_move_composition.go` +51 −26 (code and comments), the rest tests and spec. "Red" means a run of the new tests against `cdb02def`'s `planner_move_composition.go` through `go test -overlay`; mutants replace that file the same way (scratchpad `W79c-mut/`).

- **Finding 1** (a file holder): reworded, behaviour kept. `remoteLandingWaits` (`planner_move_composition.go:1170`), `sync-planning.md:569-571`, the test comment (`planner_move_composition_test.go:991`) and the commit message say the move waits for a file or a frozen folder still to be decided, a file even when it then moves. The `filecase.go` alternative is refused: measured as a mutant of the final tree, it plans `f->a/f` into the path OneDrive's file a keeps in the file-and-folder shape below, and that scenario never finishes.
- **Finding 2**: case 3 of `remoteLandingWaits` (`case !e.movedOnOneDrive(holder)`) deleted; the take-back is the one rule for a holder decided without an OneDrive move, and the comment says so. New row "f's destination held by an item that swaps with f, x held in a new b" (`planner_move_composition_test.go:914`). Red: FAIL (`x/f->x/f.swap-1` planned). Green: PASS. With the note 1 check switched off it still passes, so it pins the deletion.
- **Finding 3**: `TestRunOnce_MoveIntoAFolderMadeHereWhileTheOldFolderWaitsForADeleteConverges` (`engine_move_recreate_test.go:561`, in `runOnceScenarios()`): one sync, zero superseded, moves `item-a->b`, `item-x->x`, `item-f->f`, delete `item-w`, create `x`, both trees equal, x the item at `b/x`. The old row at :845 now has the inputs SQLite produces ("x waiting for a file deleted in a renamed a, deferred after f", `:893`: remote `[a->b]`, x and f deferred). Test only: both pass on `cdb02def`; with the take-back off both fail (`item-f->f` sent twice). `assertOneSyncSends` (`:600`) also serves the deleted-file scenario.
- **Finding 4**: row "m into a new b, b into a new c, c waiting for a file deleted in a renamed p" (`:903`; remote `[p->q]`, b, c, m deferred). Test only: passes on `cdb02def`; with one rerun it fails, and so does `TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileTheOldFolderWaitsInANewFolder`, which takes two take-backs now that case 3 is gone.
- **Note 1**: fixed. A move refused by `remoteLandingWaits` is recorded in `remoteHeld` (`:1163`) and deferred at once by `deferRefusedMove` (`:1006`), so no item is parked for it. Row "f's destination held by an item that swaps with f, x kept where OneDrive has it" (`:864`). Red: FAIL (`x/f->x/f.swap-1`). Green: PASS. With only this check off, that row alone fails in the focused set. OneDrive only: on this computer the park of `TestComposeMoves_FolderAMoveLandsInIsNotParkedToBreakASwap` saves the next pass.
- **Note 3**: rows for each probed shape (ring of four; ring of three with g into the waiting b; x into a new f/sub with f into a new x; a new X with x kept and with x moved away), the row "on OneDrive, with the new X" in `TestBuildDependencies_MoveIntoThePathAnotherMoveVacatesWaitsForIt`, and `TestBuildDependencies_MoveIntoAFolderMadeAtADeletedPathWaitsForTheDeleteAndTheCreate` (x deleted, x, X or x/sub made: delete, then create, then move). Test only: all pass on `cdb02def`.
- **Note 5**: not done: no benchmark composes a plan that takes moves back; one would be a change of its own.
- **Note 7**: corrects "Not fixed" item 3 above: by reading it no longer holds. A parked item ends the run deferred, so the take-back takes back a move into its path, and a move refused for its holder is deferred before any park.
- **Note 8**: the Move Composition block and the Dependency Build paragraph of `sync-planning.md` reflowed (78 columns); the history sentence now says what the build costs and what the scanning oracle is.
- **Notes 2, 4, 6**: nothing to change.
- **Also fixed** (seen here): a file and a folder each moved here into a new folder at the other's path, the folder decided first, never converged. Probe, three syncs each: on `main` every pass dropped the moves as a dependency cycle; on `cdb02def` nothing moved and two items stayed will-retry. A file still to be decided is now recorded in `loopHeld` as the first of partner moves (`:1203`), so its move into the old folder is not taken back. Row "a file a into a new f, f into a new a, f decided first" (`:949`) and `TestRunOnce_FileAndFolderMovedHereIntoNewFoldersAtEachOthersPathsConverge` (`engine_move_recreate_test.go:741`, in `runOnceScenarios()`). Red: row FAIL (a deferred too), scenario FAIL ("sync never finished"). Green: both PASS (one sync, zero superseded, `item-a->a`, `item-f->f`, `item-a->a`, creates a and f).
- **Red run of all four behaviour tests** on `cdb02def`'s planner: exactly those four fail; every other test of the focused set passes there.
- **Test layout**: `heldRemoteLandingCases()` is three functions (kept or moved, taken back, partners) to stay under the length limit; the two-folder scenario and the new one share `moveEachIntoANewFolderAtTheOthersPath`.
- **Checks** on `46dba1f7`: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l`: nothing. `go test -count=1 -timeout 60m ./internal/sync/` (lock): ok, 372 s. The mutant verdicts above were measured on the final tree. `go test -race -count=1 -timeout 90m -run "TestComposeMoves_|TestBuildDependencies_|TestRunOnce_|TestDropReconciliation|TestReverseDispatch" ./internal/sync/` (lock): ok, 3310 s. List tests (`ListsEvery|Table|Governs|Exhaustive|Registered`): ok. `RealRepoIsClean` (devtool): 3 PASS. `go run ./cmd/devtool verify lint` (lock): pass, 0 issues (log in the scratchpad, `W79c-lint.log`).
- **Live proof** (`w79live.py`, test account, installed `46dba1f7`, one case at a time): `pair` and `ring` exit 0 in one sync; `kept` and `nested` exit 1 while the collision lasts, f still at f and not inside the old x on OneDrive, then exit 0 in one sync after it. Every case: trees equal, every folder still the item it was, a quiet sync after, no dependency-cycle lines.
- **Seen, not fixed**: "Not fixed" items 1, 2 and 4 above stand.

## Third review fixes

Commit `19bbf5d0` on `9e5498a5` (rebased from `0e31e63c`, one commit, 11 files against `main`). Against `46dba1f7`, the branch's own delta: `planner_move_composition.go` (one condition in `isLoopPartner`, the new delete rule, comments), `planner_actions.go` (+5), `actions.go` (comment), tests in four files, `sync-planning.md` and R-2.17.2 in `spec/requirements/sync.md`. Every new test was seen failing on the rebased `46dba1f7` before the product change (scratchpad `W79d-red1.log`, `W79d-red2.log`, `W79d-red3.log`).

- **Rebase and the fifty-times rule** (#939): the rebase onto `9e5498a5` was clean. No test of this branch, old or new, matches the sync step's placement rule (`verify_stress_members.go:54-62`: none starts with a listed prefix, none holds Shutdown, Drain, Concurren or Cancel), so nothing is declared; `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` passes.
- **Finding 1** (a move into a file): confirmed and fixed as the reviewer's first proposal. `isLoopPartner` (`planner_move_composition.go:696`) now answers true only for a folder holder: the partner's move lands in the holder on OneDrive this pass, and a file there refuses it on every pass. Checking at the use covers both places that record `loopHeld` (the pending-file case and the `remoteLandingLoops` case of `remoteLandingWaits`, `:1203`), which recording only for a folder key would also have needed. Neither variant can delete or overwrite a file: both turn a refused move into a quiet wait. Rows "two files each into a new folder at the other's path" and "a ring through a file: b into a new a, a into a new g, g into a new b" (`planner_move_composition_test.go:987`, `:995`). Red: `[a->b/a]` with b deferred, and `[g->b/g a->g/a]` with b deferred. Green: nothing moved, every item deferred. `TestRunOnce_TwoFilesMovedHereIntoNewFoldersAtEachOthersPathsWait` (`engine_move_recreate_test.go:953`, in `runOnceScenarios()`): red, each of two syncs superseded actions; green, two syncs, zero superseded, zero failed, work left, nothing sent to OneDrive, both files where they were. These shapes still never converge (the reviewer's note 4): main and `cdb02def` do not either, and nothing in this change claims it.
- **Finding 2** (data loss): confirmed, and fixed more widely than proposed. The proposed place, `dropReconciliationOfComposedMoves`, runs before normalization, where a local delete still carries OneDrive's path, so the rule runs after normalization instead: `withholdDeletesAboveHeldMoves` (`planner_move_composition.go:1906`, called at `planner_actions.go:113`). On each side it holds back every delete at or above the path where an item sits that is still to be moved on that side, with every action that waits for those deletes (`buildDependencies` and `withDependents`, built only when a delete is held), and counts them in `Dropped` and `Unfinished`. "Still to be moved" (`keysStillToMove`, `:1965`) is any item in `deferredMoves`, whatever held it (the folder holder, the take-back, the final sweep, a frozen item, a parked item, whose temporary name is beside its source), plus any composed move the plan no longer holds; I found one step that drops one, `trimKeepBothDescendants`, which removes a move below a keep-both path while the delete of its source folder stays. A delete the mode does not run is left to the mode partition. Other paths checked and found safe: the mode partition admits a side's moves and deletes together; cycle isolation drops a delete with the move it waits for; the other drops remove creates only; a folder delete here removes only an empty folder (`executor_delete.go:141-177`), so the mirror case failed rather than lost data.
  - Rows in `TestPlanBuiltActions_ADeleteAboveAMoveHeldForTheNextPassWaitsWithIt` (`planner_move_composition_test.go:1258`): a held OneDrive move out of P (P held, P/old.txt and Pa run); two folders deep (P and P/Q held); the mirror on this computer; an upload at P's path held with P's delete; download-only (deferred by the mode, nothing held); the keep-both trim. Red: every row but download-only failed (the trim row against a variant without the trim part).
  - `TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaits` (`engine_move_recreate_test.go:615`, three cases: held for x, two folders deep, taken back while x waits in a new y). Red: P deleted on OneDrive in the first sync, c.txt recycled, f uploaded again as a new item. Green: while the collision lasts nothing is deleted, moved or made on OneDrive and f stays in P; once it is gone, one sync, zero superseded, both trees equal, f the same item at x/f, P (and Q) deleted after it, nothing recycled or uploaded, quiet next plan.
  - `TestRunOnce_AFolderDeletedOnOneDriveStaysHereWhileAMoveOutOfItWaits` (`:743`), the mirror the reviewer did not measure. Red on `46dba1f7`: the local delete of P failed with "folder still contains files that are not on OneDrive (f)". Green: one sync, zero superseded or failed, both trees equal, nothing downloaded again or sent. Whether `main` fails the same way: unknown, not measured.
  - R-2.17.2 now says a folder deleted on OneDrive is deleted here after the moves out of it, and that a delete waits with such a move; `sync-planning.md` says it beside the drop of creates and in the `Dropped` list.
- **Should fix 3**: the header comment (`planner_move_composition.go:41-78`) says the move waits for an item at its target or a file or frozen folder still to be decided, and through the take-back when the item is decided without an OneDrive move before or after it; which of two items waits (a folder; two files both wait); that a move held for its folder's path is deferred at once (`deferRefusedMove`); the new delete rule; and that the two-file loop is never broken.
- **Notes**: 1 fixed (`sync-planning.md`, "The move waits while the item is at its target there, or is a file or a frozen folder still to be decided, even a file that then moves in the same pass"); 2 fixed (`remoteHeld` field comment); 3 and 4 not fixed (a chain of k take-backs costs k runs, unmeasured; the file loops never converge); 5 nothing to do.
- **Also fixed**: `deleteSide` is registered in `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum`; the `planFollowUp.Dropped` comment and `planBuiltActions` comment name the new drop.
- **Checks** on `19bbf5d0`: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l` and `goimports -l`: nothing. Focused set (`TestComposeMoves_|TestBuildDependencies_|TestPlanBuiltActions_|TestDropReconciliation|TestPlannerPlanCurrentState_|TestRunOnce_|ListsEvery|Table|Governs|Exhaustive|Registered|TestActionType|TestPassWithoutActions|TestReverseDispatch`): ok. Under the lock: `go test -count=1 -timeout 60m ./internal/sync/`: ok, 812 s; `go test -race -count=1 -timeout 150m ./internal/sync/`: ok, 4291 s; `go run ./cmd/devtool verify lint`: pass, 0 issues, workflow-lint pass (scratchpad `W79d-plain.log`, `W79d-race.log`, `W79d-lint.log`). devtool `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` and the four `RealRepoIsClean` tests: PASS.
- **Live proof** (`w79dlive.py`, a copy of `w79live.py` with two more cases; test account, synthetic data in a private tree, installed `19bbf5d0`, one case at a time): `pair` and `ring` exit 0 in one sync; `kept` and `nested` exit 1 while the collision lasts, f still at f and not in the old x on OneDrive, then exit 0 in one sync. New `files` (two files each moved into a new folder at the other's path): five syncs each exit 1 ("2 will be retried"), nothing moved or made on OneDrive, both files still the same items at their paths and no folder took them, no dependency-cycle, error or warning lines. New `deleted` (x renamed x2, a new x, the file moved out of P into it, P removed here, X2 made on OneDrive): the sync while X2 lasts exits 1 with P and the file still on OneDrive, the file still in P as the same item; after X2 is removed, one sync exits 0, moves x and then the file, deletes P after it; trees equal, the file is the same item at x, a quiet sync after. The recycle bin cannot be listed on a personal account, so what entered it is unknown; the file's survival is read from OneDrive by path and item. Every case: no dependency-cycle lines, test folder removed.
- **Install**: `19bbf5d0` was installed before the live proof. At 02:16 another branch's newer build (`7dcc4cba`) replaced it; it was not installed again, so the watch client keeps that work, and `CLAUDE.md` still names `7dcc4cba`, which is what runs.
- **Seen, not fixed**: notes 3 and 4 of the review; "Not fixed" items 1, 2 and 4 above stand.

## Fourth review fixes

Commit `18ec70dd` on `9e5498a5` (main had not moved; amended, one commit). Against `19bbf5d0`: `planner_move_composition.go` (the carry rule, the log line, comments), `planner_actions.go` (the logger passed in), `actions.go` (comment), tests in three files, `sync-planning.md`. "Red" means a run of the new tests against `19bbf5d0`'s `planner_move_composition.go` and `planner_actions.go` through `go test -overlay`; mutants replace the current file the same way (scratchpad `w79e-ov/`, probes in `w79e-ov/zz_w79e_probe_test.go`).

- **Finding 1** (blocking): confirmed by the probe (each sync while X2 lasts sent only the delete of a1.txt; OneDrive kept A, A/P/f and no a1.txt). Fixed as proposed, and narrower than the measured overlay, which cleared every dependency of such a move: `withoutWorkTheMoveCarries` (`planner_move_composition.go:1961`) removes, before the closure, only the edges by which a move waits for work strictly below its source that does not follow it (the second rule of `moveOrderDependency`), and keeps such an edge when it also orders the work first (`movePrecedes`: the move lands at or below the destination of a move it waits for, where OneDrive still keeps the held path).
  - Rows (`planner_move_composition_test.go`, `carriedHeldDeleteCases`, :1266): "A renamed B here carries P, which holds f, and B/a1.txt edited is uploaded after it" and "A renamed B on OneDrive carries P here, which holds f". Red: the OneDrive row kept only `remote_delete A/a1.txt` (dropped 4), the local row kept nothing (dropped 3). Green: PASS.
  - Row "A moved into p/A, where c leaves A for p, the same name as P on OneDrive, waits with c's move": pins the exception. With a mutant that cuts every edge to work below the source, the row fails (`remote_move p/A` kept, which would land in the folder OneDrive keeps at P). A first form of this row, with c moved onto P itself, was not a reachable input: normalization keeps P as the parent of P/A and drops its delete.
  - `TestRunOnce_AFolderRenamedHereIsRenamedOnOneDriveWhileAMoveOutOfAFolderDeletedInItWaits` (`engine_move_recreate_test.go:788`, in `runOnceScenarios()`): two syncs while X2 lasts, each with zero superseded, A at B on OneDrive, the edited a1.txt and new.txt there, f still in B/P, only a1.txt's old copy deleted and in the recycle bin; then one sync, zero superseded, both trees equal, f the same item at x/f, deletes `item-a1`, `item-P`, moves `item-A->B`, `item-x->x2`, `item-f->f`, two uploads, a quiet plan. Red: in both collision syncs the rename, the edited and the new file on OneDrive and f in B/P fail; zero superseded, the delete of a1.txt and the recycle bin pass on both trees (`19bbf5d0` deletes a1.txt too), and so does the end state.
  - `TestRunOnce_AFolderRenamedOnOneDriveIsRenamedHereWhileAMoveOutOfAFolderDeletedInItWaits` (`:846`, listed): the mirror, one sync, only the edited a1.txt downloaded, nothing sent. It passes on `19bbf5d0` too (the reviewer's mirror probe converged there), so it is a guard for the local side of the carry rule.
- **Should-fix 2**: fixed. Each delete the hold keeps back logs at Debug `delete waits for the move of an item inside it` with `action`, `path` and `item_path` (the first held item below that folder in path order; `pathsAboveHeldMoves` now maps each path to it). `TestPlanBuiltActions_AHeldDeleteLogsTheItemItWaitsFor` (`:1369`): P/Q and P each log `item_path=P/Q/f`, R, which runs, logs nothing. Red: FAIL (no line).
- **Should-fix 3**: `TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaitsBelowAKeptConflict` (`engine_move_recreate_test.go:891`, listed): one sync, zero superseded, f moved once into the conflict copy of K and still the item it was, P deleted, OneDrive keeps its edited K, nothing in the recycle bin. Correction to the review's wording: K's old version is in K's version history, not the recycle bin (the probe's helper counts both). Red with the trim part of `keysStillToMove` removed: one action superseded and f made again as a new folder. It passes on `19bbf5d0`.
- **Note 4**: kept as a defensive step, and the comment of `pathsAboveHeldMoves` says so and what it costs. Measured with a trace: Q moved out of P here to R/Q, f moved from R/Q into a new x (held), P deleted. Pass 1 holds P's delete by the walk alone (above f's OneDrive path P/Q/f) while Q's move runs; pass 2 of the same sync deletes P. The sync ends as it would without the walk; the walk costs that one pass.
- **Note 5**: the header comment now names the ring through a file. The words "converge on later passes" are in no file of the branch (commit message, code, spec), so nothing else to correct.
- **Note 6**: "a parked item included" removed from `sync-planning.md` and from `keysStillToMove`'s comment.
- **Notes 7, 8**: nothing to change.
- **Also**: rows for the rule's sentences (below): the move of P/g out of P added to the first row (it runs), "a file made here at p, the same name as P on OneDrive, waits with P's delete", "f's move planned: P's delete runs after it". The `Dropped` comment (`actions.go`) and list (`sync-planning.md`) say a folder's move that carries a held delete is not dropped; the spec paragraph says the carry rule and the log line and cites the new tests.

### The rule the code now follows

1. **Held**: on each side, a delete of the path where an item sits that is still to be moved on that side after this plan (its move held for a later pass for any reason, or its composed move removed by the keep-both trim), or of any folder above that path, when the mode runs the delete.
2. **Taken with it**: everything that waits for a held delete, directly or through other actions: the deletes above it, a create at its path or at a name OneDrive takes for it, a move onto such a path and a folder's move into that move's destination, and what waits for those.
3. **Never held**: a delete of any other path; a delete the mode does not run (the mode partition defers it); a folder's move on the same side whose source lies above held work, unless it also lands in the destination of a held move: it carries the folder and the item along, and the next pass plans the delete at the new path; and whatever the held actions wait for, which runs.
4. **Released**: by the pass whose plan makes the item's move: nothing is held for it, and the delete waits for that move through the ordinary dependency.

Each sentence against tests: (1) rows "P deleted here holds f" (OneDrive), "P deleted on OneDrive holds f here", "P and P/Q deleted here hold f", the keep-both trim row, "download-only holds P's delete back itself"; RunOnce `...DeletedHereStaysOnOneDrive...` (three cases), `...DeletedOnOneDriveStaysHere...`, `...BelowAKeptConflict`. (2) rows "a file made here at P's path", "a file made here at p" (new), "A moved into p/A ..." (new), "P and P/Q". (3) the first row (P/old.txt, Pa and the move of P/g run), the download-only row, both carrier rows (new), the p/A row for the exception; both RunOnce rename scenarios (new). (4) row "f's move planned: P's delete runs after it" (new); every RunOnce scenario after its collision. Every row and scenario above passes on `18ec70dd`.

### What the hold removes, and whether anything kept acts as if it ran

- **The held delete and the deletes above it.** Kept: what they wait for, the deletes of other paths below (P/old.txt) and the moves out of the folder (P/g), each complete without the folder's delete; and the folder's move above (A to B), which carries the folder. SQLite's baseline update of the held item is dropped with it (`dropReconciliationOfComposedMoves`). Nothing kept acts on the delete having run.
- **A create at the held path, or at a name OneDrive takes for it, and what lies below it.** Kept: its parent's create, harmless alone, and, not safe, the other half of a rename SQLite could not pair. **Not fixed (new with the hold)**: here P/f moved into a new x whose rename collides, P removed, and the file z.txt renamed P. SQLite does not pair z.txt to P as a move while OneDrive's P is another item (`sqlite_compare.go:94`), so it plans a delete of z.txt and an upload at P; the hold takes the upload, and the delete runs. Probe (branch, before and after this fix): each sync while X2 lasts deletes z.txt on OneDrive and uploads nothing, so OneDrive holds no copy of z.txt's content for as long as the collision lasts; once X2 is gone one sync converges (the content at P, f the same item at x/f). Main: the first sync moves f into the old x (the original defect), deletes z.txt and P and uploads P, so OneDrive always holds the content. Closing it means holding also the OneDrive delete of the synced file whose local identity a held upload carries; that holds more deletes, which this round was not to do, so it is left for a decision. Probed variants: a folder Z renamed P is safe on both trees (SQLite keeps P and moves Z's files into the old P); a folder Z renamed p, with a file in it edited, deletes that file on OneDrive in the first sync and never finishes, on main exactly as on the branch (pre-existing).
- **A move onto such a path, a folder's move into its destination, and the work below them.** Kept: the work below the move's source that it waits for, which is the same split when a file in the moved folder was edited (its upload lands below the held destination). By reading, it is reachable only through a name OneDrive takes for the held path (`sqlite_compare.go:94` never pairs a move onto a path another OneDrive item holds), and measured with p it is the pre-existing case above.
- **Seen, not fixed (pre-existing)**: on OneDrive z.txt renamed onto P after P/f moved out and P deleted: main never finishes (the local P stays, "folder still contains files that are not on OneDrive"); the branch finishes in one sync and uploads z.txt again under its old name, as main does with no held move at all (P empty). Cause unknown.

- **Checks** on `18ec70dd`: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l` and `goimports -l` on `internal/sync`: nothing. Under the lock: `go test -count=1 -timeout 60m ./internal/sync/`: ok, 487 s (scratchpad `W79e-plain.log`); `go test -race -count=1 -timeout 120m -run "TestComposeMoves_|TestBuildDependencies_|TestPlanBuiltActions_|TestDropReconciliation|TestPlannerPlanCurrentState_|TestIsolateDependencyCycles|TestRunOnce_AFolder" ./internal/sync/`: ok, 214 s (`W79e-race.log`); `go run ./cmd/devtool verify lint`: pass, 0 issues, workflow-lint pass (`W79e-lint.log`). devtool `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` and `TestFindTestConventionViolations_RealRepoIsClean`: PASS (no new test matches a stress-lane rule). Not run: a whole-package race run (it passed on `19bbf5d0`) and any full `devtool verify`. Red runs and mutants as above.

- **Live check** (`w79elive.py`, a copy of `w79dlive.py` with a `renamed` case and the message text of warning lines; test account, synthetic data in a private tree under the scratchpad as in the earlier rounds, since a tree inside `~/sync-test-root` would be uploaded by the running watch client with the copied sign-in; the branch binary built into the scratchpad and not installed, as another branch holds the installed build; one case at a time):
  - `renamed` (new, run twice, the second time to read the warnings): A holding a file and P holding a file; here `mv A B`, B's file edited, a new file in B, `mv x x2`, `mkdir x`, the file moved from B/P into x, `rmdir B/P`, and X2 made on OneDrive. Sync 1 exits 1: it deletes the old copy of A's file, moves A to B on OneDrive (the same item) and uploads the edited file (10 B) and the new one; P is still at B/P with the file in it, both the same items. Sync 2 exits 1 and sends nothing. The only warnings are the collision's two ("Not syncing x2: conflicts with synced file X2" and its child). After X2 is removed one sync exits 0: x moved to x2, x made, the file moved into it as the same item, B/P deleted; both trees equal, a quiet sync after.
  - `deleted` (the last round's data-loss case): the sync while X2 lasts exits 1 with P and the file in it still on OneDrive as the same items; after X2 is removed one sync exits 0, moves x and then the file, deletes P; both trees equal, a quiet sync after.
  - `files` (the last round's move-into-a-file case): five syncs each exit 1 ("2 will be retried"), nothing moved or made on OneDrive, both files the same items at their paths and no folder took them.
  - Every case: no dependency-cycle, error or stale-precondition lines; the test folder and the private tree removed. The recycle bin cannot be listed on this account, so what entered it is unknown; survival is read from OneDrive by path and item.
- **Install**: none (another branch holds the installed build), so no `CHANGELOG.md` entry and `CLAUDE.md` unchanged. The scratch comparison worktree on `main` and the live binary are removed.

## Exhaustive check

Commit `0c640cb0` on `238bea94` (origin/main; rebased from `9e5498a5` onto `d339dd23` first, then onto `238bea94` when #942 and #943 merged, neither touching sync code; one commit). Not pushed, no PR.

### The check

`TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge` (`internal/sync/planner_exhaustive_test.go`, marked exhaustive with `skipInShort`):

- **Universe**: folder `p` holding file `f`, empty folder `x`, file `z`, synced. Two starts: that tree, and the same tree after this row's own steps (`mv x x2; mkdir x; mv p/f x/f` here, `mkdir X2` on OneDrive), so that x's move and f's wait while X2 lasts.
- **Actions per side** (`exEditsOn`): every entry renamed to a fresh name (`g`/`q`; on OneDrive also `G`/`Q`, which OneDrive takes for `g`/`q`), moved into each other folder under its own name, or moved onto each synced path that side no longer holds; deleted; each file edited; a file made in each folder; a synced folder made again at its path. A rename onto a path the other side changed is one of these moves. Histories of none, one or two actions per side; at most three in all from the synced tree, two beyond the start from the held one.
- **Pruning, said in the test**: histories leaving a side in the same state (same entries, each the same synced item or a new one, same kind of content) count once; a pair whose actions split into groups touching unrelated paths (no path of one at, above or below one of another, compared as OneDrive compares names) is not run, as each group is a shorter pair of its own. Result: 5,275 pairs (3,731 synced, 1,544 held). Under the race detector one pair in 40 runs.
- **Execution**: the real planner (`planCurrentStateForStoreWithLogger`, the planner model's store writes, `pmState.writeStore`) and a model of the executor and OneDrive (`exWorld`) built from the planner model's executor: an action whose dependency failed does not run (`completeSubtree`); OneDrive refuses a name a sibling takes as it compares names; a local folder delete removes only an empty folder and a local file delete only the synced content; a remote folder delete refuses a folder holding an item sync has not seen or a changed file (`validateRemoteFolderDeleteChildren`), and refuses while the entry is back here; a folder conflict copy and a vanished source behave as `ExecuteConflictCopy`; local folder creates make missing parents. A full scan's case-collision skip is computed with `detectCaseCollisions` and written as observation issues. Passes run until a plan has nothing to do, a pass changes nothing, or 6 passes; then, while a name OneDrive takes for another entry's stays on both sides, OneDrive's user renames that item and the passes continue.
- **Invariants**: (a) no pass removes from a side content the user did not delete or overwrite, unless the pass keeps it in a conflict copy or as a version an upload replaced; (b) the sides end equal at a plan with nothing to do, within the limit, and nothing the user wrote is gone from both; (c) that plan defers and drops nothing; (d) a synced item deleted on one side, nothing made there again, the other side leaving it and everything above and below it as synced, is gone from both.
- **Known failures**: the pairs that break an invariant, and which, are the checked-in `internal/sync/testdata/exhaustive/pairs.golden` (316 lines), compared exactly; `-update` rewrites it, as the reconcile matrix does.
- **Speed**: the store opens through a test driver that keeps one prepared SQLite statement per query and connection (`exCachingDriver`); preparing the comparison SQL was 86 % of a plan. Measured: 95 to 229 s for the whole space on this 2-core host, depending on other load (the package's plain run took 475 s in all).
- **Stress tier**: no new test matches the sync step's placement rule (no listed prefix, no Shutdown, Drain, Concurren or Cancel); `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` passes; nothing declared.

### Results

Default space, 5,275 pairs:

| Tree | Pairs breaking an invariant | Of them passing on main |
|------|-----------------------------|-------------------------|
| main `d339dd23` (the two later merges touch no sync code) | 344 | n/a |
| branch before (the previous commit, rebased) | 357 | 16 |
| branch now | 316 | 0 |

- The 16 that failed only on the branch before were all a pass removing content: a file renamed here onto a path a waiting move leaves (onto `p`, onto `p/f`), deleted on OneDrive while its upload waited. Ids: `held L[mv z p/f] R[]`, `held L[mv z p/f] R[mv p Q]`, `held L[mv z p/f] R[mv p q]`, `held L[mv z p/f] R[edit p/f]`, `held L[mv z p/f] R[new X2/G]`, `held L[mv z p/f] R[new X2/g]`, `held L[mv z p/f] R[new x/G]`, `held L[mv z p/f] R[new x/g]`, `held L[mv p x/p, mv x/f p] R[]`, `held L[mv p x/p, mv z p] R[]`, `held L[rm p, mv z p] R[]`, `held L[mv x/f x/g, mv z p/f] R[]`, `held L[mv z x/z, mv x/f z] R[]`, `held L[mv z p/f, mv p q] R[]`, `held L[mv z p/f, new x/g] R[]`, `held L[mv z p/f, new x2/g] R[]`.
- 28 pairs main fails now pass, all a pass removing content (listed in the main list below as the lines not in `pairs.golden`).
- 9 pairs fail on both with different invariants. The branch breaks fewer on 5 (`synced L[mv p x/p, mv x/p/f p] R[mv x p/x]`, `synced L[mv p x/p, mv z p] R[mv x p/x]`, `synced L[mv x p/x, mv z x] R[mv p x/p]`, `held L[mv x p/f, mv z x] R[]`, `held L[mv x2 x/x2, mv z p/f] R[]`: no content removed). It adds `no-quiet-plan` on 4 (`synced L[mv z p/z, mv p z] R[rm p]`, `synced L[mv z p/z, mv p z] R[rm p/f]`, `held L[mv x x2/x, mv x2 p/f] R[]`, `held L[mv x p/f, new p/f/g] R[]`): traced for the first and the last, it plans an upload below a file OneDrive keeps at that path while that file's move waits, refused on every pass, where main drops the actions as a dependency cycle and ends quiet; neither converges. The other two are untraced.

Wider space, run once and not committed: every pair of up to two actions per side from the synced tree (22,483) and up to three beyond the start from the held one (23,784), 46,267 pairs. Main breaks 4,898; the branch 4,432; none fails only on the branch; 466 fail only on main (464 a pass removing content). 169 fail on both with different invariants: the branch breaks fewer on 103, adds one on 66 (`no-quiet-plan` 64, `delete-undone` 2: `synced L[mv p/f x/f, rm p] R[mv z x/z, mv x z]`, where main also removes content in a pass, and `held L[rm p, mv x2 x/x2] R[mv X2 x/X2]`). Before the rebased-write part of the rule below, two pairs of this space failed on the branch only: `held L[mv p x/p] R[rm p/f, mv z p/f]` and `held L[mv p x/p, new x/p/g] R[new p/G]`.

### The rule

Stated once in `withholdDeletesOfHeldWork` (`planner_move_composition.go:1979`), in `sync-planning.md` (Move Composition) and in R-2.17.2: on each side, a delete waits while work held for a later pass has its source at or below the deleted path there. Held work is an item's move that waits for a later pass (for any reason) or whose composed move the keep-both trim removed; what another step leaves for the next pass (a create that waits with such a move, the new entry at a path its item was renamed away from; work below a keep-both path; a create onto a path a composed item holds; an action caught in a dependency cycle); and whatever the rule itself holds, which is everything that waits for a held delete. The source of a move is the item's path on its side; the source of a create is the synced item whose content it writes at another path on that side (the file here an upload carries, by local identity; the item OneDrive keeps that a download fetches, by item ID). The delete is released by the pass that plans the held work, and then waits for it through the ordinary dependencies. A folder's move above held work still runs and carries it (unchanged). One addition outside deletes: a write here rebased through a folder whose OneDrive move waits waits with that move (`dropCreatesOntoComposedItems`, `heldFolderRebases`, `rebasedThroughHeldFolder`), as rebased it was recorded below a path OneDrive does not yet give the folder. No fallback to main's behaviour was needed anywhere.

### Code

- `planner_move_composition.go`: `withholdDeletesOfHeldWork` (:1979) replaces `withholdDeletesAboveHeldMoves`, iterating to a fixpoint; `deletesOfHeldWork` (:2012), `heldWorkFrom` (:2044), `pathAtOrBelow`, `carriesContentOf` (:2066); `recreatedSourceCreates` (:1830) now returns the creates that wait (`recreatedSourceVacated` split out); `buildComposedMoveActions` (:1701) returns them; `dropCreatesOntoComposedItems` (:1892) returns what it drops and drops the rebased writes above; header comment.
- `planner_actions.go`: `planBuiltActions` (:95) takes the waiting creates and passes everything held to the rule; `admitWithoutCycles` (:152) applies the rule again to work caught in a dependency cycle; `normalizeCurrentPlanActions`/`trimKeepBothDescendants` return the trimmed actions.
- `planner.go`: `isolateDependencyCycles` (:282) also returns what it removed. `actions.go`: the `Dropped` comment.
- Spec: `sync-planning.md` (the rule, the rebased writes, the `Dropped` list), R-2.17.2.

### Tests added (each seen failing with its part removed, through `-overlay` mutants in the scratchpad `x79-mut/`)

- Rows of `TestPlanBuiltActions_ADeleteAboveAMoveHeldForTheNextPassWaitsWithIt` (`heldWriteDeleteCases`): z renamed onto P/f (upload waiting with f's move); z renamed onto P deleted here (upload held behind P's delete); the mirror, a delete here waiting with a held download; a held upload of another file holds nothing; g made on OneDrive in P, whose move into x waits (rebased download held). Without the content-source part (m1) the first three fail; without the rebased-write part (m2) the last fails.
- `TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveUntilItsUploadRuns` (onto P deleted here; onto f's old path in P): m1 fails both (z.txt deleted on OneDrive in the collision sync).
- `TestRunOnce_AFileMovedOnOneDriveIntoAFolderWhoseMoveWaitsIsMovedHereOnce`: m2 leaves z.txt here and uploads it again as a new file (2 uploads, an extra `z.txt` on both sides).
- `TestRunOnce_AFileMadeOnOneDriveInAFolderWhoseMoveWaitsIsDownloadedOnceItMoves`: m2 downloads g.txt into the new x during the collision. It allows one superseded action after the collision: the download is superseded once when the same pass moves its folder on OneDrive; a probe on main shows the same for a plain move here with a file added in the folder on OneDrive and no collision.
- `TestPlanBuiltActions_AHeldDeleteLogsTheItemItWaitsFor`: log line now `delete waits for work held for the next pass`, attribute `held_path`.
- All four RunOnce tests are in `runOnceScenarios()`.

### Checks (on the final tree unless said)

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l`, `goimports -l` on `internal/sync`: nothing.
- `go test -count=1 -timeout 60m ./internal/sync/` (lock): ok, 475 s, on the commit before the lint fixes (test file only) and a parameter rename; the exhaustive and DSN tests rerun after them: ok.
- `go test -race` on the planner tests by `-run` (lock): ok, 374 s (`-run "TestComposeMoves_|TestBuildDependencies_|TestPlanBuiltActions_|TestDropReconciliation|TestPlannerPlanCurrentState_|TestIsolateDependencyCycles|TestPlannerExhaustive|TestPlannerModel_|TestRunOnce_AFile|TestRunOnce_AFolder|TestRunOnce_MoveInto|TestActionType"`, on `0c640cb0`).
- `go run ./cmd/devtool verify lint` (lock): pass, 0 issues, workflow-lint pass (the first run found three unchecked type assertions, a cyclomatic complexity of 16 and an unused nolint, all in the new test file; fixed).
- devtool `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` and the four `RealRepoIsClean` tests: pass. Every test the design doc cites exists.
- Not run: a whole-package race run, any full `devtool verify`.

### Live check

Test account, binary built from the final product code into the scratchpad (not installed), synthetic data: the sync folder of the run was a new folder under `~/sync-test-root`, its config, state and copied sign-in in a private tree in the scratchpad. One case at a time (`x79live.py`):

- Row's main scenario (`kept`): while X2 lasts, x and f stay where OneDrive has them; after X2 is removed one sync exits 0, trees equal, f and x the items they were, a quiet sync after.
- Renamed onto the held path (`onto-p`: `mv P/f x/f; rmdir P; mv z P`): while X2 lasts, z still on OneDrive as the same item (4 held-delete lines); after, one sync exits 0, trees equal, z's content at P.
- Two pairs that failed before: `pf-q` (`held L[mv z p/f] R[mv p q]`) and `fg-pf` (`held L[mv x/f x/g, mv z p/f] R[]`): the branch keeps z on OneDrive as the same item while X2 lasts and converges in one sync after; the previous commit's binary, same steps: z not on OneDrive after the collision sync, converging after.
- Every case: no dependency-cycle, error or stale-precondition lines; the only warnings are the collision's two; proof folders removed. The running watch client (it syncs `~/sync-test-root`) downloaded one proof folder and holds its local delete with 719 others for confirmation; I removed that local copy by hand.

### Seen, not fixed

- `held L[mv x/f x/g] R[new x/g]` still removes f's content from OneDrive for a pass on both trees: the remote-origin conflict copy and download are rebased to the old x here (`x2/g`), where nothing is, so the copy keeps nothing (by trace in the model; not run live).
- "Not fixed" item 4 above (a folder moved here into a new folder at its own path) stands: the held pairs with `mv x2 x/x2` never converge on either tree.
- The model's executor checks no eTags beyond a folder delete's children; a pass that a real eTag precondition would supersede is not modelled.

### Pairs main fails (`d339dd23`), by id

All 344, each with what it breaks; a line marked `*` passes on the branch (the other 316 are the branch's `pairs.golden`, with the 9 differences above).

```
  synced L[] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[] R[rm p, mv x p] :: no-quiet-plan sides-differ left-held delete-undone
  synced L[] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[] R[rm x, mv p x] :: sides-differ left-held
  synced L[] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[mv p q] R[mv p q, mv x p] :: no-quiet-plan
  synced L[mv p q] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[mv p q] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv p q] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv p x/p] R[mv p x/p, mv x p] :: no-quiet-plan
  synced L[mv p x/p] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[mv p x/p] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv p x/p] R[mv x p/x, mv p/f x] :: sides-differ left-held
  synced L[mv p x/p] R[mv x p/x, mv z x] :: sides-differ left-held
  synced L[mv p x/p] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv p x/p] R[mv z p/z, mv x z] :: sides-differ left-held
  synced L[mv p x/p] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[rm p] R[mv x p/x, mv p x] :: no-quiet-plan sides-differ left-held
  synced L[rm p] R[mv z p/z, mv p z] :: no-quiet-plan sides-differ left-held
  synced L[mv p/f p/g] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv p/f p/g] R[rm p, mv x p] :: sides-differ left-held
  synced L[mv p/f p/g] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv p/f p/g] R[rm x, mv p x] :: sides-differ left-held
  synced L[mv p/f p/g] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv p/f f] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv p/f f] R[rm p, mv x p] :: sides-differ left-held
  synced L[mv p/f f] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv p/f f] R[rm x, mv p x] :: sides-differ left-held
  synced L[mv p/f f] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv p/f x/f] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv p/f x/f] R[rm p, mv x p] :: no-quiet-plan sides-differ left-held
  synced L[mv p/f x/f] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv p/f x/f] R[rm x, mv p x] :: sides-differ left-held
  synced L[mv p/f x/f] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv p/f x/f] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[rm p/f] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[rm p/f] R[rm p, mv x p] :: sides-differ left-held
  synced L[rm p/f] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[rm p/f] R[rm x, mv p x] :: sides-differ left-held
  synced L[rm p/f] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[edit p/f] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[edit p/f] R[rm p, mv x p] :: sides-differ left-held
  synced L[edit p/f] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[edit p/f] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[edit p/f] R[rm x, mv p x] :: sides-differ left-held
  synced L[edit p/f] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv x q] R[mv p q, mv q/f p] :: no-quiet-plan sides-differ
  synced L[mv x q] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv x q] R[rm p, mv x p] :: no-quiet-plan sides-differ
  synced L[mv x q] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[mv x q] R[rm p/f, mv x p/f] :: no-quiet-plan sides-differ
  synced L[mv x q] R[mv x q, mv p x] :: no-quiet-plan
  synced L[mv x q] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[mv x p/x] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv x p/x] R[mv p x/p, mv z p] :: sides-differ left-held
  synced L[mv x p/x] R[rm p, mv x p] :: no-quiet-plan sides-differ
  synced L[mv x p/x] R[rm p, mv z p] :: delete-undone
  synced L[mv x p/x] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[mv x p/x] R[rm p/f, mv x p/f] :: no-quiet-plan sides-differ
  synced L[mv x p/x] R[mv x p/x, mv p x] :: no-quiet-plan
  synced L[mv x p/x] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[mv x p/x] R[mv z x/z, mv p z] :: sides-differ left-held
  synced L[mv x p/x] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[rm x] R[mv p x/p, mv x p] :: no-quiet-plan sides-differ
  synced L[rm x] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[rm x] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[mv z g] R[rm p, mv z p] :: no-quiet-plan sides-differ
  synced L[mv z g] R[rm p/f, mv z p/f] :: no-quiet-plan sides-differ
  synced L[mv z g] R[rm x, mv z x] :: no-quiet-plan sides-differ
  synced L[mv z p/z] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv z p/z] R[rm p, mv x p] :: no-quiet-plan sides-differ left-held delete-undone
  synced L[mv z p/z] R[rm p, mv z p] :: no-quiet-plan sides-differ delete-undone
  synced L[mv z p/z] R[rm p/f, mv z p/f] :: no-quiet-plan sides-differ
  synced L[mv z p/z] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv z p/z] R[rm x, mv p x] :: sides-differ left-held
  synced L[mv z p/z] R[rm x, mv z x] :: no-quiet-plan sides-differ
  synced L[mv z x/z] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[mv z x/z] R[rm p, mv x p] :: no-quiet-plan sides-differ left-held delete-undone
  synced L[mv z x/z] R[rm p, mv z p] :: no-quiet-plan sides-differ
  synced L[mv z x/z] R[mv p/f x/f, mv x p/f] :: sides-differ left-held
  synced L[mv z x/z] R[rm p/f, mv z p/f] :: no-quiet-plan sides-differ
  synced L[mv z x/z] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[mv z x/z] R[rm x, mv p x] :: sides-differ left-held
  synced L[mv z x/z] R[rm x, mv z x] :: no-quiet-plan sides-differ
  synced L[edit z] R[mv z p/z, mv p z] :: sides-differ left-held
  synced L[edit z] R[mv z x/z, mv x z] :: sides-differ left-held
  synced L[new p/g] R[mv p x/p, mv x p] :: sides-differ left-held
  synced L[new p/g] R[rm p, mv x p] :: no-quiet-plan sides-differ left-held delete-undone
  synced L[new p/g] R[rm p, mv z p] :: delete-undone
  synced L[new p/g] R[mv x p/x, mv p x] :: no-quiet-plan sides-differ left-held
  synced L[new p/g] R[rm x, mv p x] :: no-quiet-plan sides-differ left-held
  synced L[new p/g] R[mv z p/z, mv p z] :: no-quiet-plan sides-differ left-held
  synced L[new x/g] R[mv p x/p, mv x p] :: no-quiet-plan sides-differ left-held
  synced L[new x/g] R[rm p, mv x p] :: no-quiet-plan sides-differ left-held delete-undone
  synced L[new x/g] R[mv p/f x/f, mv x p/f] :: no-quiet-plan sides-differ left-held
  synced L[new x/g] R[mv x p/x, mv p x] :: sides-differ left-held
  synced L[new x/g] R[rm x, mv p x] :: sides-differ left-held
  synced L[new x/g] R[mv z x/z, mv x z] :: no-quiet-plan sides-differ left-held
  synced L[mv p q, mv q/f x/f] R[mv x q] :: no-quiet-plan sides-differ
* synced L[mv p q, mv q/f p] R[mv x Q] :: removed-in-a-pass
  synced L[mv p q, mv x q/x] R[mv x q] :: no-quiet-plan sides-differ
  synced L[mv p q, mv x p] R[mv p q] :: no-quiet-plan
  synced L[mv p q, mv z x/z] R[mv x q] :: no-quiet-plan sides-differ
* synced L[mv p q, mv z p] R[mv x Q] :: removed-in-a-pass
  synced L[mv p q, new x/g] R[mv x q] :: no-quiet-plan sides-differ
  synced L[mv p q, mkdir p] R[mv p q] :: no-quiet-plan
  synced L[mv p x/p, mv x p] R[] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv p/f p/G] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv p/f p/g] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv p/f f] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv p/f x/f] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[rm p/f] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[edit p/f] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv x Q] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv x q] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv x p/x] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[rm x] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv z p/z] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[mv z x/z] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[new p/G] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[new p/g] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[new x/G] :: sides-differ left-held
  synced L[mv p x/p, mv x p] R[new x/g] :: sides-differ left-held
  synced L[mv p x/p, mv x/p/f p] R[mv x p/x] :: removed-in-a-pass sides-differ left-held
  synced L[mv p x/p, mv z p] R[mv x p/x] :: removed-in-a-pass sides-differ left-held
  synced L[mv p x/p, mkdir p] R[mv p x/p] :: no-quiet-plan
  synced L[mv p x/p, mkdir p] R[mv x p/x] :: sides-differ left-held
  synced L[rm p, mv z p] R[mv x p/x] :: delete-undone
  synced L[rm p, mv z p] R[mv z p/z] :: delete-undone
  synced L[rm p, mv z p] R[new p/G] :: delete-undone
  synced L[rm p, mv z p] R[new p/g] :: delete-undone
* synced L[mv p/f p/g, mv z p/f] R[new p/G] :: removed-in-a-pass
  synced L[mv p/f x/f, mv x p/f] R[] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv p Q] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv p q] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv p x/p] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[edit p/f] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv x Q] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv x q] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv x p/x] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[rm x] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[mv z x/z] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[new x/G] :: sides-differ left-held
  synced L[mv p/f x/f, mv x p/f] R[new x/g] :: sides-differ left-held
  synced L[rm p/f, mv x p/f] R[mv p/f p/G] :: no-quiet-plan sides-differ
  synced L[rm p/f, mv x p/f] R[mv p/f f] :: no-quiet-plan sides-differ
* synced L[rm p/f, mv z p/f] R[mv p Q] :: removed-in-a-pass
* synced L[rm p/f, mv z p/f] R[mv p q] :: removed-in-a-pass
* synced L[rm p/f, mv z p/f] R[mv p x/p] :: removed-in-a-pass
  synced L[mv x q, mv p x] R[mv x q] :: no-quiet-plan
* synced L[mv x q, mv z x] R[mv p Q] :: removed-in-a-pass
  synced L[mv x q, mkdir x] R[mv x q] :: no-quiet-plan
  synced L[mv x p/x, mv p x] R[] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p Q] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p q] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p x/p] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[rm p] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p/f p/G] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p/f p/g] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p/f f] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv p/f x/f] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[rm p/f] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[edit p/f] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv z p/z] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[mv z x/z] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[new p/G] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[new p/g] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[new x/G] :: sides-differ left-held
  synced L[mv x p/x, mv p x] R[new x/g] :: sides-differ left-held
  synced L[mv x p/x, mv p/f x] R[mv p x/p] :: sides-differ left-held
  synced L[mv x p/x, mv z x] R[mv p x/p] :: removed-in-a-pass sides-differ left-held
  synced L[mv x p/x, mkdir x] R[mv p x/p] :: sides-differ left-held
  synced L[mv x p/x, mkdir x] R[mv x p/x] :: no-quiet-plan
  synced L[rm x, mv p x] R[mv x Q] :: no-quiet-plan sides-differ
  synced L[rm x, mv p x] R[mv x q] :: no-quiet-plan sides-differ
  synced L[rm x, mv p x] R[mv x p/x] :: no-quiet-plan sides-differ
* synced L[mv z g, mv p/f z] R[new G] :: removed-in-a-pass
  synced L[mv z p/z, mv p z] R[] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p Q] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p q] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p x/p] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[rm p] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p/f p/G] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p/f p/g] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p/f f] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv p/f x/f] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[rm p/f] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[edit p/f] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[mv x p/x] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[edit z] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[new p/G] :: sides-differ left-held
  synced L[mv z p/z, mv p z] R[new p/g] :: sides-differ left-held
  synced L[mv z p/z, new g] R[mv z g] :: no-quiet-plan sides-differ
  synced L[mv z x/z, mv x z] R[] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[mv p x/p] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[mv p/f x/f] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[mv x Q] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[mv x q] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[mv x p/x] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[rm x] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[edit z] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[new x/G] :: sides-differ left-held
  synced L[mv z x/z, mv x z] R[new x/g] :: sides-differ left-held
  synced L[mv z x/z, new g] R[mv z g] :: no-quiet-plan sides-differ
  synced L[rm z, mv p z] R[mv z G] :: no-quiet-plan sides-differ
  synced L[rm z, mv p z] R[mv z g] :: no-quiet-plan sides-differ
  synced L[rm z, mv p z] R[mv z p/z] :: no-quiet-plan sides-differ
  synced L[rm z, mv p z] R[mv z x/z] :: no-quiet-plan sides-differ
  synced L[rm z, mv x z] R[mv z G] :: no-quiet-plan sides-differ
  synced L[rm z, mv x z] R[mv z g] :: no-quiet-plan sides-differ
  synced L[rm z, mv x z] R[mv z p/z] :: no-quiet-plan sides-differ
  synced L[rm z, mv x z] R[mv z x/z] :: no-quiet-plan sides-differ
  held L[] R[mv p X2/p, mv X2 p] :: no-quiet-plan sides-differ
  held L[] R[mv p x/p, mv x p] :: no-quiet-plan sides-differ left-held
  held L[] R[mv p/f X2/f, mv X2 p/f] :: no-quiet-plan sides-differ
  held L[] R[mv p/f x/f, mv x p/f] :: no-quiet-plan sides-differ
  held L[] R[mv x p/x, mv p x] :: sides-differ left-held
  held L[] R[rm x, mv p x] :: sides-differ left-held
  held L[] R[mv z X2/z, mv X2 z] :: sides-differ left-held
  held L[] R[mv z p/z, mv p z] :: sides-differ left-held
  held L[] R[mv z x/z, mv x z] :: no-quiet-plan sides-differ left-held
  held L[mv x q] R[mv x q] :: no-quiet-plan sides-differ
  held L[mv x p/x] R[mv x p/x] :: no-quiet-plan sides-differ
  held L[mv x p/f] R[] :: sides-differ left-held
  held L[mv x p/f] R[mv X2 Q] :: sides-differ left-held
  held L[mv x p/f] R[mv X2 q] :: sides-differ left-held
  held L[mv x p/f] R[mv X2 p/X2] :: sides-differ left-held
  held L[mv x p/f] R[mv X2 x/X2] :: sides-differ left-held
  held L[mv x p/f] R[rm X2] :: sides-differ left-held
  held L[mv x p/f] R[mv p Q] :: sides-differ left-held
  held L[mv x p/f] R[mv p q] :: sides-differ left-held
  held L[mv x p/f] R[mv p X2/p] :: sides-differ left-held
  held L[mv x p/f] R[mv p x/p] :: sides-differ left-held
  held L[mv x p/f] R[edit p/f] :: sides-differ left-held
  held L[mv x p/f] R[mv x Q] :: sides-differ left-held
  held L[mv x p/f] R[mv x q] :: sides-differ left-held
  held L[mv x p/f] R[mv x X2/x] :: sides-differ left-held
  held L[mv x p/f] R[mv x p/x] :: sides-differ left-held
  held L[mv x p/f] R[rm x] :: sides-differ left-held
  held L[mv x p/f] R[mv z X2/z] :: sides-differ left-held
  held L[mv x p/f] R[mv z x/z] :: sides-differ left-held
  held L[mv x p/f] R[new X2/G] :: sides-differ left-held
  held L[mv x p/f] R[new X2/g] :: sides-differ left-held
  held L[mv x p/f] R[new x/G] :: sides-differ left-held
  held L[mv x p/f] R[new x/g] :: sides-differ left-held
  held L[mv x/f x/g] R[new x/g] :: removed-in-a-pass
  held L[mv x2 q] R[mv x q] :: no-quiet-plan
  held L[mv x2 p/x2] R[mv p x/p] :: sides-differ left-held
  held L[mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv X2 Q] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv X2 q] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv X2 p/X2] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv X2 x/X2] :: sides-differ left-held
  held L[mv x2 x/x2] R[rm X2] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p Q] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p q] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p X2/p] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p x/p] :: sides-differ left-held
  held L[mv x2 x/x2] R[rm p] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p/f p/G] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p/f p/g] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p/f f] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p/f X2/f] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv p/f x/f] :: sides-differ left-held
  held L[mv x2 x/x2] R[rm p/f] :: sides-differ left-held
  held L[mv x2 x/x2] R[edit p/f] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv z X2/z] :: sides-differ left-held
  held L[mv x2 x/x2] R[mv z x/z] :: sides-differ left-held
  held L[mv x2 x/x2] R[new X2/G] :: sides-differ left-held
  held L[mv x2 x/x2] R[new X2/g] :: sides-differ left-held
  held L[mv x2 x/x2] R[new x/G] :: sides-differ left-held
  held L[mv x2 x/x2] R[new x/g] :: sides-differ left-held
  held L[mv x2 p/f] R[mv x Q] :: no-quiet-plan sides-differ
  held L[mv x2 p/f] R[mv x q] :: no-quiet-plan sides-differ
  held L[mv x2 p/f] R[mv x X2/x] :: no-quiet-plan sides-differ
  held L[mv x2 p/f] R[mv x p/x] :: no-quiet-plan sides-differ
  held L[rm x2] R[mv x Q] :: no-quiet-plan sides-differ
  held L[rm x2] R[mv x q] :: no-quiet-plan sides-differ
  held L[rm x2] R[mv x X2/x] :: no-quiet-plan sides-differ
  held L[rm x2] R[mv x p/x] :: no-quiet-plan sides-differ
* held L[mv z p/f] R[mv p X2/p] :: removed-in-a-pass
* held L[mv z p/f] R[mv p x/p] :: removed-in-a-pass
* held L[mv z p/f] R[mv p/f X2/f] :: removed-in-a-pass
* held L[mv z p/f] R[mv p/f x/f] :: removed-in-a-pass
  held L[mv p q, mv x2 q/x2] R[] :: sides-differ left-held
  held L[mv p q, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv p x/p, mv x p] R[] :: sides-differ left-held
  held L[mv p x/p, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv p x/p, mv x2 x/p/x2] R[] :: sides-differ left-held
* held L[mv p x2/p, mv x/f p] R[] :: removed-in-a-pass
  held L[mv p x2/p, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv p x2/p, mv x2 p] R[] :: sides-differ left-held
* held L[mv p x2/p, mv z p] R[] :: removed-in-a-pass
* held L[rm p, mv x x2/x] R[] :: removed-in-a-pass
* held L[rm p, mv x/f x2/f] R[] :: removed-in-a-pass
  held L[rm p, mv x2 x/x2] R[] :: sides-differ left-held delete-undone
* held L[mv x q, mv q/f x] R[] :: removed-in-a-pass
* held L[mv x q, mv z x] R[] :: removed-in-a-pass
* held L[mv x p/x, mv p/x/f x] R[] :: removed-in-a-pass
* held L[mv x p/x, mv z x] R[] :: removed-in-a-pass
* held L[mv x x2/x, mv p x] R[] :: removed-in-a-pass
  held L[mv x x2/x, mv x2 p/f] R[] :: sides-differ left-held
* held L[mv x x2/x, mv x2/x/f x] R[] :: removed-in-a-pass
* held L[mv x x2/x, mv z p/f] R[] :: removed-in-a-pass
* held L[mv x x2/x, mv z x] R[] :: removed-in-a-pass
  held L[mv x p/f, mv p q] R[] :: sides-differ left-held
  held L[mv x p/f, mv p x2/p] R[] :: sides-differ left-held
  held L[mv x p/f, mv p/f/f p/f/g] R[] :: sides-differ left-held
* held L[mv x p/f, mv p/f/f x] R[] :: removed-in-a-pass
  held L[mv x p/f, mv x2 q] R[] :: sides-differ left-held
  held L[mv x p/f, mv x2 p/x2] R[] :: sides-differ left-held
  held L[mv x p/f, mv x2 p/f/x2] R[] :: sides-differ left-held
  held L[mv x p/f, mv x2 x] R[] :: sides-differ left-held
  held L[mv x p/f, rm x2] R[] :: sides-differ left-held
  held L[mv x p/f, mv z p/f/z] R[] :: sides-differ left-held
  held L[mv x p/f, mv z x2/z] R[] :: sides-differ left-held
  held L[mv x p/f, mv z x] R[] :: removed-in-a-pass sides-differ left-held
  held L[mv x p/f, new p/f/g] R[] :: sides-differ left-held
  held L[mv x p/f, new x2/g] R[] :: sides-differ left-held
  held L[mv x p/f, mkdir x] R[] :: sides-differ left-held
* held L[rm x, mv z x] R[] :: removed-in-a-pass
  held L[mv x/f x/g, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv x/f f, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv x/f p/f, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv x/f x2/f, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv x/f x2/f, mv x2 p/f] R[] :: sides-differ left-held
* held L[mv x/f x2/f, mv z p/f] R[] :: removed-in-a-pass
  held L[rm x/f, mv x2 x/x2] R[] :: sides-differ left-held
  held L[edit x/f, mv x2 x/x2] R[] :: sides-differ left-held
  held L[mv x2 q, mv q x/q] R[] :: sides-differ left-held
  held L[mv x2 x/x2, mv z x/z] R[] :: sides-differ left-held
  held L[mv x2 x/x2, mv z x/x2/z] R[] :: sides-differ left-held
  held L[mv x2 x/x2, mv z p/f] R[] :: removed-in-a-pass sides-differ left-held
  held L[mv x2 x/x2, new x/g] R[] :: sides-differ left-held
  held L[mv x2 x/x2, new x/x2/g] R[] :: sides-differ left-held
  held L[mv x2 p/f, mv p q] R[] :: sides-differ left-held
  held L[mv x2 p/f, mv p x/p] R[] :: sides-differ left-held
  held L[mv z p/z, mv p z] R[] :: sides-differ left-held
  held L[mv z x/z, mv x z] R[] :: sides-differ left-held
* held L[mv z x2/z, mv x/f z] R[] :: removed-in-a-pass
  held L[mv z x2/z, mv x2 z] R[] :: sides-differ left-held
  held L[mv z p/f, mv p z] R[] :: sides-differ left-held
```
