# Data Model

Implements: R-2.5.1 [designed], R-2.5.2 [verified], R-2.5.4 [designed], R-2.10.33 [designed], R-2.15.1 [designed], R-6.5.1 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| The per-mount SQLite store remains intentionally narrow, transactional, and explicit-reset based instead of migration-driven. | `internal/sync/schema_test.go`, `internal/sync/baseline_test.go`, `internal/sync/engine_run_once_test.go` |
| Remote-truth recovery from durable `remote_state` plus replanning remains part of the crash-recovery foundation. | `TestNewSyncStore_CreatesCanonicalSchema`, `TestNewEngine_RequiresResetForUnsupportedStoreGeneration`, `TestBootstrapSync_WithChanges` |

## One Database Per Mount

Each runtime mount owns one SQLite state DB. Top-level configured drives are
compiled into standalone mounts before reaching the sync engine, and managed
child mounts get their own stores too. The DB is the durable authority for
restart-safe sync state; watch-mode runtime state is a rebuildable in-memory
projection.

The target schema is intentionally narrow. It stores only:

- prior synced agreement
- current local and remote truth
- observation resume metadata
- durable current-truth problems
- exact delayed work
- shared blocker timing

It does not store a durable executable plan or a mixed failure ledger.

## Intentional Omissions

The sync store intentionally does not persist several tempting history or
rebuildable runtime facts:

- `sync_status`-style last-sync history. Status and watch summaries read
  current durable authorities directly instead of treating old success/error
  snapshots as sync truth.
- local watch refresh modes or timestamps. Local degraded/healthy scan cadence
  is runtime-owned.
- remote refresh mode or a redundant "last full refresh" timestamp. The only
  durable remote cadence fact is `observation_state.next_full_remote_refresh_at`.
- duplicate local content-identity tables. Local move matching uses
  `local_state` and `baseline` filesystem identity (`local_device`,
  `local_inode`, `local_birth_time`, `local_has_identity`) for both files and
  directories, with
  file-hash fallback only when identity is unavailable.

## Core Tables

| Table | Purpose | Key |
| --- | --- | --- |
| `baseline` | Last known converged synced agreement for paths/items | `item_id` and unique `path` |
| `local_state` | Latest admissible local snapshot truth | `path` |
| `remote_state` | Latest observed remote mirror truth | `item_id` |
| `observation_issues` | Durable current-truth/content problems | stable issue identity per path/boundary |
| `retry_work` | Exact delayed retry or blocked work for the current semantic intent | `(path, old_path, action_type)` |
| `block_scopes` | Shared blocker timing and lifecycle | `scope_key` |
| `observation_state` | Content drive owner, primary cursor, and remote refresh cadence | zero-or-one row |
| `held_deletes` | Deletes the delete-safety hold keeps for confirmation (status projection) | zero or more rows |
| `mount_boundaries` | Folders crossed into another filesystem under `cross_filesystems` | `path` |
| `kept_local_folders` | Folders deleted on OneDrive and kept locally only for content sync ignores | `path` |

## `baseline`

`baseline` is prior synced agreement. Each row records the last successfully
converged state for one item:

- identity: `item_id`, `parent_id`, `path`, `item_type`
- local comparison facts: `local_hash`, `local_size`, `local_mtime`,
  `local_ctime` (the inode change time; the hash fast path requires it
  unchanged, R-2.2.4; `0` means unknown and forces a re-read)
- last-synced local filesystem identity: `local_device`, `local_inode`,
  `local_birth_time` (the entry's creation time in Unix nanoseconds, `0` when
  the filesystem records none), `local_has_identity`
- remote comparison facts: `remote_hash`, `remote_size`, `remote_mtime`,
  `etag`

The table is keyed by item identity, not path, so remote moves stay atomic
`UPDATE`s instead of delete/reinsert churn. For local moves, the planner
compares the last-synced local filesystem identity in `baseline` with the
current identity in `local_state`; directory identity matches produce one
remote folder move instead of subtree delete/create churn. A file identity match
additionally needs size and content (or, without a hash, mtime) corroboration,
because inode numbers are reused (see `sync-planning.md`). Identities match
only when their birth times also agree wherever both are known, so a reused
inode number never pairs a deleted item with an unrelated new entry on a
filesystem that records birth times (R-2.17.1).

## `remote_state`

`remote_state` is the durable mirror of what remote observation most recently
saw for raw manageable provider items. It stores:

