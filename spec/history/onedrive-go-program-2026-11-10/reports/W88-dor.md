# DoR: drive list and drive search name the SharePoint sites they could not read

Worktree `drive-list-names-failed-sites`, branch `fix/drive-list-names-failed-sites`,
fast-forwarded to `origin/main` `52053271` (the branch had no commits of its own).

## The questions asked first

(i) What each command prints and exits with today.
- `drive list`, `SiteDrives` fails for one site: `internal/cli/drive_catalog.go:383-388`
  logs Debug "failed to list drives for site" and `continue`s. The site's libraries
  are absent from `available`; every other drive prints through
  `printDriveListText` (`drive_list_render.go:57-68`, `:219-250`) or
  `printDriveListJSON` (`:30-44`), which has no field for it. stderr: nothing at
  the default console level (Debug is below Notice, `root.go:941-956`). Exit 0
  (`drive_list.go:7-30` returns only the printer's error). No test covers it: no
  CLI test fakes `/sites/{id}/drives` (`grep -rl SiteDrives internal/cli/*_test.go`
  finds none).
- `drive search`, the same: `drive_search.go:138-144`, same Debug and `continue`.
  Other sites' libraries print (`:174-196`, `:231-275`); `--json` `results` lacks
  the site (`:167-172`). When the failed site was the only match, stdout says
  `No SharePoint sites found matching "<q>".` (`:180-182`), which is false: a site
  was found. Exit 0. No test.
- `drive list`, site search fails: `drive_catalog.go:373-378` logs Warn
  "SharePoint site search failed" and returns no SharePoint entries; the
  account's own drives, shared folders and other accounts still list. JSON has
  no field for it. Exit 0. No test (`TestDriveSearch_FailedSearchFailsInsteadOfReportingNoSites`,
  `cli_workflows_test.go:153`, covers `drive search`'s failure, which fails the
  command, `drive_search.go:123-133`, `:66-68`).

(ii) The Warn line at the default level: it reaches stderr. The default console
level is Notice (`root.go:941-956`), the "auto" handler is the human one
(`root.go:1011-1020`), which prints `Warning: SharePoint site search failed (<reason>)`
with the error reduced by `plainErrorReason` (`console_log.go:93-99`, `:194-199`,
`:248-256`). It names no account and does not say the listing is incomplete; it
is gone under `--quiet` (Error level, `root.go:952-954`), and is a `<4>` line in
the journal. It is a log line, so its wording follows the log level, not the
command's output.

(iii) How `status` says a lookup that failed for one account:
`status_live.go:27-30` `statusLiveNoteFormat = "Note: showing what is known locally for %s: %s.\n"`,
written by `printStatusLiveNotes` (`:185-193`) through `cc.Statusf` (stderr,
silenced by `--quiet`, `format.go:55-66`), in text and `--json` mode alike, before
the document (`status_snapshot.go:211-228`). JSON: no field; the reason
(`statusAccountLiveOverlay.Unanswered`, `status_live.go:45-48`) is not
serialized. Exit 0: `runStatusCommand` returns only the printer's result, and
`TestStatus_PrintsLocalStateWhenOneDriveDoesNotAnswerInTheBudget`
(`status_live_test.go:281-322`) requires no error in both forms. So by decision
(a) both commands exit 0.

(iv) Other loops in `internal/cli` that skip an entry on an error with a Debug line:
- `drive_catalog.go:391-397`, `drive_search.go:147-153` "skipping SharePoint drive
  with invalid ID": `driveid.ConstructSharePoint` fails only on an empty site or
  library name (`internal/driveid/canonical.go:165-180`). The source answered; the
  entry has no name to print or to add by. Not the same fault: listed, not fixed.
- `status_sync_state.go:53-81`: a drive whose shortcut-root snapshot cannot be
  read is left out at Debug. `status` is out of scope; whether the drive's own row
  says it for line 73 was not checked (unknown). Listed.
- `status_live.go:241-261`: a configured drive's storage left out at Debug,
  documented as deliberate (`:213-215`, a shared drive need not show its owner's
  quota). `status`, out of scope. Not the same fault.
- `shared_discovery.go:126-145`: Debug lines, but each sets `degraded` or an auth
  requirement, so it is said. `:247` (hits without remote identity) and `:323`
  (enrichment) are documented in `spec/design/drive-identity.md:106-122` and the
  entry stays visible or is not actionable. Not the same fault.
- The other Debug hits (`drive_catalog.go:208`, `:250`, `:501`,
  `account_view_snapshot.go:83`, `sync_flow.go:253`, `status_snapshot.go:487`,
  `auth_login.go`, `drive_rebind_root.go:59`, `replace_destination.go:118`, and
  command-entry lines in `cp/get/ls/mkdir/mv/put/rm/stat.go`) skip no listing
  entry or report the case another way.

(v) Tests that decode either JSON document, and strictness:
- `internal/cli`: `drive_test.go:465-524` (`printDriveListJSON` into
  `driveListJSONOutput` and into a map whose named keys it checks),
  `:683-735` (`printDriveSearchJSON`), `:1922-2075` (`runDriveListWithContext`);
  `invalid_saved_login_surfaces_test.go:250`, `:278`, `:320-321`. All
  `json.Unmarshal`, not strict.
- `json_contract_test.go:170-176` decodes strictly (`DisallowUnknownFields`) but
  into the command's own type (`:343`, `:394`), so a field added to the type
  passes. `TestJSONOutputTypes_NoListPrintsNull` and
  `TestJSONOutputCalls_PrintDeclaredTypesWithTheirListsFilled` hold every list
  to the type's `withEmptyLists`.
- e2e (read only, not run): `drive_list_e2e_test.go:59-72`, `:175-183`, `:336`,
  `:393`; `drive_list_full_test.go:46-51`, `:67-77`, `:148-159`;
  `shared_items_e2e_test.go:54`, `:85`, `:145`, `:168`;
  `shared_items_helpers_test.go:974-983`; `w7_live_caller_proof_e2e_test.go:158-162`;
  `sync_shortcut_e2e_test.go:33`; `sync_shortcut_full_e2e_test.go:52`;
  `sync_shared_e2e_test.go:25`. All `json.Unmarshal` into local structs, not
  strict. No e2e test runs `drive search`. No documented schema is strict
  (`docs/scripting.md:16-46` describes lists, not a closed schema). No stop.

## Definition of Ready

1. PASS **Task intent**: `drive list` and `drive search` never read as complete
   when a SharePoint source did not answer: each site whose libraries could not
   be listed is named in text (stderr notes) and `--json`, the output says the
   listing is incomplete, a failed site search in `drive list` is said the same
   way, and what answered is still listed. Exit 0.
2. PASS **Non-goals**: site search and paging in `internal/graph`; `drive add`,
   `login`, shared-folder discovery; other commands' log levels; `status`; the
   Debug and Warn log lines themselves (kept as they are, decision (e)); `drive
   search` still fails when its site search fails (decision (d)).
3. PASS **Governing docs**: `spec/requirements/drive-management.md` R-3.3.2,
   R-3.3.9 to R-3.3.11; `spec/requirements/non-functional.md` R-6.6.28, R-6.6.33;
   `spec/design/drive-identity.md` "CLI Drive Command" (governs `drive*.go`);
   `spec/design/cli.md` JSON row; `docs/scripting.md`; repository `CLAUDE.md`.
4. PASS **Authority owner**: `internal/cli` owns the listing and its output;
   `drive_catalog.go` (`drive list` discovery) and `drive_search.go` collect
   what did not answer; the printers in `drive_list.go`, `drive_list_render.go`,
   `drive_search.go` and `json_output.go` own stdout; `cc.Statusf` owns stderr.
   No durable state.
5. PASS **Code scope**: `internal/cli/drive_catalog.go`, `drive_search.go`,
   `drive_list.go`, `drive_list_render.go`, `account_view_snapshot.go` and their
   tests; the spec row and design paragraph; `docs/scripting.md`. Not touched:
   `internal/graph`, `status*.go`.
6. PASS **Regression plan**: new tests through the command with an httptest
   Graph fake: `SiteDrives` fails for one site of two (`drive list` and `drive
   search`, text and JSON), the site search fails in `drive list` (text and
   JSON), and every site answering keeps stdout byte for byte with `[]`/`""`
   fields and no stderr. Each is seen failing before the product change.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, tagged vet;
   `go test -count=1 ./internal/cli/...` plain and `-race` behind the lock;
   `RealRepoIsClean` (devtool); the stress-member declaration test; targeted
   lint of `./internal/cli/...` last, behind the lock. No full verify.
8. PASS **Data safety**: read-only listing; no local or remote writes, no state
   DB access changes.
9. PASS **Secret and credential safety**: tests use made-up sites, accounts and
   token files under `t.TempDir()`; no token, `.testdata` or `.env` is read.
10. PASS **Live provider and destructive operations**: none. No live check is
    planned: a site that refuses its library listing cannot be provoked on the
    test account (decision (g)).
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or nightly
    claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: `drive list` runs one site search per business account; how does
      one string field say two failed searches? Current assumption: the field
      holds each failed search as `<account>: <reason>`, joined by `; `, empty
      when every search answered. Validation step: a test with one failed search
      asserts the exact string. Owner if wrong: `drive_list_render.go`.
    - Question: what `drive search` prints on stdout when no site's libraries
      could be listed. Current assumption: not "No SharePoint sites found"
      (false), but a line saying no libraries could be listed for the matching
      sites, exit 0 by decision (a). Validation step: a test of that case.
      Owner if wrong: `drive_search.go`.
    - Question: the failure's message. Current assumption: `plainErrorReason`,
      the reduction the console log prints for an error at default verbosity
      and `sync --json` puts in its `error` field; it never carries a body or a
      request id. Owner if wrong: the shared reason helper in `drive_catalog.go`.
