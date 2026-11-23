# W49 report

## What changed
- `internal/sync/planner_move_composition.go`: `resolve` decides an entry after the folder whose move vacates the path its target lies at or below (`waitsForVacatingFolder`, `vacatingFolder`); `planMove` then rebases a file's OneDrive source through that folder's move (same pass) and holds a local move or a folder's move for the next pass. Through a folder that is not a move participant (SQLite deletes or creates it) nothing changes.
- `internal/sync/planner_dependencies.go`: `movesIntoRecreatedSource`, `recreatesMoveSource` and the `sourceRecreated` parameter removed; the `addMoveDeps` comment says what orders these moves now.
- `internal/sync/planner_dependencies_test.go`: the scanning oracle loses the exception (identical to its state before #912).
- `internal/sync/engine_move_recreate_test.go`: cases `one run same name` (+ on OneDrive), `one run folder same name` (+ on OneDrive), field `folder`, `wantTree`; every case asserts the Graph moves (`remote.moves`) and zero superseded actions (`runScenarioUntilDone` returns both); new `TestRunOnce_SubfolderMovedOnOneDriveToThePathItsMovedParentLeftConverges`.
- `internal/sync/planner_move_composition_test.go`: `recreatedFolderNodes` + `TestComposeMoves_EntryMovedIntoTheFolderRecreatedAtAMovedFoldersPath` (6 rows), `TestComposeMoves_SubfolderMovedToThePathItsMovedParentLeftWaitsForTheParent`; `TestBuildDependencies_MovesThroughAFolderRecreatedAtAVacatedPathHaveAnOrder` rewritten to the new plans (+ own-name row); `TestBuildDependencies_ChildrenLeaveFirstUnlessTheyMoveIntoTheRecreatedSource` renamed `TestBuildDependencies_ChildrenLeaveAMovingFolderFirst`, with a row pinning the cycle a pre-move source would form.
- `internal/sync/dispatch_order_test.go`: the new scenario appended at the end of `runOnceScenarios()`.
- `spec/design/sync-planning.md`: Move Composition (emission) and the `buildDependencies` paragraph.

## Before the fix (base 51e554fa)
- `one run same name`: 10/10 fail (both sides `photos/photos/50%.txt`); on OneDrive: 10/10 pass. Folder put back: 10/10 pass both ways.
- Item 2 on OneDrive: fails (cycle `local_move docs->archive`, `local_move docs/sub->docs`, dropped every pass). Item 2 on this computer: fails (see Unfinished).
- Item 3: with the zero-superseded assertion, `one run`, `one run plain name`, `one run two levels deep` fail on base (one superseded file move each).
- Composition rows: `a file put back, moved here`, `a file renamed, moved here`, `a file renamed, moved on OneDrive` and the subfolder row fail on base; the other three rows pass on base and pin the wait.

## Checks
- Item 1 both cases: 20/20 plain, 20/20 `-race` (608 s).
- `go test -count=1 ./internal/sync/...`: ok (733 s, behind the lock).
- `-race` on TestComposeMoves_*, TestBuildDependencies_*, TestIsolateDependencyCycles_*, TestPathAncestors_*, TestPathNameKey_*, both scenarios, the 15 scenarios of `engine_move_composition_test.go`, TestReverseDispatchTable_ListsEveryRunOnceScenario: ok (389 s). `TestReverseDispatch_RunOnceScenariosPass/<both scenarios>` under `-race`: ok (138 s).
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. gofumpt -l on the six Go files: clean. TestFindTestConventionViolations_RealRepoIsClean: pass.
- Lint ran twice: the first found one goconst (`"photos"` twice in the scenario test, from `wantTree`), fixed; the second `0 issues.` (`W49-lint.log`). The scenarios were rerun plain after that test-only fix.
- Commit `adf1aeae` on 51e554fa, 7 files, not pushed.
- Spec: the six tests cited exist; R-2.17.1 and R-2.17.6 exist.

## Deviations
- Item 3 assumed the folder's move is already earlier in the plan when the entry is composed. It is not: the entry sorts before its folder (depth, then path), so the rebase alone never applied; the entry now waits for that folder's decision.
- Only a file on OneDrive is rebased. A local move (its rebased source is not in local truth before the next scan) and a folder's move (deletes addressed at its old paths lie below both moves, so a rebased folder move closes a cycle with them) wait a pass instead of being superseded or kept with a stale source.
- Rebase and wait need every folder between the entry and the moving folder to be a move participant: a planner-only rebase in item 2's local case let a remote delete of `docs/sub` run before the file left it (checked by reading; the guard keeps that plan as it was).
- Item 2's local case is not fixed and its scenario is not committed (Unfinished).

## Unfinished
- `mv docs archive; mv archive/sub docs` on this computer, one sync: SQLite leaves sub unpaired (`docs/sub` `local_missing`; the local `docs` row holds the parent's baseline row), so the plan is folder move, remote create `docs`, remote move `docs/sub/inner.txt->docs/inner.txt`, remote delete `docs/sub`: a four-action cycle dropped on every pass, every sync exits 1, nothing is lost. The pairing is in `sqlite_compare.go`, outside the planner. The two-case scenario that shows it is in the scratchpad, `W49-item2-both-cases.go.txt`.

## Also fixed
- `engine_move_recreate_test.go`: the table's doc and `steps` doc named only a file.

## Not fixed (3)
1. `internal/sync/sqlite_compare.go` (local move pairing): a local entry at a baseline path whose item moved away is never a move destination for another item, so a subfolder moved onto its moved parent's path is a delete plus a create and a cycle (above). Fix: offer such a row as a destination to the identity pairing, then composition's wait finishes it in one sync.
2. `internal/sync/planner_dependencies.go:492` (`underMoveDestination`): deletes and cleanups below a move's destination are read as post-move paths, though SQLite addresses them at baseline paths; for a folder moved into the path its moving parent vacates the two readings coincide, and a rebased folder move would close a cycle with them (by reading, not run), so such folders wait one more pass. Fix: let a delete below a destination that lies inside another same-side move's source order by that source.
3. `internal/sync/planner_move_composition.go:710-724` (`blockingPendingMove`): the comment says the occupant must be pending and must move on side; the code checks neither, so an entry blocked by a settled occupant stays pending until the final sweep defers it. Fix: check the occupant's state or reword the comment.

## Packages the reviewer should test
`internal/sync` only (no other package changed).

## Second pass

### Rebase
- Onto `04983d12` with one fetch: no conflict in any file. `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. The tests under "Checks" above (plain): ok; the reverse-dispatch subtests of both scenarios: ok.

### Item 1 (folder that is not a synced item between the entry and the moving folder)
- The example as given (OneDrive `mv docs photos/photos; mkdir docs; mv photos/photos/a.txt docs/a.txt`, here `mkdir docs/new; mv docs/a.txt docs/new/a.txt`), on the rebased unchanged commit: converged in 1 sync (3 passes), no "dependency cycle" logged, nothing superseded, both trees `docs/`, `docs/a.txt`, `photos/`, `photos/photo.jpg`, `photos/photos/`, `photos/photos/kept.txt`, `photos/photos/new/`; trash and recycle bin empty. Plan: pass 1 local move `docs/new/a.txt->docs/a.txt` (into the old docs), local move `docs->photos/photos`, remote create of `docs/new` (made at `photos/photos/new`); pass 2 local create `docs`, local move `photos/photos/a.txt->docs/a.txt`; pass 3 nothing. The local create of `docs` is not in pass 1 (it comes once the old docs has left the path; the existing case "one run same name on OneDrive" shows the same: its pass 1 has no folder create), so the first of the three edges does not exist. As stated, the finding is wrong for this example.
- A variant of the same kind does cycle: OneDrive `mv docs archive; mv archive/a.txt docs` (a.txt onto the vacated path itself), same local steps. On the rebased unchanged commit, and on main's planner (`04983d12`'s two production files swapped in and back), every pass dropped `local_move docs->archive` and `local_move docs/new/a.txt->docs` as a cycle and each of 3 syncs left work; nothing lost (OneDrive: `archive/kept.txt`, `docs` = a.txt; here: `docs/kept.txt`, `docs/new/a.txt`). Present on main, not caused by this commit.
- Fix taken (1b; the smell rule for a few lines in a touched file): `vacatingFolder` passes over a directory that is not a node and keeps the return for a one-sided node. Both cases now take 1 sync: pass 1 moves docs (taking docs/new and a.txt along) and makes `new` on OneDrive inside it while a.txt waits; pass 2 moves a.txt where OneDrive put it (making the new docs first in the into-case).
- Tests: `TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges` (2 cases, added to `runOnceScenarios`; the onto-case failed before the fix, the into-case passed before and after), `TestComposeMoves_FileMovedOnOneDriveOutOfAFolderMadeHereWaitsForItsFolder` (both rows failed with the fix taken out), `TestBuildDependencies_FileMovedOntoAVacatedPathFromAFolderMadeInsideIt` (its cycle row failed with the children-first rule taken out; rule put back).
- The first report's "Through a folder that is not a move participant (SQLite deletes or creates it) nothing changes" is untrue: through a directory that is not a synced item the onto-case cycled on every pass, and through a one-sided synced folder the plan is a cycle (item 2).

### Items 2 to 9
- 2: `TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource` (docs/sub on OneDrive only): the move keeps `docs/sub/inner.txt`, the delete of `docs/sub` waits for it, and all four actions drop as a cycle. With the guard's return replaced by `continue`, the move became `archive/sub/inner.txt->docs/inner.txt` and the delete had no dependency at all. Guard put back. The whole `./internal/sync` package (plain) with the guard removed failed only this new test: no existing test catches the guard.
- 3: both comments say conditions ("would be carried away", "would hold back ... would drop").
- 4: the `vacatingFolder` and `addMoveDeps` comments, the two spec paragraphs and the message now say the same: a directory that is not a synced item is passed over; through a one-sided synced folder the move keeps its source, its delete waits for it, and the plan is a cycle the planner drops (nothing deleted, work unfinished).
- 5: `blockingPendingMove`'s comment says what it checks (any occupant, whatever its state); the matching comment in `resolve` too. No check added.
- 6: the header of `planner_move_composition.go` and the spec name both waits (the target parent, the vacating folder), say the items are decided in turn unless the waits loop, and that such a loop is deferred every pass and a one-shot sync ends with it unfinished.
- 7: `keepName` says "entry".
- 8: corrected copy at `reports/W49-item2-both-cases.go.txt` (two results; uses `newRecreateScenarioEngine`). Pasted at the end of the test file it compiled and ran once: "on this computer" failed (each of 3 syncs left 4 actions, cycle with `remote_delete docs/sub`, OneDrive unchanged, nothing lost), "on OneDrive" passed. Taken out again.
- 9: message rewritten: what converges, and that the subfolder's steps made on this computer still do not (every sync reports unfinished work, nothing lost).

### Also fixed
- `engine_move_recreate_test.go`: the three scenarios share `newRecreateScenarioEngine` instead of three copies of the engine setup.

### Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...` on the final tree: pass. `gofumpt -l` on the six Go files: clean.
- `go test -count=1 -timeout 60m ./internal/sync/...` behind the lock: ok (516 s). One word of a test comment changed after it ("at" to "below"); nothing else.
- `-race` behind the lock, `-timeout 30m`: TestComposeMoves_*, TestBuildDependencies_*, TestIsolateDependencyCycles_*, TestPathAncestors_*, TestPathNameKey_*, the three scenarios of `engine_move_recreate_test.go`, the 15 scenarios of `engine_move_composition_test.go`, TestReverseDispatchTable_ListsEveryRunOnceScenario: ok (408 s); `TestReverseDispatch_RunOnceScenariosPass/<the three scenarios>`: ok (369 s). Plain, the same set plus every test named ListsEvery/Governs/Exhaustive/Registered: ok.
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: all four pass.
- `devtool verify lint` behind the lock: `0 issues.`, actionlint pass (`W49-lint2.log`).
- Spec: the ten tests cited in the two paragraphs exist; R-2.17.1 and R-2.17.6 exist.
- Commit `c5c28e60` on `04983d12`, 7 files, not pushed; `git status --short --untracked-files=no` empty.

### Seen and not fixed (1)
1. `internal/sync/planner_move_composition_test.go`, `TestBuildDependencies_MovesThroughAFolderRecreatedAtAVacatedPathHaveAnOrder`, row "one run, the moves made on OneDrive": the test calls these the scenario's plans, but that shape's first pass (run with a.txt under its own name) holds the local folder move and a baseline update and no local create of docs; the create comes in the second pass with the file's move. Not fixed here: which row that baseline update writes, and how it orders, was not established. Fix: identify the update and make the row the two real plans.

## Third pass

### Rebase
- One fetch: `origin/main` was still `341d9820`, the commit's parent; the rebase did nothing. `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass before and after.

### Item 1 (the removed exception)
- Scenarios in `engine_move_recreate_test.go`, registered in `runOnceScenarios`: `TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges` (steps A, cases "made here" and "made on OneDrive") and `TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges` (steps B). Each asserts one sync, both trees equal to the exact map (the review's paths, their folders and the untouched `photos/photo.jpg`), the Graph moves, no upload or OneDrive delete, no cycle line and a quiet plan (`assertOneSyncLeavesTree`). No zero-superseded assertion: the fallback supersedes the file's move once.
- Unchanged commit: A made here failed, each of 3 syncs left work (WillRetry 4), cycle `remote_move docs/sub/inner.txt->docs/inner.txt`, `remote_move docs->photos/photos`, `folder_create docs` (sub's rename dropped with them), OneDrive unchanged. B failed, each of 3 syncs left work (WillRetry 5), cycle `remote_move docs->archive`, `remote_move docs/sub/inner.txt->docs/inner.txt`, `folder_create docs`; `new.txt` never reached this computer. A made on OneDrive passed. Nothing lost anywhere.
- Main's code (`341d9820` archive, the new scenario file copied over its `engine_move_recreate_test.go`): all three pass, 1 sync each. A's plan: 4 actions, the file's move from `docs/sub/inner.txt` superseded once and made by pass 2; B's: 5 actions, the file's move and the local create of `archive/sub` superseded once, pass 2 makes both and downloads `new.txt`.
- Fix: `recreatesMoveSource`, `movesIntoRecreatedSource` and the `sourceRecreated` parameter put back from main exactly (the code of `planner_dependencies.go` equals main's; only the `addMoveDeps` comment differs), and the scanning oracle's exception too, so `planner_dependencies_test.go` equals main and left the commit (6 files now). A rebased move's source lies below the folder move's destination and a held move is not in the plan, so the exception reaches only moves composition neither rebased nor held. `vacatingFolder` unchanged.
- After the fix: A (both cases) and B take 1 sync; their per-pass plans (scratch copy with plan logging) equal main's. Every scenario of `engine_move_recreate_test.go` passes with the counts and zero-superseded assertions it had (1 e did not happen).
- Rows: `TestComposeMoves_EntryBelowARenamedSubfolderKeepsItsSource` (A: the file keeps `docs/sub/inner.txt`, sub's rename is rebased, all 4 actions kept); `TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource` became rows "deleted online" (its old content: a cycle) and "made here again" (B: all 5 kept); `TestBuildDependencies_ChildrenLeaveFirstUnlessTheyMoveIntoTheRecreatedSource` (main's name and statement back) gained "a file moved out of a renamed subfolder" and "a file moved out of a subfolder made here again", main's exact plans. On the unchanged commit (fallback out) both compose rows failed on the kept count (0) and both dependency rows only on the folder move's edge; all pass with it back. Main's rows of the exception that the commit had rewritten are back in that test ("a move into the folder made there", "the same on this computer", "a move into a folder made inside the new one", main's "a sibling that moves elsewhere"); each failed without the fallback.

### Items 2 to 10
- 2: the `vacatingFolder` and `addMoveDeps` comments, both spec paragraphs and the message say: composition rebases a file's OneDrive move or holds the rest when the nearest relocated folder is the vacating one; where the search stops short (a nearer relocated folder, a one-sided folder) the move keeps its source and the exception orders it after the folder move when the plan creates a folder at the vacated path; when SQLite deletes the one-sided folder the plan stays a cycle dropped on every pass. Said in full once in the spec (Move Composition); the comments are short and point there.
- 3: header now "Plans of ..., made on this computer", two rows not taken from the scenario, and what the second holds. The other four rows checked against the scenario's plans (scratch plan logging): they are the plans of `two runs`, `one run`, `one run same name` exactly.
- 4 and 8: the pass-by-pass sentences of `TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges` are cut (a test cannot read one sync's passes); the comment states what it asserts. The other changed and new scenario comments checked against their assertions.
- 5: the header of `planner_move_composition.go` and the spec name the occupancy wait (`blockingPendingMove`) and its three ends: a move of the plan vacating the destination, `parkOneOccupancyCycle`, emit's final sweep.
- 6: the comment says the into case holds convergence with or without the wait, the onto case also the wait (naming the composition test). 7: row and header phrase "a folder the plan creates on this computer at the path a local move leaves". 9: table below. 10: message rewritten.

### What the exception covered, and what covers it now
| Case | Covered by | Test |
|---|---|---|
| Move rebased by composition | composition: its source lies below the folder move's destination, out of the exception's reach | `TestComposeMoves_EntryMovedIntoTheFolderRecreatedAtAMovedFoldersPath`, `TestBuildDependencies_MovesThroughAFolderRecreatedAtAVacatedPathHaveAnOrder` ("one run", "one run, the file under its own name") |
| Move held for the next sync | composition: not in the plan | `TestComposeMoves_EntryMovedIntoTheFolderRecreatedAtAMovedFoldersPath` (deferred rows), `TestComposeMoves_SubfolderMovedToThePathItsMovedParentLeftWaitsForTheParent` |
| Through a relocated folder (A) | the exception (fallback) | `TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges`, `TestComposeMoves_EntryBelowARenamedSubfolderKeepsItsSource`, row "a file moved out of a renamed subfolder" |
| Through a one-sided folder re-created (B) | the exception (fallback) | `TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges`, rows "made here again", "a file moved out of a subfolder made here again" |
| Through a one-sided folder deleted | nothing: a cycle on main and here, nothing deleted | `TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource` "deleted online"; no scenario (it would assert unfinished work) |
| Onto the vacated path itself | never the exception (strict prefix); composition holds the local move | `TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges` (onto), `TestBuildDependencies_FileMovedOntoAVacatedPathFromAFolderMadeInsideIt` |

### Corrections to the sections above
- First section: "a move into a re-created folder names a source below the folder move's only through such a one-sided folder, so the exception is gone" is untrue for steps A and B; both converged on main and cycled without the exception.
- Second section, item 4: the one-sided plan is a cycle only when SQLite deletes that folder; when it re-creates it (B) main ordered the plan, and it converges again.

### Also fixed
- `planner_move_composition_test.go`, `TestBuildDependencies_FileMovedOntoAVacatedPathFromAFolderMadeInsideIt` row "first pass": called the scenario's plan, but it created `docs/new` with no dependency; the scenario creates `archive/new` after docs's local move. Row (deps `{nil, {0}}`) and header corrected.
- `engine_move_recreate_test.go`: `scenarioLocalPath` replaces three copies of the local-path closure.

### Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l` on the six Go files: clean.
- `go test -count=1 -timeout 60m ./internal/sync/...` behind the lock: ok (753 s).
- `-race -timeout 30m` behind the lock: TestComposeMoves_*, TestBuildDependencies_*, TestIsolateDependencyCycles_*, TestPathAncestors_*, TestPathNameKey_*, TestReverseDispatchTable_ListsEveryRunOnceScenario, the five scenarios of `engine_move_recreate_test.go` and the 15 of `engine_move_composition_test.go`: ok (564 s); `TestReverseDispatch_RunOnceScenariosPass/<the five scenarios>`: ok (332 s).
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: all four pass.
- Lint behind the lock (`W49-lint3.log`): the first run found one govet shadow (`actionType` in a test helper), renamed `moveType`, the composition and dependency tests rerun plain (ok); second run `0 issues.`
- Spec: all 157 tests cited in `sync-planning.md` exist; every requirement id cited exists.
- Commit `133b1be0` on `341d9820`, 6 files, not pushed; `git status --short --untracked-files=no` empty.

### Seen and not fixed (1)
1. `internal/sync/planner_move_composition.go`, `vacatingFolder` (the return at a relocated folder): on OneDrive `mv docs/sub docs/sub2; mv docs archive; mv archive/sub2/inner.txt docs` (onto the vacated path itself, through a renamed subfolder) is a cycle on every pass on main and here (scratch runs: 3 syncs, WillRetry 3 each, 9 cycle lines: `local_move docs->archive`, `local_move docs/sub/inner.txt->docs`, `local_move docs/sub->docs/sub2`; nothing lost). The exception needs a folder created at the vacated path, so it cannot order this. Not fixed here: walking on past a relocated folder is the change this commit does not take. Fix: let the walk go on past a relocated folder the target does not land below, so composition holds the local move a pass. (The same steps made on this computer converge: SQLite pairs the file onto the folder's baseline path as a delete and an upload.)

## Fourth pass

Commit `d4590f38` on `341d9820` (8 files: the six before plus `planner_actions.go` and `core_types_test.go`), not pushed; `git status --short --untracked-files=no` empty.

### Finding 1 seen first
- The findings' "To run" in `w49d/` (exports of 133b1be0 and 341d9820, `zz_r_test.go`): r1-renamed on 133b1be0: 3 syncs, WillRetry 5, 15 cycle lines (`remote_move x->x2`, `f->x/f`, `x/f/b.txt->f/d.txt`, `folder_create f`, `folder_create x`), nothing moved; on main: 1 sync, 1 superseded, both trees x2/keep.txt, x/f/c.txt, f/d.txt. Same name: main ends with x/f/b.txt on both sides; r2 (g onto x): main the same, 133b1be0 cycles.

### The rule chosen for finding 1, and why it is general
- Rule: a file's OneDrive move rebased through the vacating folder's move is held for the next pass when the rebased source lies below the source of another folder's OneDrive move of the plan whose destination does not carry the file's target (`rebasedSourceMisread`); such a file is decided after every folder OneDrive relocates (`waitsForVacatingFolder`, `remoteFolderUndecided`), so every such move is in the plan when it is checked. A held move keeps its baseline row: `dropReconciliationOfComposedMoves` drops SQLite's baseline update of an item whose held move the mode runs (`holdsMoveInMode`).
- General: it names no shape; it holds exactly the moves whose rebased source the children-first rule would read as work inside another folder before it moves, whatever fills that folder's path (a new folder, a folder moved onto it, the vacating folder itself), and whatever order composition decides the folders in.
- The proposal judged, by runs: its condition is the one kept, but checked against the moves decided so far it misses a folder decided after the file (any folder whose target sorts after the file's, e.g. x moved deeper than f's target); and a held same-name file lost its baseline row: with the hold alone, `here-same` ended with x/f/b.txt on both sides, the plan holding `baseline_update f/b.txt` that waits for the new f, so after f's move it recorded b.txt at f/b.txt again and the next pass moved the local file back (the first bug's mechanism). Hence the wait and the dropped update.
- Not taken: changing the dependency rule instead (not reading a source below the destination of a move that fills the vacated path as a child). At a vacated path the path strings of the old folder and of what fills it are the same (with g moved onto x, x/k.txt of the old x and of g read alike), so only composition, which knows where each source comes from, can tell them apart.

### Findings 1 to 6
1. Fixed as above. Results (scenario `TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges`, registered): renamed made here: one sync, both trees equal, no cycle, no supersede (133b1be0: cycle on every pass); same name made here: f/b.txt on both sides (main: x/f/b.txt on both; 133b1be0: cycle); g moved onto x made here: f/b.txt on both sides, one sync, g reaches OneDrive as a new x with g.txt moved into it and g deleted, as on main (main ends with x/f/b.txt, 133b1be0 cycles), so better than main.
   The three made on OneDrive pass on 133b1be0 (run) and on main (the equivalent scratch runs) as well as here: they do not fail before (deviation). The held baseline row is held by the same-name case's tree assertion (f/b.txt on both sides fails without the dropped update) and by `TestDropReconciliationOfComposedMoves_HeldMoveKeepsItsBaselineRow`. Rows: `TestComposeMoves_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathWaits` (3 rows; each failed on 133b1be0, which rebases b.txt) and `TestBuildDependencies_FileRebasedBelowAnotherFoldersVacatedPath` (the cycle: kept 0; first and second pass ordered).
2. Message: the stopping-short sentence now holds only where the plan makes a folder at the vacated path and the folder the search stopped at is renamed inside the moving one or made again on the other side; "Still dropped" names the onto case through a renamed subfolder, note 6's shape and the family the differential run found. Spec: the same clauses in Move Composition (one for no folder at the vacated path, one for a subfolder that moves out), the hold, the wait and the dropped update.
3. Added `downloadsBefore` and the downloads assertion to both cases of `TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges`; the header says it.
4. "every action of it has an order" and "Every action of the plan ... has an order".
5. "Moved on this computer in the pass that moves docs".
6. Nothing in code; both shapes in "Still dropped"; run here and on main (`zz_n_test.go`): both cycle on both (9 and 12 cycle lines over 3 syncs).
- Texts finding 1 made untrue: sync-planning.md near 547 (a paragraph added after the sentence: the hold, the wait, the tests), near 639 (now: composition holds it also where that source would lie below another folder move's source, citing the new dependency test), message line 18 (the sentence stands for the shapes it describes; the next sentences say when the file waits instead).

### Differential run
- `W49-differential.md`, harness `W49-differential-test.go.txt`. 466 sequences of up to 4 steps (all) and 200 of 2790 five-step ones (those making two folders again first, seed 4949), each on both sides: 1332 runs per export. Same trees 1211; main converges and HEAD does not 0; different trees 82 (all: HEAD ends with the tree the user left, main does not); HEAD converges and main does not 0; neither 39 (same on both, all on this computer, named in "Still dropped"). HEAD needs more syncs than main: 0; supersedes fewer in 119, more in 58 (all on OneDrive, a timing effect on a plan main makes too: Seen and not fixed 3).

### Corrections to earlier sections
- Third pass, table row "Move rebased by composition" (line 107): true only where the rebased source lies below no other folder move's source; otherwise composition holds the file (`rebasedSourceMisread`, `TestComposeMoves_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathWaits`, `TestBuildDependencies_FileRebasedBelowAnotherFoldersVacatedPath`).
- Third pass, item 2 summary and the message's line 29 to 33 sentence: "the exception orders it after the folder move when the plan creates a folder at the vacated path" holds only when no move of the folder the search stopped at holds the item back; where that subfolder moves out of the moving folder the plan is a cycle (on main too).

### Checks
- `gofumpt -l` and `goimports -l` on the commit's seven Go files: no output.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- One `go test -count=1 -timeout 30m -run` call on `./internal/sync/`: the new and touched tests, every scenario of `engine_move_recreate_test.go`, all `TestComposeMoves_`, `TestBuildDependencies_`, `TestIsolateDependencyCycles_`, `TestActionType*`, `TestReverseDispatchTable_` and every test named `ListsEvery`, `Governs`, `Exhaustive`, `Registered`: ok (44 s). The same names without the last four patterns under `-race` behind the lock: ok (378 s). `TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges` under `-race`: ok (108 s).
- `go test -count=1 -timeout 60m ./internal/sync/...` behind the lock: ok (1040 s).
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: all four pass.
- Lint of `./internal/sync/...` behind the lock (`W49-lint4.log`): `0 issues.`
- Spec: all 161 tests cited in `sync-planning.md` exist (4 new).
- Before the change, on an export of 133b1be0 with the new test files: the three compose rows fail (b.txt rebased and moved, nothing deferred); the scenario's three cases made here fail, the three made on OneDrive pass.
- The differential ran on 33b6c8d1; d4590f38 differs from it only in comments, the spec and the message. Exports and archives removed.

### Seen and not fixed (3)
1. `internal/sync/planner_move_composition.go`, `vacatingFolder` (the return at a relocated folder): the onto case through a renamed subfolder, note 6's shape, and a subfolder that moves out of the moving folder (`mv x x2; mkdir x; mv x2/s y/s; mv y/s/t.txt x/t.txt`, 10 of the differential's 39) are cycles on every pass on main and here. Smallest fix: walk on past a relocated folder the target does not land below, and hold the move a pass when the target lies at or below a farther folder's vacated path.
2. `internal/sync/planner_dependencies.go:472-474` (children leave first) against moves and creates that must follow a folder move: in 29 of the differential's 39 (`mv y x/y; mkdir y; mv x/a.txt y/a.txt; mv x x2` and kin) an entry leaves a moving folder for a folder made at a path another folder's move leaves, and that folder's move (composed after the moving folder's, e.g. to x2/y) waits for the moving folder: a cycle on main and here. Smallest fix not determined; one candidate is composition holding the entry's move a pass when its target lies at or below a path another folder's move of the plan vacates and its source below a folder move's source.
3. Planning of a local move into a folder OneDrive made at the path a local folder move leaves (e.g. on OneDrive `mv x x2; mkdir x; mv y x/y`): the first plan has no local create of the new x (SQLite plans none while the old x holds the path) and no order between y's move and x's, so y's move either lands in the old x and is moved again from x2/y next pass (main's timing) or fails as stale once (here, 58 runs). Smallest fix: plan that local create in the same pass, waiting for the local move that leaves the path, as `recreatedSourceCreates` does on OneDrive (`planner_move_composition.go`), so the move into it waits for it.


## Fifth pass (2026-11-15, by the lead, after the fourth review)

The fourth review (`R-W49d-findings.md` in the scratch) found no
regression: must fix 1, should fix 3, notes 5. All handled in one amend;
no product code changed (comments, spec, tests and the message only).

- Must fix 1: no test failed without the wait for every folder OneDrive
  relocates. Added
  `TestComposeMoves_FileMovedBackWaitsForAFolderDecidedAfterIt` (the
  folder whose old path the file's folder moves into is decided after
  the file) and
  `TestComposeMoves_FolderMovedOntoAWaitingFilesPathWaitsOnePass`.
  Mutation check by the lead: with the wait switched off both fail;
  restored, both pass.
- Should fix: the header comment of `planner_move_composition.go` and
  the spec paragraph now tell the two loops apart (a folder that hangs
  off a waiting entry is never broken; a folder whose destination a
  waiting file holds lasts until the move that vacates the file's target
  commits); the comment before `dropReconciliationOfComposedMoves` is in
  the conditional; the scenario test's header says which cases hold the
  wait and which hold convergence; the message says "with a new x" and
  what each case did before the change.
- Note 9 (a smaller condition, `hasOwnAction` of the folder, would do
  for the cases known): not taken; the wider wait is the one the tests
  and the differential run cover.
- The earlier report's example "x moved deeper" does not need the wait.

Checks after the amend: the composition, dependency and scenario tests
pass, plain and with the race detector; `go vet` plain and all tags.
The three live proofs were made on the commit before this amend and
stand, since no product code changed.


## Merged as #926

- Squash-merged on 2026-11-15 at 18:20 PST; main is `a0dbe10b`
  ("Keep an entry put back at its old path where the user left it").
- Gate on the commit rebased onto `af2a6cf5`: local pre-PR gate passed
  (lint `0 issues.`), PR CI and the review-thread gate passed.
- Live proof with a build of the change on the test account, in a
  private folder and a private sync directory removed afterwards: three
  step sequences, each one sync, both trees equal, the same item, a
  second sync up to date. No file went to the local trash. The OneDrive
  recycle bin was not checked.
- Installed at 18:38 PST (`onedrive-go --version` names `a0dbe10b`).
- **The post-merge stage failed.** The push run of main was red:
  `TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges/renamed_on_OneDrive`,
  a test this change added, failed in one macOS unit shard. The PR's
  own run had passed the same shards. The stage had already removed the
  worktree and the remote branch.
- On this host the case fails about 1 run in 50 (3 of 150 at 18:53
  PST). Both sides end with `x2/f` and `x2/f/c.txt` where `x/f` and
  `x/f/c.txt` belong, and OneDrive gets one move of the folder that the
  steps did not ask for. The follow-up is its own branch,
  `fix/move-order-into-recreated-folder`.
