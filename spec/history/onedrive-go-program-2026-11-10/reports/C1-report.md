# C1 report: planner kinds are typed; an unclassified row fails the plan

Branch `fix/planner-fail-closed` (worktree `~/src/onedrive-go-wt/planner-fail-closed`), one commit on `90c60a8f`.

## What changed
- `internal/sync/sqlite_compare_kinds.go` (new, store layer; moved out of `planner_kinds.go` in review after the pre-pr layering check refused the store's upward reference): `comparisonKind`, `reconciliationKind` (zero = unclassified), `String`, `parse*`, `all*Kinds`, `plannerDecisionInput`.
- `internal/sync/planner_kinds.go` (new, planning layer): `errUnclassifiedPlan`, `unclassifiedPlanError` (path, both kinds, decision inputs, count of further rows), `validatePlannerKinds`.
- `sqlite_compare.go`: CASEs split into `sqlComparisonKindCase` / `sqlReconciliationKindCase`; both ELSEs `'unclassified'`; dead `WHEN`s removed; typed row fields parsed in `scanComparisonRow` / `scanReconciliationRow` (unknown name fails naming the path); `queryComparisonRows` / `queryReconciliationRows` take the query text; `sqlQueryPlannerDecisionInputs` + `queryPlannerDecisionInputs`; row field `DecisionInputs`.
- `planner_actions.go`: validation pre-pass before any row (ahead of the unavailable-truth early return); typed switch with `reconcileUnclassified` returning the typed error; `errFolderTransferPlanned` returned, no longer logged and dropped.
- `planner_move_composition.go`: typed comparison constants; string const `comparisonKindLocalMoveSource` deleted.
- `planner.go`: `actionAllowedInMode` default and unknown `CreateSide` now `false`.
- `enums.go`: `Direction()` default returns the zero direction; stale doc comment (claimed retry_work uses it) corrected.
- `engine_current_plan.go`: `queryPlannerRowsTx` reads both row sets and, only when a row is unclassified, its decision inputs in the same read transaction.
- `engine.go`: test-only `reconciliationQueryOverride`.
- Tests touched for typed kinds: `planner_actions_test.go`, `comparison_hashless_test.go`, `sqlite_compare_test.go` (helpers), `engine_folder_rebind_test.go`; `dispatch_order_test.go` lists the new RunOnce scenario.
- Docs: `spec/design/sync-planning.md` (GOVERNS + `planner_kinds.go`, Implements R-6.2.16, Ownership error boundary, Verified By row, new "Decision Table Kinds" section, folder-transfer text), `sync-engine.md` (Verified By row, seam), `sync-domain.md` (Direction row), `spec/requirements/non-functional.md` new R-6.2.16 (S14).

## Tests added (all `Validates: R-6.2.16` unless noted)
- `TestPlannerKindCases_LiteralsAreExactlyTheKinds`: SQL THEN/ELSE literals = enum names, `comparison_kind = '...'` literals = classified comparison kinds, both ELSEs = unclassified.
- `TestPlannerKinds_ParseRoundTripsEveryMemberAndRefusesOthers`: round trip, distinct names, zero first, unknown refused.
- `TestQueryPlannerRows_UnknownKindFailsTheScanNamingThePath`: doctored `'bogus'` comparison / reconciliation kinds fail the scans naming the path.
- `TestComparisonKindCase_ClassifiesEveryReachableState`: stage 1, 131,072 states; unclassified iff no side present; diverged never changed-nowhere nor equal-after-remote-change; every kind reachable.
- `TestReconciliationKindCase_ReconcilesEveryClassifiedComparison`: stage 2, 442,368 inputs; unclassified only for an unclassified comparison or the stage-1-impossible diverged cell; every kind reachable.
- `TestPlanCurrentState_UnclassifiedRowFailsEvenWhereTruthIsUnavailable`: pre-pass beats the suppression (with a control), exact error text, `Others`.
- `TestPlanCurrentState_UnclassifiedComparisonRowFailsThePlan`: comparison-only row refused.
- `TestPlanCurrentState_FolderTransferFailsThePlan` (R-2.2.6 too): folder upload fails the plan.
- `TestRunOnce_UnclassifiedRowFailsThePassNamingThePath`: engine one-shot, `errors.Is`, wrap text with inputs, no upload.
- `TestRunSteadyStateReplan_UnclassifiedRowEndsTheReplanNamingThePath` (R-2.8.11 too): watch wrap text, nothing installed.
- `TestQueryReconciliationState_LocalEditEqualToUnchangedRemoteIsAnUpload` (R-2.2.1): the (diverged, 1, 0, equal) cell is `upload`.
- `TestActionAllowedInMode_AnUnlistedActionIsNeverAdmitted` (R-2.1.3/4); `TestActionTypeDirection` gained an unlisted case.

## Checks run
- Red: compile failure before the types; with the old `ELSE 'noop'` restored the token, stage 2 and both engine tests fail for the stated reason.
- No race: new tests pass; `-run 'Plan|Reconcil|Comparison|Matrix|Kind|...'` group ok (50 s); `TestReconcileMatrix_Golden` byte-identical (full, 40 s), `TestPlannerModel_RandomTriplesConverge`, `TestRunOnce_RandomizedBidirectionalEditsConverge`, `TestReverseDispatchTable_ListsEveryRunOnceScenario` pass.
- Race: planner group with -race passed in 1626 s (reviewer run, 2-core host under load)
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...` clean; gofumpt/goimports applied; lint: first run 4 issues (dupl x2 on the two row loops, goconst on a kind name that a test file also uses as content, testifylint require-error), fixed in review (generic queryStateRows helper, nolint with reason, require.ErrorContains); rerun inside the pre-pr gate

## Deviations (behaviour, scope or interface)
- The error carries all 26 decision-table input columns (the comparison CASE's 17 plus the reconciliation CASE's 3 item types and 6 flags), not 17: a reconciliation-stage gap is explained only by the latter. Inputs are read for the first unclassified row only (`WHERE path = ?`); the error counts the rest (`Others`).
- Detection has one owner, the planner pre-pass. The engine's load does not fail; it attaches the inputs to the row (new field `sQLiteReconciliationRow.DecisionInputs`), so one-shot and watch both read `sync: planning actions: ...` (load errors are wrapped differently per entry point today).
- New test-only engine field `reconciliationQueryOverride`; no reachable state is unclassified, so the engine tests remove one rule.
- The diverged equal-content cell is pinned as `upload`, not changed (see recommendation 1).

## Recommendations
1. Make (diverged, local changed, remote unchanged, equal content) a `baseline_update`: today it uploads bytes OneDrive already holds. Product decision; small SQL change plus this test.
2. `actionType.Direction()` and the `direction` type have no production caller: delete them.
3. Unify load-error wrapping: one-shot returns load errors bare, watch bootstrap prefixes `reading sync state:`, steady state `sync: reading sync state:`; and a comparison query error is wrapped twice (`sync: querying comparison state:` in two layers).
4. The remaining unreachable `default:` branches the analysis lists (`writesLocally`, `planner_dependencies.go` 287/519, `createSide`) still return a value instead of refusing; give them the same treatment. (Watch presentation checked: `reportWatchRunnerExit` warns once per distinct error, "Stopped syncing <drive>; retrying in <delay>", with the error value attached.)

## Unfinished
Nothing. Reviewer: full `internal/sync` suite (the new RunOnce scenario also runs under reverse dispatch). No other package is affected.
