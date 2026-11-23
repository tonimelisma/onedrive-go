# A3 Definition of Ready: the created-item record survives failure and shutdown; a clean shutdown is quiet

Worktree `~/src/onedrive-go-wt/created-item-record`, branch
`fix/created-item-record`, base `origin/main` at `fc21d1aa` (after #860).
Written 2026-11-11.

1. **PASS — Task intent.** One product-visible behaviour: a new file whose
   empty item exists on OneDrive is always remembered as this client's
   placeholder, whether the upload fails, a large file's session request fails
   after the create, or a shutdown lands right after the create, and an
   adopted placeholder whose eTag moves again is still finished by upload, not
   turned into a conflict copy plus a download of the empty file. A clean
   shutdown logs no warning from the pending-create clear or from
   housekeeping. Shutdown wording in docs and logs says "cancel", which is
   what the code does.

2. **PASS — Non-goals.** No change to the placeholder removal itself
   (`graph.RemoveEmptyItem`, shipped in #860). No grace period for in-flight
   work at shutdown (analysis 1d (3), not recommended). No sweep of
   `pending_creates` at start-up, no change to the 72 h intent TTL, no change
   to the comparison SQL in `sqlite_compare.go`. No live-incident ledger entry
   (the nightly failure is #860's). No e2e changes.

3. **PASS — Governing docs.** Repo `CLAUDE.md` lines 55-236 and the routing
   table; `spec/design/drive-transfers.md` ("New files and their
   placeholder"); `spec/design/sync-execution.md` (create upload paragraph);
   `spec/design/sync-store.md` (GOVERNS `store_pending_creates.go`,
   `store_write_observation.go`; "Pending creates"; baseline writer helper);
   `spec/design/sync-engine.md` (Watch Mode, Console Reporting);
   `spec/requirements/transfers.md` R-5.6.14, R-5.6.15;
   `spec/requirements/sync.md` R-2.8.3; `spec/requirements/non-functional.md`
   R-6.3.6, R-6.6.21, R-6.6.30. Analysis `01b-product-and-e2e.md` lines
   23-298, each claim relied on checked against `fc21d1aa`.

4. **PASS — Authority owner.** Durable truth: the sync store owns
   `pending_creates` and the baseline (`store_pending_creates.go`, observation
   commit in `store_write_observation.go`, baseline writes through
   `upsertBaselineRowTx`). Mutation of the record: the executor's create hook
   and intent clear (`executor_transfer.go`). When the hook runs: driveops'
   transfer manager (`transfer_session.go`). Remote side effects (create,
   placeholder removal): `internal/graph`, unchanged. Runtime lifecycle and
   housekeeping logging: the engine (`engine_housekeeping.go`,
   `engine_watch.go`). User-facing output: log levels only.

5. **PASS — Code scope.** `internal/driveops/transfer_session.go`;
   `internal/sync/executor_transfer.go`, `store_pending_creates.go`,
   `store_write_observation.go`, `engine_housekeeping.go`, `engine_watch.go`
   (log text); `internal/cli/signal.go` (comment); tests in
   `internal/driveops/transfer_create_test.go`,
   `internal/sync/pending_creates_test.go`, `engine_housekeeping_test.go`;
   docs listed in item 3 plus repo `CLAUDE.md` line 186. Not touched:
   `internal/graph`, `e2e/`, the comparison SQL, the main checkout, `~/tommi`,
   `/srv`.

6. **PASS — Regression plan.** Tests written first and each seen failing for
   the stated reason: `TestSessionUpload_FailedSessionRecordsTheItemTheCreateMade`
   (hook never called); `TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled`
   (record held only the intent, item ID empty);
   `TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly` (WARN "Could not
   clear a pending create intent");
   `TestCommitObservation_AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict`
   (`conflict_edit_edit` instead of `upload`, cached eTag stale);
   `TestPostSyncHousekeeping_CanceledContextIsQuiet` (WARN plus a false
   "transfer cleanup resumes").

7. **PASS — Verification plan.** By this agent: `go test -race -count=1
   -timeout=20m ./internal/driveops/...`; targeted `-race` runs of
   `internal/sync` (the new tests and their neighbours: observation commit,
   placeholder, pending-create, housekeeping, create, crash-point, shutdown,
   protected-root and invariant tests); `go build ./...`; `go vet ./...`;
   gofumpt and goimports. Not by this agent (rules): the whole
   `./internal/sync/...` suite, `verify` profiles, lint, e2e. The reviewer runs
   the whole `internal/sync` suite and `verify lint`/`verify quick`. No live
   e2e or stress need beyond the nightly.

8. **PASS — Data safety.** No new delete path anywhere. The record write
   moves to a detached context bounded by the existing `outcomeCommitTimeout`
   (30 s), the same pattern as the worker's outcome commit, so it can only add
   a record, never lose one. The intent is kept on a canceled attempt, which
   errs toward recognising our own placeholder; it can claim only an empty
   live file at its exact path within 72 h, as today. The baseline follow
   changes only the eTag and remote mtime of a row whose remote size is 0, at
   the placeholder's own path and item ID, through the single baseline writer;
   content facts are untouched, so no download or delete becomes possible
   that was not before.

9. **PASS — Secret and credential safety.** No token files, `.env`, CI
   secrets or account names involved. Tests use synthetic IDs. The untracked
   `.testdata`, `.env` and `.dod-pr-comments.json` are not staged.

10. **PASS — Live provider and destructive operations.** Nothing runs
    against OneDrive, no local state is reset, no release artifacts, no
    history rewrite, no dependency change.

11. **PASS — the start stage ran on this branch; the last nightly's one failing test (TestE2E_SyncWatch_PauseResume) is fixed on main by #860 and awaits the next nightly, so this increment claims nothing.**

12. **ASSUMPTION — Unknowns and assumptions.**

    - Question: does a hashless empty placeholder's modification time move
      when a session is opened and canceled on it, as its eTag does?
      Current assumption: it may; the fix follows both, and the test covers
      both. Following an mtime that never moves costs nothing.
      Validation step: a Business live probe reading an empty item's
      `lastModifiedDateTime` and hash before and after a canceled session.
      Owner document/code path affected if assumption is wrong:
      `followAdoptedPlaceholderTx` in `internal/sync/store_pending_creates.go`.

    - Question: is the item record written for a large create whose session
      failed harmful when Graph's own cleanup did remove the item?
      Current assumption: no. The small path already records before its
      cleanup runs; a record whose item is gone is dropped when observation
      sees the deletion, a later create at the path overwrites the item ID,
      and a 409 resolver that meets 404 falls through to the name check.
      Validation step: read `ClearPendingCreate` callers (none exist; see
      report) and the 409 resolver; existing pending-create tests stay green.
      Owner document/code path affected if assumption is wrong:
      `recordItemOfFailedSession` in `internal/driveops/transfer_session.go`.

    - Question: should a canceled attempt clear its intent on a detached
      context instead of keeping it?
      Current assumption: keep it. A canceled create may still land on the
      server, which is exactly the window the intent exists for.
      Validation step: `TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly`
      and the existing intent-claim tests.
      Owner document/code path affected if assumption is wrong:
      `clearPendingCreateIntent` in `internal/sync/executor_transfer.go`,
      R-5.6.15.
