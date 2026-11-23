# W17 report: an account email change survives an interruption

Worktree `email-rename-resumable`, branch `fix/email-rename-resumable`, rebased onto `origin/main` 27e76cc3 (no conflicts), one unpushed commit `7d07c3b4`, 19 files.

## What changed
- `internal/config/email_change_journal.go` (new): journal type, strict codec and validation (plain data-dir names, no self-renames, no renames that meet), lock on `<data dir>/email-change/`, `emailChanger.apply` (state DBs, tokens, sections, catalog, data-dir sync, journal delete), `CompleteEmailChange`, `UnfinishedEmailChangeStateDBs`, step hook for tests.
- `internal/config/email_reconcile.go`: `ReconcileAccountEmail` finishes a journal under the lock, then plans, validates, builds the journal and runs `apply`; `begin` documented on `StateDBMover`; catalog step re-keys only the renamed accounts' drives; `renameManagedPathIfPresent`, `reconcileCatalogEmail`, `mapsClone`, `hasChanges` and the dead `UserID`/`CurrentAccount` result fields removed; refusal messages name the file or section to remove.
- `internal/config/write.go`: `RenameDriveSections` returns `*driveSectionCollisionError` naming both sections, and does not rewrite the file when nothing is renamed.
- `internal/sync/store_reset.go`: `moveStateDBFamily` never deletes or replaces at the target (refuses a file at both names); new `removeStateDBSidecars`, `foldStateDBLog` (SQLite-header check, schema-less open, truncating checkpoint), `hasSQLiteHeader`.
- `internal/sync/engine_locks.go`: `MoveStateDBs(ctx, dataDir, moves, begin, logger)` clears a stranger's target sidecars and calls `begin` only on a new change, folds before each move, fresh/resume refusals name the file to remove; `ErrEmailChangeUnfinished`; `acquireEngineLocks` refuses (holding both locks, before the store opens) a store an unfinished change moves, via `refuseStoreOfUnfinishedEmailChange`.
- `internal/sync/engine.go`: comment only (the refusal lives in `acquireEngineLocks`; inline it pushed `newEngine` past funlen).
- `internal/cli/root.go`: `initializeCLIContext` finishes an interrupted change before the shared-target bootstrap; `commandNeedsNoAccount` lets `help`, `completion` and `__complete` skip the CLI context.
- `internal/cli/email_reconcile.go`: `stateDBMover`, `finishInterruptedEmailChange`; `applyEmailReconcileResult` also remaps `ONEDRIVE_GO_DRIVE`.
- Specs: `config.md` (GOVERNS, side effects, Email Reconciliation rewritten with the crash table, lock nesting), `sync-store.md` (family move, fold), `sync-engine.md` (lock table, engine refusal), `cli.md` (entry call, account-free commands), `drive-management.md` (new R-3.7.3).

