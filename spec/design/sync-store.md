# Sync Store

GOVERNS: internal/syncstore/store.go, internal/syncstore/inspector.go, internal/syncstore/schema.go, internal/syncstore/schema.sql, internal/syncstore/store_baseline.go, internal/syncstore/store_observation.go, internal/syncstore/store_conflicts.go, internal/syncstore/store_failures.go, internal/syncstore/store_admin.go, internal/syncstore/store_scope_blocks.go, internal/syncstore/shortcuts.go, internal/syncverify/verify.go, internal/syncrecovery/recovery.go, internal/cli/verify.go, internal/cli/issues.go, internal/cli/failure_display.go

Implements: R-2.5 [verified], R-2.3.2 [verified], R-2.3.3 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.7 [verified], R-2.15.1 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.4 [verified], R-2.10.5 [verified], R-2.10.14 [verified], R-2.10.22 [verified], R-2.10.33 [verified], R-2.10.34 [verified], R-2.10.41 [verified], R-2.10.45 [verified], R-2.14.3 [verified], R-2.14.5 [verified], R-6.6.11 [verified], R-6.7.17 [verified], R-6.8.16 [verified], R-6.10.6 [verified]

## SyncStore (`store.go`)

`SyncStore` is the sole durable authority for sync state. It owns baseline,
remote observation state, conflicts, per-item failures, persisted scope
blocks, shortcut metadata, and sync metadata. Runtime watch state is never a
peer authority; it is rebuilt from store state when the engine starts.

## Ownership Contract

- Owns: Durable sync state in SQLite, transactional state transitions, conflict/failure/scope persistence, and restart/recovery records.
- Does Not Own: Graph calls, sync-root filesystem probing, failure classification policy, or multi-drive lifecycle.
- Source of Truth: The SQLite schema and rows defined by `schema.sql`.
- Allowed Side Effects: SQLite reads/writes and schema application only.
- Mutable Runtime Owner: `SyncStore` owns its writable DB handle and internal rebuildable baseline cache. `Inspector` owns its own read-only DB handle. Neither runs background goroutines; both expose synchronous methods only.
- Error Boundary: The store persists already-classified failure roles and categories from [error-model.md](error-model.md). It does not reinterpret raw external errors into new policy classes.

