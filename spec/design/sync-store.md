# Sync Store

GOVERNS: internal/syncstore/store.go, internal/syncstore/schema.go, internal/syncstore/schema.sql, internal/syncstore/store_baseline.go, internal/syncstore/store_observation.go, internal/syncstore/store_conflicts.go, internal/syncstore/store_failures.go, internal/syncstore/store_admin.go, internal/syncstore/store_scope_blocks.go, internal/syncstore/shortcuts.go, internal/syncverify/verify.go, internal/syncrecovery/recovery.go, internal/cli/verify.go, internal/cli/issues.go, internal/cli/failure_display.go

Implements: R-2.5 [verified], R-2.3.2 [verified], R-2.3.3 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.7 [verified], R-2.15.1 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.4 [verified], R-2.10.22 [verified], R-2.10.33 [verified], R-2.10.34 [verified], R-2.10.41 [verified], R-6.6.11 [verified]

## SyncStore (`store.go`)

`SyncStore` is the sole durable authority for sync state. It owns baseline,
remote observation state, conflicts, per-item failures, persisted scope
blocks, shortcut metadata, and sync metadata. Runtime watch state is never a
peer authority; it is rebuilt from store state when the engine starts.

`NewSyncStore()` opens SQLite in WAL mode and applies the canonical schema from
[`schema.sql`](/Users/tonimelisma/Development/onedrive-go/internal/syncstore/schema.sql)
through [`schema.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncstore/schema.go).
There is no incremental migration chain and no compatibility bootstrap path.
The repository has no launched users, so the store defines the final schema
directly.

Key operations:

- `CommitObservation()` atomically writes `remote_state` rows and advances the relevant delta token.
- `CommitOutcome()` updates `baseline` and finalizes remote-state transitions per action. Success-side `sync_failures` cleanup is engine-owned and happens before or after the store commit depending on the result flow.
- `RecordFailure(ctx, SyncFailureParams, delayFn)` is the single failure writer. The engine provides classification and retry policy; the store provides transactional persistence and conflict-safe upsert behavior.
- `ResetDownloadingStates(ctx, delayFn)`, `ListDeletingCandidates(ctx)`, and `FinalizeDeletingStates(ctx, deleted, pending, delayFn)` are the state-only crash-recovery primitives. The store no longer probes the sync-root filesystem itself.
- `ReleaseScope(ctx, scopeKey, now)` is the single durable “scope resolved” transition. It deletes the `scope_blocks` row, deletes any `boundary` failure row for that scope, and converts all `held` failures for that scope into retryable `item` rows with `next_retry_at = now`.
- `DiscardScope(ctx, scopeKey)` is the single durable “scope and blocked work are gone” transition. It deletes the `scope_blocks` row and every `sync_failures` row for that scope.

All write methods are transactional. SQLite WAL mode plus a single writer
connection gives crash-safe durability without introducing another source of
truth.

## Canonical Schema (`schema.sql`)

The schema is defined directly in final form. The important tables for failure
and scope management are:

### `sync_failures`

One row represents one concrete path/item failure state.

Important columns:

- `path`, `drive_id`, `direction`
- `category` = `transient` or `actionable`
- `failure_role` = `item`, `held`, or `boundary`
- `issue_type`
- `next_retry_at`
- `scope_key`

`failure_role` makes the row meaning explicit:

- `item`: ordinary file/path failure or actionable issue
- `held`: work currently blocked behind an active scope
- `boundary`: the actionable row that defines a scope-backed condition

Store-level constraints enforce the legal combinations:

- `held` rows must be transient, scoped, and non-retryable until release
- `boundary` rows must be actionable, scoped, and non-retryable
- boundary scope keys are unique via a partial unique index

This keeps `sync_failures` as one durable failure ledger while removing the
implicit role inference that used to depend on `category`, `scope_key`, and
`next_retry_at`.

### `scope_blocks`

One row represents one active blocking condition.

Important columns:

- `scope_key`
- `issue_type`
- `timing_source` = `none`, `backoff`, or `server_retry_after`
- `blocked_at`
- `trial_interval`
- `next_trial_at`
- `trial_count`

`scope_blocks` stores scope-level timing state only. Runtime watch admission
still uses the engine-owned `activeScopes` working set, but that working set
is ephemeral and rebuildable from this table.

`timing_source` distinguishes locally computed backoff from explicit server
deadlines. Startup repair uses this to decide whether a persisted
`throttle:account` or `service` scope should survive restart.

## Failure And Scope Lifecycle

The store models two different entities that stay separate on purpose:

- `sync_failures`: concrete failed or held items
- `scope_blocks`: active blocking conditions

The engine is responsible for deciding when to create, release, or discard a
scope. The store is responsible for persisting those transitions atomically.

That split keeps the data model honest:

- one `perm:remote:Shared/Docs` scope can block many held upload rows
- one `throttle:account` scope can block work across all drives
- one `quota:shortcut:*` scope can outlive many individual path failures

## Store Interfaces (`store_interfaces.go`)

Typed sub-interfaces enforce transition ownership at compile time. Callers get
the narrowest store surface they need.

`SyncStore.Load()` uses a cache-through baseline strategy: the store caches the
most recently loaded baseline in memory, invalidates it before outcome commits,
and rebuilds it after writes. That cache is internal to the store and is
rebuildable from durable state; it is not a competing authority.

## Verification

[`internal/syncverify/verify.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncverify/verify.go)
re-hashes local files against baseline entries through a
[`synctree.Root`](/Users/tonimelisma/Development/onedrive-go/internal/synctree/synctree.go)
capability. The store provides only baseline data; it does not own local file
hashing or filesystem probing.

## Crash Recovery Boundary

[`internal/syncrecovery/recovery.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncrecovery/recovery.go)
owns the sync-root filesystem half of crash recovery. It classifies deleting
rows as completed deletes or pending retries via
[`synctree.Root`](/Users/tonimelisma/Development/onedrive-go/internal/synctree/synctree.go),
then calls the store’s state-only recovery primitives. `SyncStore` no longer
joins sync-root paths or calls `os.Stat` itself.

## Issues CLI

`issues` reads conflicts and actionable failures directly from the store.
Grouping and display use the persisted `scope_key`, `issue_type`, and
shortcut metadata instead of re-deriving scope context from runtime state.