## Tests (the draft's tests and the cli tests were run red before the change; those marked * were written with it)
- config `TestReconcileAccountEmail_AFailureAfterAnyStepIsFinishedByTheNextCommand`: a table generated over every step (recorded, state DBs, tokens, sections, catalog) plus one row between two state DB moves; the next command finishes with every name new and its content kept.
- config `..._AChangeStoppedBeforeItWasRecordedStartsAfresh`*, `..._FinishesAnInterruptedChangeItFinds`, `..._RefusesAMoverThatDidNotRecordTheChange`, `..._RecordsTheWholeChangeBeforeTheFirstRename`*: step 0, reconcile finishing a journal, mover contract, journal contents.
- config `TestCompleteEmailChange_ATokenAtBothNamesSaysWhichToRemove`, `..._AConfigSectionAtBothNamesSaysWhichToRemove`, `..._WithNoJournalTouchesNothing`; `TestEmailChangeJournal_AJournalThatCannotBeReadFailsClosed`; `TestDecodeEmailChangeJournal_RefusesWhatItCannotFinishExactly`; `TestDataDirFileName_RefusesAFileOutsideTheDataDir` (rider); `TestUnfinishedEmailChangeStateDBs_NamesEveryStoreUntilTheChangeIsFinished`.
- config `TestReconcileAccountEmail_LeavesAnotherAccountsCatalogDrivesAlone`, `..._ConfigSectionsAtBothNamesRefuseTheChange`*, `..._RefusesTwoAccountsRenamedToOne`*; `TestRenameDriveSections_NothingToRenameLeavesTheFileAlone`*; `TestRenameDriveSections_Collision` and `TestReconcileAccountEmail_CollisionFailsWithoutMutation` tightened.
- sync `TestMoveStateDBFamily_ResumeAfterSidecarsMovedKeepsTheLog` (red on old code: the moved `-wal` was deleted), `TestMoveStateDBs_RecordsTheChangeOnceBeforeTheFirstRename`, `..._AChangeThatCannotBeRecordedRenamesNothing`, `..._FoldsTheLogIntoTheDatabaseBeforeItMoves`, `..._AResumedMoveKeepsWhatItAlreadyMoved`, `..._AResumedMoveWithADatabaseAtBothNamesSaysWhichToRemove`, `TestMoveStateDBs_RefusesATargetThatExists` (extended).
- sync `TestEngine_RefusesToStartWhileAnEmailChangeIsUnfinished`, `TestEngine_RefusesToStartWhileTheEmailChangeJournalCannotBeRead`.
- cli `TestInitializeCLIContext_FinishesAnInterruptedEmailChangeBeforeTheCommandResolvesItsDrive`, `TestInitializeCLIContext_AChangeThatCannotBeFinishedFailsOnlyCommandsThatUseAnAccount` (red: `help` failed with "no accounts configured"), `TestApplyEmailReconcileResult_RemapsEveryExactSelectorOfTheInvocation`*.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass.
- `go test -count=1 ./internal/config/... ./internal/cli/...`: pass (final tree).
- `go test -count=1 -timeout 60m ./internal/sync/...`: pass, 594 s. Run twice: the first pass (771 s) was before the lint fixes moved the engine refusal into `acquireEngineLocks`.
- `-race -run` on every test written or touched (config, cli, sync, separately): pass. `ListsEvery|Table|Governs|Exhaustive|Registered` in sync: pass. Every test the changed design docs cite exists (scripted check).
- Lint (W17-lint.log): `0 issues.` Run twice: the first found 7 (funlen on `newEngine`, a `govet` shadow, 3 `goconst`, `nolintlint`, `thelper`), all fixed.

## Deviations
- Commands that need no account (`help`, `completion <shell>`, `__complete`/`__completeNoDesc`) skip the whole CLI context, so a change that cannot be finished never blocks them; every other command fails with the reason. The DoR said a failure fails every command. Appended to W17-dor.md.
- The fold's precondition is the SQLite header, not "SQLite fails before it touches a sidecar" (it takes up a beside-log first); `ErrEmailChangeUnfinished` names an example command, since help and completion do not finish a change.
- Journal validation also refuses renames that meet: two catalog accounts of one user renamed to one ID are now refused (the second token used to replace the first).
- Interface: `EmailReconcileResult` lost `UserID`/`CurrentAccount` (never read); `MoveStateDBs` takes `begin`.

## Also fixed
- `renameManagedPathIfPresent` (rider): renamed to the target's base name in the source's folder; journal names are plain data-dir names, refused otherwise.
- `reconcileCatalogEmail` rewrote every catalog drive's ID with the new email, other accounts' included.
- `onedrive-go help` and `completion` failed without a configured account (strict config load).
- `ONEDRIVE_GO_DRIVE` holding the old exact ID was not remapped after a change.
- `RenameDriveSections` rewrote the file with nothing renamed; collision wording did not name the old section.
- Dead `hasChanges` check and redundant inner collision check in the planner; `mapsClone` replaced by `maps.Clone`; `"state_"`/`".db"` literals replaced by `stateDBPrefix`/`stateDBSuffix`; `len(moves)+len(moves)` capacity; stale comment in `TestStateDBFamily_SQLitePairsALogWithADatabaseByNameAlone`; `assert.Contains(err.Error())` turned into `require.ErrorContains` in touched tests.

## Unfinished
None.

## Packages the reviewer should test
`internal/multisync` (engines it starts now read the journal path at start; untouched, not run here).

