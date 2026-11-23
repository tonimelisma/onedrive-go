# R5 report: prepare the internal/sync split in place (S1-S4)

Branch `refactor/split-prepare`, worktree `~/src/onedrive-go-wt/split-prepare`,
on main `9b16a5c0` (#960). Four commits, one per row, pushed to `origin/refactor/split-prepare` (head `1bfd9ac4`); no PR, no
install. Model re-run: `reports/R5-model.md`. The configurations the final
verdict is for: `reports/R5-conf/` (`production.txt`, `tests.txt`,
`final.txt` = the two joined by `mkconf.py`).

## Verdict on the final tree

Simulator over `R5-conf/final.txt`, Linux and macOS file sets:

```
typecheck errors 0, violations 0, cycles 0, interface problems 0,
split const blocks 0, name collisions 0, illegal test references 0
import graph: ACYCLIC
export ledger: nothing left to export
production files in more than one package: 0; moving whole to another family: 0
test files whose declarations end up in more than one package: 0
```

(11-10 had 1 split const block and 2 name collisions; both are gone: the
block was split in S2, the two `localObserver` counters stay unexported
behind `RestoreDropped`.)

## Rows

| Row | Commit | Size | State |
|---|---|---|---|
| S1 | `e4d71da1` Export the sync identifiers other packages-to-be will use | 351 files, +8,395 / -8,395 | done |
| S2 | `9ec497e1` Give every sync file one package-to-be | 68 files, +1,555 / -1,468 | done |
| S3 | `a454c1a3` Reach sync's cross-package state through methods and seams | 25 files, +205 / -59 | done |
| S4 | `1bfd9ac4` Split sync's tests by package-to-be and guard test seams | 268 files, +8,913 / -7,701 (95 new files) | done |

Row mapping: the brief's S1 (re-run the model) is the start of S1; the
plan's S1 (export identifiers) is the S1 commit. S2 holds the plan's S2
(file moves); the brief's "renames" are the S1 exports.

### S1 (model re-run, exports)

- Model: production fits with five config lines (R5-model.md). Tests needed a
  re-solve, done in S4.
- 601 identifiers capitalised by a go/types tool over the type-checked package
  (both GOOS), by object identity: 611 objects with interface implementers and
  embedded fields, 8,871 identifier edits, 456 comment edits (a doc comment of
  a declaration that keeps its name is not touched), 160 backticked names in
  `spec/design`. 21 fields whose cross-package use gets a method in S3 stayed
  unexported. Source-reading tests that name identifiers in string literals
  follow (11 files).

### S2 (files)

- 135 declarations moved out of 25 mixed files, byte for byte, by a tool
  (`splitfiles`, plan generated from `rawdecls.tsv`): method/constant to its
  type's file, otherwise `<family prefix><old stem>.go`. 17 new files. One
  const block split (`MinPollInterval`, explicit values; it got a doc comment).
- 7 files that belonged wholly to another package renamed to that family
  (`store_types.go`->`domain_row_types.go`, `truth_status.go`->
  `domain_truth_status.go`, `sqlite_compare_kinds.go`->`domain_compare_kinds.go`,
  `observation_local_rules.go`->`domain_local_rules.go`,
  `shortcut_root_store.go`->`store_shortcut_roots.go`,
  `shortcut_root_status_read.go`->`store_shortcut_root_status_read.go`,
  `shortcut_alias_mutation.go`->`engine_shortcut_alias_mutation.go`). Every
  production file's family prefix now names its package, so R6 needs no
  `file`/`decl` lines for production.
- GOVERNS, routing table, design-doc file references, `store.go`'s file index
  and the stored-time site maps follow. The four domain files moved to
  `sync-domain.md` because the layering check (layer 0 may not depend upward)
  failed otherwise.

### S3 (seams)

- Methods replacing cross-package field access: `LocalObserver.SetClock`,
  `SetDebugEvents`, `SetSafetyScan`, `RestoreDropped`; `RemoteObserver.SetClock`,
  `SetDebugEvents`; `WorkerPool.SetRuntimeOptions`; `DepGraph.SetOwnerCheck`;
  `ExecutorConfig.SyncTree`; `NewRealClock`, `RealClock.Panics`;
  `SyncRootIdentity.CompleteBinding` (engine's mark-and-bind split into
  `commitLocalSnapshotBinding`); `DeltaExpiredError.Reason` (and
  `ResyncKindOf` for `.kind`); `ShortcutChildAckRef.Matches`;
  `SyncStore.BeginPerfTx`.
- `ExecutorStore` (six methods, `action_freshness.go`) replaces `*SyncStore` in
  exec; `executorStoreOf` keeps a nil store nil (the freshness check skips on
  a nil store). Named `ExecutorStore`, not the design's `syncexec.Store`, so
  it does not read as the store itself while in one package.
- The hash cache's `Lookup`/`Record`, `NewLocalHashCache` and the `Err`
  sentinels were already done by S1.

### S4 (tests)

- Test config re-solved from 311 illegal references to 0
  (`R5-conf/tests.txt`, "R5 decisions" block).
- Tests rewritten so each asserts one package (no production change):
  `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum` split into domain,
  `TestPlannerActionTypeDecisions_…`, `TestExecutorActionTypeDecisions_…`,
  `TestEngineActionTypeDecisions_…` (spec row updated);
  `TestPlannerModelRun_FailureContextCarriesNoSecondRerunHint` split out of
  `TestModelRun_FailureContextCarriesNoSecondRerunHint`; engine tests use the
  S3 methods instead of fields.
- 44 identifiers exported as ordinary API because only another package's
  tests use them (the 11-10 T3(b) list plus the comparison/reconciliation
  kind lists and members, `GraphRootID`, `HashRequest`, `SqliteFileURI`,
  `UploadTransferError`, …), by the same rename tool.
- 33 `…ForTest` seams in five production files (`store_test_seams.go`,
  `executor_test_seams.go`, `observer_test_seams.go`, `shortcut_test_seams.go`,
  `planner_test_seams.go`, each governed); 367 test call sites rewritten by a
  go/types tool (calls renamed, `x.f = v` to `x.SetFForTest(v)`), the rest
  by hand (DropBaselineCacheForTest, HasWindowForTest/WindowPathCountForTest,
  MountBoundaryFlagsForTest, PlanActionsForTest/DependencyCycleForTest).
  Existing setters were used where they exist (`SetPendingCreates`,
  `SetTransferMgr`).
- 458 test declarations moved out of 59 mixed test files into 90 target files
  by the same split tool; helper-package contents gathered in `fixture_*_test.go`
  (syncfixture) and `storetest_*_test.go` (syncstoretest).
- devtool test-conventions check: a production use of a `…ForTest` identifier
  fails, test files and `internal/sync/internal/{syncfixture,syncstoretest}`
  may use them (`TestRunTestConventionsRefusesAProductionUseOfATestSeam`, seen
  failing first; R-6.10.17 and system.md extended).

## Checks run

Every row: `go build ./...` (linux, darwin/arm64), `go vet` of internal/sync
with `e2e e2e_full integration` tags and under darwin, gofumpt, the governs
and layering gates (a temporary devtool test calling `runSpecGoverns` and
`findLayeringViolations`, deleted after), the devtool `RealRepoIsClean` tests,
the model. One `go test -p 1 -count=1 -timeout 90m` of internal/sync per row
behind `S/heavy.lock`, with multisync, cli (and syncverify / devtool):

- S1: sync failed 9 source-reading tests (identifier names in string
  literals); fixed, those tests rerun green; multisync, cli, syncverify ok.
- S2: sync failed `TestStoredTimes_*` (per-file site maps); fixed and rerun
  green; multisync, cli, devtool ok.
- S3: sync, multisync, cli ok.
- S4: sync failed `TestSQLiteDSNs_AreBuiltOnlyByTheTwoBuilders` (the builder's
  name in a string literal, `sqliteFileURI`); fixed with one stale message
  in `observation_findings_test.go`, rerun green; devtool, multisync, cli ok.

No `-race`, no `devtool verify`, no lint run (all at ship). No live e2e.

## Left for R6

- The model's private test copies (`testcopy` lines in `R5-conf/tests.txt`)
  cannot exist in one package; R6's generator must write them:
  `newTestExecutorConfig`, `newTestExecutorConfigWithPathConvergence`,
  `forwardedSnapshot`, `newWorkerTestSetup`, `adoptTestPlaceholder`,
  `newPendingCreateTestExecutor`, `planCurrentStateForStore*` (3),
  `testPlaceholderID`, `placeholderCreatedETag` copied into `sync` tests;
  `localBatches`, `drainLocalPatches` into `syncexec` tests;
  `newPendingCreateTestExecutor` and the store-template trio into `syncstore`
  tests.
- 15 test files sit in a package their family name does not say (the
  `testfile` lines, e.g. `pending_creates_test.go` in syncstore,
  `crash_points_test.go` in syncdomain); R6 maps them by config or renames.
  `fixture_`/`storetest_` test files become ordinary (non-`_test`) files of
  the two helper packages.
- `ExecutorStore` may be renamed to the design's `syncexec.Store` at the move.
- `TestPanicRecoverySitesCaptureTheStack` scans `../sync` non-recursively
  (end-state 10.6): its roots must cover the new package directories in R6.
- The design-doc file lists still name files by their in-place names.
- Model tools used and deleted after use: the rename tool, the split tool and
  the seam tool (scratch); the re-run commands are in R5-model.md.

Parked (PARKED.md): R5-1 (kind String fallback text), R5-2 (exported setter
with an unexported parameter type).


## Merged as #961

- Review fix `f340fb22`: the test-convention check refuses a production
  import of `syncfixture` or `syncstoretest` (new test
  `TestRunTestConventionsRefusesAProductionImportOfATestHelperPackage`,
  seen failing first; R-6.10.17 and system.md extended).
- Lint fix `1365799f` (found by pre-pr): method before its type in
  `domain_sqlite_rows.go`, duplicate `sync` import in
  `fixture_test_helpers_test.go`, unused `nolint:wrapcheck` in `worker.go`.
- `-race -p 1` of internal/sync (4736 s) and internal/devtool passed;
  pre-pr, pre-merge passed; four nightly claims released (parked as
  R2-n1, R2-n2, W101-n2). Squash merge `197f4d24`, installed 2026-11-20.
