# C2 report: observation issue types are typed (commit 2865051c)

## What changed
- issue_types.go: `type IssueType string`; every constant typed; 5 new members (transient conditions that were literals); `issueTransferStalled` renamed `issueRequestTimeout`; `isMember()` (exhaustive switch) and `parseIssueType`.
- truth_status.go: `observationIssueBlocksLocalTruth` names every member (8 true, 28 false); `default` (non-member) is now true (fail-closed). Signatures take `IssueType`.
- store_observation_issues.go: scan parses `issue_type`, unknown fails the read naming path and value; upsert refuses a non-member; SQL args converted with `string()`.
- store_retry_work.go: scan parses non-empty `condition_type` (empty = no condition); upsert refuses a non-member.
- store_types.go, core_types.go, observation_reconcile_policy.go, observation_findings.go, store_remote_twins.go: observation issue fields, `ManagedIssueTypes`, `skippedItem.Reason`, `pathTruthSideStatus.IssueType`, managed key typed; `stringSet` removed (sole caller gone).
- condition_keys.go, condition_projection.go, scope_semantics.go, scope_key.go, scope_block.go, scope.go, domain_runtime_decision.go, permission_evidence.go, permission_probe_local.go, scanner.go, engine_result_classify.go, engine_runtime_{completion,lifecycle,permissions}.go, engine_retry_trial.go, engine_results.go, engine_watch_observation.go, observer_local_handlers.go: condition-type fields and helpers typed; `string()` only at log attributes; three partial switches carry an explained `//nolint:exhaustive`.
- cli/status_condition_descriptors.go: `string()` into the JSON field.
- 13 existing test files: types and literals updated (incl. 4 `assert.Equal("literal", typed)` that would have failed at run time, found with a scratch go/types checker).
- spec/design/sync-domain.md, sync-store.md: the type, the store boundary, the verdict; Verified By rows.

## Unknown stored value
Read: the scan fails with `sync: unknown issue type "<v>"` and the row's path, so planning, reconcile and status stop for that drive instead of guessing (R-2.5.6, precedent `parseActionType`); recovery is `drive reset-sync-state`. Write: refused with the same message, the transaction rolls back.

## Verdict per issue type (local / remote)
- local: invalid_filename, path_too_long, file_too_large, case_collision, hash_panic, local_read_denied, local_unsyncable_replacement, local_mount_boundary.
- remote only: remote_read_denied, remote_name_twin.
- neither: local_interrupted_removal (observation issue at an internal name the content filter hides on both sides; inert), and the 25 runtime-only members (never written to observation_issues).
All were already handled as intended; making the switch exhaustive changed no member's verdict. The only behaviour change is the non-member default (false -> true).

## Tests added
- TestObservationIssueBlocksTruth_VerdictForEveryIssueType: every declared member (read from issue_types.go source) has the stated local and remote verdict.
- TestObservationIssueBlocksLocalTruth_ValueOutsideTheEnumIsUnavailable: a non-member blocks local truth, directly and via the truth index (red on old default).
- TestIssueType_EveryDeclaredConstantIsAMemberAndParses / TestParseIssueType_RefusesAValueNoMemberHas: isMember covers every constant, no duplicates; other strings incl. "" refuse.
- TestSyncStore_ListObservationIssues_RefusesUnknownStoredIssueType, TestSyncStore_ReconcileObservationFindings_RefusesToWriteAnUnknownIssueType, TestScanRetryWorkRow_ConditionTypeMustBeEmptyOrAMember, TestUpsertRetryWork_RefusesAConditionTypeNoMemberHas: read and write refusal on both tables (all red without the checks).

## Checks
- go test -race -count=1, two targeted batches in ./internal/sync (observation/retry/truth/condition tests; scope, classify, scanner, twins, mount, permission, logging, run-once tests of touched paths): ok.
- go build ./..., go vet ./..., go vet -tags 'e2e e2e_full' ./e2e/...: ok.
- golangci-lint ./internal/sync/... ./internal/cli/...: 0 issues (C2-lint.log).
- Scratch checker (testify asserts comparing IssueType with string) over sync, cli, multisync, synccontrol, syncverify: none left.

## Deviations (scope or interface)
- The constants are also the retry_work/scope/runtime condition vocabulary, so the type is threaded through those paths too, not only observation issues; retry_work gets the same store-boundary parse.
- Five new members and one rename (above): needed so every value the type carries is a member.
- Write-side refusal on both stores (brief named the read side).
- Non-member verdict changed to "blocks local truth" (fail-closed).

## Recommendations
1. `observationIssueBlocksRemoteTruth` has the same fail-open shape (two `==`); make it an exhaustive switch.
2. local_interrupted_removal is upserted every scan but missing from `localObservationManagedIssueTypes`, so the condition never clears after the user removes the file (status shows it forever). Add it, with a test.
3. `conditionKeyForIssueType` is a lookup table; a new member silently maps to "unexpected condition". An exhaustive switch would force a decision.
4. `hash_panic` also records ordinary read I/O errors, and sync-observation.md calls it `hash_error` (the condition key); rename needs a generation bump.
5. `fallbackFailureSummaryIssueType` is a log key, not an issue type; rename.

## Unfinished / for the reviewer
Nothing unfinished. Run ./internal/cli tests (status JSON conversion, status_test.go) and the full ./internal/sync suite; multisync, synccontrol, syncverify only compile against the change.
