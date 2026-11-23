# C4 Definition of Ready: an interrupted removal's condition clears when the leftover is gone

Worktree `~/src/onedrive-go-wt/interrupted-removal-clears`, branch
`fix/interrupted-removal-clears`, based on `main` at `05336a1a`.

1. PASS **Task intent**: a full local scan that no longer finds a leftover
   `.onedrive-go.*.detach` file clears the stored `local_interrupted_removal`
   condition, so `status` stops listing it; a scan that still finds it keeps
   it. The fix covers the class: every issue type a local scan reports is a
   managed type of the local findings batch, and a source-reading test fails
   when a type a scan reports is neither managed nor given a recorded reason
   (and when a managed type is one no scan reports).
   Evidence: `internal/sync/scanner.go:492-502` reports the leftover on every
   scan; `internal/sync/observation_findings.go:10-27` does not list
   `issueLocalInterruptedRemoval`; `observation_reconcile_policy.go:101-133`
   deletes only current rows whose type is in `ManagedIssueTypes`, so the row
   is never deleted.
2. PASS **Non-goals**: planner, executor, remote observation, store schema (no
   generation bump), e2e tests, the trash. The leftover file itself is never
   touched (P-4, R-6.4.9).
3. PASS **Governing docs**: `CLAUDE.md` 55-236; `spec/design/sync-engine.md`
   (GOVERNS `observation_findings.go`; overview paragraph at line 42);
   `spec/design/sync-observation.md` (Local Observation, "Unavailable Is Not
   Absent", the managed-types bullet at lines 680-688; GOVERNS
   `observer_local_handlers.go`); `spec/design/sync-store.md` 366-376
   (reconcile policy); `spec/design/sync-execution.md` 612-614 (the leftover);
   requirements R-6.4.9, R-6.2.14, R-6.2.15, R-2.10.10, R-2.8.21.
4. PASS **Authority owner**: the local scanner (`scanner.go`) is the only
   writer of `local_interrupted_removal` (grep over `internal/`); the engine's
   `observation_findings.go` declares what a local batch manages; the store
   (`observation_reconcile_policy.go`, `store_observation_issues.go`) owns the
   reconcile and the `observation_issues` rows; the CLI's status reads them
   through `ReadDriveStatusSnapshot` and `ProjectStoredConditionGroups`.
5. PASS **Code scope**: `internal/sync/observation_findings.go` and its test;
   `observer_local_handlers.go` (`runSafetyScan` forwarding, refreshed after
   item 12's second question was answered) and `local_observation_batch_test.go`;
   `store_observation_issues_test.go`, `engine_unavailable_not_absent_test.go`,
   `issue_types_test.go` (shared source-reading helper). Refreshed for the
   review follow-up: `observer_local.go` (`TrySendScanFindings`, `skippedCh`
   comment), `engine_watch.go` (`runFallbackScan`), `observation_reconcile_policy.go`
   (managed-type refusal, `managedObservationPathSet`), `store_observation_issues.go`
   and every caller of the two reconcile functions (unused `now` removed,
   `internal/cli/status_test.go` included), `scanner.go` (regular files only),
   `dispatch_order_test.go`. Not touched: planner, executor, remote observer,
   schema.
6. PASS **Regression plan**: store level, a local batch built by
   `localObservationFindingsBatchFromSkippedItems` is reconciled with the
   leftover, again with it (kept, no change reported), then without it
   (cleared, change reported, no status group); engine level, `RunOnce` with
   the leftover, again with it, then after removing it; a source-reading class
   guard over every production file that builds a `skippedItem`. Refreshed:
   the fallback scan records and clears a finding; the plan and the store
   refuse an issue of an unmanaged type naming it; a folder and a FIFO under a
   set-aside name are not reported.
7. PASS **Verification plan**: `go -C <wt> test -race -count=1 -timeout 15m
   -run '<touched tests>' ./internal/sync/`; `go build ./...`; `go vet ./...`;
   targeted `golangci-lint run ./internal/sync/...` (one lint at a time). The
   full DoD verify is the reviewer's.
8. PASS **Data safety**: the change only lets a scan delete an
   `observation_issues` row it re-proves absent. `local_interrupted_removal`
   blocks no truth (`truth_status.go:138-166`, false group), so clearing it
   cannot unblock a delete; the leftover file is never read, moved or deleted.
   No local or remote content changes.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or
   credential-bearing logs involved.
10. PASS **Live provider and destructive operations**: none; unit tests use
    temp directories and the fake remote. No dependency upgrades, no history
    rewrite.
11. ASSUMPTION **Review carryover and nightly**: the reviewer runs the DoD start stage (GitHub's job-log endpoint answered HTTP 502 when the brief was written); the nightly's one failing test is held by another branch; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does any reason require `local_interrupted_removal` to outlive
      the scan that stops reporting it?
      Current assumption: no. The scanner is its only writer and re-reports it
      on every full scan while the file exists; it blocks no truth.
      Validation step: read the store clearing rule and every writer (done:
      `observation_reconcile_policy.go`, grep for the constant).
      Owner document/code path affected if wrong: `observation_findings.go`,
      `sync-observation.md` managed-types bullet.
    - Question: does any production caller store an issue of a type its batch
      does not manage (so refusing such a batch would break it)?
      Current assumption: no.
      Validation step: read every batch constructor and reconcile caller.
      Result: none. Local batches come only from
      `localObservationFindingsBatchFromSkippedItems` (every reported type
      managed, held by the source guard); remote batches carry either no
      issues or `remote_read_denied` with that type managed
      (`engine_primary_root.go`, `engine_watch_maintenance.go`,
      `observer_remote.go` zero batches); dry-run scratch reconciles reuse the
      same batches. Test seeders manage the type they seed.
      Owner document/code path affected if wrong: `observation_reconcile_policy.go`,
      `sync-store.md`.
    - Question: do watch-mode safety scans clear a finding that went away?
      Current assumption: they should; checked at implementation time
      (`observer_local_handlers.go` runSafetyScan forwarding).
      Validation step: read the forward condition and test it. Result: they
      did not when the set was empty (`if len(result.Skipped) > 0`), so the
      last finding stayed until restart; fixed with
      `TestLocalObserver_RunSafetyScanForwardsAnEmptyFindingSet`.
      Owner document/code path affected if wrong: `observer_local_handlers.go`,
      `sync-observation.md`.
