# W55 report: folder listings are read a page at a time

Commit `b5859a4a` on `refactor/paged-folder-listing`, one commit on `af2a6cf5`, not pushed.

## What changed
- internal/graph/items_fetch.go: `ChildrenPageFunc`, `ListChildrenPages`, `ListChildrenByPathPages`; one loop `visitChildPages`; `ListChildren`/`ListChildrenByPath` are `collectChildPages` over it; the loop counts pages asked for.
- internal/graph/items_permissions.go: `ListRecycleBinItems` is the slice form over the same loop; the permissions loop reads the cap field.
- internal/graph/client.go, client_construction.go: `maxListPages` field (default `defaultMaxListPages`), like `maxDeltaPages`.
- internal/graph/drives_identity.go, drives_sites.go: read the cap field. drives_shared.go: also the miscount fixed.
- internal/driveops/tree_download.go: `ChildLister` is the paged form; the walk handles each page before the next; first page creates the folder; a later page failing is that folder's failure; only the root's first page is `TreeRootError`.
- internal/driveops/tree.go: `TreeRootError` comment says the first page.
- internal/driveops/session.go: `MountSession.ListChildrenPages` (path routing once); `ListChildren` built on it.
- internal/cli/replace_destination.go: the `--force` leftover hint walks pages.
- spec/design/drive-transfers.md: download bullet, failures bullet, the "No whole-tree state" bound, evidence. spec/design/graph-client.md: Paged Listings paragraph, the cap, two names.

