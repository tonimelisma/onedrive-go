# A13 Definition of Ready: e2e buckets derived from test names

Base: `origin/main` `9e5498a5` (#940), worktree
`~/src/onedrive-go-wt/e2e-family-buckets`, branch `test/e2e-family-buckets`
(created with `devtool worktree add`, 2026-11-17).

## Facts on the base

(i) Sets, from a scratch program that asks `go/build` `Context.MatchFile` with no tags,
`{e2e}` and `{e2e, e2e_full}` (`scratchpad/a13/discover/main.go`), top-level tests by
go test's name rule from the parsed AST:

- full tier (built with both tags, not with `e2e` alone): 24 files, 136 tests, all `TestE2E_`.
  Buckets today: `full-parallel-misc` 57, `full-serial-sync` 33,
  `full-serial-watch-shared` 45, plus `TestE2E_FixturePreflight_Full` = 136, as the brief says.
- fast tier (built with `e2e`, not without tags): 21 files, eight `TestE2E_` tests
  (`AuthPreflight_Fast`, `FileOpsSmokeCRUD`, `FixturePreflight_Fast`,
  `ShortcutSmoke_DownloadOnlyProjectsChildMount`, `SyncWatch_WebsocketDisabledLongPollRegression`,
  `Sync_DownloadOnly`, `Sync_UploadOnly`, `TrackedFilesNameNoTestAccount`), 20 helper tests and `TestMain`.
- untagged: 17 files, 54 helper tests (`go test -list . ./e2e/` agrees: 54).
- no `_test.go` file of `e2e/` is built by none of the three tag sets.
- Difference from the brief: it counts 21 fast helper tests and 63 untagged ones. Those
  counts include `func Test...` lines inside raw-string fixtures (for example
  `e2e/printed_value_rule_test.go:882` `func TestPlantedLive` sits in a string literal).
  The sets this change relies on (136 full, eight fast `TestE2E_`) are equal. Not a stop.
- Families today (text after `TestE2E_` up to the next `_`): CLI 1, Cp 5, DriveList 9,
  FixturePreflight 1, InternalBaselineVerification 1, Logout 1, Mv 6, Orchestrator 8,
  Pause 2, Resume 2, Shared 8, SharedFolder 3, Shortcut 10, Status 12, Sync 35, SyncFull 3,
  SyncWatch 18, no family 11. `Sync` sits in misc (5) and watch-shared (1) besides
  serial-sync; `Shared` in misc (3) and watch-shared (5); no-family in misc (10) and serial-sync (1).
- After the renames: CLI 1, Cp 5, DriveList 9, Edge 4, FileOps 4, FixturePreflight 1,
  InternalBaselineVerification 1, Logout 1, Mv 6, Orchestrator 9, Pause 2, RecycleBin 2,
  Resume 2, Shared 5, SharedFolder 3, SharedRead 3, Shortcut 10, Status 12, Sync 30,
  SyncFull 3, SyncIsolated 5, SyncWatch 18. The table of decision (b) places exactly
  today's three lists (renamed), 57/33/45 (`scratchpad/a13/samebuckets.py`: "same" x3).
- The 20, old name, file, bucket today:
  `Sync_QuietMode` output_validation_e2e_test.go misc; `Sync_MtimeOnlyChange`,
  `Sync_TransferWorkersConfig` sync_edge_cases_full_test.go misc;
  `Sync_IncrementalDeltaToken`, `Sync_DriveRemovePurgeResetsState`
  sync_recovery_e2e_test.go misc; `Sync_MultiDriveReport` output_validation_e2e_test.go
  watch-shared; `Shared_FileDiscoveryAndSelectorReadCommands`,
  `Shared_FileDiscoveryRejectsDriveAdd` shared_items_fast_e2e_test.go misc;
  `Shared_JSON_RecipientListingUsesLiveAccountCatalog` w7_live_caller_proof_e2e_test.go
  misc; `EdgeCases` sync_full_test.go serial-sync; `RoundTrip`, `ErrorCases`,
  `JSONOutput`, `QuietFlag` fileops_full_e2e_test.go misc; `ZeroByteFileSync`,
  `UnicodeFilenameRoundtrip`, `InvalidFilenameRejection`, `RapidFileChurn`
  edge_cases_e2e_test.go misc; `RecycleBinRoundtrip`, `RecycleBinEmpty`
  cli_commands_e2e_test.go misc (all prefixed `TestE2E_`).

(ii) `fullE2EBuckets()` is called by `runE2EFull` (`verify_e2e.go:760`) and by
`runLiveVerification` for the fast-preflight not-run record (`verify.go:596`, not 600).
In e2e-full the first live call is the fast lane's auth preflight (`runE2E` inside
`runLiveVerification`), so discovery runs there first, as a step of its own, when the
plan runs e2e-full; its buckets are passed to the fast-preflight not-run record and to
`runE2EFull`, which uses them for the full-preflight not-run record, the
cancellation not-run record and every bucket run. A failed preflight records the same
bucket names, kinds, parallel values and deadlines as before; only the run pattern's
list is now sorted and carries the new names. A discovery failure records its step as
failed and the three buckets as not run with the reason, so the summary still lists
every bucket (R-6.10.26).

(iii) `MatchFile(dir, name)` (go 1.27 `go/build/build.go:1408`) returns false for a
name starting with `_` or `.`, applies the `_GOOS`/`_GOARCH` file-name rule, opens the
file with `os.Open` (no `OpenFile` hook set), reads the header and evaluates the
`//go:build` line (or `+build` lines) with `matchTag`: GOOS, GOARCH, compiler,
`unix`, cgo when enabled, `BuildTags`, `ToolTags`, `ReleaseTags`. So the temp-dir tests
work with real files in `t.TempDir()`, no change outside `internal/devtool`; a
host-GOOS constraint is matched, another GOOS is not.

