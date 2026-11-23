# Report: slice listings refuse a repeated link

Commit `d03415ba` on `fix/slice-listings-refuse-a-repeated-link` (parent `c0792e98`), not pushed.

## What changed
- `internal/graph/items_fetch.go`: new unexported `askedPages` (`newAskedPages`, `ask`): counts one listing's pages, fails past the cap with `graph: <listing> exceeded N pages`, then on a link back with `graph: <listing> looped back from page N to page M`, before the request; `visitChildPages` uses it (its own map is gone).
- `internal/graph/drives_identity.go` (`fetchAllDrives`), `drives_shared.go` (`SearchDriveItems`), `items_permissions.go` (`ListItemPermissions`), `drives_sites.go` (`SearchSites`): each loop asks `askedPages` before each request; cap error texts unchanged.
- `internal/graph/client.go`: `defaultMaxListPages` comment says the cap bounds ever-new links and a circular chain fails at its first repeat.
- `internal/graph/drives_pagination_test.go`: `pagedListingCalls` returns each listing's result; `loopingListing` fake server; new table test.
- `spec/design/graph-client.md`: "Paged Listings" says the rule holds for every non-delta listing and that delta keeps no such record; evidence row at line 93 cites the new test.

## Tests added
- `TestPagedListings_ALinkBackToAPageAlreadyAskedForFailsTheListing`: over all eight non-delta listings, a self-link and a two-page cycle each: the error names the loop (`from page 1 to page 1`, `from page 2 to page 1`), no page is asked for twice, the caller gets nothing. On the base: drives, site drives, drive item search, item permissions failed with `exceeded 3 pages` after asks `[1 1 1]` / `[1 2 1]`; site search returned `page-1` three times (and `page-1, page-2, page-1`) with no error; the three children rows passed.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/graph/... ./internal/driveops/... ./internal/cli/...`: pass.
- `go test -race -count=1 ./internal/graph/...` behind the lock: pass.
- `RealRepoIsClean` (4 PASS); `ListsEvery|Table|Governs|Exhaustive|Registered` in graph and devtool: pass.
- Lint `./internal/graph/...` behind the lock: `0 issues.` (`W76-lint.log`).
- Every test name in the spec checked by hand.

## Deviations
- `askedPages` owns the cap check as well as the repeat check (cap first, as decision (d) orders). Every cap error was already `graph: <listing> exceeded N pages`, so no text changed, and the loops keep no second counter.
- The new table runs over all eight non-delta listings, not only the four slice loops, so one test holds the rule the spec states. It shares `pagedListingCalls` with the cap test, whose site search limit is now `testListPages` (was 10; that test serves empty pages, so it does not care).
- Lint ran twice. A comment-only change to the test file and the spec came after the first run, and the second run covers the final tree.
- `TestSearchSites_StopsFollowingAtTheLimit`: still tests what its name says (the limit ends the walk after page 2, before the first repeat at page 3); its fake is unchanged.

## Also fixed
- `drives_identity.go:72`: dropped the history sentence from the `fetchAllDrives` comment.
- `client.go:55`: comment named `maxDeltaPages` for `defaultMaxDeltaPages`.
- `drives_pagination_test.go`: three `// Validates:` lines moved to directly before `func`; two history sentences in the test comments put in the present tense.
- `drives_shared.go`: page items variable `page` renamed `pageItems` (it sat beside `pages`).
- `graph-client.md:484-492`: history paragraph put in the present tense; it now cites all four follow-the-link tests.

## Not fixed
- `internal/graph/delta.go:181-187`: `deltaAllPages` asks again for a next link it already followed, and only `maxDeltaPages` (10000) stops it, with every page's items held; `TestDeltaAll_MaxPages` (`delta_test.go:313`) serves exactly such a link. Out of scope by decision. Fix: keep the tokens asked for in `deltaAllPages` and fail a repeat before asking.
- `internal/graph/delta_test.go:313`: `TestDeltaAll_MaxPages` has no `// Validates:` line. Fix: add `// Validates: R-6.7.32`.
- `internal/graph/drives_shared.go:44`: `fetchItemPage` takes a `label` that its one caller always passes as `"search"`. Fix: drop the parameter and name the search in the decode error.

## Unfinished
- Nothing.

## For the reviewer to test
- `internal/sync` (`permission_probe_remote.go` calls `ListItemPermissions`): permission probe tests. Behaviour changes only on a repeated link.



## Review fixes (by the orchestrator, 2026-11-16)

Commit `a7f878d9`, one commit on `41919e44`. Paths are under
`internal/graph/` unless said otherwise.

- Finding 1: `fetchItemPage(ctx, path, label)` is now
  `fetchSearchPage(ctx, path)` in `drives_shared.go`; the label only
  ever was "search". The error text a user sees is unchanged.
- Finding 2: the two comments in `drives_pagination_test.go` that
  spoke of a circular next link now speak of an endless chain of new
  links, which is what the page cap is left to stop.
- Finding 3: the looped-link table has a third row, a later page that
  links to the one before (page 3 to page 2), for every listing. The
  row passes before and after: it was not seen failing, because the
  only mutation that breaks it (remembering just the first page)
  loops until the cap. It holds that every page asked for is
  remembered.
- Note 4: the hash at the top of this report is from before the
  rebase; the commit is the one named above.
- Note 5: `spec/design/graph-client.md` quotes the error with its
  `graph: ` prefix.
- Notes 6 to 8: no change. Note 8 is the delta plan row's material.
- Note 9 (a site whose libraries cannot be listed vanishes from
  `drive list` and `drive search` without a word): a new plan row.
- Checks on this commit: gofumpt, both vets, the tests of
  `internal/graph`, `internal/driveops` and `internal/cli` plain; the
  race run and lint are recorded in the merge section.



## Merged as #933

Squash commit `ca8ecac6` on main, 2026-11-16 01:37 PST. The branch was
one commit, `0ada47d2`, on `0104b8f2`. All of this ran on that commit:

- Both vets; `RealRepoIsClean`: four PASS. Start stage: no claim.
- `./internal/graph/` and `./internal/driveops/` with the race
  detector: ok.
- Pre-PR gate (`verify --dod --stage pre-pr`): lint 0 issues, passed.
- Live, test account: `TestE2E_DriveList_JSON` and
  `TestE2E_DriveList_PersonalAccountDoesNotDuplicateCanonicalDrive`
  pass (rerun after the rebase).
- Pre-merge: PR CI (Linux and macOS shards) and review threads passed.
