# Data Model

Implements: R-6.5.1 [verified], R-6.5.2 [verified], R-2.5.1 [verified], R-2.5.2 [verified], R-2.3.2 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-6.7.17 [verified]

## One Database per Drive

Each configured drive gets its own SQLite database file. The canonical drive identifier determines the filename (`:` replaced by `_`):

- **Linux**: `~/.local/share/onedrive-go/state_<type>_<email>[_<site>_<library>].db`
- **macOS**: `~/Library/Application Support/onedrive-go/state_<type>_<email>[_<site>_<library>].db`

Engine: `modernc.org/sqlite` (pure Go, no CGO). WAL mode, `synchronous = FULL`, 5-second busy timeout.

The store applies embedded goose migrations and records schema history in
`goose_db_version`. Existing state DBs with user tables but no goose history
are rejected with rebuild/migrate guidance rather than guessed forward.
Preexisting goose metadata is also rejected when it is empty or malformed; a
broken `goose_db_version` table is not treated as a pristine empty store. Most
sync state is rebuildable from local filesystem plus OneDrive truth, but
`conflict_requests` rows and `held_deletes` approvals are durable user intent
and must not be silently migrated or erased by guesswork.

## Three-Table State Model

The sync database uses remote state separation: three core tables decouple API observation from sync success.

| Table | Purpose | Key |
|-------|---------|-----|
| `remote_state` | Full mirror of every item the delta API reports | `(drive_id, item_id)` |
| `baseline` | Confirmed synced state | `(drive_id, item_id)`, `path` UNIQUE |
| `sync_failures` | Unified item failure tracking with explicit role semantics | `(path, drive_id)` |

Supporting tables: `delta_tokens`, `conflicts`, `conflict_requests`, `held_deletes`, `sync_metadata`, `shortcuts`, `scope_blocks`.

### remote_state

`remote_state` is a pure mirror of the latest observed remote truth for the
drive. It is updated only by remote observation and by successful remote-side
executor outcomes that change remote truth (for example, upload, remote move,
and remote delete).

It does **not** store workflow phases such as pending download, in-progress
work, success, or failure. Those concerns belong to the planner, the executor,
and `sync_failures`.

Important fields:

- remote identity: `drive_id`, `item_id`, `parent_id`
- materialized location: `path`, `previous_path`
- item shape: `item_type`
- remote comparison facts: `hash`, `size`, `mtime`, `etag`
- scope policy metadata: `is_filtered`, `filter_generation`, `filter_reason`
- observation time: `observed_at`

Remote deletion is represented by row absence. If a baseline row exists and the
corresponding remote mirror row no longer exists, the next reconciliation pass
derives remote-delete drift directly from `baseline` vs `remote_state`.

`previous_path` tracks rename history for move detection. `path` remains unique
for live mirrored items.

### baseline

Confirmed synced state per `(drive_id, item_id)`. Each row represents content that both local and remote agree on.

Per-side hashes (`local_hash`, `remote_hash`) handle SharePoint enrichment: when SharePoint modifies a file server-side, the remote hash changes but the local content hasn't changed. The planner compares new hashes against the correct side's baseline hash to avoid false conflicts.

File baselines also persist side-specific comparison metadata:

- local side: `local_size`, `local_mtime`
- remote side: `remote_size`, `remote_mtime`, `etag`

This makes hashless comparison durable across restarts. When both local-side
hashes are absent, the planner falls back to `local_size + local_mtime`. When
both remote-side hashes are absent, it falls back to
`remote_size + remote_mtime + etag`. Unknown metadata is preserved as unknown
and never treated as equality.

`path` is a UNIQUE secondary key for fast local lookups. Moves are a single UPDATE (atomic) rather than DELETE+INSERT, enabled by the ID-based primary key.

Baseline memory footprint is ~19 MB per 100K files, additive across drives. Monitor under profiling. [planned]

SQLite stores zero-byte known sizes as `sql.NullInt64{Valid: true, Int64: 0}`.
`NULL` means "unknown size", not "zero". This matters for hashless comparison:
zero-byte OneDrive files often have no hash, so `0` must remain distinguishable
from missing metadata.

### sync_failures

Unified item failure tracking for download, upload, and delete work. Each row
records one path-level failure state with an explicit `failure_role`:

- `item` — ordinary failed work item, either transient or actionable
- `held` — work item blocked behind an active scope until release
- `boundary` — actionable scope-defining row for a permission boundary

Categories remain:
- `transient` — retried via `next_retry_at`
- `actionable` — require user intervention or boundary recheck

`action_type` is the authoritative replay/rebuild field. `direction` remains a
coarse summary/display column and is normalized from `action_type` at write
time so persisted rows cannot drift into illegal combinations.

The role model makes row meaning explicit instead of inferring it from
`scope_key` and `next_retry_at`. Keyed by `(path, drive_id)`. Surfaced via
per-drive `status`. Shared-folder read-only state is modeled as `held`
blocked-write rows only; it does not keep a durable `boundary` row once the
blocked write intent is gone.

### scope_blocks

Durable per-scope blocking conditions. This table stores the restart/recovery
record for active scopes together with trial timing metadata.

Key columns:
- `scope_key` — unique scope identity
- `issue_type` — scope-level user/reporting classification
- `timing_source` — `none`, `backoff`, or `server_retry_after`
- `blocked_at`, `trial_interval`, `next_trial_at`, `preserve_until`, `trial_count`

`scope_blocks` is separate from `sync_failures` because scope-level timing state
and item-level failure state are different entities with different cardinality.
The watch loop keeps only a rebuildable in-memory working set; durable truth
remains in SQLite. `perm:remote-write` is the exception: its scope is derived
from held blocked-write rows and is not persisted in `scope_blocks`.

