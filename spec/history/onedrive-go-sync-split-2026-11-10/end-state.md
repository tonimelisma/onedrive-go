# `internal/sync` split: the end state and every dependency

Pinned to onedrive-go `70301c0e` (`main`, 2026-11-10). Nothing in onedrive-go has
been changed; this is the design and the evidence that it is legal Go.

`internal/sync` is one package today: 169 production files (47,659 lines) and 193
test files (64,895 lines), every test in `package sync`. The end state is seven
production packages in a strict one-way import graph, plus two test-helper
packages.

**Status of the proof.** Every reference in the type-checked package (production
and test code, Linux and macOS file sets) was attributed from the declaration
that makes it to the declaration it names, and checked against the package each
declaration ends up in. Result for both file sets: 0 references outside the
allowed imports, 0 import cycles, 0 interface problems, 0 illegal test
references. A second tool checked the hazards a reference ledger cannot see
(struct conversions, anonymous structs, shadowed package names, name
collisions): none. This is a model, not a compiled split tree. What it does not
cover is listed in section 14.

For contrast, turning today's file-name families into packages as they stand
gives 13 import cycles and 119 illegal references (`naive/report.txt`).

## 1. Packages

| Directory | Package | Holds | Production decls / lines | May import (production) | Who may import it |
|---|---|---|---|---|---|
| `internal/sync/syncdomain/` | `syncdomain` | Vocabulary: row and action types, enums, sentinel errors, scope keys, content filter, sync-root identity, clock and IO interfaces, truth-status index | 492 / 3,561 | nothing in the sync tree | everyone |
| `internal/sync/syncshortcut/` | `syncshortcut` | Shortcut-root state machine and its pure planners, protected-root types | 242 / 2,661 | `syncdomain` | everyone |
| `internal/sync/syncstore/` | `syncstore` | SQLite store, including shortcut-root persistence | 421 / 7,937 | `syncdomain`, `syncshortcut` | everyone |
| `internal/sync/internal/syncplan/` | `syncplan` | Planner | 181 / 3,093 | `syncdomain` | the sync tree only |
| `internal/sync/internal/syncobserve/` | `syncobserve` | Local and remote observers, scanner, socket.io wake source | 442 / 8,370 | `syncdomain`, `syncshortcut` | the sync tree only |
| `internal/sync/internal/syncexec/` | `syncexec` | Executor, worker pool, dependency graph, scope state | 276 / 4,925 | `syncdomain` | the sync tree only |
| `internal/sync/` | `sync` | Engine: wires the stages, runtime, watch loop, permissions, result classification, the shortcut-root lifecycle on disk | 754 / 12,503 | all six | cli, multisync, e2e (as today) |
| `internal/sync/internal/syncfixture/` | `syncfixture` | Shared test fixtures (mocks, clocks, builders) | test helpers only | `syncdomain` | tests in the sync tree |
| `internal/sync/internal/syncstoretest/` | `syncstoretest` | Test store construction and seeding | test helpers only | `syncdomain`, `syncstore`, `syncfixture` | tests in the sync tree |

(macOS file set: `syncobserve` 439 / 8,263 and `sync` 754 / 12,500; the rest is identical.)

```
syncdomain  <-  syncshortcut  <-  syncstore
     ^               ^
     |               +----------  syncobserve
     +--  syncplan
     +--  syncexec
sync imports all six. Plan, observe and exec never import the store or each other.
```

Three decisions sit in this table:

- **Plan, observe and exec are private** (`internal/sync/internal/...`). No package
  outside the sync tree uses any of their symbols today (section 12), so Go's
  `internal` rule can enforce it.
- **No stage imports the store in production.** The engine is the only place the
  store meets a stage. Exec reaches the store through its own six-method
  interface (section 4); observe reaches it through the hash-cache interface.
- **No re-export layer in `sync`.** Each consumer imports the package that owns
  the symbol. Aliases in `sync` would hide the boundaries the split exists to
  create.

Where every production file goes: section 15.

## 2. The 13 production import edges

Distinct symbols / reference sites, and how many of those symbols are exported
today. Per-symbol list: `linux/edges.tsv`.

| Edge | Symbols / sites | Exported today / to export | What crosses |
|---|---|---|---|
| `syncshortcut` → `syncdomain` | 19 / 41 | 12 / 7 | Content filter (config, constructor, path normalisation, `isAncestorPath`), `localStateRow` and its file identity, `ItemTypeFolder` |
| `syncstore` → `syncdomain` | 286 / 1,512 | 241 / 45 | Row, baseline and action types; enums and their parsers; scope keys; issue types; truth-status index |
| `syncstore` → `syncshortcut` | 43 / 80 | 35 / 8 | Shortcut-root record types; the pure planners the store runs inside its transactions (`planShortcutRootTopology`, `planShortcutRootArtifactCleanupAck`, `planShortcutRootDrainReleasePending`, `normalizeShortcutRootRecord`, the two snapshot/status projections); the topology batch |
| `syncplan` → `syncdomain` | 156 / 984 | 122 / 34 | Actions, rows, baseline, enums, delete-safety types, `errDependencyCycle` |
| `syncobserve` → `syncdomain` | 170 / 957 | 115 / 55 | Rows, observed items, content filter, sync-root identity, clock, hash-cache interface, seven sentinel errors |
| `syncobserve` → `syncshortcut` | 48 / 108 | 29 / 19 | The topology batch and its binding upsert/delete/unavailable entries (the remote observer emits them); protected roots, their events and the event sink (the local observer honours them) |
| `syncexec` → `syncdomain` | 219 / 1,536 | 163 / 56 | Actions, outcomes, rows, scope keys, content filter, sync-root identity, two sentinel errors |
| `sync` → `syncdomain` | 341 / 1,752 | 213 / 128 | The whole vocabulary |
| `sync` → `syncshortcut` | 147 / 307 | 92 / 55 | Planners and their result types for the lifecycle the engine carries out on disk |
| `sync` → `syncstore` | 101 / 164 | 51 / 50 | `SyncStore` and its methods; scope-trial policy; the planning read (see note) |
| `sync` → `syncplan` | 13 / 13 | 7 / 6 | `newPlanner`, `planner.PlanCurrentState`, `plannerMountContext`, `deleteSafetyInput`, `applyDeleteSafetyHold`, `countByType` |
| `sync` → `syncobserve` | 119 / 326 | 43 / 76 | Observers and their constructors, the two batch types, scan result, dirty buffer, socket.io wake source and its lifecycle events |
| `sync` → `syncexec` | 44 / 58 | 19 / 25 | `executorConfig`, `depGraph`, `scopeState`, `workerPool` and their constructors; action-freshness check; scope lookups; two error types; two sentinel errors |

