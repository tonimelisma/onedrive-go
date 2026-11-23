# onedrive-go planning cost at 10k / 40k / 140k items — profile, scaling and fixes

Investigation of 2026-11-01 on the dev host (2 cores, 6 GB), repo `origin/main`
144f5965, Go 1.27.1, modernc.org/sqlite v1.61.0. Everything in-process with
the package's own fake OneDrive (`moveScenarioRemote`) and synthetic trees on
the SSD under `~/.local/state/onedrive-go-rehearsal/plan-profile/`
(`TMPDIR` pointed there too, so the dry-run scratch store and test `TempDir`s
never touched tmpfs). No real account, no `<production sync folder>`. Raw logs, stage
JSON, CPU/heap profiles and the harness source are kept in
`~/.local/state/onedrive-go-rehearsal/plan-profile/out/`
(`run-<shape>-<n>[-variant].log`, `stages-*.json`, `cpu-*.pprof`,
`heap-*.pprof`, `equiv.log`, `zz_plan_profile_harness_test.go`); the
synthetic trees and the worktree were removed.

Harness: `zz_plan_profile_harness_test.go` (full code in Appendix B), added
to a detached worktree, env-gated, never committed. It drives the exact stage
functions `runLiveCurrentPlan` calls (`runStartupStage`,
`observeAndCommitRemoteCurrentState`, `refreshLocalCurrentState`,
`commitCurrentLocalSnapshot`, then `loadCurrentInputsTx` and
`planner.PlanCurrentState` re-implemented body-for-body with a timer between
their steps, `applyDeleteSafetyHold`, `reconcileRuntimeStateStage`,
`registerPlanActions`, `admitReady`), each with a wall-clock and `getrusage`
CPU timer. A CPU profile covers the measured section; a heap profile is
written while plan and dependency graph are alive.

**Read the numbers as CPU seconds where given.** The two cores were shared
with the live watch client (itself stuck in 600 s replans) and, from
~13:30, with an unrelated `go test -count=5` of the same package, so wall
times in the later runs are 2–5× their CPU times. The first three upload
runs (10k/40k/140k, 11:59–12:40) had only the live client beside them.

## 0. Summary

Two independent superlinear costs, one per question the coordinator asked:

1. **`buildDependencies` is O(n²) in the number of actions**
   (`internal/sync/planner.go:269`). `addMoveDeps` (:470) and
   `addRemoteCaseVariantDeps` (:417) scan the whole action slice for every
   action. Measured: 6.4 s at 11,010 actions, 148 s at 44,040, **1877 s
   (31 min) at 154,140** — 91 % of all CPU samples of the 140k first-sync
   run; exponent 2.0–2.3. It is pure Go, so it is exactly the
   `plan_time_ms` bucket, which is why the live first sync logged nothing
   for 17 minutes (143,078 actions; this harness has 154,140 because its
   tree has more folders). Independent of the baseline. An indexed rewrite
   with identical output takes **0.63 s** on the same 154,140-action plan.

2. **The planner-visible prune is O(rows × baseline rows)**
   (`internal/sync/store_planner_visible.go:51` and `:61`,
   `sqlPrunePlannerVisibleLocalDescendants` / `…RemoteDescendants`, run from
   `replacePlannerVisibleStateTx` :120). Each is a `DELETE … WHERE EXISTS
   (SELECT 1 FROM baseline parent LEFT JOIN planner_visible_… WHERE … AND
   substr(outer.path, 1, length(parent.path)+1) = parent.path || '/')`: a
   correlated full scan of `baseline` per planner-visible row that no index
   can serve. Zero cost while the baseline is empty (first sync), quadratic
   once it is populated, with a further cliff when `baseline` outgrows the
   connection's 2 MB page cache and each outer row re-reads it through
   `pread`. Measured CPU for the pair: 21.7 s at 5.5k rows, **98.8 s at
   11k rows** (live: 619 s wall at 28 % CPU ≈ 173 s CPU on 11.3k rows,
   WAL reads included), and by the measured exponent **≈ 5 h CPU at
   154k rows** — the 140k run with the original prune was aborted for that
   reason. A semantics-equal rewrite takes 0.03 s at 11k and 2.6 s wall at
   154k.

Everything else is linear or n·log n. After both fixes the first-sync plan
at 140k is ~5 s of Go planning on top of the SQL inputs stage (≈ 40 s
unloaded), and a steady-state replan at 154k rows costs ≈ 80 s CPU (down
from hours), dominated by the reconciliation query (32 s), the two per-row
TEMP insert loops (19 s, half of it statement compilation) and the
comparison query (10 s); §5 ranks what to do about those next.

## 1. Where the time goes (upload shape: local tree, empty OneDrive, no baseline)

Tree: 10 files per folder, 100 folders per top folder (140,000 files in
14,140 folders at 140k; production is 129,286 files in 8,855 folders).
Every file plans as an upload, every folder as a remote folder create.

Wall seconds per stage (these three runs had no CPU column yet; pprof
samples for the whole run were 7.4 s / 93.9 s / 958.8 s, i.e. the process
held about half a core).

| Stage | 10k (11,010 actions) | 40k (44,040) | 140k (154,140) |
|---|---|---|---|
| startup (drive proof, baseline Load) | 0.001 | 0.001 | 0.021 |
| observe remote + commit remote_state (empty) | 0.004 | 0.013 | 0.016 |
| observe local: walk + quickxor hash | 1.74 | 31.4 | 112.9 |
| commit local snapshot (`ReplaceLocalState`, first time: every row inserted) | 1.15 | 6.05 | 32.7 |
| inputs: `listLocalStateRows` | 0.11 | 0.40 | 1.99 |
| temp: create/delete planner_visible tables | 0.003 | 0.003 | 0.005 |
| temp: insert local rows one by one | 0.65 | 2.76 | 9.37 |
| temp: insert remote rows (0 rows) | 0 | 0 | 0 |
| temp: prune descendants (baseline empty) | 0.009 | 0.042 | 0.47 |
| temp: re-list planner_visible rows | 0.17 | 0.45 | 1.95 |
| sql: comparison query | 0.52 | 2.64 | 5.06 |
| sql: reconciliation query | 0.70 | 3.31 | 19.0 |
| inputs: `loadBaselineWithRunner` (second copy; empty here) | 0 | 0 | 0.002 |
| plan: truth availability index | 0.007 | 0.14 | 0.26 |
| plan: `buildSQLitePathViews` | 0.044 | 0.19 | 1.28 |
| plan: move composition | 0.006 | 0.046 | 0.39 |
| plan: `buildActionsForReconciliation` loop | 0.032 | 0.14 | 1.31 |
| plan: normalize / dropCreates / partition | 0.036 | 0.14 | 0.49 |
| **plan: `buildDependencies`** | **6.37** | **148.4** | **1877.5** |
| plan: Tarjan cycle check | 0.000 | 0.001 | 0.023 |
| `applyDeleteSafetyHold` (no baseline → returns) | 0 | 0 | 0 |
| `reconcileRuntimeStateStage` (held deletes, retry prune, block scopes) | 0.032 | 0.072 | 0.20 |
| dep graph: `Register` + `WireDeps` | 0.043 | 0.059 | 0.40 |
| `admitReady` (freshness of the ready frontier: 10/40/140 folder creates) | 0.015 | 0.078 | 0.27 |
| **measured total** | **11.7** | **196.5** | **2065.7** |

Observations:

- At 140k, `buildDependencies` is 1877.5 of 2065.7 s (91 %) and 875 of
  959 s of CPU samples. Next: the local scan (113 s wall, ~27 s CPU: it is
  I/O-bound on 154k `openat`/`statx`/`read` syscalls even with the files
  in page cache, and in production it scales with the tree's bytes, so it
  is out of scope), `ReplaceLocalState` (33 s, first full insert only;
  later passes write only the diff, 0.6 s at 10k), the reconciliation query
  (19 s), the TEMP inserts (9.4 s) and the comparison query (5 s).
- Counter attribution: `RecordObserve` is called after
  `loadCommittedCurrentObservation`, so the whole SQL inputs stage (TEMP
  rebuild, prune, comparison, reconciliation) is inside `observe_time_ms`
  and only the Go planning is `plan_time_ms`. Dry-run cross-check at 40k
  with a perf collector attached: `RunOnce(DryRun)` reported
  `observe_time_ms=62,535` and `plan_time_ms=267,215` for 44,040 actions,
  against the staged `buildDependencies` of 320.8 s in the same (loaded)
  process. The live `plan_time_ms` 1,024,393 was therefore
  `buildDependencies` plus ~5 s of views and action building; the prune
  never contributed to it, and on a first sync it could not (empty
  baseline).
- Heap in use after plan + graph at 140k: 95 MB (`buildSQLitePathViews`
  32 MB flat / 46.5 MB cumulative, `partitionCurrentActionsForMode` 21 MB —
  the copy of the `[]Action` slice, 144 bytes per Action —
  `localStateFromSnapshotRow` 14.5 MB, modernc `columnText` 12 MB,
  `buildDependencies` 5.5 MB). `depGraph.Register` copies each Action
  again into a `trackedAction` (the 32 MB seen live); it costs 0.2 s CPU.

### Top cumulative frames, 140k upload CPU profile (2065.7 s wall, 958.8 s samples)

```
      flat  flat%   sum%        cum   cum%
     0.37s 0.039% 0.039%    875.43s 91.31%  sync.buildDependencies
   108.38s 11.30% 11.34%    492.07s 51.32%  sync.addMoveDeps
   377.29s 39.35% 50.69%    382.46s 39.89%  sync.isMoveAction (inline)
   376.21s 39.24% 89.93%    381.30s 39.77%  sync.addRemoteCaseVariantDeps
     0.06s 0.0063% 89.94%     22.29s  2.32%  errgroup.(*Group).Go.func1          (hash workers)
     0.25s 0.026% 89.96%     21.58s  2.25%  sync.(*localObserver).hashPhase.func1
         0     0% 89.96%     21.48s  2.24%  sync.ppLoadCurrentInputs             (whole SQL inputs stage)
     0.04s 0.0042% 89.97%     20.77s  2.17%  driveops.ComputeQuickXorHashContext
     0.06s 0.0063% 89.97%     19.20s  2.00%  database/sql.withLock
     0.02s 0.0021% 89.98%     19.15s  2.00%  database/sql.(*Tx).ExecContext
     0.02s 0.0021% 89.99%     18.18s  1.90%  modernc.org/sqlite.(*conn).exec
    16.59s  1.73% 91.72%     16.59s  1.73%  internal/runtime/syscall/linux.Syscall6
     0.01s 0.001% 91.72%     15.66s  1.63%  modernc.org/sqlite.(*conn).step
     6.88s  0.72% 92.45%     15.31s  1.60%  modernc.org/sqlite/lib._sqlite3VdbeExec
         0     0% 92.48%     13.14s  1.37%  sync.(*engineFlow).commitCurrentLocalSnapshot
     0.05s 0.0052%  2.55%     13.04s  1.36%  sync.replaceLocalStateTx
     0.04s 0.0042%  2.55%     12.99s  1.35%  sync.upsertLocalStateRowTx
     0.10s  0.01%  2.57%     12.67s  1.32%  modernc.org/sqlite.(*rows).Next
         0     0%  2.58%     10.41s  1.09%  runtime.gcBgMarkWorker
     0.04s 0.0042%  2.89%      9.54s     1%  modernc.org/sqlite.(*conn).newStmtContext   (statement compilation, all sites)
     0.06s 0.0063% 0.0063%      8.28s  0.86%  sync.queryReconciliationStateWithRunnerForTables
     0.03s 0.0031% 0.0031%      6.68s   0.7%  sync.ppInsertPlannerVisibleLocal
     0.05s 0.0052% 0.0052%      3.72s  0.39%  sync.queryComparisonStateWithRunnerForTables
         0     0%     0%      2.62s  0.27%  sync.listLocalStateRowsWithQuery
     0.01s 0.001% 0.001%      0.23s 0.024%  sync.(*engineFlow).registerPlanActions
         0     0%     0%      0.12s 0.013%  sync.(*engineFlow).admitReady
```

