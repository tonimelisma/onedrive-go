# Error Model

Implements: R-6.8.16 [designed]

## Overview

The repository uses one domain error model across configuration, Graph I/O,
sync runtime, durable persistence, and CLI presentation. Each boundary owns
one translation step from raw errors into that shared model. Higher layers may
add context, but they do not invent a second classification scheme.

`internal/errclass` is the executable leaf package that defines the shared
`Class` type. Boundary packages consume that vocabulary rather than re-declaring
local enums.

## Related Projections

The target architecture keeps three related but distinct projections of failure
state:

- `errclass.Class`: the runtime execution contract used by sync routing,
  retry/trial policy, and CLI exit behavior
- `sync.SummaryKey`: the shared sync-domain grouping key used for structured
  logs and status rendering
- durable sync authorities: `observation_issues`, `retry_work`, and
  `block_scopes`

These projections intentionally answer different questions:

- runtime class answers "what should the process do next?"
- summary key answers "how should sync-domain state be grouped and explained?"
- durable authorities answer "what restart-safe fact or policy decision do we
  need to persist?"

## Canonical Classes

| Class | Meaning | Automatic Follow-Up |
| --- | --- | --- |
| `success` | The operation completed and durable/runtime state should advance normally. | Commit success, clear stale durable rows. |
| `shutdown` | Work stopped because the caller canceled or the process is shutting down. | Stop cleanly; do not invent durable retry or actionable rows just because the process is exiting. |
| `retryable transient` | A specific item failed for a condition expected to clear without human action. | Persist `retry_work` with the next retry time. |
| `scope-blocking transient` | A wider transient condition makes a whole scope unsafe to keep dispatching. | Persist `block_scopes` plus blocked `retry_work`. |
| `actionable` | Automatic retry is not appropriate; the user must fix content, permissions, or configuration. | Persist a durable `observation_issues` row or other user-facing status fact, depending on what current truth the failure revealed. |
| `fatal` | The current command or drive runtime cannot continue safely. | Abort the current flow and return an error immediately. |

## Translation Ownership

Each boundary owns exactly one translation step:

- `graph`: normalize wire/auth/API failures into `GraphError` plus sentinels
- `config`: normalize parse, validation, and discovery outcomes into fatal load
  errors or lenient warnings
- `sync`: normalize action completions, observer sentinels, and permission
  checks into `ResultDecision`, retry scheduling, scope decisions, and durable
  authority writes
- `cli`: map fatal/actionable/transient outcomes into exit behavior and
  reason/action text

## Executable Mapping

The docs and code stay aligned through a small number of explicit classifier
entry points:

- `internal/errclass`: the `Class` type
- `internal/sync/summary_keys.go`: shared `SummaryKey` normalization helpers
- `internal/cli/status_issue_descriptors.go`: CLI-owned rendering tables for
  `SummaryKey` values until the status-surface naming sweep lands
- `internal/config/failure_class.go`: classify config load results
- `internal/sync/engine_result_classify.go`: classify each action completion
- `internal/cli/failure_class.go`: classify command-returned errors into exit
  behavior and reason/action text

## Persistence Mapping

The target durable projection is intentionally small:

- `retryable transient` -> `retry_work`
- `scope-blocking transient` -> `block_scopes` plus blocked `retry_work`
- `actionable` -> `observation_issues` when the result reveals a durable
  current-truth/content problem
- `success` -> baseline/remote-state commit plus explicit durable cleanup where
  required
- `shutdown` and `fatal` -> returned to the caller unless a higher boundary
  intentionally converts them into one of the durable classes above

This keeps durable state as a record of decisions the engine must recover from,
not a catch-all copy of every raw error string or a mixed reporting table.

## Boundary Rules

- Errors cross one classification boundary before being wrapped with local
  context.
- The boundary that understands the invariant owns the classification.
- Retry/backoff consumes the classified result; it does not classify on its
  own.
- User-facing messaging consumes the classified result; it does not inspect raw
  HTTP or filesystem payloads directly.
- The sync package owns logging for its classified results.

## Verified By

| Boundary | Evidence |
| --- | --- |
| Shared failure classes | `internal/errclass/errclass_test.go` (`TestClassStringAndValidity`), `internal/config/failure_class_test.go` (`TestClassifyLoadOutcome`), `internal/cli/failure_class_test.go` (`TestClassifyCommandError`, `TestCommandFailurePresentationForClass`) |
| Shared summary-key normalization and CLI rendering tables | `internal/sync/summary_keys_test.go` (`TestSummaryKeyForPersistedFailure_RepresentativeMappings`, `TestSummaryKeyForBlockScope_RepresentativeMappings`, `TestSummaryKeyForIssueType_RepresentativeMappings`), `internal/cli/status_test.go` (`TestQuerySyncState_PreservesIssueGroupScopeContext`, `TestPrintSyncStateText_KeepsSameSummaryGroupsSeparatedByScope`, `TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsIssues`, `TestPrintSyncStateText_WithIssueGroups`), `internal/cli/status_golden_test.go` (`TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON`) |
| Sync result classification foundations | `internal/sync/engine_result_scope_test.go` (`TestClassifyResult_LifecycleAndAuth`, `TestClassifyResult_RemoteRetriesAndSkips`, `TestClassifyResult_StorageScopes`, `TestClassifyResult_LocalErrors`, `TestRetryPipeline_TransientFailure_IntegratedRetrier`, `TestDiskLocalBlockScope_FullCycle`, `TestProcessTrialResultV2_Success_ClearsScope`, `TestProcessTrialResultV2_Preserve_LocalPermissionRecordsCandidateFailure`, `TestEvaluateTrialOutcome_OnlyMatchingScopeEvidenceExtends`) |
