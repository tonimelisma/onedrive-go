# Retry Architecture Transition — Requirements

Specific, testable requirements derived from the design analysis in `retry-transition-design.md`.

---

## R1. Failure Categories

- R1.1. `sync_failures.category` has exactly two values: `transient` and `actionable`.
- R1.2. `transient` means the system will auto-retry forever with exponential backoff. No user action needed.
- R1.3. `actionable` means the failure will never succeed without user intervention. No automatic retry.
- R1.4. The old `permanent` category value must not exist in code or schema (except migration SQL that renames it).
- R1.5. There is no escalation threshold. No failure count triggers a category change from `transient` to `actionable`. Transient failures remain transient regardless of how many times they fail.

## R2. Actionable Failure Classification

- R2.1. The following errors are classified as `actionable`: invalid filename, path too long (>400 chars), file too large (>250 GB), quota exceeded (507).
- R2.2. Permission denied (403) is handled by the dedicated permission subsystem (suppression + periodic recheck), not by the retry system.
- R2.3. All other errors (429, 500, 502, 503, 504, 509, 408, 412, 423, network errors, hash mismatches, 400 ObjectHandle) are classified as `transient`.
- R2.4. Actionable failures have `next_retry_at = NULL`. They are never re-injected by the reconciler.
- R2.5. Every actionable failure must have a human-readable reason and a specific user action (e.g., "invalid filename — rename the file").

## R3. Transient Retry Behavior

- R3.1. Transient failures are retried forever with exponential backoff: 30s base, 2x multiplier, 1h cap, ±25% jitter.
- R3.2. The backoff curve is: 30s → 1m → 2m → 4m → 8m → 16m → 32m → 1h → 1h → 1h → ...
- R3.3. After ~7 failures (~2 hours), every subsequent retry is spaced ~1 hour apart.
- R3.4. Items that have been failing longer are retried less frequently. Fresh work always takes priority in the buffer.
- R3.5. No recovery detection mechanism. When the API recovers, items resume retrying as their individual jittered backoff timers expire (spread over ~30 minutes at the 1h cap).

## R4. Retry Layers

- R4.1. Transport retry (`retry.Transport`): 5 attempts, 1-60s exponential backoff. Retries a single HTTP request on 429/5xx/network errors. Synchronous, blocks the worker.
- R4.2. Action retry (`retry.Action`): 3 attempts, 1-4s exponential backoff. Retries the entire operation (download/upload/delete). Synchronous, blocks the worker.
- R4.3. Reconciler retry (`retry.Reconcile`): Computes `next_retry_at` via `Reconcile.Delay(failureCount)`. Not a synchronous loop. SQL-driven, async.
- R4.4. `Policy.MaxAttempts` is only used as a synchronous loop bound by Transport and Action. The Reconcile policy sets `MaxAttempts = 0` (not applicable) and nothing reads it.
- R4.5. Transport and Action `MaxAttempts` must not be removed — without them, a failing file would monopolize a worker forever.

## R5. Circuit Breaker

- R5.1. The circuit breaker (`internal/retry/CircuitBreaker`) is not wired into the HTTP client or sync engine at runtime.
- R5.2. The circuit breaker library code (`circuit.go`, `circuit_test.go`) is retained in `internal/retry/` for potential future use.
- R5.3. `graph/client.go` has no `breaker` field, no `SetCircuitBreaker()` method, no `recordBreakerSuccess()`/`recordBreakerFailure()` calls.

## R6. Account-Wide Throttle Gate

- R6.1. When any HTTP request receives a 429 response with a `Retry-After` header, the `Retry-After` deadline is stored on the Graph client (mutex-protected).
- R6.2. Before every HTTP request, the client checks the stored deadline. If `now < throttledUntil`, the request sleeps until the deadline passes.
- R6.3. All workers sharing the same Graph client respect the same throttle deadline. No worker makes a request while the account is throttled.
- R6.4. The throttle gate only activates on 429 responses with `Retry-After`. No other error code triggers it.
- R6.5. After the deadline passes, requests proceed immediately. No probe or half-open state.

## R7. Table Separation

- R7.1. `remote_state` is a pure observation table. It has no failure-related columns (no `failure_count`, `next_retry_at`, `last_error`, `http_status`).
- R7.2. `sync_failures` owns the entire retry lifecycle for all directions (download, upload, delete).
- R7.3. `conflicts` contains only content conflicts: `edit_edit`, `edit_delete`, `create_create`. The `sync_failure` conflict type must not exist in the CHECK constraint or in any runtime code.
- R7.4. A failing item exists in exactly one place: `sync_failures`. There is no dual representation across tables.
- R7.5. When a failure is resolved (retry succeeds or user clears), `sync_failures` row is deleted AND `remote_state` status is reset to pending. No orphaned records.

## R8. No EscalateToConflict

