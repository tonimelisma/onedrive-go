# W69 fix report: the four should-fix findings on d945e278

Commit `841355f9` on `fix/path-read-returns-its-item`, one commit on top of `d945e278` (not amended, not pushed). origin/main (`4886b38c`) is its ancestor after a fetch; installed: `onedrive-go --version` prints `onedrive-go version 841355f9`.

## 1. Placing an answer without a parent path is bounded

- Changed: `GetItemByPath` is now one read held to the path by the answer alone (`readItemAtPath`: its parent path, or failing one its leaf name), then, only for an answer without a parent path, one read of its parent (`parentIDByPath`). The parent is read by its own path through `readItemAtPath` and held to it as far as its own answer allows (its parent path, or its name), and is not placed by its own parent in turn. The drive root (top-level name) is read by its alias, as before.
- Requests per read: with parent paths 1 at depth 0, 1 and 5; without 2 at depth 0, 1 and 5 (a path over 240 escaped characters adds one per further chunk, as before).
- 1,000 reads of files at depth d (scratch probe, same fake on all three trees): main 1,000 at every depth and shape (no placement at all). d945e278: 1,000 with parent paths; without them 2,000 / 3,000 / 7,000 at depth 0 / 1 / 5. Fix: 1,000 with parent paths; 2,000 without at every depth.
- Pinned by `TestGetItemByPath_CostsAtMostTwoRequestsAtAnyDepth` (on d945e278: 3 requests at depth 1, 7 at depth 5) and `TestGetItemByPath_TheParentThatPlacesAnAnswerIsHeldToItsPath` (on d945e278 the parent of the right name without a parent path failed with the grandparent's 404).
- No parent identity is kept across calls. The drive root's ID is read again on every top-level read without a parent path (not memoized). The one ID kept for reuse is the folder ID of finding 4, held on the stack of one listing call: it is read at the path during that call, and a stale value can only make a child mismatch, which refuses the listing; it never accepts a child.
- What stays short: on a drive whose by-path answers carry no parent path, a folder of the right name that the path route found in the wrong place two or more levels up is not caught (d945e278 caught it at one request per level; main checked nothing). Unknown for Business and SharePoint: whether their by-path answers carry `parentReference.path` (no Business test drive). Measured earlier on the Personal test account: depths 1 to 3 carried it, so a Personal read costs 1 request before and after.

## 2. Path forms main accepted on a mount session are found again

- Main's normalisation, read: `CleanRemotePath` = `strings.Trim(path, "/")`, `SplitParentAndName` through it, mkdir's own skip of empty segments; nothing treats `.` or `..` (they are names on main and here). So the forms are slashes at either end and any run of doubled slashes inside. On main a mount session found `a//b` only through the parent-listing fallback (WaitPathVisible, ResolveDeleteTarget); its `ResolveItem("Documents//report.docx")` answered `segment "": graph: not found` (seen by running the new test on main's product files).
- Changed: `CleanRemotePath` drops the empty names (`strings.FieldsFunc` on `/`), so every session entry (ResolveItem, ListChildrenPages, the parent-listing fallback, SplitParentAndName) and every CLI caller sees `a/b`. Mount sessions now find `a//b` by one read per name with all three lookups; a drive session reads `a/b` directly (main sent `a//b` to the path route). mkdir's now-dead empty-segment skip is gone. Printed paths change: mkdir's `created` and put's `--json` path drop the empty names (cli.md updated); mv's folder-into-itself check now sees `a//b/c` as inside `a/b`.
- Pinned by `TestMountSession_APathWithEmptyNamesIsThePathWithoutThem` (d945e278: ErrInvalidPath; main: not found for ResolveItem), `TestCleanRemotePath` and `TestSplitParentAndName` new cases, and end to end on a shared folder (shared canonical ID, mount root) through the CLI fake drive: `TestFileCommands_OnASharedFolderAPathWithEmptyNamesIsThePathWithoutThem` runs `mkdir /Reports//2026/` and `put notes.txt Reports//2026//notes.txt` (d945e278: "confirming folder "Reports//2026" visibility: ... invalid remote path").

## 3. Page stop and folder-is-not-the-file are pinned

- `TestMountSession_ParentListingStopsAtThePageThatHoldsTheName`: name on page 2 of 3, by path and by ID after the parent's path lags, for the delete target and the visibility wait; 2 pages each. The reviewer's mutant (`overlay-mut-noparentstop`) reads 3 and fails all four.
- `TestLookupUploadTarget_AFolderOfTheNameIsNotTheUpload` (read answers a folder; listing holds a folder): ErrNotFound. Fails on `overlay-mut-lookupfolderok`.
- `TestCreateFile_AFolderOfTheNameIsNotTheCreatedFile` (same two cases): ErrCreateOutcomeUnknown. Fails on `overlay-mut-createdfolderok`.

## 4. A listing by path is held to its path

- Changed: `ListChildrenByPathPages` holds each page of a short by-path listing to the path before handing it on (`heldToListedPath`). A child with a parent path must equal the asked path (`namekey.EqualPath`). A child without one must sit (`itemSitsIn`) in the folder read at the path (`GetItemByPath`), read once per listing and only when such a child appears; a folder answered without an ID is an error. Any other child, or one naming no parent, ends the listing with ErrNotFound before its page is handed on. Long paths were already listed by ID after GetItemByPath. Cost: 0 extra requests when children carry parent paths, otherwise 1 per listing (2 when the folder's answer lacks its parent path too).
- Effect: the delete/visibility fallback gets ErrNotFound for a misplaced listing and, as for a lagging parent path, reads the parent through its own parent's listing and lists it by ID, so it takes the right item. `ls` of such a path reports not found instead of another folder's children; the leftover-aside hint logs at Debug. Mount sessions and every by-ID listing name their folder by ID and are unchanged.
- Pinned by `TestMountSession_AFallbackNeverTakesItsItemFromAnotherFolder` (the reviewer's probe as a test: fails on main and on d945e278 with the wrong item) and `TestListChildrenByPath_HandsOnOnlyTheFolderAtThePath` (six cases; the misplaced ones are handed on by d945e278).
- Unknown: whether Personal by-path listings carry `parentReference.path` on each child (not measured; if not, each by-path listing costs the one extra read); whether Graph ever lists another folder for a path (the fake models it after the documented read quirk; unmeasured); Business and SharePoint.

## Notes

- 5 (escaped IDs on mutation routes, live): not fixed here. No live run was needed for the four findings; PR CI's fast e2e (CRUD and shortcut smoke) exercises DELETE, POST children, PATCH and upload sessions on Personal: read its result before merge.
- 6 (shortcut path in a drive session): not fixed, unmeasured whether Graph answers such reads. The new listing check refuses a listing whose children carry the sharer's path, as GetItemByPath already refuses the read.
- 7: fixed. The CLI fake drive answers `parentReference.path` from its folders, as Graph does; graph and driveops listing fixtures carry theirs.
- 8 (readCannotSettleName twice): not fixed; one copy needs an exported graph function, which this row ruled out.
- 9: fixed. `nameOnPage` (driveops) and graph's read-back listing (`listedChildNamed`, was `firstListedChild`) stop only at the exact spelling; a page with only another spelling OneDrive treats as the same reads on, and that spelling is the item when no page holds the exact one, as main's whole-folder match did. Pinned by `TestFindExistingChild_TheListingPrefersTheNameAsSpelled` and `TestCreateFile_TheReadBackPrefersTheNameAsSpelled` (both fail on d945e278). `matchChildByName` is gone (unused).
- 10: itemRoute's doc now says the drive ID goes in as it is. mv.go:90 is reachable, as noted; no change. The first report's other "seen and not fixed" entries stand unchanged.
- Also fixed: ResolveItem's doc (mount walk, cleaning), the docs of the parent-listing fallback and of FindExistingChild.

## Callers re-checked after the change

- `GetItemByPath`: `MountSession.ResolveItem` (drive sessions: stat, get, ls fallback, mkdir, cp, mv x3, put target, rm, delete convergence, every WaitPathVisible incl. sync executor), the long-path listing, the listing's folder read. ErrNotFound and other errors keep their meaning; a nested no-path answer whose ancestor read failed on d945e278 is now placed by its parent alone.
- `ListChildrenByPathPages`: `MountSession.ListChildrenPages` for drive paths below the root, reached by ls, the leftover-aside hint and the parent-listing fallback (above).
- `CleanRemotePath`/`SplitParentAndName`: mkdir, put (4), get, mv (5), rm, replace_destination (2), session (4); all take the cleaned form.
- `FindExistingChild`/`nameOnPage`: EnsureFolder, putFile after 409, lookupUploadTarget (put and sync), the mount walk, the parent-listing fallback. `listedChildNamed`: readCreatedFileByName (every file create without an item in its answer).
- No sync product file changed; sync reaches the changed code through the transfer manager and PathConvergence.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1` of `./internal/graph/... ./internal/driveops/... ./internal/cli/...` on the final tree: pass; the same with `-race` behind the lock: pass.
- Callers outside the touched packages: `go test -short -count=1 ./internal/sync/... ./internal/multisync/...` behind the lock: pass.
- Source rules (`./internal/devtool`): the RealRepoIsClean tests, `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`, the full-e2e tier and spec-governs tests: pass. No new test is in `internal/sync` or `e2e`, so none needs a stress or tier declaration; none is marked exhaustive. Every test name the touched spec files cite exists (checked by script).
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint` behind the lock: golangci-lint 0 issues, actionlint pass.
- Red runs: each new test was run on d945e278's product files first (and on main's for the empty-name and other-folder tests, through an overlay of main's graph and driveops files); the finding-3 tests on the reviewer's three mutant overlays. No live run was made.

## Scratch

Probe trees, overlays and logs only under the session scratchpad; nothing left in either repository.