Sum of distinct (edge, symbol) pairs: 1,706.

**Note on `sync` → `syncstore`.** 17 of the 50 identifiers to export are SQL-level
helpers used by one function pair, `engineFlow.loadCurrentInputs` /
`loadCurrentInputsTx` (`engine_current_plan.go:496–584`): `beginPerfTx`,
`sqlTxRunner`, `SyncStore.db`, `listLocalStateRows`, `readObservationStateTx`,
`queryRemoteStateRowsWithRunner`, `sqlSelectRemoteStateCols`, the three
`filter…ForPlanning` functions, `replacePlannerVisibleStateTx`, the two
`plannerVisible…Table` constants, the two `query…WithRunnerForTables` functions,
`queryObservationIssueRowsWithRunner` and `loadBaselineWithRunner`. The proven
configuration exports them. The tighter form is to move that 90-line read into
the store as one method that takes the content filter and returns the six
result fields (all `syncdomain` types); the graph does not change. That is a
small rewrite, so the model cannot check it mechanically, and two tests call
the `Tx` form directly (`engine_baseline_snapshot_test.go:36`,
`engine_run_once_test.go:340`).

## 3. Declarations that leave their file family (111)

A reference that points against the graph is fixed by moving the declaration it
names, never by adding an import. 111 declarations end up in a package other
than the one their file name suggests. 6 files move whole, 19 files are split.
Ledger: `linux/moves.tsv`.

### 3.1 Into `syncdomain` (61): vocabulary that more than one package reads

| From | What | Why |
|---|---|---|
| store family | Whole `store_types.go` (16 declarations: `localStateRow`, `remoteStateRow`, `observedItem`, `RetryWorkRow`, `RetryWorkFailure`, `ObservationIssue`, `ObservationIssueRow`, `ObservationFindingsBatch`, `localFileIdentity`, the `fileIdentity`/`recordIdentity` methods, the three `localRow…Baseline…` predicates) | Row shapes are read by every package |
| store family | Whole `truth_status.go` (12 declarations: `truthAvailabilityIndex` and its methods, `pathTruthStatusForPath`, the `observationIssueBlocks…Truth` predicates, …) | The planner reads it; the planner must not import the store |
| store family | `PendingCreate` (`store_pending_creates.go`), `observationState` and the three `localTruthRecovery…` constants (`store_observation_state.go`), `mutationFromActionOutcome` and `actionItemType` (`store_write_baseline.go`), `RetryWorkRow.WorkKey` (`store_retry_work.go`) | They are the parameter and result types of the store methods exec calls through its interface |
| store family | `localHashCache`, `localHashCacheEntry` (`store_local_hash_cache.go`) | The interface the observer consumes and the store implements |
| store family | `sQLiteComparisonRow`, `sQLiteReconciliationRow` (`sqlite_compare.go`) | Planner input |
| observe family | Whole `observation_local_rules.go` (`LocalObservationRules`); `syncRootIdentity` with `Expected` and `Validate`, `newSyncRootIdentity`, `fixedSyncRootIdentity`, `validateExpectedSyncRootIdentity`, `withRefusalHint`, `errSyncRootRebindHint`, `resolvedSyncRoot`, `CountSyncFolderFiles` (`observation_root_identity.go`); `MinPollInterval`, `nfcNormalize` (`observer_remote.go`); `fileChangeTime` (`local_hash_reuse.go`) | The identity is read by observe, exec and the engine; `LocalObservationRules`, `CountSyncFolderFiles` and `MinPollInterval` are used by cli and multisync, so they cannot sit in a private package; the two helpers are used by observe and exec |
| plan family | `deletedItemType`, `resolveItemType` (`planner.go`) | Used by exec and the store |
| exec family | `ConflictStemExt` (`executor_conflict.go`) | Used by e2e |
| engine family | `localObservationManagedIssueTypes`, `newLocalObservationFindingsBatch`, `newRemoteObservationFindingsBatch` (`observation_findings.go`); method `resultDecision.retryWorkScope` (`engine_runtime_completion.go`) | The constructors only name issue types, and the store's tests build batches; a method lives with its receiver type |

### 3.2 Into `sync` (30): code that needs the engine

- 26 `Engine` methods written in shortcut-family files: all of
  `shortcut_alias_mutation.go` (3), 3 in `protected_roots.go`
  (`currentProtectedRoots`, `refreshProtectedRootsFromStore`,
  `setProtectedRoots`) and 20 in `shortcut_root_lifecycle.go` (materialise, move,
  remove, reconcile and release of the shortcut root on disk). Go requires a
  method to be declared in its receiver's package. `spec/design/sync-engine.md`
  already governs `shortcut_alias_mutation.go` and `shortcut_root_lifecycle.go`.
- `shortcutRootCleanupBlockedReport`, `shortcutRootLocalReconcileStep`: used only
  by two of those methods, so they move with them instead of being exported.
- `ApplyShortcutChildRunCommandToEngineMountConfig` (`shortcut_topology.go`): it
  writes the engine's mount configuration.
