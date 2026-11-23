# W79 fix 6: nothing made at a waiting item's own landing, moves followed through nested folders, content edges one at a time

One new commit `d18345b6` on `84a0420f`, branch `fix/remote-move-into-recreated-folder`: not amended onto 84a0420f, not rebased, not pushed, not installed. 8 files: `planner_move_composition.go`, `planner_dependencies.go`, `spec/design/sync-planning.md`, and five test files (`engine_move_recreate_test.go`, `planner_move_composition_test.go`, `planner_dependencies_test.go`, `scenario_remote_test.go`, `dispatch_order_test.go`). `pairs.golden` is unchanged (the broken set is the same, see the counts).

## Blocking 1: fixed, parking kept

**Decision: parking stays.** With the hold below, a refused or interrupted move aside leaves OneDrive as main leaves it for that shape (nothing made, nothing moved inside the old folder), and the next sync converges. Probes and the new RunOnce row show it for every shape tried; nothing made parking unsound once the creates wait.

**Change (`planner_move_composition.go`).**
- `holdOwnLandings` (called at the end of `emit`): for every item the composition defers whose move on a side lands in a new folder made on the other side at the item's own path (`landingFolderHolder`, the old `folderHolder` that now also returns the folder's path), it records that folder's path, under OneDrive's name folding, in `moveComposition.ownLandings` (replaces `parkedTargets`).
- `dropCreatesOntoComposedItems` drops every create at or below such a path (`atOrBelowOwnLanding`, replaces `belowParkedTarget`, which held only creates below the target). This is the reviewer's "smallest fix" (hold at or below the parked item's source), widened in two ways the probes showed were needed:
  - It holds for every deferred item with its own landing, not only a parked one: the item that waits a pass for the move of a folder above it (P renamed Q with Q/f moved into Q/f/a/f, or Y renamed Z with Z/P/f moved deeper) had the create of the new folder superseded on the old one and `a` made inside it (reviewer's `DeepParkBelowRenamedParent` probe showed `superseded=1`; my probe reproduced it on the previous tree, and the earlier-move branch reaches it too).
  - The path is the one the move's target names (target coordinates), so a renamed folder above does not hide it.
- `parkAwayFromItsOwnLanding` no longer records anything itself; its comment says the creates wait through `holdOwnLandings`.
- Moves of other items into that region were already held: probe with g.txt moved into `f/g.txt` or `f/a/g.txt` and the move aside refused once: g's move waits ("move waits for the next pass", taken back because the holder ends without an OneDrive move), nothing goes into the old f, the next sync converges.

**RunOnce row** `TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath`, 4 shapes x {move aside refused once, not refused}:
- f into f/a/f (the reviewer's row); f into f/f with f/new.txt made beside it (the milder new-file case); f into Z/f/a/f with Y renamed Z; f into Z/P/f/a/f with Y renamed Z and P between.
- Asserts: nothing is ever created (folder or file) with the old f as parent (a create hook sees every create); after the refused sync the old f holds only c.txt; one sync (the next one when refused) finishes it with zero superseded; both trees equal; the old f ends at its target as the same item; the moves sent to OneDrive are exactly park then unpark (with Y's rename first in the renamed shapes); no dependency cycle; quiet plan.
- On 84a0420f the first version of the row (the two shapes without a rename) failed in all four variants: deep not refused, `a` made in the old f; deep refused, OneDrive left with `f/a` and `f/c.txt` and the syncs never finish; beside, `new.txt` uploaded into the old f, refused or not. The renamed shapes, with the hold only for a parked item, superseded the create of the new folder (probe).
- The scenario remote gained `refuseMove` (test-only), so a move can be refused like a create.
- Also run 40 times (`-count=40`, with the two tests below): pass.

**Interrupted park.** A stop between the create and the park no longer applies: in the pass that moves the item aside, nothing else runs at or below its path. A stop between the park's OneDrive call and its commit is note 9's class (shared with swap parking on main): not changed here.

## Should-fix 2: pass-count claim corrected

The pass after the move aside makes the folder at the path the item left and moves the item into it. Corrected in: the `parkAwayFromItsOwnLanding` comment, `sync-planning.md` (parking paragraph), the comment of `TestComposeMoves_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst`. The commit message of 84a0420f cannot be changed without amending it (not allowed); the new commit's message states the correct count and says the earlier description was wrong.

## Should-fix 3: carriedPath indexed, and now right for nested moves

- `carriedPaths` builds the moves by source per side once (`movesBySource`) and computes, before `withoutWorkTheMoveCarries` edits the graph, where the plan's moves carry each left-out delete.
- `carriedPath` follows the moves: the nearest move above the path, then the nearest move above where that one puts it that waits directly for it (binary search in the sorted deps), and so on. That covers a subfolder renamed on OneDrive after its folder (rebased) and one moved here before it (children first), and rejects a folder's move that ran first because another move takes its path. Cost: depth of the path per step.
- `deletesAheadOfTheirContent` tests the path first (now a map lookup), then `waitsFor`.
- `waitsFor` moved to `planner_dependencies.go`; it now asks each walked action for the target by binary search before walking what it waits for.
- Cost (reviewer's methodology, the plan's own actions, `withholdDeletesOfHeldWork` alone; this host under load from other jobs):

  | folder renamed here, n files edited | 1,000 | 3,000 | 10,000 |
  | --- | --- | --- | --- |
  | previous tree | 22 ms | 145 ms | 3.53 s |
  | this commit | 9 ms | 25 ms | 101 ms |

  Nested (P renamed Q, Q/S renamed Q/T, n files edited in Q/T): this commit 9 / 26 / 119 ms; the previous tree held all n deletes there (dropped = n, the wrong carried path).
- Correctness, real engine, `TestRunOnce_AFileEditedInASubfolderMovedWithItsRenamedFolderIsUploadedOnce` (S renamed inside the renamed P, or moved out to R): one sync, zero superseded, the old copy deleted once, the edit uploaded once. The previous tree superseded the upload once in the nested case and needed a second pass (reproduced the reviewer's `EditInNestedRenames`).
- `TestCarriedPath_FollowsTheMovesThatCarryTheDeletedEntry`: 7 rows (no move, renamed on OneDrive, nested on OneDrive, nested here, subfolder leaving first, folder moved to the path a renamed folder leaves, a move on the other side).

## Should-fix 4: fixed, and not reachable in the committed space

- `addContentEdges` adds every edge, computes the strongly connected components once, and decides only the edges whose ends share one, one at a time: an edge is left out only if its create already reaches its delete through the plan's edges and the content edges kept so far (`waitsFor`). Others are kept without a walk.
- `TestBuildDependencies_OfTwoContentEdgesThatCloseACycleOnlyTogetherTheFirstIsKept` (the reviewer's joint plan): a.txt's edge kept, b.txt's left out, no cycle, and `withholdDeletesOfHeldWork` keeps all 4 actions (it kept 0 before).
- The scanning oracle calls the same `addContentEdges`; `TestBuildDependencies_MatchesScanningOracle` passes.
- Reachability:
  - The committed exhaustive space with refusals, instrumented: 131 content edges lay on a cycle, 0 of them closed it only together with another (every one closed a cycle alone). On main's tree the count is 0 (main has no content edges).
  - Real engine: names traded here in another case are paired as moves (`TestRunOnce_NamesTradedHereInAnotherCaseAreMovedOnOneDrive`: no upload, no delete, both files the same items). A ring of three in another case was dropped from that test: move-to-move case variants have no order on main by design (`vacatesCaseVariantOf`), and under reverse dispatch the ring needed a fourth sync.
  - Outside these: unknown.

## Should-fix 5: mutants

Every mutant is the reviewer's replacement re-applied to this tree (mParkedTarget is now mLandingDrop, mParkedSide mLandingSide), plus new ones for the new code. "Unit" = `-short` over the planner unit tests; "area" = `-short` over the planner tests and every `TestRunOnce_`; "short" = the whole `-short` tier.

| mutant | result | killed by (`-short`) |
| --- | --- | --- |
| mEdge | killed (unit) | dependency rows, oracle, joint row, planner rows, log test |
| mExclusion | killed (unit) | `ADeleteDoesNotWaitForACreateThatWaitsForIt`, joint row, planner rows |
| mJoint (new: every on-cycle edge left out) | killed (unit) | joint row |
| mNoDownloadKey | killed (unit) | `ADeleteWaitsForTheCreateThatWritesItsContent`, planner rows |
| mNoUploadKey | killed (unit) | 5 tests |
| mSamePath | killed (unit) after a new assertion | `ADeleteWaitsForTheCreateThatWritesItsContent` now asserts no edge is left out; behaviourally the mutant is equivalent (the same-path rule makes that edge cyclic, it is left out and holds nothing) |
| mAhead | killed (unit) | planner row "f moved up to P" |
| mAheadNoWaits | killed (unit) | joint row, planner row |
| mAheadNoPath | killed (unit) | planner row "a1.txt edited in A renamed B" |
| mAheadMode | killed (unit), new row | planner row "the same in download-only" (the delete is left to the mode: deferred by mode, not dropped) |
| mAheadGate | killed (unit) | planner row |
| mAheadLog | killed (unit) after extending the log test | `TestPlanBuiltActions_AHeldDeleteLogsTheItemItWaitsFor` (the second rule's Debug line) |
| mCarriedFar | killed (unit), new test | `TestCarriedPath_` (3 rows) |
| mCarriedSide | killed (unit), new test | `TestCarriedPath_` (other-side row and others) |
| mCarriedChain (new) | killed (unit) | `TestCarriedPath_` |
| mCarriedAfter (new) | killed (unit) | `TestCarriedPath_` (folder moved to the path a renamed folder leaves) |
| mPark | killed (unit) | composition row |
| mParkEarlier | killed (unit), new row | composition row "a folder above its unmoved parent renamed on OneDrive too" (reachable after all: Y renamed Z with Z/P/f moved into a new folder at its own path); the RunOnce row pins its move order too |
| mLandingOff, mLandingSide, mLandingExact, mLandingBelow, mLandingFold (new) | killed (unit) | composition row (held paths, folded name, other side empty) |
| mLandingDrop (was mParkedTarget) | killed (area) | `TestRunOnce_AFolderMovedIntoTheFolderThatTakesItsPathConvergesWithAnEditInIt`, the new RunOnce row |
| mLandingAnyHolder (new: hold also where another item holds the landing) | survives unit, area and the whole `-short` tier (201 s) | none; it widens the hold to cases nothing in the tier reaches, and the wider rule is plausibly also right (see "Seen, not fixed") |
| mDropped | killed (unit) | planner row "z renamed here onto P/f" |
| mTrimmedHeld | killed (unit) | planner row |
| mCycles | killed (unit) | planner row |
| mRebase | killed (unit) | planner row, `AHeldRebasedCreateLogsTheFolderItWaitsFor` |
| mHeldCreate | killed (unit) | planner rows |

The unmutated unit set passes (base). mAheadLog and mSamePath were rerun after their assertions were added; the other unit results ran before those two test edits (which only add assertions).

## Checks (final tree unless noted; heavy ones behind the lock)

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass; `go vet ./internal/sync/` and `gofumpt -l` again on the committed tree: clean.
- `go test -short ./internal/sync/...`: pass, 175 s (run before the last test-only edit, which dropped the ring case of the case-swap test; the plain run below covers the committed tree).
- `go test ./internal/sync/...` (plain): pass, 827 s, committed tree. The first plain run failed `TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_NamesTradedHereInAnotherCaseAreMovedOnOneDrive/three_in_a_ring` (4 syncs needed); the ring case was removed (see should-fix 4) and the test was then run 30 times forward and reversed: pass.
- `-race` on `TestComposeMoves_|TestBuildDependencies_|TestCarriedPath_|TestPlanBuiltActions_|TestDropReconciliation|TestPlannerPlanCurrentState_|TestIsolateDependencyCycles|TestPlannerModel_|TestPlannerExhaustive|TestRunOnce_`: pass, 2,323 s.
- Lint (`golangci-lint run ./internal/sync/...`, the targeted form; the brief's `verify lint` was not run as `devtool verify` is reserved): 0 issues on the committed tree (the first run found one goconst in a new test, fixed).
- devtool `RealRepoIsClean`: 4 PASS. Every test `sync-planning.md` cites exists (204 names checked).
- The new RunOnce tests: 40 runs each (`-count=40`) before the last edit: pass.

## Pair counts (committed space, pairs and refusals)

Main's product files with this tree's tests, and this commit, run through the same probe (every run written out):

| tree | runs | broken runs | broken pairs |
| --- | --- | --- | --- |
| main (238bea94) | 8,671 | 824 | 344 |
| this commit | 8,387 | 255 | 154 |

- Only on the branch: 0 (none absent from main, none ok on main, none breaking more). 550 runs broken on main are ok here.
- Against 84a0420f (8,391 runs): the same 255 broken runs. Four refusal runs are gone: refusals of creates the new hold no longer plans in that pass (a folder create below an own landing, three uploads/downloads at z/f), all of them ok before.
- The broken lines equal `pairs.golden`, so the committed test is unchanged.

## Seen, not fixed

- `planner_move_composition.go` `dropCreatesOntoComposedItems` / `recreatedSourceCreates`: a new folder made here at the path of a folder whose own move waits (f renamed f2 while OneDrive holds F2, a new f with new.txt made here): new.txt is uploaded into the old f on OneDrive, on main and on this branch alike (probe). Fix: hold every create at or below the path of a waiting recreated-source create (the `waiting` list `planBuiltActions` already has), which is what mLandingAnyHolder partly does.
- A kill between a park's OneDrive call and its commit (note 9): the next sync takes the parking name for a rename. Shared with swap parking on main. Not touched.
- `planner_dependencies.go` `vacatesCaseVariantOf`: two moves onto names that fold together have no order (deliberate on main), so a ring of case-changing renames can need more than three syncs under reverse dispatch. Not touched.
- Local-side parking under a folder OneDrive renamed in the same plan was not probed (no local refusal hook); unknown.

## Not run

- Live tests: none (nothing in this change needs the service; not asked).
- The reviewer's widened spaces (moves refusable, three actions on one side, two refusals, deeper tree): not run.
- Watch mode, a case-insensitive local volume: not checked.
