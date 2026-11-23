# Report: Graph request bodies as bytes, honest not-sent marks, own timeouts, quirks

Commit `2c4b3cfe` on `fix/graph-client-leftovers` (base `27e76cc3`), 39 files, +946/-465, amended once after review. Not pushed.

## What changed
- `client_auth.go`: `do`, `doRequest`, `doRequestVia`, `doOnce`, `retryUnauthorized`, `buildAuthorizedRequest` take `body []byte` (nil for none); each attempt builds a fresh `bytes.Reader`; `rewindBody`/`errBodyNotReplayable` deleted; new `errRequestNotSent` (token failure, context already ended, build failure, every pre-retry failure after a 401); Content-Type only with bytes.
- `client_preauth.go`: `doPreAuth` marks an ended context and a failed `makeReq` not sent; its GraphError via `buildError` (duplicate removed).
- `item_create.go`: `createOutcomeUnknown` is false on `errRequestNotSent`; comments. `quirks.go`: canceled backoff marked not sent.
- `upload_transfer.go`: `sessionCancelTimeout`; a last fragment that never left is not a `FinalFragmentError`.
- Callers converted to bytes: `CreateFolder`, `MoveItemIfMatch`, `UpdateFileSystemInfo` (items_mutation.go), `CreateFile`, `CopyItem`, `CreateUploadSessionForItem`; `RestoreItem` passes nil plus an explicit JSON Content-Type (wire unchanged). All other callers already passed nil. No caller streams a large body; `doRawUpload` (zero-byte overwrite) uses an `io.ReaderAt` with a fresh `SectionReader` per attempt and no 401 retry, left as is.
- `items_copy.go`: `PollCopyStatus` through `doPreAuth`. `auth.go`/`auth_browser.go`: `callbackShutdownTimeout`, `callbackHeaderReadTimeout`. `errors.go`: docs.
- Docs: graph-client.md (profiles with `notices`, Verified By, Transfers, 401 Token Refresh, constraint), R-6.8.14, R-5.6.12, R-5.6.15, graph-api-quirks.md (eTag probe, bare-400 folder create, Upload Session Leak), live-incidents.md (LI-20261111-01, LI-20261110-01 bullet, LI-20260422-05 link).
- Marks reviewed: set at item_create.go createOutcomeUnknown (fixed), 2xx decode failure and `readCreatedFileByName` (true: file exists, kept), upload_transfer.go final fragment (fixed); tested at `removeCreatedPlaceholder` (follows the mark).
- Constants: `shutdownTimeout` was borrowed at upload_transfer.go:330 and auth_browser.go:105; renamed `callbackShutdownTimeout`. Others checked, own names: `placeholderCleanupTimeout`, `forcedRefreshMinInterval`, `defaultTransferStallTimeout`.

## Tests (all seen failing first unless noted)
- `TestDo_401RetrySendsTheBytesOfTheFirstRequest`: retry sends exactly the first request's bytes (old API resent the buffer from its start).
- `TestCreateFile_TokenFailureIsNotOutcomeUnknown`, `TestCreateFile_RequestBuildFailureIsNotOutcomeUnknown`: no request, no mark.
- `TestCreateFile_UnsentRetryIsNotOutcomeUnknown`: 401 then refresh fails; fresh-parent 404 then canceled backoff: one request, no mark.
- `TestUploadFromSession_UnsentFinalFragmentIsNotUncertain`: refused session URL; context ended before the last fragment.
- `TestPollCopyStatus_RetryLogNeverNamesTheMonitorURL`: User-Agent sent, monitor URL absent from retry log.
- Changed: expired-context case of `TestCreateFile_MarksOnlyAFailureThatMayHaveCreatedTheFile` flipped (failed first); `TestDo_ContentTypeForBody` (body / no body), `TestDo_ContextCancellation` (not sent, 0 requests), `TestDo_TokenError`, `TestRestoreItem_Success` (wire), `TestDoOnce_401_RefreshSucceeds`. Removed (the type rules them out): `TestDo_401WithNonSeekableBodyFailsLoudly`, four `TestRewindBody_*`, two seeker fakes.
- Not written: `TestRemoveEmptyItem_WaitsBetweenRefusedDeletes` (eTag does not keep moving).