Flat top of the same profile: `isMoveAction` 377 s (inlined into
`addMoveDeps`' inner loop), `addRemoteCaseVariantDeps` 376 s, `addMoveDeps`
108 s, then `Syscall6` 16.6 s, `_sqlite3VdbeExec` 6.9 s, everything else
below 5 s. The inner loops do nothing but load `actions[otherIdx].Type`
2 × n² times; 154k Actions × 144 bytes is 22 MB, so each inner pass streams
the whole slice through cache, which is why the exponent sits above 2.

### Why `buildDependencies` is quadratic (`internal/sync/planner.go`, origin/main lines)

- `addMoveDeps` (:470): `for otherIdx := range actions` for every action, to
  find moves that must precede it. Rule 1 (`isMoveAction(other) &&
  movePrecedes(other, a)`) needs only the moves; rules 2 and 3 apply only
  when `a` itself is a move. A first upload has zero moves and still pays
  n² iterations.
- `addRemoteCaseVariantDeps` (:417): for every upload, remote move and
  remote folder create — every action of a first upload — scans all
  actions for remote deletes/moves whose vacated path is
  `namekey.EqualPath` to its path. A first upload has none and pays n².
- Latent O(folder deletes × n) in the same family: `addChildDeleteDeps`
  (:357) iterates the whole `deleteIdx` map per folder delete;
  `addMoveOutOfDeletedFolderDeps` (:382) scans all actions per folder
  delete; `runnableSubtreeActionRequiresParent` (:238, from
  `preserveParentFoldersForRunnableDescendants` :176) scans all actions per
  folder delete. Deleting a 10k-folder subtree holding 100k files costs
  3 × 10k × 110k iterations.
- `applyDeleteSafetyHold` (`planner_delete_safety.go:55`) calls
  `buildDependencies` again whenever deletes are held, and
  `heldDeleteClosure` (:200) a third time if `plan.Deps` is stale, so a held
  mass delete pays the quadratic cost twice.

## 2. Scaling exponents

Exponent = log(t2/t1) / log(n2/n1).

Upload shape (actions 11,010 → 44,040 → 154,140; ×4.0, ×3.5), wall:

| Stage | 10k→40k | 40k→140k | class |
|---|---|---|---|
| `buildDependencies` (wall) | 2.27 | 2.03 | quadratic |
| `buildDependencies` (pprof CPU: 3.48 / 72.3 / 875 s) | 2.19 | 1.99 | quadratic |
| reconciliation query | 1.12 | 1.39 | n·log n plus cache growth |
| comparison query | 1.17 | 0.52 | n·log n |
| `ReplaceLocalState` (first insert) | 1.20 | 1.35 | linear plus index growth |
| TEMP insert local rows | 1.04 | 0.98 | linear (59–63 µs/row) |
| `listLocalStateRows` | 0.91 | 1.29 | linear |
| `buildSQLitePathViews` | 1.05 | 1.53 | linear with GC |
| local scan (wall) | 2.09 | 1.02 | I/O-bound, not planning |
| dep graph Register/Wire | 0.23 | 1.53 | linear |
| measured total | 2.03 | 1.88 | dominated by buildDependencies |

Baseline shape (rows 5,505 → 11,010, ×2.0), CPU seconds, original prune:

| Stage | 5k | 10k | exponent |
|---|---|---|---|
| prune local descendants | 10.91 | 53.12 | 2.28 |
| prune remote descendants | 10.81 | 45.70 | 2.08 |
| TEMP insert local / remote | 0.29 / 0.29 | 0.56 / 0.58 | 0.95 / 1.0 |
| comparison query | 0.30 | 0.60 | 1.0 |
| reconciliation query | 1.13 | 2.21 | 0.97 |
| `loadBaselineWithRunner` (second copy) | 0.10 | 0.21 | 1.0 |
| move composition | 0.07 | 0.15 | 1.1 |

Extrapolating the prune pair with exponent 2 from 98.8 s CPU at 11,010 rows
gives ≈ 5.4 h CPU at 154,140 rows (×196), before the page-cache effect
that made the live 4.4k → 11.3k step 9.5× for 4.3× the pairs. That run was
not attempted.

## 3. Steady state on a complete baseline — the watch replan

Shape `baseline`: the tree is scanned once, `baseline` and `remote_state`
are seeded from the scan exactly as a finished sync leaves them (same hash,
size, mtime, ctime, device/inode/birth time, eTag), the remote cursor is
committed so the next delta is empty, one file is edited, and the engine
runs the one-shot stages; then `loadCurrentInputs` + plan run a second time
without a scan, which is what `runSteadyStateCurrentPlan` does on every
watch replan ("REPLAN" rows). Wall s (CPU s).

| Stage | 5k rows | 10k rows (11,010) | 10k, candidate prune | 140k (154,140), candidate prune |
|---|---|---|---|---|
| observe remote (delta, no changes) | 0.32 (0.07) | 0.25 (0.13) | 0.37 (0.13) | 7.3 (1.8) |
| observe local (hash reuse: 1 file hashed) | 0.52 (0.18) | 0.64 (0.36) | 1.1 (0.40) | 42.8 (9.1) |
| commit local snapshot (diff: 1 row) | 0.16 (0.07) | 0.63 (0.18) | 0.57 (0.15) | 10.8 (2.3) |
| inputs: `listLocalStateRows` | 0.16 (0.06) | 0.31 (0.11) | 0.27 (0.12) | 11.2 (1.9) |
| inputs: remote_state rows | 0.17 (0.06) | 0.47 (0.15) | 0.20 (0.11) | 8.9 (2.4) |
| temp: insert local rows one by one | 1.05 (0.29) | 1.50 (0.56) | 1.34 (0.54) | 47.0 (8.8) |
| temp: insert remote rows one by one | 1.04 (0.29) | 1.19 (0.58) | 2.09 (0.66) | 47.0 (10.0) |
| **temp: prune local descendants** | **46.7 (10.9)** | **140.2 (53.1)** | 0.072 (0.019) | 1.94 (0.23) |
| **temp: prune remote descendants** | **45.5 (10.8)** | **116.0 (45.7)** | 0.022 (0.006) | 0.71 (0.12) |
| temp: re-list planner_visible rows | 0.32 (0.11) | 0.53 (0.25) | 1.04 (0.25) | 16.4 (3.8) |
| sql: comparison query | 1.00 (0.30) | 1.55 (0.60) | 2.23 (0.63) | 41.8 (10.3) |
| sql: reconciliation query | 3.76 (1.13) | 4.83 (2.21) | 7.28 (2.32) | 155.2 (34.3) |
| inputs: `loadBaselineWithRunner` (second Baseline copy) | 0.41 (0.10) | 0.59 (0.21) | 0.75 (0.22) | 22.1 (3.9) |
| plan: `buildSQLitePathViews` | 0.08 (0.04) | 0.36 (0.10) | 0.15 (0.09) | 6.6 (0.96) |
| plan: move composition | 0.22 (0.07) | 0.40 (0.15) | 0.19 (0.15) | 21.0 (4.4) |
| plan: rest (1 upload action) | 0.1 | 0.1 | 0.1 | 3.7 (0.7) |
| **measured total** | **101.6** | **269.6** | **18.0** | **446.2** |
| **REPLAN (inputs + plan, no scan)** | **50.4** (prune 23.9 + 19.2 wall, 10.3 + 10.5 CPU) | **378.4** (prune 193.7 + 168.7 wall, 52.7 + 45.6 CPU) | **13.5** (prune 0.03) | **340.9 wall, ≈ 79 CPU** (prune 2.8) |

Comparison with the live client (11,420 local rows, 4 workers): replan 10 s
at 0 baseline rows, 65 s at 4,429, 619 s at 11,254. Harness at 11,010 rows:
the prune pair alone is 98.8 s CPU and 257–362 s wall on the loaded box;
at 5,505 rows 21.7 s CPU. The live 30 s profile (Appendix A) is the same
statement: 98.8 % under one `replacePlannerVisibleStateTx → ExecContext`,
`_sqlite3BtreeNext` 50.7 % (the full scan of `baseline` per outer row),
`_readDbPage → _unixRead → pread` 30.5 % with `_walFindFrame` (the table no
longer fits the 2 MB page cache, so each outer row re-reads it from the OS
cache, through the WAL index), `_substrFunc` and `_lengthFunc` (only the two
prune statements use them). The harness 5k profile has the same shape minus
the pread frames (`_sqlite3VdbeExec` 94 %, `_btreeParseCell` 16 %,
`_sqlite3BtreeNext` 9 %, `_substrFunc` 4 %), i.e. at 5.5k rows the scan is
still in cache — that is the cliff between the live 4.4k and 11.3k numbers.

### The prune statement, and the coordinator's four questions

`store_planner_visible.go:51` (remote twin at :61):

```sql
DELETE FROM planner_visible_local_state
WHERE EXISTS (
    SELECT 1
    FROM baseline parent
    LEFT JOIN planner_visible_local_state parent_local ON parent_local.path = parent.path
    WHERE parent.item_type = 'folder'
        AND parent.path <> ''
        AND parent_local.path IS NULL
        AND substr(planner_visible_local_state.path, 1, length(parent.path) + 1) = parent.path || '/'
)
```

`EXPLAIN QUERY PLAN` (from `TestPlanProfileExplainPrune`, `equiv.log`):

```
SCAN planner_visible_local_state
CORRELATED SCALAR SUBQUERY 1
  SCAN parent
  SEARCH parent_local USING COVERING INDEX sqlite_autoindex_planner_visible_local_state_1 (path=?) LEFT-JOIN
```

- **Per row or per statement?** `replacePlannerVisibleStateTx`
  (`store_planner_visible.go:120`) issues, per plan and inside the
  planner's read transaction (`beginPerfTx`, hence `db_transaction_time`):
  2 × `CREATE TEMP TABLE IF NOT EXISTS`; 2 × whole-table `DELETE FROM
  planner_visible_*` (one statement each); **one `INSERT` per local row
  (:138) and one per remote row (:156)**, each a fresh `tx.ExecContext`
  that modernc prepares, steps and finalizes (nothing is cached: ~60 µs per
  row, half of it compilation); the two prune `DELETE`s above (one
  statement each — the n² is inside one statement, not a statement per
  row); then two `SELECT … ORDER BY path` re-reads of what survived.
- **Missing index / O(n²) nested loop?** Yes, the prune. The subquery is
  correlated on the outer row's `path`, so it runs once per planner-visible
  row; inside it `baseline parent` is a full `SCAN` (no index on
  `item_type`, and the only predicate that would narrow it,
  `substr(outer.path, 1, length(parent.path)+1) = parent.path || '/'`, is a
  function of both rows), with a PK probe into the TEMP table per baseline
  row. Cost = planner-visible rows × baseline rows × (row decode + probe):
  11k × 11k = 1.2 × 10⁸ pairs per statement, 2.4 × 10¹⁰ at 154k.
- **Does synchronous=FULL make each statement fsync?** No. The prune is a
  read of `baseline` plus deletes in a TEMP table inside one transaction;
  no `fsync`/`fdatasync` frame appears in the live or harness profiles, and
  the harness CPU column tracks the work (the wall/CPU gap of 2–5× is
  shared by every stage in those runs, including pure-Go ones — it is the
  loaded box, not sleeping on I/O). The `pread` frames are SQLite reading
  `baseline` pages (and WAL frames) back from the OS page cache because
  `PRAGMA cache_size` is the default `-2000` (2 MB) and 11k baseline rows
  are about 3 MB of pages; the live "28 % of one core" is pprof seeing a
  single goroutine that spends most of its time inside `pread`-heavy
  btree walks while the rest of the process idles. `PRAGMA synchronous`
  governs commits, not this.
- **`PRAGMA temp_store`?** Default `0` (file), confirmed. It is not the
  lever: the TEMP tables are the outer loop, read once; the repeated scan
  is of `baseline`, in the main database. A bigger `cache_size` would move
  the 4.4k→11k cliff outward but leave the n²; the fix is the query.

Candidate rewrite (same rows deleted; `TestPlanProfileExplainPrune` checks
it on a tree with a missing top folder, a missing nested folder, a present
folder, and a sibling that shares the missing folder's prefix — `a` missing
must not prune `ab/z.txt`; both variants leave `[ab/z.txt c c/z.txt d
d/e2/q.txt d/f.txt]`):

```sql
WITH missing AS MATERIALIZED (
    SELECT parent.path AS path
    FROM baseline parent
    LEFT JOIN planner_visible_local_state parent_local ON parent_local.path = parent.path
    WHERE parent.item_type = 'folder' AND parent.path <> '' AND parent_local.path IS NULL
)
DELETE FROM planner_visible_local_state
WHERE path IN (
    SELECT pv.path FROM missing m
    JOIN planner_visible_local_state pv ON pv.path > m.path || '/' AND pv.path < m.path || '0'
)
```

```
SEARCH planner_visible_local_state USING COVERING INDEX sqlite_autoindex_planner_visible_local_state_1 (path=?)
LIST SUBQUERY 2
  MATERIALIZE missing
  SCAN parent
  SEARCH parent_local USING COVERING INDEX ... (path=?) LEFT-JOIN
  SCAN m
  SEARCH pv USING COVERING INDEX ... (path>? AND path<?)
```

`missing` is one pass over `baseline` with a PK probe each, is normally
empty or a handful of folders, and each costs one PK range scan — the
`> p || '/' AND < p || '0'` idiom the comparison CTEs already use. Measured:
0.072 + 0.022 s at 11k (was 140 + 116 s wall / 53 + 46 s CPU), 1.9 + 0.7 s
wall (0.23 + 0.12 CPU) at 154k rows.

## 4. Prepared statements: what statement compilation costs

modernc's driver compiles every `ExecContext`/`QueryContext` issued through
`*sql.Tx`/`*sql.DB` (`(*conn).ExecContext → newStmtContext → prepareV2`,
then finalize); only a `*sql.Stmt` is reused, and with `SetMaxOpenConns(1)`
a statement prepared once on the DB and used via `tx.StmtContext` is never
re-prepared. Share of `newStmt`/`_sqlite3Prepare` under each hot call site
(`go tool pprof -focus=<site>`):

| call site | profile | cum | `_sqlite3Prepare` | `_sqlite3Step` |
|---|---|---|---|---|
| TEMP insert loop, local rows (`store_planner_visible.go:139`) | upload 140k | 6.68 s | 3.34 s (50 %) | 1.63 s |
| `replaceLocalStateTx → upsertLocalStateRowTx` (`store_local_state.go:330`) | upload 140k | 13.0 s | 5.98 s (46 %) | 4.58 s |
| `commitObservation` of 154k remote items (`processObservedItem`: `scanRemoteStateRow` SELECT per item 42.9 s, `insertRemoteState` 29.1 s) | mirror 140k | 102.9 s | 49.6 s (48 %) | — |
| `queryReconciliationStateWithRunnerForTables` | upload 140k | 8.28 s | 0.09 s (1 %) | 6.75 s |
| `queryComparisonStateWithRunnerForTables` | upload 140k | 3.72 s | 0.02 s | 1.80 s |
| `listLocalStateRowsWithQuery` | upload 140k | 2.62 s | ≈ 0 | 0.43 s (rest is row decoding) |
| `admitReady` (freshness SELECTs for 140 ready actions) | upload 140k | 0.12 s | 0.05 s | 0.04 s |

So the `sqlite3WalkExpr`/`substExpr`/`exprDup` dominance in the scenario-test
profile is a property of tests that run thousands of tiny statements on tiny
tables. At drive scale compilation is ~50 % of every per-row loop
(TEMP inserts, `ReplaceLocalState`, the remote_state commit) and negligible
for the big queries, which compile once. Caching the per-row statements
saves at 154k rows ≈ 9 s CPU per steady-state replan (both TEMP loops,
measured 8.7 + 9.9 s CPU), 6 s on a first `ReplaceLocalState`, and ≈ 48 s
CPU on a full remote enumeration commit. Useful, but a factor 2 on those
stages, not the orders of magnitude in §1 and §3. `ReadObservationState`
(two statements per call, 0.001 s per stage) and
`localStateFromSnapshotRow` (a Go allocation per row: 14.5 MB live, inside
the 1.3 s `buildSQLitePathViews`) are not prepare costs.

## 5. Fixes, ranked by measured payoff

1. **Rewrite the two prune statements** (`store_planner_visible.go:51–71`,
   the two constants; nothing else changes — same callers, same TEMP
   tables). Payoff: steady-state replan at 11k rows from 378 s wall /
   ~105 s CPU to 13.5 s wall / ~5 s CPU; at 154k from hours to ≈ 3 s for
   the prune. Risk: low — the candidate is `EXPLAIN`ed above, produces the
   same surviving rows in the nested/sibling-prefix test, uses the path
   range idiom already in `sqlite_compare.go`, and cannot change which
   folders count as missing because `missing` is computed before the
   delete from the same snapshot. Regression test to add: a
   `store_planner_visible_test.go` case with a missing baseline folder at
   depth 1 and 2 plus a prefix-sharing sibling, and a benchmark seeding
   10k/100k baseline rows (the existing `seedComparisonBench` can seed it).
   **Safe for a first PR, and the one that unsticks the live client.**

2. **Index `buildDependencies`** (`planner.go:269` and the helpers at
   :238, :357, :382, :417, :470). The harness's `ppBuildDependenciesIndexed`
   (Appendix B) keeps every rule's predicate function (`movePrecedes`,
   `underMoveSource`, `underMoveDestination`, the case-variant checks) and
   only changes how candidates are found: moves indexed by `Path` and
   `OldPath` and looked up at the ancestors of an action's `Path`/`OldPath`
   (every rule of `movePrecedes` needs one of those to match); work under a
   move's source attributed by walking each action's ancestors to the
   moves whose `OldPath` is one of them; child deletes and moves-out
   attributed to the folder deletes among their ancestors; case-variant
   names matched through a folded key (`namekey.Key` per segment joined by
   `/`, which is exactly what `namekey.EqualPath` compares). Output is
   identical to the original on 300 random plans mixing all ten action
   types, both folder-create sides, random nesting and case/Unicode
   variants (`TestPlanProfileIndexedDepsEqual`) and on the real 44,040- and
   154,140-action upload and mirror plans (checked inside the harness
   stage). Payoff: 1877 s → 0.63 s at 154,140 actions, 148–320 s → 0.68 s at
   44,040; `plan_time_ms` at 140k becomes ≈ 5 s. Risk: moderate on paper
   (dependency ordering is a safety property) but fully differential-
   testable: keep the old function in the test file as the oracle, run the
   random comparison as an exhaustive-tier test, keep
   `TestBuildDependencies_*`. Also fixes the latent folder-delete × n scans
   and the second `buildDependencies` in `applyDeleteSafetyHold`. **Safe
   for the first PR with the oracle test.** The remaining
   `preserveParentFoldersForRunnableDescendants` scan (:176/:238) should get
   the same ancestor-walk treatment in that PR (not exercised by these
   shapes; it is O(folder deletes × n)).

