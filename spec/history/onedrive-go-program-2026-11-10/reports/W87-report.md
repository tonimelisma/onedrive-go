# Report: a delta listing fails on a next link it already followed

Commit `0a1a01d4` on `fix/delta-refuses-a-repeated-link`, one commit on
`e248aae5`, not pushed. 6 files, +210/-98.

## What changed
- `internal/graph/delta.go`: `deltaAllPages` asks
  `newAskedPages("delta enumeration", c.maxDeltaPages)` before each request,
  keyed on the path the token resolves to; the hand-kept page counter and cap
  error are gone (`ask` gives the same text and count). The loop resolves
  each token once and fetches through the new `fetchDeltaPath`; initial
  paths are named (`driveDeltaPath`, `folderDeltaPath`). Delta/DeltaFolder
  unchanged in behavior.
- `internal/graph/items_fetch.go`: `askedPages` and `visitChildPages`
  comments no longer say "non-delta".
- `internal/graph/client.go`: `defaultMaxDeltaPages` comment.
- `internal/graph/delta_test.go`: new test and helpers, cap tests rewritten.
- `internal/graph/drives_pagination_test.go`: `everyPagedListingCall`; the
  link-back table also runs the delta listings.
- `spec/design/graph-client.md`: Paged Listings bound sentence and link-back
  paragraph name the delta loop; R-6.7.32 evidence row says "every paged
  listing, delta enumeration included".

## Tests
- `TestPagedListings_ALinkBackToAPageAlreadyAskedForFailsTheListing`: new
  subtests "drive delta", "drive delta from a saved token", "folder delta"
  x self-link / two-page cycle / page 3 back to page 2: pages asked [1],
  [1 2], [1 2 3], no items, no token, the error names both pages. Seen
  failing before the change on all 9: asks [1 1 1 1], [1 2 1 2],
  [1 2 3 2] and "graph: delta enumeration exceeded 4 pages".
- `TestDeltaListings_ALoopBackFailsAsThePageCapDoes` (new): per delta
  listing, the loop error answers `errors.Is` (ErrGone, ErrNotFound,
  ErrMethodNotAllowed, ErrForbidden, context errors) and `errors.As`
  (`*GraphError`) as the cap error does; exact cap text. Seen failing before:
  the loop error was the cap error ("does not contain looped back").
- `TestDeltaAll_MaxPages`, `TestDeltaFolderAll_MaxPages`: ever new links,
  exact cap error, exactly 3 requests, no items or token, Validates
  R-6.7.32. The old forms (a repeating link) were seen failing after the
  change ("looped back from page 2 to page 2" lacks "exceeded").
- `internal/sync`, `internal/multisync`: no test added, renamed or removed.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/graph/...`: ok. Same with `-race`, behind the
  lock: ok (the first attempt was killed by the background limit while
  queued, before it ran; rerun passed).
- `RealRepoIsClean`: 4 PASS. Cited test names checked by grep: all exist.
- Lint `./internal/graph/...` behind the lock: `0 issues.` (W87-lint.log).
- No `internal/sync` run: no file there changed.

## Deviations
- The three shapes are subtests of the existing link-back table, not a new
  delta test function: same assertions, no second table (dupl).
- The observer loop is not changed: `deltaLoopGuard` already refuses a
  repeated next link before asking (R-2.15.2), so it does not need the check.
- At exactly the cap, a next link off the base URL now reports the invalid
  link instead of the cap (the token is resolved before the page is counted).
- No e2e test; no live check possible. The Graph documentation online was
  not read; the recorded quirks name no legitimate repeat.

## Also fixed
- `delta_test.go`: a blank line split the testify imports.
- `client.go`: comment said circular NextLinks run to the cap.

## Not fixed
1. `internal/sync/observer_remote.go:802-806`: a tokenless window whose next
   link is its own first request's URL is asked once more before
   `deltaLoopGuard` notices (`""` is never in its set). Fix: key the guard on
   the path a token resolves to (graph resolves it), seeded with the first.
2. `internal/graph/delta.go:89`: "fetching delta page" logs at Info on every
   page (up to 10,000 per enumeration, every watch poll); per-request logs
   belong at Debug. Fix: Debug. Left because a normal listing had to behave
   as before.
3. `internal/graph/delta_test.go`: most `TestDelta*` tests have no Validates
   line and are not parallel. Fix: add requirement lines and `t.Parallel`.
4. `internal/graph/delta.go:129`: a non-empty token that is not a URL
   silently restarts from the initial path, a full enumeration its caller
   counts as incremental; tested as intended (`TestResolveDeltaToken`) but
   stated in no design doc. Fix: document it, or refuse such a token.

## For the reviewer
- Packages to test: `internal/sync` uses `DeltaFolderAll` through an
  interface only (mocked); no behavior change expected there.


## Review fixes (2026-11-16, applied by the coordinator)

Review verdict: accept; must fix 0, should fix 4, notes 8. The commit
was amended once; it is still one commit on main.

Fixed in this commit:
- A test holds a listing of exactly the page cap's pages:
  `TestDeltaListings_AListingOfExactlyThePageCapCompletes`.
- A test holds that a next link off the base URL is refused before it
  is counted or asked for, below the cap and at it:
  `TestDeltaListings_ANextLinkOffTheBaseURLIsRefusedBeforeItIsAsked`.
- Both tests hold behavior the first commit already had. Each was
  seen failing under a temporary change of the product code (the cap
  compare off by one; a swallowed off-base link error), and the
  change was reverted.
- The line logged before each delta page request is at debug level
  (report item 2). The two lines for a whole enumeration stay at
  info.
- The delta tests run in parallel (report item 3): `t.Parallel()` on
  the 19 that lacked it.
- `spec/design/graph-client.md` cites the two tests and says what a
  page's key is for a token that is not a link.

Checks on the amended tree: `go test ./internal/graph/...` plain and
with `-race`, both vets, the four RealRepoIsClean tests, lint over
`./internal/graph/...`.

Given a home elsewhere (a new plan row for the observer's own loop
and the token rules):
- The observer's page-at-a-time guard keys on raw tokens and does not
  seed a tokenless window's first request (report item 1).
- A looping change list of a shared folder is detected and not
  recovered like an expired cursor.
- A non-empty token that is not a link restarts from the initial
  path without telling the caller (report item 4).
- Keys are exact spellings; an upper-case scheme or a relative next
  link resolves to the initial path.
- A test for a repeat at exactly the cap, for every listing.
- Validates lines for the delta tests where a requirement applies.

## Merged as #937

Squash-merged on 2026-11-16 at 11:26 PST as `ec5a0efe`; installed at 11:28. The pre-PR gate, the pre-merge CI wait and the post-merge stage passed; the post-merge wait reran two macOS shards that the runner had canceled. No live run: the fake Graph server of the unit tests is the proof. The first nightly after the merge is still to be read.