- identity: persisted owning `drive_id`, `item_id`
- materialized path: `path`
- remote facts: `item_type`, `hash`, `size`, `mtime`, `etag`

The row-level `drive_id` is durable authority. Shared-root and cross-drive
remote rows keep the drive that actually owns the item, even though the state
DB itself belongs to one mounted content root.

Remote parent ancestry is intentionally not persisted in `remote_state`.
Observation reconstructs sparse paths from the current delta item plus baseline
context, and successful execution reconstructs outcome parent IDs before
publishing baseline mutations.

Remote deletion is represented by row absence. If a baseline row exists and the
corresponding `remote_state` row is missing, later runs rediscover remote delete
drift from durable state alone.

Product sync filters do not delete `remote_state` rows. Planner loading derives
filtered remote current-state views from raw `remote_state` so a later config
change can reveal previously ignored remote truth without a forced remote
rescan.

## `local_state`

`local_state` is the durable mirror of the latest admissible local snapshot:

- identity/materialization: `path`, `item_type`
- local facts: `hash`, `size`, `mtime`, `ctime` (regular files only; `0` for
  folders and when unknown)
- current filesystem identity: `local_device`, `local_inode`,
  `local_birth_time`, `local_has_identity`

Ignored content does not enter `local_state`. Full local observation replaces
the table with the latest visible/admissible local truth. Planner loading still
reapplies the current `contentFilter` so stale rows from a pre-reload state
cannot leak into reconciliation.

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

Observation-owned issue rows are reconciled through one observation-batch
mutation shape. Full observation passes replace their managed current set by
issue type. The same mutation input also supports scoping reconciliation to an
exact managed path set, so a narrow probe cannot clear unrelated durable
observation rows; that mode currently has no caller (see
[sync-store.md](sync-store.md)).

Read-denied subtree boundaries are carried by `ScopeKey` on the corresponding
issue row instead of a second durable scope table. Derived truth reads use that
tagged boundary fact to block descendants.

## `retry_work`

`retry_work` is the durable ledger for exact delayed work aligned with the
latest runtime-owned actionable set:

- semantic work identity: `path`, `old_path`, `action_type`
- block linkage: `scope_key`, `blocked`
- retry timing: `attempt_count`, `next_retry_at`

The composite `(path, old_path, action_type)` identity is the durable key for
one semantic unit of work. Replans prune stale delayed work against the current
actionable set without inventing a second durable action-plan table.

## `block_scopes`

`block_scopes` stores active timed shared blocking conditions that outlive a
process:

- identity: `scope_key`
- timing: `trial_interval`, `next_trial_at`

`block_scopes` is timing and lifecycle authority only. Concrete blocked work
belongs in `retry_work`.

- all persisted scopes require blocked `retry_work` and are discarded when
  that blocked work disappears

The target persisted scope families are:

- `service`
- `throttle:target:drive:<driveID>`
- `quota:own`
- `disk:local`
- `perm:dir:write:<path>`
- `perm:remote:write:<boundary>`
- `account:throttled`

`scope_key` remains the durable identity referenced by `retry_work`. Runtime
and read-side code recover scope semantics from that key via
`describeScopeKey`; SQLite does not persist a second copy of parsed scope
metadata.

Read-denied subtree boundaries continue to use `perm:*:read:*` keys, but only
as boundary tags on `observation_issues`. They are not persisted as
`block_scopes`.

## `observation_state`

`observation_state` is the single durable owner of per-mount observation
identity and cadence:

- `content_drive_id`
- `cursor`
- `next_full_remote_refresh_at`
- `sync_root_bound`, `sync_root_device`, `sync_root_inode`, `sync_root_fsid`,
  `sync_root_birth_time`, `sync_root_token`: the fingerprint of the local sync
  folder this state was built from (the birth time is zero where the
  filesystem records none; generation 23)
- `sync_root_rebind`: an explicit `drive rebind-root` is pending

The cursor is committed atomically with the corresponding remote observation
writes. The stored next-refresh deadline makes periodic remote full-refresh
cadence restart-safe in one-shot and watch mode without persisting a fake
durable observation mode. Runtime decides the interval from the current remote
observation capability:

- delta-based watch path -> next full refresh in 24 hours
- enumerate-only watch path -> next full refresh in 1 hour

Whole-drive observation remains delta-based. Mount-root observation is
capability-driven:

