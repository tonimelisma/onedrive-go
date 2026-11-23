# W17 Definition of Ready: an account email change survives an interruption

Worktree `~/src/onedrive-go-wt/email-rename-resumable`, branch `fix/email-rename-resumable`, base `5de4ff7f`.

1. PASS **Task intent**: an account email change is journaled and rolled forward. Before the first rename a journal (the complete list of renames) is written atomically into the data dir; every step (state DB families, token files, config sections, catalog) is idempotent; the journal is deleted after the last step. Any command finishes a journal it finds at the CLI's entry, before it resolves its config; an engine refuses to start on a drive a journal names. A resumed state DB family move never deletes a file at its target, and a store that can be opened has its log folded into the database before the move.
2. PASS **Non-goals**: no store schema or generation bump; nothing in the planner or executor; no change to the purge and reset commands; no completion inside `config.Load`; no engine that waits for or completes a change; no rollback path (the change only rolls forward); no live e2e test.
3. PASS **Governing docs**: `spec/design/config.md` (Email Reconciliation, lines 313-375, and the `GOVERNS` line), `spec/design/sync-store.md` (store_reset.go bullet, 693-711), `spec/design/sync-engine.md` (lock table row "email rename" near 1106 and its verified-by list), `spec/design/cli.md` (`initializeCLIContext`, 279-290), `spec/requirements/drive-management.md` R-3.7.2 (line 113), plus R-2.1.3 (family), R-2.9.5 and R-6.3.9 (locks, in use) which the changed tests already cite. Repo `CLAUDE.md` 55-236 read.
4. PASS **Authority owner**: `internal/config` owns the journal (format, path, lock, steps, completion) and the plan; `internal/sync` owns the store locks, the file family, the fold and the family move, and reaches config only as the injected `StateDBMover`; the CLI owns the one entry call and the wording a user sees; the engine owns its start refusal (`engine_locks.go`).
5. PASS **Code scope**: new `internal/config/email_change_journal.go`; `internal/config/email_reconcile.go`, `write.go` (section collision wording only); `internal/sync/store_reset.go` (`moveStateDBFamily`, new fold), `engine_locks.go` (`MoveStateDBs`, new sentinel, journal check), `engine.go` (one call in `newEngine`); `internal/cli/root.go` (one call in `initializeCLIContext`), `email_reconcile.go`; their tests; the specs in item 3. Not touched: e2e, schema, planner, executor, purge, reset, devtool.
6. PASS **Regression plan**: red first, on skeletons that only add the new signatures: a table test that injects a failure after each of the four steps and then completes (`TestReconcileAccountEmail_AFailureAfterAnyStepIsFinishedByTheNextCommand`); `TestMoveStateDBFamily_ResumeAfterSidecarsMovedKeepsTheLog` (red against today's code, which deletes the moved `-wal`); `TestEngine_RefusesToStartWhileAnEmailChangeIsUnfinished`; a resume with a fresh store at the old name that names the file to remove and why; the entry call finishing a journal before config resolution and failing the command when it cannot; in use leaves no journal; strict journal decoding; the fold leaves one file holding the logged rows; the catalog step leaves another account's drives alone.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`; until the other worktree's `verify default` has ended only non-race `go test -count=1 ./internal/config/...` and single tests by `-run` elsewhere; after it has ended, the new and changed tests with `-race -run`, the `ListsEvery|Table|Governs|Exhaustive|Registered` tests of touched packages, one targeted `golangci-lint` of `internal/config`, `internal/sync`, `internal/cli` into `W17-lint.log`, and on the final tree `go test -count=1 -timeout 60m ./internal/config/... ./internal/cli/... ./internal/multisync/... ./internal/sync/...`, all through `runq.py`. No devtool verify stage.
8. PASS **Data safety**: the change removes one delete path (a resumed family move deletes nothing at its target) and adds none after the journal exists; the only deletes left are lone sidecars at a source without its database (unchanged) and sidecars at a target before the journal is written (unchanged, they are a stranger's). A token rename no longer replaces a file at its target. The fold is SQLite's own truncating checkpoint under the store lock; a failed fold changes nothing and the family moves as files. All tests run in `t.TempDir()` with `XDG_*` and `HOME` pointed there.
9. PASS **Secret and credential safety**: the journal holds canonical IDs and file names only (the catalog already holds both), mode 0600; no token content is read or logged; tests use the suite's `example.com` identities.
10. PASS **Live provider and destructive operations**: none. No product run against an account, nothing under the installed client's config or data directories, no e2e.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does the fold disturb a family whose files are not SQLite (the stand-in bytes several existing tests write)? Current assumption: opening fails with "not a database" before SQLite touches a sidecar, the failure is logged and the family moves as files. Validation step: run the existing `TestMoveStateDBs_*` tests unchanged apart from the new argument. Owner if wrong: `internal/sync/store_reset.go` (the fold's precondition), `engine_locks_test.go`.
    - Question: can a truncating checkpoint leave a log behind without an error? Current assumption: yes (a busy checkpoint answers with a row, not an error), so the family move must still handle sidecars after a fold; under the store lock nothing else has the store open, so in practice the fold leaves one file. Validation step: the fold test asserts one file at the target for a real store; the family move test covers sidecars. Owner if wrong: `store_reset.go`.
    - Question: does a running watch learn about a change another process finished? Current assumption: no, as today: `reconcileGraphUser` asks no watch to reload, so a watch keeps its old drive names until its next reload, and its runner for a paused or backed-off drive could open a store at the old name after the journal is gone. The journal narrows that window to after the change, it does not close it. Validation step: none here; recorded under "Not fixed". Owner if wrong: `internal/cli/email_reconcile.go`, `sync-control-plane.md`.
    - Question: is the advice in a "both present" refusal always right? Current assumption: it rests on one invariant (below) that holds for everything onedrive-go does; a file planted by another program could make it name the wrong side. The message says why, so the user can judge. Validation step: the refusal tests. Owner if wrong: `email_change_journal.go`.
    - Question: is a second config file that shares the data dir renamed? Current assumption: no, as today; the journal names the one config file the change started with. Recorded under "Not fixed".

## Design

**Journal.** One file, `<dataDir>/email-change/journal.json`, written with the config package's atomic writer (temp file, fsync, rename; 0600) and read strictly (unknown field, trailing data or another `schema_version` is an error that names the file). Its folder is also the lock: every writer and finisher of a journal holds an exclusive `flock` on `<dataDir>/email-change/` (the existing bounded directory lock, 10 s), so one email change runs per data dir. Lock order: email-change folder, then store locks, then the config-dir and data-dir locks the steps take; never the reverse. The folder stays; only the file is deleted.

```json
{
  "schema_version": 1,
  "config_path": "/abs/path/config.toml",
  "account_renames": [{"from": "business:old@example.com", "to": "business:new@example.com"}],
  "drive_renames":   [{"from": "sharepoint:old@example.com:site:Docs", "to": "sharepoint:new@example.com:site:Docs"}],
  "state_db_moves":  [{"from": "state_business_old@example.com.db", "to": "state_business_new@example.com.db"}],
  "token_moves":     [{"from": "token_business_old@example.com.json", "to": "token_business_new@example.com.json"}]
}
```

File entries are base names inside the data dir (checked on load: a name with a separator is refused), so a journal cannot rename anything outside it. The whole list is recorded because a plan rebuilt after a crash sees half-renamed inputs. The finisher uses the journal's `config_path`, not the running command's.

**Mover.** `StateDBMover func(moves []StateDBMove, begin func() error) error`. A non-nil `begin` means a fresh change: the mover calls it once, holding every source and target store lock, after its checks pass and before its first rename; `begin` writes the journal. So a store in use refuses with no journal and nothing renamed. A nil `begin` means the journal already lists these moves.

**Steps of a fresh change** (under the email-change lock):

0. Plan and validate: any source and target both present is refused, nothing changed, no journal. The message names the file at the new name (it was there before this change).
1. State DBs: take all store locks; refuse a target database that exists; delete sidecars at each target (a stranger's, since no journal exists); `begin()` writes the journal; then for each move: if the source has a sidecar and nothing of the family is at the target, fold the log (open without schema, truncating checkpoint, close; a failure is logged and ignored); rename each family file that exists at the source, sidecars first, database last, refusing a file present on both sides and deleting nothing at the target. A source with no database has its lone sidecars deleted (unchanged).
2. Token files: rename; source missing means done; both present is refused.
3. Config sections: `RenameDriveSections` on the journal's config file (a missing source section is already skipped).
4. Catalog: accounts re-keyed, and only the drives the renamed account owns.
5. Sync the data dir, delete the journal.

**Resume** runs 1-5 from the journal with `begin == nil`: step 1 skips the checks and the target clearing, because with a journal everything at a new name was put there by this change. A database at both names is the only refusal; it names the file at the old name (and its `-wal`, `-shm`, `-journal`) as the one to remove, because the drive's state already moved and the old-named file was created afterwards.

| Crash after | On disk | The next command |
|---|---|---|
| step 0, target sidecars cleared | every name old, no journal | plans afresh |
| journal written | journal; every name old | finishes 1-5 |
| inside step 1 | journal; each family whole at the old name, whole at the new, or split (sidecars new, database old) | finishes the split family without deleting, then 2-5 |
| step 1 | journal; stores new; tokens, sections, catalog old | 2-5 |
| step 2 | journal; stores, tokens new | 3-5 |
| step 3 | journal; only the catalog old | 4-5 |
| step 4 | journal; every name new | deletes the journal |
| step 5 | every name new, no journal | nothing |

**Where it is finished.** `initializeCLIContext` calls `config.CompleteEmailChange` once, after the logger is attached (the log settings are read first so the completion is logged; that read renames nothing and resolves no drive) and before `resolveSharedTargetBootstrap`. No journal: one `stat`, no lock, no folder created. A journal: lock, re-read, resume, remap this invocation's `--account`/`--drive` and print the existing "Account email changed" line. A failure fails the command, whatever the command. `ReconcileAccountEmail` also finishes a journal it finds under the lock before it plans.

**Engine.** `newEngine`, after it holds its locks and before it opens the store, reads the journal's state DB moves and returns `ErrEmailChangeUnfinished` ("an account email change is unfinished; any onedrive-go command finishes it") when its store is a source or target of one, compared by store lock identity. The mover writes the journal under the store locks and the engine checks under its store lock, so one of them always sees the other. An unreadable journal refuses too.

**Invariant behind the refusals.** With no journal, nothing of this change is at a new name. With a journal, everything at a new name was put there by this change.

## Design changes (second writer, rebased on 27e76cc3)

- **Commands that need no account skip the entry.** "A failure fails the command, whatever the command" is replaced: cobra's own `help`, `completion <shell>` and the hidden `__complete`/`__completeNoDesc` requests skip the whole CLI context (`commandNeedsNoAccount` in `root.go`), so they work with an unfinishable journal. They used to run the strict config load and failed without a configured account. `--help`/`--version` never reach the hook. Every other command fails when the change cannot be finished. `ErrEmailChangeUnfinished` therefore names an example command instead of "any command".
- **The fold's precondition is the SQLite header.** SQLite takes up a log beside a file (and can delete it on close) before it reads the header, so "opening fails with not a database before SQLite touches a sidecar" is wrong; a file without `SQLite format 3\0` is not opened. The fold opens the store with a discarding logger and runs `Checkpoint` then `Close`.
- **One more step boundary.** The step hook also fires right after the journal is written (`emailChangeStepRecorded`), and the step table is generated over every step from that one to the catalog, plus a row that stops between two state DB moves.
- **Renames that meet are refused.** Journal validation (built and read) also refuses two renames to one target and a target that is another rename's source, so two catalog accounts of one user renamed to one ID are refused instead of the second token replacing the first.
- **Catalog drives by ownership.** The catalog step re-keys a drive whose owner is a renamed account and whose key is under the old address; its owner is updated either way. The result drops the unused `UserID` and `CurrentAccount` fields.
