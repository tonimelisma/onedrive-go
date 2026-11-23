# Definition of Ready: one-shot `sync` exits 1 when work is left

1. PASS **Task intent**: one product-visible behavior. A one-shot `sync` exits 1
   when a drive it ran left a file unsynced (a failed action, work queued to
   retry or left for the next sync, or a condition from an earlier run that
   still keeps a file unsynced), and exits 0 only when `status` would show
   nothing left to do for those drives. The text summary, the JSON `summary`
   line and the exit status take their numbers from one CLI function
   (`leftWork`), fed by a new engine report field `Report.Unresolved` read
   from the snapshot `status` reads.

2. PASS **Non-goals**: no new exit code (left work is 1; 77/78 drive errors
   still win); watch mode (exit status, summary lines, JSON events) untouched;
   what `status` prints is unchanged; the control socket is unchanged; work
   deferred by `--upload-only`/`--download-only`, deletes held for
   `--confirm-deletes` (including `deletes_held`), conflict copies, a paused
   drive beside a drive that ran, and `--dry-run` do not count. No live e2e run.

3. PASS **Governing docs**: read `CLAUDE.md` 1-370 (routing, conventions, DoR);
   `spec/design/cli.md` (Verified By, Exit Status, Console Output, R-6.6.28
   paragraph); `spec/design/sync-engine.md` (GOVERNS, One-Shot Sync, Console
   Reporting, Activity Hook); `spec/design/sync-store.md` 655-701
   (`DriveStatusSnapshot`, read-denied boundaries not in `block_scopes`);
   requirements R-6.6.19, R-6.6.28, R-6.6.31, R-6.6.33 (non-functional.md
   lines 88, 97, 100, 102 today; the brief's line 99 is R-6.6.30 now),
   R-6.8.15 (line 161); `docs/scripting.md`, `docs/running-as-a-service.md`;
   analysis 02 section 8 (lines 742-877), checked against today's code.

4. PASS **Authority owner**: truth of what is left is the sync store
   (`DriveStatusSnapshot`, `internal/sync/store_inspect.go`), counted by one
   store-layer function next to `ProjectStoredConditionGroups`
   (`condition_projection.go`) that applies status's own filters; the
   per-issue-type verdict is one exhaustive switch in `issue_types.go`
   (sync-domain); the engine (`RunOnce`, `engine_run_once.go`) fills
   `Report.Unresolved` at the end of a live run; the CLI owns user-facing
   output and the exit status (`leftWork` in `sync_render.go`, used by
   `syncConsole.Summary` text and JSON and returned to `runOnceResultError`).
   No new mutation, side effect or runtime lifecycle.

5. PASS **Code scope**: `internal/sync` (`api_types.go`, `engine_run_once.go`,
   `store_inspect.go`, `condition_projection.go`, `issue_types.go` and tests),
   `internal/cli` (`sync_render.go`, `sync_console.go`, `sync_console_json.go`,
   `sync_flow.go`, tests; `failure_class_test.go` one table line only),
   `e2e` helpers and the named tests, spec and docs. Must not touch beyond one
   line: `internal/cli/sync_runtime.go`, `internal/cli/failure_class.go`,
   `main.go` (another writer). Never `~/tommi`, the installed client, `/srv`.

6. PASS **Regression plan**: new CLI tests `TestRunSyncCommand_*` (failed,
   left for next sync, still-open conditions, plan-invariant failure exit 1;
   deferred by mode, held deletes, conflict copies, dry run, paused beside a
   synced drive exit 0); console tests extended (summary, JSON `will_retry`,
   `still_open`, `incomplete`); `TestCommandExitCode` case; engine tests
   `TestRunOnce_ReportCountsUnresolvedWork`, `TestRunOnce_DryRunLeavesUnresolvedZero`,
   a store-layer test that the counts apply status's filters, and
   `TestIssueTypes_EveryTypeIsClassifiedForOneShotExit`. Each is run red first
   where the old code can show the fault.

7. PASS **Verification plan**: `go build ./...`, `go vet ./...`,
   `go vet -tags 'e2e e2e_full' ./e2e/...`; `go test -count=1` over
   `./internal/cli/... ./internal/multisync/...`; `internal/sync` with `-run`
   for the added/changed tests plus
   `TestRunOnce|TestIssueType|ListsEvery|Table|Governs|Exhaustive|Registered`;
   `-race` only for the added tests; targeted golangci-lint per package
   (e2e separately with its build tags) into `B2-lint.log`. No `devtool verify`,
   no e2e run; the live tests of the entry are run later by the reviewer.

8. PASS **Data safety**: the change only reads the store after a run (a
   read-only snapshot through the open store) and changes exit status and
   console text. No delete, overwrite, schema or state-DB write path changes.
   A failed snapshot read fails the drive (exit 1) instead of claiming success.

9. PASS **Secret and credential safety**: no tokens, `.env`, CI secrets or
   account names are read or written; tests use `example.com` addresses
   already in the suite; no test account name, drive ID or item ID is written
   anywhere.

10. PASS **Live provider and destructive operations**: none. No live
    OneDrive call, no state reset, no release artifact, no history rewrite, no
    dependency change. e2e code is only compiled (`go vet` with tags).

11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.

12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does a failure of this run also appear in the store snapshot
      at the end (actionable and blocked retry rows), so that counting both
      double-counts it? Current assumption: yes (verified in
      `recordRetryWork`, `applyPermissionFailureEvidence`): `stillOpen` is the
      store's unresolved total beyond this run's own failed and will-retry
      counts, so a file is not counted twice; the exit status is unaffected
      either way. Validation step: CLI tests with overlapping counts.
      Owner affected if wrong: `internal/cli/sync_render.go` (`leftWork`).
    - Question: which issue types leave a file unsynced? Current assumption:
      every type except `deletes_held` (a deliberate hold) and
      `local_interrupted_removal` (a report about a leftover at an internal
      name sync never syncs); unsure rows answer "unsynced" and are marked in
      the report. Validation step: the owner reviews the table. Owner
      affected if wrong: `internal/sync/issue_types.go`.
    - Question: which e2e tests break once a still-open condition exits 1?
      Current assumption: the tests the analysis names plus any that create a
      persistent condition and then sync through the converging helper.
      Validation step: file-by-file audit (table in the report) and live runs
      by the reviewer. Owner affected if wrong: `e2e/`.
