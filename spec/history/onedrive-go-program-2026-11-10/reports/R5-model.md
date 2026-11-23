# R5 / S1: the split model re-run on current main

Run 2026-11-20 against onedrive-go `9b16a5c0` (#960), worktree
`~/src/onedrive-go-wt/split-prepare`. The model was pinned to `70301c0e`
(11-10); 97 commits since then changed `internal/sync` in 312 files
(+41,899 / -7,554 lines, 68 files added, 4 removed).

## Verdict

**Production: the target layout still fits.** With the 11-10
`conf/production.txt` unchanged the simulator reports 57 references
outside the allowed imports and 10 cycles. All of them come from code
added after the pin, and five config lines, none of them a new package
or a new edge, bring both file sets back to:

```
Linux:  typecheck errors 0, violations 0, cycles 0, interface problems 0,
        split const blocks 1, name collisions 2
macOS:  the same
import graph: ACYCLIC
```

The lines added to `conf/production.txt` (the 11-10 line naming
`nfcNormalize`, which no longer exists, loses that name):

```
# the comparison and reconciliation kinds are read by the row shapes, the store's scans and the planner
file sqlite_compare_kinds.go syncdomain
# the stored-time encoding is read by exec, observe and PendingCreate; the intent lifetime by PendingCreate
decl syncdomain storedTimeIsSet remoteModTime storedTimeNano storedEpochNano pendingCreateIntentTTL
# remoteItem replaced changeEvent, which was vocabulary; the store's orphan finders return it
decl syncdomain remoteItem
```

What caused the 57:

| New code | Violations it caused | Resolution |
|---|---|---|
| `sqlite_compare_kinds.go` (comparison/reconciliation kinds, `plannerDecisionInput`) | syncplan→sync 28, syncstore→sync 5, syncdomain→sync 5 | vocabulary: whole file to `syncdomain` |
| `store_sql_encoding.go` (`storedTimeNano`, `storedTimeIsSet`, `remoteModTime`) | syncexec→syncstore 3, syncobserve→syncstore 1, syncdomain→syncstore 2 | the four encoding decls to `syncdomain` (the file is split 53/7) |
| `pendingCreateIntentTTL` (read by `PendingCreate.intentIsFresh`, already vocabulary) | syncdomain→syncstore 1 | to `syncdomain` |
| `remoteItem` (`observed_items.go`, replaced `changeEvent`) returned by the store's `baseline_orphans.go` | syncstore→syncobserve 2 | to `syncdomain`, where `changeEvent` was |

**Tests: not solved yet.** With the 11-10 `conf/tests.txt`, 311 illegal
test references remain (192 test code of another package, 119 unexported
production identifiers). They come from tests added since 11-10 and from
helpers that now span packages. Forcing the five new planner test files
to `syncplan` brings it to 205. The remaining work is the test-placement
decisions of section 10, which is S4's work:

- `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum` (`core_types_test.go`)
  reaches 25 unexported planner, exec and domain identifiers in one test: split it per package.
- `planner_kinds_test.go` checks the store's SQL `CASE`s against the Go
  kinds and so needs store internals (`sqlComparisonKindCase`,
  `sqlReconciliationKindCase`, `queryComparisonRows`): the SQL half belongs in `syncstore`.
- The planner tests read the store's planning tables at SQL level (the
  section 2 note); with the store's 17 planning-read helpers exported for
  production they stay in `syncplan`, but `planCurrentStateForStore*` is
  shared with 4 engine tests and has no helper package able to hold it
  (it needs `syncplan` and `syncstore`).
- 12 shared test roots with no legal helper home (`localBatches`,
  `drainLocalPatches`, `newWorkerTestSetup`, `newPendingCreateTestExecutor`,
  `adoptTestPlaceholder`, `forwardedSnapshot`, …): move the test or keep
  a private copy.
- Test-only exports newly needed: the kind lists (`allComparisonKinds`,
  `allReconciliationKinds` and four kind constants), `graphRootID`,
  `executor.uploadNewFile`, five `executorConfig` fields,
  `sqliteFileURI`/`sqlitePragmaParam`/`openEngineSyncStore`, and others
  (full list: `evaltests.py <out> <tests> syms unexported-production-symbol`).

## What changed since 11-10 (production)

| Package | decls / LOC 11-10 | decls / LOC now |
|---|---|---|
| syncdomain | 492 / 3,561 | 641 / 4,575 |
| syncshortcut | 242 / 2,661 | 245 / 2,699 |
| syncstore | 421 / 7,937 | 477 / 9,088 |
| syncplan | 181 / 3,093 | 256 / 4,733 |
| syncobserve | 442 / 8,370 | 448 / 8,457 |
| syncexec | 276 / 4,925 | 300 / 5,312 |
| sync | 754 / 12,503 | 847 / 15,330 |

- Edges: the same 13, no new one. Biggest growth: syncplan→syncdomain
  156/984 → 190/1,274 symbols/sites, sync→syncstore 101/164 → 128/220.
- Declarations leaving their file family: 111 → 227 (58 are constants
  following the kinds types, 46 are `sqlite_compare_kinds.go` moving
  whole). Files to split: 19 → 25 (new: `observed_items.go`,
  `planner_kinds.go`, `store_sql_encoding.go`, `engine_watch.go`,
  `engine_run_once.go`, `engine_runtime_completion.go`). Files moving
  whole: 6 → 7 (`sqlite_compare_kinds.go`).
- Identifiers to export for production: 444 → 622 (syncdomain 215 → 338,
  syncstore 50 → 77, syncplan 6 → 8, syncobserve 76 → 86, syncexec 25 → 41,
  syncshortcut 72 → 72).
- Unexported fields reached across packages: 11 structs / 39 fields →
  19 structs / 60 fields. New since 11-10: `localObservation` (8, the
  #960 local output), `localSnapshotCommit` and `localSnapshotCommitResult`
  (4 + 4), `realClock` (2), `localObserver.clock` and `.debugEvents`,
  `remoteObserver.clock` and `.debugEvents`, `depGraph.ownerCheck`,
  `executorConfig.syncTree`, `localContentChangedError.path`,
  `remoteItem.observedItem` (the embedded type). `localObservationBatch`
  is gone (replaced by `localObservation`).
- Exec's store interface: still the same six methods, all `[ok]`.
- Name collisions: still only `localObserver.droppedEvents` /
  `droppedRetries` against the existing methods.

## Rerunning

```
S=<scratch>; cd ~/agent/notes/onedrive-go-sync-split-2026-11-10
tools/run.sh $S/production.txt $S/prod ~/src/onedrive-go-wt/split-prepare
python3 tools/mkconf.py $S/production.txt conf/tests.txt $S/prod $S/final.txt
tools/run.sh $S/final.txt $S/final ~/src/onedrive-go-wt/split-prepare
```

Each run takes about 40 s on this host.
