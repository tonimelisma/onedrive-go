# A3 report: created-item record survives failure and shutdown; clean shutdown is quiet

Branch `fix/created-item-record`, base `fc21d1aa`, one commit `ce909aeb` (17 files).

## What changed
- `internal/driveops/transfer_session.go`: `openSessionForUpload` calls new `recordItemOfFailedSession`, which runs `OnCreated` once for the item a failed session request names in a `*graph.CreatedItemError` (G1); not adopted as `up.createdItem`; hook failure is Debug.
- `internal/sync/executor_transfer.go`: the create hook writes the record on `context.WithTimeout(context.WithoutCancel(ctx), outcomeCommitTimeout)` (G3); `clearPendingCreateIntent` skips the write at Debug when `ctx.Err() != nil`.
- `internal/sync/store_pending_creates.go`: new decision `placeholderAdopted` (was `placeholderNotPending` for "already adopted"); `followAdoptedPlaceholderTx` sets the baseline row's eTag and remote mtime to the observed ones while the item stays empty at its path, through `upsertBaselineRowTx`; helper `baselineEntryByItemIDTx`; comments on clear precondition and file header (G2).
- `internal/sync/store_write_observation.go`: dispatches `placeholderAdopted` to the follow, after `processObservedItem`; the followed row goes to the in-memory baseline with the adopted ones.
- `internal/sync/engine_housekeeping.go`: `refreshProtectedRootsForHousekeeping` logs Debug and returns false when ctx is done, leaving the failure streak untouched.
- `internal/sync/engine_watch.go`: log text "graceful shutdown: canceling in-flight actions and waiting for them to stop".
- `internal/cli/signal.go`: comment says the first signal cancels in-flight work, which is not waited for.
- `CLAUDE.md` (line 186), `spec/requirements/sync.md` R-2.8.3: "cancel, not drain" wording.
- `spec/requirements/transfers.md` R-5.6.15: record from an error, detached record write, canceled attempt keeps its intent, adopted hashless placeholder whose eTag/mtime move is not a remote change.
- `spec/design/drive-transfers.md`, `sync-execution.md`, `sync-engine.md` (Watch Mode list, Console Reporting), `sync-store.md` (Pending creates, baseline writer).

## Tests added (each seen failing first, for the stated reason)
- `TestSessionUpload_FailedSessionRecordsTheItemTheCreateMade` (driveops): hook runs once with the item ID and creation eTag from the error; upload still fails; no second removal requested. Failed: hook never called.
- `TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled` (sync): action canceled right after the create; the store row names the item. Failed: only the intent (empty item ID).
- `TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly` (sync): create canceled in flight; no WARN, intent kept. Failed: WARN "Could not clear a pending create intent".
- `TestCommitObservation_AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict` (sync): eTag moves, then eTag and mtime; plan stays `upload`; cached baseline follows. Failed: `conflict_edit_edit`, stale cached eTag.
- `TestPostSyncHousekeeping_CanceledContextIsQuiet` (sync): no WARN, partial kept, next live pass cleans without "transfer cleanup resumes". Failed: WARN plus the false recovery line.

## Checks run
- `go test -race -count=1 -timeout=20m ./internal/driveops/...`: ok.
- `go test -race -count=1 -timeout=30m -run '<new tests>|TestPendingCreateIntent_|TestRunOnce_.*Placeholder|TestRunOnce_InterruptedCreate|TestRunOnce_EmptyItemACrashLeftUnrecorded|TestCommitObservation_|TestPostSyncHousekeeping_' ./internal/sync`: ok.
- Wider targeted `internal/sync` run (`CommitObservation|Placeholder|PendingCreate|UploadNewFile|Housekeeping|TestRunOnce_.*Create|Crash|TestExecutor_Upload|TestExecutor_.*Create|Shutdown|ProtectedRoot|SyncInvariant`, `-race`): ok (641 s).
- `go build ./...`, `go vet ./...`: clean. gofumpt and goimports (`-local github.com/tonimelisma/onedrive-go`): no changes needed.

## Deviations
- G2 is fixed in the observation commit (the adopted row follows eTag and mtime), not in the comparison SQL. A comparison change would need `pending_creates` inside the shared comparison CTE, which also runs on scratch tables; following the row keeps one source of truth and leaves `sqlite_compare.go` untouched. The row follows the remote mtime as well as the eTag, since without hashes a moved mtime is also read as a remote edit.
- A canceled attempt keeps its intent (not only "skip the write"): documented as intended in R-5.6.15, because a canceled create may still land.
- The docs list in the brief did not name `spec/design/sync-store.md`; it governs the changed store files, so it is updated too.

## Recommendations
1. `SyncStore.ClearPendingCreate` has no callers; remove it or use it after a successful create's outcome commit.
2. A record whose item Graph's cleanup removed lingers until observation sees a deletion (small path before this change too). A sweep at start-up, or clearing the record when the cleanup reports removal (`RemoveEmptyItem` returns `removed`), would bound it.
3. A zero-byte file recovered through the intent keeps its pending row forever, because the item never gains content; consider dropping the record once the baseline holds a committed outcome for it.
4. Live probe on Business: does an empty item's mtime and hash change when a session is opened and canceled on it (settles assumption 1 of the DoR).
5. `internal/sync/doc.go:24` still says the watch loop owns "graceful drain"; it names the drain phase, which is accurate, but could say "shutdown drain of canceled actions" for consistency.

## Unfinished
- Nothing in scope.

## Packages for the reviewer
- Whole `./internal/sync/...` with `-race -timeout=30m` (not run whole here, by the rules), including the exhaustive model/chaos tiers, which exercise placeholder adoption.
- `./internal/cli/...` (comment-only change in `signal.go`).
