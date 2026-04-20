# Data Model

Implements: R-2.5.1 [designed], R-2.5.2 [verified], R-2.5.4 [designed], R-2.10.33 [designed], R-2.15.1 [designed], R-6.5.1 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| The per-drive SQLite store remains intentionally narrow, transactional, and explicit-reset based instead of migration-driven. | `internal/sync/schema_test.go`, `internal/sync/baseline_test.go`, `internal/sync/engine_run_once_test.go` |
| Remote-truth recovery from durable `remote_state` plus replanning remains part of the crash-recovery foundation. | `TestNewSyncStore_CreatesCanonicalSchema`, `TestNewEngine_RequiresResetForUnsupportedStoreGeneration`, `TestBootstrapSync_WithChanges` |

## One Database Per Drive

Each configured drive owns one SQLite state DB. The DB is the durable authority
for restart-safe sync state; watch-mode runtime state is a rebuildable
in-memory projection.

The target schema is intentionally narrow. It stores only:

- prior synced agreement
- current local and remote truth
- observation resume metadata
- durable current-truth problems
- exact delayed work
- shared blocker timing
- one-shot run-status metadata

It does not store a durable executable plan or a mixed failure ledger.

## Core Tables

| Table | Purpose | Key |
| --- | --- | --- |
| `baseline` | Last known converged synced agreement for paths/items | `item_id` and unique `path` |
| `local_state` | Latest admissible local snapshot truth | `path` |
| `remote_state` | Latest observed remote mirror truth | `item_id` |
| `observation_issues` | Durable current-truth/content problems | stable issue identity per path/boundary |
| `retry_work` | Exact delayed retry or blocked work for the current semantic intent | `work_key` |
| `block_scopes` | Shared blocker timing and lifecycle | `scope_key` |
| `observation_state` | Configured drive owner, primary cursor, and refresh cadence | singleton row |
| `run_status` | Typed one-shot status metadata for `status` | singleton row |

## `baseline`

`baseline` is prior synced agreement. Each row records the last successfully
converged state for one item:

- identity: `item_id`, `parent_id`, `path`, `item_type`
- local comparison facts: `local_hash`, `local_size`, `local_mtime`
- remote comparison facts: `remote_hash`, `remote_size`, `remote_mtime`,
  `etag`

The table is keyed by item identity, not path, so moves stay atomic `UPDATE`s
instead of delete/reinsert churn.

## `remote_state`

`remote_state` is the durable mirror of what remote observation most recently
saw. It stores:

- identity: `item_id`, `parent_id`
- materialized path: `path`, `previous_path`
- remote facts: `item_type`, `hash`, `size`, `mtime`, `etag`,
  `content_identity`

Remote deletion is represented by row absence. If a baseline row exists and the
corresponding `remote_state` row is missing, later runs rediscover remote delete
drift from durable state alone.

## `local_state`

`local_state` is the durable mirror of the latest admissible local snapshot:

- identity/materialization: `path`, `item_type`
- local facts: `hash`, `size`, `mtime`, `content_identity`
- observation timestamp: `observed_at`

Ignored content does not enter `local_state`. The table stores only current
local truth that can participate in reconciliation.

## `observation_issues`

`observation_issues` is the durable ledger for current-truth/content problems
that are not automatic retry scheduling. Representative rows include:

- invalid filename
- path too long
- file too large
- case collision
- local file read denied
- hash/inspection failure
- policy-disallowed content

Observation is the sole durable owner of `observation_issues`. Worker-result
handling may discover a condition that should later appear there, but execution
does not upsert observation rows directly. The next observation pass is
responsible for proving and persisting that current-truth problem.

Observation-owned issue rows and observation-owned read scopes are reconciled as
one current set. If an old observation-owned issue or read scope is missing
from the new batch, the store removes it during that same reconciliation.

## `retry_work`

`retry_work` is the durable ledger for exact delayed work aligned with the
latest runtime-owned actionable set:

- semantic work identity: `work_key`, `path`, `old_path`, `action_type`
- block linkage: `scope_key`, `blocked`
- retry timing: `attempt_count`, `next_retry_at`
- operator/debug facts: `last_error`
- timestamps: `first_seen_at`, `last_seen_at`

`work_key` is the stable serialized identity for one semantic unit of work. It
is derived from the action type plus paths so replans can prune stale delayed
work against the current actionable set without inventing a durable action-plan
table.

## `block_scopes`

`block_scopes` stores active shared blocking conditions that outlive a process:

- identity: `scope_key`
- condition type
- timing source
- `blocked_at`, `trial_interval`, `next_trial_at`, `trial_count`

`block_scopes` is timing and lifecycle authority only. Concrete blocked work
belongs in `retry_work`.

- timed transient scopes (`service`, drive throttles, `quota:own`, `disk:local`)
  require blocked `retry_work` and are discarded when that blocked work
  disappears
- permission scopes are probe-owned facts and may persist without blocked
  `retry_work` until revalidation proves recovery

The target persisted scope families are:

- `service`
- `throttle:target:drive:<driveID>`
- `quota:own`
- `disk:local`
- `perm:dir:read:<path>`
- `perm:dir:write:<path>`
- `perm:remote:read:<boundary>`
- `perm:remote:write:<boundary>`
- `account:throttled`

## `observation_state`

`observation_state` is the single durable owner of per-drive observation
identity and cadence:

- `configured_drive_id`
- `cursor`
- `remote_refresh_mode`, `last_full_remote_refresh_at`,
  `next_full_remote_refresh_at`
- `local_refresh_mode`, `last_full_local_refresh_at`,
  `next_full_local_refresh_at`

The cursor is committed atomically with the corresponding remote observation
writes. The stored refresh timestamps and modes make periodic full-refresh
cadence restart-safe in one-shot and watch mode.

## `run_status`

`run_status` stores typed one-shot status metadata used by `status`:

- `last_completed_at`
- `last_duration_ms`
- `last_succeeded_count`
- `last_failed_count`
- `last_error`

It is explicitly non-authoritative for planning and observation. Watch mode
does not invent a one-shot run timestamp.

## Schema Discipline

`internal/sync/schema.go` owns the canonical schema directly. Fresh DBs
bootstrap that schema in one transaction, seed the singleton
`observation_state` row, and reopen against the same shape.

Existing DBs are trusted only when they already match the current canonical
table and column layout. Stores with stale or incompatible shapes are rejected
loudly so startup can require an explicit per-drive reset instead of migrating
or guessing forward.
