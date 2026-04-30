# Error Model

Implements: R-6.8.16 [verified]

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
- `sync.ConditionKey`: the shared sync-domain grouping key used for structured
  logs and status rendering
- durable sync authorities: `observation_issues`, `retry_work`, and
  `block_scopes`

These projections intentionally answer different questions:

- runtime class answers "what should the process do next?"
- condition key answers "how should sync-domain state be grouped and
  explained?"
- durable authorities answer "what restart-safe fact or policy decision do we
  need to persist?"

## Canonical Classes

| Class | Meaning | Automatic Follow-Up |
| --- | --- | --- |
| `success` | The operation completed and durable/runtime state should advance normally. | Commit success, clear stale durable rows. |
| `shutdown` | Work stopped because the caller canceled or the process is shutting down. | Stop cleanly; do not invent durable retry or actionable rows just because the process is exiting. |
| `superseded` | The exact runtime action was valid when planned but is obsolete under current execution preconditions or newer truth. | Retire that exact action and its old dependents without success semantics, clear any exact stale retry row, and replan in watch mode. |
| `retryable transient` | A specific item failed for a condition expected to clear without human action. | Persist `retry_work` with the next retry time. |
| `scope-blocking transient` | A wider transient condition makes a whole scope unsafe to keep dispatching. | Persist `block_scopes` plus blocked `retry_work`. |
| `actionable` | Automatic retry is not appropriate; the user must fix content, permissions, or configuration. | Persist the appropriate durable authority for the owning boundary: observation may write `observation_issues`; execution persists `retry_work`, `block_scopes`, or account-auth state and waits for observation to prove durable current-truth issues. |
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
- `internal/sync/condition_keys.go`: shared `ConditionKey` normalization and
  canonical condition-family ordering helpers
- `internal/cli/status_condition_descriptors.go`: CLI-owned rendering tables for
  `ConditionKey` values until the status-surface naming sweep lands
- `internal/config/failure_class.go`: classify config load results
- `internal/sync/executor_preconditions.go`: translate stale live local/remote
  side-effect checks into `ErrActionPreconditionChanged`
- `internal/sync/engine_result_classify.go`: classify each action completion
- `internal/cli/failure_class.go`: classify command-returned errors into exit
  behavior and reason/action text

## Persistence Mapping

The target durable projection is intentionally small:

- `retryable transient` -> `retry_work`
- `scope-blocking transient` -> `block_scopes` plus blocked `retry_work`
- `actionable` -> `observation_issues` only when observation or probe proves a
  durable current-truth/content problem; execution-owned actionable results do
  not upsert observation rows directly
- `success` -> baseline/remote-state commit plus explicit durable cleanup where
  required
- `superseded` -> no ordinary retry/block row; clear any exact existing
  `retry_work` row for the stale action and let the next plan decide from
  current truth
- `shutdown` and `fatal` -> returned to the caller unless a higher boundary
  intentionally converts them into one of the durable classes above

This keeps durable state as a record of decisions the engine must recover from,
not a catch-all copy of every raw error string or a mixed reporting table.

For permission-derived conditions, the durable mapping is access-specific:

- observation-owned read denial -> `observation_issues` tagged with the
  unreadable boundary `ScopeKey`
- execution-owned write denial -> `block_scopes` plus blocked `retry_work`
- raw `403` / `os.ErrPermission` without probe evidence -> no permission block scope;
  fall back to ordinary retry or fatal handling

Permission recovery follows the same ownership split:

- observation-owned read boundaries clear only when a later observation pass
  stops proving the boundary
- execution-owned write scopes clear through normal timed trials, successful
  writes, or cleanup that leaves no blocked work
- block-scope timing lives in the engine/watch retry-trial loop, not in a
  separate permission-maintenance boundary

## Boundary Rules

- Errors cross one classification boundary before being wrapped with local
  context.
- The boundary that understands the invariant owns the classification.
- `ErrActionPreconditionChanged` is the sync executor/worker signal for
  "this exact action is obsolete." Worker-start freshness, admission freshness,
  and executor live preconditions may return it; the engine maps it to
  `superseded`, not ordinary retry. Transient failures while trying to read
  live precondition truth are not wrapped with this sentinel. Graph conditional
  mutation mismatches surface first as `graph.ErrPreconditionFailed` from HTTP
  412; the executor translates that boundary-specific fact into
  `ErrActionPreconditionChanged` when it proves a post-preflight stale action.
- Retry/backoff consumes the classified result; it does not classify on its
  own.
- User-facing messaging consumes the classified result; it does not inspect raw
  HTTP or filesystem payloads directly.
- The sync package owns logging for its classified results.

## Verified By

| Boundary | Evidence |
| --- | --- |
| Shared failure classes | `internal/errclass/errclass_test.go` (`TestClassStringAndValidity`), `internal/config/failure_class_test.go` (`TestClassifyLoadOutcome`), `internal/cli/failure_class_test.go` (`TestClassifyCommandError`, `TestCommandFailurePresentationForClass`) |
| Shared condition-key normalization, ordering, shared stored-condition projection, and CLI rendering tables | `internal/sync/condition_keys_test.go` (`TestConditionKeyForStoredCondition_RepresentativeMappings`, `TestConditionKeyLess_UsesCanonicalDisplayOrder`, `TestConditionKeyForIssueType_RepresentativeMappings`), `internal/sync/condition_projection_test.go` (`TestProjectStoredConditionGroups_MergesDurableAuthorities`), `internal/cli/status_test.go` (`TestQuerySyncState_PreservesConditionScopeContext`, `TestPrintSyncStateText_KeepsSameSummaryGroupsSeparatedByScope`, `TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsConditions`, `TestPrintSyncStateText_WithConditions`), `internal/cli/status_golden_test.go` (`TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON`) |
| Sync result classification foundations | `internal/sync/engine_result_classify_test.go` (`TestClassifyResult_SuccessAndShutdown`, `TestClassifyResult_HTTPPersistenceAndScopeRouting`, `TestClassifyResult_LocalPersistenceAndScopeRouting`), `internal/sync/engine_runtime_completion_test.go` (`TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess`, `TestEngineFlow_ProcessTrialDecision_SupersededClearsExactRetryAndDiscardsEmptyScope`), `internal/sync/engine_run_once_results_test.go` (`TestOneShotEngineLoop_SupersededCompletionRetiresDependentsWithoutSuccessOrRetry`), `internal/sync/engine_retry_trial_test.go` (`TestReleaseDueHeldRetriesNow_ReleasesHeldRetryEntriesOnly`, `TestReleaseDueHeldRetriesNow_DoesNotConsultDurableRetryRowsWithoutHeldRuntimeEntry`, `TestReleaseDueHeldTrialsNow_ReleasesFirstHeldScopeCandidateAsTrial`, `TestReleaseDueHeldTrialsNow_SkipsScopesWithoutHeldDependencyReadyCandidates`, `TestReleaseDueHeldTrialsNow_DoesNotConsultDurableBlockedRetryRowsWithoutHeldRuntimeEntry`, `TestClearRetryWorkOnSuccess_RemovesResolvedRetryRow`), `internal/sync/engine_runtime_lifecycle_test.go` (`TestEngineFlow_ApplyTrialReclassification_RehomesDiskScopeRetryWork`), `internal/sync/store_scope_admin_test.go` (`TestPruneBlockScopesWithoutBlockedWork`) |
