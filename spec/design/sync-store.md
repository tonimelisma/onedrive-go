# Sync Store

GOVERNS: internal/syncstore/store.go, internal/syncstore/inspector.go, internal/syncstore/schema.go, internal/syncstore/migrations/*.sql, internal/syncstore/tx.go, internal/syncstore/store_baseline.go, internal/syncstore/store_observation.go, internal/syncstore/store_conflicts.go, internal/syncstore/store_failures.go, internal/syncstore/store_held_deletes.go, internal/syncstore/store_admin.go, internal/syncstore/store_scope_blocks.go, internal/syncstore/shortcuts.go, internal/syncverify/verify.go, internal/syncrecovery/recovery.go, internal/cli/verify.go, internal/cli/issues.go, internal/cli/failure_display.go

Implements: R-2.4.4 [verified], R-2.4.5 [verified], R-2.5 [verified], R-2.5.5 [verified], R-2.3.2 [verified], R-2.3.3 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.7 [verified], R-2.15.1 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.4 [verified], R-2.10.5 [verified], R-2.10.14 [verified], R-2.10.22 [verified], R-2.10.32 [verified], R-2.10.33 [verified], R-2.10.34 [verified], R-2.10.41 [verified], R-2.10.45 [verified], R-2.14.3 [verified], R-2.14.5 [verified], R-6.6.11 [verified], R-6.7.17 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## SyncStore (`store.go`)

`SyncStore` is the sole durable authority for sync state. It owns baseline,
remote observation state, conflict facts, durable conflict-resolution
requests, held-delete approval state, per-item failures, persisted scope
blocks, shortcut metadata, and sync metadata. Runtime watch state is never a
peer authority; it is rebuilt from store state when the engine starts.

## Ownership Contract

- Owns: Durable sync state in SQLite, transactional state transitions, conflict/failure/scope persistence, and restart/recovery records.
- Does Not Own: Graph calls, sync-root filesystem probing, failure classification policy, or multi-drive lifecycle.
- Source of Truth: The embedded goose migration history and the SQLite rows it defines.
- Allowed Side Effects: SQLite reads/writes and schema application only.
- Mutable Runtime Owner: `SyncStore` owns its writable DB handle and internal rebuildable baseline cache. `Inspector` and the package-level read helpers own short-lived read-only DB handles. Neither runs background goroutines; all store boundaries expose synchronous methods only.
- Error Boundary: The store persists already-classified failure roles and categories from [error-model.md](error-model.md). It does not reinterpret raw external errors into new policy classes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| One-shot projection readers use store-owned read-only helpers while broader admin readers still use `Inspector`, so CLI status/issues/conflicts/auth projection and daemon status all consume the same read boundary without opening writable store handles. | `TestReadStatusSnapshot`, `TestReadDurableIntentCounts_ReadOnlyDB`, `TestHasScopeBlockAtPath`, `TestListConflictsAtPath`, `TestInspector_ReadIssuesSnapshot`, `TestInspector_ReadStatusSnapshot_StaysConsistentWithIssuesSnapshot`, `TestOrchestrator_ControlSocket_StatusCountsUseReadOnlyInspector` |
| Status issue-group projection preserves separate entries when the same summary key appears in multiple scopes. | `TestQuerySyncState_PreservesIssueGroupScopeContext`, `TestPrintStatusJSON_KeepsSameSummaryGroupsSeparatedByScope`, `TestPrintSyncStateText_KeepsSameSummaryGroupsSeparatedByScope` |
| Integrity inspection and safe repair stay store-owned, deterministic, and durable-intent aware. | `TestInspector_AuditIntegrityReportsPersistedProblems`, `TestSyncStore_AuditIntegrityReportsDurableIntentWorkflowProblems`, `TestSyncStore_RepairIntegritySafeNormalizesDeterministicViolations`, `TestSyncStore_RepairIntegritySafePreservesDurableUserIntent` |
| Held-delete approval identity requires matching item ID and repeated approvals are idempotent. | `TestSyncStore_UpsertHeldDeletesRequiresItemID`, `TestSyncStore_HeldDeleteConsumeRequiresMatchingItemID`, `TestSyncStore_DeleteHeldDeleteRequiresMatchingItemID`, `TestSyncStore_ApproveHeldDeletesConcurrentCallsAreIdempotent` |
| Conflict request concurrency is first-writer-wins until engine claim completes. | `TestSyncStore_RequestConflictResolutionSameStrategyIsIdempotent`, `TestSyncStore_RequestConflictResolutionFirstWriterWinsConcurrently`, `TestSyncStore_RequestConflictResolutionRejectsResolvingAndResolved` |
| Visible issue grouping comes from one store-owned projection instead of CLI-local reconstruction. | `TestSyncStore_ListVisibleIssueGroups` |
| Crash-shaped `downloading` / `deleting` residue in the state DB is replayed from durable store truth on the next live sync run, and the immediate rerun is idle. | `TestE2E_Sync_CrashRecovery_ReplaysDurableInProgressRows` |
| Store schema migration uses embedded goose migrations, preserves durable user intent when migrating from schema v1, and rejects missing, empty, or malformed goose history instead of silently guessing. | `TestNewSyncStore_AppliesCurrentGooseMigration`, `TestSyncStore_MigrationProviderFreshDBUpgradesToCurrent`, `TestSyncStore_MigrationFromVersionOnePreservesDurableIntentSemantics`, `TestSyncStore_MigrationFilesIncludeUpDownAndMatchCurrentVersion`, `TestNewSyncStore_RejectsUnversionedExistingStateDB`, `TestNewSyncStore_RejectsEmptyGooseHistory`, `TestNewSyncStore_RejectsMalformedGooseHistoryWithoutLegacyTables`, `TestNewSyncStore_RejectsMalformedGooseHistoryWithLegacyTables`, `TestNewSyncStore_AcceptsPreexistingAppliedGooseHistory` |

`NewSyncStore()` opens SQLite in WAL mode and applies the embedded goose
migrations from
[`internal/syncstore/migrations`](/Users/tonimelisma/Development/onedrive-go/internal/syncstore/migrations)
through [`schema.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncstore/schema.go).
Fresh databases start at migration `00001_init.sql` and record applied
versions in goose's `goose_db_version` table. Existing stores that already
carry valid goose history are advanced by the migration runner. Existing stores
with user tables but no goose history are rejected with a clear rebuild/migrate
message. Existing stores with a preexisting `goose_db_version` table are
accepted only when that table has at least one applied goose version row that
can be read through the expected schema. Empty goose tables, malformed goose
tables, and broken goose rows are all explicit `ErrIncompatibleSchema`
conditions, even when no other user tables are present. This is deliberate: the
state DB now contains durable user intent, not only rebuildable derived cache,
so unknown schema shape must fail loudly instead of being guessed into a newer
lifecycle.

Key operations:

- `CommitObservation()` atomically writes `remote_state` rows and advances the relevant delta token.
- `CommitOutcome()` updates `baseline` and finalizes remote-state transitions per action. Success-side `sync_failures` cleanup is engine-owned and happens before or after the store commit depending on the result flow.
- `ApplyScopeState(ctx, req)` is the durable sync-scope transition boundary.
  It upserts the singleton `scope_state` row and atomically re-evaluates
  existing `remote_state` rows against the effective snapshot, marking
  out-of-scope rows `filtered` and reactivating in-scope filtered rows.
- `UpsertSyncMetadataEntries(ctx, entries)` remains the generic sync-metadata
  writer for status/report metadata only; sync-scope durability no longer
  piggybacks on generic metadata keys.
- `RefreshLocalBaseline(ctx, LocalBaselineRefresh)` is the explicit reconciliation path used when local disk now represents the chosen truth without a new executor transfer result. It updates only the local-side baseline tuple, preserves known remote-side metadata/`etag`, and marks a matching `remote_state` row synced.
- `RequestConflictResolution(ctx, id, resolution)` records conflict-resolution intent without executing side effects. The durable request workflow lives in `conflict_requests`, while `conflicts` remains the derived fact/history table. `ClaimConflictResolution`, `MarkConflictResolutionFailed`, and `ResolveConflict` enforce the durable `resolution_requested -> resolving -> resolve_failed` workflow for engine-owned execution. Successful resolution deletes the request row in the same transaction that marks the conflict resolved in `conflicts`.
- `UpsertHeldDeletes`, `ApproveHeldDeletes`, `ListHeldDeletesByState`, `ConsumeHeldDelete`, and `DeleteHeldDelete` own the durable delete-safety approval workflow. Approval changes rows from `held` to `approved`; successful engine deletes consume approved rows only when drive, action type, path, and item ID match. Engine planning prunes stale approved rows when the current plan proves a reused path now targets a different item ID.
- `RecordStaleHeldDeletePrune(ctx, count, at)` records store-owned audit metadata when the engine prunes stale approved held-delete rows after a live plan proves a reused path now points at a different item ID.
- `RecordFailure(ctx, SyncFailureParams, delayFn)` is the single failure writer. The engine provides classification and retry policy; the store provides transactional persistence and conflict-safe upsert behavior.
- `TakeSyncFailure(ctx, path, driveID)` is the store-owned read-and-delete boundary for engine-managed failure resolution logging. It returns the authoritative persisted row, including `failure_count`, in the same transaction that removes it.
- `ResetDownloadingStates(ctx, delayFn)`, `ListDeletingCandidates(ctx)`, and `FinalizeDeletingStates(ctx, deleted, pending, delayFn)` are the state-only crash-recovery primitives. The store no longer probes the sync-root filesystem itself. Their bridge rows preserve the exact replay action (`ActionDownload` for reset downloads, `ActionLocalDelete` for reset deletes) so the engine can re-enter the correct execution lane on the next pass.
- `ReleaseScope(ctx, scopeKey, now)` is the single durable “scope resolved” transition. It deletes the `scope_blocks` row when one exists, deletes any legacy `boundary` failure row for that scope, and converts all `held` failures for that scope into retryable `item` rows with `next_retry_at = now`.
- `DiscardScope(ctx, scopeKey)` is the single durable “scope and blocked work are gone” transition. It deletes the `scope_blocks` row when one exists and every `sync_failures` row for that scope.

All write methods are transactional. SQLite WAL mode plus a single writer
connection gives crash-safe durability without introducing another source of
truth.

Transactional write paths use one shared rollback-finalization helper. On
return, it always calls `Rollback()`, suppresses only `sql.ErrTxDone` after a
successful commit, and joins any real rollback failure into the returned
error. The store therefore never silently discards rollback failures at the
durable state boundary.

For file rows, `CommitOutcome()` persists the comparison tuple the planner
needs later:

- local side: `local_hash`, `local_size`, `local_mtime`
- remote side: `remote_hash`, `remote_size`, `remote_mtime`, `etag`

The store does not fabricate hashes or synthesize fallback decisions. It
persists exactly what observation and execution learned, including known zero
sizes. Comparison policy stays in the planner.

For sync-scope filtering, `remote_state.sync_status = filtered` is the durable
"currently outside effective sync scope" marker. It is intentionally distinct
from `deleted` and from retryable download states:

- filtered rows stay visible to the engine for later re-entry reconciliation
- filtered rows are excluded from unreconciled/retry projections
- an in-scope observation moves a filtered row back to `synced` or
  `pending_download` based on hash equality
- `filter_generation` records which effective scope generation last filtered
  the row
- `filter_reason` records whether the row is filtered by `path_scope` or
  `marker_scope`

### `scope_state`

One row represents the durable sync-scope projection for the current sync root.

Important columns:

- `generation`
- `effective_snapshot_json`
- `observation_plan_hash`
- `observation_mode`
- `websocket_enabled`
- `pending_reentry`
- `last_reconcile_kind`
- `updated_at`

`scope_state` is the durable authority for restart-safe scope recovery and for
restart-safe filtered-row repair. It replaces the earlier convention where the
engine serialized the effective snapshot into generic `sync_metadata`.

`NewSyncStore()` runs only the deterministic scope-state repair needed for
runtime correctness after schema application. Scope-state drift is
store-owned: on open, the store normalizes filtered `remote_state` rows
against the current `scope_state` row, and if the stored snapshot is
unreadable it drops that broken authority and reactivates filtered rows so the
next engine run can rebuild scope truth cleanly.

Interrupted scope transitions are repaired at the same store boundary. If a
crash advances `scope_state.generation` before row-level filtered/reactivated
state is fully reconciled, store-owned repair re-evaluates `remote_state`
against the persisted snapshot and fixes rows that should no longer be
`filtered`. Valid `pending_reentry` state is preserved across restart; the
engine remains the only owner that can consume that persisted signal and clear
it after a successful re-entry reconciliation.

`CommitOutcome()` classifies baseline mutations through one shared
ActionType-to-mutation mapping before any transaction writes happen. Unknown
actions are explicit store errors, not silent no-ops. The in-memory baseline
cache uses that same classifier after commit; if that path ever encounters an
impossible unclassified action anyway, the store invalidates and reloads the
cache from SQLite so durable state remains the only authority.

`RefreshLocalBaseline()` is deliberately narrower than `CommitOutcome()`. It
exists for local reconciliation paths such as `keep_both`, where the
engine has authoritative current local disk facts but is not committing a new
executor-produced remote result. The method preserves the remote-side
comparison tuple for existing rows, creates unknown remote-side fields for
local-only placeholder rows, and converges `remote_state` in the same
transaction so the next sync does not rediscover stale remote work.

## Canonical Schema Migrations

The schema is defined by embedded goose migrations. The important tables for
failure and scope management are:

### `sync_failures`

One row represents one concrete path/item failure state.

Important columns:

- `path`, `drive_id`, `direction`, `action_type`
- `category` = `transient` or `actionable`
- `failure_role` = `item`, `held`, or `boundary`
- `issue_type`
- `next_retry_at`
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
`throttle:target:*` or `service` scope should survive restart. Legacy
`throttle:account` rows are treated as stale data and released during repair
rather than preserved or migrated.

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
- one `throttle:target:drive:*` scope can block only that drive, while one `throttle:target:shared:*` scope can block only that shared boundary
- one `quota:shortcut:*` scope can outlive many individual path failures
- one `auth:account` scope can represent an account-level authorization stop without fabricating a path-level failure row

## Store Interfaces (`store_interfaces.go`)

Typed sub-interfaces enforce transition ownership at compile time. Callers get
the narrowest store surface they need.

`SyncStore.Load()` uses a cache-through baseline strategy: the store caches the
most recently loaded baseline in memory, invalidates it before outcome commits,
and rebuilds it after writes. That cache is internal to the store and is
rebuildable from durable state; it is not a competing authority.

Callers are expected to depend on narrow store-owned interfaces rather than a
raw `*SyncStore` whenever they only need one slice of functionality. One-shot
projection readers use package-level read helpers, multi-read administrative
readers use `Inspector`, runtime mutation paths use the writable store, and
tooling opens the smallest boundary that can answer its question.

## Read-Only Inspection

`Inspector` is the read-only companion to `SyncStore`. It is opened only from
`internal/syncstore` and gives administrative readers a narrow projection of
state without handing them raw SQL ownership. Package-level helper functions
such as `ReadStatusSnapshot`, `ReadIssuesSnapshot`, `ReadDurableIntentCounts`,
`HasScopeBlockAtPath`, and `ListConflictsAtPath` are the default one-shot
projection entrypoints. They open one read-only inspector, run one store-owned
query/projection, and close it before returning so callers do not own DB
lifecycle just to answer one question.

- `OpenInspector(dbPath, logger)` opens SQLite in read-only mode.
- `ReadStatusSnapshot(ctx, dbPath, logger)` is the one-shot status reader used
  by CLI status paths that need one projection and nothing else.
- `ReadIssuesSnapshot(ctx, dbPath, history, logger)` is the one-shot `issues`
  projection reader used by `issues list`.
- `ReadDurableIntentCounts(ctx, dbPath, logger)` is the one-shot durable-intent
  projection reader used by daemon status aggregation.
- `HasScopeBlockAtPath(ctx, dbPath, key, logger)` is the one-shot auth/scope
  probe used by offline auth-health projection and similar exact lookups.
- `ListConflictsAtPath(ctx, dbPath, history, logger)` is the one-shot conflict
  listing reader used by `conflicts` list/history.
- `HasScopeBlock(ctx, key)` provides an exact read-only scope-block probe for
  CLI auth-health and other administrative readers that need one persisted
  signal without opening the writable `SyncStore` path.
- `ReadDurableIntentCounts(ctx)` is the narrow read-only durable-intent
  projection used by daemon `GET /v1/status` and other status-only readers
  that need queued/resolving/failed workflow counts without paying writable
  store lifecycle side effects.
- `ReadStatusSnapshot(ctx)` returns metadata, aggregate counts, durable-intent
  counters, and one derived `IssueSummary`. It builds those durable-intent
  counters through the same `ReadDurableIntentCounts` helper used by the
  control socket.
- `ReadIssuesSnapshot(ctx, history)` returns the full read-only `issues`
  projection: grouped visible issue families, held deletes, pending retries,
  and conflict history.
- `IssueSummary.Groups` are keyed by the shared
  [`synctypes.SummaryKey`](../../internal/synctypes/summary_keys.go),
  not by raw SQL categories. Each group also carries the normalized scope kind
  plus an optional humanized scope label so CLI `status` can show file,
  directory, shortcut/drive, account, or service context without reopening raw
  tables.
- `IssueSummary.VisibleTotal()`, `ConflictCount()`, `ActionableCount()`,
  `RemoteBlockedCount()`, `AuthRequiredCount()`, and `RetryingCount()` are the
  read-only status contract. CLI `status` consumes those helpers instead of
  reconstructing visible-issue semantics from raw counters.
- `StatusSnapshot.DurableIntents` reports approved held-delete counts plus
  queued/resolving/failed conflict-request counts from `conflict_requests`.
  CLI `status` turns those store-owned counters into human text, JSON fields,
  and next-step hints without opening writable store paths.
- Control-socket `GET /v1/status` uses the same read-only durable-intent
  helper instead of opening a writable `SyncStore`, so status probes do not
  trigger writable-store close/checkpoint work.
- `StatusSnapshot` and `IssuesSnapshot` share one visible-issue projection
  builder inside `Inspector`, so `status` and `issues` cannot silently drift
  on what counts as a visible issue family.
- CLI `status`, `issues list`, `conflicts` list/history, and offline auth
  projection consume store-owned read helpers; they do not build their own
  DSNs or call `sql.Open` directly.
- `AuditIntegrity(ctx)` is the read-only integrity surface. It reports stable
  finding codes for impossible scope shapes, invalid auth timing, impossible
  retry/trial timing, scope/failure contradictions, visible-projection overlap,
  and other persisted-state violations without mutating the DB.

## Integrity Audit And Safe Repair

The store owns sync-state integrity inspection and deterministic safe repair.

- `Inspector.AuditIntegrity(ctx)` is the pure read-only path used by tests and
  administrative readers.
- `SyncStore.AuditIntegrity(ctx)` adds writable-store-only signals such as
  baseline-cache consistency after loading the cache.
- `SyncStore.RepairIntegritySafe(ctx)` applies only repairs that do not guess
  user intent: it normalizes illegal `auth:account` timing fields, clears
  impossible retry timestamps on non-retryable rows, normalizes deterministic
  scope-state drift, and removes legacy persisted `perm:remote` scope/boundary
  authorities that are now derived from held rows. It must not silently delete
  approved held deletes or queued conflict-resolution requests.
- Durable-intent integrity findings include invalid `conflict_requests`
  workflow states, missing request/resolving timestamps, request rows that
  point at missing or already-resolved conflicts, resolved conflict facts with
  `resolution='unresolved'`, invalid held-delete states, missing held-delete
  item IDs, and approved held deletes without `approved_at`.
- `cmd/devtool state-audit` is the human/JSON administrative entrypoint. It
  opens the database through `syncstore`, reports the store-owned
  `IntegrityReport`, optionally runs `RepairIntegritySafe`, then reruns the
  audit and exits non-zero if findings remain.

The audit/repair split is deliberate: normal runtime code never auto-repairs
durable state during ordinary sync execution beyond the narrow scope-state
normalization required to make persisted scope ownership self-consistent on
open. All broader safe repair stays explicit, and both inspection and repair
stay inside the store-owned boundary.

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

`issues list` reads one store-owned `IssuesSnapshot` through
`syncstore.ReadIssuesSnapshot`.
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
`scope_blocks` through `syncstore.HasScopeBlockAtPath`, but it combines that
store-backed signal with token and account-profile discovery instead of
replacing either source of truth.
Offline/read-only surfaces only project the stored auth block. Live proof
surfaces may clear `auth:account` after a successful authenticated Graph
response for the account.