(iv) `grep -rlw` of the 20 old names over the worktree (excluding `.git`): 18 files,
exactly the brief's list: devtool `verify_e2e.go`, `verify_e2e_full_test.go`,
`verify_runner_test.go`, `nightly_test.go`; `internal/config/drive.go`,
`internal/cli/status_test.go`; e2e `cli_commands_e2e_test.go`, `edge_cases_e2e_test.go`,
`fileops_full_e2e_test.go`, `output_validation_e2e_test.go`, `shared_items_fast_e2e_test.go`,
`sync_edge_cases_full_test.go`, `sync_full_test.go`, `sync_recovery_e2e_test.go`,
`w7_live_caller_proof_e2e_test.go`; `spec/design/cli.md` (2), `spec/reference/graph-api-quirks.md`
(4), `spec/reference/live-incidents.md` (39). No workflow file.

(v) A failing test's nightly key is `nightlyFailedTestKey` (`nightly_verdict.go:354`):
the package as go test names it, a dot, the top-level name (`e2e.TestE2E_Sync_IncrementalDeltaToken`);
the claim ref is `nightlyClaimRefName(key)`. After the rename a failure is keyed by the
new name; a claim on an old key is never matched again, and `pruneNightlyClaims`
deletes it only when its lease expires unrenewed (claimed) or when a run with its fix
passed the jobs (fixed). So the rename leaves an open claim on an old name to lapse
or to be released by its holder, and a failure of the renamed test is a new key.

(vi) R-6.10 ids on the base run to R-6.10.27 (`spec/requirements/non-functional.md:214`);
R-6.10.28 is free here and in the six other worktrees.

Decision (h): the constants keep their values and none is one of the 20
(`verify.go:21-31`). The fifty-times stress member rule (#939) covers `internal/sync`
tests only; this change adds no test there.

## Checklist

1. PASS **Task intent**: a full-tier live test's name alone places it in its nightly
   bucket; a name the family table cannot place stops `verify e2e-full` before its first
   live call; the fast tier's live tests equal one allow-list.
2. PASS **Non-goals**: bucket deadlines, `-parallel` values, the number of buckets, the
   classified quirk rerun, the fast tier's run command, moving a test between buckets,
   rewriting incident entries, `e2e/e2e_test.go` helpers, `.golangci.yml`, workflow files,
   test bodies, build lines and file names.
3. PASS **Governing docs**: `spec/design/system.md` (GOVERNS `internal/devtool/*.go`;
   the e2e bullets at 524-526, the devtool split at 401), `spec/requirements/non-functional.md`
   R-6.10, `spec/reference/live-incidents.md` "Historical test names",
   `spec/reference/graph-api-quirks.md`, `spec/design/cli.md`, repo `CLAUDE.md` e2e paragraph (226).
4. PASS **Authority owner**: `internal/devtool` owns the verifier's tiers and buckets;
   the family table and discovery live there, in one new file `verify_e2e_tiers.go`.
   No product runtime state, no user-facing CLI output changes.
5. PASS **Code scope**: `internal/devtool` (new `verify_e2e_tiers.go` and its test,
   `verify_e2e.go`, `verify.go`, `verify_summary.go`, the fake-runner tests), the nine e2e
   files (function names only), one comment in `internal/config/drive.go` and in
   `internal/cli/status_test.go`, the four spec files and `CLAUDE.md`. Not touched: workflows,
   `.golangci.yml`, any other e2e code.
6. PASS **Regression plan**: the seven tests of the brief's step 2, each seen failing
   or not compiling first; the existing e2e-full fake-runner tests keep passing with
   buckets read from a temp `e2e/` tree.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, tagged vet; devtool tests
   with and without `-race` behind the lock; `go test ./e2e/ ./testutil/...` untagged;
   gofumpt; targeted golangci-lint (also `--build-tags e2e,e2e_full` over `./e2e/`) last,
   behind the lock; `RealRepoIsClean` (4 PASS). No live run, no `devtool verify`.
8. PASS **Data safety**: no product code path changes; discovery only reads files of `e2e/`.
9. PASS **Secret and credential safety**: no token, `.env`, `.testdata` or account name
   is read or written; tests use invented names; `testutil/testenv.go` 25-35 not read.
10. PASS **Live provider and destructive operations**: none; no e2e test is run.
11. ASSUMPTION **Review carryover and nightly**: the start stage has not been run in this
    worktree (the shipping step runs it); no carryover threads or nightly claims were handed
    to this increment. The plan entry records one claim ref on 2026-11-16 naming
    `e2e.TestE2E_Sync_IncrementalDeltaToken`; per (v) it lapses or is released, and the
    shipping step lists the refs.
12. ASSUMPTION **Unknowns**:
    - Question: does another unmerged branch add full-tier e2e tests? Current assumption:
      possibly. Validation step: discovery places any test whose family is in the table with
      no further edit, and names any other one before the first live call. Owner if wrong:
      `internal/devtool/verify_e2e_tiers.go`.
    - Question: do other devtool tests run e2e-full with a temp repository root? Current
      assumption: yes (the fake-runner tests); they get a temp `e2e/` tree with invented
      names. Validation step: the devtool package test run. Owner: the devtool test helpers.

## Refresh during the work

- Scope: discovery reads every package the pattern `./e2e/...` names (the buckets run that pattern), not only `e2e/` itself; a new test covers it.
- Scope: discovery runs as a step of its own, `e2e-full-discovery`, in the closed step set (runs no go test), so a failure prints a marker line.
