# R9 report: state and planner (branch `refactor/state-planner`)

Base `f0ac23e5`. Built F1, F2, F3, F4, one commit each. **C3 not built** (below).
Worktree `~/src/onedrive-go-wt/state-planner`. **Head `9f9c9471`, pushed** (`refactor/state-planner`), no PR.
**PAUSED on the coordinator's order** (owner priority: slow unit tests first): the final green `pre-pr` is still owed; see Gates.

## Commits
| Row | Commit | Summary |
| --- | --- | --- |
| F1 | `268d9d9c` Hold the baseline outside the store as a read-only view | `syncdomain.BaselineView` (by-value lookups) is all executor, observers, converter, planner, watch, verify hold; writable cache moved into the store as `baselineCache` (unexported indexes, `put`/`delete`/`swap`); immutable `syncdomain.BaselineSnapshot` for snapshots and tests. `...HeldBaselinePointerCurrent` rewritten as `TestCommitMutation_FolderMoveKeepsHeldBaselineViewCurrent` (also proves a value read before a move keeps its old path). |
| F2 | `a82b5086` Leave nothing fallible after COMMIT; plan and dry-run from the database | `baselineCachePatch` decided inside the tx (put/delete/rebuild read through the tx) for `CommitMutation`, `CommitObservation`, `CommitLocalSnapshot`; applied after COMMIT, cannot fail. `CheckCacheConsistency` compares every field, returns `ErrBaselineCacheDiverged`, runs in `assertStoreIntegrity`. Planner and delete-safety hold take `*BaselineSnapshot`; `planningBaseline` fallback gone. Dry-run scratch store copies the baseline table in its seed tx. |
| F3 | `745e43c5` Decide in Go too, and hold every plan's SQL decisions to it | `syncdomain.Decide`/`DecisionState`/`SideType`/`Reconcile`/`Valid`; comparison query widened to every table input; every plan ran `CheckDecisionsAgree` (fail the pass on disagreement). |
| F4 | `9f9c9471` Decide in Go; delete the SQL decision CASEs | One query per plan (`sqlQueryPlannerState`), `QueryPlannerRows` decides in Go; both CASEs, the reconciliation query, the agreement check and the unclassified-row diagnostic query deleted; unclassified rows carry `DecisionState.Inputs`; engine test hook is `decideOverride`. Benchmarks recorded in `performance-benchmarking.md`. |

## Agreement proof (F4 changes file operations only if Go and SQL disagree)
- At F3: `TestDecide_ComparisonMatchesTheSQLCaseOnEveryInput` (all 131,072 SQL stage-1 inputs) and `TestReconcile_MatchesTheSQLCaseOnEveryInput` (all 442,368 stage-2 inputs): zero disagreements. Every plan of the full `internal/sync` suite (engine model, scenarios, crash points), `syncplan` (matrix, planner model) ran the per-plan agreement check: all green at the F3 commit (before final fixups; sha then `e6392978`) (sync 973 s, syncplan 931 s).
- Matrix golden byte-identical at F4 (`TestReconcileMatrix_Golden` without `-update`).
- One deliberate decision change, unreachable in practice: the `(absent, root, absent)` cell and its remote mirror were upload/download; now unclassified in both tables (F3), since no observer records a root-typed entry.

## Exhaustive decision test
`TestDecide_EveryValidStateHasTheTableProperties`: 8,388,608 states, 23,077 valid; properties hold (unclassified only with no side or a root create; no upload of a local folder, no download onto a remote folder; every reconciliation kind reached).

## Benchmarks (host the dev host, one iteration each, same session, another branch's tests also running)
`BenchmarkComparisonQueries` (100k / 10k items; quiet, remote rename, local rename):
| | Quiet | Remote rename | Local rename |
| --- | --- | --- | --- |
| 10k before (two queries) | 3.1 s | 1.6 s | 2.0 s |
| 10k after (one query + Go) | 0.43 s | 0.63 s | 0.74 s |
| 100k before | 16.6 s | 16.8 s | 16.6 s |
| 100k after | 4.7 s | 6.7 s | 7.6 s |
Earlier run on main the same day: 15.5 / 18.6 / 38.4 s (load). After is well under 23 s: F4 stays.
`BenchmarkBaselineLookups` (F1, 100k lookups by path+ID, one iteration, variants): before (pointer API, on `f0ac23e5`) 63-66 ms; after (by value) 84-103 ms.

## Generation change
None. No schema generation bump (C3 not built); no test-drive reset needed.