- `resolveExpectedSyncRoot` (`observation_root_identity.go`): it takes a
  `*SyncStore` and calls `ReadObservationState` and `baselineEntryCount`, and
  observe may not import the store. Its one caller is the engine.

### 3.3 Into `syncstore` (13): SQL

- Whole `shortcut_root_store.go` (10 declarations: `replaceShortcutRoots`,
  `applyShortcutTopology`, `listShortcutRoots`, the two acknowledge methods,
  `ShortcutChildWorkSnapshot`, the query and scan helpers) and
  `shortcut_root_status_read.go` (2). They are `SyncStore` methods and SQL.
- `SyncedFileCount` (`observation_root_identity.go:251`): a store query, used by
  cli (`drive_rebind_root.go:60`).

### 3.4 Into `syncobserve` (7): observer behaviour written in engine files

- `localWatchHealthySafetyScanInterval`, `localWatchRetryInitialDelay`,
  `localWatchRetryMaxDelay` (`engine_watch.go`): they set the local observer's
  behaviour, and the observer's own tests use them.
- Four `remoteObservationBatch` methods: `deferredProgress`,
  `hasObservationFindings` (`engine_primary_root.go`), `finishApplied`,
  `waitApplied` (`engine_observation_postprocess.go`). Methods live with their
  type.

### 3.5 Files that are split (lines per package)

| File | Split |
|---|---|
| `engine_observation_postprocess.go` | sync 213, syncobserve 22 |
| `engine_primary_root.go` | sync 206, syncobserve 30 |
| `engine_runtime_completion.go` | sync 799, syncdomain 11 |
| `engine_watch.go` | sync 683, syncobserve 5 |
| `executor_conflict.go` | syncexec 277, syncdomain 18 |
| `local_hash_reuse.go` | syncobserve 57, syncdomain 14 |
| `observation_findings.go` | sync 58, syncdomain 28 |
| `observation_root_identity.go` | syncdomain 131, sync 53, syncobserve 25, syncstore 12 |
| `observer_remote.go` | syncobserve 949, syncdomain 6 |
| `planner.go` | syncplan 335, syncdomain 62 |
| `protected_roots.go` | syncshortcut 73, sync 36 |
| `shortcut_root_lifecycle.go` | sync 538, syncshortcut 147 |
| `shortcut_topology.go` | syncshortcut 386, sync 17 |
| `sqlite_compare.go` | syncstore 761, syncdomain 28 |
| `store_local_hash_cache.go` | syncstore 77, syncdomain 18 |
| `store_observation_state.go` | syncstore 568, syncdomain 21 |
| `store_pending_creates.go` | syncstore 232, syncdomain 12 |
| `store_retry_work.go` | syncstore 453, syncdomain 4 |
| `store_write_baseline.go` | syncstore 953, syncdomain 54 |

Whole files that move away from their family: `observation_local_rules.go` →
syncdomain (10 lines); `shortcut_alias_mutation.go` → sync (78);
`shortcut_root_status_read.go` → syncstore (31); `shortcut_root_store.go` →
syncstore (245); `store_types.go` → syncdomain (158); `truth_status.go` →
syncdomain (160).

## 4. Interfaces that replace a concrete dependency

**The executor's view of the store.** Exec names `*SyncStore` at 7 sites
(`action_freshness.go:57, 92, 118, 150, 194`; `worker.go:47` field, `:75`
constructor parameter). In the end state it declares its own interface with the
six methods it calls. `*syncstore.SyncStore` satisfies it as it stands, and
every parameter and result type is in `syncdomain`:

```go
type Store interface { // in syncexec
    CommitMutation(ctx context.Context, outcome *syncdomain.BaselineMutation) error
    GetLocalStateByPath(ctx context.Context, path string) (*syncdomain.LocalStateRow, bool, error)
    GetRemoteStateByID(ctx context.Context, driveID driveid.ID, itemID string) (*syncdomain.RemoteStateRow, bool, error)
    GetRemoteStateByPath(ctx context.Context, path string, driveID driveid.ID) (*syncdomain.RemoteStateRow, bool, error)
    Load(ctx context.Context) (*syncdomain.Baseline, error)
    ReadObservationState(ctx context.Context) (*syncdomain.ObservationState, error)
}
```

The package already does this once (`executor_transfer.go:477–480`).

**The hash cache.** `localHashCache` has two unexported methods, `lookup` and
`record`. It is implemented by `storeLocalHashCache` (store) and by the test
type `mapHashCache` (observe tests). An interface with unexported methods can
only be implemented in its own package, so the interface moves to `syncdomain`
and the methods become `Lookup` and `Record`. The engine asks the store for the
implementation (`SyncStore.newLocalHashCache`, exported) and hands it to the
observer (`SetLocalHashCache`, exported today).

No other interface with unexported methods is implemented outside its end-state
package.

## 5. Unexported struct fields reached from another package (11 structs, 39 fields, 116 sites)

26 fields are exported; 13 stay unexported behind a method.