## Tests added (each seen failing first, except the slice-form guard)
- `TestListChildrenPages_HandsEachPageOnBeforeAskingForTheNext` (by ID and by path): a page goes to the caller before the next is asked for; the server fails the test otherwise.
- `TestListChildrenPages_RetriesAFailedPageAsThatPage`: a 503 on page 2 is retried as page 2; page 1 asked once, handed once.
- `TestListChildrenPages_CancellationBetweenPagesAsksForNoFurtherPage`: the context's error, no page 2.
- `TestListChildrenPages_ACallerThatStopsAsksForNoFurtherPage` (both forms): no further request, no error.
- `TestListChildren_ReturnsEveryPageInListingOrder`: slice form unchanged over three pages (passes before and after, a guard).
- `TestPagedListings_ThePageCapCountsThePagesAskedFor` (8 listings, cap 3): exactly the cap completes, past it fails with no extra request. Failed before for children (seen at the full 10,000 cap) and drive search.
- `TestDownloadTree_WalksAPageBeforeAskingForTheNext`: pages asked root#1, Sub#1, root#2.
- `TestDownloadTree_AFailedLaterPageFailsOnlyItsFolder`: earlier pages download; Sub and the root get one listing failure each; no run error.
- `TestDownloadTree_CancellationAsksForNoFurtherPage`: at most 2xTreeWorkers/pageSize+1 pages asked.
- Before the code changed, the graph and tree tests ran against stand-ins that read every page first; each failed on its log or page order.
- `TestDrives_SelfReferentialNextLinkIsBounded` (10,000 requests, about 8 s) removed; the table covers it.
- Requirements cited: R-1.1.3, R-1.2.1, R-6.7.32, R-6.8.2, R-6.3.6. No requirement states a memory bound for a listing; none was added.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test ./internal/graph/... ./internal/driveops/... ./internal/cli/...`: pass; the same with `-race`: pass.
- untagged `go test ./e2e/`: pass. `RealRepoIsClean`: 4 PASS. Lint of the three packages: 0 issues (W55-lint.log).
- Spec citations checked by hand: all 9 new test names exist; no `internal/sync` file or fake changed, so the sync suite was not run.

## Deviations
- Shape: a callback per page. Delta hands a cursor out only because sync keeps its token; its page loop is one client loop calling a function per page, which is what the listing keeps (loop, cap and next link in the client).
- A later page of the root that fails is now a root `TreeFolderFailed` event, after the earlier pages downloaded, not the command's `listing "<remote>"` error. Today any root page failing was the error and nothing downloaded.
- The cap became a client field read by five loops (four files outside the listing), to test the cap at 3 pages instead of 10,000 requests.

## Also fixed
- items_fetch.go and drives_shared.go: the page cap failed a listing of exactly the cap's pages (counted one past the last).
- The tree fake (`fakeTree`) now pages and fails a chosen page; `listedFolders` keeps its meaning.

## Not fixed (seen)
1. internal/graph/item_create.go:151 `readCreatedFileByName` and internal/driveops/transfer_verify.go:282 `lookupUploadTarget` list a folder whole for a first-match lookup; fix: the paged form, stopping at the first match.
2. internal/driveops/session.go:836 `resolveItemFromMountRoot` lists every folder on the path whole to find one name per segment; fix: `GetChildByName` per segment with the listing as fallback, as `FindExistingChild` does.
3. internal/graph/test_helpers_test.go:51-63 `writeTestResponse(f)` call `require.NoError` inside server handler goroutines (FailNow off the test goroutine); fix: `assert.NoError` there.
4. Unknown, not checked: whether a children nextLink stays valid while the walk spends long in an earlier page's subtree; a failure then is that folder's failure. Fix: a live get of a folder of more than 200 children with large subfolders.

## Unfinished / for the lead
- Nothing unfinished. No other package's tests needed: internal/sync callers keep the unchanged slice form.

## Review fixes

Amended commit `2e283fb2` (one commit on `2afdb376`).

- Item 1, loop guard: `internal/graph/items_fetch.go:66-76` keeps a map of the paths asked for (page number by path, at most the cap's) and fails with `graph: <listing> looped back from page N to page M` before asking again. The cap's error has no sentinel, so neither does this one. Test `TestListChildrenPages_ALinkBackToAPageAlreadyAskedForFailsTheListing` (`items_fetch_pages_test.go:248`), cases "a page that links to itself" and "two pages that link to each other", a visit counting pages, cap lowered to 3; the fixture got `linkBack` (`:44`) and `pageURL` (`:97`). Seen failing without the guard: both cases ended with `exceeded 3 pages` after 3 pages handed on. Spec: new paragraph in `spec/design/graph-client.md:536-543` citing the test. R-6.7.32 lists no tests, so no change there.
- Other paged loops in this commit (`fetchAllDrives`, `SearchDriveItems`, `ListItemPermissions`, `SearchSites`): they hand nothing on before the end, so the hole is not there. The first three gather until the cap, then fail whole: as on the base, a cycle costs the cap's requests and up to the cap's pages in memory. `SearchSites` stops at the caller's limit, so a repeating link can put the same site in the result more than once, up to the limit (`TestSearchSites_StopsFollowingAtTheLimit` serves exactly such a repeating link). This has not changed from the base. Left as is; candidate row: one shared repeat check for the slice loops, with a de-duplicated `SearchSites` result.
- Item 2: comment `items_fetch.go:45-53` and spec `graph-client.md:519-523` now say "a page asked for at the drive root's children collection (the first page, and a later one whose link keeps that form)".
- Item 3 (decided): `spec/design/drive-transfers.md:160-165`: nothing downloaded or created holds for the root's first page; a later root page is a folder failure like any other, exit 1 whatever its error, sign-in included, unless the context ended. The commit message says the same. The same bullet's broken reflow ("reported every") was repaired as well.
- Item 4: `internal/graph/test_helpers_test.go:58,67` use `assert.NoError`, with a comment that says why.
- Note 6: `drives_pagination_test.go:164` asserts that a constructed client holds `defaultMaxListPages`.
- Note 7: `TestDownloadTree_CancellationAtALaterPageCutsItsFolderShort` (`internal/driveops/tree_download_test.go:502`) uses the existing `onList` and `failPage` hooks, with no change to the fake. It cancels while the root's page 1 is walked into Sub, and page 2 then fails with `context.Canceled`. The root is reported cut short, and the run ends with the cancellation. Checked that the test reaches the branch: with the `ctx.Err()` branch bypassed for the root, it failed with the "listing the OneDrive folder" text. Reverted. The test is cited at `drive-transfers.md:730`.
- Note 9: `drive-transfers.md:89-95` reflowed. Note 10: `graph-client.md:516-519`: stopping early is the one exception to following nextLink to completion, and only a recursive `get` uses it (when it cannot create the folder, or when the context ends the walk).
- Note 8: nothing.
- Checks: `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` clean; `go test -count=1 -timeout 20m ./internal/graph/ ./internal/driveops/ ./internal/cli/` ok (behind the lock); `-run RealRepoIsClean ./internal/devtool/` 4 PASS; gofumpt/goimports clean. No lint run (as instructed).


## Merged as #929

- Merge commit `9798e318`, 2026-11-15 21:33 PST. Pre-PR, pre-merge and post-merge gates passed; installed 21:48.
- No nightly failure was claimed by this branch.
