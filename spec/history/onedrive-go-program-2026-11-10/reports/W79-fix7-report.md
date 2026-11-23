# W79 fix 7: claims narrowed to what the hold covers, squash message, shared stalls as plan rows

One new commit `46741270` on `d18345b6`, branch `fix/remote-move-into-recreated-folder`: comments, spec and the squash message only. No planner or executor behaviour changed. Not pushed, not rebased, not installed. 5 files: `actions.go`, `planner_move_composition.go` (comments only), `planner_move_composition_test.go`, `engine_move_recreate_test.go` (comments only), `spec/design/sync-planning.md`.

## Finding 1: claims narrowed, the case named as a known limit, no behaviour change

- `sync-planning.md`, parking paragraph: the hold now covers only items the side's snapshot has at the folder's path as the target names it, which on OneDrive is looked up back through the plan's earlier OneDrive moves. A new "Known limit" sentence names the case: OneDrive renames `Y` to `Z` and moves `Z/f` into a new `Z/f/a/f`; `Z/f/a` is made here inside the old `f` once `Y` has moved, and every later sync waits with only Debug lines.
- `holdOwnLandings` comment: says the OneDrive lookup goes through `snapshotPath` and the local one does not, and names the same case as a known limit (pointing at the spec).
- `TestComposeMoves_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst` comment: "on that side" became "on OneDrive" for the folder above. Its rows with a folder moving above move it on OneDrive, and it states the limit on this computer's side.
- `TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath` comment: every case makes its moves here; the same rename and move made on OneDrive is the known limit. The test name stays: true for its cases.
- Unchanged and still true: the `ownLandings` field, `atOrBelowOwnLanding`, the `dropCreatesOntoComposedItems` paragraph, `parkAwayFromItsOwnLanding` (it fires only where the same lookup finds the item).
- No test records the stall. A committed test that asserts a sync never converges would pin wrong behaviour that a fix has to invert, so the reproduction is described here instead:
  1. Synced `Y/f/c.txt`.
  2. On OneDrive: rename `Y` to `Z`, move `Z/f` aside, make `Z/f` and `Z/f/a`, move the old `f` into `Z/f/a/f`.
  3. Sync 1: here `Y` is moved to `Z`, the old `f`'s local move waits ("move waits for the next pass", Debug), and the local create of `Z/f/a` lands inside the old `f`.
  4. Every later sync: Debug waits only, WillRetry 2. Here the tree is `Z/f/c.txt` with `a` inside the old `f`; OneDrive has `Z/f/a/f/c.txt`.
  5. The same on main and on 84a0420f.
- Cause: `landingFolderHolder` on this computer's side looks the target-coordinate dirs up in the local snapshot, where the item is still at `Y/f`.
- A fix, for a later change: map the local lookup through the plan's local moves, as `planMove` gives the local path. Then add the OneDrive-side row to `ownLandingCases`.

## Finding 2: fixed

`actions.go` `planFollowUp.Dropped` now says "at or below the path of a new folder a deferred item moves into at its own path (moveComposition.ownLandings), parked or not".

## Finding 3: squash message written

`S/W79-commit-msg.txt` (S = the scratchpad): the full message for the one squashed commit of the branch.
- 149 lines, at most 71 characters each, ending with the trailer.
- It drops 84a0420f's "the creates below its target wait with it; the next pass makes the folder and the one after moves the item into it" and d18345b6's unqualified "nothing is made at or below the new folder's path on its side".
- It states the hold with the known limit.
- It describes the content rule as it now stands: one at a time, moves followed through nested folders.
- Run counts: 8,387 runs, 255 broken, against main's 8,671 and 824, measured on d18345b6's product files, which this commit does not change.
- It contains no model names, plan ids, or the words round, review, finding or mutant.

## Finding 4 and finding 1: shared stalls as plan rows

Each is the same on main; none is a regression of this branch.

- **Hold creates at an own landing below a folder that moves here.** Reproduction: on OneDrive rename `Y` to `Z` and move `Z/f` into a new `Z/f/a/f`, then sync. Expected: here the create of `Z/f/a` waits until the old `f` has left `Z/f`; the next sync moves `f` aside and then into `Z/f/a/f`, and nothing is made inside the old `f`.
- **Converge a folder moved into different new folders at its own path on each side.** Reproduction: here move `f` into a new `f/a/f` while OneDrive moves `f` into a new `f/b/f`, then sync. Expected: OneDrive's location wins as for any divergent move, so this computer ends with `f/b/f`, keeps `f/a` as a new folder, and finishes in one or two syncs. Today every sync supersedes 4 actions and leaves WillRetry 5 with nothing above Debug.
- **Converge three folders nested into each other's new folders.** Reproduction: here move `H` aside, move `Y/f` into a new `Y/f/a/f`, move `Y` into a new `H/Y`, and move the old `H` into `H/Y/f/a/H`, then sync. Expected: the folders end where they were put on both sides in a bounded number of syncs, with nothing moved into or made inside an old folder on OneDrive. Today the branch fails one create every sync ("parent folder H/Y has not been synced yet", WillRetry 4); main moves `Y` into the old `H` on OneDrive, makes `a` inside the old `f`, then waits at Debug.

## Checks (behind the heavy lock)

- `gofumpt -l internal/sync`: nothing.
- `go build ./...`: pass.
- A focused `go test -count=1` of `internal/sync`, the only package touched: pass in 8.3 s. The `-run` pattern was `TestComposeMoves_|TestBuildDependencies_|TestCarriedPath_|TestPlanBuiltActions_|TestPlanFollowUp_|TestRunOnce_NothingIsMade|TestRunOnce_AnItemMovedIntoANewFolder|TestRunOnce_AFolderMovedIntoTheFolderThatTakes|TestRunOnce_AFileEditedInASubfolder|TestRunOnce_NamesTraded|ListsEvery|Registered|Governs`.
- Every test the spec cites exists: 204 names.
- Not run, as asked: race, lint, the whole package, mutants, verify, live runs. The change is comments and spec text only.

## Not done

- No behaviour change: finding 1's fix waits for a later change, which the first plan row describes.
- The scratch work of the earlier rounds (the `w79g/` probes and logs) had already been removed from the scratchpad, so nothing of it was re-run.