`NewSyncStore()` opens SQLite in WAL mode and applies the canonical schema from
[`schema.sql`](/Users/tonimelisma/Development/onedrive-go/internal/syncstore/schema.sql)
through [`schema.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncstore/schema.go).
Fresh databases use the final schema directly. There is one narrow legacy
repair path for old `baseline` rows that still use the pre-side-aware
`size`/`mtime` columns. On open, the store rewrites that table into the
canonical shape and backfills only the local-side metadata that can be
recovered exactly. Remote-side size/mtime remain unknown when the legacy row
never stored them.

Key operations:

- `CommitObservation()` atomically writes `remote_state` rows and advances the relevant delta token.
- `CommitOutcome()` updates `baseline` and finalizes remote-state transitions per action. Success-side `sync_failures` cleanup is engine-owned and happens before or after the store commit depending on the result flow.
- `RecordFailure(ctx, SyncFailureParams, delayFn)` is the single failure writer. The engine provides classification and retry policy; the store provides transactional persistence and conflict-safe upsert behavior.
- `ResetDownloadingStates(ctx, delayFn)`, `ListDeletingCandidates(ctx)`, and `FinalizeDeletingStates(ctx, deleted, pending, delayFn)` are the state-only crash-recovery primitives. The store no longer probes the sync-root filesystem itself.
- `ReleaseScope(ctx, scopeKey, now)` is the single durable “scope resolved” transition. It deletes the `scope_blocks` row when one exists, deletes any legacy `boundary` failure row for that scope, and converts all `held` failures for that scope into retryable `item` rows with `next_retry_at = now`.
- `DiscardScope(ctx, scopeKey)` is the single durable “scope and blocked work are gone” transition. It deletes the `scope_blocks` row when one exists and every `sync_failures` row for that scope.

All write methods are transactional. SQLite WAL mode plus a single writer
connection gives crash-safe durability without introducing another source of
truth.

For file rows, `CommitOutcome()` persists the comparison tuple the planner
needs later:

- local side: `local_hash`, `local_size`, `local_mtime`
- remote side: `remote_hash`, `remote_size`, `remote_mtime`, `etag`

The store does not fabricate hashes or synthesize fallback decisions. It
persists exactly what observation and execution learned, including known zero
sizes. Comparison policy stays in the planner.

## Canonical Schema (`schema.sql`)

The schema is defined directly in final form. The important tables for failure
and scope management are:

### `sync_failures`

One row represents one concrete path/item failure state.

Important columns:

- `path`, `drive_id`, `direction`, `action_type`
- `category` = `transient` or `actionable`
- `failure_role` = `item`, `held`, or `boundary`
- `issue_type`
- `next_retry_at`
- `manual_trial_requested_at`
- `scope_key`

`failure_role` makes the row meaning explicit:

- `item`: ordinary file/path failure or actionable issue
- `held`: work currently blocked behind an active scope
- `boundary`: the actionable row that defines a scope-backed condition

`perm:remote` is a special case: it uses only `held` rows. There is no normal
`boundary` row for shared-folder read-only state because the blocked writes are
the only durable authority for that derived scope.

Store-level constraints enforce the legal combinations:

- `held` rows must be transient, scoped, and non-retryable until release
- `boundary` rows must be actionable, scoped, and non-retryable
- boundary scope keys are unique via a partial unique index

This keeps `sync_failures` as one durable failure ledger while removing the
implicit role inference that used to depend on `category`, `scope_key`, and
`next_retry_at`.

The store's role in the shared error model is persistence, not classification:
`category` and `failure_role` are the durable projection of higher-layer
decisions documented in [error-model.md](error-model.md).

### `scope_blocks`

One row represents one active blocking condition.

Important columns:

- `scope_key`
- `issue_type`
- `timing_source` = `none`, `backoff`, or `server_retry_after`
- `blocked_at`
- `trial_interval`
- `next_trial_at`
- `preserve_until`
- `trial_count`

`scope_blocks` stores scope-level timing state only. Runtime watch admission
still uses the engine-owned `activeScopes` working set, but that working set
is ephemeral and rebuildable from this table plus derived `perm:remote` held
rows.

`timing_source` distinguishes locally computed backoff from explicit server
deadlines. Startup repair uses this to decide whether a persisted
`throttle:account` or `service` scope should survive restart.

`preserve_until` is a bounded restart-safe override used only for
scoped-failure-backed scopes. It allows the engine to keep a preserved scope
alive across restart even when the candidate row was replaced or re-homed to a
more specific failure shape. Scope ownership still remains in `scope_blocks`;
the field is not duplicated into `sync_failures`.

`auth:account` also lives in `scope_blocks`, but it is intentionally
non-trial: `timing_source='none'`, `trial_interval=0`, `next_trial_at=0`, and
`preserve_until=0`. The row records an account-level blocking condition, not a
retry curve.

## Failure And Scope Lifecycle

The store models two different durable authorities on purpose:

- `sync_failures`: concrete failed or held items
- `scope_blocks`: active blocking conditions

The engine is responsible for deciding when to create, release, or discard a
scope. The store is responsible for persisting those transitions atomically.

That split keeps the data model honest:

- one derived `perm:remote:Shared/Docs` scope can block many held upload rows without any persisted `scope_blocks` row
- one `throttle:account` scope can block work across all drives
- one `quota:shortcut:*` scope can outlive many individual path failures
- one `auth:account` scope can represent an account-level authorization stop without fabricating a path-level failure row

## Store Interfaces (`store_interfaces.go`)

Typed sub-interfaces enforce transition ownership at compile time. Callers get
the narrowest store surface they need.

`SyncStore.Load()` uses a cache-through baseline strategy: the store caches the
most recently loaded baseline in memory, invalidates it before outcome commits,
and rebuilds it after writes. That cache is internal to the store and is
rebuildable from durable state; it is not a competing authority.

## Read-Only Inspection

`Inspector` is the read-only companion to `SyncStore`. It is opened only from
`internal/syncstore` and gives administrative readers a narrow projection of
state without handing them raw SQL ownership.

- `OpenInspector(dbPath, logger)` opens SQLite in read-only mode.
- `HasScopeBlock(ctx, key)` provides an exact read-only scope-block probe for
  CLI auth-health and other administrative readers that need one persisted
  signal without opening the writable `SyncStore` path.
- `ReadStatusSnapshot(ctx)` returns metadata, aggregate counts, and one
  derived `IssueSummary`.
- `ReadIssuesSnapshot(ctx, history)` returns the full read-only `issues`
  projection: grouped visible issue families, held deletes, pending retries,
  and conflict history.
- `IssueSummary.Groups` are keyed by the shared
  [`synctypes.SummaryKey`](/Users/tonimelisma/Development/onedrive-go-shared-failure-summaries/internal/synctypes/summary_keys.go),
  not by raw SQL categories. Each group also carries the normalized scope kind
  plus an optional humanized scope label so CLI `status` can show file,
  directory, shortcut/drive, account, or service context without reopening raw
  tables.
- `IssueSummary.VisibleTotal()`, `ConflictCount()`, `ActionableCount()`,
  `RemoteBlockedCount()`, `AuthRequiredCount()`, and `RetryingCount()` are the
  read-only status contract. CLI `status` consumes those helpers instead of
  reconstructing visible-issue semantics from raw counters.
- `StatusSnapshot` and `IssuesSnapshot` share one visible-issue projection
  builder inside `Inspector`, so `status` and `issues` cannot silently drift
  on what counts as a visible issue family.
- CLI `status` and `issues list` consume `Inspector`; they do not build their
  own DSNs or call `sql.Open` directly.

## Verification

[`internal/syncverify/verify.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncverify/verify.go)
re-hashes local files against baseline entries through a
[`synctree.Root`](/Users/tonimelisma/Development/onedrive-go/internal/synctree/synctree.go)
capability. The store provides only baseline data; it does not own local file
hashing or filesystem probing.

