# Definition of Ready: a delta listing fails on a next link it already followed

Worktree `~/src/onedrive-go-wt/delta-refuses-a-repeated-link`,
branch `fix/delta-refuses-a-repeated-link`, HEAD `e248aae5` (checked; only
`.testdata` untracked).

## Answers asked for before any edit

### (i) Every loop in the product that follows delta next links

1. `internal/graph/delta.go:140` `deltaAllPages`, shared by `DeltaAll`
   (`:122`) and `DeltaFolderAll` (`:131`). Stops today on a delta link
   (`:169`), on a page with neither link (`:192`), on a fetch error (`:156`),
   and otherwise only at the cap: `page++` then `page > c.maxDeltaPages`
   (`:183-187`, default 10000, `client.go:58`), checked before the next
   request. Nothing records the links followed: a next link equal to one
   already asked for is asked for again. Holds in memory until then:
   `allItems`, every page's items (`:160`), so a repeating link costs up to
   10000 requests and 10000 pages of items before
   `graph: delta enumeration exceeded 10000 pages` (plain `fmt.Errorf`, no
   `%w`, `:186`).
   - Production callers: only `internal/sync/engine_mount_root.go:58` and
     `:69` (`DeltaFolderAll`); `DeltaAll` has no production caller (tests
     only: `delta_test.go`, `quirks_test.go:672`). What the mount-root caller
     does with the cap error: `:60` branches on `graph.ErrGone` (no match),
     `:82` `shouldFallbackMountRootDelta` (`:155-156`, `ErrMethodNotAllowed`
     or `ErrNotFound`: no match), so `:83` returns
     `sync: mount-root delta: %w`; `engine_primary_root.go:230`
     `rootReadFailureIssueType` (`observation_findings.go:126-134`,
     `ErrNotFound`/`ErrForbidden`: no match), so the batch and the error go
     up. One-shot: `engine_current_plan.go:208-210` -> `:20-22` ->
     `engine_run_once.go:348-350` -> `:92-93`: the run returns the error
     (shown, not retried, no scope, no issue row, no outage count). Dry run:
     `engine_current_plan.go:414-416`, same. Watch bootstrap: `:70-72`
     (`checking for changes: %w`), `engine_watch.go:277-279`, watch start
     fails. Watch poll of a mount root: `engine_mount_root.go:331-333`
     `handleMountRootPollError` (`:392-409`): backoff, one Warn per outage
     ("Can't check the shared folder for changes; retrying"), retried.
     Periodic full refresh: `engine_watch_maintenance.go:176-185`: Warn
     "Full check of OneDrive for changes failed; will retry", retried when
     idle.
2. `internal/sync/observer_remote.go:241` `fullDelta`, the page-at-a-time
   loop over `fetcher.Delta` (`:988`, retry policy `:985-1007`,
   `retry.DeltaPagePolicy`). Stops today on a delta link (`:264`), an error
   (`:243`), `deltaLoopGuard` (`:290`, type `:793-834`) and the range counter
   `maxDeltaPaginationGuard` (10000, `:27`, `:241`, error `:300-301`).
   `deltaLoopGuard.next` refuses a next link already in its token set before
   that link is asked for (`:812-815`; the saved token is seeded, `:804-806`)
   and a non-empty page whose item IDs repeat an earlier page (`:817-831`).
   Holds until then: `window` (every page's raw items, `:249`), `inflight`
   and the guard's sets. Caller: the loop error is `newCursorUnusableError`
   (an `errDeltaExpired`): one-shot full refresh
   (`engine_primary_root.go:142-147`), watch full refresh spaced by
   `expiredDeltaRecoveryMinInterval` (`observer_remote.go:362-372`). Its cap
   error is plain: one-shot returns it, watch backs off
   (`handleWatchPollError`, `:373-383`). **This loop does not have the
   hole**: it already refuses the repeat before the request
   (`TestFullDelta_RepeatedNextLinkIsCursorUnusable`: tokens asked
   `["saved-token", "page-2"]`, no third), so it is not changed and gets no
   test (decision (b) applies only if it needs the check). One shape it
   notices one request late: a tokenless window whose next link is the URL
   of its own first request (token `""` is not in the set); keying on the
   resolved request path would need graph's path resolution in `sync`. Listed
   under "Not fixed" in the report.
3. No other production caller of `Delta`, `DeltaFolder`, `DeltaAll` or
   `DeltaFolderAll` (grep over `internal`, `cmd`, `e2e`, `testutil`).

### (ii) What the recorded quirks and the code comments say

- `spec/reference/graph-api-quirks.md`, every line on delta (grep `-i
  delta`, read `:127-160`, `:183-250`, `:1094-1100`): **nothing recorded**
  about a next link that equals the one just asked, or about an empty page
  with a next link. `spec/reference/onedrive-sync-behavior.md`: no line on
  next links.
- Code comments: `observer_remote.go:793-796` "deltaLoopGuard notices a
  change list that leads back on itself: a nextLink already fetched in this
  window ... Followed, such a list ran to the 10,000-page guard with every
  item buffered in memory (R-2.15.2)" (a repeat is a fault);
  `observer_remote_cursor_test.go:96` "Empty pages are not a loop: Graph
  sends them while it catches up." (an empty page with a **new** next link,
  which continues); `client.go:56-57` "A buggy API or circular NextLinks
  could loop forever without this guard."; `delta_test.go:317` "Always
  return a NextLink, never a DeltaLink — simulates infinite loop."
