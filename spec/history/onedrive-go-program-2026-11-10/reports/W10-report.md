# W10 report: exhaustive issue-type and condition-key decisions (commit a49819af on aed97bce, 38 files)

## What changed
- `internal/sync/truth_status.go`: one exhaustive switch `truthVerdictForIssueType` (local, remote, remote subtree), read by `observationIssueBlocks{Local,Remote}Truth`; the two `== issueRemoteNameTwin` boundary tests use its subtree verdict. A non-member blocks both sides of its path.
- `internal/sync/condition_keys.go`: `conditionKeyForIssueType` exhaustive (non-member: `ConditionUnexpectedCondition`, ok; transient members and "": none); transient runtime results get no `condition_key`; class chain is an exhaustive `errclass.Class` switch; new exhaustive `conditionClearsAutomatically` over every ConditionKey.
- `internal/sync/watch_condition_log.go`: the summary's "automatic" test reads `conditionClearsAutomatically`; labels are the exhaustive `conditionLabelText` (texts byte-identical, checked by script against HEAD).
- `internal/sync/engine_runtime_lifecycle.go`: `blockScopeActivationLine` = exhaustive `blockScopeActivationWording` + level derived from the family's `conditionClearsAutomatically`.
- `internal/sync/engine_runtime_completion.go`: `retryDelayForCondition` exhaustive, no nolint; non-member waits on the durable curve.
- `internal/sync/engine_runtime_permissions.go`: `scopeKeyForPermissionEvidence` exhaustive, no nolint.
- `internal/sync/permission_probe_local.go`: evidence helpers lose the dead `actionType` parameter and the unused receiver; repeated reason is `localFileNotAccessibleReason`.
- `internal/sync/engine_results.go`: `fallbackFailureSummaryIssueType` -> `noConditionSummaryGroup` (value `transient_failure` kept), field `issueType` -> `group`; summary simplified; `overdueTrialDelay`.
- `internal/sync/issue_types.go`: D27 comment at `issueHashPanic`; const-block comment names every per-type switch.
- 14 files (`issue_types`, `store_read_remote_state`, `store_write_block_scopes`, `scanner`, `buffer`, `store_write_baseline`, `enums`, `observer_local`, `scope`, `store_write_observation`, `observer_local_handlers`, `store`, `active_scopes`, `observer_local_collisions`): second package doc made a file comment; stale names in their lists corrected. `go doc ./internal/sync` shows only `doc.go`'s text.
- Tests: helpers in `issue_types_test.go` (`requireEveryConstantDecided`, `verdictTable`/`verdictGroup`, IssueType wrappers, `stringConstantsOfType`, untyped `issueTypeOutsideTheEnum`, `conditionKeyOutsideTheSet`) plus the tests below; requirement tags fixed in `engine_results_test.go`, `output_noise_test.go`.
- Docs: `sync-domain.md`, `sync-store.md` (generation untouched), `sync-observation.md`, `sync-engine.md`, `error-model.md`; `spec/requirements/non-functional.md` R-6.6.7.

## Tests (red before the fix unless noted)
- `TestObservationIssueBlocksRemoteTruth_ValueOutsideTheEnumIsUnavailable`: a non-member blocks the path's OneDrive truth.
- `TestObservationIssueBlocksTruth_VerdictForEveryIssueType` (extended; compile-red): every member's three verdicts, from source.
- `TestTruthAvailabilityIndex_OnlyASubtreeBoundingTypeBlocksDescendants` (green before: pins what the boundary refactor keeps).
- `TestConditionKeyForIssueType_EveryMemberIsDecided`: every member's family from source; non-member unexpected; "" none.
- `TestConditionKeyForStoredCondition_ValueOutsideTheEnumIsUnexpectedUnderAnyScope`: no longer the scope's condition.
- `TestRuntimeConditionKey_TransientConditionsHaveNoFamily`: a 408's decision has no condition key.
- `TestRetryDelayForCondition_EveryMemberIsDecided`: every member's curve; non-member durable.
- `TestScopeKeyForPermissionEvidence_EveryMemberIsDecided` (green before: verdicts unchanged; holds new members).
- `TestBlockScopeActivationLine_EveryMemberIsDecided` (compile-red): level and wording per member.
- `TestConditionKeyRank_EveryDeclaredKeyHasItsOwnRank` (green before: missing guard).
- `TestEngineFlow_LogFailureSummary_GroupsFailuresWithNoConditionApart` (green before: pins the renamed group).
- `TestConditionClearsAutomatically_EveryKeyIsDecided` (compile-red): every declared key's verdict from source; a key outside the set is not automatic.
- `TestConditionLabel_EveryDeclaredKeyHasALabel` (compile-red): every declared key has a label in words; only an undeclared key falls back.
- `TestBlockScopeActivationLine_AgreesWithTheFamilyVerdict` (compile-red): the line's level equals the family's verdict for every issue type.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass (final tree).
- `go test -race` over the touched tests and every `ListsEvery|Table|Governs|Exhaustive|Registered` test: pass (final tree).
- `go test -count=1 ./internal/cli/...`: ok (final tree).
- First pass only: full `go test -count=1 -timeout 60m ./internal/sync/...` ok (572 s), `./internal/devtool/...` ok, golangci-lint `./internal/sync/...` `0 issues.` (W10-lint.log). Not rerun after the second pass, as instructed; the coordinator runs both.