3. **Cache the per-row statements** (`store_planner_visible.go:139/157`,
   `store_local_state.go:330`, and the remote-state commit loop in
   `store_write_observation.go`: `processObservedItem`'s
   `scanRemoteStateRow` SELECT and `insertRemoteState`). `*sql.Stmt`
   prepared once per `SyncStore` (or per transaction with
   `tx.PrepareContext`, which is enough for the TEMP inserts), reused via
   `tx.StmtContext`. Payoff: ≈ 9 s CPU per 154k replan, 6 s per first
   `ReplaceLocalState`, ≈ 48 s per full remote enumeration commit (≈ 2× on
   each loop). Risk: low; TEMP tables are per connection and there is one
   connection; a prepared statement survives `DELETE FROM`/re-`CREATE IF
   NOT EXISTS` (SQLite re-prepares on `SQLITE_SCHEMA` by itself). Second
   PR.

4. **Stop re-inserting both snapshots row by row on every replan**
   (`replacePlannerVisibleStateTx`). The planner-visible tables are the
   live `local_state`/`remote_state` minus the content filter minus the
   prune. `INSERT INTO planner_visible_local_state SELECT … FROM
   local_state` is one statement (~1–2 s for 154k rows against 47 s wall /
   8.8 s CPU), followed by a batched `DELETE … WHERE path IN (…)` of the
   paths the Go filter rejects (usually none; computed by the same
   `filterLocalStateRowsForPlanning` pass, which already runs). And when
   both prunes report `RowsAffected() == 0` and nothing was filtered, skip
   the two re-list `SELECT`s (16 s wall / 3.8 s CPU) and reuse the slices
   already in hand. Payoff ≈ 22 s CPU per 154k replan. Risk: moderate
   (changes the data path, must keep the filter semantics exactly); second
   PR, after 3 or instead of it for these two loops.

