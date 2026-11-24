# A1 — Definition of Ready

Increment: "Empty file left by a failed upload is removed; PauseResume waits for content"
Worktree: `~/src/onedrive-go-wt/upload-placeholder-retry`, branch `fix/upload-placeholder-retry`, base `origin/main` at `70301c0e`.
Written 2026-11-11, before any code edit.

1. **Task intent** — PASS. One product-visible behavior: when an upload fails
   after creating its empty item, the empty item is removed even if its eTag
   moved between the cleanup's read and its delete (a 412 is answered by
   reading again, at most three attempts, and a non-empty item is never
   deleted). Plus one test-harness outcome: `TestE2E_SyncWatch_PauseResume`
   waits for the remote content, not only the name, before stopping the
   daemon. Evidence: `internal/graph/item_create.go:213-239` does one read and
   one conditional delete today; `e2e/sync_watch_e2e_test.go:261` waits with
   `stat` only and sends SIGTERM at line 264.

2. **Non-goals** — PASS. Not in this increment: the other name-only e2e waits
   (for example `TestE2E_SyncWatch_BasicRoundTrip`, line 103), any change to
   shutdown behaviour, the created-item record (`recordPendingCreate`), and
   any fake-Graph fault-injection framework. `internal/sync`'s own
   `DeleteItemIfMatch` seam (`domain_io_seams.go:38`, `executor_delete.go:357`)
   is a different interface and stays as it is.

3. **Governing docs** — PASS. Read: repository `CLAUDE.md` (routing table,
   conventions, Dev Process steps 1-4); `spec/requirements/transfers.md`
   R-5.6.12 to R-5.6.15; `spec/design/graph-client.md` (create by body and
   placeholder cleanup, lines 308-320; conditional mutations, line 208);
   `spec/design/drive-transfers.md` ("New files and their placeholder",
   lines 279-311; `GOVERNS:` line); `spec/reference/graph-api-quirks.md`
   ("An Upload Session Moves An Empty Item's eTag", line 1137);
   `spec/reference/live-incidents.md` (index and entry format). The analysis
   `analysis/01b-product-and-e2e.md` lines 23-298 was read as input without
   authority and checked against the code.

4. **Authority owner** — PASS. `internal/graph.Client` owns the Graph
   requests and the rule for removing an empty item (new
   `Client.RemoveEmptyItem`); `removeCreatedPlaceholder` owns when a failed
   create cleans up and on which (detached, time-limited) context.
   `internal/driveops.TransferManager` owns the upload outcome and calls the
   removal through its optional `PlaceholderRemover` seam; it holds no
   durable state for this. No store schema, runtime lifecycle or user-facing
   output changes. The e2e helper owns only test waiting.

5. **Code scope** — PASS. Production: `internal/graph/item_create.go`,
   `internal/driveops/interfaces.go`, `internal/driveops/transfer_session.go`,
   `internal/driveops/transfer_verify.go`. Tests:
   `internal/graph/item_create_test.go`, `internal/graph/upload_test.go` (the
   `sessionUploadRecorder` fake), `internal/driveops/transfer_create_test.go`,
   `e2e/remote_read_helpers_test.go`, `e2e/sync_watch_e2e_test.go`. Docs:
   `spec/requirements/transfers.md`, `spec/design/graph-client.md`,
   `spec/design/drive-transfers.md`, `spec/reference/graph-api-quirks.md`,
   `spec/reference/live-incidents.md`. Must not be touched: `internal/sync`,
   `internal/syncstore`, shutdown code, the main checkout, the running client,
   `~/sync-test-root`, `/srv`. No new production file, so no `GOVERNS:` change.

6. **Regression plan** — PASS. New graph tests beside
   `TestUpload_FailedContentStepRemovesItsEmptyItemByItsCurrentETag`
   (`internal/graph/item_create_test.go`), each `// Validates: R-5.6.14`:
   412 then 204 sends both eTags in order; a second read showing size 7 stops
   after one DELETE and leaves the item; 412 every time stops after exactly
   three DELETEs and the upload still returns `*CreatedItemError`; a second
   read answering 404 sends no further DELETE and reports removed. Direct
   tests of `RemoveEmptyItem` for the read-error and delete-error paths.
   driveops: `tmCreatingUploader` records `RemoveEmptyItem` calls and the two
   existing R-5.6.15 tests assert the item ID. The new tests are run and seen
   failing before the production change.

7. **Verification plan** — PASS. `go test -race -count=1 ./internal/graph/...`
   and `./internal/driveops/...`; `go build ./...`; `go vet ./...`;
   `go vet -tags 'e2e e2e_full' ./e2e/...` as the compile check of the e2e
   change (no e2e test is run by this agent). `gofumpt` and `goimports` with
   local prefix `github.com/tonimelisma/onedrive-go`. The reviewer runs
   `devtool verify` (lint and quick lanes) and the PR's CI, including the live
   e2e that proves the PauseResume change.

8. **Data safety** — PASS. The only remote mutation added is a conditional
   DELETE that is sent only after a read in the same attempt showed size 0 and
   a non-empty eTag, with `If-Match` on that eTag; a 412 causes a fresh read,
   never an unconditional delete; a non-empty item ends the loop at once. The
   attempt count and the timeout are bounded by named constants. No local
   delete, no overwrite, no state DB change.

9. **Secret and credential safety** — PASS. No token file, `.env`, CI secret
   or account name is read or written. The live-incident entry names the run
   number, commit and test only. The untracked `.testdata` symlink in the
   worktree is not staged (files are added by explicit path).

10. **Live provider and destructive operations** — PASS. This agent performs
    no live operation: no e2e run, no CLI against an account, no push, no
    history rewrite, no dependency change. In production the change can delete
    one remote item per failed create, and only the empty item that create
    just made (see item 8); if the delete does not happen the placeholder
    stays an ordinary remote file that the next pass adopts (R-5.6.15).

11. **Review carryover and nightly** — PASS — `go run ./cmd/devtool verify --dod --stage start` (2026-11-11): manifest ready, no actionable stale review threads; nightly run 42865435904 concluded failure; `TestE2E_SyncWatch_PauseResume` claimed by this increment (new claim) and fixed here.

12. **Unknowns and assumptions** — ASSUMPTION.
    - Question: does a session-create request the client cancelled still reach
      the server and move the empty item's eTag after the cleanup's read?
      Current assumption: yes; it is the only sequence that fits the 412 seen
      in nightly run 42865435904, and it is not probed live.
      Validation step: the unit tests script the 412 directly, so the fix does
      not depend on the cause; the quirk note is marked unconfirmed until a
      live probe.
      Owner document/code path affected if assumption is wrong:
      `spec/reference/graph-api-quirks.md` (the note) and
      `spec/reference/live-incidents.md` LI-20261110-01.
    - Question: are three attempts within 10 seconds enough for the eTag to
      settle?
      Current assumption: yes; one late-landing request moves the eTag once.
      Validation step: the nightly e2e after merge; a leftover placeholder
      remains safe (R-5.6.15) if not.
      Owner document/code path affected if assumption is wrong:
      `internal/graph/item_create.go` constants.
    - Question: does the graph test client's retrying transport resend a 404
      or 412, which would inflate the request counts the new tests assert?
      Current assumption: no, it retries only throttling, server errors and
      network failures.
      Validation step: the new tests count requests; if the counts are
      inflated they use the no-retry test client.
      Owner document/code path affected if assumption is wrong:
      `internal/graph/item_create_test.go`.
