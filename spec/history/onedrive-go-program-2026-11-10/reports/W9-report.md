# W9 report: unlisted action types refuse, equal content never transfers, dead enum code goes

Worktree `~/src/onedrive-go-wt/planner-leftovers`, branch `fix/planner-leftovers`, commit `eb442d96` on `27e76cc3` (the first pass's `c970583d`, amended), not pushed.

## What changed
- `enums.go`: `direction`, `Direction()`, `changeSource` with its two constants and its `String()` deleted; `actionType.isMember()`, `folderCreateSide.isMember()` and `outsideTheEnum` added (it describes the value without a package prefix, since it also sits inside errors that carry one); `Value()` refuses through `isMember`.
- `planner_kinds.go`: `errUnlistedActionPlanned` and `validatePlannedActions` (type outside the enum, or a folder create side outside its enum, names the path).
- `planner_actions.go`: `PlanCurrentState` builds, then `planBuiltActions` validates before the composition drop, normalization, admission and ordering; `countRenames` a switch; `writesLocally` panics; two "create on side" tests use `createsOnSide`; local `moveInputs` (shadowed its type) renamed.
- `planner.go`: `actionAllowedInMode` (type and side), `preserveParentFolders...`, `deletedItemType` panic; `actionRequiresParentFolder` (a `!=` chain, true for any unlisted value) replaced by `!isDeleteAction`; `isFolderDelete` reuses `isDeleteAction`; stale comments fixed.
- `planner_dependencies.go`: `indexByPath`, `addRemoteCaseVariantDeps`, `vacatesCaseVariantOf`, `underMoveSource` tagged switches; `isDeleteAction`, `isMoveAction` exhaustive switches; `indexVacatedNames`, `addMoveOutOfDeletedFolderDeps`, `createsAtVacatedSource` panic; create tests use `createSide`.
- `planner_move_composition.go`: `pathsVacatedByPlan`, `createSide` (type and side) panic; `dropReconciliationOfComposedMoves` a switch; `createsOnSide` added.
- `domain_delete_safety.go`: `deleteSafetySideFor` (an `if`: "remote" for every type but a local delete) replaced by `deleteSafetySideOf`, one exhaustive switch answering (side, is a delete) that panics in its default. `planner_delete_safety.go` (the hold, the confirmation matching, whose `!=` pair it also replaces) and `store_delete_ledger.go` use it; the store checks `isMember` first and keeps its error. `deleteSafetyThreshold`'s parameter and comment said files; they are items (files and folders).
- `actions.go`, `action_freshness.go`, `engine_activity.go`, `engine_result_classify.go`, `scope_semantics.go`, `worker_result.go`: each default panics with `outsideTheEnum`.
- `engine_runtime_completion.go` -> `actions.go`: `isPublicationOnlyActionType` moved to the domain layer, its panic now `outsideTheEnum`.
- `store_write_baseline.go`: `classifyBaselineMutation` and `updateRemoteStateOnOutcome` refuse with an error built with `outsideTheEnum`; `publicationMutationFromAction` refuses through `isPublicationOnlyActionType`. `worker.go`: dispatch refuses with `errUnknownActionType` wrapping `outsideTheEnum`.
- `core_types.go`, `observed_items.go`, `scanner.go`, `observer_local_handlers.go`, `item_converter.go`, `baseline_orphans.go`: `changeEvent.Source` and its 13 writes deleted, and the check in `projectRemoteObservations` that skipped non-remote events; the event comment says where an event's origin is known instead.
- `sqlite_compare.go`: `equal_again` is three sides present, a change, `current_equal = 1`, whichever side changed; comments of both CASEs updated.
- Tests: `core_types_test.go`, `planner_kinds_test.go`, `sqlite_compare_test.go`, `engine_transfer_integrity_test.go`, `dispatch_order_test.go`, `planner_actions_test.go`, `baseline_test.go`, and the `Source` fields and assertions in `engine_refresh_test.go`, `engine_observation_postprocess_test.go`, `local_observation_batch_test.go`, `engine_delta_expiry_test.go`, `observer_local_trysend_test.go`, `observer_local_test.go`, `observer_local_create_test.go`, `observer_local_delete_test.go`, `observer_local_write_test.go`, `observer_local_handlers_test.go`, `observer_remote_test.go`.
- Docs: `sync-planning.md` (Error Boundary, Verified By, Decision Table Kinds), `sync-domain.md` (actionType paragraph, the test that holds the defaults, `deleteSafetySideOf`, the event source, Verified By row), `requirements/sync.md` R-2.2.3, `requirements/non-functional.md` R-6.2.16.

## Tests
- `TestPlanCurrentState_UnlistedActionTypeFailsThePlan`: an action outside the enum, built by reconciliation or by composition, and a folder create side outside its enum fail the plan naming the path (was: planned, deferred, uncounted).
- `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum`: 27 decisions panic with `outsideTheEnum` (was: a quiet verdict each), `deleteSafetySideOf` among them, plus the side switch.
- `TestActionTypeRefusals_ATypeOutsideTheEnumIsAnError`: `Value`, classification, publication mutation, delete ledger (was nil), remote mirror (was nil), worker dispatch; classification and dispatch now carry the `outsideTheEnum` text.
- `TestActionTypeSwitches_EveryDefaultRefuses` (new): reads every non-test file of `internal/sync` with `go/parser` and the member lists of `actionType` and `folderCreateSide` from the const blocks of `enums.go`. Every switch whose case clauses name such a constant, tagged or not, must have a default that ends in a panic or a return and calls `outsideTheEnum` (the panic value, or inside the returned error). Exceptions: `actionType.String`, `folderCreateSide.String` (they name the value for refusals and logs), `actionType.isMember`, `folderCreateSide.isMember` (false is the answer); an exception matching no switch fails. Proved able to fail: with `writesLocally`'s default changed to `return false` and `pathsVacatedByPlan`'s default removed, it failed naming both (`the default answers instead of refusing with outsideTheEnum`, `no default`; `W9-class-red.log`); both edits reverted, the two files equal the commit again.
- `TestQueryReconciliationState_LocalEditEqualToUnchangedRemoteIsABaselineUpdate` (renamed from `...IsAnUpload`, flipped).
- `TestRunOnce_LocalEditEqualToUnchangedRemoteUploadsNothing`: accepted transform, local file rewritten to the reported content: no upload (was 1); the baseline holds the local_state hash, size, mtime, ctime, birth time, device, inode beside unchanged item ID, remote hash, size, mtime, eTag; the next plan has no action at all; dry run quiet. Listed in `runOnceScenarios()`.
- `TestComparisonKindCase_ClassifiesEveryReachableState`: the invariant is now "diverged never has equal content" (red before the SQL change).
- `TestItemType_StoredNamesRoundTrip`, `TestEnumStrings_AreTheStoredAndLoggedNames` replace five tests that asserted only non-empty names.
- `TestCommitMutation_UnknownAction_ReturnsErrorAndSkipsDBWrite`: asserts the `outsideTheEnum` text.
- Removed: `TestChangeEventsToObservedItems_RemoteOnly` (`engine_refresh_test.go`), which asserted only that the projection skipped a local event; 12 `Source` assertions in tests that assert more and stay.

## The change event's source: why it could go
`projectRemoteObservations` is reached in the product only through `buildRemoteObservationBatch` (`engine_primary_root.go`), whose callers pass:
- `buildPrimaryWatchBatch` (watch, `engine_primary_root_watch.go:74`): `polledEvents` from `remoteObserver.Watch` -> `fullDelta` -> `classifyWindow` -> `itemConverter.ClassifyItem`.
- `executeDriveRootObservation`: `nil` (read denied), or `observeRemoteWithShortcutTopology` -> `remoteObserver.FullDeltaWithShortcutTopology` -> `ClassifyItem`.
- `executeDriveRootFullObservation`: `nil`, or `observeRemoteFullWithShortcutTopology`: `FullEnumerationWithShortcutTopology` events plus orphans from `findBaselineOrphans` and `findRemoteStateOrphans` (`baseline_orphans.go`), filtered by `confirmEnumerationOrphans`.
- `executeMountRootObservation` (one-shot and `watchMountRootRemote`): `nil`, or `observeMountRootRemote` -> `mountRootEvents`: `convertMountRootItems` -> `itemConverter.ConvertItems` -> `ClassifyItem`, plus `detectMountRootOrphans` -> `findBaselineOrphans`, filtered.

Every product construction of a `changeEvent` is one of: `itemConverter.classifyAndConvert` (the only builder behind `ClassifyItem`), the two orphan builders in `baseline_orphans.go`, `Baseline.FindOrphans`, all of which set the remote source; or the scanner and local observer handlers (9 sites), which set the local source and feed only the local paths (event channel, scan results, `localObservationBatch`). No local event reaches `buildRemoteObservationBatch`, so the check never fired and the field was write-only.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass on the final tree.
- Red runs of the first pass: each new or changed test failed for its reason (`W9-red.log`); the class test's failure probe is above.
- Non-race, final tree: `-run 'TestPlan|TestQueryReconciliation|TestReconcileMatrix|TestQueryComparison|TestExecutor|TestRunOnce|TestObserver|TestScanner|TestDeleteSafety|TestActionType'` plus the tests of the files touched (full scan, local and remote observers, watch, delta, item classification, observation batches, orphans, refresh, commit mutation, baseline, enum names) and `ListsEvery|Table|Governs|Exhaustive|Registered`: ok (273 s of tests).
- `-race` on the added and changed tests (the three action-type tests, the commit-mutation refusal, and every test whose `Source` field or assertion went: full scan, the watch and handler tests, `TestFullDelta_NewFiles`, orphans, projection, observation batches, try-send, remote batch handling): ok.
- Lint `./internal/sync/...` into `W9-lint.log`, once, last: `0 issues.`
- Golden matrix: no cell changed (its baseline records the same content on both sides, so no cell is in the bound). Live proof: none, per the entry.

## Deviations
- The test hands unlisted actions to `planBuiltActions`, a new unexported stage of `PlanCurrentState`: no row can make one. Name kept as the entry gives it.
- The folder create side is validated too, the same class (two switches decide on it).
- Tagless switches and the `==` predicates `isDeleteAction`/`isMoveAction` became exhaustive switches.
- Store and dispatch defaults return errors instead of panicking (they already return errors); all of them now build the error with `outsideTheEnum`, so one syntactic rule holds every default.
- `underMoveSource` answers false for a `move` that is not a move (only moves reach it).
- R-2.2.3 and R-6.2.16 extended; the entry named only the design docs.

## Also fixed
- `planner.go`: "pure decision engine" (it logs), `ActionPlan`/`Planner` names, `countByType` "Exported"; duplicated delete set in `isFolderDelete` and `actionRequiresParentFolder`.
- "Creates on this side" was written out five times; all go through `createSide` now.
- `PlanCurrentState`'s local `moveInputs` shadowed the type of that name.
- `publicationMutationFromAction` restated the publication-only set; it asks `isPublicationOnlyActionType`, moved to `actions.go` so the store layer may call it.
- `core_types_test.go`: five enum tests asserted only non-empty names and used positional struct literals.
- The change event's source (was "Not fixed" 1): written at 13 sites, read by one check no event could trip; deleted with `changeSource` (proof above).
- The delete-safety side (was "Not fixed" 4): one exhaustive switch, `deleteSafetySideOf`, for the planner and the store.
- `deleteSafetyThreshold` named its argument `baselineFiles` and said "file deletes" while counting files and folders.
- The first pass's errors read "sync: ...: sync: ActionType(10) is outside its enum"; `outsideTheEnum` no longer carries the prefix.

## Not fixed (seen, file:line, what, fix)
1. `internal/sync/sqlite_compare_kinds.go:90`, `:192`: `allComparisonKinds`, `allReconciliationKinds` are test-only. Handled outside this commit.
2. Test-only product functions (deadcode from `main`): `active_scopes.go:117` `extendScopeTrial`, `:139` `dueTrials`, `:156` `earliestTrialAt`, `:174` `scopeKeys`; `engine_run_once.go:511` `observeRemoteFull`; `engine_runtime_lifecycle.go:530` `recordBlockedRetryWork`; `engine_watch_state.go:517` `kickDueHeldRelease`; `executor_conflict.go:186` `conflictCopyPath`; `observer_local.go:415` `watchRegistry.paths`; `scanner.go:1338` `validateOneDriveName`; `shortcut_root_publication.go:188` `shortcutChildRunModeForRoot`; `store.go:208` `cachedBaseline`; `store_inspect.go:175` `readPathTruthStatus`. Handled outside this commit.

## Unfinished
None.

## Packages the reviewer should test
Only `internal/sync` changed; run the whole package (the exhaustive model and decision-table stages included).

## Appendix A: every switch on the action type in `internal/sync` product code

Held by, for every row: the `exhaustive` linter holds the member list (`default-signifies-exhaustive: false`, no `//nolint:exhaustive` remains), and `TestActionTypeSwitches_EveryDefaultRefuses` holds the default, except where the last column names an exception. The last column also names the behaviour test that calls the decision with a value outside the enum: `Panic` is `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `Refusal` is `TestActionTypeRefusals_ATypeOutsideTheEnumIsAnError`.

| File | Function | Default before | Default after | Behaviour test |
| --- | --- | --- | --- | --- |
| action_freshness.go | evaluateMoveEndpointFreshness | fall-through: fresh | panic | Panic |
| action_freshness.go | movePeerPath | fall-through: no peer | panic | Panic |
| actions.go | DeferredCounts.AddAction | fall-through: not counted | panic | Panic |
| actions.go (was engine_runtime_completion.go) | isPublicationOnlyActionType | panic after the switch | panic | Panic |
| domain_delete_safety.go | deleteSafetySideOf (was deleteSafetySideFor, an `if`) | remote | panic | Panic |
| engine_activity.go | activityKindFor | fall-through: no activity | panic | Panic |
| engine_result_classify.go | effectiveRemotePermissionCapability | unknown capability | panic | Panic |
| engine_result_classify.go | effectiveLocalPermissionCapability | unknown capability | panic | Panic |
| enums.go | actionType.String | `ActionType(n)` | unchanged | exception: names the value |
| enums.go | actionType.Direction | `""` | deleted | - |
| enums.go | actionType.Value | error | error, through `isMember` (no longer a switch) | Refusal |
| enums.go | actionType.isMember (new) | - | false | exception: the membership answer |
| enums.go | folderCreateSide.String | `FolderCreateSide(n)` | unchanged | exception: names the value |
| enums.go | folderCreateSide.isMember (new) | - | false | exception: the membership answer |
| planner.go | actionAllowedInMode (type; side) | not admitted; not admitted | panic; panic | Panic |
| planner.go | preserveParentFoldersForRunnableDescendants | fall-through: kept | panic | (behind `isDeleteAction`, which panics first) |
| planner.go | deletedItemType | fall-through: view's type | panic | Panic |
| planner.go | actionRequiresParentFolder (`!=` chain) | true | deleted: `!isDeleteAction` | - |
| planner_actions.go | writesLocally | false | panic | Panic |
| planner_actions.go | countRenames (was tagless) | not counted | panic | Panic |
| planner_delete_safety.go | approvedDeleteIndexes (`!=` pair, not a switch) | - | `deleteSafetySideOf` | - |
| planner_dependencies.go | indexByPath (was tagless) | not indexed | panic | Panic |
| planner_dependencies.go | indexVacatedNames | fall-through: vacates nothing | panic | Panic |
| planner_dependencies.go | addMoveOutOfDeletedFolderDeps | no edge | panic | Panic |
| planner_dependencies.go | addRemoteCaseVariantDeps (was tagless) | no edge | panic | Panic |
| planner_dependencies.go | vacatesCaseVariantOf (was tagless) | false | panic | Panic |
| planner_dependencies.go | underMoveSource (was tagless on `a`, now on `move`) | remote delete or cleanup test | panic | Panic |
| planner_dependencies.go | createsAtVacatedSource | false | panic | Panic |
| planner_dependencies.go | isDeleteAction, isMoveAction (were `==`) | false | panic | Panic |
| planner_move_composition.go | pathsVacatedByPlan | fall-through: vacates nothing | panic | Panic |
| planner_move_composition.go | createSide (type; side) | creates nothing; remote | panic; panic | Panic |
| planner_move_composition.go | dropReconciliationOfComposedMoves (was tagless) | kept | panic | Panic |
| scope_semantics.go | localWriteScopeBlocksAction | fall-through: not blocked | panic | Panic |
| scope_semantics.go | remoteWriteScopeBlocksAction | fall-through: not blocked | panic | Panic |
| store_delete_ledger.go | recordDeleteLedgerTx (had `//nolint:exhaustive`, two members) | nil: not ledgered | no switch: `isMember`, then `deleteSafetySideOf`; error | Refusal |
| store_write_baseline.go | classifyBaselineMutation | error | error with `outsideTheEnum` | Refusal |
| store_write_baseline.go | updateRemoteStateOnOutcome | fall-through: nil | error with `outsideTheEnum` | Refusal |
| worker.go | workerPool.dispatchAction | `errUnknownActionType` outcome | the same, wrapping `outsideTheEnum` | Refusal |
| worker_result.go | outcomeDisplaySize | fall-through: no size | panic | Panic |

37 switches now in the class test's sense: 33 on the action type and 4 on the folder create side (the two nested in `actionAllowedInMode` and `createSide`, `folderCreateSide.String`, `folderCreateSide.isMember`). The remaining single-member tests in the touched planner files (`a.Type == ActionConflictCopy` and the like) name exactly one member and are correct for every other.

## Appendix B: consumers of `equal_again` and `diverged`
- SQL comparison CASE: the widened rule; `diverged` now means content differs.
- Reconciliation, `equal_again -> baseline_update`: right for the new rows, since nothing transfers and `publicationMutationFromAction` records the view's local and remote facts, which the RunOnce test checks.
- Reconciliation, diverged with a type mismatch (`conflict_keep_both`): `current_equal` is 0 when types differ; unaffected.
- Diverged, both types changed to one type (`conflict_create_create`): both changed means `remote_changed = 1`, already `equal_again` when equal.
- Diverged, local change with a type change (`replace_remote`): equal content and an unchanged remote mean all three types agree, so no real row moves.
- Diverged, remote type change (`replace_local`), remote-only change (`download`), both changed (`conflict_edit_edit`): each has `remote_changed = 1`, already `equal_again` when equal.
- Diverged, local-only change (`upload`): the decided cell; it now plans only bytes OneDrive does not hold.
- Go: `comparisonEqualAgain`, `comparisonDiverged` (`sqlite_compare_kinds.go`) are names only; no Go code branches on either. `buildActionsForReconciliation` maps `reconcileBaselineUpdate` as before.
- Tests: decision-table stage 1 (invariant tightened), stage 2 (unchanged, passes), the flipped pinning test, the golden matrix (unchanged).
- Spec: `sync-planning.md` Decision Table Kinds.

## Appendix C: golden cells changed
None.

## Appendix D: dead code removed
- `direction` and its three constants, `actionType.Direction()`; `TestActionTypeDirection`.
- `changeSource`, `sourceRemote`, `SourceLocal`, `changeSource.String()`, the `changeEvent.Source` field and its 13 writes (scanner 4, local observer handlers 5, item converter 1, orphan builders 3), the check in `projectRemoteObservations`; `TestChangeEventsToObservedItems_RemoteOnly`, 12 `Source` assertions, and the `Source` fields of test events.
- `actionRequiresParentFolder` and `deleteSafetySideFor` (duplicated knowledge, replaced by `isDeleteAction` and `deleteSafetySideOf`).
- No planner function is test-only (deadcode from `main` lists none in the planner files).

## Review (2026-11-12, first pass c970583d)

Read: this report, the whole product diff, the test diff, the spec word-diff, and `sqlite_compare.go` around both CASE expressions on the branch. No defect found.

- **The wider `equal_again` rule.** Three sides present, `current_equal = 1` and a change on one side can only be a file of one type on all three sides whose content now matches the other side (a type change makes `current_equal = 0` or sets `remote_changed`; folders never have `local_changed`). `local_move_source` and the type-change rules sit before it or are untouched. No Go code reads `equal_again` or `diverged`: the two CASE expressions and the kind registry are the only consumers.
- **Rewritten switches checked by hand for the same answers**: `underMoveSource`, `addSamePathReplacementDep`, `addRemoteCaseVariantDeps`, `vacatesCaseVariantOf`, `dropReconciliationOfComposedMoves`, `indexByPath`.
- **Deviations accepted**: the plan test calls `planBuiltActions` directly; the folder create side is validated too; tagless switches and `==` predicates became exhaustive switches; store and dispatch defaults return errors; `underMoveSource` answers false for a move that is neither kind; R-2.2.3 and R-6.2.16 extended.
- **No `//nolint:exhaustive` remains** in `internal/sync` (grep).
- **Correction to "Not fixed" entry 1**: the source field is also read by `projectRemoteObservations` (`observed_items.go:23`), which the product calls from `buildRemoteObservationBatch`. The field goes only once every caller is shown to pass remote events only.
- **Second pass (writer)**: entry 4 (one shared delete-safety side switch), entry 1 (with the proof above), and a source-reading test that every switch on the action type or the folder create side has a default that refuses.
- **Handled outside this commit**: entries 2 and 3, as a rider on the change that removes test-only product code across the tree.

## Review (2026-11-12, second pass eb442d96, final d4865523)

Read the whole second-pass diff against the first pass. No defect.

- **Done as asked**: `deleteSafetySideOf` is the one switch the hold, the confirmation matching and the delete ledger ask (the ledger checks `isMember` first and keeps its error); the change event's source is gone with the proof above (every caller of `buildRemoteObservationBatch` checked against the code); `TestActionTypeSwitches_EveryDefaultRefuses` holds the defaults, with four named exceptions and a failure on an unused one.
- **Limit of that test, accepted**: it walks the bodies of function declarations, so a switch inside a package-level function literal is not seen. None exists; the linter still holds member lists everywhere.
- **Mine, amended in (d4865523)**: the switch's parameter was named like its type; its comment and the comment of `errUnlistedActionPlanned` told history; `deleteSafetyMinFiles` is `deleteSafetyMinItems` (it bounds files and folders).
- **Checks on d4865523**: build, gofumpt, plan-id, backlog-id and principle-label checks clean; the whole non-race `internal/sync` suite: see the plan entry.
