# W16 report: a state store has its own lock

Branch `fix/state-store-lock`, one commit on `af259a57` (hash in the final message).

## What changed
- `internal/sync/engine_locks.go` (renamed from `engine_root_lock.go`): `ErrSyncStoreInUse`; store lock `locks/store-<sha256 of the DB path, existing folders' symlinks resolved>.lock`; `acquireEngineLocks` (root, then store; a refused store lock releases the root lock); `engineLocks.release` (store, then root); `LockDriveForOfflineChange` = folder at the path (optional) + store lock, recorded-folder lock gone; `MoveStateDBs` (locks every source and target first, all or nothing); one `lockDataDirFile` for both locks.
- `engine.go`, `api_types.go`: the engine holds both locks until the store has closed; seam `lockFlock`.
- `store_reset.go`: deletes and moves by `config.StateDBFamily` (`-wal`, `-shm`, `-journal`, database last; owned by config since the second pass), `moveStateDBFamily`; `store_compatibility.go`: `stateDBFamilyExists` returns `(bool, error)`; `store_inspect.go`: `readRecordedSyncRoot` removed.
- `internal/cli/drive_cleanup.go`, `drive_remove.go`, `purge.go`, `auth_logout.go`, `auth_flow.go`: purge order is section removed, watch told, then `deleteStateDB` under the locks; a refused store is kept and the command fails naming the rerun (`drive remove --purge --drive <id>`, `logout --purge --account <email>`).
- `drive_reset_sync_state.go`, `drive_rebind_root.go`: `changeDriveStateUnderLock` (refusal phrased by the caller), `isSyncInUse`.
- `internal/cli/email_reconcile.go`, `internal/config/email_reconcile.go`: state DBs move first, through an injected `StateDBMover` (`syncengine.MoveStateDBs`); in use: nothing renamed.
- `internal/multisync/shortcut_child_artifacts.go`: child cleanup under the child's store lock, nothing touched when refused.
- Specs: `sync-engine.md` (Sync root and store ownership, with the table of every offline writer), `sync-store.md`, `sync-control-plane.md`, `cli.md` ([planned] gone), `drive-identity.md`, `config.md`, R-2.9.5, R-6.3.9, R-3.1.4, R-3.3.8, R-3.7.2.