| Struct (package) | Fields / sites | Resolution |
|---|---|---|
| `remoteObservationBatch` (observe) | 14 / 71, all from the engine | Export the type and all 14 fields. It is a message from observer to engine; the engine reads every field |
| `localObservationBatch` (observe) | 7 / 17 | Export the type and fields; same reason |
| `remoteObservationResult` (observe) | 2 / 3 | Export both fields |
| `localObserver` (observe) | 4 / 4 | `droppedEvents`, `droppedRetries`: stay unexported (exporting collides with the existing `DroppedEvents()` and `DroppedRetries()` methods); `engine_watch_maintenance.go:24–35` restores the counters after a failed flush, so add `RestoreDropped(events, retries int64)`. `safetyScanInterval`, `startupSafetyScan` (written at `engine_watch.go:392–393`): one setter, `SetSafetyScan(interval time.Duration, atStartup bool)` |
| `workerPool` (exec) | 2 / 4 | `perfCollector`, `serialize` are written right after construction (`engine_run_once.go:355–356`, `engine_watch.go:194–195`): one setter called before `Start`, `SetRuntimeOptions(collector *perf.Collector, serialize bool)`. The constructor keeps its signature, so its many test callers do not change |
| `remoteItemGoneError` (exec) | 1 / 3 | Export the field as `ItemID` (read through `errors.As` at `engine_runtime_completion.go:194–198`) |
| `syncRootIdentity` (domain) | 3 / 6 | `bindPending`, `bindMu`, `current` stay unexported. `Engine.commitLocalSnapshot` (`engine.go:355–407`) takes the lock, re-checks, binds, stores the result and clears the flag; that sequence becomes `CompleteBinding(bind func(expected *synctree.RootIdentity) (*synctree.RootIdentity, error)) (ran bool, err error)`. The engine passes the store call as `bind`; `ran == false` means no binding was pending and the engine does the plain replace |
| `resolvedSyncRoot` (domain) | 2 / 4 | Export `expected` and `bindPending`: a plain result struct |
| `deltaExpiredError` (domain) | 2 / 2 | `kind` (one site, `engine_mount_root.go:61`): use the existing `resyncKindOf(...)`. `reason` (`observer_remote.go:289`): add a `Reason()` method. Export the type |
| `SyncStore` (store) | 1 / 1 | `db` (one site, `engine_current_plan.go:501`): add `BeginPerfTx(ctx)`; the field stays unexported |
| `ShortcutChildAckRef` (shortcut) | 1 / 1 | `bindingItemID` is opaque by design. One engine read (`shortcut_root_lifecycle.go:603`): add `Matches(record *ShortcutRootRecord) bool`. No getter |

None of the 9 new names (`RestoreDropped`, `SetSafetyScan`, `SetRuntimeOptions`,
`CompleteBinding`, `Reason`, `BeginPerfTx`, `Matches`, `Lookup`, `Record`) exists
today on its type or in the tree.

## 6. Package-level variables referenced across packages

12 variables are referenced across packages. All are immutable sentinel errors;
no mutable variable crosses a package boundary. One is exported today; the
other 11 are exported with the `Err` prefix, which keeps the `errname` and
`gochecknoglobals` exemptions.

| Variable | Owner | Used from (sites) |
|---|---|---|
| `errActionPreconditionChanged` | syncdomain | syncexec 13, sync 2 |
| `ErrMountRootUnavailable` (exported today) | syncdomain | syncobserve 5, sync 3 |
| `errFileChangedDuringHash` | syncdomain | syncobserve 4 |
| `errLocalWatchUnavailable` | syncdomain | syncobserve 4 |
| `errDeltaExpired` | syncdomain | syncobserve 3, sync 2 |
| `errSyncRootDeleted` | syncdomain | syncobserve 2 |
| `errRemoteObservationIncomplete` | syncdomain | syncobserve 1, sync 1 |
| `errSyncRootMissing` | syncdomain | syncobserve 1 |
| `errDependencyCycle` | syncdomain | syncplan 1 |
| `errPathEscapesSyncRoot` | syncdomain | syncexec 1 |
| `errDatabaseInUse` | syncexec | sync 1 |
| `errRemoteFolderTooLargeToVerify` | syncexec | sync 1 |

## 7. Identifiers to export for production code (444)

| Package | Surface used by other packages | Exported today | To export |
|---|---|---|---|
| syncdomain | 687 | 472 | 215: 93 constants, 46 types, 49 functions, 9 methods, 2 interface methods, 9 variables, 7 fields |
| syncshortcut | 192 | 120 | 72: 20 constants, 13 types, 33 functions, 5 methods, 1 field |
| syncstore | 101 | 51 | 50: 11 constants, 2 types, 21 functions, 15 methods, 1 field |
| syncplan | 13 | 7 | 6: 3 types, 3 functions |
| syncobserve | 119 | 43 | 76: 15 constants, 13 types, 14 functions, 7 methods, 27 fields |
| syncexec | 44 | 19 | 25: 6 types, 13 functions, 1 method, 2 variables, 3 fields |

Per-symbol list: `linux/exports.tsv`. After section 5, 13 of the 39 fields stay
unexported, so 431 existing identifiers are capitalised and 7 methods are added.
The only two capitalisations that collide with an existing name are the two
`localObserver` counters, which section 5 leaves unexported.

## 8. Language-level hazards checked

| Hazard | Finding |
|---|---|
| Methods declared away from their receiver type | 35 such methods; each follows its type (section 3) |
| Unkeyed composite literals of a struct that ends up in another package | none |
| `init` functions | none |
| Const block whose constants end up in two packages | one, `observer_remote.go:26` (`MinPollInterval` to syncdomain, 5 others to syncobserve). Explicit values, no `iota`, no implicit repetition: safe to split |
| Conversions between two different named struct types | none |
| Anonymous struct types with unexported fields outside a function body | one: the parameter of test helper `assertClassifyResultCases` (`engine_result_classify_test.go:155`); its three callers stay in `sync` with it |
| Declared identifier or import alias equal to a new package name | none |
| Build-constrained files | `inotify_other.go` follows `inotify_linux.go` (syncobserve); `permissions_rdonly_bsd.go` follows `permissions_rdonly_linux.go` (sync). The macOS file set was checked separately |
| `go:embed`, `go:generate`, `go:linkname`, cgo | none |
| Blank imports | one: `store_inspect.go` registers the SQLite driver (`modernc.org/sqlite`); it lives in syncstore, which is the only package that opens a database |
| Serialised structs whose field names would change | none (no struct with unexported fields is marshalled) |
| Package identity at run time | `%T` at `enums.go:120` and `:268` only; `slog.String("log_owner", "sync")` is a literal; `dispatch_order_test.go:336–340` uses the text after the last dot of the function name; `observer_local_test.go:35` reflects by field name |

