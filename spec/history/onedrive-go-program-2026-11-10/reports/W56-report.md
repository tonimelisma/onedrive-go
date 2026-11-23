# W56 report: a failed Graph answer names its error code

## What changed
- `internal/graph/errors.go`: `Error()` puts the most specific code after the status (`graph: HTTP 400 <code> (request-id: <id>): <message>`, `graph: HTTP 400 <code>: <message>`); no code, text unchanged. `mostSpecificCode` shared by `MostSpecificCode` and `MostSpecificErrorCode` (which now sanitizes too); `errorBodyKind` (`graph_error`/`other`/`empty`); field and method doc comments.
- `internal/graph/client_preauth.go`: `buildGraphError` sanitizes `Code` and each inner code like the message; the `request failed` record gains `code`, `inner_codes` (`[]` when none), `content_type`, `body_bytes`, `body_kind` (of the bytes that arrived); a body read failure gets its own debug record with the cause.
- `internal/graph/redaction.go`: `sanitizeGraphErrorTexts`; doc comment on `sanitizeGraphErrorText`.
- Tests: `client_test.go` (`TestGraphError_ErrorString` exact), `redaction_test.go`, new `error_evidence_test.go`.
- `spec/reference/live-incidents.md`: LI-20261115-01 with index row; LI-20261111-01 Last seen 2026-11-15, Recurring yes, recurrence paragraph naming LI-20261115-01, new section links.
- `spec/reference/graph-api-quirks.md`: "A Folder Create Can Be Refused With A Bare 400" became "A Write Can Be Refused With A Bare 400": both occurrences, what neither log could show, what is recorded now.
- `spec/reference/live-recurring-families.md`: family "Bare 400 `Invalid request` On An Ordinary Write" with the decision's rule.
- `spec/design/graph-client.md`: Error Handling paragraph on sanitized codes, new "Error text" and "Debug record of a failed answer" paragraphs, a Verified By row.

## Tests added (each red before the change, logs W56-red1.log, W56-red2.log)
- `TestGraphError_ErrorString` (rewritten as exact table, R-2.10.68): code chain with request ID, outer code without, empty inner codes fall back; the two no-code cases pin today's text (pass before and after by design).
- `TestBuildError_DebugLineNamesTheRequest` (R-2.10.68): fake server; what the record held before the change (method, path, status, request ID, gateway diagnostic, never the message). Written first and kept; passed before and after.
- `TestBuildError_DebugLineRecordsWhatTheBodyHeld` (R-2.10.68): Graph error object with a chain, plain-text body, empty body: code, inner codes, content type, bytes, kind, error text.
- `TestBuildGraphError_RedactsSecretsInCodes` (R-6.8.17): a bearer token in the code and a pre-authenticated URL in the inner code are redacted on the error, in `Error()`, in `MostSpecificErrorCode` and in the debug record.

## Checks
Commit `099d3b25` on `04983d12`.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/graph/...`: pass; with `-race` under the lock: pass (75 s).
- `go test -count=1 ./internal/cli/... ./internal/driveops/... ./cmd/...`: pass; untagged `go test -count=1 ./e2e/`: pass.
- `go test -count=1 ./internal/sync/...` whole, once, under the lock: pass (630 s).
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: four checks pass.
- Lint `./internal/graph/...` under the lock: the first run found 6 tagliatelle issues (snake_case JSON tags of the test's decoded log record); fixed with a type-level `//nolint:tagliatelle` naming the reason, as `go_test_log.go` does; second run `0 issues.` (W56-lint.log). Run twice, not once.
- Spec names checked by hand: every test named in the four docs exists; R-2.10.68, R-6.8.17, R-6.8.21 exist.

