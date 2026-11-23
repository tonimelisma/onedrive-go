# A1 — Report

Increment: "Empty file left by a failed upload is removed; PauseResume waits for content"
Worktree: `~/src/onedrive-go-wt/upload-placeholder-retry`, branch `fix/upload-placeholder-retry`.
Commit: `28b80dc2` (one local commit on top of `origin/main` at `70301c0e`; 14 files, 535 insertions, 96 deletions). Not pushed.
Written 2026-11-11.

## What changed

### Product

- `internal/graph/item_create.go`
  - New constants `placeholderCleanupTimeout` (10 s) and
    `placeholderCleanupAttempts` (3). The cleanup used to borrow
    `shutdownTimeout` (5 s), a constant of the sign-in callback server.
  - New `func (c *Client) RemoveEmptyItem(ctx, driveID, itemID) (removed bool, err error)`.
    Each attempt reads the item and deletes it with `If-Match` on the eTag
    that attempt read. A 412 on the delete starts the next attempt with a
    fresh read. It returns `true` when the item is gone (delete succeeded, or
    a 404 on the read or on the delete), `false, nil` when the item reads
    back with content or without an eTag (no delete is sent), and an error
    for any other read or delete failure or after three refused deletes.
  - `removeCreatedPlaceholder` keeps its guard (no cleanup when the final
    fragment's outcome is uncertain, or when there is no created item) and
    its detached, time-limited context, and now calls `RemoveEmptyItem`.
    Why: the old code made one read and one conditional delete. In nightly
    run 42865435904 that delete answered 412 and the empty file stayed on
    OneDrive, against R-5.6.14.
- `internal/driveops/interfaces.go`: `PlaceholderRemover` is now
  `RemoveEmptyItem(ctx, driveID, itemID) (removed bool, err error)` instead of
  `DeleteItemIfMatch`. The compile-time assertion that `*graph.Client`
  satisfies it is unchanged and still holds.
- `internal/driveops/transfer_session.go`: `removePlaceholder` and
  `removeEmptyPlaceholder` are one function. It no longer reads the item
  itself; it hands the item ID to the remover on a context detached from the
  upload's and bounded by `cancelSessionTimeout`. Both outcomes that leave
  the item (an error, or `removed == false`) are logged at Debug.
- `internal/driveops/transfer_verify.go`: `settleUncertainFinalFragment`
  asks for the removal by the landed item's ID, and only when the lookup
  shows the create's own item still empty.

Why the driveops change: it had its own copy of the read and the single
conditional delete, with the same weakness. With the seam moved, both of its
callers (a final fragment that did not land; a failed `OnCreated` hook) get
the bounded retry from one implementation.

### Tests and test harness

- `internal/graph/upload_test.go`: the fake server `sessionUploadRecorder`
  can script the read-backs (`readBacks`) and the delete answers
  (`deleteStatuses`) and counts reads. Past the end of a script it answers
  as before.
- `internal/graph/item_create_test.go`, `internal/driveops/transfer_create_test.go`:
  new and changed tests, listed below.
- `e2e/remote_read_helpers_test.go`: new helper `waitForRemoteFileContent`.
  It polls `get` through `pollRemoteEventually` until the downloaded file
  equals the expected content.
- `e2e/sync_watch_e2e_test.go`: `TestE2E_SyncWatch_PauseResume` waits for
  the remote content before it sends SIGTERM (it used to wait for the name
  with `stat`). The content literal became the constant `pausedContent`.
  Compile-checked only; no e2e test was run.

### Documents

- `spec/requirements/transfers.md`: R-5.6.14 gains the rule (a 412 means the
  eTag moved after the read; repeat the read and the conditional delete, at
  most three attempts in one bounded cleanup; never delete an item with
  content or without an eTag). R-5.6.15 points at the same removal.
- `spec/design/graph-client.md` (Transfers): `RemoveEmptyItem`, the two
  constants, the outcomes, and the five new test names.
- `spec/design/drive-transfers.md` (New files and their placeholder): the
  transfer manager never deletes a placeholder itself; the two places that
  ask for the removal, with their tests.
- `spec/reference/graph-api-quirks.md` (An Upload Session Moves An Empty
  Item's eTag): the eTag just read can be stale too; marked as inferred,
  not probed.
- `spec/reference/live-incidents.md`: entry `LI-20261110-01` and its index
  row.

No production file was added or renamed, so no `GOVERNS:` line changed.
All 11 test names cited in the two design-doc sections exist.

## Tests added and what each proves

Graph (`internal/graph/item_create_test.go`), all `// Validates: R-5.6.14`:

| Test | Proves |
|---|---|
| `TestUpload_FailedContentStepReadsAgainWhenItsDeleteIsRefused` | A 412 is followed by a second read, and the second delete carries the second read's eTag. The regression test for the nightly failure. |
| `TestUpload_FailedContentStepLeavesAnItemThatGainedContentBetweenAttempts` | When the second read shows content, no further delete is sent. |
| `TestUpload_FailedContentStepStopsAfterThreeRefusedDeletes` | Exactly three reads and three deletes, never a fourth; the caller still gets the upload's own error with the created item, not the cleanup's 412. |
| `TestUpload_FailedContentStepStopsWhenItsEmptyItemIsAlreadyGone` | A 404 on the second read ends the removal without another delete. |
| `TestRemoveEmptyItem` (10 cases) | The method directly: delete by the eTag just read; already gone; gone between read and delete; content is never deleted; an item without an eTag is never deleted; re-read after a 412; content after a 412 stays; gives up after three with `ErrPreconditionFailed`; a failed read is reported and nothing is deleted; a delete refused for another reason is not tried again. Errors name the item. |

Driveops (`internal/driveops/transfer_create_test.go`), `// Validates: R-5.6.15`:

| Test | Proves |
|---|---|
| `TestSessionUpload_FailedCreatedHookRemovesTheEmptyItem` (new) | When recording the created item fails, no content is sent, the session is cancelled, and the item is handed to the removal by its ID. This path had no test. |
| `TestUploadFile_UncertainFinalFragmentThatDidNotLandRemovesThePlaceholder` (changed) | Now asserts the removal is asked for by item ID. |
| `TestUploadFile_UncertainFinalFragmentRemovesThePlaceholderByItsCurrentETag` (changed) | Removal is asked for while the item is empty under a moved eTag, and not asked for when someone else's content landed. |

Seen failing before the production change: the four `TestUpload_...` tests
failed on their assertions (one delete where two or three were expected).
`TestRemoveEmptyItem` and the driveops tests could fail only as compile
errors, because they call a method and a fake field that did not exist yet.

## Verification

| Check | Result |
|---|---|
| `go test -race -count=1 ./internal/graph/...` | ok (38.9 s, on the final tree) |
| `go test -race -count=1 ./internal/driveops/...` | ok (23.9 s) |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `go vet -tags 'e2e e2e_full' ./e2e/...` | ok |
| `go vet -tags e2e ./e2e/...` | ok |
| `go test -count=1 ./internal/sync/` (no race detector; extra, see deviation 6) | ok (554.9 s) |
| `gofumpt -l`, `goimports -l -local github.com/tonimelisma/onedrive-go` | no file listed |

Not run, by the rules: `devtool verify` (so `golangci-lint` has not seen this
change), any e2e test, any push. The driveops race run predates the last
amend, which changed one comment in a graph test file only.

## Compliance with the design-doc sections touched

`spec/design/graph-client.md`, Transfers:

- Create by body, content by item ID conditional on the created eTag: unchanged.
- Cleanup only before the final fragment: the guard in `removeCreatedPlaceholder` is unchanged.
- Delete only an item that reads back empty, conditional on the eTag just read: `RemoveEmptyItem` returns before any delete when `Size != 0` or the eTag is empty.
- A 412 is answered by reading again, at most `placeholderCleanupAttempts`: the loop; proved by the three-refusals test.
- One bound for the whole cleanup, detached from the upload's context: `placeholderCleanupTimeout` on `context.WithoutCancel`.
- A 404 counts as removed; any other failure ends the removal: both branches, each with a test case.
- The caller gets the upload's error, never the cleanup's: cleanup failures are logged at Debug only.

`spec/design/drive-transfers.md`, New files and their placeholder:

- The transfer manager never deletes a placeholder itself: it has no `DeleteItemIfMatch` call left; it calls `PlaceholderRemover.RemoveEmptyItem`.
- Detached context bounded by `cancelSessionTimeout`; failure or an item left in place is logged at Debug.
- Two callers, each cited with its test.

Requirements: R-5.6.14 (rule and tests), R-5.6.15 (same removal; new hook-failure test).

Repository conventions: errors wrapped with `%w`; named constants; no
`time.Sleep`; testify; every new test has a `// Validates:` line; no
compatibility shim (the old interface method is gone from the seam;
`Client.DeleteItemIfMatch` itself stays because sync's delete path uses it).

## Deviations from the analysis or the brief

1. Error strings carry the file's `graph: ` prefix (`graph: removing empty item %s: %w`), as every other error in that file does; the brief's literal had no prefix.
2. `settleUncertainFinalFragment` keeps a `landed.Size == 0` check before it asks for the removal. `RemoveEmptyItem` would refuse a non-empty item anyway; the check saves two requests and keeps the "someone else's content landed" test meaningful.
3. One extra driveops test, `TestSessionUpload_FailedCreatedHookRemovesTheEmptyItem`. The brief asked for the fakes to be updated; this caller of the changed function had no test at all.
4. `spec/design/graph-client.md` was updated although the brief's list names four documents. It governs `item_create.go` and cites its tests, so rule 5 requires it.
5. The fake server got two scripting fields and a counter. That is not a fault-injection framework, but it is more than a rename.
6. `internal/sync`'s test mock has `DeleteItemIfMatch` and therefore used to satisfy `PlaceholderRemover` by accident; it no longer does. Nothing in `internal/sync` was edited. I ran its tests once (no race detector) to see that none depended on it: all pass.
7. The analysis placed `getRemoteFile` at lines 438-471; on `main` it is at 432-436. The analysis' "5 s `shutdownTimeout`" and its cleanup line numbers describe the code before this change.
8. The analysis says the 2026-10-30 occurrence had "the same signature". It did not: that upload was abandoned because the file changed during it, and the cleanup deleted on the creation eTag (commit `2891ad55`, #818). The ledger entry records it that way.
9. The analysis' timeline was not taken on trust. I downloaded the run's `e2e-debug-logs` artifact (read-only) and checked every time and status in the ledger entry against the test's log. The download is deleted.
10. "The cancelled request still reached the server" is recorded as inferred from the order of the requests, in the ledger, the quirk note and the test comment. Nothing has confirmed it.
11. Ledger entry: `First seen` is 2026-10-30 (the #818 occurrence, which got no entry then) while the ID is dated 2026-11-10 as the brief specified. The title names what both occurrences share. Classification is "product bug"; the test's name-only wait is explained in the Summary.
12. R-5.6.15 was reworded by one clause so it points at the same removal as R-5.6.14.
13. A Debug line is logged when the item is left in place (graph and driveops). It was silent before.
14. The e2e helper follows `eventuallySharedContentEquals` in shape but uses `pollRemoteEventually` and one local path, and reads the file only after a successful `get`.
15. `spec/reference/live-recurring-families.md` was not given a family entry; the brief does not name it.
16. Commit subject is the imperative form of the PR title (rule 8): "Remove the empty file a failed upload left; make PauseResume wait for content".
17. The commit was amended once before hand-back (one comment reworded, deviation 10). It is still one commit.
18. The host rule "install after every onedrive-go commit" was not followed: the subagent rules forbid touching `~/.local`, `/usr/local/bin` and the running client. Installing `28b80dc2` is left to the launching agent.

## Recommendations

1. Convert the other name-only waits in `e2e/sync_watch_e2e_test.go` to content waits with the new helper. `TestE2E_SyncWatch_BasicRoundTrip` (line 103) and `TestE2E_SyncWatch_ControlSocketReload` (line 374) send SIGTERM right after a `stat` succeeds, exactly as PauseResume did; line 351 waits on a name before a reload.
2. A live probe on the test account: cancel `createUploadSession` client-side on an empty item, then read its eTag before and after, and try the conditional delete. It settles deviation 10, and it shows whether the eTag moves once or keeps moving while an orphan session is open. If it keeps moving, three immediate attempts can still lose, and a short backoff between attempts would be the next step.
3. Recorded-session (large file) path: `Client.CreateUploadSession` creates the item and opens the session in one call (`internal/graph/item_create.go:187-203`). When the session request fails, the cleanup runs (now with the retry), but the `OnCreated` hook has not run yet, so no pending-create record exists. If the cleanup fails too, nothing identifies the leftover as the client's own. That belongs to the later PR on the created-item record.
4. `internal/graph/upload_transfer.go:329` still bounds a session cancel with the sign-in server's `shutdownTimeout`. It deserves its own named constant, as the cleanup now has.
5. `cancelSessionTimeout`'s comment in `internal/driveops/transfer_session.go` mentions only the session cancel; it also bounds the placeholder removal.
6. Add a family entry to `spec/reference/live-recurring-families.md` for "empty item left by a failed create" (two occurrences now).
7. The three "drain" wordings the analysis found (repository `CLAUDE.md` line 186, `spec/design/sync-engine.md` line 567, and the shutdown log text) describe a shutdown that waits for in-flight work, which the 2026-11-10 log does not show for an upload in its session request. A documentation defect, outside this increment.

## Unfinished

Nothing inside the brief's scope.

Left for the launching agent or the reviewer, by the rules: `devtool verify`
(lint and quick lanes), the push and the PR, the live e2e that proves the
PauseResume change, and installing the commit. The nightly claim on
`TestE2E_SyncWatch_PauseResume` is held by this increment and should be
named under "Nightly failures" in the PR.