`preserve_until` makes preserve semantics durable without inventing duplicate
held rows. A preserved scope may therefore survive restart even when no
same-scope candidate row remains, but only until the next scheduled trial
deadline.

`auth:account` is also stored in `scope_blocks`, but unlike quota/service/disk
it is not a trial-driven scope. The row uses `timing_source='none'` with zero
trial metadata and represents a durable account-level authorization stop until
startup proof clears it.

### delta_tokens

Delta API cursor per drive scope. `scope_id = ""` for primary scope. Drives with shortcuts to shared folders have additional scopes (one per shortcut). The cursor is committed atomically with `remote_state` observations — it tracks what the API has reported, not what has been synced.

### conflicts

Per-file conflict tracking. Three conflict types: `edit_edit`,
`edit_delete`, `create_create`.

`conflicts` is the derived conflict-facts and history table. It records:

- conflict identity: `id`, `drive_id`, `item_id`, `path`
- conflict evidence: `conflict_type`, `detected_at`, per-side hashes/mtimes
- final outcome only: `resolution`, `resolved_at`, `resolved_by`

`resolution` is the durable final outcome:
`unresolved`, `keep_both`, `keep_local`, or `keep_remote`.

This table deliberately does **not** own active request workflow. Conflict
existence and final engine outcome are derived sync facts; queued user intent
belongs in `conflict_requests`.

### conflict_requests

Durable conflict-resolution request workflow, keyed by `conflict_id`.

Columns:

- `requested_resolution`
- `state`
- `requested_at`
- `applying_at`
- `last_error`

Valid states:

- `queued` — CLI/control socket recorded durable user intent, possibly with a
  preserved `last_error` from the most recent failed attempt
- `applying` — one engine claimed execution

Concurrent request semantics are last-write-wins while still queued, then
engine-owned once application begins:

- unresolved conflict + valid request => create/update one `queued` row
- same strategy while `queued` => idempotent
- different strategy while `queued` => overwrite the queued strategy and clear the old `last_error`
- `applying` => rejected as already in progress
- resolved conflict => rejected as already resolved

Successful engine resolution deletes the `conflict_requests` row in the same
transaction that marks the matching `conflicts` row resolved. Failed
execution rewrites the row back to `queued` with `last_error` preserved so the
unresolved conflict stays visible until the chosen layout can actually be
established.

### held_deletes

Delete safety protection ledger. Held deletes are not sync failures; they are
durable user-gated safety decisions.

Keyed by `(drive_id, action_type, path, item_id)`. `item_id` is required so a
user approval for one deleted item cannot authorize a later unrelated delete
after path reuse. Fields:

- `state='held'` — engine observed a delete above the configured threshold and filtered it out
- `state='approved'` — user approved the delete; the next engine pass may execute it
- `item_id` — the remote/local item identity that must match the planned delete
- `held_at`, `approved_at`, `last_planned_at`, `last_error` — audit and display metadata

Approved rows are consumed only after the corresponding delete action succeeds.
Approved rows are also excluded from future delete-safety holds, so the same
approved delete does not retrigger protection on the next normal sync pass.

### shortcuts

Shortcut-to-shared-folder registry. Tracks `remote_drive`, `remote_item`, and observation method (`delta` or `enumerate`). Shared-folder read-only state is not stored here.

## SyncStore Sub-Interfaces

All database writes flow through typed sub-interfaces, enforcing transition ownership at compile time:

| Interface | Caller | Purpose |
|-----------|--------|---------|
| `ObservationWriter` | Remote observer | Write observed remote truth + advance delta token atomically |
| `OutcomeWriter` | Worker pool | Commit action results to baseline + update remote truth when the action changes remote state |
| `FailureRecorder` | Worker result drain | Record failure metadata in sync_failures |
| `StateReader` | Reconciler, planner, CLI | Read-only queries across all tables |
| `StateAdmin` | CLI commands | Admin writes (resolve conflicts, reset failures) |

## Upload Sessions (File-Based)

Resumable upload sessions are tracked as JSON files in the data directory (not in the database). On resume, the local file's hash is compared against the hash at session start — if it differs, the session is discarded.

## Schema Bootstrap

The sync store bootstraps and upgrades through embedded goose migrations.
Fresh databases start at `00001_init.sql`; later migrations rewrite schema and
data in place while preserving durable user intent.

Existing DBs that contain user tables but no goose history are rejected
instead of guessed forward. Existing DBs with empty or malformed
`goose_db_version` history are also rejected before migration startup, even if
that broken goose table is the only user-visible table. That fail-loudly
behavior is intentional because the DB now holds explicit user decisions
(`conflict_requests`, `held_deletes`), not only rebuildable cache.

The current migration stream includes a schema split that moves active
conflict-resolution workflow out of `conflicts` and into
`conflict_requests`. Migration correctness matters because it preserves queued
and failed user requests without keeping the old mixed-authority table shape
alive in the runtime schema.

## Performance

- **WAL checkpointing**: After initial sync, every 30 minutes, and on shutdown. Explicit `PRAGMA wal_checkpoint(TRUNCATE)` before database close ensures all WAL data is flushed to the main file.
- **Prepared statements**: Cached for connection lifetime
- **Per-action commits**: Each completed action committed individually for incremental durability
- **VACUUM**: Not part of normal sync-store bootstrap
- **Batched commits**: Per-action commit is ~0.5ms. For high-throughput workloads, batched commits could reduce overhead. Currently bottleneck is network I/O, not SQLite. [planned]