`syncverify` uses the local-side baseline metadata only. Size checks compare
against `local_size` when it is known; remote-side metadata is irrelevant to
local verification.

Verification remains read-only all the way through the store boundary:
per-path stat/rooted-path/hash failures are reported as mismatch rows instead
of aborting the whole pass, while context cancellation is still fatal to the
overall verify command. `VerifyReport.Mismatches` is sorted by path before it
reaches CLI formatting so text and JSON output stay deterministic across map
iteration order.

## Crash Recovery Boundary

[`internal/syncrecovery/recovery.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncrecovery/recovery.go)
owns the sync-root filesystem half of crash recovery. It classifies deleting
rows as completed deletes or pending retries via
[`synctree.Root`](/Users/tonimelisma/Development/onedrive-go/internal/synctree/synctree.go),
then calls the store’s state-only recovery primitives. `SyncStore` no longer
joins sync-root paths or calls `os.Stat` itself.

Per-candidate stat failures do not abort recovery; they downgrade that path to
the pending-delete set so the next engine pass retries it. Store-boundary
failures (`ResetDownloadingStates`, `ListDeletingCandidates`,
`FinalizeDeletingStates`) still abort the recovery pass with context-wrapped
errors because the durable-state transition itself is incomplete.

## Issues CLI

`issues list` reads one store-owned `IssuesSnapshot` from `Inspector`.
`syncstore` owns grouping, scope labeling, pending-retry aggregation, and held
delete separation; CLI only formats that snapshot. Grouping and display use
the persisted `scope_key`, `issue_type`, `category`, `failure_role`, and
shortcut metadata, but the user-facing grouping key is still the shared
`synctypes.SummaryKey`. This keeps `issues` presentation aligned with
`status` summaries and sync-runtime logging without persisting a second
summary column in SQLite.

Retryable transient item failures intentionally surface through
`IssuesSnapshot.PendingRetries` rather than the visible grouped-issue list.
The durable row still carries the raw evidence (`issue_type`, `category`,
`failure_role`, `scope_key`), and `synctypes.SummaryKeyForPersistedFailure`
remains the read-time normalization rule for testable reprojection.

Derived shared-folder blocked writes and scope-only auth blocks are normalized
into that same `IssuesSnapshot`, so `issues`, `status`, and watch/runtime
summaries all consume one store-owned visible-issue taxonomy instead of
rebuilding different views from raw tables.

The broader CLI auth-health projection also reads `auth:account` from
`scope_blocks` through `Inspector.HasScopeBlock`, but it combines that
store-backed signal with token and account-profile discovery instead of
replacing either source of truth.
Offline/read-only surfaces only project the stored auth block. Live proof
surfaces may clear `auth:account` after a successful authenticated Graph
response for the account.
