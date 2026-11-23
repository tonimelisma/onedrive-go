# A14 Definition of Ready: a create whose answer was lost keeps its claim

Branch `fix/create-answer-lost`, worktree `~/src/onedrive-go-wt/create-answer-lost`, base `main` at `68ca8f3b`.

1. PASS **Task intent**: user-visible outcome. When the request that creates a new file reaches OneDrive but
   its answer never reaches the client (no response, 5xx, 408, canceled or expired context), sync keeps its
   create intent, recognizes the empty file as its own and uploads into it, instead of clearing the intent
   and later reading its own empty file as a stranger's (a create conflict and a conflict copy).
2. PASS **Non-goals**: no store generation bump and no schema change; no change to the observation-side
   claim (`claimIntendedPlaceholderTx`); no change to the CLI `put` path or to the retrying transport; no
   creation-time comparison; no claim of any file on a 409 that had no prior intent; no live or e2e runs, no
   push, no PR, no install.
3. PASS **Governing docs**: `spec/requirements/transfers.md` R-5.6.14 and R-5.6.15;
   `spec/design/sync-execution.md` (the create-upload paragraph, lines 168-194);
   `spec/design/drive-transfers.md` "New files and their placeholder"; `spec/design/sync-store.md` "Pending
   creates"; `spec/design/graph-client.md` Transfers (`item_create.go`); the program's `A14-design.md`; repo
   `CLAUDE.md` (DoR, test style, doc ownership); `.golangci.yml`.
4. PASS **Authority owner**: `internal/graph/item_create.go` owns the create request and says what its
   failure means (new sentinel `ErrCreateOutcomeUnknown` in `errors.go`); `internal/sync/executor_transfer.go`
   (`uploadNewFile`, `resolveCreateConflict`) owns when an intent is kept, cleared or turned into a claim;
   `internal/sync/store_pending_creates.go` owns the durable rows and is changed in comments only.
5. PASS **Code scope**: `internal/graph/errors.go`, `internal/graph/item_create.go`,
   `internal/sync/executor_transfer.go`, comments in `internal/sync/store_pending_creates.go`; tests in
   `internal/graph/item_create_test.go`, `internal/sync/pending_creates_test.go` (or a sibling test file),
   `internal/sync/scenario_remote_test.go`, `internal/sync/chaos_remote_test.go`,
   `internal/sync/dispatch_order_test.go`; docs `spec/requirements/transfers.md`,
   `spec/design/sync-execution.md`, `drive-transfers.md`, `sync-store.md`, `graph-client.md`. Not touched:
   `internal/driveops` production code (its `%w` wrapping already carries the sentinel), the store schema,
   the observation path, `e2e/`, `.testdata`, `.env`.
6. PASS **Regression plan**: tests first, each seen failing before the fix: (a) executor: the create lands,
   the answer is lost, the retry gets 409, claims and uploads, one remote file; (b) a `TestRunOnce_` scenario
   listed in `runOnceScenarios()`: answer lost, the next pass observes the empty item and uploads into it;
   (c) a 409 without a prior intent does not claim a stranger's empty file; (d) chaos fault
   `create:response-lost`; plus a `graph` test of which `CreateFile` failures carry the sentinel.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`
   (compile only); `go test ./internal/graph/... ./internal/driveops/...`; targeted `internal/sync` runs of
   the new tests, the existing pending-create and create-conflict tests,
   `TestReverseDispatchTable_ListsEveryRunOnceScenario` and `TestModel_ChaosGraphConverges`, with a targeted
   `-race` run of the new tests; gofumpt and goimports; `devtool verify lint` once before the commit.
   Reviewer: the pre-pr stage and the live runs.
8. PASS **Data safety**: the only new write to OneDrive is an upload into an existing item, made only when
   the path had a create intent without an item ID within `pendingCreateIntentTTL`, the item has the same
   spelling, is a live file of size 0, and the upload carries If-Match on the eTag just read. A 409 without a
   prior intent behaves exactly as today. No local delete, no overwrite of remote content, no new state DB
   column; an intent kept longer still expires after `pendingCreateIntentTTL`.
9. PASS **Secret and credential safety**: no token, `.env`, account name, drive ID or item ID is read,
   written or logged; test fixtures use the existing synthetic IDs; `.testdata` and `.env` are not staged.
10. PASS **Live provider and destructive operations**: this increment runs nothing live and nothing e2e.
    No history rewrite, no release artifact, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: can a retry layer inside one sync `CreateFile` call replay after an unknown outcome and
      return 409? Current assumption: no. The sync clients have no retrying transport, and the two in-call
      replays (the documented 404 quirk and the 401 token refresh) follow a definite refusal. Validation
      step: read `graphtransport/profiles.go`, `graph/quirks.go`, `graph/client_auth.go` (done); the `graph`
      sentinel test uses the non-retrying client. Owner document/code path affected if assumption is wrong:
      `internal/graph/item_create.go` and `spec/design/graph-client.md`.
    - Question: is the brief's "new requirement R-5.6.15" a free ID? Current assumption: no. R-5.6.15 is the
      existing pending-create requirement, which the design's Docs line also names, so it is amended in
      place and the new tests cite it. Validation step: `grep -rn "R-5.6.15" spec/requirements`; the
      traceability check in `verify lint`. Owner document/code path affected if assumption is wrong:
      `spec/requirements/transfers.md`.
    - Question: does a zero-size local file need a separate path when its lost create is claimed? Current
      assumption: no. `UploadFileToItem` with size 0 is one conditional zero-byte PUT plus the mtime patch,
      guarded by If-Match, the path an adopted placeholder already takes. Validation step: read
      `graph.UploadToItem` (done) and a zero-size case in the executor test. Owner document/code path
      affected if assumption is wrong: `internal/sync/executor_transfer.go`.
    - Question: is a non-`GraphError` failure that never left the client (a token or request-building
      error) safe to mark unknown? Current assumption: yes. It keeps an intent for a create that did not
      land, which claims nothing unless an empty file of that spelling appears at that path within the TTL,
      the exposure a kept canceled intent already has. Validation step: review. Owner document/code path
      affected if assumption is wrong: `internal/graph/item_create.go` (`createOutcomeUnknown`).
