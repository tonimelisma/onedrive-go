# Report: drive list and drive search name the SharePoint sites they could not read

Commit `7dcc4cba` on `fix/drive-list-names-failed-sites`, one commit on `9e5498a5`
(origin/main, #940), not pushed; 10 files, +539/-49, product +162/-37. Written on
`52053271`, rebased onto `9e5498a5` before the heavy checks (#940 touches none of
these files). Installed as the watch client (`onedrive-go version 7dcc4cba`).

## What was wrong (on `52053271`)
- `internal/cli/drive_catalog.go:383-388`, `drive_search.go:138-144`: a failed
  `SiteDrives` was a Debug line and `continue`; nothing on stdout, stderr or in
  `--json` said a site was missing; exit 0.
- `drive_catalog.go:373-378`: a failed site search in `drive list` was a Warn line
  (default level: `Warning: SharePoint site search failed (<reason>)`, no account,
  gone under `--quiet`) and no SharePoint drives; JSON said nothing.
- `drive_search.go:180-182`: with no matching site listable, stdout said
  `No SharePoint sites found matching "<q>".`, which is false.

## What changed
- `drive_catalog.go`: `unlistedSite` (`site_name`, `error` = `plainErrorReason`),
  `failedSiteSearch`, `sharePointGaps`; the three discovery functions return the
  gaps (recorded under the existing mutex, sorted after the workers);
  `printSharePointGapNotes` writes `Note: could not list the libraries of SharePoint
  site <name>: <reason>.`, `Note: could not search the SharePoint sites of
  <account>: <reason>.`, `Note: this listing is incomplete.` via `cc.Statusf`.
- `account_view_snapshot.go`, `drive_list.go`: carry the gaps, print the notes.
- `drive_list_render.go`: `sites_unlisted` (`[]`), `site_search_error`
  (`siteSearchErrorText`); `withEmptyLists` on a pointer receiver (gocritic).
- `drive_search.go`: unlisted sites returned, noted, in `sites_unlisted`; the
  no-results line for that case. `drive_test.go`: printer call sites.
- Spec: R-3.3.14 (`drive-management.md`); `drive-identity.md` Implements line and
  one paragraph (both commands, exit 0, five tests). `docs/scripting.md`: a bullet.

## Tests (`internal/cli/drive_unlisted_sites_test.go`, httptest fake, made-up sites)
Red on `52053271` with only the tests (W88-red.log), green after (W88-green.log):
- `TestDriveList_NamesASiteWhoseLibrariesCouldNotBeListed`,
  `TestDriveSearch_NamesASiteWhoseLibrariesCouldNotBeListed` (text, json): red on
  stderr `""` and `sites_unlisted` with 0 items.
- `TestDriveSearch_NoSiteListedIsNotNoSiteFound`: red on stdout `No SharePoint sites
  found matching "team".` and empty stderr.
- `TestDriveList_SaysAFailedSiteSearchAndListsTheOtherDrives` (text, json): red on
  stderr `""` and `site_search_error` absent.
- `TestDriveListAndSearch_EverySiteAnsweringPrintsWhatItDidBefore`: text subtests
  pass before and after (stdout byte for byte, stderr empty); json subtests red on
  the absent fields.
- `TestSiteSearchErrorText_NamesEachAccountWhoseSearchFailed`: new function; red
  only as a compile failure. No `internal/sync` test, so no stress declaration.

## Checks (final tree `7dcc4cba` unless said)
- Build, `go vet ./...`, tagged vet: pass. `go test ./internal/cli/...`: ok.
  `-race` behind the lock: ok (a first attempt hit the 2-hour background limit
  while queued, before it ran).
- `RealRepoIsClean` 4 PASS; stress declaration test PASS; the 5 cited tests exist.
- `devtool verify lint` on `ed57eeec` (before the receiver fix): 1 issue, gocritic
  `hugeParam`. After it, `golangci-lint run ./internal/cli/...`: `0 issues.`
  (W88-lint.log); only `internal/cli` changed between.
- Live check not run: none is planned (a site refusing its library listing cannot
  be provoked on the test account).

## Deviations
- `drive search` with every matching site unlisted prints `No libraries could be
  listed for the SharePoint sites matching "<q>".`, exit 0 (the old line
  contradicted the notes); not in the brief's list.
- `site_search_error` is `<account>: <reason>`, `; `-joined: `drive list` searches
  once per business account.
- Notes print in `--json` mode too and are silenced by `--quiet`, as `status`'s.
- `docs/scripting.md` bullet: it says partly failed runs exit non-zero.
- Rebased once onto `9e5498a5` so the installed client keeps #940.

## Seen and not fixed (8)
1. `drive_catalog.go:467-473`, `drive_search.go:161-167`: a library with an empty
   site or library name is skipped at Debug; the listing reads complete. Fix: name
   it as an unlisted site, or decide it is not a library.
2. `drive_search.go:57-61`: with several business accounts, one account's failed
   search is only a stderr `Warning:`; `--json` reads complete, and the text uses
   `commandErrorText`, which can carry a request id for an unrecognized answer.
   Fix: a field like `site_search_error`, `plainErrorReason` for the text.
3. `drive_catalog.go:447`: the "SharePoint site search failed" Warn names no
   account. Fix: add the `account` attribute (with a log test).
4. `drive_catalog.go:457-464`, `drive_search.go:151-158`: the site loops keep
   asking after the context ended; by reading, `drive list` returns no error after
   a stop request once discovery has begun, so it exits 0 (not run), and each
   remaining site is now named "context canceled". Fix: stop on `ctx.Err()` and
   return the cancellation (R-6.3.6).
5. `drive_catalog.go:314-317`: `appendAvailableCatalogDrive` drops a `/me/drives`
   entry `driveid.Construct` refuses, without a log line. Fix: Debug line or note.
6. `drive_catalog.go:25-27`: `sharePointSiteUnlimited = 999` is described, and
   R-3.3.4 specified, as removing the cap; it is a cap of 999. Fix: say so, or page
   without one.
7. `status_sync_state.go:70-78`: a shortcut-root snapshot read failure drops that
   drive's shared folders from `status` at Debug; whether the drive's row says it:
   unknown (`status` out of scope). Fix: check, then note it like line 60's case.
8. `drive_catalog.go:390-392`: `newUnlistedSite` names a site by `DisplayName`;
   whether Graph can answer one without it: unknown. Fix: fall back to `Name`.

## Not done
- No live check (above). No other package needs a run: the change is local to
  `internal/cli`, and every e2e decoder of both documents is a non-strict
  `json.Unmarshal` (DoR item v).

## Review fixes

Commit `bd9801a2` (the one commit, amended), on `9e5498a5`: origin/main had not
moved at the fetch on 2026-11-17, so no rebase. Not pushed, not installed
(another branch holds the newest engine work). Against `7dcc4cba`: 9 files,
+608/-109, product +127/-53. Red runs: a `git archive` copy of `7dcc4cba` with
the new test file minus the two tests of new functions (`w88fix-red.log` in the
scratchpad), and two mutations in the worktree, each reverted.

### Blocking
- B1, fixed. `sharePointGapReason` (`drive_catalog.go:443`) gives both gap
  kinds a reason from a closed set of the code's own phrases, chosen by the
  kind of failure: sign-in, read-only window, access denied, not found,
  throttled, server problem, `OneDrive answered HTTP <n>` for any other status,
  a timeout, a failed connection, "the request failed". It never reads
  `Error()`, `Message`, `Code`, `RawBody`, the request id or a URL, so the
  `stripBaseURL` path ends as "the request failed" too. The change adds no log
  line. Verdicts: `TestDriveListAndSearch_AnUnrecognizedAnswerIsNamedByItsStatusAlone`
  (415, a `request-id` header and a two-line HTML body; both commands, both
  forms, and drive list's site search) was red on `7dcc4cba` because stderr and
  the JSON held `graph: HTTP 415 (request-id: …): <html>…`, and is green after.
  `TestSharePointGapReason_SaysTheKindOfFailureInItsOwnWords` (14 cases) tests
  a new function; with its body swapped for `plainErrorReason`, 12 of 14 cases
  were red (request id, body, URL, OS cause), and all are green after.
  `drive-identity.md` now says what the reason holds.
- B2, fixed. Both site loops stop on `ctx.Err()` without recording a gap
  (`drive_catalog.go:528`, `drive_search.go:166`). drive list's failed search
  records nothing and logs no Warn on a stop (`:511`).
  `runDriveListWithContext` (`drive_list.go:15`) and the account loop of
  `runDriveSearchWithContext` (`drive_search.go:59`) return the wrapped
  cancellation before printing anything. main then exits 130 and prints
  nothing more (`root.go:789-795`), as `spec/design/cli.md:497-500` says every
  stopped command does. Verdict:
  `TestDriveListAndSearch_AStopNamesNoSiteAndEndsWithTheCancellation` (4
  subtests) was red on `7dcc4cba` on a nil error. It is green after: the error
  is `context.Canceled`, `commandExitCode` gives 130, and stdout and stderr are
  empty.

### Should-fix
- S1, fixed. `unlistedSite` has an `account` field, the note reads `… SharePoint
  site <name> of <account>: …`, and `sortSharePointGaps` (account, site,
  reason) sorts the gaps in both commands. Verdict: the updated command tests
  were red on `7dcc4cba` (the note named no account, `account` was absent).
  `TestSortSharePointGaps_OrdersByAccountThenSite` was red with the old stable
  name-only comparator. All are green after.
- S2, fixed. When another account's search found results, a failed search
  becomes a `failedSiteSearch`: `printSharePointGapNotes` notes it, replacing
  the `Warning:` line (and with it the request id that line could carry), and
  the drive search document gains `site_search_error` (left out when empty).
  A search with no result still fails. Verdict:
  `TestDriveSearch_NamesAFailedSearchBesideAnotherAccountsResults` (two
  made-up accounts, told apart by their saved sign-in) was red on `7dcc4cba`:
  stderr was `Warning: OneDrive denied access to the item. …` and the JSON had
  no `site_search_error`. It is green after. The design sentence, the commit
  message and the test message (old `:321`) are corrected.
- S3, fixed in the code rather than by an exception: the notes are written with
  `writeWarningf(cc.Status(), …)`, so `--quiet` keeps them. R-3.3.14 says so,
  and `docs/scripting.md:12` stays true. Verdict:
  `TestDriveListAndSearch_QuietKeepsTheNotes` (4 subtests) was red on
  `7dcc4cba` with empty stderr, and is green after.
- S4, fixed. `site_search_error` is `omitempty` in both documents, and R-3.3.14
  and `docs/scripting.md` say "left out". Verdict: the drive list json subtest
  of `…EverySiteAnsweringPrintsWhatItDidBefore` was red on `7dcc4cba` because
  the key was present, and is green after.
- S5, both fixed. A site with no `DisplayName` is named by `site.Name`
  (`newUnlistedSite`). Verdict:
  `TestDriveListAndSearch_NameASiteWithoutADisplayNameByItsURLName` was red on
  `7dcc4cba` on `SharePoint site : graph: forbidden.`, and is green after. The
  comment at `drive_catalog.go:25-26` now says that 999 is a higher cap. It is
  a comment, so it has no test.

### Notes
- N1-N3, N7-N9: findings that the claims hold; nothing to fix. N2's
  `site_search_error: ""` is now left out (S4).
- N4: not fixed. The decision on log levels keeps the Warn line where it is;
  it is no longer written for a stop.
- N5: fixed with B1. Reasons are plain words (`access denied`, not
  `graph: forbidden`).
- N6: the `; `-joined string stays, because its form was decided. drive
  search's `sites_unlisted` is now sorted like drive list's. Verdict:
  `TestDriveListAndSearch_NameTheUnlistedSitesInOneOrder` (sites answered in
  reverse) was red on `7dcc4cba` for drive search (Lantern before Harbor), and
  is green after.

### The 8 seen-and-not-fixed items
- 2, 4, 6 and 8 are fixed here (S2, B2, S5). 1, 3, 5 and 7 are left, as the
  verdicts say. For 6, R-3.3.4 and the restatement in `drive-identity.md:104`
  ("--all to remove the SharePoint site cap") are left to R-3.3.4's own row.
- New (1), in files this change does not touch: `plainErrorReason`
  (`sync_console.go:618`, through `innermostErrorText`) still returns
  `GraphError.Error()` for a status with no sentinel, and at the default level
  the console log handler renders every error attribute through it
  (`console_log.go:254-255`). So drive list's kept Warn line
  (`drive_catalog.go:514`) still prints the request id and body for an
  unclassified failed site search, as before this commit, and so can any Warn
  line and the sync console. `status_live.go:160-162` puts `graphErr.Message`
  (the whole body when the answer has no `error.message`) into status's note.
  The B1 tests use a discard logger, so they do not cover the Warn line.
  Fix: a closed-set fallback in `plainErrorReason` for every console path, as
  its own row.

### Behaviour beyond the findings
- A cancellation that is not a stop the user asked for now ends both commands
  with exit 1 and its error, where it used to exit 0. This is the B2 path, and
  the exit code comes from `commandExitCode`.

### Stress tier
- No new test is in `internal/sync` or `internal/multisync`. The sync step's
  placement rule reads only `internal/sync` (`verify_stress_members.go:37`),
  and the multisync step runs its whole package. So no new test is a member or
  needs a declaration. `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`
  passes.

### Checks (on `bd9801a2`)
- `go build ./...`, `go vet ./...`, tagged vet: pass. `go test ./internal/cli/...`:
  ok. With `-race` behind the lock: ok (`w88fix-race2.log`). `RealRepoIsClean`:
  4 PASS. Stress declaration test: PASS. All 13 test names cited in
  `drive-identity.md` exist. `devtool verify lint` behind the lock: `0 issues`,
  workflow-lint pass (`w88fix-lint2.log`). The first lint, on the previous
  amend, found 3 issues in this change (a global test table, a G101 on a
  constant named "token", and nilerr on the stop return in
  `searchAccountSharePoint`). All three were fixed: the table is now a
  function, the constant is renamed, and the stop returns its error.
- Not run: a live check (none is planned, as before) and a full
  `devtool verify`.


## Shipped

Merged as #942, squash commit `17564167` on `main` (2026-11-17 05:13 PST), installed
from `main` (`onedrive-go --version` prints 17564167). Branch rebased onto `d339dd23`
first; still one commit.

Changes after the second review:
- S1: a failing `drive search` (no result, a failed search) with several business
  accounts first prints the note of each failed search and of each site another
  account could not list (sorted; no incomplete-listing note, as nothing is listed),
  then fails on the first failed search, exit 1 (`printSharePointSourceNotes`, split
  out of `printSharePointGapNotes`). New test
  `TestDriveSearch_FailingStillNamesEverySourceThatDidNotAnswer` (two accounts, both
  forms): red in all 4 subtests with stderr empty, green after.
- S2: new test `TestSharePointDiscovery_AStopIsNoGapAndEndsTheSiteLoop` calls
  `discoverSharePointDrives` (with a Warn-capturing logger) and
  `searchAccountSharePoint` directly, with a stop in the site search and while a
  site is listed. With the three loop-level stop checks removed, the subtest of each
  check failed (gaps returned, a Warn line written, unlisted sites and a nil error);
  checks restored. A fake asserting that a later site is not asked would not catch it:
  the HTTP transport never sends a request on a canceled context.
- S3: the design paragraph and the commit message list the read-only reason.
- Notes fixed: the design paragraph says "which exits 130" instead of "as every
  stopped command does" (N2); the commit message says the text output is unchanged
  and `--json` adds only an empty `sites_unlisted` (N6).
- Gate fix: the first pre-PR gate failed `TestIdentifierKeysFollowTheProductsKeys`
  (`testutil`): the new `site_name` JSON tag is a fourth writer of that masked key;
  the count is now 4.

Left: the phrase "as every command does" in the stop comments of `drive_list.go` and
`drive_search.go` (N2; a comment, found while the gate ran); `--quiet` differs from
`status` (N4, out of scope); the Warn line's raw error (N8) is plan row W99. The
`account` JSON key (an address) is not in the identifier-key masks, as with the
existing `account` keys of the auth and sync JSON; unchanged.

Gates: start passed (nightly green, nothing claimed; rerun after the amend);
pre-PR failed once (above), then passed (lint "0 issues."); pre-merge passed with
every PR check green, the live `e2e` and `integration` jobs included; post-merge
passed (it reran one canceled macOS shard of main's CI). No live run: a site
refusing its library listing cannot be provoked on a test account.
