# R3b report

Branch `refactor/observation-leftovers`, base R3 `2e51cb22`. Pushed; no PR, nothing installed. Worktree `~/src/onedrive-go-wt/observation-leftovers` (made with `git worktree add`: devtool's `worktree add` has no base flag; no `.env`/`.testdata` in it).

- `076eb75b` G5: Give the local observer one output, and drop what only fed the dead channel (32 files, about +800/-1080)
- `c2aad1e0` G7: Report remote items as their stored row, and changes as three facts (27 files, +397/-419)
- C3: **not built, stopped** (see below and `C3-dor.md`). No store generation change on this branch: generation stays 28.

DoR files: `G5-dor.md`, `G7-dor.md`, `C3-dor.md`.

## G5 (done)
- The local observer has one output, the batch channel. `Watch`, every handler, the safety scan and the fallback scan lose the event channel; `TrySendChangeEventOnly`, both forwarding loops, `detectDeletions`, `shouldSuppressDeleteForExcludedPath`, `observedKindFromItemType`, the builder's move arm, local `LastActivity`/`lastActivityNano`/`recordActivity` and the delete type guess are gone.
- Producers do not tell create from modify; the batch builder reads `IsDeleted` only. A scan's change list keeps Path and ItemType (the collision check's inputs). `scanResult.Events` stays (decision a).
- Watch cleanup on delete is decided from the watch set (`forgetWatchedSubtree` if watched, else `forgetWatchedDescendants`). This fixes a real gap: an unknown directory whose own Add failed kept the watches below it.
- Also fixed: write-only `hashJob.name`, `hashRequest.Name` and the `name`/`entryName` parameters it left unused (8 functions); stale comments (the five, plus Watch doc, `droppedEvents`, scanResult, FullScan phases, HashAndEmit, core_types contract); logs print rows not event counts; lint in touched files from R3 (testifylint x8, hugeParam: `TrySendLocalObservationBatch` takes a pointer, goconst).
- Tests: about 240 sites moved to the batch channel / scan rows, via new helpers in `watch_test_helpers_test.go`. New `TestHandleDelete_UnknownDirectoryDropsTheWatchesBelowIt` (failed on base: watches kept). Renamed `TestFullScan_DeletedFile` -> `TestFullScan_DeletedSyncedFileHasNoRow` (it and `TestFullScan_DeletedFolder` failed on base for the delete entries they now refuse); `TestHandleDelete_EmitsDeleteEvent` -> `TestHandleDelete_SendsTheRemovalOfThePath`. Merged `TestLocalObservationBatchForEvent_DirectoryDeleteRemovesPrefix` into `..._FileDeleteRemovesSubtree` (Historical test names row added). Three watch-create tests no longer assert content size/hash: the Create event can precede the write (racy; the old tests checked path only).
- Spec: sync-engine.md (the replan's full scan is local truth; a watch updates rows in scope and wakes the replan), sync-observation.md, R-2.8.8.

## G7 (done)
- `remoteItem{observedItem; OldPath}` is what the converter and orphan finders build; batches carry rows plus `[]remoteChange{Path, OldPath, ItemType}` (`changes`, same entries and order as `emitted`, no-ID items included). No remote-side file names `changeEvent` or a change type.
- Deleted: the dropped return of `observeAndCommitRemoteCurrentState`, remote `LastActivity` (+ its test), `observeRemoteFull`, `projectObservedItems`, and `Baseline.FindOrphans` (no caller at all). Write-only Type/CTag/Name gone.
- `baseline_orphans.go` moved from sync-store.md to sync-observation.md GOVERNS (layer 1 may not reference the layer-2 `remoteItem`). Retired-backlog row `B-125` removed (no longer cited).
- New tests `TestRemoteObservation_RowsAndChangesForEveryItemKind`, `TestRemoteBatch_AChangeWithoutAnItemIDStillCountsForTheCursor`: passed on the pre-G7 tree (old-API form), fail under a temporary change (no OldPath for moves; no change for a no-ID item), reverted (numstat checked). Renamed `TestRemoteEventHasPlannerVisibleEffect_TransitionMatrix` -> `TestRemoteChange...` (no spec cites). Type assertions now assert the fact (IsDeleted / OldPath / not deleted).
- Spec: R-6.7.28 wording, sync-observation.md two sentences plus the row/change paragraph.

## C3 (stopped)
1. Blocked rows under read-permission scopes are written without a `block_scopes` row on purpose (`engine_runtime_permissions.go`, both `permissionEvidenceBoundaryDenied` arms; `PersistsInBlockScopes` false for read scopes; `ReleaseOrphanBlockedRetryWork` releases them). The decided key refuses them: owner decision (brief stop condition v).
2. Scope row after blocked row on five paths across completion, trial fallback, disk-local reclassification and permission handling: one transaction means reworking the retry_work failure-persistence path, not only the scope ledger (caller's limit).

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- gofumpt clean. `RealRepoIsClean` (devtool): 4 PASS.
- Focused `internal/sync` runs (`-p 1`, TMPDIR on disk, `-skip TestPlannerExhaustive`): G5 set plus ListsEvery/Table/Governs/Exhaustive/Registered and engine tests, pass; G7 set, pass.
- Lint `./internal/sync/...` (`R3b-lint.log`): only 6 issues remain, all in files R3b does not touch, from R3 (R3's lint never ran): parked as R3b-4.
- Whole `./internal/sync/...` (`-p 1`, `-timeout 90m`, behind the lock, 24 min): 3 failures. `TestStoredTimes_EveryPlainUnixNanoIsDeclared` and `..._EveryDecodeIsDeclared` (the deleted LastActivity and event-mtime sites): declarations updated in each commit, both pass. `TestIsForeignMountPoint_CrossingRecordsTheBoundary`: fails on the R3 base as well (R3's mount-flag snapshot), parked R3b-5. Not rerun whole after the declaration fix.
- No `-race`, no verify, no e2e.

## PARKED
R3b-1 (dead changeEvent fields), R3b-2 (remote names still say events), R3b-3 (C3 blocker), R3b-4 (R3 lint), R3b-5 (R3 mount-flag test fails on base).


## Merged as #960

- Rebased onto main 899617bc (conflicts in `local_observation_batch.go` and `observer_local.go` with R3's lint fixes: main's constant name kept, the pointer argument kept; no behaviour change) and squashed into one commit. Review nit fixed: the stale section header in `engine_refresh_test.go`; the stale test names stay with R3b-2.
- Checks: build, both vets, gofumpt, whole `./internal/sync/...` with `-race` (`-p 1`, passed in 7145 s), pre-pr (lint 0 issues, so R3b-4 is resolved by R3's ship; quick tier), PR CI, pre-merge.
- Nightly: the start stage claimed four failures of the last nightly (crash recovery, planner exhaustive, reverse dispatch, watch shutdown timers); all four released, not fixed here.
- Merged 2026-11-20 03:46 PST as `9b16a5c0`; installed `9b16a5c0`. No live run (no file operation changes).