## Deviations
- No requirement states the error text or the debug record (searched `spec/requirements/`), so none was amended; the new tests cite R-2.10.68 (evidence a Graph error keeps) and R-6.8.17 (redaction). A requirement of its own may be wanted.
- `MostSpecificErrorCode` (used by e2e preflights) now returns the sanitized code, to match its doc ("the same view the client has"); unchanged for every real code.
- A body read failure is logged on its own debug record (it was dropped), not as an attribute of `request failed`, so that line holds exactly the decided attributes.

## Also fixed
- `errors.go`: dead normalization block in `parseGraphErrorBody` removed (it returned what the code below returned anyway); duplicated most-specific-code loop merged.
- `client_preauth.go`: the cause of a failed error-body read was swallowed; now logged at Debug.
- `redaction_test.go:14,48`: two redaction tests had no `Validates` line; now R-6.8.17.
- `live-incidents.md`: LI-20260422-05 "Last seen" said 2026-11-09 though it records a recurrence of 2026-11-11; entry and index now 2026-11-11.

## Seen and not fixed
- `internal/graph/quirks.go:112-114`: `buildQuirkRetryAttempt` falls back to `graphErr.Code` when `MostSpecificCode()` is empty, which already returns `Code`: dead branch. Fix: drop the `if`.
- `internal/graph/upload_transfer.go:211-238`: the raw simple-upload error path builds its `GraphError` without `buildError`, so a failed simple upload leaves no `request failed` record (no code chain, body kind). Fix: route it through `buildError`, deciding whether a body read failure stays an error.
- `internal/graph/service_read_only_test.go:70,81,90`: `TestMostSpecificErrorCode_*` cite R-6.6.17 (perf stale-work observability), unrelated. Fix: cite the requirement they prove or none.
- `internal/graph/socketio_redaction_test.go:11`: `TestBuildGraphError_RedactsNotificationURL` has no `Validates` line. Fix: R-6.8.17.
- `internal/graph/client_test.go:538,550`: `TestGraphError_ErrorsIs`, `TestGraphError_Unwrap` (and other tests of the file) have no `Validates` line. Fix: tag the requirement each proves.
- `spec/requirements/`: no requirement owns the text of `GraphError` or the debug record of a failed answer. Fix: add one under R-2.10 or R-6.6 and retag the tests above.

## Unfinished
None.

## Packages the reviewer should test
None beyond the checks above; `internal/sync` ran whole once.

## Second pass

