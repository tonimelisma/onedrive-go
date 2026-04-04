# Error Model

Implements: R-6.8.16 [verified]

## Overview

The repository uses one domain error model across configuration, Graph I/O,
sync runtime, durable persistence, and CLI presentation. Each boundary owns
one translation step from raw errors into this shared model. Higher layers may
add context, but they do not invent a second classification scheme.

`internal/failures` is the executable leaf package that names the shared
classes and log owners. Boundary packages consume that shared vocabulary rather
than re-declaring local enums.

## Related Projections

The repository keeps three related but distinct projections of failure state:

- `failures.Class`: the runtime execution contract used by sync routing,
  retry/trial policy, and CLI exit behavior.
- `synctypes.SummaryKey`: the shared sync-domain rendering key used for
  structured logs, read-only issue summaries, and human-facing sync issue
  presentation.
- Persisted issue fields: `issue_type`, `category`, `failure_role`, and
  `scope_key`, which remain the durable record in SQLite.

These projections intentionally answer different questions:

- runtime class answers "what should the process do next?"
- summary key answers "how should sync-domain state be grouped and explained?"
- persisted fields answer "what durable evidence and policy decision was
  recorded?"

## Canonical Classes

| Class | Meaning | Automatic Follow-Up |
|------|---------|---------------------|
| `success` | The operation completed and durable/runtime state should advance normally. | Commit success, clear stale transient state. |
| `shutdown` | Work stopped because the caller canceled or the process is shutting down. | Stop cleanly; do not invent retry or actionable rows purely because the process is exiting. |
| `retryable transient` | A specific item failed for a condition that is expected to clear without human action. | Persist `sync_failures` row with `category='transient'`, `failure_role='item'`, and `next_retry_at`. |
| `scope-blocking transient` | A wider transient condition makes a whole scope unsafe to keep dispatching. | Persist `scope_blocks` plus held/boundary failure rows when the scope is durable; derived scopes such as `perm:remote` persist held blocked-write rows only and recover through recheck or manual trial. |
| `actionable` | Automatic retry is not appropriate; the user must fix content, permissions, or configuration. | Persist/display actionable failure with reason and user action. |
| `fatal` | The current command or drive runtime cannot continue safely. | Abort the current flow and return an error immediately. |

## Translation Ownership

Each boundary owns exactly one translation step:

- `graph`: normalize wire/auth/API failures into `GraphError` plus sentinels such as `ErrGone`, `ErrUnauthorized`, `ErrNotFound`, and `ErrThrottled`.
- `config`: normalize parse, validation, and discovery outcomes into fatal load errors or lenient warnings.
- `sync`: normalize `WorkerResult`, observer sentinels, and permission checks into `ResultDecision`, scope actions, retry scheduling, and success cleanup.
- `syncstore`: persist the engine's classification using `category`, `failure_role`, `scope_key`, and `next_retry_at`; it never reclassifies raw transport failures.
- `cli`: map fatal/actionable/transient outcomes into command exit errors and user-facing reason/action text.

## Executable Mapping

The docs and code stay aligned through a small number of explicit classifier
entry points:

- `internal/failures`: shared `Class` and `LogOwner` definitions.
- `internal/synctypes/summary_keys.go`: shared `SummaryKey`,
  `SummaryDescriptor`, and the normalization helpers that map runtime results,
  persisted failures, and scope blocks into one rendering key.
- `internal/config/failure_class.go`: classify config load results into
  `success`, `actionable`, or `fatal`.
- `internal/sync/engine_result_classify.go`: classify each `WorkerResult` into
  a full `ResultDecision` carrying class, shared summary key, persistence
  mode, trial hint, scope evidence, and log ownership.
- `internal/cli/failure_class.go`: classify command-returned errors into exit
  behavior and reason/action text without inspecting raw transport payloads.

## Persistence Mapping

The durable projection of the error model is intentionally small:

- `retryable transient` -> `sync_failures.category='transient'`, `failure_role='item'`
- `scope-blocking transient` -> `scope_blocks` row plus `sync_failures.failure_role='held'` and `'boundary'` when the scope itself is durable; derived `perm:remote` uses only `held` blocked-write rows
- `actionable` -> `sync_failures.category='actionable'`
- `success` -> baseline/remote-state commit plus explicit failure cleanup where required
- `shutdown` and `fatal` -> returned to the caller unless a higher boundary intentionally converts them into one of the durable classes above

This keeps durable state as a record of policy decisions, not a copy of every
raw error string seen in the process.

## Boundary Rules

- Errors cross one classification boundary before being wrapped with local context.
- The boundary that understands the invariant owns the classification.
- Retry/backoff consumes the classified result; it does not classify on its own.
- User-facing messaging consumes the classified result; it does not inspect raw HTTP or filesystem payloads directly.
- Logging ownership follows the classified result (`failures.LogOwner`) instead
  of duplicate per-layer ad hoc logging.

## Verified By

| Boundary | Evidence |
|----------|----------|
| Shared failure classes | `internal/failures/failures_test.go`, `internal/config/failure_class_test.go`, `internal/cli/failure_class_test.go` |
| Shared summary key mapping and descriptors | `internal/synctypes/summary_keys_test.go`, `internal/cli/failure_display_test.go` (`TestGroupFailures_UsesSharedSummaryFallback`), `internal/sync/engine_result_scope_test.go` (`TestRecordFailure_LogsSummaryKey`, `TestProcessWorkerResult_EndToEndSummaryKey_ServiceOutage`, `TestProcessWorkerResult_EndToEndSummaryKey_SharedFolderWritesBlocked`, `TestProcessWorkerResult_EndToEndSummaryKey_AuthenticationRequired`, `TestProcessWorkerResult_EndToEndSummaryKey_LocalPermissionDenied`) |
| Sync result classification and persistence mapping | `internal/sync/engine_result_scope_test.go` (`TestClassifyResult_LifecycleAndAuth`, `TestClassifyResult_StorageScopes`, `TestDiskLocalScopeBlock_FullCycle`, `TestRetryPipeline_TransientFailure_IntegratedRetrier`) |
| Trial routing from classified decisions | `internal/sync/engine_result_scope_test.go` (`TestProcessTrialResultV2_Success_ClearsScope`, `TestProcessTrialResultV2_Preserve_LocalPermissionRecordsCandidateFailure`, `TestEvaluateTrialOutcome_OnlyMatchingScopeEvidenceExtends`) |
| CLI exit/presentation mapping | `internal/cli/failure_class_test.go`, `internal/cli/root_test.go` (`TestErrVerifyMismatch_IsSentinel`) |
| Read-only/persistent store projection of classified failures | `internal/syncstore/inspector_test.go` (`TestInspector_ReadIssuesSnapshot`, `TestInspector_ReadStatusSnapshot_IssueSummary`, `TestInspector_ReadStatusSnapshot_StaysConsistentWithIssuesSnapshot`), `internal/cli/issues_test.go` (`TestIssuesService_RunList_UsesReadOnlyInspector`), `internal/cli/status_test.go` (`TestQuerySyncState_WithMetadata`, `TestQuerySyncState_PendingSyncAndIssues`, `TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsIssues`) |