5. **One query instead of two** for comparison + reconciliation. Both
   queries evaluate the whole `sqlComparisonStateCTEs` chain; the
   reconciliation query is 3× the comparison because `reconciliation_kind`
   references the correlated `local_subtree_changed`/
   `remote_subtree_changed`/`metadata_drift` columns the comparison
   `SELECT` never touches (SQLite elides them there). Selecting the
   comparison columns from `reconciliation_state` in the same statement
   and filling both slices from one cursor saves the comparison query
   (10 s CPU at 154k, 25 % of the SQL time). Risk: low-moderate (planner
   inputs plumbing, `sqlite_compare.go` + `engine_current_plan.go:532–559`);
   second or third PR.

6. **Second `Baseline` copy** (`engine_current_plan.go:559`,
   `loadBaselineWithRunner` inside the read tx): 3.7–3.9 s CPU and
   12–22 s wall at 154k per replan, plus ~50 MB. It exists for a real race
   (a worker's `CommitMutation` landing between the cached baseline and
   the snapshot read). Cheaper: a generation counter on the store's cached
   baseline bumped by every `CommitMutation`/`reloadBaselineCache`; read it
   before `BEGIN` and after the queries, reuse the cache when equal, reload
   only on change. Moderate risk; third tier.

7. **`moveCompositionInputs` builds a node for every baseline row** even
   when nothing moved (4.2–4.4 s CPU, 13–21 s wall at 154k). Skipping rows
   whose comparison kind cannot participate in a move needs a careful
   read of `planner_move_composition.go`; low priority.

8. Not worth doing now: lazy `pathView`/`Action` construction
   (`buildSQLitePathViews` 1–1.3 s CPU, 46 MB), avoiding the Action copy in
   `depGraph.Register` (0.2 s CPU), `admitReady` (0.1–0.9 s), Tarjan
   (0.02 s), `PruneRetryWorkToCurrentActions` (0.2 s), indexing
   `baseline(item_type)` (the rewrite removes the scan altogether).

Estimated plan times at 140k after 1 + 2 (first PR): first sync (upload
shape) `plan_time_ms` ≈ 5 s instead of 1,024 s; the SQL inputs stage stays
≈ 40 s unloaded (TEMP 9 s, comparison 5 s, reconciliation 19 s, lists 4 s)
plus `ReplaceLocalState` 33 s and the scan. Steady-state replan at 154k
rows ≈ 80 s CPU (measured with the candidate prune, loaded wall 341 s)
instead of hours; at 11k rows ≈ 5 s CPU (measured 13.5 s wall, loaded)
instead of 378 s. With 3 + 4 + 5 on top, the 154k replan drops to ≈ 44 s
CPU, dominated by the reconciliation query (32 s). Whether 80 s is
acceptable against the watch loop's 5-minute safety-scan budget on a
loaded 2-core host is the open question for the production tree; the
first PR alone takes the 11k case from "never dispatches" to seconds.

## 6. The production first-sync shape (mirror: both sides populated, no baseline)

Shape `mirror`: the fake OneDrive holds the same tree with the same bytes
(hash-equal), no baseline, so every path is `create_equal` →
`baseline_update`, as the production first sync of the already-uploaded
tree plans ~138k of them. Wall s (CPU s):

| Stage | 40k (44,040 actions), original deps | 140k (154,140), indexed deps |
|---|---|---|
| observe remote (full enumeration of 44k/154k items) + commit remote_state | 82.8 (30.8) | 622.0 (120.3) |
| observe local | 30.3 (11.4) | 174.1 (49.4) |
| commit local snapshot (first insert) | 14.2 (4.3) | 75.5 (16.1) |
| temp: insert local + remote rows | 5.7 + 8.2 (2.3 + 2.7) | 35.0 + 21.7 (9.2 + 9.0) |
| temp: prune (baseline empty) | 0.3 | 0.7 |
| sql: comparison / reconciliation | 6.2 / 19.9 (1.7 / 5.4) | 20.2 / 81.6 (6.4 / 20.6) |
| plan: views / move composition / actions | 0.5 / 0.1 / 0.9 | 4.9 / 4.5 / 3.6 |
| plan: `buildDependencies` original | 91.1 (41.9) | not run (≈ 1,000 s projected: this shape hits only `addMoveDeps`) |
| plan: `buildDependencies` indexed | 0.23 (0.09) | 0.20 (0.12) |
| dep graph + `admitReady` (all 154k ready at once) | 0.3 | 1.8 |
| measured total | 268.3 | 1072.6 |

Two things outside the planner that this shape shows:

- The remote enumeration commit is 100 s CPU per 154k items, 48 % of it
  statement compilation (`scanRemoteStateRow` SELECT + `insertRemoteState`
  per item, fix 3 above).
- The publication drain that follows planning applies each
  `baseline_update` in its own `CommitMutation` transaction with
  `synchronous=FULL`: measured at 10k, **11,010 baseline_updates took
  214.9 s wall / 25.1 s CPU, 19.5 ms each** (fsync-bound). For the
  production 138k that is ≈ 45 minutes of publishing after a 5-second
  plan — the next thing the first sync will be waiting on once the
  planner is fixed. Batching publication-only outcomes (one transaction
  per few hundred) would make it seconds; out of this report's scope but
  worth a line in the backlog.

## Appendix A — live stall profile (perf-stall3, 30 s, watch replan at 11.4k rows, binary 144f5965)

```
Duration: 30.30s, Total samples = 8.47s (27.95%)
      flat  flat%   sum%        cum   cum%
         0     0%     0%      8.37s 98.82%  database/sql.(*Tx).ExecContext
         0     0%     0%      8.37s 98.82%  sync.(*watchRuntime).runPendingWatchReplan
         0     0%     0%      8.37s 98.82%  sync.(*watchRuntime).runSteadyStateCurrentPlan
         0     0%     0%      8.37s 98.82%  sync.(*engineFlow).loadCurrentInputsTx
         0     0%     0%      8.37s 98.82%  sync.replacePlannerVisibleStateTx
         0     0%     0%      8.37s 98.82%  modernc.org/sqlite.(*conn).exec
         0     0%     0%      8.37s 98.82%  modernc.org/sqlite/lib._sqlite3Step
     2.23s 26.33% 26.33%      8.37s 98.82%  modernc.org/sqlite/lib._sqlite3VdbeExec
     0.32s  3.78% 30.11%      4.29s 50.65%  modernc.org/sqlite/lib._sqlite3BtreeNext
     0.05s  0.59% 30.70%      3.97s 46.87%  modernc.org/sqlite/lib._btreeNext
     0.01s  0.12% 30.81%      3.49s 41.20%  modernc.org/sqlite/lib._moveToLeftmost
     0.03s  0.35% 31.17%      3.48s 41.09%  modernc.org/sqlite/lib._moveToChild
     0.02s  0.24% 31.40%      3.45s 40.73%  modernc.org/sqlite/lib._getAndInitPage
     0.01s  0.12% 31.52%      3.39s 40.02%  modernc.org/sqlite/lib._sqlite3PagerGet (inline)
     0.04s  0.47% 32.00%      3.38s 39.91%  modernc.org/sqlite/lib._getPageNormal
     0.07s  0.83% 32.82%      2.97s 35.06%  modernc.org/sqlite/lib._readDbPage
     0.01s  0.12% 32.94%      2.63s 31.05%  modernc.org/sqlite/lib._sqlite3OsRead (inline)
     0.02s  0.24% 33.18%      2.62s 30.93%  modernc.org/sqlite/lib._unixRead
     0.02s  0.24% 33.41%      2.60s 30.70%  modernc.org/sqlite/lib._seekAndRead
         0     0% 33.41%      2.58s 30.46%  modernc.org/libc.Xpread
flat:
     2.33s 27.51%  internal/runtime/syscall/linux.Syscall6      (pread)
     2.23s 26.33%  modernc.org/sqlite/lib._sqlite3VdbeExec
     0.34s  4.01%  modernc.org/sqlite/lib._btreeParseCellPtr
     0.32s  3.78%  modernc.org/sqlite/lib._sqlite3BtreeNext
     0.24s  2.83%  modernc.org/sqlite/lib._sqlite3BtreeCursorHasMoved
     0.13s  1.53%  modernc.org/sqlite/lib._sqlite3MemCompare
     0.11s  1.30%  modernc.org/sqlite/lib._walFindFrame
     0.08s  0.94%  modernc.org/sqlite/lib._lengthFunc
     0.06s  0.71%  modernc.org/sqlite/lib._substrFunc
```

`manifest.json` of that capture: `plan_time_ms` 4,336 over 4 plans (the Go
planner was never the problem in steady state), `observe_time_ms` 98,745,
`replan_idle_planning_ms` 6,687,912 — 1 h 51 min of workers idle while a
plan was being built.

## Appendix B — harness

Run as

```
cd <worktree>
export TMPDIR=~/.local/state/onedrive-go-rehearsal/plan-profile   # SSD, not tmpfs
export ONEDRIVE_PLAN_PROFILE_DIR=$TMPDIR/out
ONEDRIVE_PLAN_PROFILE_SHAPE=upload|baseline|mirror ONEDRIVE_PLAN_PROFILE_ITEMS=140000 \
  [ONEDRIVE_PLAN_PROFILE_SKIP_ORIGINAL_DEPS=1] [ONEDRIVE_PLAN_PROFILE_FAST_PRUNE=1] \
  [ONEDRIVE_PLAN_PROFILE_DRYRUN=1] [ONEDRIVE_PLAN_PROFILE_DRAIN=1] \
  go test ./internal/sync -run '^TestPlanProfileHarness$' -count=1 -timeout 0 -v
ONEDRIVE_PLAN_PROFILE_EQUIV=1 go test ./internal/sync -run '^TestPlanProfile(IndexedDepsEqual|ExplainPrune)$' -v
```

