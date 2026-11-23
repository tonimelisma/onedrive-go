# W79 fix 5: released deletes wait for their content, refusals in the exhaustive check

One new commit `84a0420f` on `0c640cb0`, branch `fix/remote-move-into-recreated-folder`: not amended, not rebased, not pushed, not installed. 13 files: `planner_dependencies.go`, `planner_move_composition.go`, `planner_actions.go`, `planner.go`, `actions.go`, `spec/design/sync-planning.md`, the golden list, and six test files.

## Blocking 1: a delete waits for the create that carries its content

**Changed.**
- Rule 7 of `buildDependencies` (`addContentCarrierDeps`): a delete waits for each create on its side that writes the deleted file's content at another path.
  - Keys (`contentKey`): synced local identity for a remote delete and an upload; item ID for a local delete and a download; deletes of files only.
  - `carriesContentOf` moved to `planner_dependencies.go` and is defined by those keys.
  - Indexed (`contentCarriers`, `contentDeletes`), so the build stays linear.
  - The scanning oracle applies the rule by scan. Its generator now draws identities and item IDs, and a counter of content edges must be positive.
- Cycles (the "check that no swap makes a cycle" of the smallest fix). An edge whose create already waits for the delete would close a cycle, and is left out.
  - Such edges are found with one pass over the strongly connected components (`dependencyComponents`, the Tarjan code of `dependencyCycleMembers`, now shared), only in a plan that has a content edge.
  - The case is real: the upload of a1.txt edited in a folder renamed here waits for the folder's move, which waits for the delete of the old copy. `TestRunOnce_AFolderRenamedHere…` drops a cycle without the exclusion.
- Second rule (`deletesAheadOfTheirContent`), one sentence in the spec. Where the left-out create waits for the delete only through the move of a folder above the file, and lands elsewhere than where that move carries the file, the delete waits for the next pass instead.
  - Found by the widened check: `held L[mv p x/p, mv x/f p] R[]` with the create of the new x refused lost f's content on OneDrive in the releasing pass. That run failed only on the branch.
  - Two exceptions keep the old order: a create at the carried path (a1.txt edited in A renamed B), and a create behind a folder's delete (x moved up onto P's path, P deleted).
- Third change, also found by the widened check: parking.
  - The problem: an item moved into a new folder made at its own path held that path itself. A file waited forever; a folder dropped a dependency cycle on every pass (the "out of scope" class of the earlier note 8).
  - Now it parks at a free `.swap-N` name (`parkAwayFromItsOwnLanding`, sharing `park` with the swap parking). Creates below its target wait with it (`belowParkedTarget`, `moveComposition.parkedTargets`). When a folder above it moves on that side in the same plan, it waits a pass instead.
  - Needed for `held L[mv x p/f, mv p x] R[]` with the create of x refused. That run failed only on the branch: the refusal led into a file moved into a new folder at its own path, and the sides never met.
- Spec: the claim "it waits for that work through the ordinary dependencies" is now true for creates as well. The new rules are stated in the move-composition, delete and Dependency Build paragraphs, and `Dropped`/`Deferred` say what they now count.

**The probe as a test.** `TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveWhileACreateBeforeItsUploadFails` covers both renames of z.txt.
- It checks z's content on OneDrive after every sync, and at every delete through `countDeletesDropping`, a new `afterDelete` hook of the scenario remote.
- On `0c640cb0` it fails: after the refused sync OneDrive holds `keep`, `c` and no z.
- After the fix it passes, converging once the refusal's retry is due.

**Widened executor** (`planner_exhaustive_test.go`).
- Every pair whose passes delete something runs again once for each create of each pass, refused that time it is planned.
- A refusal is `exRefusal{action, nth}`: it is named by the create and the how-many-th planning, not by the pass, so that main's and the branch's runs compare.
- The refused pass never ends a run early, and the run gets one more pass. Content is checked after every pass, as before.
- A full run now also fails on a golden line that the pairs no longer make (gap 2 of note 9).

### Pair counts

Committed space: 5,275 pairs. A run is a pair or one of its refusal runs. The counts come from a probe overlay that writes the broken runs; main is `238bea94`'s product files with the branch's tests.

| tree | runs | broken runs | broken pairs | broken refusals |
| --- | --- | --- | --- | --- |
| main | 8,671 | 824 | 344 | 480 |
| `0c640cb0` | 8,313 | 761 | 316 | 445 |
| this commit | 8,391 | 255 | 154 | 101 |

- **Main fails 824, the branch fails 255, only on the branch: 0.** Every run broken here is broken on main. 7 runs broken on both break fewer invariants here, none more. 569 runs fail only on main.
- `0c640cb0` had 24 runs failing only there, 17 of them content removed under a refusal. That is the class of this finding, now visible to the check.
- Without refusals: main 344, `0c640cb0` 316, this commit 154 broken pairs. Parking fixed most of the held-start pairs that never converged.
- Three actions on one side (3,963 pairs, outside the committed space, refusals included):
  - main: 580 broken runs (329 pairs); this commit: 254 broken runs (228 pairs); only on the branch: 0.
  - One pair breaks `left-held` here and `no-quiet-plan` on main: `synced L[mv p x/p, mkdir p, mv x p/f]`. It converges on neither tree.
- Cost: the space with refusals took 141 to 269 s plain on this host while other jobs ran, against 85 s before. It stays in the exhaustive tier (`skipInShort`, every 40th pair under race). Nothing was cut.

## Blocking 2: the dropped-create and cycle parts in the `-short` tier