## Live probe (Personal test account, drive endpoints only, throwaway integration test via `-overlay`, not committed)
- Session opened (2 rounds): eTag `,1` to `,2` at the open; `,2` at 0, 0.25 ... 60 s; size 0, mtime and empty-content hash unchanged. Abandoned request after 80/160 ms (2 rounds): `,2` within the first second, then held. Conditional delete on the last eTag: 204 in all four; then 404.
- Every request to the upload URL (`PUT` fragment, status `GET`, cancel `DELETE`) answered 401 after about 12 s that day (6 of 6), so a successful client-side cancel could not be measured. Cause unknown. Uploads to the test account fail at the first fragment while this lasts.
- Business not probed (no Business test drive); recorded as such.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`, `go vet -tags integration ./internal/graph/`: pass.
- `go test -count=1 ./internal/graph/... ./internal/retry/... ./internal/graphtransport/...`: pass. `-race -run` on added/changed tests: pass.
- golangci-lint `./internal/graph/...` (go1.26.11): `0 issues.` (W4-lint.log). Only a Markdown file changed after it.

## Deviations
1. A context already ended before sending is "not sent" (explicit pre-check in `buildAuthorizedRequest`/`doPreAuth`); the existing expired-context case pinned the opposite. R-5.6.15 and R-5.6.12 reworded.
2. Unsent retries after a 401 and canceled quirk backoffs are also marked not sent (the sent attempt was a refusal).
3. `PollCopyStatus` defect fixed in a touched file (URL in retry logs against R-6.8.17; no User-Agent against R-6.8.18). Its 401 now reads `ErrPreAuthURLRejected`.
4. The analysis says the canceled-request landing was never probed; the quirk doc already held a 2026-11-11 probe confirming it. This probe added the minute of readings.
5. The integration harness wrote the refreshed test token back into `.testdata`, a symlink into the main checkout's git-ignored bundle, as it always does.

## Also fixed
- auth_browser.go: `registerCallbackHandler`'s doc comment sat on `deliverCallbackResult`.
- errors.go: package doc claimed retry and rate limiting. quirks_test.go: `require` on a server goroutine. client_auth.go: `doRequestVia` undocumented. client_preauth.go: copy of `buildError`. graph-api-quirks.md: "Upload Session Leak" prescribed `context.Background()`.
- Review pass (each was a "Not fixed" entry or a review item):
  - retired-backlog-ids.md: the `B-159` row went with its only citation.
  - `createOutcomeUnknown` comment and graph-client.md: only the attempts the client itself makes are known refusals; a retrying transport's own attempts are unseen; sync's transport retries nothing.
  - Quirk and policy renamed `file-create-transient-404` / `QuirkFileCreateTransient404` / `retry.FileCreatePolicy` / field `fileCreate`; e2e classifier and its fixture, retry.md, graph-client.md, quirk doc (two stale runtime policies), ledger note on LI-20260405-09. No old name left.
  - `graph.SessionCancelTimeout` (10 s, in upload_session.go) is the one session-cancel bound; driveops `dropSession` uses it; its removal bound is `placeholderRemovalTimeout`. Docs updated. `TestDetachedContexts_AlwaysCarryADeadline` passes.
  - profiles.go cites R-6.8.27. socketio.go: no Content-Type on the GET (a live call answered 200 without one; recorded in LI-20261112-01).
  - `ResumeUpload`: only a session-status 404 is expired, wrapped with its answer (`TestResumeUpload_OnlyAGoneSessionIsExpired`, failed first).
  - `Login`/`TokenSourceFromPath` docs: the command's or daemon's context.
  - `UpdateFileSystemInfo` validates first (`TestUpdateFileSystemInfo_ZeroMtimeSendsNoRequest`, failed first on the log record).
  - Quirk retry loop restructured, exhaustion record in `quirkRetryExhausted`; no unreachable return (a zero `MaxAttempts` now makes one attempt).
  - All 20 verb-less `fmt.Errorf` in internal/graph are `errors.New`.
  - Ledger LI-20261112-01 (provider incident, open), linked from the quirk doc.
- Review-pass checks: `go build ./...`, `go vet ./...`, e2e and integration vets pass; `go test -count=1` graph, retry, graphtransport, driveops, cli pass; `-race -run` on the new tests and quirk neighbours pass; `TestFindBacklogIDViolations_RealRepoIsClean`, `TestFindMutexDocViolations_RealRepoIsClean`, `TestFindRequirementViolations_RealRepoIsClean`, `TestDetachedContexts_AlwaysCarryADeadline` pass; golangci-lint over graph, driveops, graphtransport, retry, e2e: `0 issues.` (W4-lint.log).

## Not fixed (file:line, what, fix)
1. internal/sync/chaos_remote_test.go:393-405: `chaosCreateFailure` re-derives CreateFile's mark by hand and cannot express a not-sent create. Planned elsewhere.

## Unfinished
Nothing.

## Packages the reviewer should test
`internal/sync` (consumer of `ErrCreateOutcomeUnknown`).

## Review (2026-11-12)
Both passes read in full (the product and test diff, the spec word diffs, the ledger entry).
- **Second pass asked** (seven items): the `B-159` registry row whose only citation the first pass removed (the pre-pr gate would have failed on it); the comment and spec claim that every attempt of a create is a known refusal, true only of the attempts the graph client makes itself; one session-cancel bound instead of two; the quirk and policy named for the upload session that no longer creates files; nine of the writer's own "Not fixed" entries; the 20 verb-less `fmt.Errorf` calls in the package; a ledger entry for the 2026-11-12 provider incident.
- **Checked in the second pass**: no old quirk or policy name left in the tree; `driveops/transfer_session.go` tests `ErrUploadSessionExpired` before `ErrNotFound`, so the newly wrapped 404 keeps the lookup path; `doGetWithHeaders` still has two callers; R-6.7.34 states the zero-time rule the new test cites; `urlValidators` has the five fields its comment names; the ledger entry holds no account, drive or item ID.
- **Fixed by me in review**: the ledger entry ended with a sentence about other work in flight. It now has the ledger's own fields: Resolution / mitigation (none in the client for the outage; a transfer whose upload URL answers 401 fails alone and is retried) and Follow-up (the two product faults the incident showed).
- **Decisions**: recorded in the plan entry, (a) to (f).
- **Left for planned work**: the chaos layer's hand-derived create mark (the watch-absence and session-chaos change); the deadline through token refresh and retry waits; the status and identity-check changes the incident called for.
- **Checks of mine**: plan-id diff shows only the removal of an old id from a comment; retired-backlog-id diff lists `client_auth.go` and the registry, which agree; `go test -count=1` over graph, retry, graphtransport, driveops and cli (123 s); the whole non-race `go test ./internal/sync/...`; tagged e2e vet; the pre-pr gate.
