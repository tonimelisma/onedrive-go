# Temporary Data Model Field Audit

Historical note: this audit captured the pre-cleanup sync-store schema before
generation 12 removed `sync_status`,
`observation_state.remote_refresh_mode`,
`observation_state.last_full_remote_refresh_at`, and
`local_state.content_identity`. References to those deleted fields below are
intentionally historical.

This is a blunt audit of the current SQLite sync store.

Method:

- I only counted non-test code paths.
- `Critical` means I found a current read path that changes sync behavior or startup behavior.
- `CLI surfaced` means the field is shown to the user today.
- `At risk` means I could not find a current material read, or I only found schema/singleton plumbing or narrow metadata-preservation fallbacks.

## `store_metadata`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `singleton_id` | At risk | Current code uses it only to address the singleton row. Removing it would require rewriting the store metadata queries, but I found no sync or CLI behavior that depends on the value itself. | `internal/sync/schema.go` `sqlEnsureStoreMetadataRow`, `readStoreCompatibilityMetadata` |
| `schema_generation` | Critical | Startup compatibility checks would stop being able to reject stale/incompatible state DBs. | `internal/sync/schema.go` `validateStoreCompatibilityMetadata` |

## `baseline`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `item_id` | Critical | Remote move detection, action targeting, and parent resolution for existing remote items break because baseline identity is item-ID-based. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go`, `internal/sync/executor.go` `ResolveParentID` |
| `path` | Critical | The planner’s comparison/reconciliation SQL and the in-memory baseline cache are path-keyed; dropping it breaks nearly every sync decision. | `internal/sync/sqlite_compare.go`, `internal/sync/store_write_baseline.go` `Load` |
| `parent_id` | Critical | Remote observation uses baseline parent IDs as a fallback when delta items omit `parent_id`, and upload outcome construction falls back to it too. | `internal/sync/item_converter.go` `registerInflight`, `internal/sync/executor_transfer.go` `resolvedUploadParentID` |
| `item_type` | Critical | File-vs-folder reconciliation and execution branching break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `local_hash` | Critical | Local change detection, local move detection, and baseline hash reuse break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go`, `internal/sync/local_hash_reuse.go`, `internal/sync/single_path.go` |
| `remote_hash` | Critical | Remote change detection and download/upload conflict decisions break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `local_size` | Critical | Local change detection and baseline hash reuse fallbacks break when hashes are absent or reused. | `internal/sync/sqlite_compare.go`, `internal/sync/local_hash_reuse.go`, `internal/sync/planner.go` |
| `remote_size` | Critical | Remote change detection and local-vs-remote equality checks break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `local_mtime` | Critical | Local change detection and baseline hash reuse break. | `internal/sync/sqlite_compare.go`, `internal/sync/local_hash_reuse.go`, `internal/sync/planner.go` |
| `remote_mtime` | Critical | Remote change detection and durable remote-drift detection break. | `internal/sync/sqlite_compare.go`, `internal/sync/store_inspect.go` `sqlCountRemoteDriftItems` |
| `etag` | Critical | Remote change detection and the overwrite upload freshness guard break. | `internal/sync/sqlite_compare.go`, `internal/sync/executor_transfer.go` `ExecuteUpload` |

## `observation_state`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `singleton_id` | At risk | It only anchors the singleton row. Removing it needs query rewrites, but I found no sync/CLI behavior attached to the value itself. | `internal/sync/store_observation_state.go` |
| `configured_drive_id` | Critical | The store would lose its DB-to-drive ownership check and its fallback drive ID for baseline/remote reads. | `internal/sync/store_observation_state.go` `ensureMatchingConfiguredDriveID`, `configuredDriveIDForRead`; `internal/sync/engine_current_projection.go` |
| `cursor` | Critical | Delta observation resume breaks and watch startup falls back to empty-token behavior. | `internal/sync/engine_current_observe.go` `observeRemote`, `internal/sync/engine_primary_root_watch.go`, `internal/sync/engine_shared_root.go` |
| `remote_refresh_mode` | Critical | Full remote refresh cadence changes because the engine reads this persisted mode to compute the interval. | `internal/sync/engine_watch_maintenance.go`, `internal/sync/store_observation_state.go` |
| `last_full_remote_refresh_at` | Critical | The engine uses it to recover the next refresh deadline when `next_full_remote_refresh_at` is empty. | `internal/sync/engine_watch_maintenance.go` `fullRemoteRefreshDelay` |
| `next_full_remote_refresh_at` | Critical | The engine uses it to decide when a full remote refresh is due. | `internal/sync/engine_watch_maintenance.go` `shouldRunFullRemoteRefresh`, `armFullRefreshTimer` |
| `local_refresh_mode` | At risk | I found one non-test reread before re-writing local refresh state, but no scheduler or CLI logic that materially changes behavior from the persisted value. | `internal/sync/engine_current_observe.go` `commitObservedLocalSnapshot` |
| `last_full_local_refresh_at` | At risk | I found writes, but no non-test reader that consults it. | `internal/sync/store_observation_state.go` `MarkFullLocalRefresh` |
| `next_full_local_refresh_at` | At risk | I found writes, but no non-test reader that consults it. | `internal/sync/store_observation_state.go` `MarkFullLocalRefresh` |

## `sync_status`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `singleton_id` | At risk | It only anchors the singleton row. Removing it needs query rewrites, but the value itself has no sync or CLI meaning. | `internal/sync/store_sync_status.go` |
| `last_synced_at` | CLI surfaced | `status` would stop showing the last successful sync time. | `internal/sync/store_inspect.go` `ReadDriveStatusSnapshot`; `internal/cli/status_sync_state.go` |
| `last_sync_duration_ms` | CLI surfaced | `status` would stop showing the last successful sync duration. | `internal/sync/store_inspect.go` `ReadDriveStatusSnapshot`; `internal/cli/status_sync_state.go` |
| `last_succeeded_count` | At risk | It is persisted and read into `DriveStatusSnapshot`, but current CLI rendering never surfaces it and I found no engine decision that uses it. | `internal/sync/store_inspect.go`, `internal/cli/status_sync_state.go` |
| `last_failed_count` | At risk | Same story as `last_succeeded_count`: persisted, loaded, but not surfaced or used for control flow. | `internal/sync/store_inspect.go`, `internal/cli/status_sync_state.go` |
| `last_error` | CLI surfaced | `status` would stop showing the last sync error string. | `internal/sync/store_inspect.go` `ReadDriveStatusSnapshot`; `internal/cli/status_sync_state.go`, `internal/cli/status_render.go` |

## `remote_state`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `drive_id` | Critical | Cross-drive/shared-root targeting breaks because planner action targeting prefers the remote row’s owning drive. | `internal/sync/store_read_remote_state.go`, `internal/sync/planner.go` `view.Remote.DriveID` handling, `resolveActionTargetDriveID`, target-root helpers |
| `item_id` | Critical | Remote move detection, delete detection, and action targeting break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `path` | Critical | The planner joins current remote truth by path; durable remote drift counting also depends on it. | `internal/sync/sqlite_compare.go`, `internal/sync/store_inspect.go` `sqlCountRemoteDriftItems` |
| `parent_id` | At risk | I found live fallback use through `remoteStateFromSnapshotRow` into download/upload outcome construction, but no planner or CLI decision that directly depends on persisted remote parent IDs. | `internal/sync/planner_sqlite.go` `remoteStateFromSnapshotRow`, `internal/sync/executor_transfer.go` `downloadOutcome`, `resolvedUploadParentID` |
| `item_type` | Critical | File-vs-folder planning and current-equality checks break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `hash` | Critical | Remote change detection and local-vs-remote equality checks break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `size` | Critical | Remote change detection and equality checks break when size is the deciding fact. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `mtime` | Critical | Remote change detection and durable remote-drift detection break. | `internal/sync/sqlite_compare.go`, `internal/sync/store_inspect.go` `sqlCountRemoteDriftItems` |
| `etag` | Critical | Remote change detection breaks when hashes are absent, and upload freshness checks lose the last known remote ETag. | `internal/sync/sqlite_compare.go`, `internal/sync/executor_transfer.go` |
| `content_identity` | At risk | I found store read/write plumbing and tests, but no non-test consumer that changes behavior from the persisted remote value. | `internal/sync/store_read_remote_state.go`, `internal/sync/store_write_observation.go` |
| `previous_path` | At risk | It is written on moves, but I found no non-test reader. | `internal/sync/store_write_observation.go`, `internal/sync/store_write_baseline.go` `updateRemoteStateOnOutcome` |

## `local_state`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `path` | Critical | The planner’s comparison/reconciliation SQL is path-keyed. | `internal/sync/sqlite_compare.go` |
| `item_type` | Critical | File-vs-folder planning and equality checks break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `hash` | Critical | Local change detection and local-vs-remote equality checks break. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `size` | Critical | Local change detection and equality checks break when size is the deciding fact. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `mtime` | Critical | Local change detection breaks when hash is absent. | `internal/sync/sqlite_compare.go`, `internal/sync/planner.go` |
| `content_identity` | Critical | Local move detection breaks because the comparison SQL explicitly prefers `content_identity` over `hash` for matching renamed local content. | `internal/sync/sqlite_compare.go` `local_move_sources` |

## `retry_work`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `work_key` | At risk | It is the physical PK today, but semantic identity is re-derived everywhere from `(path, old_path, action_type)` rather than from the stored string. | `internal/sync/store_retry_work.go` `serializeRetryWorkKey`, `retryWorkKey`, `ResolveRetryWork`, `PruneRetryWorkToCurrentActions` |
| `path` | Critical | Retry rows are matched, pruned, grouped, and released by path. | `internal/sync/store_retry_work.go`, `internal/sync/blocked_retry_projection.go`, `internal/sync/engine_runtime_admission.go` |
| `old_path` | Critical | Move retries lose identity without it. | `internal/sync/store_retry_work.go` `retryWorkKey`, `ResolveRetryWork`, `DeleteRetryWorkByWork` |
| `action_type` | Critical | Retry rows lose their semantic work identity and cannot be resolved back to actions. | `internal/sync/store_retry_work.go` `retryWorkKey`, `ResolveRetryWork`, `PruneRetryWorkToCurrentActions` |
| `condition_type` | At risk | I found persisted/logging context, but no planner, scheduler, or CLI decision that depends on the stored value after persistence. | `internal/sync/store_retry_work.go`, `internal/sync/engine_retry_trial.go`, `internal/sync/engine_runtime_scopes.go` |
| `scope_key` | Critical | Blocked-work grouping, release, discard, invariant checking, and CLI condition grouping break. | `internal/sync/store_retry_work.go`, `internal/sync/engine_runtime_admission.go`, `internal/sync/engine_runtime_scopes.go`, `internal/sync/blocked_retry_projection.go` |
| `blocked` | Critical | The engine would stop distinguishing ready retry work from scope-blocked work. | `internal/sync/store_retry_work.go`, `internal/sync/engine_runtime_admission.go`, `internal/sync/store_inspect.go` |
| `attempt_count` | Critical | Backoff progression and the `status` retrying count break. | `internal/sync/store_retry_work.go` `RecordRetryWorkFailure`, `CountRetryingWork`; `internal/sync/store_inspect.go` |
| `next_retry_at` | Critical | Retry scheduling after restart breaks because held retry work would lose its next eligible time. | `internal/sync/store_retry_work.go`, `internal/sync/engine_runtime_admission.go`, `internal/sync/engine_runtime_held.go` |
| `last_error` | At risk | I found persistence, but no non-test read that changes engine behavior or CLI output from the stored string. | `internal/sync/store_retry_work.go` |
| `http_status` | At risk | HTTP status drives classification before persistence, but I found no post-persistence reader that reclassifies or schedules from the stored value. | `internal/sync/engine_result_classify.go`, `internal/sync/store_retry_work.go` |
| `first_seen_at` | At risk | I found persistence, but no non-test reader that uses it for behavior or CLI output. | `internal/sync/store_retry_work.go` |
| `last_seen_at` | At risk | Same as `first_seen_at`: persisted, not currently used for behavior or CLI output. | `internal/sync/store_retry_work.go` |

## `observation_issues`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `path` | Critical | Truth blocking and issue reconciliation are keyed by path. | `internal/sync/store_observation_issues.go`, `internal/sync/truth_status.go`, `internal/sync/observation_reconcile_policy.go` |
| `action_type` | At risk | It is persisted and validated, but I found no non-test read-side logic that depends on it. | `internal/sync/store_observation_issues.go` |
| `issue_type` | Critical | CLI condition grouping and truth blocking both depend on it. | `internal/sync/condition_projection.go`, `internal/sync/truth_status.go`, `internal/cli/status_condition_descriptors.go` |
| `item_id` | At risk | I found persistence, but no non-test read that uses the stored item ID. | `internal/sync/store_observation_issues.go` |
| `last_error` | At risk | Persisted text only; I found no non-test read that changes behavior or CLI output from it. | `internal/sync/store_observation_issues.go` |
| `first_seen_at` | At risk | I found persistence, but no non-test consumer. | `internal/sync/store_observation_issues.go` |
| `last_seen_at` | At risk | It currently affects SQL ordering, but I found no downstream non-test behavior or CLI output that depends on that order. | `internal/sync/store_observation_issues.go`, `internal/sync/condition_projection.go` |
| `file_size` | At risk | Stored from observation findings, but I found no non-test read that uses it. | `internal/sync/observation_findings.go`, `internal/sync/store_observation_issues.go` |
| `local_hash` | At risk | Stored from observation findings, but I found no non-test read that uses it. | `internal/sync/observation_findings.go`, `internal/sync/store_observation_issues.go` |
| `scope_key` | Critical | Read-boundary truth blocking and CLI scope grouping break without it. | `internal/sync/truth_status.go`, `internal/sync/condition_projection.go`, `internal/cli/status_condition_descriptors.go` |

## `block_scopes`

| Field | Verdict | Material impact if dropped | Evidence |
| --- | --- | --- | --- |
| `scope_key` | Critical | Active-scope identity, blocked-work linkage, restart recovery, and CLI grouping break. | `internal/sync/block_scope_rows.go`, `internal/sync/engine_runtime_workset.go`, `internal/sync/engine_runtime_scopes.go` |
| `blocked_at` | At risk | It is persisted and copied into runtime scope structs, but I found no non-test decision that branches on it. | `internal/sync/block_scope_rows.go`, `internal/sync/scope_block.go` |
| `trial_interval` | Critical | Trial rearm timing breaks. | `internal/sync/engine_runtime_scopes.go` `rearmScopeTrial`, `extendScopeTrial`; `internal/sync/active_scopes.go` |
| `next_trial_at` | Critical | Due-trial scheduling and timer arming break. | `internal/sync/engine_runtime_workset.go`, `internal/sync/active_scopes.go`, `internal/sync/engine_watch_state.go` |

## Detailed Notes For At-Risk Fields

This section is the deeper keep-or-drop memo for the fields marked `At risk`.
For each one, I traced the current write path, the current non-test reread path,
what behavior that reread participates in, and what would really have to change
to remove the field cleanly.

### `store_metadata.singleton_id`

- Current writes: schema bootstrap writes `singleton_id = 1` through `createCanonicalSchema -> ensureStoreCompatibilityMetadata -> sqlEnsureStoreMetadataRow` in `internal/sync/schema.go`.
- Current reads: startup compatibility validation reads `schema_generation` with `SELECT ... WHERE singleton_id = 1` in `readStoreCompatibilityMetadata`.
- Runtime effect today: startup uses the row, but not the value of this field. The field is just the addressing mechanism for the one-row table.
- If dropped: sync behavior does not change, but schema/bootstrap/validation SQL must be rewritten to use some other singleton-row pattern.
- Decision signal: weak field. It is schema plumbing, not product behavior.

### `observation_state.singleton_id`

- Current writes: `sqlEnsureObservationStateRow` and `sqlUpsertObservationState` always insert or update the row with `singleton_id = 1` in `internal/sync/store_observation_state.go`.
- Current reads: `ReadObservationState`, `readObservationStateTx`, and `configuredDriveIDForDB` all query `WHERE singleton_id = 1`.
- Runtime effect today: watch startup, cursor commits, refresh cadence reads, and drive-ID ownership checks all depend on the row existing, but not on this field carrying information beyond "this is the singleton row".
- If dropped: every observation-state query and upsert needs to change, but no engine branch or CLI output would change because of the missing value itself.
- Decision signal: weak field. Same singleton-anchor story as `store_metadata.singleton_id`.

### `observation_state.local_refresh_mode`

- Current writes:
  - `commitObservedLocalSnapshot` in `internal/sync/engine_current_observe.go` ends by calling `MarkFullLocalRefresh`.
  - watch startup writes `watch_healthy` in `startObservers` in `internal/sync/engine_watch.go`.
  - the local observer safety scan writes `watch_healthy` again through `AfterSafetyScan`.
  - watch-limit fallback writes `watch_degraded`, and `runPeriodicFullScan` keeps writing `watch_degraded` after each degraded scan.
- Current reads:
  - I found one persisted reread in `commitObservedLocalSnapshot`: it calls `ReadObservationState`, copies `state.LocalRefreshMode`, then passes that mode back into `MarkFullLocalRefresh`.
  - I did not find a local equivalent of the remote-refresh scheduler reads in `fullRemoteRefreshDelay`, `shouldRunFullRemoteRefresh`, or `armFullRefreshTimer`.
- Runtime effect today: the persisted field preserves whatever mode was already in the DB when a one-shot local snapshot is committed, but the actual watch runtime chooses healthy vs degraded directly from explicit code paths in `engine_watch.go`, not by reloading this field on startup and scheduling from it.
- If dropped: `commitObservedLocalSnapshot` would need to stop rereading and re-emitting the stored mode. The watch runtime could keep using explicit healthy/degraded literals exactly as it does now.
- Decision signal: strong drop candidate. There is a persisted reread, but I did not find a material runtime decision that depends on the stored value.

### `observation_state.last_full_local_refresh_at`

- Current writes: `applyLocalRefreshSchedule` sets it, and `MarkFullLocalRefresh` persists it in `internal/sync/store_observation_state.go`.
- Current reads: I did not find a non-test consumer that branches on this field after it is read from SQLite.
- Runtime effect today: it is written whenever a local full refresh is marked, but I did not find scheduling, admission, planning, or CLI code that consults it.
- If dropped: remove it from `ObservationState`, the schema, the local refresh schedule helper, and the read/write SQL. No known runtime behavior would need replacement.
- Decision signal: strong drop candidate.

### `observation_state.next_full_local_refresh_at`

- Current writes: `applyLocalRefreshSchedule` computes it from `localRefreshIntervalForMode`, and `MarkFullLocalRefresh` persists it.
- Current reads: I did not find a non-test consumer that uses it to decide whether a local refresh is due.
- Runtime effect today: unlike `next_full_remote_refresh_at`, this field does not appear in any restart scheduling path. Local degraded-scan timing is driven directly by `runPeriodicFullScan(..., localRefreshIntervalForMode(localRefreshModeWatchDegraded))` in `internal/sync/engine_watch.go`.
- If dropped: remove it from schema and the observation-state helpers. No local timer arming logic appears to need a replacement because I did not find any code arming from this stored deadline.
- Decision signal: strong drop candidate.

### `sync_status.singleton_id`

- Current writes: `sqlEnsureSyncStatusRow` seeds a singleton row with `singleton_id = 1`, and `WriteSyncStatus` updates `WHERE singleton_id = 1`.
- Current reads: `ReadSyncStatus` and `storeInspector.ReadDriveStatusSnapshot` query `WHERE singleton_id = 1`.
- Runtime effect today: the status row is real product data, but this field is only the singleton-row anchor.
- If dropped: update the schema and status queries, but no engine or CLI behavior changes because the value is gone.
- Decision signal: weak field. Pure row-addressing scaffolding.

### `sync_status.last_succeeded_count`

- Current writes: `syncStatusFromUpdate` in `internal/sync/engine_run_once_report.go` copies `update.Succeeded` into `SyncStatus.LastSucceededCount`, and `WriteSyncStatus` persists it.
- Current reads:
  - `ReadSyncStatus` scans it.
  - `storeInspector.ReadDriveStatusSnapshot` scans it into `DriveStatusSnapshot`.
  - `buildSyncStateInfo` in `internal/cli/status_sync_state.go` does not use it.
- Runtime effect today: persisted and loaded, but dropped by the CLI shaping layer before rendering. I did not find engine control flow that rereads it.
- If dropped: remove it from the schema, `SyncStatus`, `DriveStatusSnapshot` scan, and `syncStatusFromUpdate`.
- Decision signal: strong drop candidate. This is stored status detail with no current downstream consumer.

### `sync_status.last_failed_count`

- Current writes: `syncStatusFromUpdate` copies `update.Failed` into `SyncStatus.LastFailedCount`, and `WriteSyncStatus` persists it.
- Current reads:
  - `ReadSyncStatus` scans it.
  - `storeInspector.ReadDriveStatusSnapshot` scans it.
  - `buildSyncStateInfo` does not use it.
- Runtime effect today: same pattern as `last_succeeded_count`. It survives persistence and inspection, but current CLI status output ignores it.
- If dropped: same cleanup as `last_succeeded_count`.
- Decision signal: strong drop candidate.

### `remote_state.parent_id`

- Current writes:
  - remote observation writes it through `CommitObservation -> processObservedItem -> insertRemoteState` or `updateRemoteStateFromObs` in `internal/sync/store_write_observation.go`.
  - execution writes it through `updateRemoteStateOnOutcome` for upload and folder-create outcomes in `internal/sync/store_write_baseline.go`.
- Current reads:
  - remote snapshot reads scan it in `internal/sync/store_read_remote_state.go`.
  - `Planner.buildSQLitePathViews` calls `remoteStateFromSnapshotRow`, which copies it into `view.Remote.ParentID` in `internal/sync/planner_sqlite.go`.
  - `Executor.downloadOutcome` copies `action.View.Remote.ParentID` into the successful `ActionOutcome`.
  - `resolvedUploadParentID` falls back to `action.View.Remote.ParentID` before falling back again to `action.View.Baseline.ParentID`.
- Runtime effect today: this is not a primary planner key, but it is a live fallback/metadata-preservation field in execution outcomes. Dropping it would make some successful download/upload outcomes lose a persisted parent ID unless baseline data covers the case.
- If dropped: audit all places that consume `ActionOutcome.ParentID` and decide whether baseline-only fallback is acceptable. This is not the same class as a write-only dead field.
- Decision signal: borderline. I would not drop this without an explicit replacement story for outcome parent ID.

### `remote_state.content_identity`

- Current writes:
  - the schema includes the column in `internal/sync/schema.go`.
  - the read SQL selects it in `internal/sync/store_read_remote_state.go`.
  - but the live write paths do not populate it: `sqlInsertRemoteState` and `sqlUpdateRemoteState` in `internal/sync/store_write_observation.go` omit the column, and `updateRemoteStateOnOutcome` in `internal/sync/store_write_baseline.go` omits it too.
- Current reads:
  - `store_read_remote_state.go` scans it into `RemoteStateRow.ContentIdentity`.
  - `remoteStateFromSnapshotRow` does not map it into the runtime `RemoteState` used by planner/executor code.
  - the only non-test consumer I found after scan is scratch-store cloning in `internal/sync/store_scratch.go`, which copies the entire remote row shape into the dry-run scratch DB.
- Runtime effect today: I found no current sync behavior or CLI behavior that depends on the persisted value. More strongly: current production write paths do not populate the field in the first place.
- If dropped: remove it from the schema, row structs, select lists, and scratch-store seed/copy code. Current engine behavior should not change.
- Decision signal: strongest drop candidate in the whole audit.

### `remote_state.previous_path`

- Current writes:
  - remote observation writes it when a remote item changes path: `observedRemoteStateUpdate` returns `previousPath`, and `updateRemoteStateFromObs` persists it in `internal/sync/store_write_observation.go`.
  - execution writes it for remote moves in `updateRemoteStateOnOutcome` in `internal/sync/store_write_baseline.go`.
- Current reads:
  - `store_read_remote_state.go` scans it into `RemoteStateRow.PreviousPath`.
  - scratch-store seed/copy reads and re-inserts it in `internal/sync/store_scratch.go`.
  - I did not find planner, executor, truth-status, retry, or CLI code that uses the scanned value.
- Runtime effect today: it is durable historical metadata about the last known path transition, but I did not find a non-test behavioral consumer.
- If dropped: remove it from schema, row structs, observation writes, execution writes, and scratch-store copy code.
- Decision signal: very strong drop candidate.

### `retry_work.work_key`

- Current writes:
  - `upsertRetryWorkTx` in `internal/sync/store_retry_work.go` computes it from `serializeRetryWorkKey(retryWorkKey(path, old_path, action_type))` if the caller did not fill it in.
  - the table uses it as the primary key and `ON CONFLICT` target.
- Current reads:
  - list queries select and order by `work_key`.
  - `scanRetryWorkRow` loads it into `RetryWorkRow`.
  - but runtime identity is re-derived from `path`, `old_path`, and `action_type`: `retryWorkKeyForRetryWork`, `ResolveRetryWork`, `DeleteRetryWorkByWork`, and `PruneRetryWorkToCurrentActions` all operate on the composite semantic key, not on the stored string.
  - `initializePreparedRuntime` in `internal/sync/engine_runtime_workset.go` rebuilds its map key from the row fields, not from `row.WorkKey`.
- Runtime effect today: the stored string matters to SQLite row identity and ordering, but not to sync semantics after rows are loaded.
- If dropped: the table needs a new primary key or unique index on `(path, old_path, action_type)`, and list ordering would need to stop using `work_key`.
- Decision signal: real schema work, low product risk. Good candidate if you want a cleaner composite-key table.

### `retry_work.condition_type`

- Current writes: `RecordRetryWorkFailure` persists it from `RetryWorkFailure.ConditionType`, and blocked retry persistence writes the scope-derived condition type in `persistBlockedRetryWork`.
- Current reads:
  - `scanRetryWorkRow` loads it.
  - `resolveRetryWorkAndLogResolution` logs `row.ConditionType` when a retry row is resolved.
  - `recordRetryWorkFailure` in `internal/sync/engine_runtime_permissions.go` logs `row.ConditionType` after persistence.
  - `engine_retry_trial.go` logs `row.ConditionType` when a held retry row is turned back into a dispatchable action.
- Runtime effect today: I found logging/observability use, not planning, scheduling, admission, or CLI grouping use. Stored retry grouping uses `scope_key`; retry identity uses `path`, `old_path`, `action_type`.
- If dropped: log lines lose a persisted condition label unless they recompute it from `scope_key` or from the in-memory result that caused the failure.
- Decision signal: moderate drop candidate if persisted retry diagnostics are not valued.

### `retry_work.last_error`

- Current writes: `RecordRetryWorkFailure` persists `RetryWorkFailure.LastError`; blocked retry persistence stores a synthetic `"blocked by scope: ..."` string.
- Current reads:
  - `scanRetryWorkRow` loads it.
  - I did not find a downstream non-test consumer beyond that scan.
- Runtime effect today: retry rows keep text about why they exist, but current engine logic does not reread that text to classify, schedule, or render anything user-facing.
- If dropped: remove it from schema and retry row structs; current retry behavior should stay the same.
- Decision signal: strongest drop candidate in `retry_work`.

### `retry_work.http_status`

- Current writes: `RecordRetryWorkFailure` persists the HTTP status captured on the original failure.
- Current reads:
  - `scanRetryWorkRow` loads it.
  - I did not find a post-persistence consumer.
  - the classification decisions happen before persistence in `internal/sync/engine_result_classify.go` from the live `ActionCompletion.HTTPStatus`, not from the stored row.
- Runtime effect today: retained as historical context only.
- If dropped: remove it from schema and row structs. Retry timing still comes from `attempt_count`, `blocked`, `scope_key`, and `next_retry_at`.
- Decision signal: strongest drop candidate in `retry_work`.

### `retry_work.first_seen_at`

- Current writes: `RecordRetryWorkFailure` initializes it on first failure and preserves the original timestamp on subsequent updates.
- Current reads:
  - `scanRetryWorkRow` loads it.
  - I did not find any non-test read after that.
- Runtime effect today: no scheduling, pruning, status, or CLI behavior depends on it.
- If dropped: remove it from schema and row structs. No replacement path appears necessary.
- Decision signal: strongest drop candidate.

### `retry_work.last_seen_at`

- Current writes: `RecordRetryWorkFailure` updates it every time the same retry row is re-recorded.
- Current reads:
  - `scanRetryWorkRow` loads it.
  - I did not find any non-test consumer after scan.
- Runtime effect today: no engine decision uses it; `next_retry_at` is the actual retry timer.
- If dropped: same cleanup as `first_seen_at`.
- Decision signal: strongest drop candidate.

### `observation_issues.action_type`

- Current writes:
  - local skipped-item observation writes `ActionUpload` through `appendSkippedObservationFinding` in `internal/sync/observation_findings.go`.
  - remote read-denied observation writes `ActionDownload` through `remoteReadDeniedObservationBatch`.
  - `upsertObservationIssuesTx` persists the value.
- Current reads:
  - `scanObservationIssueRow` loads it.
  - current read-side consumers do not use it: `TruthAvailabilityIndex` only branches on `IssueType`, `Path`, and `ScopeKey`; `ProjectStoredConditionGroups` groups by `IssueType` and `ScopeKey`.
- Runtime effect today: stored provenance detail only. It does not currently affect truth blocking or CLI grouping.
- If dropped: remove validation and persistence plumbing in `store_observation_issues.go` and remove the field from `ObservationIssue`/`ObservationIssueRow`.
- Decision signal: very strong drop candidate.

### `observation_issues.item_id`

- Current writes:
  - the schema and upsert path support it.
  - I did not find a current observation producer that fills it. The only live constructors I found in `internal/sync/observation_findings.go` populate `Path`, `ActionType`, `IssueType`, `Error`, `FileSize`, and sometimes `ScopeKey`, but not `ItemID`.
- Current reads:
  - `scanObservationIssueRow` loads it.
  - I did not find a downstream non-test consumer.
- Runtime effect today: I found neither a producer using it meaningfully nor a consumer rereading it.
- If dropped: remove it from schema, structs, and observation-issue scan/upsert SQL.
- Decision signal: strongest drop candidate in `observation_issues`.

### `observation_issues.last_error`

- Current writes:
  - local skipped items write `item.Detail` into `ObservationIssue.Error`, which `upsertObservationIssuesTx` persists as `last_error`.
  - remote read-denied observation persists `err.Error()`.
- Current reads:
  - `scanObservationIssueRow` loads it.
  - I did not find truth-status, condition projection, or CLI rendering code that uses the persisted string.
- Runtime effect today: durable diagnostic text only.
- If dropped: current truth blocking and condition grouping continue to work because they do not depend on the message text.
- Decision signal: very strong drop candidate.

### `observation_issues.first_seen_at`

- Current writes: `upsertObservationIssuesTx` sets it to `nowNano` on insert/upsert.
- Current reads:
  - `scanObservationIssueRow` loads it.
  - I did not find any non-test consumer after scan.
- Runtime effect today: none found beyond persistence.
- If dropped: remove it from schema and scan/upsert plumbing.
- Decision signal: very strong drop candidate.

### `observation_issues.last_seen_at`

- Current writes: `upsertObservationIssuesTx` refreshes it to `nowNano` on each upsert.
- Current reads:
  - the list queries order rows by `last_seen_at DESC` in `queryObservationIssueRowsWithRunner` and `ListObservationIssues`.
  - after the rows are loaded, the real consumers do not care about that order: `TruthAvailabilityIndex` indexes by path and read-boundary specificity, and `ProjectStoredConditionGroups` groups then re-sorts by condition/scope/path.
- Runtime effect today: it affects raw SQL output ordering, but I did not find a material behavior or user-visible rendering that depends on that order surviving downstream.
- If dropped: change the observation-issue list queries and index definition; no core truth-blocking logic appears to need a replacement.
- Decision signal: strong drop candidate.

### `observation_issues.file_size`

- Current writes: local skipped-item observation populates it from `SkippedItem.FileSize` in `appendSkippedObservationFinding`.
- Current reads:
  - `scanObservationIssueRow` loads it.
  - I did not find a downstream non-test consumer.
- Runtime effect today: none found after persistence.
- If dropped: observation issue truth blocking continues to work because it keys off path/issue/scope, not file size.
- Decision signal: very strong drop candidate.

### `observation_issues.local_hash`

- Current writes:
  - the schema and upsert path support it.
  - I did not find a current observation producer that sets it. `appendSkippedObservationFinding` does not populate `LocalHash`, and the remote read-denied path does not either.
- Current reads:
  - `scanObservationIssueRow` loads it.
  - I did not find a downstream non-test consumer.
- Runtime effect today: same pattern as `item_id`: schema support, no current meaningful producer, no current consumer.
- If dropped: remove it from schema, structs, and observation-issue SQL.
- Decision signal: strongest drop candidate in `observation_issues`.

### `block_scopes.blocked_at`

- Current writes:
  - new block scopes are created in `applyBlockScope` in `internal/sync/engine_runtime_scopes.go`, which sets `BlockedAt: now`.
  - `UpsertBlockScope` persists it and `validateBlockScope` requires it to be non-zero in `internal/sync/store_write_block_scopes.go`.
- Current reads:
  - `scanBlockScopeRow` loads it from SQLite into `BlockScope`.
  - `activeScopeFromBlockScopeRow` copies it into `ActiveScope`.
  - I did not find timer logic, admission logic, scope release, or CLI grouping that branches on it afterward.
- Runtime effect today: it survives restart and is carried in the in-memory scope struct, but due-trial scheduling is driven by `NextTrialAt`, not `BlockedAt`.
- If dropped: stop validating/persisting it, remove it from `BlockScope`/`ActiveScope`, and update the `block_scopes` schema.
- Decision signal: strong drop candidate, with one caveat: if humans value knowing when a block started for debugging, this is the field that preserves that history.

## Safest Immediate Drop Candidates

If the goal is to cut dead weight first with the lowest behavior risk, these are the safest targets based on current code:

- `remote_state.content_identity`
- `remote_state.previous_path`
- `retry_work.last_error`
- `retry_work.http_status`
- `retry_work.first_seen_at`
- `retry_work.last_seen_at`
- `observation_issues.item_id`
- `observation_issues.last_error`
- `observation_issues.first_seen_at`
- `observation_issues.file_size`
- `observation_issues.local_hash`
- `sync_status.last_succeeded_count`
- `sync_status.last_failed_count`

These all currently look like persisted diagnostics or historical metadata, not inputs to live sync behavior.
