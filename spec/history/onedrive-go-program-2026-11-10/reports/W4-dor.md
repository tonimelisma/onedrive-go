# Definition of Ready: Graph client request bodies, create marks, timeouts, quirks

Worktree `~/src/onedrive-go-wt/graph-client-leftovers`, branch
`fix/graph-client-leftovers`, base `27e76cc3`.

1. PASS **Task intent**: repo-maintenance outcome in `internal/graph`. (a) The
   authenticated request helpers take the request body as bytes, so every
   attempt, the 401 retry included, is built from the same bytes and a body
   that cannot be replayed cannot be passed. (b) A request that never left the
   client (token source failed, context already ended, request not buildable)
   carries a "not sent" mark, and neither the create mark
   (`ErrCreateOutcomeUnknown`) nor the final-fragment mark
   (`FinalFragmentError`) is set on it. (c) No timeout borrows a constant named
   for another purpose. (d) Live facts recorded: an upload session opened and
   canceled on an empty item (eTag over one minute, conditional delete), and
   the folder-create 400 of the 2026-11-11 nightly.
2. PASS **Non-goals**: `TokenSource`'s signature and `RetryTransport`'s waits;
   `internal/sync` beyond callers that must change (none expected; sync fakes
   build their own errors); the control socket client; `stallWatchdog` /
   `transfer_watchdog.go` and its tests (another open PR); renaming the
   upload-session-create quirk and policy; a Business probe (no Business test
   drive exists).
3. PASS **Governing docs**: `spec/design/graph-client.md` (Graph HTTP Runtime
   Policy, Item Operations, Transfers, Transport-Layer Retry, 401 Token
   Refresh); `spec/requirements/non-functional.md` R-6.8.14, R-6.8.19,
   R-6.8.21; `spec/requirements/transfers.md` R-5.6.14, R-5.6.15;
   `spec/reference/graph-api-quirks.md` ("An Upload Session Moves An Empty
   Item's eTag"); `spec/reference/live-incidents.md` (LI-20261110-01,
   LI-20260422-05 recurrence of 2026-11-11); repository `CLAUDE.md` 55-236.
4. PASS **Authority owner**: `graph.Client` owns request construction, the 401
   lifecycle retry and the wire-to-domain marks (`client_auth.go`,
   `client_preauth.go`, `item_create.go`, `upload_transfer.go`). Sync's
   executor stays the only consumer of `ErrCreateOutcomeUnknown`; driveops and
   graph consume `IsFinalFragmentUncertain`. No durable state, no output change.
5. PASS **Code scope**: `internal/graph/client_auth.go`, `client_preauth.go`,
   `item_create.go`, `items_mutation.go`, `items_copy.go`,
   `items_permissions.go`, `upload_session.go`, `upload_transfer.go` (session
   cancel bound and the final-fragment mark only), `quirks.go`, `auth.go`,
   `auth_browser.go`, their tests; docs listed in item 3. Must not touch:
   `transfer_watchdog.go` and its tests, `internal/retry`, `TokenSource`, the
   control socket client.
6. PASS **Regression plan**: `TestDo_401RetrySendsTheBytesOfTheFirstRequest`
   (seen failing on the reader API first: the retry resent from the slice
   start); `TestCreateFile_TokenFailureIsNotOutcomeUnknown`,
   `TestCreateFile_RequestBuildFailureIsNotOutcomeUnknown`, an unsent-retry
   test (401 refresh failure, canceled quirk backoff), the expired-context
   case of `TestCreateFile_MarksOnlyAFailureThatMayHaveCreatedTheFile`
   flipped, and an unsent final fragment that is not uncertain. Restore keeps
   its wire (assertion added to `TestRestoreItem_Success`).
   `TestRemoveEmptyItem_WaitsBetweenRefusedDeletes` only if the probe shows a
   moving eTag.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`,
   `go vet -tags 'e2e e2e_full' ./e2e/...`; `go test -count=1
   ./internal/graph/... ./internal/retry/... ./internal/graphtransport/...`
   (non-race), `-race -run` on the added tests; targeted golangci-lint on the
   touched packages, last, once. No `devtool verify`, no e2e.
8. PASS **Data safety**: no local or store writes. The marks change only which
   failures sync treats as "may have created": a request that never left
   cannot have created anything, so clearing its intent is correct; every
   failure that may have reached OneDrive keeps the mark. The final-fragment
   change only lets the placeholder cleanup run when the last fragment was
   never sent, through the existing read-back-and-If-Match removal.
9. PASS **Secret and credential safety**: the probe reads the token in the
   worktree's `.testdata` (test account only) through the integration test
   harness, which enforces the allowlist. Its output stays in the scratchpad;
   no account name, drive ID, item ID or token is copied into code, docs,
   commit or reports. The nightly log is quoted with names and IDs masked.
10. ASSUMPTION **Live provider and destructive operations**: the probe mutates
    the test account only: it creates a uniquely named scratch folder under
    the root, empty files in it, opens and cancels upload sessions on them,
    deletes them conditionally and deletes the folder. Drive endpoints only,
    never `/me`, never the production account, never `<production sync folder>`.
    Recovery: the folder is deleted in the test cleanup; anything left is a
    test-account scratch folder named `onedrive-go-test-<n>`. The probe file is
    applied with `go test -overlay` from the scratchpad and never committed.
    No dependency upgrade, no history rewrite.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does an upload session opened and canceled on an empty item
      keep moving its eTag after the cancel?
      Current assumption: it moves once (2026-11-11 probe of an abandoned
      session request, 0.24-0.65 s), not repeatedly.
      Validation step: the live probe, eTag at the cancel and over one minute,
      then the conditional delete.
      Owner document/code path affected if assumption is wrong:
      `internal/graph/item_create.go` (`RemoveEmptyItem` backoff),
      `spec/reference/graph-api-quirks.md`, R-5.6.14.
    - Question: does any authenticated caller stream a body that is not small?
      Current assumption: no; every `do`/`doRequest` caller passes marshaled
      JSON, nil, or `http.NoBody`; the streamed simple upload goes through
      `doRawUpload` over an `io.ReaderAt` with a per-attempt body.
      Validation step: grep of every caller (done) and the compiler after the
      signature change.
      Owner document/code path affected if assumption is wrong:
      `internal/graph/client_auth.go`, `spec/design/graph-client.md`.
    - Question: does restore need `Content-Type: application/json` on its
      body-less POST?
      Current assumption: unknown; the wire stays as it is (the header is
      passed explicitly), since restore is exercised live only on Business.
      Validation step: none in this increment.
      Owner document/code path affected if assumption is wrong:
      `internal/graph/items_permissions.go`.