To turn it into a benchmark: keep `ppBuildTree`, `ppSeedBaseline`,
`ppPopulateRemote` as seeding helpers next to `seedComparisonBench`, and
benchmark `replacePlannerVisibleStateTx` (seeded baseline, 10k/100k),
`buildDependencies` (the upload plan, 10k/100k) and the full
`loadCurrentInputs` + `PlanCurrentState` pair; `ppBuildDependenciesIndexed`
and `ppSQLPrune*Fast` are the candidate implementations, and
`TestPlanProfileIndexedDepsEqual` is the differential test to keep.

File `internal/sync/zz_plan_profile_harness_test.go` as last run:

```go
package sync

// Throwaway planning-cost harness (investigation 2026-11-01). Not part of the
// suite: it runs only when ONEDRIVE_PLAN_PROFILE_ITEMS is set.
//
//	ONEDRIVE_PLAN_PROFILE_ITEMS=140000 \
//	ONEDRIVE_PLAN_PROFILE_SHAPE=upload|baseline|mirror \
//	ONEDRIVE_PLAN_PROFILE_DIR=/path/on/ssd \
//	TMPDIR=/path/on/ssd \
//	go test ./internal/sync -run '^TestPlanProfileHarness$' -count=1 -timeout 0 -v
//
// Shapes:
//   - upload:   local tree, empty OneDrive, no baseline (first sync of a
//               populated folder; every file plans as an upload).
//   - baseline: local tree already synced (baseline + remote_state seeded
//               from the scan), then one file edited: the steady-state
//               replan on a complete baseline.
//   - mirror:   local tree and an identical OneDrive, no baseline (the
//               production first sync: everything plans as baseline_update).
//
// It drives the engine's own stage functions one by one (the same calls
// runLiveCurrentPlan makes) so each stage is timed on the wall clock, and it
// re-implements loadCurrentInputsTx and PlanCurrentState body-for-body with a
// timer between the steps. A CPU profile covers the whole measured section
// and a heap profile is written while the plan and dependency graph are
// alive.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

type ppStage struct {
	Name    string  `json:"name"`
	Seconds    float64 `json:"seconds"`
	CPUSeconds float64 `json:"cpu_seconds"`
	Note       string  `json:"note,omitempty"`
}

type ppRun struct {
	Shape   string    `json:"shape"`
	Files   int       `json:"files"`
	Folders int       `json:"folders"`
	Actions int       `json:"actions"`
	Stages  []ppStage `json:"stages"`
	t       *testing.T
	out     io.Writer
}

func ppCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func (r *ppRun) stage(name string, fn func()) {
	start := time.Now()
	cpu0 := ppCPUSeconds()
	fn()
	d := time.Since(start)
	cpu := ppCPUSeconds() - cpu0
	r.Stages = append(r.Stages, ppStage{Name: name, Seconds: d.Seconds(), CPUSeconds: cpu})
	line := fmt.Sprintf("STAGE %-44s %10.3fs  cpu %8.3fs", name, d.Seconds(), cpu)
	r.t.Log(line)
	fmt.Fprintln(r.out, line)
}

func (r *ppRun) note(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.t.Log(line)
	fmt.Fprintln(r.out, line)
}

// ppBuildTree writes files tiny files under root: top/sub/file with 100
// subfolders per top folder and 10 files per subfolder, so 140k files sit in
// ~14k folders (the production tree is 129k files in 8.9k folders).
func ppBuildTree(t *testing.T, root string, files int) (folders int) {
	t.Helper()
	const filesPerSub, subsPerTop = 10, 100
	tops := (files + filesPerSub*subsPerTop - 1) / (filesPerSub * subsPerTop)
	written := 0
	for top := 0; top < tops && written < files; top++ {
		topDir := filepath.Join(root, fmt.Sprintf("d%03d", top))
		require.NoError(t, os.Mkdir(topDir, 0o750))
		folders++
		for sub := 0; sub < subsPerTop && written < files; sub++ {
			subDir := filepath.Join(topDir, fmt.Sprintf("s%03d", sub))
			require.NoError(t, os.Mkdir(subDir, 0o750))
			folders++
			for f := 0; f < filesPerSub && written < files; f++ {
				name := fmt.Sprintf("f%02d.txt", f)
				rel := fmt.Sprintf("d%03d/s%03d/%s", top, sub, name)
				require.NoError(t, os.WriteFile(filepath.Join(subDir, name), []byte("content of "+rel+"\n"), 0o640))
				written++
			}
		}
	}
	return folders
}

// ppPopulateRemote mirrors the local tree into the fake OneDrive with the
// same bytes (hash-equal), for the mirror shape.
func ppPopulateRemote(t *testing.T, remote *moveScenarioRemote, root string) {
	t.Helper()
	ids := map[string]string{"": "root"}
	n := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		n++
		id := "it-" + strconv.Itoa(n)
		parentID := ids[path.Dir(rel)]
		if parentID == "" {
			parentID = "root"
		}
		if d.IsDir() {
			ids[rel] = id
			remote.addFolder(id, parentID, d.Name())
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		remote.addFile(t, id, parentID, d.Name(), string(content))
		return nil
	})
	require.NoError(t, err)
}

// ppSeedBaseline writes baseline and remote_state rows that agree exactly
// with the scanned local rows, as a completed sync would have left them.
func ppSeedBaseline(t *testing.T, ctx context.Context, store *SyncStore, rows []localStateRow) {
	t.Helper()
	tx, err := store.rawDB().BeginTx(ctx, nil)
	require.NoError(t, err)
	insertBaseline, err := tx.PrepareContext(ctx, sqlUpsertBaseline)
	require.NoError(t, err)
	insertRemote, err := tx.PrepareContext(ctx, `INSERT INTO remote_state
		(drive_id, item_id, path, item_type, hash, size, mtime, etag) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	require.NoError(t, err)
	ids := map[string]string{"": "root"}
	for i := range rows {
		row := &rows[i]
		id := "it-" + strconv.Itoa(i+1)
		ids[row.Path] = id
		parentID := ids[path.Dir(row.Path)]
		if row.Path == "" || path.Dir(row.Path) == "." {
			parentID = "root"
		}
		etag := "e-" + id
		_, err := insertBaseline.ExecContext(ctx, baselineRowArgs(&BaselineEntry{
			Path: row.Path, ItemID: id, ParentID: parentID, ItemType: row.ItemType,
			LocalHash: row.Hash, RemoteHash: row.Hash,
			LocalSize: row.Size, LocalSizeKnown: true, RemoteSize: row.Size, RemoteSizeKnown: true,
			LocalMtime: row.Mtime, RemoteMtime: row.Mtime, LocalCtime: row.Ctime,
			LocalDevice: row.LocalDevice, LocalInode: row.LocalInode, LocalBirthTime: row.LocalBirthTime,
			LocalHasIdentity: row.LocalHasIdentity, ETag: etag,
		})...)
		require.NoError(t, err)
		_, err = insertRemote.ExecContext(ctx, engineTestDriveID, id, row.Path, row.ItemType.String(),
			nullString(row.Hash), row.Size, nullOptionalInt64(row.Mtime), etag)
		require.NoError(t, err)
	}
	require.NoError(t, insertBaseline.Close())
	require.NoError(t, insertRemote.Close())
	require.NoError(t, tx.Commit())
	require.NoError(t, store.reloadBaselineCache(ctx))
}

