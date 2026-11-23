# A6 Definition of Ready: one-shot syncs in the e2e suite converge or say why not

Branch `test/e2e-converge`, worktree `~/src/onedrive-go-wt/e2e-converge`, base `main` at `31fd2a59`.

1. PASS **Task intent**: repo-maintenance outcome. Every one-shot `sync` in the e2e suite whose test
   checks only the end state goes through a converge helper (default drive, one named drive, or all
   drives), so a dropped connection or delta lag on one pass no longer fails the test; every sync left as
   one pass carries a comment saying why one pass is the point.
2. PASS **Non-goals**: no product code; the exit code of a pass that left work (B2 owns it); watch-daemon
   tests the list did not analyse; the ten error-tolerating single runs (analysis 01c (a)); fixing any
   product bug a live run exposes (reported instead); timeouts of existing converge sites.
3. PASS **Governing docs**: `CLAUDE.md` 55-236 (test style, E2E tiers); `spec/design/system.md`
   Verification Policy (the e2e harness bullets, "assert eventual sync convergence");
   `spec/reference/live-incidents.md` LI-20260422-05 (2026-11-09 recurrence);
   `spec/reference/live-recurring-families.md` "Upload-Only Live Sync Convergence ...";
   analysis `01b` Item 3 and `01c` lines 1-292, each site re-read in the current code (#867 moved lines).
4. PASS **Authority owner**: the e2e harness: `e2e/e2e_test.go` owns the converge helpers
   (`requireSyncEventuallyConverges*`, `runSyncThroughTransientFailures*`, the new `syncRunner`);
   `e2e/cli_runner_test.go` owns the CLI runners they call; `e2e/shared_items_helpers_test.go` takes the
   shortcut projection loop. The retry-work contract they rely on (a pass may leave retryable work and
   exit 0, R-6.6.28, R-6.8.15) is owned by `internal/sync/engine_results.go` and is not changed. No
   durable state, runtime lifecycle or user-facing output is touched.
5. PASS **Code scope**: `e2e/e2e_test.go`, `e2e/shared_items_helpers_test.go` and the e2e test files
   holding the sites (`sync_full_test.go`, `sync_demoted_full_e2e_test.go`,
   `sync_edge_cases_full_test.go`, `sync_recovery_e2e_test.go`, `sync_kill_mid_transfer_full_e2e_test.go`,
   `sync_safety_full_e2e_test.go`, `sync_hardening_full_e2e_test.go`, `sync_shortcut_full_e2e_test.go`,
   `sync_shortcut_e2e_test.go`, `sync_shared_e2e_test.go`, `cli_commands_e2e_test.go`,
   `drive_list_full_test.go`, `output_validation_e2e_test.go`, `orchestrator_e2e_test.go`,
   `shared_items_e2e_test.go`, `sync_watch_full_test.go`, `sync_e2e_test.go`, `edge_cases_e2e_test.go`);
   docs `spec/design/system.md`, `spec/reference/live-incidents.md`,
   `spec/reference/live-recurring-families.md`. Not touched: `internal/`, `cmd/`, `pkg/`, `.env`,
   `.testdata`, `.dod-pr-comments.json`.
6. PASS **Regression plan**: a live convergence has no failing unit test to write first; the suite's own
   proof is the live runs. The moved sites keep every assertion they had (a site whose proof would change
   stays single-pass and is listed in the report). `go test -short ./e2e/...` keeps the untagged harness
   unit tests green.
7. PASS **Verification plan**: `go vet -tags 'e2e e2e_full' ./e2e` and `go vet -tags e2e ./e2e`
   (compile only); `go test -short ./e2e/...`; gofumpt and goimports on touched files; targeted
   golangci-lint on `./e2e/...`, one lint at a time. Live: at most six tests, one at a time, each
   `go test -count=1 -timeout 20m -tags 'e2e e2e_full' -run '^TestName$' ./e2e`, after checking
   `pgrep -f "e2e.tes[t]"` is empty: the named flake candidates plus a moved site of each variant this
   host can run (drive2, shared-folder and shortcut tests skip here; CI runs them).
8. PASS **Data safety**: test-only change. A converge helper repeats the same sync the test already ran;
   every pass still runs against the test's own isolated root or folder on the test account. No local
   deletes outside temp dirs, no state DB writes by the harness, no overwrites. `<production sync folder>` and the
   production account are never touched.
9. PASS **Secret and credential safety**: `.env` and `.testdata` are read only by the suite itself, never
   printed or staged. Live logs carry the test account name, drive ID and item IDs; none of them is copied
   into the commit, the reports or the final message (test names and pass/fail only).
10. PASS **Live provider and destructive operations**: the live runs create and delete only the tests'
    own `e2e-` folders on the test account, as today; the suite's start-up scrub removes stale `e2e-`
    artifacts on that account. No history rewrite, no release artifacts, no dependency upgrade.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns**:
    - Question: can a converge loop hide a regression where a pass that should finish in one go needs two?
      Current assumption: yes for the moved sites, by design; the 37 single-pass sites (17 idempotence
      checks) keep that visible. Validation step: each moved site is checked against its assertions; a site
      whose proof is the one pass stays single-pass. Owner if wrong: the site's test file.
    - Question: does the unclear `shared_items_e2e_test.go` closing download-only pass prove something on
      its own? Current assumption: it proves R-2.14.2 ("still allowing downloads") under the standing
      write block, so it stays single-pass. Validation step: read the test and the requirement. Owner if
      wrong: `e2e/shared_items_e2e_test.go`.
    - Question: does the shortcut projection loop still work from the fast tier? Current assumption: yes
      once moved to an `e2e`-tagged helper file. Validation step: `go vet -tags e2e ./e2e`; the shortcut
      fixture is absent here, so CI runs it. Owner if wrong: `e2e/shared_items_helpers_test.go`.
