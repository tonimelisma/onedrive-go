# Definition of Ready: slice listings refuse a repeated link

Worktree `~/src/onedrive-go-wt/slice-listings-refuse-a-repeated-link`,
branch `fix/slice-listings-refuse-a-repeated-link`, HEAD `c0792e98`.

## Inventory of the paged loops of `internal/graph` (base `c0792e98`)

| Loop | File:lines | Callers | Cap check | Repeat check |
|------|-----------|---------|-----------|--------------|
| `visitChildPages` | `items_fetch.go:54-98` (loop 69, cap 70-72, repeat 73-76) | `ListChildrenPages` (286), `ListChildrenByPathPages` (326), `ListRecycleBinItems` (`items_permissions.go:306`); slice forms via `collectChildPages` (`items_fetch.go:102`) | yes, `maxListPages` | yes, `askedPages` map |
| `fetchAllDrives` | `drives_identity.go:78-107` (loop 81, cap 82-84, next link 96-103) | `drivesList` -> `Drives` (`drives_identity.go:60`, inside the 403 quirk retry, which matches only 403 accessDenied, `quirks.go:118-126`), `SiteDrives` (`drives_sites.go:83`) | yes | no |
| `SearchDriveItems` | `drives_shared.go:16-39` (loop 22, cap 23-25, next link from `fetchItemPage` 65-72) | public | yes | no |
| `ListItemPermissions` | `items_permissions.go:249-284` (loop 259, cap 260-262, next link 272-280) | public | yes | no |
| `SearchSites` | `drives_sites.go:19-59` (loop 26 with `len(sites) < limit`, cap 27-29, next link 41-49, truncation 52-54) | public | yes | no |
| `deltaAllPages` | `delta.go:140-194` (loop 154, next link 181-183, cap 185-187) | `DeltaAll` (122), `DeltaFolderAll` (131) | yes, `maxDeltaPages` | no |

No other loop follows a continuation: every `@odata.nextLink` field
(`drives.go:61`, `drives.go:98`, `items.go:146`, `items_permissions.go:43`,
`delta.go:17`, `drives_shared.go:53`) and every `stripBaseURL` call
(`items_fetch.go:434`, `drives_identity.go:99`, `drives_shared.go:68`,
`drives_sites.go:45`, `items_permissions.go:276`, `delta.go:111`) belongs to
one of the six loops above. `ListChildrenRecursive` recurses over
`ListChildren`, so it is the children loop. The row names all six.

## What each does today with a link it already followed

- `visitChildPages`: fails `graph: <listing> looped back from page N to page M`
  before asking again; no extra request; holds one page; the slice forms
  return nil and the error (`TestListChildrenPages_ALinkBackToAPageAlreadyAskedForFailsTheListing`,
  `items_fetch_pages_test.go:248`).
- `fetchAllDrives`, `SearchDriveItems`, `ListItemPermissions`: ask the repeated
  path again on every page, append every page's values (up to
  `defaultMaxListPages` = 10000 pages in memory, `client.go:53`), then fail
  `graph: <listing> exceeded 10000 pages` at page 10001: 10000 requests and
  the cap's error, nil result.
- `SearchSites`: asks again and appends; when the limit is reached first it
  returns the same sites more than once, truncated to the limit, with no
  error; otherwise the cap's error.