## Deviations (accepted in review)
- Truth is one verdict switch read through the two existing functions, not two switches.
- The class was fixed beyond the two named functions: twin subtree tests, retry curve, permission scope, block-scope line. No member's verdict changed there.
- Non-members fail closed: remote truth blocked; unexpected condition even under a scope (was the scope's); durable curve (was reconcile).
- Member change: retried transient results (408, 409/412 conflict, 404, 423, rejected transfer URL) log `condition_key=` empty, was `unexpected_condition`.

## Survey of per-issue-type decisions
- Fixed: `truth_status.go` remote verdict and twin subtree (3 equality tests); `condition_keys.go` family table; retry curve, permission scope, scope line (3 `nolint:exhaustive` defaults); per ConditionKey: the summary's automatic test and the label map.
- Already right: `isMember`; `observationIssueBlocksLocalTruth` (#875); `localObservationManagedIssueTypes` (#880) and the reconcile refusal; `parseIssueType` and the retry_work write check; `aggregateSkippedText` (uses the family switch); `engine_runtime_completion.go:602` (emptiness only); `permission_probe_local.go:65` (value from the two-way `localPermissionIssueType`); `serverAnswerIssueTypes` (produces types). `internal/cli` decides nothing per IssueType; its descriptors are exhaustive over ConditionKey.

## Also fixed
- `logFailureSummary`: dead `key == ""` branch, duplicated Debug loop, redundant `count`; stale "WARN aggregation" comment on `recordError`. `armTrialTimer`: magic `1 * time.Millisecond`.
- `TestConditionKeyLess_UsesCanonicalDisplayOrder` cited R-2.10.47 (auth state); now R-2.10.4. Duplicate assertion in `TestConditionKeyForStoredCondition_RepresentativeMappings` removed.
- R-6.6.7 reworded per summary kind; `TestEngineFlow_LogFailureSummary_AggregatesRetryWorkBelowWarn` now cites R-6.6.7, R-6.6.12 (R-6.6.12 is the INFO-not-WARN rule; R-6.6.18 is the console line it does not test); the two skipped-file aggregation tests now cite R-6.6.7.
- Corrected my own sentence in `retryDelayForCondition`'s comment (and its test): "every other member clears by itself or is ClassActionable" was wrong for throttling, outage, full drive/disk (held under their block scope) and sign-in (never retry work).
- `scope.go` comments: `ScopeState`/`ScopeUpdateResult`/`ScopeAdmissionDecision` -> current names; dangling "§7.3.1" reference and a nonexistent `BlockScopeStore` removed.

## Review fixes (same commit, by the reviewer)
- `internal/sync/scope_key.go` and `scope_test.go`: the comments that named `ParseScopeKey` now name `parseScopeKey` (the writer had listed these under "Not fixed").
- `engine_runtime_completion.go`: an overlong comment line in `retryDelayForCondition` rewrapped.

## Not fixed
- `hash_panic` rename with its generation: later change, per D27 (planned as a rider on C3).

## Unfinished
- None. Reviewer: run golangci-lint over `./internal/sync/...` and the full `./internal/sync/...` suite on `a49819af` (not run by me in the second pass, as instructed).