Commit `d18e0337` on `341d9820` (#918). Rebase of `099d3b25` onto origin/main: no conflicts; build, vet, tagged vet and `./internal/graph/...` passed before any change. Red runs: `w56p2/red1.log` (unchanged code), `w56p2/mutA.log` to `mutD.log` (lines taken out), under the scratch directory.

### Items
1. New row R-2.10.72 (`spec/requirements/sync.md`, after R-2.10.68, `[verified]`; highest before was R-2.10.71): the text names the most specific code (cut, quoted), the debug record of every failed answer returned as a GraphError, the record of an unreadable body, the fragment 2xx record, never body or message, codes redacted. Wired like R-2.10.68: Validates lines plus the Verified By row (`graph-client.md:99`, now R-2.10.72, R-6.8.17, eight tests); R-2.10 is implemented by `sync-engine.md` as a group. `TestBuildError_DebugLineNamesTheRequest` cites R-2.10.68 and R-2.10.72 and now also asserts the diagnostic on the error (it asserted it only in the record, which R-2.10.68 does not name); `TestBuildGraphError_RedactsSecretsInCodes` cites R-6.8.17 and R-2.10.72.
2. `TestBuildError_RecordsAnUnreadableBody` (RoundTripper with a body cut after 27 bytes): red on `status` 0 and `request_id` ""; with the record's log call taken out it fails "no such debug record" (mutA). Fix: `logErrorBodyReadFailure` adds `status` and `request_id`.
3. `TestUploadToItem_RefusedEmptyOverwriteRecordsTheAnswer` red (no `request failed` record); `TestUploadToItem_UnreadableErrorBodyIsRecorded` red (no read-failure record), its error text already equal. Fix: `logFailedAnswer` (one record for `buildError` and the new `rawUploadError`, `doRawUpload`'s error branch) and `graphErrorOfAnswer` (one builder of the error and its diagnostic, which both paths duplicated). The unreadable-body upload returns the same plain error (`require.NotErrorAs` holds it). `graph-client.md:526` corrected; the three reference sentences are true as written.
4. `TestUploadChunk_UnexpectedStatusRecordsNoBody` (fake server, 206 with a marker body) red: the log held the body, no size or kind. Fix: `body_bytes`, `body_kind`, comment says so; the body is still read, bounded.
5. Rows in `TestGraphError_ErrorString` (space and colon, newline, 300 bytes with a space past the cut, two real codes) red on the first three; the two real-code rows fail when the plain-byte check is taken out (mutD). Fix: `errorTextCode`/`isPlainCodeByte`, `maxErrorTextCodeBytes`. Consequence: a redacted code (`[REDACTED_URL]`) holds brackets and is now quoted; the expectation of `TestBuildGraphError_RedactsSecretsInCodes` changed with it. Sentence in "Error text"; a clause in the row.
6. Doc comments of `Message` and `Error()` say "no error.message"; row "an error object with a code and no message" added, fails with the fallback line taken out (mutC).
7. `live-incidents.md:128`: "only GET, HEAD, OPTIONS, TRACE, PUT and DELETE, and a create marked replay-safe".
8. Rows for `{"error":"invalid_request"}` (fails with the kind check taken out, mutB) and a body over the bound (fails with the bound taken out, mutA); sentence on 65536 and a cut JSON body.
9. Sentence written. The code differs from the brief's 401 clause: the first 401 is closed without a record when the request goes again with another token or the refresh fails, but when no other token can be had that 401 is the returned answer and is recorded; the doc says so.
10. Done (`live-recurring-families.md:190-195`, `live-incidents.md:111-114`, `:134-135`).
11. The ID was on `graph-api-quirks.md:327-328` and also `live-incidents.md:1736,1738`; all four replaced with an invented ID of the same shape (50 characters, `!` at 16). On the two request lines the drive ID in the same path was replaced too, so the path matches the ID; no other tracked file held the ID.
12. Removed; the quirk evidence tests pass (`TestBuildQuirkRetryAttempt_CarriesAgsDiagnostic`, `TestDoQuirkRetry_*`, `TestExtractQuirkEvidence_*`).
13. (a) All seven tests of `service_read_only_test.go` cited R-6.6.17, not five: `TestIsTransientDrivesDiscoveryError_PlainAccessDeniedStillRetries` now R-6.7.13; the three `TestMostSpecificErrorCode_*` R-2.10.72; no row fits `TestIsTransientDrivesDiscoveryError_ServiceReadOnlyIsNotTokenPropagation`, `TestServiceReadOnly_IsClassifiedAsProviderUnavailable`, `TestServiceReadOnly_OrdinaryForbiddenIsNotProviderUnavailable` (R-6.7.13 says such a 403 is retried), lines removed; R-6.6.17 is still cited by tests in `internal/perf`, `internal/sync`, `internal/cli`. (b) done. (c) no requirement row states sentinels or unwrapping: `TestGraphError_ErrorsIs`, `TestGraphError_Unwrap` left untagged.
14. Message rewritten; checked line by line against the diff.

### First report corrections
- Line 43 named three lines of `service_read_only_test.go`; every Validates line of the file was wrong (seven).
- Lines 14-16: three tests cited R-2.10.68 without proving it (now R-2.10.72).

### Also fixed
- `upload_transfer.go`: the close-failure records name the method too; the unused `retry` import is gone.
- `client_preauth.go`: the placeholder message is a named constant.

### Checks
- build, vet, tagged vet: pass. gofumpt and goimports: clean.
- `./internal/graph/... ./internal/retry/...`: pass; `-race ./internal/graph/...` under the lock: pass (88 s).
- `./internal/cli/... ./internal/driveops/... ./internal/devtool/... ./cmd/...`: pass; untagged `./e2e/`: pass.
- `./internal/sync/...` whole, under the lock: pass (556 s).
- `RealRepoIsClean`: four pass. Lint `./internal/graph/...`, once: `0 issues.` (W56-lint2.log).
- By hand: every test named in `graph-client.md` exists; every id in the five spec files exists.

### Seen and not fixed
- `spec/reference/graph-api-quirks.md` and `spec/reference/live-incidents.md` (about 36 occurrences): the live test drive's ID and further item IDs on it are still quoted. Fix: invented IDs of the same shape. Not here: dozens of lines in two ledgers, beyond the one ID this commit replaces.
- `spec/requirements/non-functional.md:123` (R-6.7.13): no requirement states that an inner `serviceReadOnly` is excluded from the drives token-propagation retry or that it reads as `ErrProviderReadOnly`; three tests above cite none, and the Verified By row at `spec/design/system.md:25` names no requirement. Fix: one row, cited by them.
- `spec/requirements/non-functional.md:140` (R-6.7.30) says an oversized body is reported as an error rather than silently truncated; an error body is cut at 64 KiB without one (the record's `body_bytes` shows the bound). Fix: name the bounded error-body read in the row, or change the read.
- `internal/graph/quirks_test.go:324-327`: the comment of `TestHandleChunkResponse_Unexpected2xx` says the request path cannot produce a 204; a fake server can. Fix: drive it through `UploadChunk`, or drop the comment.

## Review fixes

Commit `8a16977c` (amended from `d18e0337`; still one commit on `341d9820`, not pushed).

1. Done. `recordIndex` (`error_evidence_test.go`) now fails unless exactly one record has the message, and every reader of a record goes through it: `answerBadRequest`, `TestUploadToItem_RefusedEmptyOverwriteRecordsTheAnswer`, both unreadable-body tests and both fragment tests. Checked by mutation: a second `logFailedAnswer` call in `buildError` and `rawUploadError` failed four tests with "expected 1, actual 2"; reverted.
2. Done. In `buildError` (`client_preauth.go`), when the read fails, the record's error is built from the arrived bytes by `graphErrorOfAnswer`, so its codes pass the same `buildGraphError` sanitizing as the error's; the returned error keeps the placeholder. New row "a whole Graph error object followed by a read error is recorded by its codes" in `bodyEvidenceCases`; `TestBuildGraphError_RedactsSecretsInCodes` checks the record with and without a read error. One clause in `graph-client.md:526`.
3. Done: the record bullet of the message says both close-failure records of the simple upload now name the method.
4. Done. Checked against `retryUnauthorized` (`client_auth.go`): a failed `Token` or `ForceRefresh`, a failed close of the 401's body and a retry request that cannot be built all return with no record and send nothing more; the same token with no forced refresh, or a forced refresh that returns the rejected token, leave the 401 as the recorded answer. The clause now names all of them.
5. Done. `handleChunkResponse` calls `logErrorBodyReadFailure` with `PUT` and `preAuthLogTarget(uploadChunkDesc)`; the new `preAuthLogTarget` (used by `doPreAuth` too) and const `uploadChunkDesc` keep the name in one place. The returned error is unchanged and pinned by `EqualError`. Clause and test name added to `graph-client.md:526`; the doc comment of `logErrorBodyReadFailure` names the new caller.
6. Done: the comment says the body is read up to `maxErrBodySize` and only a body within that bound lets the connection be reused.
7. Done. `answerBadRequest` now gets its answer from a RoundTripper, `answerTransport` (status, content type, body, read error), which replaces `cutAnswerTransport`; so the whole body table, the over-the-bound row included, and `TestBuildError_DebugLineNamesTheRequest` no longer depend on a server's write. That is smaller than a per-row switch. `badRequestServer` stays for the simple upload test.
8. Nothing to do.
9. Done: both sentences now say "when the answer has no `error.message`".
10. Done: the message says `MostSpecificErrorCode` sanitizes the code it returns the same way, without the "so".
11. Nothing to do.
12. Nothing to do.
13. The reason the report gave for leaving `TestServiceReadOnly_OrdinaryForbiddenIsNotProviderUnavailable` untagged was beside the point: that test is about `IsProviderUnavailable`, which R-6.7.13 does not address. The conclusion stands because no row owns it.

Also fixed: `answerTransport` closes the request body, as the RoundTripper contract asks; `cutAnswerTransport` did not.

New tests, and the product line that makes each fail:
- Row "a whole Graph error object followed by a read error is recorded by its codes" (`TestBuildError_DebugLineRecordsWhatTheBodyHeld`): without `arrived = graphErrorOfAnswer(resp, body)` in `buildError` the record's code is empty and its inner codes `[]` (seen red).
- Read-error round of `TestBuildGraphError_RedactsSecretsInCodes`: the same line (seen red).
- `TestUploadChunk_UnreadableUnexpectedStatusBodyIsRecorded`: without the `logErrorBodyReadFailure` call in `handleChunkResponse` there is no record (seen red: "expected 1, actual 0").
- The count in `recordIndex`: a second `logFailedAnswer` call (mutation above).

Checks (final tree):
- `gofumpt -l` and `goimports -l` on the four changed Go files: no output.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/graph/`: ok. With `-race`, behind the lock: ok.
- `RealRepoIsClean`: four pass.
- Lint `./internal/graph/...` behind the lock: `0 issues.` (W56-lint3.log).
- Test names in `graph-client.md:526`: all exist, the new one included.

### Seen and not fixed
- `internal/graph/upload_transfer.go:149`: the record of an unexpected 2xx fragment answer names no request ID, method or path, unlike the read-failure record now written beside it, so it cannot be tied to OneDrive's side. Fix: add `request_id`, `PUT` and `preAuthLogTarget(uploadChunkDesc)`.
- `internal/graph/client_preauth.go:71`: the message `failed to read error response body` now also covers a 2xx fragment body, which is not an error body. Fix: name it `failed to read response body`, with the test constant and the design doc.

## After the review fixes (2026-11-15, by the lead)

Commit `0514820e` (amended from `8a16977c`; one commit on `341d9820`).
Both "Seen and not fixed" entries above are fixed in the commit:

- The record of a fragment's unexpected 2xx answer
  (`internal/graph/upload_transfer.go`) names `method`, `path`
  (`preAuthLogTarget(uploadChunkDesc)`) and `request_id`.
  `TestUploadChunk_UnexpectedStatusRecordsNoBody` asserts all three (its
  server now sends a request ID). Not run red: without the three
  attributes the record's fields are empty, so the assertions fail by
  construction.
- The read-failure record's message is `failed to read response body`
  and its helper is `logBodyReadFailure`; the test constant, the doc
  comment, `graph-client.md` and the commit message follow.
- R-2.10.72 and `graph-client.md` say what the fragment record names.

Checks: `gofumpt -l` on the three Go files: no output.
`go test -count=1 ./internal/graph/`: ok. Tagged vet of graph,
driveops, sync and e2e: pass. Lint runs in the pre-PR gate.


## Merged as #921

- Squash-merged 2026-11-15 about 12:42 PST as `12a48bb2` on main.
- Gates: local pre-PR gate passed; PR CI and review-thread gate passed;
  post-merge stage passed ("merge, cleanup, and post-merge CI
  interpretation passed").
- Installed on this host at 12:50 PST (`onedrive-go --version` names
  `12a48bb2`).
- Live evidence, with a build of the commit on the test account: a
  path read OneDrive refuses with HTTP 400 prints the code word
  `invalidRequest` after the status.
- Still open: the bare 400 on a write that started the row has no
  recorded code, because it cannot be produced on demand. No retry
  rule is added until one occurrence is recorded with its code.