- R8.1. The `EscalateToConflict()` method does not exist.
- R8.2. The `ConflictEscalator` interface does not exist.
- R8.3. The `ConflictSyncFailure` constant does not exist.
- R8.4. The reconciler never creates conflict records. Conflicts are created only by the planner when it detects genuine content version conflicts.

## R9. Unified `issues` CLI Command

- R9.1. `onedrive-go issues` lists everything needing user attention: unresolved conflicts AND actionable failures. Single command, single view.
- R9.2. `onedrive-go issues resolve [id] --keep-local|--keep-remote|--keep-both` resolves a content conflict.
- R9.3. `onedrive-go issues clear [path]` dismisses an actionable failure after the user has fixed the underlying problem.
- R9.4. `onedrive-go issues clear --all` dismisses all actionable failures.
- R9.5. The `issues` output groups items into sections: CONFLICTS and FILE ISSUES. Each item shows a specific reason and what the user should do.
- R9.6. Transient failures never appear in `issues` output.
- R9.7. The `conflicts` command does not exist — not even as a hidden alias.
- R9.8. The `failures` command does not exist — not even as a hidden alias.
- R9.9. The `resolve` top-level command does not exist — not even as a hidden alias. Resolve is a subcommand of `issues`.
- R9.10. No backward-compat aliases exist. Pre-1.0 software has no backward-compat obligations.

## R10. Status Display

- R10.1. `status` shows a single "Issues" count = unresolved conflicts + actionable failures.
- R10.2. `status` shows a "Retrying" count = transient failures with `failure_count >= 3` (below 3 is invisible).
- R10.3. **Not yet implemented.** The retrying count is currently a simple item count with no scope breakdown. Target: scope context showing service-wide (e.g., "503 Service Unavailable since 2h ago"), account-wide, or file-scoped. See B-343.
- R10.4. **Not yet implemented.** Target: service-wide errors collapsed in display — "47 items (503 Service Unavailable since 2h ago)" not 47 individual lines. See B-343.
- R10.5. Short-term transient failures (fewer than 3 reconciler-level attempts) are invisible in all user-facing output.

## R11. Transient Failure Scope Classification

> **Not yet implemented.** The following requirements describe the target state for scope-classified failure display and retry policies. See B-343 for the planned implementation.

- R11.1. Service-wide errors: 500, 502, 503, 504, 509, network timeout, 400 ObjectHandle. Detected by multiple items failing with the same HTTP status.
- R11.2. Account-wide errors: 429 (throttled), 401 (auth expired), 507 (quota full). Inherently per-account.
- R11.3. Folder-scoped errors: 403 (permission denied). Detected by the permission subsystem.
- R11.4. File-scoped errors: 423 (locked), hash mismatch, 412 (ETag conflict). Only affects one file.

## R12. Schema Migrations

- R12.1. Incremental migrations 00002-00006 are deleted. The consolidated schema (`00001_consolidated_schema.sql`) is the single source of truth.
- R12.2. The consolidated schema has: no failure columns in `remote_state`, `sync_failures` with `category IN ('transient', 'actionable')`, `conflicts` CHECK constraint excluding `sync_failure`.
- R12.3. No `local_issues` table exists in the schema.

## R13. Naming Cleanup

- R13.1. No Go source file (excluding migrations) references `ConflictSyncFailure`, `EscalateToConflict`, `ConflictEscalator`, `newResolveCmd`, `newConflictsCmd`, or `newFailuresCmd`.
- R13.2. No Go source file (excluding migrations and test migration helpers) references `local_issues`.
- R13.3. All test functions named `TestRecordLocalIssue_*` are renamed to `TestRecordSyncFailure_*`.
- R13.4. The helper `newTestSyncStoreForIssues()` is renamed to `newTestSyncStoreForFailures()`.
- R13.5. `isPermanentIssue()` is renamed to `isActionableIssue()`.
- R13.6. `MarkSyncFailurePermanent()` is renamed to `MarkSyncFailureActionable()`.
- R13.7. All comments referencing `local_issues` are updated to reference `sync_failures`.

## R14. Reconciler Simplification

- R14.1. The reconciler's only job is: find transient failures whose `next_retry_at <= now`, skip in-flight items, synthesize events, re-inject into the buffer.
- R14.2. The reconciler has no escalation threshold, no direction-based branching, no conflict creation.
- R14.3. `defaultEscalationThreshold`, `FailureRetrierConfig`, and `DefaultFailureRetrierConfig()` do not exist.
- R14.4. The `FailureRetrier` constructor does not accept a `ConflictEscalator` parameter.

## R15. Backpressure

- R15.1. The worker pool (fixed size, configurable) is the primary concurrency limiter. At most N items are processed concurrently.
- R15.2. Transport-level backoff (1-60s) limits per-worker request rate during failures.
- R15.3. Per-item backoff in `sync_failures` (30s-1h) limits how frequently long-failing items are re-injected.
- R15.4. The 429 throttle gate (R6) pauses all workers when the account is rate-limited.
- R15.5. No other backpressure mechanism exists at runtime (no circuit breaker on the hot path).
