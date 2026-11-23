# Definition of Ready: a stale precondition is superseded whatever status it carries

Worktree `~/src/onedrive-go-wt/stale-precondition-first`, branch
`fix/stale-precondition-first`, base `main` at `34fb1a66`.

1. PASS **Task intent**: an action whose completion error is a stale precondition
   (`errActionPreconditionChanged`) is classified `superseded` and replanned in
   the same run, whatever Graph answer travels in the same error chain (a 409
   create collision resolved as stale, a 412 on a conditional overwrite, any
   other status). Today `classifyResult` (engine_result_classify.go:67) runs
   `classifyHTTPResult` before the stale check (:134), so a stale 409 becomes
   anonymous actionable retry_work on the durable curve and a stale 412 becomes
   `transient_conflict` retry_work.
2. PASS **Non-goals**: pending-create intent rows nothing clears (chaos seeds 9
   and 11); `engine_watch.go`, `observation_reconcile_policy.go`, `scanner.go`,
   `observer_local.go` and the observation store (an open branch owns them);
   the folder-create conflict join in `executor.go` and the driveops
   upload/lookup joins (reported under "Not fixed"); no change to the
   server-answer table itself.
3. PASS **Governing docs**: repo `CLAUDE.md` (55-236, DoR 252-278);
   `spec/design/sync-engine.md` (governs `engine_result_classify.go`,
   `engine_runtime_permissions.go`; superseded path 727-742, permission split
   168-200); `spec/design/sync-execution.md` (governs `executor_transfer.go`;
   create-409 resolution 193-215, live preconditions 295-372);
   `spec/design/error-model.md` (boundary rules 124-136: "the engine maps it to
   superseded"; "transient failures while trying to read live precondition
   truth are not wrapped with this sentinel"); requirements R-2.8.7
   (superseded), R-2.1.7 (replan in the same run), R-5.6.2 (412 on overwrite is
   a stale precondition and replans), R-5.6.14/R-5.6.15 (create 409).
4. PASS **Authority owner**: classification of a completion is owned by
   `classifyResult` (engine_result_classify.go); the runtime decision effects
   (retry_work, scopes, permission probes, supersede streaks) by `engineFlow`
   in `engine_runtime_completion.go` / `engine_runtime_permissions.go`; the
   stale verdict itself by the executor (`uploadTransferError`,
   `resolveCreateConflict` in executor_transfer.go); durable retry_work by the
   sync store through the engine. No new owner.
5. PASS **Code scope**: `internal/sync/engine_result_classify.go` (order),
   `internal/sync/engine_runtime_permissions.go` (a superseded decision never
   reaches the permission probes, the second reader of the server answer ahead
   of the decision), `internal/sync/executor_transfer.go` (resolution failures
   after a 409 are not stale; remote stale answers carry remote-write
   capability; duplicated doc comment), tests in `internal/sync`, docs
   `sync-execution.md`, `sync-engine.md`, `error-model.md`. Not touched: the
   files listed in item 2.
6. PASS **Regression plan**: flip
   `TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt`
   (superseded, replanned in the same run, no anonymous retry_work row, the
   collision found in the same run); classifier table rows stale-with-409,
   stale-with-409-resourceModified, stale-with-412 and stale with other
   statuses; a new `TestRunOnce_` for the 412 on a conditional overwrite
   (listed in `runOnceScenarios()`); an engine-flow test that a stale
   completion carrying a 403 or a local permission denial is superseded, not
   probed; an executor test that a listing failure after a 409 is the
   listing's own failure, not a stale precondition. Each seen failing first.
7. PASS **Verification plan**: targeted `go test -race -count=1 -run ...` on the
   new and touched tests; the `ListsEvery|Table|Governs|Exhaustive|Registered`
   tests; `go build ./...`, `go vet ./...`; full non-race
   `go test -count=1 -timeout 60m ./internal/sync/...`; golangci-lint of
   `./internal/sync/...` with `0 issues.` after the last change (one lint at a
   time). No devtool verify, no e2e (the reviewer runs those).
8. PASS **Data safety**: superseding writes nothing; the replan decides from
   fresh truth, which is the safer path for a 412 (a remote edit landed) and a
   409 (the name is taken). Repeated supersedes stay bounded by the existing
   park at three in a row (`supersedeParkThreshold`, 30 s doubling). The
   overwrite stays conditional on If-Match, so no remote edit is replaced; the
   412 engine test asserts both edits survive. No schema or store change.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or
   credential-bearing logs involved; tests use fakes only.
10. PASS **Live provider and destructive operations**: none. No live OneDrive
    call, no state reset, no dependency upgrade, no history rewrite, no push.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does any error class rely on the HTTP branch running before the
      stale check? Current assumption: no. Evidence: every producer of
      `errActionPreconditionChanged` in `internal/sync` drops its cause
      (`stalePreconditionError`, `remoteItemGoneError`, the admission and
      worker-start `"%w: %s"` forms, `executor_delete.go` hash/size forms) or
      wraps a local cause (`synctree.ErrDetachRaced`, executor_delete.go:217),
      except `uploadTransferError` (executor_transfer.go:372), whose 409/412
      answers are meant to supersede, and the three `errors.Join(conflictErr,
      ...)` in `resolveCreateConflict`/`claimLostCreate`, which are not stale at
      all. The shutdown, stalled-transfer and name-taken checks stay ahead of
      the stale check. Validation step: full non-race `./internal/sync/...`.
      Owner if wrong: `engine_result_classify.go`.
    - Question: is a stale completion carrying a 403 or a permission denial
      reachable today? Current assumption: not through current producers; the
      guard makes the order hold for any status. Validation step: the
      engine-flow test. Owner if wrong: `engine_runtime_permissions.go`.
    - Question: does the replanned upload-only pass find the collision in the
      same run? Current assumption: yes, the one-shot follow-up pass runs after
      a supersede (`oneShotFollowUp`, R-2.1.7). Validation step: the flipped
      test. Owner if wrong: `engine_run_once*.go`.