- R-2.15.2 (`spec/requirements/sync.md:234`) treats "a change list whose
  nextLink leads back to a page already fetched in the window" as a cursor
  the client cannot continue. Nothing names a repeat OneDrive sends
  legitimately, so the stop condition does not apply. The Graph
  documentation online was not read in this run.

### (iii) What `askedPages` keys a page on

`items_fetch.go:475-502`: `pageOf map[string]int`, keyed on the exact
`apiPath` string the listing is about to ask for (a relative path and
query, after `stripBaseURL`), at most `maxPages` entries; `ask` checks the
cap first (`:491-493`, `graph: <listing> exceeded N pages`), then the
repeat (`:495-497`, `graph: <listing> looped back from page N to page M`).
A delta next link is a full URL with a token; `resolveDeltaToken`
(`delta.go:106-117`) turns it into exactly that form through `stripBaseURL`,
and the first page's key is the built path (or a saved token's stripped
path). So it fits as is when the delta loop asks with the resolved path, not
the raw token: keyed on the token, a link back to a tokenless first request
would be asked once more. With listing `"delta enumeration"` and
`c.maxDeltaPages`, `ask`'s cap error text is byte-identical to today's
(`graph: delta enumeration exceeded %d pages`) and its count is the same:
pages 1..N are asked, page N+1 fails before the request.

### (iv) Every test and caller matching the cap error's text or type

- Text: `delta_test.go:334` and `:581` (`"exceeded"`, `:335` `"3"`); both
  serve a repeating link today and would read the repeat error after the
  change, so both get distinct links. `drives_pagination_test.go:184`
  (`"exceeded"`, non-delta). No product code matches `exceeded`, `pages` or
  `looped back` (grep over `internal/sync`, `multisync`, `cli`, `errclass`).
- Type: none. The cap error wraps nothing; every classifying caller above
  branches on sentinels it does not carry. The repeat error from `ask` is
  also a plain `fmt.Errorf` without `%w`, so it is classified the same; a
  graph test holds that for the sentinels those callers branch on.

## Checklist

1. PASS **Task intent**: no delta listing asks twice for a next link it
   already followed in the same listing; the repeat fails the listing before
   the request with `graph: delta enumeration looped back from page N to page
   M`, the page cap stays the second bound, and a listing that ends normally
   behaves as before.
2. PASS **Non-goals**: the non-delta listings; how a delta token is stored,
   resolved or reset (`resolveDeltaToken` keeps its behavior); the observer
   loop (already guarded, (i) 2); planner, executor, `internal/cli`, retry
   numbers; no stored field, store generation, issue type, e2e test.
3. PASS **Governing docs**: `spec/design/graph-client.md` (Paged Listings
   `:479-553`, client guards `:237-246`), `spec/reference/graph-api-quirks.md`
   (delta lines), R-6.7.32 (`spec/requirements/non-functional.md:142`),
   R-2.15.2, `spec/design/sync-observation.md:172-180`, repository
   `CLAUDE.md` `:1-290`, SUBAGENT-RULES.md, builder additions.
4. PASS **Authority owner**: `internal/graph` owns the page loop, its bound
   and its error; `askedPages` stays the one check. The engine owns what a
   failed observation does, unchanged.
5. PASS **Code scope**: `internal/graph/delta.go`, `items_fetch.go`
   (comments), `client.go` (one comment), `delta_test.go`,
   `spec/design/graph-client.md`. Untouched: `internal/sync` (no change
   needed), `internal/cli`, other branches' files.
6. PASS **Regression plan**: new tests in `delta_test.go`: the three shapes
   (self-link, two-page cycle, page 3 back to page 2) for `DeltaAll`,
   `DeltaAll` from a saved token and `DeltaFolderAll`, asserting the pages
   asked and that no items or token come back; a test that the repeat error
   is classified as the cap error is; `TestDeltaAll_MaxPages` and
   `TestDeltaFolderAll_MaxPages` serve distinct links, count the requests and
   get `// Validates: R-6.7.32`.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, vet with
   `e2e,e2e_full,integration,stress`; `go test -count=1 ./internal/graph/...`
   and with `-race` behind the lock; `RealRepoIsClean` (4 PASS); the
   `ListsEvery|Table|Governs|Exhaustive|Registered` tests of `internal/graph`;
   lint of `./internal/graph/...` last, behind the lock. No `internal/sync`
   runs (no file there changes).
8. PASS **Data safety**: the change only ends a listing earlier with an
   error; no token is saved from a failed listing (unchanged), nothing is
   deleted or written.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata` or
   account names involved; test IDs and links are invented.
10. PASS **Live provider and destructive operations**: none; no live check
    is possible (OneDrive does not send such a link on request).
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD
    start stage has already been run in this worktree; no carryover threads
    or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does OneDrive ever legitimately send a next link equal to one
      already asked in the same listing? Current assumption: no (nothing
      recorded; R-2.15.2 already treats it as a fault for the observer).
      Validation step: none live is possible; a recorded quirk would reopen
      it. Owner if wrong: `internal/graph/delta.go`,
      `spec/reference/graph-api-quirks.md`.
    - Question: is keying on the resolved request path right for a token
      that is not a URL? Current assumption: yes; such a token resolves to
      the built path, as it is asked for today. Validation step:
      `TestResolveDeltaToken` unchanged and green. Owner if wrong:
      `delta.go` `resolveDeltaToken`.
