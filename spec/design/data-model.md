# Data Model

Implements: R-2.5.1 [verified], R-2.5.2 [verified], R-2.5.4 [verified], R-2.10.33 [verified], R-2.15.1 [verified], R-6.5.1 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| The per-drive SQLite schema remains intentionally narrow and excludes deleted manual conflict/delete-approval state. | `internal/sync/schema_test.go`, `internal/sync/baseline_test.go`, `internal/sync/engine_run_once_test.go` |
| Baseline, current local/remote snapshots, retry state, and scope-block timers remain the durable sync authority surfaces. | `TestReadDriveStatusSnapshot`, `TestSyncStore_ListVisibleIssueGroups`, `TestSyncStore_ReleaseScope`, `TestSyncStore_DiscardScope` |

## One Database Per Drive

Each configured drive owns one SQLite state DB. The DB is opened in WAL mode
with `synchronous=FULL` and a 5-second busy timeout. The file is durable
authority for restart-safe sync state; watch-mode runtime state is a
rebuildable in-memory projection.

The current schema is intentionally narrow. It does **not** store manual
conflict-resolution requests, held-delete approvals, embedded shared-folder
registries, or sync-scope snapshots. Those were removed when conflict handling
became fully engine-owned, delete-safety approval was deleted, and sync
narrowed back to whole-drive or separately configured shared-root drives.

## Core Tables

| Table | Purpose | Key |
| --- | --- | --- |
| `baseline` | Last known synced truth for paths/items | `item_id` and unique `path` |
| `local_state` | Latest observed admissible local snapshot truth | `path` |
| `remote_state` | Latest observed remote mirror truth | `item_id` |
| `retry_state` | Pending retryable and blocked work for the latest semantic intent | `work_key` |
| `sync_failures` | Per-path retryable and actionable failures | `path` |
| `scope_blocks` | Durable scope-level blocking conditions and trial timing | `scope_key` |
| `observation_state` | Configured drive owner, primary cursor, and persisted refresh cadence | singleton row |
| `run_status` | Typed one-shot status/report projection for `status` | singleton row |

## `baseline`

`baseline` is the planner's common ancestor. Each row records the last
successfully converged state for one item:

- identity: `item_id`, `parent_id`, `path`, `item_type`
- local comparison facts: `local_hash`, `local_size`, `local_mtime`
- remote comparison facts: `remote_hash`, `remote_size`, `remote_mtime`, `etag`

The table is keyed by item identity, not by path, so moves stay atomic
`UPDATE`s instead of delete/reinsert churn. Because each state DB owns exactly
one configured drive, `drive_id` is not duplicated onto every row.

## `remote_state`

`remote_state` is the durable mirror of what remote observation most recently
saw. It stores:

- identity: `item_id`, `parent_id`
- materialized path: `path`, `previous_path`
- remote facts: `item_type`, `hash`, `size`, `mtime`, `etag`

Remote deletion is represented by row absence. If a baseline row exists and
the corresponding `remote_state` row is missing, later runs rediscover remote
delete drift from durable state alone.

`remote_state` persists `content_identity` in addition to the remote facts so
later SQLite-side planning can infer remote rename/move intent from durable
state rather than short-lived event envelopes.

## `local_state`

`local_state` is the durable mirror of the latest admissible local snapshot:

- identity/materialization: `path`, `item_type`
- local facts: `hash`, `size`, `mtime`, `content_identity`
- observation timestamp: `observed_at`

Ignored content does not enter `local_state`. The table stores only current
local truth that can participate in reconciliation.

## `retry_state`

`retry_state` is the durable ledger for pending retryable and blocked work
aligned with the latest runtime-owned actionable set:

- semantic work identity: `work_key`, `path`, `old_path`, `action_type`
- blocking linkage: `scope_key`, `blocked`
- retry timing: `attempt_count`, `next_retry_at`
- operator/debug facts: `last_error`
- timestamps: `first_seen_at`, `last_seen_at`

`work_key` is the stable serialized identity for one semantic unit of retryable
work. It is derived from `action_type`, `old_path`, and `path`, so replans can
prune stale delayed work against the current actionable set without inventing a
durable executable plan table.

## `sync_failures`

`sync_failures` is the durable ledger for retryable and actionable per-path
problems used for reporting, status, and issue inspection. Important columns:

- path identity: `path`, `direction`, `action_type`, `item_id`
- classification: `category`, `issue_type`, `scope_key`
- retry metadata: `failure_count`, `next_retry_at`
- operator/debug facts: `last_error`, `http_status`, `file_size`, `local_hash`
- timestamps: `first_seen_at`, `last_seen_at`

The table stores concrete failed work items and actionable path issues. It is
not a mailbox for user decisions and not the runtime authority for retry or
scope admission. Because it is keyed by path, it is also not the owner of
generic retry-ledger deletion; `retry_state` cleanup uses exact `work_key`
identity or scope-owned transitions instead.

## `scope_blocks`

`scope_blocks` stores active sync blocking conditions that outlive a process:

- identity: `scope_key`
- visible issue type: `issue_type`
- timing state: `timing_source`, `blocked_at`, `trial_interval`,
  `next_trial_at`, `trial_count`

The engine rebuilds its in-memory active-scope working set from this table at
startup and owns all runtime mutations thereafter. `scope_blocks` is timing
authority only; concrete blocked work belongs in `retry_state`.

Current persisted scope keys are:

- `quota:own`
- `throttle:target:drive:<driveID>`
- `service`
- `perm:dir:<localPath>`
- `disk:local`

Remote permission scopes are derived from blocked `retry_state` rows carrying
`perm:remote:<localPath>` scope keys rather than persisted directly in
`scope_blocks`.

## `observation_state`

`observation_state` is the single durable owner of per-drive observation
identity and cadence:

- `configured_drive_id`: the configured drive that owns this DB
- `cursor`: the one primary remote observation cursor for this DB
- `remote_refresh_mode`, `last_full_remote_refresh_at`, `next_full_remote_refresh_at`
- `local_refresh_mode`, `last_full_local_refresh_at`, `next_full_local_refresh_at`

The cursor is committed atomically with the corresponding remote observation
writes. The persisted local/remote refresh timestamps and modes make periodic
full-refresh cadence restart-safe in both one-shot and watch mode.

## `run_status`

`run_status` stores the typed one-shot status projection used by `status`:

- `last_completed_at`
- `last_duration_ms`
- `last_succeeded_count`
- `last_failed_count`
- `last_error`

It is explicitly non-authoritative for planning and observation. Watch mode
does not invent a one-shot run timestamp.

## Schema Discipline

`internal/sync/schema.go` owns the full canonical schema directly. Fresh DBs
bootstrap that schema in one transaction, seed the singleton
`observation_state` row, and reopen against the same shape.

Existing DBs are trusted only when they already match the current canonical
table and column layout. Stores with stale or incompatible user tables are
rejected loudly so startup can require an explicit per-drive reset instead of
migrating or guessing forward.