## Tests
Test functions under `internal/sync` + `syncverify`: 1929 before, 1930 after. Added: `TestBaselineSnapshot_HandsOutValuesItOwns`, `TestBaselineCache_GetCaseVariantsCopyIsSafeAgainstConcurrentPut` (renamed), `TestFault_FolderMoveCacheReloadFailureCommitsNothing`, `TestCommitMutation_UnknownActionChangesNeitherDatabaseNorCache` (replaces the reload test), `TestCheckCacheConsistency_ReportsAMismatchInAnyColumn`, `TestCreateScratchPlanningStore_CopiesTheDatabaseNotAStaleCache`, `TestDecide_EveryValidStateHasTheTableProperties`, `TestDecide_ARootOnlyOneSideHoldsIsUnclassified`, `TestDecisionState_ValidNamesTheBrokenRule`, `TestQueryPlannerRows_UnknownSideTypeFailsTheScanNamingThePath`, `TestQueryPlannerRows_GoDecidesEveryRowOfOneQuery`, benchmark `BenchmarkBaselineLookups`. Removed with the SQL (F4): the SQL token/literal test, both SQL stage tests, both oracles, the F3 disagreement test, `TestCreateScratchPlanningStore_RequiresBaseline`, `TestCloneBaselineEntries_ReturnsIndependentCopy`.

## Gates
- `verify --dod --stage start`: pass (nightly 44794563163 green, no claims).
- Lint (`internal/sync/...`, `internal/syncverify/...`, pinned linter): 0 issues (`reports/R9-lint.log`).
- Full `internal/sync/...` + `syncverify`, no `-short`, at F4 (`3ad3bc12`, before the last fixups): all packages ok (syncplan 913 s incl. matrix golden and planner model) except `TestPlanningLoadError_SameTextInOneShotAndWatch`, whose fault fragment named the deleted `comparison_state` CTE; fixed (`comparison_flags`) and that test plus the unclassified engine tests rerun green.
- `verify --dod --stage pre-pr`: NOT YET GREEN. Three runs, each stopped by one finding, each fixed into its row's commit: (1) retired-backlog-ids: `B-198` row removed (F2); (2) mutex-ownership-docs: `baselineCache.mu` documented (F1); (3) `go test -short ./...`: only `TestStressDeclarationsAreInNameOrder` failed (renamed stress member out of order, F1); fixed and `go test ./internal/devtool -run Stress` green. In run 3 lint, format, every repo-rule step and all other short tests passed. A fourth run was killed while queued (pause).
- Every commit compiles and vets (`go vet ./internal/...` at each of the four).

## Still needed when resumed (heavy runs)
1. One `verify --dod --stage pre-pr` on head `9f9c9471` (expected green: the only open finding of run 3 is fixed).
2. Optional: full non-short `./internal/sync/` once more on head (the last fixups are a test fragment, a doc comment, a registry row and a stress-list order).

## C3: not built, and the foreign-key form decided
Why stopped: REFACTOR-ORDER says C3's write-order rework "spans the same 5 paths R9 rewrites"; F1-F4 touch none of them. The five paths (`persistFailureWithScopeEffects` x2 arms, `applyTrialRetryFallback`, `applyTrialReclassification`, both `permissionEvidenceBoundaryDenied` arms) are engine runtime completion/trial/permission code, and one scope+blocked-row transaction means threading activation through `applyResultPersistence`; plus the generation bump, CHECKs, invariant deletions and the `hash_panic` rename over 16 files. That is a PR of its own, much larger than expected here.
Decided form (for whoever builds it): new column `retry_work.blocked_scope_key TEXT REFERENCES block_scopes(scope_key)` (NO ACTION, immediate), set exactly when a row is blocked under a scope that persists in `block_scopes` (`ScopeKey.PersistsInBlockScopes`), with `CHECK(blocked_scope_key IS NULL OR (blocked = 1 AND blocked_scope_key = scope_key))`; `block_scopes.scope_key NOT NULL CHECK(scope_key <> '')`. Blocked rows under read-permission scopes stay blocked with a NULL key (observation-owned, as designed); the start-up repair narrows to `blocked = 1 AND blocked_scope_key IS NULL` instead of being deleted, so their current release-at-start behaviour is kept. Rejected: giving read scopes `block_scopes` rows (the store refuses read boundaries there by design) and not recording read denials as blocked work (behaviour change).

## Parked
R9-1 .. R9-4 appended to `PARKED.md`.
