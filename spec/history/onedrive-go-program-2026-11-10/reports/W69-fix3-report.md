# W69 fix report, round 3: waits that ride out disagreement; "elsewhere" is not found

Commit `f3636c71` on `fix/path-read-returns-its-item`, one new commit on `4459791b` (not amended, not pushed, no rebase, no PR). **Not installed**: origin/main is at `b00e6da3`, which the branch does not contain.

## Finding 1: a wait stopped at the first disagreement (fixed)

**What changed**
- New exported sentinel `graph.ErrPathAnswersDisagree`. Every error for answers that do not agree on the folder at a path wraps it:
  - the read's two cases in `placeByParent`;
  - the listing's case in `listedChildPlaced`;
  - the listing's "the read found nothing there" case in `readListedFolder`.
- `MountSession.WaitPathVisible` now reads the path again on the sentinel, exactly as it does on `ErrNotFound` (`visibilityMaySettle`). It lists no folder because of the sentinel: `resolveConvergingPath` falls back to a listing only on `ErrNotFound`. If the answers still disagree when the schedule runs out, it ends with `*PathNotVisibleError`, which gives rm its note.
- `ResolveDeleteTarget` and the delete convergence still fail on the sentinel at once, with no wait.
- Test seam: `SessionRuntime.VisibilityWaitSchedule`, an exported field like `TokenSourceFn`. When it is not empty, it replaces the delays of the visibility wait and the delete convergence. The CLI tests use it so a wait runs without sleeping.

**Tests.** Each fails on 4459791b's `items_fetch.go` and `session.go`, overlaid with a shim that only declares the sentinel name:
- `TestMountSession_AVisibilityWaitReadsAgainWhileAnswersNameDifferentFolders`, both shapes:
  - "settling": the answers agree after the first wait, and the wait returns right-f.
  - "still disagreeing": the wait ends in `*PathNotVisibleError` after 2 waits.
  - In both: the other folder is never listed, and in the read-versus-folder-read shape no folder is listed at all.
- `TestRunRm_NotesAParentWhoseAnswersStillDisagreeAfterTheDelete`: rm deletes right-f, the parent's answers keep disagreeing, rm exits 0 and prints its note.
- `TestMountSession_ADeleteTargetIsNotTakenWhereAnswersNameDifferentFolders` (renamed from `…AFallbackStops…`, which also covered the wait): the delete target is refused with the sentinel, with no wait.
- Moved to assert the sentinel with `ErrorIs`: `TestMountSession_DeleteResolvedPathStopsWhereAnswersNameDifferentFolders` and `TestRunRm_DeletesNothingWhereAnswersNameDifferentFolders`.
- `TestRunPut_UploadsNothingWhereAnswersNameDifferentFolders` now expects put's parent wait to end not visible, with no request that changes anything.

**Commands now:**
- mkdir, put and mv, after their change has landed, succeed when the answers settle within the wait. When they do not settle, the commands fail as they do for a path that is never found.
- rm notes the parent instead of failing.
- put's parent lookup and sync's parent gate wait, then fail, before any upload or create.
- A persistent disagreement in a wait now costs the whole visibility schedule, about 96 s, the same as a persistent "not found".

## Finding 2: answers that agree the item is elsewhere (fixed)

The answers are now placed explicitly (`answerPlace`, `pathAnswer`, `readPlacedAtPath`).

**`ErrNotFound`: the answers put the item elsewhere**
- the answer's own parent path or name is another place's;
- a top-level item sits outside the drive root;
- the folder the item sits in is answered at another path or under another name. These are cases A1 and A2 of the review's probe A.
- another folder is answered at the parent's path by its own parent path. This is the reverse case, which the sweep found: the old "nested, in another folder" subtest now expects not found.

**`ErrPathAnswersDisagree`: the answers do not show whether the item is at the path.** The read of the parent's path:
- finds nothing; or
- answers another folder held there by its name alone; or
- answers another folder that is itself at another place.

**The listing** follows the same rules, using the folder read placed as GetItemByPath places it. A placed folder is "settled" when its own parent paths and IDs hold it there: a child of a folder the answers put elsewhere, or of a folder other than a settled one, is not found.

**Wording.** The doc claim that "which of two answers is wrong cannot be told" now says, and is exactly true: "the answers do not show whether the item is at the path", and the code names the cases. This is in the sentinel's doc, the `placeByParent` and `heldToListedPath` comments, graph-client.md rule (2), drive-transfers.md, the quirks entry and R-6.7.22.

**Sweep.** The sweep of the branch diff for "cannot be told", "two answers" and "name different folders" left only the test names and fakes, whose shapes do name two different folders.

**Tests.** Each fails on 4459791b:
- `TestMountSession_AReadTheAnswersPutElsewhereFallsBackToTheListing` (probe A1 and A2): the delete target is right-a, found through the listings.
- New or changed subtests in `TestGetItemByPath_AnswerWithoutParentPathIsPlacedByItsParentsID`, `TestGetItemByPath_TheParentThatPlacesAnAnswerIsHeldToItsPath` and `TestListChildrenByPath_HandsOnOnlyChildrenItsAnswersPlaceAtThePath`.

**Cost.** No request is added. Probe A now costs the fallback's climb, as on 841355f9.

## Finding 3

Not taken here: it is a plan row (CLI wording for the sentinel).

## Checks

- `go build ./...`, `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` pass.
- `go test -count=1` passes on the touched packages and the packages that import them: graph, driveops, cli, syncverify, multisync and sync. The sync run, unshortened, took 9 minutes.
- The same with `-race` also passes; the sync run took 100 minutes.
- After that I changed one function in graph without changing its behaviour, to pass gocyclo (`placedByFolderAnswer` split out of `placeByParent`). graph, driveops and cli were then re-run plain, and graph with `-race`; all pass.
- Repository rules: all seven steps pass on the real tree, through a scratch overlay test that left no file in the repo. So do the devtool RealRepoIsClean, SpecGoverns and stress-declaration tests.
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint`: 0 issues, actionlint passes.
  - The first lint run was cut off with the check chain when that chain hit its 2-hour background limit.
  - The second run found the gocyclo issue, which is fixed in the commit.
- Test tiers: every new test is a unit test in graph, driveops or cli, with `// Validates:` on the line before it. None is in `internal/sync` or `e2e`, and none is exhaustive. The CLI tests are not parallel, because they use `setTestDriveHome`.
- Red runs, against 4459791b's `items_fetch.go` and `session.go` plus a shim that only declares the sentinel name and the runtime field: every new test, and every subtest whose expected outcome changed, fails.

## Not run

- Live probes.
- The full `devtool verify`.
- e2e tests, beyond the tagged vet.
