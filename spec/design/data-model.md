# Data Model

Implements: R-2.5.1 [verified], R-2.5.2 [verified], R-2.5.4 [verified], R-2.10.33 [verified], R-2.15.1 [verified], R-6.5.1 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| The per-drive SQLite schema remains intentionally narrow and excludes deleted manual conflict/delete-approval state. | `internal/sync/schema_migration_test.go`, `internal/sync/store_integrity_test.go`, `internal/sync/baseline_test.go` |
| Baseline, remote mirror, failure, and scope-block tables remain the durable sync authority surfaces. | `TestReadDriveStatusSnapshotAndScopeBlockHelpers`, `TestSyncStore_ListVisibleIssueGroups`, `TestSyncStore_FailureAdminMutations` |

## One Database Per Drive

Each configured drive owns one SQLite state DB. The DB is opened in WAL mode
with `synchronous=FULL`, a 5-second busy timeout, and embedded goose migration
history. The file is durable authority for restart-safe sync state; watch-mode
runtime state is a rebuildable in-memory projection.

The current schema is intentionally narrow. It does **not** store manual
conflict-resolution requests, held-delete approvals, embedded shared-folder
registries, or sync-scope snapshots. Those were removed when conflict handling
became fully engine-owned, delete-safety approval was deleted, and sync
narrowed back to whole-drive or separately configured shared-root drives.

## Core Tables

| Table | Purpose | Key |
| --- | --- | --- |
| `baseline` | Last known synced truth for paths/items | `(drive_id, item_id)` and unique `path` |
| `remote_state` | Latest observed remote mirror truth | `(drive_id, item_id)` |
| `sync_failures` | Per-path retryable and actionable failures | `(path, drive_id)` |
| `scope_blocks` | Durable scope-level blocking conditions and trial timing | `scope_key` |
| `delta_tokens` | Remote observation cursors | `(drive_id, scope_id)` |
| `sync_metadata` | Last-run/report metadata for status and diagnostics | `key` |

## `baseline`

`baseline` is the planner's common ancestor. Each row records the last
successfully converged state for one item:

- identity: `drive_id`, `item_id`, `parent_id`, `path`, `item_type`
- local comparison facts: `local_hash`, `local_size`, `local_mtime`
- remote comparison facts: `remote_hash`, `remote_size`, `remote_mtime`, `etag`
- commit timestamp: `synced_at`

The table is keyed by remote identity, not by path, so moves stay atomic
`UPDATE`s instead of delete/reinsert churn.

## `remote_state`

`remote_state` is the durable mirror of what remote observation most recently
saw. It stores:

- identity: `drive_id`, `item_id`, `parent_id`
- materialized path: `path`, `previous_path`
- remote facts: `item_type`, `hash`, `size`, `mtime`, `etag`
- observation timestamp: `observed_at`

Remote deletion is represented by row absence. If a baseline row exists and
the corresponding `remote_state` row is missing, later runs rediscover remote
delete drift from durable state alone.

`remote_state` no longer carries filtered-row fields such as `is_filtered`,
`filter_generation`, or `filter_reason`. Observation now stores only raw
remote truth; planning and permission policy decide what work is admissible.

## `sync_failures`

`sync_failures` is the durable ledger for retryable and actionable per-path
problems. Important columns:

- path identity: `path`, `drive_id`, `direction`, `action_type`, `item_id`
- classification: `category`, `issue_type`, `scope_key`
- retry metadata: `failure_count`, `next_retry_at`
- operator/debug facts: `last_error`, `http_status`, `file_size`, `local_hash`
- timestamps: `first_seen_at`, `last_seen_at`

The table stores concrete failed work items and actionable path issues. It is
not a mailbox for user decisions.

## `scope_blocks`

`scope_blocks` stores active blocking conditions that outlive a process:

- identity: `scope_key`
- visible issue type: `issue_type`
- timing state: `timing_source`, `blocked_at`, `trial_interval`,
  `next_trial_at`, `preserve_until`, `trial_count`

The engine rebuilds its in-memory active-scope working set from this table at
startup and owns all runtime mutations thereafter.

Current persisted scope keys are:

- `auth:account`
- `quota:own`
- `throttle:target:drive:<driveID>`
- `service`
- `perm:dir:<localPath>`
- `perm:remote:<localPath>`
- `disk:local`

Legacy `throttle:account` remains parseable for startup cleanup only.

## `delta_tokens`

`delta_tokens` stores remote observation cursors. `scope_id=""` is the primary
cursor for a drive-root session. Shared-root drives may also store scoped
cursors keyed by their configured remote root item ID and scope drive ID.

Tokens are committed atomically with the corresponding remote observation
writes. They describe what Graph has reported, not what the planner has acted
on yet.

## `sync_metadata`

`sync_metadata` stores summary/report facts from completed runs, such as last
run timing and counters used by status and diagnostics. It is explicitly
non-authoritative for planning and observation.

## Migration Discipline

The embedded goose history is authoritative. Fresh DBs start at
`internal/sync/migrations/00001_init.sql`, and existing DBs are trusted only
when they already contain valid goose history. Stores with user tables but no
migration history are rejected loudly instead of guessed forward.

Because the current schema no longer carries durable manual-intent tables,
state DB compatibility is simpler than before: the DB is authoritative for
sync truth and retry/scope state, not for queued user decisions.