Each mutant ran on the final tree, `-short` with `-run` over the planner, dependency and new RunOnce tests (`res/` in the scratchpad):

| mutant | fails (in `-short`) |
| --- | --- |
| mDropped (dropped creates not held) | the planner row "z renamed here onto P/f, which f still holds on OneDrive this pass", and `TestRunOnce_AFileRenamedHereOntoADeletedFileInAFolderMovedOnOneDriveStaysThereUntilItsUploadRuns` (the reviewer's pairs as a real-engine scenario: P/f deleted and z.txt renamed onto it while OneDrive moves P to q or into x; no delete may leave OneDrive without z's content) |
| mCycles (no hold for cycled work) | the planner row "z moved up onto its folder P's path, P deleted, P/y's move out of P in a cycle" |
| mRebase (rebased-write hold off) | `TestPlanBuiltActions_AHeldRebasedCreateLogsTheFolderItWaitsFor`, the planner row for g, and two RunOnce tests |
| mTrimmedHeld (should-fix 5) | the planner row for an upload trimmed below the keep-both K |
| mEdge (no content edge) | the two dependency rows, the oracle, the refused-create RunOnce test, the moved-up RunOnce test, and a planner row |
| mExclusion (cycle edges kept) | the cycle row, `TestRunOnce_AFolderRenamedHere…`, the parking scenario and two more |
| mAhead (second rule off) | the planner row for P renamed Q with Q/f moved up to P, and `TestRunOnce_AFileMovedUpOntoItsRenamedFoldersPathStaysOnOneDriveUntilItsUploadRuns` |
| mPark (parking off) | `TestComposeMoves_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst` and two RunOnce tests |
| mParkedTarget (creates below a parked target not held) | `TestRunOnce_AFolderMovedIntoTheFolderThatTakesItsPathConvergesWithAnEditInIt` |

The whole `-short` tier also ran on the tree before the last row fix (188 s, pass), and under mDropped and mRebase (both failing the tests above).

## Should-fix

3. **Rebased-write hold.**
   - Each held create now logs `create waits for its folder's move on OneDrive` at Debug, with `folder` and `folder_on_onedrive` (`TestPlanBuiltActions_AHeldRebasedCreateLogsTheFolderItWaitsFor`).
   - The comment and the spec now name what the hold prevents: a OneDrive move applied as a download, and a file downloaded beside one made here under a name OneDrive takes for the same (seen in the planner model). They also name its cost (a new file under a free name waits for the folder's move) and say that writes SQLite reconciles at the folder's path here are not held.
   - The new-file RunOnce test now says it pins the wait. The harm is pinned by the moved-here test and the planner row.
4. **History sentence** removed from the spec.
5. **Trimmed actions** stay held work, pinned by a row: an upload of z's content trimmed below the keep-both K holds z's delete. mTrimmedHeld fails that row.
6. **The test's comment** now says what the model does not see: the store is rewritten each pass, actions run in one order, content is compared between passes only, only creates are refused, plus the list of areas the earlier note 6 named. The spec says so in one sentence. The commit's evidence claim now rests on the widened check.

## Checks (final tree, each behind the lock)

- Pass: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`, `gofumpt -l` and `goimports -l` (nothing).
- `go test -count=1 ./internal/sync/` (plain, whole package): pass, 600 s.
- `-race` on `TestComposeMoves_|TestBuildDependencies_|TestPlanBuiltActions_|TestDropReconciliation|TestPlannerPlanCurrentState_|TestIsolateDependencyCycles|TestPlannerExhaustive|TestPlannerModel_|TestRunOnce_`: pass, 2,470 s.
- List tests (`ListsEvery|Table|Governs|Exhaustive|Registered`): pass.
- devtool `RealRepoIsClean` (4) and `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`: pass.
- Every test the spec cites exists.
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint`: pass. The first run found a cyclomatic complexity of 16 in `resolve`, fixed by `resolveMove`.
- After these checks only three comment and message lines of `planner_exhaustive_test.go` changed (wording); `gofumpt -l` and `vet` ran again on them.

## Live

Test account and synthetic data only: the binary of the final tree was built into the scratchpad (not installed) and run with `x79live.py`, case `kept` (the row's main scenario). The sync folder was a new folder under `~/tommi`.
- First sync: exit 0.
- While X2 lasts: the sync exits 1, with only the collision's two warnings. On OneDrive x stays at x, f at P/f and z, each the same item.
- After X2 is removed: one sync exits 0, both trees are equal, and f and x are the items they were. One more sync has nothing to do.
- No dependency-cycle, error or superseded lines. The proof folder, private tree and sync folder were removed, and nothing of the run is left under `~/tommi`.

## Seen, not fixed

- Refused folder create, both trees: after it, `held L[] R[rm p, mv x X2/x]` and its family never reach a quiet plan. The model re-keys x's baseline between `x` and `X2/x` on every pass. Fix: find which composition re-key and its inverse both plan after the refused create (`planner_move_composition.go`, re-keys).
- `held L[mv x/f x/g] R[new x/g]` still removes f's content from OneDrive in pass 1 on both trees (known).
- An edited file in a folder renamed here still has its old copy deleted before the upload of the edit. No invariant breaks (the user overwrote it), but a failed upload leaves the edit only here for a pass. Fix: run that delete after the folder's move, by item ID, which needs the outcome to commit at the moved path.
- `carriesContentOf` compares device and inode, not birth time (note 12). A reused inode orders a delete after an unrelated upload, never the reverse.
- Swap parking (`parkOneOccupancyCycle`) does not hold creates below the parked item's target as the new parking does. Not seen failing.
