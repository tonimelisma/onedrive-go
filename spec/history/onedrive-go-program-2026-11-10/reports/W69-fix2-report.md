# W69 fix report, round 2: the declared gap, and answers that disagree

Commit `4459791b` on `fix/path-read-returns-its-item`, one new commit on `841355f9` (not amended, not pushed, no rebase). **Not installed**: after `git fetch origin main`, origin/main is at `b00e6da3` (#947), past the branch's base `4886b38c`, so `merge-base --is-ancestor origin/main HEAD` fails. `onedrive-go --version` prints `b00e6da3`: the binary now installed is main after #947, not this branch.

## 1. Blocking: overstated sentences, and whether the gap can close

**The gap cannot be closed within the bound.** A folder of the right name sitting in the wrong place two or more levels up can only be caught by checking that folder's own place. An answer without a parent path gives that place only through its parent's ID, so the check costs one read per level, which is what d945e278 paid. Graph has no ancestors read. The client does not use `$batch`, and a batch would still be throttled as one request per level. A read by ID might carry a parent path where a read by path does not, but that has not been measured. So every overstated sentence is qualified instead.

**The four sentences, rewritten so each is exactly true and names the case it misses:**
- `graph-client.md` rule (2). Its header also said a read "returns the item at that place or says it is not there". It now says reads are held "as far as OneDrive's answers tell", and rule (2) names the uncaught case.
- The fallback paragraph in `drive-transfers.md`.
- The "OneDrive Is Case-Insensitive" entry in `graph-api-quirks.md`. Its cost is now "at most two requests for each chunk of the path".
- R-6.7.22.

A new test pins the uncaught case as it stands: `TestGetItemByPath_AgreeingAnswersFromAnotherSubtreeAreNotCaught`.

**Other overstatements found in a sweep of the tree, all fixed:**
- Doc comments: `GetItemByPath` (cost per chunk, and the gap), `ListChildrenByPathPages`, `heldToListedPath`, `findInParentListing`, `resolveItemFromParentListing`.
- The CLI fake's `parentPathOf` comment said "as Graph answers". That was measured for reads only; for listings it is unmeasured.
- `graph-client.md` said "depths 1 to 3". The W69 report records only "nested reads", so the depth range is gone.
- Four test names claimed more than their tests show. They are renamed, with every citation updated:
  - `TestGetItemByPath_CostsAtMostTwoRequestsAtAnyDepth` → `TestGetItemByPath_AShortPathCostsAtMostTwoRequestsAtAnyDepth`
  - `TestListChildrenByPath_HandsOnOnlyTheFolderAtThePath` → `TestListChildrenByPath_HandsOnOnlyChildrenItsAnswersPlaceAtThePath`
  - `TestListChildrenByPath_LongPathListsOnlyTheFolderAtThePath` → `TestListChildrenByPath_LongPathListsNoFolderAnsweredFromAnotherPath`
  - `TestMountSession_AFallbackNeverTakesItsItemFromAnotherFolder` → `TestMountSession_AFallbackTakesNoItemFromAListingThatNamesAnotherFolder`

**Left as they are:** the commit messages of d945e278 ("returns the item at that place or says it is not there") and 841355f9 ("two when it does not, at any depth", "as Graph does"). Correcting them would need an amend, which is ruled out.

## 2. Should-fix: answers that disagree are now an error, not "not found"

**The listing** (`heldToListedPath` and the new `folderIDAtListedPath` in `internal/graph/items_fetch.go`). Three cases now end the listing with an error that does not wrap `ErrNotFound`:
- a child without a parent path that sits in another folder than the one read at the path;
- a read of the path that finds no folder there;
- a child that names no folder.

A child whose own parent path names another folder is still `ErrNotFound`, and the fallback reads the parent again.

**The read** (`placeByParent`, which replaces `parentIDByPath`). Tracing the read's callers turned up a second case of the same fault. When a read's answer sits in another folder than the one read at its parent's path, 841355f9 called it "not found", and the fallback then took an item from a listing that agreed with that folder read.
- Shape of the case: the read of `a/b/c/f.txt` answers right-f in folder c. The read of `a/b/c` answers x-c, a folder c in a folder b in a folder x, which the gap lets through. The listing of `a/b/c` lists x-c, which holds wrong-f.
- 841355f9 deletes wrong-f, and put uploads into wrong-d.
- Main deletes right-f.

This case is now the same error. So is a parent's path that reads as no folder, and an answer below the root that names no folder. A top-level answer outside the drive root, which is read by its ID, stays `ErrNotFound`; its fallback is the root's listing by ID. No request is added, and a child that names no folder now costs one request fewer.

**Each caller with the new error:**
- **rm, rm -r.** `ResolveDeleteTarget` gives a "resolving" error and sends no DELETE. When a delete answers 404, the retry stops with "confirm delete path state" and deletes no other item. The parent visibility wait after a successful delete can now fail on a disagreement, so rm exits non-zero after deleting. It reports an error, not a warning, and deletes nothing more.
- **mv, cp.** The reads of the source, the destination and the destination's parent all fail before any move or copy.
  - With `--force`, the cleanup of the set-aside copy (`DeleteResolvedPath`) logs its warning and deletes nothing else.
  - The visibility wait after a move can report an error after the move.
  - The leftover hint logs at Debug.
- **put, single file and folder.** The target read and the parent's visibility wait fail before any upload. The wait after an upload can report an error after the upload.
- **mkdir.** An error from the first read leads on to the walk by ID (`EnsureFolder`, whose reads sit under item IDs and are unaffected). The final wait then reports the error. mkdir only creates folders by ID.
- **get, stat, ls.** Read-only; each gives an error. `ls` reports the listing error.
- **Sync.** Its two `WaitPathVisible` calls only log at Debug, or fail a create or upload as actionable retry work. Parent IDs come from the baseline, and sync acts on nothing the path read answers. `FindExistingChild` and the upload lookup are by ID and unaffected.

**Can any caller still delete, overwrite or move an item main would not have touched?** No. The reasoning:
1. Every read the branch accepts, main accepts too. They send the same first request, and main's checks are a subset of the branch's.
2. The branch returns "not found" only where main's read also refuses (a 404, or an answer at another exact path or with another name), plus for a top-level answer outside the root, whose fallback is the root's listing by ID.
3. Every by-path listing page the branch hands on, main hands on too. The pages are the same and so is the name rule: exact spelling first, otherwise the first spelling OneDrive treats as the same. Where the branch refuses a listing as not found, main took from a listing whose own answers place a child in another folder.
4. Every other refusal is an error, and nothing acts on it.

By induction up the fallback's climb, the branch acts differently from main only where main acts on an item that its own answers place outside the path.

**Tests that pin this.** Each fails on 841355f9 (checked by overlaying 841355f9's `items_fetch.go` and `session.go`):
- `TestRunRm_DeletesNothingWhereAnswersNameDifferentFolders`: both shapes, for a file and for a folder with `-r`. 841355f9 sent `DELETE wrong-f` and `DELETE wrong-d`.
- `TestRunPut_UploadsNothingWhereAnswersNameDifferentFolders`: both shapes, single file and folder. 841355f9 sent a POST into wrong-d.
- `TestMountSession_DeleteResolvedPathStopsWhereAnswersNameDifferentFolders`: recycle-bin and permanent delete. This is the retry path that rm and the `--force` cleanup of mv and cp go through. 841355f9 deleted wrong-f.
- `TestMountSession_AFallbackStopsWhereAnswersNameDifferentFolders`: the delete target and the visibility wait.
- The changed graph subtests:
  - for the read: an answer in another folder, no folder at its parent's path, an answer in no folder, the parent at another path, a parent of another name;
  - for the listing: another folder's children by ID, a read of the path that finds another folder, children that name no folder.

**Against main's product files** (the earlier review's overlay), the CLI and driveops tests also fail, because main acts: it deletes right-f or right-d and posts into right-d. Which of the two items is right cannot be told from the answers, which is why the branch now acts on neither. On a drive whose answers disagree (none has been observed), that costs availability compared with main. It never acts on a wrong item.

## The declared gap: verdict

- **Scenario:** the answers carry no parent path, and the path route resolves `a/b/c/...` into `x/b/c/...` consistently, for reads and listings alike.
- **What each command does there:**
  - rm and rm -r delete `x/b/c/f.txt`, or a folder there.
  - mv moves that item, or moves into it as the destination. With `--force` it sets aside x's file and deletes it.
  - cp `--force` replaces x's file the same way.
  - put uploads into `x/b/c`, replacing a file of the same name.
  - mkdir says the folder exists and prints x's ID.
  - get, stat and ls read x's items.
  - sync uses the read only to confirm visibility, and acts on nothing.
- **Against main at 4886b38c:** main takes the same item for every command, through its leaf-name-only check. So the gap is **equal to main**. Elsewhere the branch is better than main, and in the gap it is worse than d945e278, which caught it at one request per level. The gap needs answers without a parent path: Personal answers carried theirs when measured, and Business and SharePoint are unknown.

## 3. The review's notes

- **Note 3: `ls` is stricter than main through a shortcut.** Not fixed. Whether Graph answers a by-path listing inside a shortcut in a drive session is unknown, and no live check ran in this round. The cheap check is `ls /<shortcut>` and `stat /<shortcut>/<file>` on the test account, with main's binary and then the branch's. On the branch now:
  - children carrying the sharer's path give "not found";
  - children without the root marker that carry the target's ID give the disagreement error;
  - children without a `parentReference` give the "without its folder" error.
- **Note 4: commit-message wording.** The new commit message states the cost per chunk and does not say "as Graph does". The depth range in `graph-client.md` is removed. 841355f9's own message stays, since amending is ruled out.
- **Note 5: drive part of the parent path ignored.** Not changed. A child that carries a parent path is checked by that path alone, as reads are, on main too. Comparing `parentReference.driveId` would first need a probe of its known truncation and letter-case problems in listings, and none has run. Whether Business percent-encodes the path is unknown.
- **Note 6: path reads through a shortcut.** Unchanged and still unmeasured.
- **Note 8: `readCannotSettleName` exists twice.** Unchanged. Keeping one copy would need an exported graph function, which this row ruled out.

## Checks

- `go build ./...`, `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` all pass.
- `go test -count=1` of `./internal/graph/... ./internal/driveops/... ./internal/cli/...` passes, and so does the same run with `-race`. After the last edit, which was test-only, cli was re-run both plain and with `-race`, and passed.
- Source rules:
  - All seven repository-rule steps (output-boundaries, test-conventions, spec-governs, layering, retired-backlog-ids, mutex-ownership-docs, requirement-traceability) pass on the real tree, run through a scratch overlay test that left no file in the repository.
  - The devtool tests matching RealRepoIsClean, TestSpecGoverns, E2E tier and TestStressRepeatedStepsDeclareEveryTestTheirRuleNames pass.
  - Every test name the touched design docs cite exists, checked by script. The three missing names that script reports were already missing on main: two historical names in the quirks reference and the `TestE2E_` prefix in requirements.
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint`: 0 issues, and actionlint passes. The first run found one testifylint issue (assert → require), which was fixed before the commit.
- Test tiers: every new test is a unit test in graph, driveops or cli, with `// Validates:` on the line before it. None is in `internal/sync` or `e2e`, so none needs a stress or tier declaration. None is marked exhaustive. The CLI tests are not parallel, because they set the environment through `setTestDriveHome`.
- Red runs: on 841355f9's product files, every new test, and every subtest whose expected outcome changed, fails, except the gap pin, which passes on both trees by design. On main's product files, the new CLI and driveops tests fail because main acts on the item in c.

## Still open

- The new error has no CLI wording of its own. The user sees the raw message ("answers for … disagree: …"), not "was not found".
- When a disagreement shows up only in a confirmation wait after a mutation (rm's parent, mv's destination, put's file, mkdir's folder), the command reports failure after the mutation succeeded.
- Not run: live probes, a full `devtool verify`, and the sync tests.