## 9. Imports from outside `internal/sync` (production, sites)

No package below can import `internal/sync` today (the compiler forbids it), so
none can close a cycle through the new packages. Per-symbol list:
`linux/extimports.tsv`.

| Package | Other `internal/` packages | Standard library and modules of note |
|---|---|---|
| syncdomain | driveid 32, graph 23, synctree 22, errclass 7, namekey 2, driveops 1, localtrash 1 | net/http 13, os 2, x/text/unicode/norm 1 |
| syncshortcut | synctree 25, driveid 5, config 4 | os 3 |
| syncstore | driveid 51, synctree 13, localpath 5, driveops 2, perf 2, fsroot 1 | database/sql 80, os 3, modernc.org/sqlite 2 (+ lib 2) |
| syncplan | namekey 3, driveid 1, driveops 1 | none |
| syncobserve | graph 53, synctree 53, driveid 24, retry 16, localpath 12, namekey 8, driveops 5, loglevel 1 | os 24, coder/websocket 18, fsnotify 13, x/sys/unix 12, net/http 10, errgroup 1 |
| syncexec | driveops 29, driveid 24, synctree 23, graph 21, perf 6, localtrash 5, namekey 4, localpath 3 | os 43, net/http 2 |
| sync | errclass 45, driveops 40, graph 24, synctree 24, driveid 19, perf 12, retry 6, localpath 5, config 4, localtrash 4, authstate 2 | os 14, x/sys/unix 8, net/http 2, database/sql 1 |

The planner has no filesystem, network or database import. Only the store
imports the SQLite driver.

## 10. Tests

### 10.1 Rules

1. A test lives in the package whose behaviour it asserts, as an in-package
   `_test.go` file.
2. A test may import any end-state package whose production code does not
   import the test's own package. (Test variants are never imported, so store
   tests importing observe while observe tests import the store is legal.)
3. When a test needs another package's unexported identifier, the fix is, in
   order: move the test; export the identifier if it is ordinary API; add a
   `…ForTest` function in a production file of the owning package; rewrite the
   test.
4. Fixtures used by more than one package live in a helper package. Where a
   package cannot import the helper (the helper imports it), it keeps a private
   copy.

### 10.2 Where the tests end up

| Package | Test files | Test declarations | Lines | Test / Benchmark functions |
|---|---|---|---|---|
| syncdomain | 8 | 45 | 652 | 44 |
| syncshortcut | 4 | 26 | 689 | 23 |
| syncstore | 36 | 299 | 7,423 | 211 / 3 |
| syncplan | 9 | 200 | 3,985 | 72 / 2 |
| syncobserve | 28 | 488 | 11,611 | 341 |
| syncexec | 16 | 290 | 7,990 | 246 |
| sync | 92 | 1,035 | 25,419 | 556 |
| syncfixture (helper) | – | 141 | 1,164 | – |
| syncstoretest (helper) | – | 13 | 177 | – |

39 test files have declarations in more than one package and are split; 10 have
an explicit home. Per-file breakdown: `linux/tests.tsv`.

### 10.3 What each package's tests import beyond their own package (symbols / sites)

| Tests of | syncdomain | syncshortcut | syncstore | syncobserve | syncexec | syncfixture | syncstoretest |
|---|---|---|---|---|---|---|---|
| syncdomain | – | | | | | | |
| syncshortcut | 7 / 18 | – | | | | 1 / 11 | |
| syncstore | 221 / 2,787 | 44 / 230 | – | 3 / 3 | | 15 / 160 | |
| syncplan | 162 / 1,790 | | 28 / 136 | | | 22 / 116 | 2 / 53 |
| syncobserve | 136 / 1,705 | 32 / 56 | 7 / 24 | – | | 59 / 750 | 2 / 9 |
| syncexec | 189 / 3,104 | | 8 / 21 | 3 / 4 | – | 32 / 892 | 2 / 11 |
| sync | 376 / 4,406 | 64 / 414 | 75 / 493 | 61 / 212 | 29 / 190 | 104 / 1,616 | 10 / 58 |
| syncfixture | 59 / 90 | | | | | – | |
| syncstoretest | 25 / 34 | | 10 / 21 | | | 1 / 1 | – |

Every cell is legal under rule 2. The test-only imports are: store tests →
observe (`buildLocalStateRows`, `scanResult`, `scanResult.Rows`); plan, observe
and exec tests → store (they run against a real SQLite store); exec tests →
observe (`timeSleep`, `newLocalObserver`, `localObserver.HandleDelete`). No test
imports `syncplan` from outside it.

### 10.4 Helper packages

- `syncfixture` (imports only `syncdomain`): 85 declarations used from other
  packages, 123 identifiers to capitalise, no collisions. Mocks
  (`engineMockClient`, `executorMockItemClient`, `mockFsWatcher`, …), clocks
  (`manualClock`, `scriptedSleepClock`, `inVirtualTime`), builders
  (`baselineWith`, `testRetryWorkRow`, …), `raceDetectorEnabled` as two
  build-tagged files. Full list: `conf/final.txt`, `testdecl syncfixture` lines.
- `syncstoretest` (imports `syncdomain`, `syncstore`, `syncfixture`): 10
  declarations used from other packages: `newTestStore`, `newTestManager`, the
  template loader, and the seed/read helpers for observation state.
- The existing `internal/synctest` is a different package and keeps its name.
- Private copies. `syncstore` tests keep their own 12 store helpers (159 lines),
  because `syncstoretest` imports `syncstore`. `sync` tests keep copies of
  `newTestExecutorConfig`, `newTestExecutorConfigWithPathConvergence` (originals
  in the exec tests) and `receiveBatch` (original in the observe tests), 37
  lines: a helper that builds an `executorConfig` must import `syncexec`, so the
  exec package's own tests could not import it. A private copy never meets the
  helper's types, so duplication is safe.