## Every path that opens, deletes, resets or replaces a state DB family
Engine open (`newEngine`): root + store lock. Reset, rebind: folder + store (`changeDriveStateUnderLock`). `drive remove --purge` configured: folder + store, after section and notify; orphaned: store. `logout --purge` drives: folder + store, after sections and notify, only drives whose section was removed; orphans by email: store. Multisync child cleanup: store only (a replacement child may own the folder). Email rename: every source and target store, before any other rename. Not locked by design: read-only inspectors (`status`, `SyncedFileCount`, shortcut status reads; `mode=ro`, `query_only`), the dry-run scratch store (a temp DB), `internal/devtool` bench (opens its own run's store after that run exited).

## Tests (each seen red: logs `w16-red-*.log` in the scratchpad)
- `TestEngine_SecondEngineOnSameStoreRefused`: two folders, one DB: refused, folder lock released.
- `TestLockDriveForOfflineChange_ContendsWithAnEngineWhoseFolderMoved`: found with nothing recorded.
- `TestLockDriveForOfflineChange_WithoutAFolderTakesTheStoreLock`, `TestSyncStoreLock_NamesOneFilePerStore`.
- `TestEngineLocks_ReleasedInReverseOnlyAfterTheStoreCloses` (extended: store then folder, both after close), `TestEngineLocks_ReleasedWhenStartupFailsAfterTakingThem`, `TestEngineLocks_UnsupportedFilesystemWarnsAndContinues`.
- `TestMoveStateDBs_MovesTheWholeFamily`, `_RefusesEveryMoveWhileOneStoreIsInUse`, `_RefusesATargetThatExists`, `_ASourceThatCannotBeCheckedIsAnError`; `TestRemoveStateDBFiles_RemovesALeftoverJournal`.
- `TestDriveRemovePurge_KeepsTheStateWhileAnEngineHoldsIt`, `TestLogoutPurge_KeepsTheStateWhileAnEngineHoldsIt` (kept, message, rerun finishes).
- `TestDriveRemovePurge_NotifiesTheWatchBeforeDeleting`, `TestLogoutPurge_NotifiesTheWatchBeforeDeleting` (fake watch retires the runner on reload).
- `TestOfflineStateChanges_RefuseWhileAnEngineHoldsTheDrive` (reset and rebind x folder and store).
- `TestShortcutChildArtifactCleanupExecutor_KeepsTheStateWhileAnEngineHoldsIt`, `TestReconcileAccountEmail_AStateDBInUseChangesNothing`, `TestReconcileGraphUser_RefusesWhileASyncUsesTheState`, `TestDeleteStateDB_*`, `TestDriveRemove_NamesTheFolderTheDriveSynced`.

## Checks
`go build ./...`, `go vet ./...`, tagged e2e vet: pass. `go test -count=1 -timeout 60m` over `internal/sync/...` (422 s), `cli`, `multisync`, `config`: pass (includes the list/table/governs tests and `TestDetachedContexts_AlwaysCarryADeadline`). `-race -run` for new and changed tests in all four: pass. golangci-lint over the four: 0 issues (`W16-lint.log`). Cited-test existence over `spec/design`: none missing.
Second pass: build, vet; full `config`, `cli`, `multisync` suites; `-run` of the touched `sync` and `devtool` tests; `-race -run` of the new tests; lint over the five packages: 0 issues. Red seen for the three new tests (`w16-p2-red-*.log`). The last lint run overlapped another agent's devtool lint by my mistake (I started it before reading the `pgrep` result); both finished.

## Deviations
- Email rename covered (not in the entry): it renamed a store under a live engine, and only the database file.
- A refused purge names the rerun of the same purge, not `drive reset-sync-state`, which needs the drive in the config.
- Plain `logout` now tells a running watch too; `logout --purge` runs every step, still removes the account from the catalog, and purges state only of drives whose section was removed.
- The family gained `-journal` and a sidecars-first order everywhere.

## Also fixed
- `stateDBFamilyExists` read a stat error as absent; `RebindSyncRoot` dropped a close error when the change failed.
- `purgeOrphanedFiles` removed the database but left `-wal`/`-shm`; purge left `-journal`.
- `removeAccountDriveConfigs` logged and returned each failure; now returns the removed drives.
- "Purged %d orphaned data file(s)" for a 0-or-1 count; duplicate `TestRemoveDriveDataFiles_*` tests.
- Plain `drive remove` printed an empty sync folder for a drive on its default folder.
- `ensureNoLiveSyncOwner` dead nil-client check (states now explicit); `joinAccountViewEmails`; `canonicalIDForToken` documented zero; multisync dead `ctx != nil` check and shadowed errors; `Engine.Close` doc.
- (second pass) `CatalogDrive.RetainedStatePresent` deleted: only ever set false, never read, never on disk (`omitempty`); the prune of a primary drive no longer rewrites an unchanged record; tests keep what they proved otherwise.
- (second pass) `DiscoverStateDBsForEmail` finds a store by any file of its family, once, by its database path, so `logout --purge` deletes a lone `-wal` (`TestDiscoverStateDBsForEmailIn_FindsAStoreByAnyFileOfItsFamily`, `TestLogoutPurge_RemovesALoneSidecarOfTheAccount`). One owner of the family: `config.StateDBFamily` (moved from `internal/sync`; sync, multisync and the CLI use it). The email rename now moves only a store whose database exists; sidecars alone stay put, since moved they would be replayed into the new name's later database (`TestMoveStateDBs_LeavesAStoreWithoutItsDatabase`). Discovery is state-specific now (`discoverStateDBsForEmailWithIO`; the generic prefix/suffix helper had no other user).
- (second pass) `internal/devtool/bench_live_bidirectional.go` takes the state path from the new `config.DriveStatePathForDataDir` (the pair of `MountStatePathForDataDir`; `DriveStatePath` calls it) and parses the drive ID first; `MountStatePathForDataDir` did not fit, it names child mounts.

## Review (coordinator, 2026-11-11, commit 83a1028d)
- The claim "a stale `-wal` is replayed into the database created at that path" was checked against the product's SQLite driver and is false: a log left alone is deleted when a database is created at that name; only a log beside an existing database is replayed. Pinned by `TestStateDBFamily_SQLitePairsALogWithADatabaseByNameAlone` (three subtests), and the reason was reworded in `config/drive.go`, `drive_test.go`, `cli/purge_test.go`, `config.md`, `sync-store.md` and the commit message.
- `MoveStateDBs` now classifies under the locks and deletes sidecars left without a database (`removeStateDBLeftovers`): `TestMoveStateDBs_RemovesTheLeftoversOfAStoreWithoutItsDatabase` replaces `_LeavesAStoreWithoutItsDatabase`; `TestMoveStateDBs_KeepsLeftoversWhileTheirStoreIsLocked` is new.
- `config.md` now states what a rerun does after an email change cut short between two renames of one store (the store falls back to its last checkpoint).

## Not fixed
- `internal/config/email_reconcile.go` `apply`: a failure after the state DBs moved (token rename, section rewrite, catalog) leaves the account half renamed, and `moveStateDBFamily` cut short after a sidecar moved loses that log on the rerun; needs a journal and a checkpoint before the move, a design change of its own. Planned: an account email change survives an interruption (W17).
- `NewSyncStore` beside lone sidecars: checked in review, not a defect (SQLite deletes the lone log); pinned by the test above.
- Leftover sidecars under an account's old email after a rename: fixed in review (deleted by the rename under the store lock).

## Unfinished
None. Live check for the reviewer, on the test account under `~/sync-test-root` with a binary built from this branch: (1) start `sync --watch` on the test drive, then `drive reset-sync-state --drive <test drive> --yes` must refuse; (2) add a throwaway second drive section, let the watch start its runner, run `drive remove --purge --drive <throwaway>`: expect "picked up the change", then "Purged config and state", the runner retired in the watch log, and no `state_*` family for it in the data dir.

## Packages the reviewer should test
`internal/devtool` (repository checks), e2e (purge, logout, reset, rebind flows).
