# A7 report: fakes answer with the faults seen live

## What changed (test files only; no product file, no spec)
- internal/graph/upload_test.go: `sessionUploadRecorder` gains `createStatus` (refuse the create, invalidRequest body) and `sessionLandsThenDrops` (the session request lands, moves the eTag to `sessionLandedETag`, the connection drops). Create and session branches moved into `serveCreate`/`serveSessionCreate` to keep the handler under gocyclo 15. `readBacks`/`deleteStatuses` already existed (#860).
- internal/graph/item_create_test.go: two tests below.
- internal/driveops/transfer_create_test.go: `tmCreatingUploader.removeResults` (scripted RemoveEmptyItem failures); one test.
- internal/sync/scenario_remote_test.go: `refuseCreate` (file creates by both routes, folder creates), `invisibleFor` (path reads and child listings), session state; `churnETag` shared by the old knob and the chaos variant; clone copies open sessions.
- internal/sync/scenario_remote_session_test.go (new): `scenarioSessionClient`, the scenario fake with Graph's upload sessions (SessionUploader, ItemSessionUploader, SessionCanceler), and `newScenarioSessionEngine`.
- internal/sync/chaos_remote_test.go: `churnPlaceholder` variant of timeout-after-create (kind `upload-session:timeout-after-create:etag-moved`), one layer test. `create:response-lost` (#871) reused untouched.
- internal/sync/engine_model_based_test.go: the model run wires `churnPlaceholder`.
- internal/sync/pending_creates_test.go: three engine tests below.

## Tests added (all pass; "correct" = passed on first run, documents behaviour already right)
- `TestUpload_RefusedCreateSendsNoContentAndDeletesNothing`: 400 on create is ErrBadRequest, not unknown-outcome, no CreatedItemError, hook not run, sent once, no session/content/read/delete. Correct.
- `TestUpload_SessionRequestThatLandsWithoutAnAnswerStillRemovesTheEmptyItem`: the 2026-11-10 sequence on sync's no-retry transport: delete by creation eTag gets 412, re-read, delete by moved eTag. Correct (#860).
- `TestUploadFile_FailedPlaceholderRemovalKeepsTheUploadsOwnError`: a removal that fails with 412 leaves the upload's own error, asked once. Correct.
- `TestRunOnce_RefusedCreateWaitsOnTheDurableCurveAndClearsItsIntent` (path-limit and bare 400): nothing created, intent cleared, retry_work on the durable curve, `path_too_long` only with the message, not retried before due. Correct.
- `TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt`: listing lag on the 409; states CURRENT behaviour, see defect.
- `TestRunOnce_LargeFileWhoseSessionRequestFailedFinishesIntoItsPlaceholder`: first engine test on the >4 MiB route: the failed session's item is recorded (#864), adopted, filled by ID through an item session; no conflict copy. Correct.
- `TestChaosGraph_TimeoutAfterCreateCanMoveThePlaceholderETag`: the storm has timed-out placeholders with and without a moved eTag; counts match.

## Product defect exposed (not fixed)
internal/sync/engine_result_classify.go:67 classifies by HTTP status (`classifyHTTPResult`) before the stale-precondition check at :134, and internal/sync/executor_transfer.go:369-372 (`uploadTransferError`) wraps the Graph error with `%w`, so worker_result.go:98 records its status. What happens: a create 409 that `resolveCreateConflict` returns as stale (:694 same spelling, :697 nothing listed yet) completes as HTTP 409, `serverAnswerOther`: actionable retry_work with no condition, durable curve (30 s doubling), logged "Not syncing <path>: file changed while it was being synced: ... 409". What should happen: superseded and replanned in the same run (uploadTransferError's comment; sync-execution.md 211-215). By the same order a 412 on a conditional overwrite is requeued as `transient_conflict`, not superseded (not exercised here). `stalePreconditionError` drops the cause, so other stale paths are unaffected.

## Checks
- `go build ./...`, `go vet ./...`: pass. gofumpt and goimports: no changes.
- `go test -race` of internal/graph/... and internal/driveops/... (whole packages): pass.
- internal/sync with `-race -short -run` over create, placeholder, refusal, collision, lost-answer, crash-recovery, chaos-layer, observation and intent tests (the new tests included): pass, 454 s.
- `TestModel_ChaosGraphConverges`: `-race` seed 1 passes (draws no session fault). Without race, `ONEDRIVE_GO_CHAOS_SEEDS=12`: 10 pass, the churn variant fired in seeds 5, 8, 10, 12 and those converged; seeds 9 and 11 fail "no pending creates after convergence", and fail identically with the variant switched off, so they are pre-existing on this base, not from this change (see below).
- golangci-lint on ./internal/graph/... ./internal/driveops/... ./internal/sync/...: `0 issues.` (A7-lint.log; one misspelling fixed first).

## Pre-existing failure found (outside scope, not investigated)
Chaos seeds 9 and 11 leave an intent-only pending create (no item ID) at a local case variant the model's user then renamed away (`docs/50%.txt` beside remote `docs/50%.Txt`; `b.txt` beside `B.txt`). The kept intent outlives the rename until its 72 h TTL. Likely from the kept-intent rules of #871; nightly runs only seeds 1-2, so it does not show there.

## Deviations
- `sessionLandsThenDrops` lands the session request (analysis, LI-20261110-01), not fragment content as the brief's gloss reads; content that lands without its answer is the final-fragment path already tested (`TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain`, `TestUploadFile_UncertainFinalFragment*`).
- The session uploader is an opt-in wrapper (`scenarioSessionClient`), not methods on `engineMockClient` (analysis row 10): a method set cannot be optional, and every engine test with a file above 4 MiB would change route.
- `invisibleFor` is keyed by root-relative path and counted per path read or listing; delta and reads by ID are unaffected (delta lag is another family).
- No spec or ledger change: test-only, no governed file, no live failure investigated. Analysis row 8's ledger entry for the 400 on create is left (brief: specs out of scope).

## Recommendations
1. Fix the defect: check `errActionPreconditionChanged` before the HTTP branch, or keep the Graph status out of the stale error; then flip the listing-lag test and add an engine test for the 412 overwrite.
2. Add the ledger/quirk entry for the 400 on create (analysis Item 4.1).
3. Route `scenarioSessionClient` through the chaos layer so the storm covers the large-file route.
4. The `-race` chaos run (one seed, fewer batches) drew no create fault at all; pick a race seed that does.

## Unfinished / for the reviewer
Nothing unfinished. Reviewer should run all of internal/sync: the scenario fake is shared by about 35 test files (default knobs unchanged; `churnETag` refactor and clone sessions touch them).