### 10.5 The 38 production identifiers that only another package's tests need

**Exported as ordinary API (19).** syncdomain: `strRoot`, `strDownload`,
`syncTicker`, `resyncKindUnknown`, `resyncKindUploadDifferences`,
`truthAvailabilityAvailable`, `truthAvailabilityBlockedObservationIssue`,
`pathTruthSourceObservationIssue`, `realAfterFunc`,
`observationIssueBlocksLocalTruth` (each completes an enum or a group that is
otherwise exported for production). syncstore: `queryHeldDeleteRowsWithRunner`,
`remoteStateDriveID`, `stateStoreIncompatibleReasonOpenFailed`. syncobserve:
`canReuseBaselineHash`, `mountBoundarySkippedItem`, `vanishedMountSkippedItem`.
syncexec: `newExecution`, `remoteFolderDeleteListLimit`,
`serviceWindowThreshold`.

**Reached through a `…ForTest` function (17 identifiers, 16 functions).**

| Identifier (test sites) | Function |
|---|---|
| `SyncStore.rawDB` (sync 49, syncplan 41, syncobserve 7) | `SyncStore.RawDBForTest()` |
| `openSyncStore` (sync 2, syncstoretest 2) | `OpenSyncStoreForTest(...)` |
| `SyncStore.crashAt` (sync 2) | `SyncStore.SetCrashHookForTest(func(point string) error)` |
| `executorConfig.crashAt` (sync 2) | `executorConfig.SetCrashHookForTest(...)` |
| `SyncStore.nowFunc` (sync 1) | `SyncStore.NowForTest()` |
| `executorConfig.nowFunc` (sync 1) | `executorConfig.NowForTest()` |
| `SyncStore.logger` (sync 1) | `SyncStore.SetLoggerForTest(...)` |
| `SyncStore.reloadBaselineCache` (sync 3) | `SyncStore.ReloadBaselineCacheForTest(ctx)` |
| `SyncStore.updateObservationState` (sync 1) | `SyncStore.UpdateObservationStateForTest(...)` |
| `SyncStore.baseline` (sync 2, syncplan 1) and `SyncStore.baselineMu` (syncplan 2) | `SyncStore.DropBaselineCacheForTest()` |
| `upsertBaselineRowTx` (syncplan 1) | `UpsertBaselineRowForTest(ctx, *BaselineEntry) error` |
| `replaceLocalStateTx` (sync 1) | `ReplaceLocalStateTxForTest(...)` |
| `canonicalSchemaSQL` (sync 1) | `CanonicalSchemaSQLForTest()` |
| `executorConfig.pathConvergence` (sync 1) | `executorConfig.PathConvergenceForTest()` |
| `scopeState.windows` (sync 2) | `scopeState.HasWindowForTest(key ScopeKey) bool` |
| `newShortcutChildAckRef` (sync 2, syncstore 2) | `NewShortcutChildAckRefForTest(bindingItemID string)` |

The precedent is `NewBaselineForTest` (`core_types.go:328–330`). No rule guards
it today, so the split adds one devtool check: an identifier ending in `ForTest`
may be referenced only from `_test.go` files and the two helper packages.
(`export_test.go` cannot serve this purpose: it is visible only to the tests of
its own package.)

**Test rewritten, no production change (2).** `uploadTransferError` (one site,
`engine_transfer_integrity_test.go:65–74`): the test is split into an exec half
and an engine half. `nanosPerSecond` (one site): the test writes
`2*int64(time.Second)`.

One test moves for the same reason:
`TestBuildObservationReconcilePlan_ReportsChangeOnlyWhenTheIssueSetDiffers` goes
to `syncstore`.

**Kept in `sync` by design.** Result classification (`classifyResult`,
`issueTypeForResult`, …, `engine_result_classify.go`) is engine policy. Executor
tests that call it, and `TestFault_ContextCancel_WorkerPool`, are executor-plus-
engine integration tests and live in `sync`.

### 10.6 Tests that depend on where files are

| Test | Dependency | Resolution |
|---|---|---|
| `TestReverseDispatchTable_ListsEveryRunOnceScenario` (`dispatch_order_test.go:307–334`) | Globs `*_test.go` in its own directory and requires every `TestRunOnce_*` in the table | All 141 `TestRunOnce_*` functions stay in `sync` with it: no change, and the guard stays complete |
| `TestPanicRecoverySitesCaptureTheStack` (`panic_stack_test.go:24–25`) | Scans `../sync` and `../multisync`, not recursively, for `recover()` sites | Two of the five sites move out of the scanned directory: `scanner.go:279` (syncobserve) and `worker.go:159` (syncexec). Without a change they silently leave the guard. Its roots become every package directory of the sync tree, in the PR that moves those files |
| `reconcile_matrix_test.go:27` | `testdata/reconcile/matrix.golden`, relative to the package directory | The golden file moves to `syncplan` with the test |
| `permissions_rdonly_linux_test.go:19` | `/proc/self/mounts`, absolute | none |
| `testdata/watch_capture/` | no Go reference | stays or is deleted; nothing depends on it |

## 11. Shared state between tests

Tests in one package run in one process; after the split they run in up to
seven. Nothing depends on that: no `TestMain`, no mutable package-level variable
crosses a package boundary (section 6), and the only process-wide test cache is
the store template (`testStoreTemplateOnce`), which each test binary builds
once for itself.

## 12. Code outside `internal/sync` that imports it (identifiers / sites)

