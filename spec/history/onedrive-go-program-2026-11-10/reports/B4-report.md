# B4 report: config and catalog locks, bounded wait, catalog updates locked

Commit `5f308c5d` on `fix/config-lock-bounded` (base `90c60a8f`), 7 files.

## What changed
- `internal/config/mutation_lock.go` (new): `configLocker` (flock, sleep, timeout, poll interval), `defaultConfigLocker`, `withConfigMutationLock`, `withMutationLock`, `acquire` (non-blocking flock every 50 ms, `ErrConfigLocked` after 10 s naming the directory), `setDriveKey`, `updateCatalog` (load/update/save under the lock on the catalog's directory).
- `internal/config/write.go`: old blocking lock removed; `SetDriveKey` calls the locker's `setDriveKey`.
- `internal/config/catalog.go`: `UpdateCatalogForDataDir` delegates to the locked `updateCatalog`.
- `internal/config/mutation_lock_test.go` (new): the four tests below plus `holdDirectoryLock`.
- `spec/requirements/configuration.md`: R-4.4.4.
- `spec/design/config.md`: GOVERNS, runtime owner, Verified By row, pause housekeeping, Catalog JSON Policy, lock text in Auto-Creation (bounded wait, error, no nesting rule).
- `docs/troubleshooting.md`: a section for the new error.

## Tests (all `// Validates: R-4.4.4`, each seen failing first)
- `TestWithConfigMutationLock_HeldLockTimesOutWithTheDirectoryNamed`: fake flock always contended; `ErrConfigLocked`, directory and "10s" in the text, callback never ran, 200 waits of 50 ms, never a blocking request. Before: "blocking flock requested".
- `TestWithConfigMutationLock_AcquiresWhenTheHolderReleases`: real holder released on the second wait; callback runs once after two waits, lock free afterwards. Before: hung.
- `TestSetDriveKey_CrossProcessHolderIsNotWaitedForForever`: real holder; SetDriveKey's path fails with `ErrConfigLocked` after the full wait, file unchanged. Before: hung.
- `TestUpdateCatalog_CrossProcessUpdatesAreNotLost`: holder reads the catalog, the update starts and waits, holder writes and releases; both accounts survive. Before: the waiting update was lost.
- "Cross-process" is a second open file description of the directory in the test process (said in the helper's comment). No sleeps; fakes replace `sleep`.

## Nesting trace (flock per open file description)
- Lock takers: AppendDriveSection, SetDriveKey, setDriveStringKeyIfMissing, DeleteDriveKey, DeleteDriveKeyIfDriveEquals, DeleteDriveSection, DeleteDriveSectionIfDriveEquals, RenameDriveSections, and UpdateCatalog/UpdateCatalogForDataDir.
- Config callbacks are the `*Snapshot` functions in write.go: read file, parse, `driveFromConfigSnapshot` (decode + Validate, no writes), atomic write. No lock inside.
- Catalog writers and their callbacks (all in-memory catalog edits): RecordLogin, RegisterDrive, RegisterSharedDrive, ApplyPurgeLogout, PruneDriveAfterPurge, MarkAccountAuthRequired, ClearAccountAuthRequirement, reconcileCatalogEmail (config); restoreDriveCatalogSnapshot, restoreLoginCatalogSnapshot (cli); pruneShortcutChildCatalogRecord (multisync); applyFatalAuthEffects, normalizePersistedAccountAuthRequirement (sync); e2e bootstrap and test helpers.
- Multi-step flows take locks one after the other, never nested: completeLogin, addNewDriveWithOps, addSharedDriveWithOps and their rollbacks, ReconcileAccountEmail (RenameDriveSections, then the catalog), ClearExpiredPauses. Reads (LoadCatalog, ResolveAccountNames, LoadOrDefault) take no lock.
- No other flock on the data or config directory exists: the owner lock and root locks are on files (`owner_lock.go:52`, `engine_root_lock.go:113`).

## Checks
- `go test -race -count=1 ./internal/config/...`: ok. New tests with `-race -count=50`: ok.
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: clean.
- `verify lint` (runq, log `B4-lint.log`): 0 issues, 9m47s.

## Deviations (behavior, scope, interface)
- The catalog is locked on its own directory (the data directory), not on the config directory: `--config` can move config.toml anywhere, so the config directory does not identify the catalog. On macOS the two are one directory and one lock.
- `UpdateCatalogForDataDir` now creates the data directory before running the callback (the lock needs it); before, only the save created it. Differs only when the callback fails.
- Lock error texts changed ("opening lock root/directory", "locking <dir>"); unlock and close errors are now wrapped.
- Added the troubleshooting section (docs only, not in the brief).

## Recommendations
1. ClearExpiredPauses makes two separately locked deletes per drive, so a stuck holder now stalls a reload up to 20 s per expired drive: delete both keys in one locked write.
2. An engine start that must clear a persisted auth requirement fails that mount when the catalog stays locked (`normalizePersistedAccountAuthRequirement`); consider a Warn for `ErrConfigLocked` there, as the fatal-auth path does.
3. Give config writers a context so Ctrl-C ends the wait at once (CLAUDE.md asks for ctx on blocking calls; no caller has one today).
4. flock on a directory can fail on NFS (EBADF) or FUSE (ENOSYS), and then every config change fails; a lock file plus `isUnsupportedLockErrno`, as the root lock does (analysis 02 6.2, unconfirmed).

## Unfinished
Nothing.

## Packages for the reviewer to test
`internal/cli` (login, drive add/remove, logout, status auth marking, pause/resume), `internal/multisync` (reload pause expiry incl. `TestOrchestrator_Reload_TimedPauseExpiry`, child-artifact catalog cleanup), `internal/sync` (auth-requirement mark and clear; test helpers use `UpdateCatalogForDataDir`).
