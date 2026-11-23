# W10 Definition of Ready: exhaustive issue-type decisions

Worktree `~/src/onedrive-go-wt/issue-type-leftovers`, branch
`fix/issue-type-leftovers`, base `origin/main` at `aed97bce`.

1. PASS **Task intent**: every decision `internal/sync` makes per `IssueType`
   names every member on purpose (an exhaustive switch the linter holds, plus
   a test that reads the members from `issue_types.go`), and a value outside
   the enum gets a verdict chosen on purpose instead of the "not this one"
   answer an equality test or a lookup miss gives. Concretely:
   `observationIssueBlocksRemoteTruth` (two equality tests) and
   `conditionKeyForIssueType` (a lookup table) become exhaustive; the same
   class found elsewhere is fixed too (remote-twin subtree test in
   `truth_status.go`, the retry curve, the permission boundary scope, the
   block-scope activation line). The end-of-pass summary key
   `fallbackFailureSummaryIssueType` is renamed to say it is a summary group
   for retry work without a condition, not an issue type.
2. PASS **Non-goals**: no rename of the stored value `hash_panic` or of its Go
   constant (only a comment at the constant); no store generation change; no
   one-shot exit-status table in `internal/cli`; no change to how a member
   is decided today (every member keeps its present verdict, so no retry
   timing, truth verdict or log level changes for a member).
3. PASS **Governing docs**: `CLAUDE.md` (lines 55-236), `spec/design/sync-domain.md`
   (IssueType paragraph, Verified By), `spec/design/sync-store.md` (truth
   availability paragraph, line 148 Verified By row; generation text left
   alone), `spec/design/sync-observation.md` ("Unavailable Is Not Absent"),
   `spec/design/sync-engine.md` (Verified By rows for console reporting and
   permission handling), `spec/design/error-model.md` (condition-key test row),
   requirements R-6.2.14, R-6.8.16, R-2.10.71, R-2.10.13, R-6.6.30.
4. PASS **Authority owner**: issue-type vocabulary and condition keys:
   `issue_types.go`/`condition_keys.go` (sync-domain, layer 0); truth verdicts:
   `truth_status.go` (sync-store, layer 1); retry curve, permission scope and
   block-scope wording: engine runtime files (sync-engine, layer 4); the
   summary key: `engine_results.go` (sync-engine). No new owner; every new
   declaration lives in the file that already owns the decision.
5. PASS **Code scope**: `internal/sync/{issue_types,condition_keys,truth_status,
   engine_results,engine_runtime_completion,engine_runtime_permissions,
   engine_runtime_lifecycle}.go` and their tests, plus the docs above. Must not
   touch: `schema.go`/store generation, `scanner.go` (the rename rides on a
   later change), `internal/cli`.
6. PASS **Regression plan**: new tests, red before the change:
   `TestObservationIssueBlocksRemoteTruth_ValueOutsideTheEnumIsUnavailable`,
   `TestConditionKeyForIssueType_EveryMemberIsDecided` (members read from
   source; a non-member is the unexpected condition), source-read verdict
   tests for the retry curve, the permission boundary scope and the
   block-scope line, the remote-subtree verdict added to
   `TestObservationIssueBlocksTruth_VerdictForEveryIssueType`. Existing
   `TestConditionKey*`, `TestRuntimeConditionKey_RepresentativeMappings`,
   `TestEngineFlow_BlockScope_ReportedOnceWhenItStarts`,
   `TestEngineFlow_LogFailureSummary_AggregatesRetryWorkBelowWarn` stay green.
7. PASS **Verification plan**: targeted `go test -race -run` over the touched
   tests; `go build ./...`; `go vet ./...`; `go vet -tags 'e2e e2e_full'
   ./e2e/...`; full non-race `go test -count=1 -timeout 60m
   ./internal/sync/...`; `go test -count=1 ./internal/devtool/...`; one
   targeted golangci-lint over `./internal/sync/...` (one lint at a time).
   No `devtool verify`, no e2e.
8. PASS **Data safety**: no member's verdict changes, so planning, deletes and
   retry timing are unchanged for every value a store can hold (the stores
   refuse non-members on read and write). The only behaviour change is for a
   value outside the enum, which only a programming error makes, and it moves
   toward fail-closed: remote truth unavailable, the unexpected condition,
   the slower retry curve.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or
   credential-bearing logs involved; tests use synthetic values only.
10. PASS **Live provider and destructive operations**: none. No live
    OneDrive calls, no state reset, no dependency upgrade, no history rewrite,
    no push.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does any member reach `retryDelayForCondition` with a class
      that makes its verdict matter beyond the two durable conditions?
      Current assumption: keep every member's present curve, so it does not
      matter. Validation step: the retry-curve verdict table equals today's
      behaviour for every member. Owner affected if wrong:
      `engine_runtime_completion.go`, R-2.10.71.
    - Question: is an unknown issue type with a scope key better shown as the
      scope's condition or as the unexpected condition?
      Current assumption: the unexpected condition (the store refuses such a
      value, so it marks a defect; the scope's own group is still shown from
      `block_scopes`). Validation step: `TestConditionKeyForStoredCondition_RepresentativeMappings`
      states it. Owner affected if wrong: `condition_keys.go`, sync-domain.md.