| Consumer | syncdomain | syncshortcut | syncstore | sync | plan / observe / exec |
|---|---|---|---|---|---|
| `internal/cli` production | 75 / 209 | 19 / 40 | 21 / 31 | 43 / 143 | none |
| `internal/cli` tests | 86 / 366 | 53 / 109 | 32 / 96 | 32 / 206 | none |
| `internal/multisync` production | 16 / 73 | 35 / 131 | 1 / 1 | 36 / 70 | none |
| `internal/multisync` tests | 12 / 154 | 32 / 364 | 6 / 15 | 30 / 248 | none |
| `internal/syncverify` production | 9 / 25 | | | | none |
| `internal/syncverify` tests | 15 / 165 | | | | none |
| `internal/devtool` production (`bench_live_bidirectional.go:18`) | 1 / 1 | | 3 / 3 | | none |
| `e2e` tests | 1 / 1 | 8 / 13 | 4 / 4 | 4 / 13 | none |

Each consumer changes its import lines and qualifiers; nothing else. `syncverify`
ends up depending on the vocabulary alone. Per-symbol list: `linux/external.tsv`.

## 13. Gates and tooling in onedrive-go

| Dependency | What happens at the split | Resolution |
|---|---|---|
| **Coverage gate** (`internal/devtool/verify.go`: threshold 75.5, `go test -race -coverprofile` over `./...` without `-coverpkg`) | A line is counted as covered only by its own package's tests. Tests in `sync` that today cover store, observer and executor lines stop counting for them, so the total drops with no behaviour change | Before any move, in its own PR: add `-coverpkg=./...` and re-baseline the threshold. From then on the total does not change when code moves between packages. The new number is not measured |
| **Stress lane** (`verify_stress.go:75, 85, 91, 101`) | "sync concurrency race x50" and "sync race x3" target `./internal/sync`, which then holds only the engine. The 209 tests matching the concurrency pattern end up in sync 118, syncobserve 65, syncexec 20, syncstore 6 | Both targets become `./internal/sync/...`. `verify_runner_test.go:780, 791, 802–806, 841, 898` hard-code the command strings and change with them; comments at `verify_stress.go:14, 51` and `verify.go:553` too |
| **depguard** (`.golangci.yml:80–136`) | Rule `driveops-no-sync` (`:119`) matches `internal/sync$` and would stop covering the subpackages. The directions the compiler does not enforce (for example plan must not import the store) have no rule | Widen the pattern to `internal/sync(/|$)`. Add one rule per package directory from the table below, with `"!$test"` where tests may import more |
| **wrapcheck** | Calls that are in-package today become cross-package calls returning an unwrapped error: 33 sites in production code, 21 in tests (`linux/wrapcheck.tsv`) | One exclusion for `path: internal/sync/`, `linters: [wrapcheck]`, `text:` matching a callee signature inside `internal/sync/`; it is anchored on the callee, so calls from cli and multisync into sync stay checked. A second exclusion for the 15 mock function-field sites in tests. `exclusions.warn-unused` is on, so each rule must match at least one finding. Rejected: wrapping all 33 sites (changes error strings); `ignore-package-globs` (loosens cli and multisync) |
| **staticcheck ST1000** | Every package needs a package comment | A `doc.go` in each of the 8 new packages. No lint requires doc comments on the newly exported identifiers |
| **Lints that treat `_test.go` specially** | The two helper packages are not `_test.go` files. Measured exposure: syncfixture has 8 raw `os.*` calls, 10 lines over 140 columns, 17 `defer`s, 15 number literals, 3 undocumented mutexes; syncstoretest has 4 raw `os.*` calls, 1 `defer`, 2 number literals | Path exclusions for `internal/sync/internal/(syncfixture|syncstoretest)/` that mirror the test exemptions (`lll`, `errcheck` on `defer`, the `forbidigo` raw-os message, `mnd`). Extend devtool's test-conventions check to treat the two directories as test code. Add guard comments to the 3 mutexes |
| **nolintlint** (`allow-unused: false`) | Some `//nolint` directives become stale | Remove the ones the lint run reports. Not counted in advance |
| **`unused`, `unparam`** | They stop covering identifiers once exported | Accepted cost |
| **Spec ownership** (`verify_governs.go`: every non-test `.go` file is owned by exactly one `spec/design/*.md` through its `GOVERNS:` line) | Paths change; new `doc.go`, helper and `…ForTest` files need an owner | Each doc points at its directory with a glob: `sync-domain.md` → `syncdomain/*.go`; `sync-shortcut.md` → `syncshortcut/*.go`; `sync-store.md` → `syncstore/*.go`; `sync-planning.md`, `sync-observation.md`, `sync-execution.md` → their `internal/...` directories; `sync-engine.md` → `internal/sync/*.go`; the helper packages → `system.md`, which governs `internal/synctest` today. The cli and syncverify files those docs also govern stay. Ownership that changes hands: `store_types.go`, `truth_status.go`, `observation_local_rules.go` → domain doc; `shortcut_root_store.go`, `shortcut_root_status_read.go` → store doc; `shortcut_root_publication.go` → shortcut doc |
| **Cited-test check** | Backticked test names in `spec/design/*.md` must exist somewhere. It is name-based, so moving a test is free | Only the split of the test at `engine_transfer_integrity_test.go:65–74` can break it: keep or update the cited name |
| **Routing-table check** (backticked paths in onedrive-go `CLAUDE.md` must match a file) | Lines 26–33 list the sync file families; line 86 is the rule "No fake intermediate boundaries" | Rewrite the table to the package directories in the PR that makes each move |
| **Layering check** (`verify_layering.go`, by file family within one package directory) | Each sync directory holds one family, so the check has nothing to compare there. It cannot produce a false positive; it keeps working in `internal/cli` | None. The compiler and depguard now enforce what it enforced by name |
| **Quick-lane shards** (`verify_quick.go`), **requirement traceability** (`verify_requirements.go`) | Shards split by test name across all packages; traceability reads comments | None |
| **CI workflows, `scripts/`, `cmd/`** | No reference to `internal/sync` | None |
| **Comments and docs** | 7 Go comments name `go test ./internal/sync` (`store_planner_visible_bench_test.go:28`, `sqlite_compare_bench_test.go:23`, `planner_dependencies_bench_test.go:24`, `reconcile_matrix_test.go:21`, `engine_model_based_test.go:52`, `synctree/identity.go:78`, `e2e/socketio_helpers_test.go:49`); 24 markdown files mention the path (`system.md` 20, `developer-onboarding.md` 18, `CLAUDE.md` 11, …) | Updated in the PR that moves the code they name. `retired-backlog-ids.md` (36) and `live-incidents.md` (21) are history and stay as written |