func TestPlanProfileHarness(t *testing.T) {
	itemsEnv := os.Getenv("ONEDRIVE_PLAN_PROFILE_ITEMS")
	if itemsEnv == "" {
		t.Skip("set ONEDRIVE_PLAN_PROFILE_ITEMS to run the planning-cost harness")
	}
	files, err := strconv.Atoi(itemsEnv)
	require.NoError(t, err)
	shape := os.Getenv("ONEDRIVE_PLAN_PROFILE_SHAPE")
	if shape == "" {
		shape = "upload"
	}
	outDir := os.Getenv("ONEDRIVE_PLAN_PROFILE_DIR")
	require.NotEmpty(t, outDir, "ONEDRIVE_PLAN_PROFILE_DIR")
	require.NoError(t, os.MkdirAll(outDir, 0o750))
	tag := fmt.Sprintf("%s-%d", shape, files)
	outFile, err := os.Create(filepath.Join(outDir, "stages-"+tag+".txt"))
	require.NoError(t, err)
	defer outFile.Close()
	run := &ppRun{Shape: shape, Files: files, t: t, out: outFile}

	ctx := context.Background()
	tmpDir, err := os.MkdirTemp(outDir, "run-"+tag+"-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	logFile, err := os.Create(filepath.Join(outDir, "engine-"+tag+".log"))
	require.NoError(t, err)
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))

	setupStart := time.Now()
	run.Folders = ppBuildTree(t, syncRoot, files)
	if shape == "mirror" {
		ppPopulateRemote(t, remote, syncRoot)
	}
	run.note("setup: tree of %d files in %d folders built in %.1fs", files, run.Folders, time.Since(setupStart).Seconds())

	eng, _ := newTestEngineInDirWith(t, ctx, remote.client(t), tmpDir, func(in *engineInputs) {
		in.Logger = logger
	})
	e := eng.Engine
	store := e.baseline

	if shape == "baseline" {
		// Untimed setup pass: scan once, seed baseline and remote_state from
		// the scan, mark the remote observed, then edit one file.
		setup := newOneShotRunner(e)
		bl, err := setup.runStartupStage(ctx)
		require.NoError(t, err)
		_, pending, err := setup.observeAndCommitRemoteCurrentState(ctx, bl, true)
		require.NoError(t, err)
		_, err = setup.commitPendingRemoteObservation(ctx, pending)
		require.NoError(t, err)
		scan, err := setup.refreshLocalCurrentState(ctx, bl)
		require.NoError(t, err)
		require.NoError(t, setup.commitCurrentLocalSnapshot(ctx, scan))
		seedStart := time.Now()
		ppSeedBaseline(t, ctx, store, buildLocalStateRows(scan))
		run.note("setup: seeded %d baseline+remote rows in %.1fs", len(scan.Rows), time.Since(seedStart).Seconds())
		edited := filepath.Join(syncRoot, "d000", "s000", "f00.txt")
		require.NoError(t, os.WriteFile(edited, []byte("edited once\n"), 0o640))
	}

	// ---- measured section -------------------------------------------------
	cpuFile, err := os.Create(filepath.Join(outDir, "cpu-"+tag+".pprof"))
	require.NoError(t, err)
	require.NoError(t, pprof.StartCPUProfile(cpuFile))
	measuredStart := time.Now()

	runner := newOneShotRunner(e)
	var bl *Baseline
	run.stage("startup (drive proof + baseline Load)", func() {
		bl, err = runner.runStartupStage(ctx)
		require.NoError(t, err)
	})
	var fullRefresh bool
	run.stage("decide full remote refresh", func() {
		fullRefresh, err = e.shouldRunFullRemoteRefresh(ctx, false)
		require.NoError(t, err)
	})
	run.note("full remote refresh: %v", fullRefresh)
	var pendingRemote *remoteObservationBatch
	run.stage("observe remote + commit remote_state", func() {
		_, pendingRemote, err = runner.observeAndCommitRemoteCurrentState(ctx, bl, fullRefresh)
		require.NoError(t, err)
	})
	var scan scanResult
	run.stage("observe local (walk + hash)", func() {
		scan, err = runner.refreshLocalCurrentState(ctx, bl)
		require.NoError(t, err)
	})
	run.note("local scan rows: %d", len(scan.Rows))
	run.stage("commit local snapshot (ReplaceLocalState)", func() {
		require.NoError(t, runner.commitCurrentLocalSnapshot(ctx, scan))
	})

	// loadCurrentInputsTx, step by step.
	inputs := ppLoadCurrentInputs(t, ctx, run, runner.engineFlow, store, driveID)
	run.note("inputs: comparisons=%d reconciliations=%d local=%d remote=%d baseline=%d",
		len(inputs.comparisons), len(inputs.reconciliations), len(inputs.localRows), len(inputs.remoteRows), inputs.baseline.Len())

	// PlanCurrentState, step by step.
	plan := ppPlanCurrentState(t, run, e.planner, &inputs, bl, driveID)
	run.Actions = len(plan.Actions)
	counts := countByType(plan.Actions)
	run.note("plan: %d actions (uploads=%d folder_creates=%d baseline_updates=%d downloads=%d)",
		len(plan.Actions), counts[ActionUpload], counts[ActionFolderCreate], counts[ActionBaselineUpdate], counts[ActionDownload])

	run.stage("applyDeleteSafetyHold", func() {
		require.NoError(t, applyDeleteSafetyHold(plan, &deleteSafetyInput{
			Baseline: planningBaseline(&inputs, bl), LocalRows: inputs.localRows, RemoteRows: inputs.remoteRows,
		}))
	})
	var report *Report
	run.stage("report counts", func() {
		report = buildReportFromCounts(countByType(plan.Actions), plan.DeferredByMode, SyncBidirectional, RunOptions{})
	})
	build := &builtCurrentPlan{Plan: plan, Report: report, PendingRemoteObservation: pendingRemote}

	var rt *runtimePlan
	run.stage("reconcileRuntimeStateStage (held deletes, retry prune)", func() {
		rt, err = runner.reconcileRuntimeStateStage(ctx, build)
		require.NoError(t, err)
	})
	run.stage("commit remote observation cursor", func() {
		_, err = runner.commitPendingRemoteObservation(ctx, rt.PendingRemoteObservation)
		require.NoError(t, err)
	})

	// startRuntimeStage, first half: graph build.
	var ready []*trackedAction
	run.stage("dep graph: initialize + Register + WireDeps", func() {
		runner.initializeRuntimeState(rt)
		runner.sched.graph = newDepGraph(runner.deps.logger)
		ready = runner.registerPlanActions(plan)
	})
	run.note("initially ready actions: %d, graph in flight: %d", len(ready), runner.sched.graph.InFlightCount())
	run.stage("admitReady (freshness checks of the ready frontier)", func() {
		ready, err = runner.admitReady(ctx, ready)
		require.NoError(t, err)
	})
	run.note("admitted: %d", len(ready))
	if os.Getenv("ONEDRIVE_PLAN_PROFILE_DRAIN") == "1" {
		// The rest of startRuntimeStage: publication-only actions
		// (baseline_update) are applied here, one CommitMutation each.
		var outbox []*trackedAction
		run.stage("publication drain (reduceReadyFrontierStage)", func() {
			outbox, err = runner.reduceReadyFrontierStage(ctx, bl, ready)
			require.NoError(t, err)
		})
		run.note("after drain: worker outbox=%d graph in flight=%d", len(outbox), runner.sched.graph.InFlightCount())
	}

	measured := time.Since(measuredStart)
	pprof.StopCPUProfile()
	require.NoError(t, cpuFile.Close())

	runtime.GC()
	heapFile, err := os.Create(filepath.Join(outDir, "heap-"+tag+".pprof"))
	require.NoError(t, err)
	require.NoError(t, pprof.WriteHeapProfile(heapFile))
	require.NoError(t, heapFile.Close())
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	run.note("heap in use after plan+graph: %.0f MB (sys %.0f MB)", float64(ms.HeapInuse)/1e6, float64(ms.Sys)/1e6)
	run.note("MEASURED TOTAL %.3fs (shape=%s files=%d folders=%d actions=%d)", measured.Seconds(), shape, files, run.Folders, len(plan.Actions))

	// Keep the graph alive until the heap profile is written.
	runtime.KeepAlive(ready)
	runtime.KeepAlive(rt)

	if shape == "baseline" {
		// Steady-state watch replan: no scan, only inputs + plan.
		replan := &ppRun{Shape: shape + "-replan", Files: files, Folders: run.Folders, t: t, out: outFile}
		replanStart := time.Now()
		flow := newEngineFlow(e)
		in2 := ppLoadCurrentInputs(t, ctx, replan, flow, store, driveID)
		p2 := ppPlanCurrentState(t, replan, e.planner, &in2, bl, driveID)
		replan.Actions = len(p2.Actions)
		replan.note("REPLAN TOTAL (inputs + plan, no scan) %.3fs actions=%d", time.Since(replanStart).Seconds(), len(p2.Actions))
		writeJSON(t, filepath.Join(outDir, "stages-"+tag+"-replan.json"), replan)
	}

	if os.Getenv("ONEDRIVE_PLAN_PROFILE_DRYRUN") == "1" {
		// Cross-check against the public entry point and its perf counter.
		eng2, _ := newTestEngineInDirWith(t, ctx, remote.client(t), tmpDir+"-dry", func(in *engineInputs) {
			in.Logger = logger
			in.SyncRoot = syncRoot
		})
		eng2.perfCollector = perf.NewCollector(nil)
		start := time.Now()
		rep, err := eng2.RunOnce(ctx, SyncBidirectional, RunOptions{DryRun: true})
		require.NoError(t, err)
		snap := eng2.collector().Snapshot()
		run.note("DRYRUN RunOnce wall %.3fs: uploads=%d folder_creates=%d observe_time_ms=%d plan_time_ms=%d",
			time.Since(start).Seconds(), rep.Uploads, rep.FolderCreates, snap.ObserveTimeMS, snap.PlanTimeMS)
	}

	writeJSON(t, filepath.Join(outDir, "stages-"+tag+".json"), run)
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, data, 0o640))
}

// ppLoadCurrentInputs is loadCurrentInputsTx with a timer between its steps,
// and replacePlannerVisibleStateTx inlined for the same reason.
func ppLoadCurrentInputs(
	t *testing.T, ctx context.Context, run *ppRun, flow *engineFlow, store *SyncStore, defaultDriveID driveid.ID,
) currentInputs {
	t.Helper()
	tx, err := beginPerfTx(ctx, store.db)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, pragma := range []string{"page_size", "cache_size", "temp_store", "synchronous", "journal_mode"} {
		var v any
		require.NoError(t, tx.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v))
		run.note("pragma %s = %v", pragma, v)
	}

	var rawLocalRows []localStateRow
	run.stage("inputs: listLocalStateRows", func() {
		rawLocalRows, err = listLocalStateRows(ctx, tx)
		require.NoError(t, err)
	})
	var observationState *observationState
	run.stage("inputs: readObservationStateTx", func() {
		observationState, err = store.readObservationStateTx(ctx, tx)
		require.NoError(t, err)
	})
	contentDriveID := observationState.ContentDriveID
	if contentDriveID.IsZero() {
		contentDriveID = defaultDriveID
	}
	var remoteRows []remoteStateRow
	run.stage("inputs: query remote_state rows", func() {
		remoteRows, err = queryRemoteStateRowsWithRunner(ctx, tx, `SELECT `+sqlSelectRemoteStateCols+` FROM remote_state`, contentDriveID)
		require.NoError(t, err)
	})
	var localRows []localStateRow
	run.stage("inputs: content filters", func() {
		localRows = filterLocalStateRowsForPlanning(rawLocalRows, flow.engine.contentFilter)
		remoteRows = filterRemoteStateRowsForPlanning(remoteRows, flow.engine.contentFilter)
	})

	// replacePlannerVisibleStateTx, inlined.
	run.stage("temp: create + delete planner_visible tables", func() {
		for _, q := range []string{
			sqlCreatePlannerVisibleLocalState, sqlCreatePlannerVisibleRemoteState,
			sqlDeletePlannerVisibleLocalState, sqlDeletePlannerVisibleRemoteState,
		} {
			_, err = tx.ExecContext(ctx, q)
			require.NoError(t, err)
		}
	})
	run.stage("temp: insert local rows one by one", func() {
		ppInsertPlannerVisibleLocal(t, ctx, tx, localRows)
	})
	run.stage("temp: insert remote rows one by one", func() {
		ppInsertPlannerVisibleRemote(t, ctx, tx, remoteRows)
	})
	if os.Getenv("ONEDRIVE_PLAN_PROFILE_FAST_PRUNE") == "1" {
		run.stage("temp: prune local descendants FAST (candidate)", func() {
			_, err = tx.ExecContext(ctx, ppSQLPruneLocalFast)
			require.NoError(t, err)
		})
		run.stage("temp: prune remote descendants FAST (candidate)", func() {
			_, err = tx.ExecContext(ctx, ppSQLPruneRemoteFast)
			require.NoError(t, err)
		})
	} else {
		run.stage("temp: prune local descendants (DELETE..EXISTS)", func() {
			_, err = tx.ExecContext(ctx, sqlPrunePlannerVisibleLocalDescendants)
			require.NoError(t, err)
		})
		run.stage("temp: prune remote descendants (DELETE..EXISTS)", func() {
			_, err = tx.ExecContext(ctx, sqlPrunePlannerVisibleRemoteDescendants)
			require.NoError(t, err)
		})
	}
	var visible plannerVisibleRows
	run.stage("temp: re-list planner_visible rows", func() {
		visible.Local, err = listLocalStateRowsWithQuery(ctx, tx, sqlListPlannerVisibleLocalState)
		require.NoError(t, err)
		visible.Remote, err = queryRemoteStateRowsWithRunner(ctx, tx, sqlListPlannerVisibleRemoteState, contentDriveID)
		require.NoError(t, err)
	})

	var comparisons []sQLiteComparisonRow
	run.stage("sql: comparison query", func() {
		comparisons, err = queryComparisonStateWithRunnerForTables(ctx, tx, plannerVisibleLocalStateTable, plannerVisibleRemoteStateTable)
		require.NoError(t, err)
	})
	var reconciliations []sQLiteReconciliationRow
	run.stage("sql: reconciliation query", func() {
		reconciliations, err = queryReconciliationStateWithRunnerForTables(ctx, tx, plannerVisibleLocalStateTable, plannerVisibleRemoteStateTable)
		require.NoError(t, err)
	})
	var issues []ObservationIssueRow
	run.stage("inputs: observation issues", func() {
		issues, err = queryObservationIssueRowsWithRunner(ctx, tx)
		require.NoError(t, err)
		issues = filterObservationIssueRowsForPlanning(issues, flow.engine.contentFilter)
	})
	var snapshotBaseline *Baseline
	run.stage("inputs: loadBaselineWithRunner (second Baseline copy)", func() {
		snapshotBaseline, err = loadBaselineWithRunner(ctx, tx, contentDriveID)
		require.NoError(t, err)
	})
	return currentInputs{
		baseline: snapshotBaseline, comparisons: comparisons, reconciliations: reconciliations,
		localRows: visible.Local, remoteRows: visible.Remote, observationIssues: issues,
	}
}