- business/sharepoint mount roots skip folder delta and stay enumerate-only
- personal mount roots prefer folder delta, but may fall back to recursive
  enumeration until a later delta pass succeeds again

Local watch safety-scan cadence is runtime-owned rather than persisted in
SQLite.

`content_drive_id` identifies the remote drive for the mounted content root that
owns the DB and cursor. It does not replace the per-row
`remote_state.drive_id` authority.

The sync-folder fingerprint is recorded in the same transaction as the first
committed local snapshot (never by a dry run) and is never overwritten by a
scan. Binding first writes a random token as the extended attribute
`user.onedrive-go.root` on the folder and records it. Engine startup and every
full scan compare the folder at the sync path against the record:

- when the record has a token, the token alone decides: the same token is the
  same folder whatever its inode is now (FUSE, SMB and FAT inode churn, macOS
  fsid renumbering), and a missing or different token is a different folder
  (rm -rf and mkdir may get the inode back, never the token);
- without a token (a filesystem with no user extended attributes) the inode
  must match, and the filesystem by `statfs` fsid when both sides have one,
  otherwise by device number; a btrfs subvolume root (inode 256) also needs the
  device to match, because sibling subvolumes share fsid and root inode.

A token that does not read back after it is written counts as no token. A
marker found on the folder at startup while the state has no binding (after
`drive reset-sync-state`) is ignored, because the bind replaces it. The
expected identity has one owner per engine (`syncRootIdentity`): the executor
and every local observer read it there, so the token a bind adds is enforced
everywhere at once, and mark-and-bind is serialized; if the store already
holds a binding, the folder is re-marked with the recorded token.

A mismatch is `ErrMountRootUnavailable`, so an unmounted disk, a wiped folder
or a re-pointed symlink is refused instead of scanned as a deletion of
everything. A state whose baseline is not empty but which has no recorded
folder is refused the same way. Every refusal of a primary root (startup,
full scan, remote delete) says whether the folder lacks the
`user.onedrive-go.root` marker (replaced, or copied or restored without
extended attributes) or carries a different one, and names `drive
rebind-root`. `drive rebind-root` (which shows the
remembered and present file counts and asks for confirmation) clears the
fingerprint and sets `sync_root_rebind`, so the next start binds the folder now
there (R-6.2.12).

Remaining gap: on a filesystem without extended attributes and without
recorded birth times, a folder recreated at the same path that happens to get
the same inode on the same filesystem is accepted; the delete-safety hold
(R-6.2.5) is then the guard.

## `mount_boundaries`

`mount_boundaries` lists folders (by path) that a scan crossed into another
filesystem under `cross_filesystems`. It is written in the local-snapshot
transaction and pruned of paths that are neither in the baseline nor in
`local_state`; a flag outlives an unmount on purpose, because it is how the
next scan recognises the empty folder as a disk that is not mounted.

## `kept_local_folders`

`kept_local_folders` lists folders deleted on OneDrive whose local delete kept
them because they hold only content sync ignores (D-012): `path` and the
folder's `local_device`, `local_inode` and `local_birth_time`. The row is
written in the transaction that drops the folder's baseline row. While
`local_state` shows the same folder (`sqlSameLocalEntryL`) at that path with
nothing below it, the comparison classifies it `kept_local_folder` and plans
nothing, instead of a `local_only_create` that re-created the folder, empty,
online (R-2.17.2). A visible entry inside it makes it ordinary local content
again. The row goes when a baseline row is written at the path, and a full
local snapshot prunes rows whose folder is gone, moved or replaced.

## `held_deletes`

`held_deletes` lists the deletes the delete-safety hold kept out of the most
recent live plan: `path`, `side` (`local` or `remote`, where the delete would
remove content), `reason` (`side_vanished` or `too_many`), `item_type` and
`item_id`. `sync --confirm-deletes` releases exactly these rows.
It is replaced on every live plan; the planner re-derives the hold from
current truth and reads the table only as the set a confirmation approves.

## Schema Discipline

`internal/sync/schema.go` owns the canonical schema directly. Fresh DBs
bootstrap that schema in one transaction, seed the zero-or-one-row
`observation_state` table when absent, and reopen against the same shape.

Existing DBs are trusted only when they already match the current canonical
table and column layout. Stores with stale or incompatible shapes are rejected
loudly so startup can require an explicit mount-state reset instead of
migrating or guessing forward.