- `deltaAllPages`: takes `dp.NextLink` verbatim as the next token
  (`delta.go:181-183`); nothing compares it with an earlier one. A repeated
  link is asked for again until `maxDeltaPages` (10000, `client.go:57`), with
  every page's items held in `allItems`, then `graph: delta enumeration
  exceeded`. `TestDeltaAll_MaxPages` (`delta_test.go:313-336`) serves exactly
  a repeating link and expects the cap's error. So the delta loop can be handed
  a link it already followed, and only its cap bounds it: the same hole.
  By decision (e) it is reported and not changed.

## `TestSearchSites_StopsFollowingAtTheLimit`

Its server links every page to `/sites-more` (`drives_pagination_test.go:225`),
with distinct site IDs per request. Page 1 is the search path, page 2 is
`/sites-more`; after page 2 four sites are in hand, the loop condition
`len(sites) < limit` (3) ends the walk before page 3, which would be the first
repeat. The repeat check runs inside the loop, after that condition, so it
never fires here: the test still proves the walk stops at the limit (two
requests, three sites). Its fake stays as it is.

## Checklist

1. PASS **Task intent**: every non-delta paged listing of the Graph client
   fails on a next link back to a page it already asked for, before asking for
   that page again, through one shared check; `SearchSites` no longer returns a
   site twice.
2. PASS **Non-goals**: the delta loop (hole recorded above, unchanged); name
   lookups that list a folder whole; body-read bounds; `internal/driveops`,
   `internal/cli`; no new exported name; no caller outside `internal/graph`
   changes.
3. PASS **Governing docs**: `spec/design/graph-client.md` "Paged Listings"
   (lines 479-543) and the evidence row at line 93; `spec/requirements/non-functional.md:142`
   (R-6.7.32, already requires a circular continuation to fail); repository
   `CLAUDE.md` lines 55-236.
4. PASS **Authority owner**: `internal/graph` owns how a listing walks its
   pages; the shared check is an unexported type there that owns the page
   count, the cap and the asked paths of one listing. No durable state, no
   user-facing output beyond the returned error.
5. PASS **Code scope**: `internal/graph/items_fetch.go` (shared type,
   children loop), `drives_identity.go`, `drives_shared.go`,
   `items_permissions.go`, `drives_sites.go`; tests in
   `drives_pagination_test.go`; `spec/design/graph-client.md`. Not touched:
   `delta.go` loop, other packages.
6. PASS **Regression plan**: a new table over the slice listings, a self-link
   and a two-page cycle each, asserting the looped-back error, the pages asked
   (none twice) and a nil result; seen failing on the base with the cap's
   error (drives, site drives, item search, permissions) or a repeated result
   (site search). Existing `TestListChildrenPages_ALinkBackToAPageAlreadyAskedForFailsTheListing`,
   `TestPagedListings_ThePageCapCountsThePagesAskedFor` and
   `TestSearchSites_StopsFollowingAtTheLimit` keep passing.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, tagged vet;
   `go test -count=1 ./internal/graph/... ./internal/driveops/... ./internal/cli/...`;
   `-race` over `./internal/graph/...` behind the lock; targeted lint into
   `W76-lint.log`; `RealRepoIsClean`. No e2e, no verify.
8. PASS **Data safety**: a listing that used to fail at the cap now fails
   earlier with another error, and a site search that returned duplicates now
   fails; no caller deletes on a failed listing (they return the error). No
   local or remote writes are involved.
9. PASS **Secret and credential safety**: no token, `.env`, `.testdata` or
   account name is read or written; test values are invented.
10. PASS **Live provider and destructive operations**: none; no live test, no
    history rewrite, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does any listing legitimately hand back a link it already asked for?
      Current assumption: no; Graph continuations carry a fresh `$skiptoken`, and the children loop has refused a repeat since #929 without a report.
      Validation step: the unit tables; live behaviour is not testable here.
      Owner document/code path affected if assumption is wrong: `spec/design/graph-client.md` "Paged Listings", the shared check in `internal/graph/items_fetch.go`.
    - Question: may the shared check also own the page cap?
      Current assumption: yes; every loop's cap error is already `graph: <listing> exceeded N pages`, so one type that counts the pages checks the cap first and the repeat after it, with both texts unchanged (decisions (b), (d)).
      Validation step: `TestPagedListings_ThePageCapCountsThePagesAskedFor` and the children test pass unchanged in what they assert.
      Owner document/code path affected if assumption is wrong: the five loops.