func ppInsertPlannerVisibleLocal(t *testing.T, ctx context.Context, tx sqlTxRunner, rows []localStateRow) {
	t.Helper()
	for i := range rows {
		row := rows[i]
		_, err := tx.ExecContext(ctx, sqlInsertPlannerVisibleLocalState,
			row.Path, row.ItemType, nullString(row.Hash), nullKnownInt64(row.Size, true), nullOptionalInt64(row.Mtime),
			row.Ctime, int64(row.LocalDevice), int64(row.LocalInode), row.LocalBirthTime, boolInt(row.LocalHasIdentity))
		require.NoError(t, err)
	}
}

func ppInsertPlannerVisibleRemote(t *testing.T, ctx context.Context, tx sqlTxRunner, rows []remoteStateRow) {
	t.Helper()
	for i := range rows {
		row := rows[i]
		_, err := tx.ExecContext(ctx, sqlInsertPlannerVisibleRemoteState,
			row.DriveID.String(), row.ItemID, row.Path, row.ItemType, nullString(row.Hash),
			nullKnownInt64(row.Size, true), nullOptionalInt64(row.Mtime), nullString(row.ETag))
		require.NoError(t, err)
	}
}

// ppPlanCurrentState is planner.PlanCurrentState with a timer between its
// steps. isolateDependencyCycles is split into buildDependencies and the
// cycle search so the quadratic part is visible on its own.
func ppPlanCurrentState(
	t *testing.T, run *ppRun, p *planner, inputs *currentInputs, fallback *Baseline, driveID driveid.ID,
) *actionPlan {
	t.Helper()
	baseline := planningBaseline(inputs, fallback)
	comparisons, reconciliations := inputs.comparisons, inputs.reconciliations
	mode := SyncBidirectional

	var truthStatusByPath map[string]pathTruthStatus
	run.stage("plan: truth availability index", func() {
		truthIndex := newTruthAvailabilityIndex(inputs.observationIssues)
		truthStatusByPath = truthIndex.StatusByPath(comparisonPaths(comparisons))
	})
	var views map[string]*pathView
	var comparisonByPath map[string]*sQLiteComparisonRow
	var err error
	run.stage("plan: buildSQLitePathViews", func() {
		views, comparisonByPath, err = buildSQLitePathViews(comparisons, inputs.localRows, inputs.remoteRows, baseline, truthStatusByPath)
		require.NoError(t, err)
	})
	var moveInputs moveInputs
	var composition moveComposition
	run.stage("plan: move composition", func() {
		moveInputs = moveCompositionInputs(comparisons, views, inputs.remoteRows, inputs.localRows)
		composition = composeMoves(moveInputs.nodes, moveInputs.occupancy, moveInputs.frozen)
	})
	var allActions []Action
	run.stage("plan: buildActionsForReconciliation loop", func() {
		for i := range reconciliations {
			rec := reconciliations[i]
			cmp := comparisonByPath[rec.Path]
			require.NotNil(t, cmp)
			actions, err := buildActionsForReconciliation(&rec, cmp, views)
			require.NoError(t, err)
			allActions = append(allActions, actions...)
		}
	})
	run.stage("plan: composed move actions", func() {
		allActions = dropReconciliationOfComposedMoves(allActions, &composition)
		allActions = append(allActions, buildComposedMoveActions(&composition, &moveInputs, views, baseline)...)
	})
	var normalized []Action
	run.stage("plan: normalizeCurrentPlanActions", func() {
		normalized, _ = normalizeCurrentPlanActions(allActions, remoteToLocalFolderPrefixes(moveInputs.nodes), mode)
	})
	run.stage("plan: dropCreatesOntoComposedItems", func() {
		normalized = dropCreatesOntoComposedItems(normalized, &moveInputs, &composition)
	})
	var admitted []Action
	var deferred DeferredCounts
	run.stage("plan: partition by mode + bindMountContext", func() {
		admitted, deferred = partitionCurrentActionsForMode(normalized, mode)
		bindMountContext(admitted, plannerMountContext{DriveID: driveID})
	})
	var deps [][]int
	skipOriginal := os.Getenv("ONEDRIVE_PLAN_PROFILE_SKIP_ORIGINAL_DEPS") == "1"
	if !skipOriginal {
		run.stage("plan: buildDependencies", func() {
			deps = buildDependencies(admitted)
		})
	}
	run.stage("plan: buildDependencies INDEXED (candidate fix)", func() {
		indexed := ppBuildDependenciesIndexed(admitted)
		if skipOriginal {
			deps = indexed
			return
		}
		for i := range deps {
			want := slices.Compact(deps[i])
			if len(want) == 0 {
				want = nil
			}
			if len(indexed[i]) == 0 {
				indexed[i] = nil
			}
			require.Equal(t, want, indexed[i], "deps differ at action %d %s %s", i, admitted[i].Type, admitted[i].Path)
		}
	})
	run.stage("plan: dependencyCycleMembers (Tarjan)", func() {
		onCycle := dependencyCycleMembers(deps)
		require.Empty(t, onCycle)
	})
	var plan *actionPlan
	run.stage("plan: assemble + countRenames", func() {
		plan = &actionPlan{
			Actions: admitted, Deps: deps, DeferredByMode: deferred,
			FollowUp: planFollowUp{Renames: countRenames(admitted), Deferred: len(composition.deferred)},
		}
		logActionPlanSummary(p.logger, "harness plan complete", plan)
	})
	return plan
}

// ---------------------------------------------------------------------------
// Candidate fix: buildDependencies with indexes instead of all-pairs scans.
// Same edges as buildDependencies (checked by TestPlanProfileIndexedDepsEqual
// on random plans and by the harness on its real plans); only the way the
// candidates are found changes:
//   - moves are looked up by Path and OldPath at the ancestors of an action's
//     paths (every rule of movePrecedes needs one of those to match);
//   - work under a move's source is found by walking each action's ancestors
//     to the moves whose OldPath is one of them;
//   - child deletes and moves out of a deleted folder are attributed to the
//     folder deletes among their ancestors;
//   - case-variant remote names are matched through a folded path key
//     (namekey.Key per segment), which is what namekey.EqualPath compares.
// ---------------------------------------------------------------------------

func ppFoldPath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = namekey.Key(s)
	}
	return strings.Join(segments, "/")
}

// ppAncestors calls fn for p and each of its ancestor directories ("a/b/c",
// "a/b", "a"), never for ".".
func ppAncestors(p string, fn func(string)) {
	for p != "" && p != "." {
		fn(p)
		dir := path.Dir(p)
		if dir == p {
			return
		}
		p = dir
	}
}

