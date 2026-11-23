# C2 Definition of Ready: observation issue types are typed

1. PASS **Task intent**: repo-maintenance outcome with one behaviour edge.
   Issue types become the named type `IssueType` (internal/sync/issue_types.go);
   `observationIssueBlocksLocalTruth` switches over it exhaustively, with a
   stated verdict for every member, instead of an untyped string with a
   fall-through `default: false`. The stores convert at their boundary and
   refuse a value no member has.
2. PASS **Non-goals**: the reconciliation and comparison SQL `CASE`s and every
   other item of analysis section A1; any schema or store-generation change;
   renames beyond what the type needs; the managed-types list of the local
   findings batch (see report, recommendations).
3. PASS **Governing docs**: CLAUDE.md lines 55-236; spec/design/sync-domain.md
   (governs issue_types.go, core_types.go, condition_keys.go, scope_*.go,
   domain_runtime_decision.go; LAYER 0); spec/design/sync-store.md (governs
   truth_status.go, store_*.go, observation_reconcile_policy.go,
   condition_projection.go; LAYER 1); spec/design/sync-observation.md
   "Unavailable Is Not Absent" (scanner producers); requirements R-2.5.6,
   R-6.2.14, R-6.2.15 (spec/requirements/sync.md, non-functional.md).
   Analysis section A1.7 read (lines 425-450 only).
4. PASS **Authority owner**: the vocabulary and its membership list are owned
   by the sync domain (issue_types.go, layer 0). The durable rows are owned by
   the sync store (observation_issues by observation reconciliation, retry_work
   by the store's retry helpers); the string conversion and the refusal of
   unknown values live there. The truth verdict is owned by the store's
   derived truth view (truth_status.go). No runtime lifecycle or user-facing
   output changes except that a store holding an unknown issue type now fails
   its read with a message naming the row and value.
5. PASS **Code scope**: internal/sync (issue_types.go, truth_status.go,
   store_observation_issues.go, store_retry_work.go, store_types.go,
   observation_reconcile_policy.go, observation_findings.go, and the files that
   produce or carry issue types: scanner, permission probe, result
   classification, scope semantics, condition keys/projection, engine logging);
   internal/cli/status_condition_descriptors.go (one conversion at the JSON
   boundary). Not touched: schema.go, the SQL CASEs, store generation.
6. PASS **Regression plan**: new tests
   `TestObservationIssueBlocksTruth_VerdictForEveryIssueType`,
   `TestObservationIssueBlocksLocalTruth_ValueOutsideTheEnumIsUnavailable`,
   `TestIssueType_EveryDeclaredConstantIsAMemberAndParses`,
   `TestParseIssueType_RefusesAValueNoMemberHas`,
   `TestSyncStore_ListObservationIssues_RefusesUnknownStoredIssueType`,
   `TestSyncStore_ReconcileObservationFindings_RefusesToWriteAnUnknownIssueType`,
   `TestScanRetryWorkRow_ConditionTypeMustBeEmptyOrAMember`,
   `TestUpsertRetryWork_RefusesAConditionTypeNoMemberHas`. Each behaviour test
   was run red against the old behaviour (fall-through `false`, no store
   check) and fails for the stated reason.
7. PASS **Verification plan**: targeted `go test -race -count=1 -timeout`
   runs of every test in the touched test files and of the tests that exercise
   the retyped paths (two batches); `go build ./...`; `go vet ./...`;
   `go vet -tags 'e2e e2e_full' ./e2e/...`; targeted golangci-lint on
   ./internal/sync/... and ./internal/cli/...; a scratch go/types checker for
   testify assertions comparing an `IssueType` with a plain string (compiles,
   never equal). The full `devtool verify` is the reviewer's.
8. PASS **Data safety**: no delete, overwrite or schema change. The only
   behaviour change is fail-closed: an unknown stored issue type stops the
   read (planning and status) instead of being treated as "not blocking", and
   a value outside the enum is unavailable truth. Every value production code
   writes is a member, so existing stores of this generation read as before.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or
   credential-bearing logs involved.
10. PASS **Live provider and destructive operations**: none. No live OneDrive
    call, no state reset, no dependency upgrade, no history rewrite.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does the constant set double as another vocabulary, so that
      typing it reaches beyond observation issues? Current assumption: yes; the
      same constants are retry_work `condition_type`, scope default conditions
      and runtime decision conditions, so the type is threaded through those
      too (deviation in the report). Validation step: compiler and the
      testify-mismatch checker over sync, cli, multisync, synccontrol,
      syncverify. Owner affected if wrong: issue_types.go, store_retry_work.go.
    - Question: what should an unknown stored value do? Current assumption:
      fail the read naming the row (precedent: `parseActionType`,
      `parseComparisonKind`; R-2.5.6 "reject non-canonical state loudly at the
      store boundary"). Validation step: store tests above. Owner affected if
      wrong: store_observation_issues.go, store_retry_work.go.
    - Question: is any existing issue type wrongly missing from the
      local-truth list (analysis: SUSPECTED)? Current assumption: no; settled
      by listing every member (report). Validation step: verdict test.
      Owner affected if wrong: truth_status.go.
