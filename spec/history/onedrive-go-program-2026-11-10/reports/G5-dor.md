# G5 definition of ready (built in R3b, branch refactor/observation-leftovers, base 2e51cb22)

(i) Each item of the facts file, on 2e51cb22: all still present and dead in product code.
- `events chan<- changeEvent`: production passes nil (engine_watch.go `localObs.Watch(ctx, tree, nil)`, `runPeriodicFullScan(..., nil, ...)`); read only by `TrySendChangeEventOnly` and the two forwarding loops (observer_local_handlers.go `runSafetyScan`, engine_watch.go `runFallbackScan`).
- `TrySendChangeEventOnly` returns at once on a nil channel; `TrySend` is live (builds the batch).
- `scanResult.Events`: product readers are the two loops and two log counts (scanner.go FullScan Info line, handlers safety-scan Info line).
- Create/modify classification: scanner.go hashPhase (`changeType`), walk folder/cached-file events, handlers' `Type:` literals. Only product reader of a local `Type`: `localObservationBatchForEvent` (create and modify share one arm) and the drop log in `TrySendChangeEventOnly`.
- `detectDeletions` with `shouldSuppressDeleteForExcludedPath` and `observedKindFromItemType` (only caller is that helper): output reached events (then only the dead loops), a Debug log and `recordActivity`.
- Move arm in `localObservationBatchForEvent`: no local producer of `ChangeMove`; only caller is `TrySend`.
- Local `LastActivity`/`lastActivityNano`/`recordActivity`: no product or test reader of `LastActivity`.
- Delete type guess in `HandleDelete`: since R3's always-subtree delete it only gates `forgetWatchedSubtree`.
(ii) Once the loops, the deletion walk and `recordActivity` are gone, `scanResult.Events` is read by `detectCaseCollisions` (Path, and ItemType in `suppressDirectoryChildren`) and by tests. A scan event keeps Path and ItemType; a handler event keeps what `localStateRowFromEvent` reads plus `IsDeleted`.
(iii) The remote observer reads `changeCreate`, `ChangeModify`, `ChangeDelete`, `ChangeMove` (item_converter.go, baseline_orphans.go, core_types.go): none is deleted.
(iv) The delete handler decides watch cleanup from the watch set: `watchedDirs.has(fsPath)` drops the path's subtree (`forgetWatchedSubtree`), otherwise the watches below it (`forgetWatchedDescendants`, which also covers a directory whose own Add failed). Test: `TestHandleDelete_UnknownDirectoryDropsTheWatchesBelowIt`.
(v) Test sites on base: 72 event-channel lines in 18 files; about 90 `.Events` reads (observer_local_test 79, scanner_test 4, observer_edge_test 4, 1 each in delete, rescan_replan, hash_cache, name_length); 84 local create/modify/delete type sites. Helpers replaced: `startLocalWatch`/`startMockWatch`/`startMockWatchInLoop` lose the channel; `waitForLocalEvent` -> `waitForLocalPatch`/`waitForLocalRow`; `waitForLocalCreateEvent` -> `waitForLocalRow`; `drainChangeEvents` -> `drainLocalPatches`/`drainPatchRows`; `requireNoChangeEvent` -> `requireNoLocalPatch`; new `localBatches`, `nextLocalPatch`, `nextLocalRow`, `waitForLocalRemoval`, `waitForLocalSnapshot`, `patchRow`, `patchRemoves`, `patchRowPaths`, `findRow`.
(vi) Merged: `TestLocalObservationBatchForEvent_DirectoryDeleteRemovesPrefix` into `..._FileDeleteRemovesSubtree` (a removal carries no kind, so the two are one test). Renamed: `TestFullScan_DeletedFile` -> `TestFullScan_DeletedSyncedFileHasNoRow`; `TestHandleDelete_EmitsDeleteEvent` -> `TestHandleDelete_SendsTheRemovalOfThePath`.
(vii) Only `TestLocalObservationBatchForEvent_DirectoryDeleteRemovesPrefix` is named by a spec (sync-observation.md test table); a Historical test names row is added.

Design choice for (c): the batch builder reads `IsDeleted` (exists or gone) and nothing else of a local event's kind; it is the existing field, so no new type or flag.