func ppBuildDependenciesIndexed(actions []Action) [][]int {
	deps := make([][]int, len(actions))

	folderCreateIdx := make(map[string]int)
	conflictCopyIdx := make(map[string]int)
	deleteIdx := make(map[string]int)
	// Exact-duplicate-safe indexes (several actions may share a path).
	deletesOrCopiesByPath := map[string][]int{}
	folderDeletesByPath := map[string][]int{} // LocalDelete/RemoteDelete of a folder
	movesByPath := map[string][]int{}
	movesByOldPath := map[string][]int{}
	vacatedByKey := map[string][]int{} // remote deletes (Path) and remote moves (OldPath)
	var moveIdx []int

	for i := range actions {
		a := &actions[i]
		if a.Type == ActionFolderCreate {
			folderCreateIdx[a.Path] = i
		}
		if a.Type == ActionConflictCopy {
			conflictCopyIdx[a.Path] = i
			deletesOrCopiesByPath[a.Path] = append(deletesOrCopiesByPath[a.Path], i)
		}
		if isDeleteAction(a) {
			deleteIdx[a.Path] = i
			deletesOrCopiesByPath[a.Path] = append(deletesOrCopiesByPath[a.Path], i)
			if (a.Type == ActionLocalDelete || a.Type == ActionRemoteDelete) && deletedItemType(a) == ItemTypeFolder {
				folderDeletesByPath[a.Path] = append(folderDeletesByPath[a.Path], i)
			}
		}
		if isMoveAction(a) {
			moveIdx = append(moveIdx, i)
			movesByPath[a.Path] = append(movesByPath[a.Path], i)
			movesByOldPath[a.OldPath] = append(movesByOldPath[a.OldPath], i)
		}
		switch a.Type {
		case ActionRemoteDelete:
			key := ppFoldPath(a.Path)
			vacatedByKey[key] = append(vacatedByKey[key], i)
		case ActionRemoteMove:
			key := ppFoldPath(a.OldPath)
			vacatedByKey[key] = append(vacatedByKey[key], i)
		}
	}

	// Rule (2): child delete/cleanup before parent folder delete, and (4):
	// a folder delete waits for same-side moves out of it. Both attribute
	// the child to the folder deletes among its ancestors.
	for childPath, childIdx := range deleteIdx {
		ppAncestors(path.Dir(childPath), func(anc string) {
			for _, j := range folderDeletesByPath[anc] {
				if j != childIdx {
					deps[j] = append(deps[j], childIdx)
				}
			}
		})
	}
	for _, m := range moveIdx {
		move := &actions[m]
		if move.OldPath == "" {
			continue
		}
		ppAncestors(path.Dir(move.OldPath), func(anc string) {
			for _, j := range folderDeletesByPath[anc] {
				a := &actions[j]
				sameSide := (a.Type == ActionRemoteDelete && move.Type == ActionRemoteMove) ||
					(a.Type == ActionLocalDelete && move.Type == ActionLocalMove)
				if j != m && sameSide {
					deps[j] = append(deps[j], m)
				}
			}
		})
	}

	seen := make(map[int]struct{}, 8)
	for i := range actions {
		a := &actions[i]
		deps[i] = addParentFolderDep(deps[i], i, a, folderCreateIdx)
		deps[i] = addConflictCopyDep(deps[i], i, a, conflictCopyIdx)
		deps[i] = addSamePathReplacementDep(deps[i], i, a, deleteIdx)

		// Rule (3a): moves that must run before a. movePrecedes(move, a)
		// needs move.Path ∈ {a.Path} ∪ anc(a.Path) ∪ anc(a.OldPath) or
		// move.OldPath ∈ {a.Path} ∪ anc(a.Path).
		clear(seen)
		consider := func(candidates []int) {
			for _, m := range candidates {
				if m == i {
					continue
				}
				if _, dup := seen[m]; dup {
					continue
				}
				seen[m] = struct{}{}
				if movePrecedes(&actions[m], a) {
					deps[i] = append(deps[i], m)
				}
			}
		}
		ppAncestors(a.Path, func(anc string) {
			consider(movesByPath[anc])
			consider(movesByOldPath[anc])
		})
		if isMoveAction(a) && a.OldPath != "" {
			ppAncestors(path.Dir(a.OldPath), func(anc string) { consider(movesByPath[anc]) })
		}
		// Rule (3c): the old occupant of a move's destination leaves first.
		if isMoveAction(a) {
			for _, j := range deletesOrCopiesByPath[a.Path] {
				if j != i {
					deps[i] = append(deps[i], j)
				}
			}
		}
		// Rule (6): remote case-variant names.
		switch {
		case a.Type == ActionUpload, a.Type == ActionRemoteMove,
			a.Type == ActionFolderCreate && a.CreateSide == CreateRemote:
			for _, j := range vacatedByKey[ppFoldPath(a.Path)] {
				other := &actions[j]
				if j == i {
					continue
				}
				var vacated string
				switch {
				case other.Type == ActionRemoteDelete:
					vacated = other.Path
				case other.Type == ActionRemoteMove && a.Type != ActionRemoteMove:
					vacated = other.OldPath
				default:
					continue
				}
				if vacated == a.Path || vacated == a.OldPath || !namekey.EqualPath(vacated, a.Path) {
					continue
				}
				if a.Type == ActionRemoteMove && strings.HasPrefix(a.OldPath, vacated+"/") {
					continue
				}
				deps[i] = append(deps[i], j)
			}
		}
	}

	// Rule (3b): a move waits for the work under its source that does not
	// follow it. underMoveSource(other, move) needs movedSourcePath(other,
	// move) strictly under move.OldPath: attribute each action to the moves
	// whose OldPath is an ancestor of its Path (or of its OldPath, for a
	// move of the same type).
	for j := range actions {
		other := &actions[j]
		attribute := func(anc string) {
			for _, m := range movesByOldPath[anc] {
				if m == j {
					continue
				}
				move := &actions[m]
				if underMoveSource(other, move) && !underMoveDestination(other, move) {
					deps[m] = append(deps[m], j)
				}
			}
		}
		ppAncestors(path.Dir(other.Path), attribute)
		if isMoveAction(other) && other.OldPath != "" {
			ppAncestors(path.Dir(other.OldPath), attribute)
		}
	}

	for i := range deps {
		if len(deps[i]) > 1 {
			sort.Ints(deps[i])
			deps[i] = slices.Compact(deps[i])
		}
	}

	return deps
}

// ppNormalizeDeps dedupes the original's edges too (the original can add the
// same edge from two rules; the graph treats it as one dependency either way
// is NOT true for WireDeps, which counts duplicates — so duplicates are
// reported by the equivalence test rather than hidden).
func ppRandomPlan(rng *rand.Rand, n int) []Action {
	names := []string{"a", "A", "b", "c", "ä", "d", "e"}
	randPath := func() string {
		depth := 1 + rng.IntN(3)
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = names[rng.IntN(len(names))]
		}
		return strings.Join(parts, "/")
	}
	types := []actionType{
		ActionDownload, ActionUpload, ActionLocalDelete, ActionRemoteDelete, ActionLocalMove, ActionRemoteMove,
		ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup,
	}
	actions := make([]Action, n)
	for i := range actions {
		a := Action{Type: types[rng.IntN(len(types))], Path: randPath()}
		itemType := ItemTypeFile
		if rng.IntN(2) == 0 {
			itemType = ItemTypeFolder
		}
		a.View = &pathView{Path: a.Path, Baseline: &BaselineEntry{Path: a.Path, ItemType: itemType}}
		if rng.IntN(2) == 0 {
			a.View.Local = &localState{ItemType: itemType}
		}
		if rng.IntN(2) == 0 {
			a.View.Remote = &remoteState{ItemType: itemType}
		}
		if isMoveAction(&a) || (a.Type == ActionBaselineUpdate && rng.IntN(2) == 0) {
			a.OldPath = randPath()
		}
		if a.Type == ActionFolderCreate {
			a.CreateSide = createLocal
			if rng.IntN(2) == 0 {
				a.CreateSide = CreateRemote
			}
		}
		actions[i] = a
	}
	return actions
}

func TestPlanProfileIndexedDepsEqual(t *testing.T) {
	if os.Getenv("ONEDRIVE_PLAN_PROFILE_EQUIV") == "" {
		t.Skip("set ONEDRIVE_PLAN_PROFILE_EQUIV=1 to run the indexed buildDependencies equivalence check")
	}
	for seed := uint64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919))
		actions := ppRandomPlan(rng, 20+rng.IntN(400))
		want := buildDependencies(actions)
		for i := range want {
			want[i] = slices.Compact(want[i]) // original is sorted; duplicates from two rules collapse
			if len(want[i]) == 0 {
				want[i] = nil
			}
		}
		got := ppBuildDependenciesIndexed(actions)
		for i := range got {
			if len(got[i]) == 0 {
				got[i] = nil
			}
		}
		require.Equal(t, want, got, "seed %d", seed)
	}
}


// ---------------------------------------------------------------------------
// Candidate fix: prune planner-visible descendants from the (small) set of
// baseline folders missing on that side, with a path-range per missing
// folder on the TEMP table's primary key, instead of a correlated scan of
// baseline for every planner-visible row.
// ---------------------------------------------------------------------------

const ppSQLPruneLocalFast = `WITH missing AS MATERIALIZED (
	SELECT parent.path AS path
	FROM baseline parent
	LEFT JOIN planner_visible_local_state parent_local ON parent_local.path = parent.path
	WHERE parent.item_type = 'folder' AND parent.path <> '' AND parent_local.path IS NULL
)
DELETE FROM planner_visible_local_state
WHERE path IN (
	SELECT pv.path FROM missing m
	JOIN planner_visible_local_state pv ON pv.path > m.path || '/' AND pv.path < m.path || '0'
)`

const ppSQLPruneRemoteFast = `WITH missing AS MATERIALIZED (
	SELECT parent.path AS path
	FROM baseline parent
	LEFT JOIN planner_visible_remote_state parent_remote ON parent_remote.path = parent.path
	WHERE parent.item_type = 'folder' AND parent.path <> '' AND parent_remote.path IS NULL
)
DELETE FROM planner_visible_remote_state
WHERE path IN (
	SELECT pv.path FROM missing m
	JOIN planner_visible_remote_state pv ON pv.path > m.path || '/' AND pv.path < m.path || '0'
)`

// TestPlanProfileExplainPrune prints the query plans of the original and the
// candidate prune statements and checks they delete the same rows on a tree
// with missing folders at several depths.
func TestPlanProfileExplainPrune(t *testing.T) {
	if os.Getenv("ONEDRIVE_PLAN_PROFILE_EQUIV") == "" {
		t.Skip("set ONEDRIVE_PLAN_PROFILE_EQUIV=1")
	}
	ctx := context.Background()
	remaining := func(prune string) []string {
		store := newTestStore(t)
		tx, err := store.rawDB().BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback() //nolint:errcheck
		for _, q := range []string{sqlCreatePlannerVisibleLocalState, sqlCreatePlannerVisibleRemoteState} {
			_, err = tx.ExecContext(ctx, q)
			require.NoError(t, err)
		}
		// baseline folders: a, a/b, c, d, d/e; a and d/e are missing locally.
		for i, p := range []string{"a", "a/b", "c", "d", "d/e"} {
			_, err = tx.ExecContext(ctx, sqlUpsertBaseline, baselineRowArgs(&BaselineEntry{Path: p, ItemID: fmt.Sprintf("f%d", i), ItemType: ItemTypeFolder})...)
			require.NoError(t, err)
		}
		_, err = tx.ExecContext(ctx, sqlUpsertBaseline, baselineRowArgs(&BaselineEntry{Path: "a/b/file", ItemID: "ff", ItemType: ItemTypeFile})...)
		require.NoError(t, err)
		for _, p := range []string{"a/b", "a/b/y.txt", "a/x.txt", "ab/z.txt", "c", "c/z.txt", "d", "d/e/w.txt", "d/f.txt", "d/e2/q.txt"} {
			it := ItemTypeFile
			if p == "a/b" || p == "c" || p == "d" {
				it = ItemTypeFolder
			}
			_, err = tx.ExecContext(ctx, sqlInsertPlannerVisibleLocalState, p, it, nil, 0, nil, 0, 0, 0, 0, 0)
			require.NoError(t, err)
		}
		rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+prune)
		require.NoError(t, err)
		for rows.Next() {
			var id, parent, notused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
			t.Logf("  plan: %*s%s", 2*depthOf(id, parent), "", detail)
		}
		require.NoError(t, rows.Close())
		_, err = tx.ExecContext(ctx, prune)
		require.NoError(t, err)
		left, err := tx.QueryContext(ctx, "SELECT path FROM planner_visible_local_state ORDER BY path")
		require.NoError(t, err)
		var out []string
		for left.Next() {
			var p string
			require.NoError(t, left.Scan(&p))
			out = append(out, p)
		}
		require.NoError(t, left.Close())
		return out
	}
	t.Log("ORIGINAL sqlPrunePlannerVisibleLocalDescendants:")
	orig := remaining(sqlPrunePlannerVisibleLocalDescendants)
	t.Log("CANDIDATE ppSQLPruneLocalFast:")
	fast := remaining(ppSQLPruneLocalFast)
	t.Logf("remaining after original:  %v", orig)
	t.Logf("remaining after candidate: %v", fast)
	require.Equal(t, orig, fast)
	require.Equal(t, []string{"ab/z.txt", "c", "c/z.txt", "d", "d/e2/q.txt", "d/f.txt"}, fast)
}

func depthOf(_, parent int) int {
	if parent == 0 {
		return 0
	}
	return 1
}
```