**depguard table.**

| Package | Production may import | Tests may also import |
|---|---|---|
| syncdomain | – | – |
| syncshortcut | syncdomain | syncfixture |
| syncstore | syncdomain, syncshortcut | syncobserve, syncfixture |
| syncplan | syncdomain | syncstore, syncfixture, syncstoretest |
| syncobserve | syncdomain, syncshortcut | syncstore, syncfixture, syncstoretest |
| syncexec | syncdomain | syncstore, syncobserve, syncfixture, syncstoretest |
| sync | all six | syncfixture, syncstoretest |
| syncfixture | syncdomain | |
| syncstoretest | syncdomain, syncstore, syncfixture | |

## 14. What is proven and what is not

Proven by the model, both file sets: the package assignment of every
declaration is acyclic and every reference is legal; every interface can be
satisfied; every test can live where it is placed. Proven by the shape check
(Linux file set): no struct conversion, anonymous struct, shadowed name or name
collision breaks the split.

Estimated, not run: the wrapcheck count (33 + 21) and the helper-package lint
exposure come from a replica of the linters' rules, not from golangci-lint. The
`mnd` figure is an upper bound.

Not measured:

- which `//nolint` directives become stale;
- the coverage total under `-coverpkg=./...`;
- test timing when seven test binaries run side by side instead of one (the
  work is the same; timing-sensitive watch tests share the cores differently);
- the shape check on the macOS file set;
- `race_enabled_test.go`, a two-line build-tagged constant file that neither run
  type-checks.

Not modelled because it is a rewrite, not a move: the bodies of the 7 new
methods in section 5, the 16 `…ForTest` functions, the two rewritten tests, and
the optional planning-read move in section 2. Each is small and local; their
names were checked for collisions.

## 15. Where every production file goes

147 files move whole, 19 are split (section 3.5), 2 follow a build-constrained
sibling, and `doc.go` stays.

- **syncdomain (26):** `actions.go`, `condition_keys.go`, `content_filter.go`,
  `core_types.go`, `crash_points.go`, `domain_clock.go`,
  `domain_delete_safety.go`, `domain_graph_error.go`, `domain_io_seams.go`,
  `domain_observation_mode.go`, `domain_path_kind.go`,
  `domain_runtime_decision.go`, `enums.go`, `errors.go`, `issue_types.go`,
  `observation_local_rules.go`, `permission_capability.go`, `retry_work_key.go`,
  `scope_block.go`, `scope_key.go`, `scope_semantics.go`, `store_types.go`,
  `tracked_action.go`, `truth_status.go`, `types.go`, `worker_result.go`
- **syncshortcut (11):** `shortcut_alias_mutation_types.go`,
  `shortcut_protected_root_types.go`, `shortcut_root_planner.go`,
  `shortcut_root_planner_ack.go`, `shortcut_root_planner_cleanup.go`,
  `shortcut_root_planner_local.go`, `shortcut_root_planner_topology.go`,
  `shortcut_root_publication.go`, `shortcut_root_state.go`,
  `shortcut_root_status.go`, `shortcut_root_transition.go`
- **syncstore (26):** `baseline_orphans.go`, `block_scope_rows.go`,
  `blocked_retry_projection.go`, `condition_projection.go`,
  `observation_reconcile_policy.go`, `schema.go`, `scope_lifecycle_policy.go`,
  `shortcut_root_status_read.go`, `shortcut_root_store.go`, `store.go`,
  `store_compatibility.go`, `store_delete_ledger.go`, `store_held_deletes.go`,
  `store_inspect.go`, `store_local_state.go`, `store_observation_issues.go`,
  `store_planner_visible.go`, `store_read_remote_state.go`,
  `store_remote_twins.go`, `store_reset.go`, `store_scope_admin.go`,
  `store_scratch.go`, `store_sql_encoding.go`, `store_write_block_scopes.go`,
  `store_write_observation.go`, `tx.go`
- **syncplan (5):** `planner_actions.go`, `planner_delete_safety.go`,
  `planner_dependencies.go`, `planner_move_composition.go`,
  `planner_truth_overlay.go`
- **syncobserve (16 + `inotify_other.go`):** `buffer.go`, `inotify_linux.go`,
  `item_converter.go`, `local_observation_batch.go`,
  `observation_remote_batch.go`, `observed_items.go`, `observer_local.go`,
  `observer_local_collisions.go`, `observer_local_handlers.go`, `scanner.go`,
  `scanner_hash_cache.go`, `scanner_unavailable.go`, `socketio.go`,
  `socketio_conn.go`, `socketio_protocol.go`, `symlink_observation.go`
- **syncexec (10):** `action_freshness.go`, `active_scopes.go`, `dep_graph.go`,
  `executor.go`, `executor_delete.go`, `executor_parent_checks.go`,
  `executor_preconditions.go`, `executor_transfer.go`, `scope.go`, `worker.go`
- **sync (53 + `permissions_rdonly_bsd.go` + `doc.go`):** `api_types.go`,
  `debug_event_sink.go`, every `engine*.go` file not listed in section 3.5 (42
  files), `permission_evidence.go`, `permission_handler.go`,
  `permission_probe_local.go`, `permission_probe_remote.go`, `permissions.go`,
  `permissions_rdonly_linux.go`, `shortcut_alias_mutation.go`,
  `watch_condition_log.go`, `watch_summary.go`