## Seen and not fixed
1. `internal/config/email_reconcile.go:319` (`buildEmailReconcilePlan`) with `internal/config/mount_state_path.go:48` (`ChildMountID`): a shortcut child's state DB is named by a digest of a mount ID that embeds the parent's canonical ID, so an email change never renames it; the child starts a fresh store and the old one is orphaned. Fix: plan child mount state moves from the parent store's shortcut roots, or key child mount IDs on something email-free.
2. `internal/cli/email_reconcile.go:85` (`reconcileGraphUser`): a running `sync --watch` is not told about a change another process finished; its runners keep the old names until the next reload, and a runner for a paused or backed-off drive could create a fresh store at the old name once the journal is gone. Fix: notify the daemon after a change (as login does) and reload.
3. `internal/config/email_reconcile.go:355` (`collectConfiguredDriveRenames`): only the config file the change runs with is rewritten; a second config sharing the data dir keeps the old section names. Fix: record every config file that names the account, or refuse while another exists.
4. `internal/config/email_reconcile.go:447`: a shared drive's state DB whose catalog record is missing is skipped with a Warn and stays under the old email, orphaned. Fix: decide ownership from the config section's owner or report it in the result.
5. `internal/config/drive_test.go:823` (`setTestDataDir`): sets only `HOME`, so a run with `XDG_DATA_HOME` set writes tokens, catalogs and journals into that directory. Fix: also set `XDG_DATA_HOME` to the temp dir.
6. `internal/sync/store.go:110`: the store DSN is `file:%s` with the path unescaped (a `?` or `#` in the data dir breaks it); the new fold opens through it too. Fix: build the URI with `url.URL` (already planned as a rider elsewhere).

## Review (2026-11-12)
Findings, all fixed in the same commit (13 files, amended):
- (a) The refusal for a config section under both names did not say which to remove. It now says the old-name section was added after the change and names the section and the file.
- (b) A journal with a newer schema version read as "unsupported". It now says the newer onedrive-go that started the change must finish it. A damaged journal gets no remedy text (decision: there is no safe general remedy to print).
- (c) `spec/design/config.md`: the four steps are a nested list.
- (d) Test isolation (the writer's entry 5, wider than reported). `setConfigTestHome(t)` sets HOME to a temp dir and clears the three XDG overrides; every HOME-only site in `internal/config` uses it (about 28), the two `_PlatformSpecific` tests included (both failed under an exported XDG root). In `internal/cli`: six HOME-only sites in `auth_test.go`, `TestFindTokenFallback` (read the ambient data dir), `TestFindTokenFallback_ExistingToken` (wrote a token file into the developer's real data dir) and nine `replace_destination_test.go` tests (created the default data dir; now serial, since they set the environment) use `setTestDriveHome(t)`.
- (e) `renameCatalogAccount` left a re-keyed catalog drive's stored default display name on the old address. A name equal to the old ID's default now follows the ID; a name Graph or the user supplied stays. Test `TestReconcileAccountEmail_CatalogDefaultDriveNameFollowsTheAddress`. Nothing shows that field today; the config section's stored default, which `status` does show, is planned with entries 1 to 4.

Proof:
- Bisect of each package's test binary under empty probe XDG roots (which tests create anything there): `internal/config` clean; `internal/cli` 0 of 701; `internal/multisync` 0 of 160; `internal/sync` 0 of 1586.
- `go test -count=1 ./internal/config/... ./internal/cli/... ./internal/multisync/...`: pass. Lint of `./internal/config/... ./internal/cli/...`: 0 issues.
- Live, offline half (branch binary, private tree copied from the test bundle, invented old address): each of the five interrupted states is finished by the next command (`pause`) in 0.0 s with the one-line notice, every name new, journal gone, store tables and row counts unchanged; a section under both names exits 1 with message (a), `--help` and `completion` still work, and the rerun finishes once the old section is removed; a schema 2 journal exits 1 with message (b), nothing touched; an unreadable journal exits 1 for `pause` and `sync`, nothing touched.
- Live, online half (no journal, everything old, a command that asks Graph): waits on Microsoft Graph answering `/me` again; run before the merge.

Disposition of "Seen and not fixed": 1 to 4, and the config section's stored default name, are one planned change (an email change reaches everything that names the old email); 5 is fixed here (d); 6 is a rider on the planned store-time change.
