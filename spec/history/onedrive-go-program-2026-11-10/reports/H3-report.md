# H3 report: 401 retry with a body that cannot be rewound

Commit `f2dafabb` on `fix/rewind-body-loud` (base `68ca8f3b`), 5 files, +150/-46.

## What changed
- `internal/graph/client_auth.go`: new sentinel `errBodyNotReplayable` ("graph: request body cannot be replayed"). `rewindBody` returns it for a body that is non-nil, not `http.NoBody` and not an `io.Seeker`; before, it returned nil and the body went out consumed. `retryUnauthorized` wraps the error as "rewinding request body after 401: %w" and now rewinds before the Info log and before building the retry, so no second request goes out. The Seek-failure text is now "seeking request body: ..." (it used to carry a second `graph:` prefix once doRequestVia wrapped it). `doOnce` doc comment updated.
- `internal/graph/client_test.go`: new `TestDo_401WithNonSeekableBodyFailsLoudly`; `TestDoOnce_401_RefreshSucceeds` is table-driven (nil body, `http.NoBody`); `TestRewindBody_SeekError` expects the new Seek text.
- `internal/graph/quirks_test.go`: `TestRewindBody_NilBody` became `TestRewindBody_EmptyBodiesNeedNoRewind` (nil and `http.NoBody`); `TestRewindBody_NonSeeker` (pinned the silent no-op) became `TestRewindBody_NonSeekerIsNotReplayable`; `TestRewindBody_FailOnSecondSeek` expects the new Seek text.
- `spec/design/graph-client.md`: "401 Token Refresh" gets a paragraph on the body rewind and the loud failure, with test names; new Verified By row. GOVERNS unchanged (no new files).
- `spec/requirements/non-functional.md`: R-6.8.14 gets one sentence: the retried request carries the complete original body, and a body that cannot be rewound fails with an error that says so, with no second request.

## Tests and what each proves
- `TestDo_401WithNonSeekableBodyFailsLoudly` (R-6.8.14): an `io.MultiReader` body (asserted not an `io.Seeker`) gets a 401; the call fails with `errors.Is(err, errBodyNotReplayable)`, the error names `PATCH /items/item-1` and "after 401", and the server saw exactly one request. Seen failing before the fix: the retry went out with an empty body, the server answered 200 and the call returned no error.
- `TestDoOnce_401_RefreshSucceeds` / `http.NoBody` (new case): a body-less POST with `http.NoBody` (the shape `items_permissions.go:325` sends) still refreshes and retries; guards the fix against treating `http.NoBody` as unreplayable. The nil-body case is the original test.
- `TestRewindBody_EmptyBodiesNeedNoRewind`, `TestRewindBody_NonSeekerIsNotReplayable`: unit-level contract of `rewindBody`.
- Seekable body resent in full: existing `TestDoOnce_401_RefreshSucceedsWithBodyAndQueryPreserved` (bytes.Reader; without the Seek the GetBody snapshot NewRequest takes would be empty), cited, not duplicated.

## Checks run
- `go test -race -count=1 -timeout=20m ./internal/graph/...`: ok (41.9 s).
- `go build ./...`: ok. `go vet ./...`: ok.
- `golangci-lint run --allow-parallel-runners ./internal/graph/...` (GOTOOLCHAIN=go1.26.11): 0 issues (`H3-lint.log`).
- gofumpt and goimports (local prefix) on the three Go files: no changes needed after edit.

## Verification of the analysis (Item 8)
- Confirmed: graph's `rewindBody` returned nil for a non-seekable body and `retryUnauthorized` resent it. All production `c.do`/`doRequest`/`doRequestVia` callers pass nil, `http.NoBody` or `bytes.NewReader` (grep of non-test graph files), so no production caller is affected and none was rewritten.
- Confirmed: the retry transport is already loud (`requestBodyReplayable`, `rewindBody`, `errBodyNotReplayable` in `internal/retry/transport.go:454-506`); `doPreAuth` sets GetBody from makeReq; `doRawUpload` sets ContentLength and GetBody and does not go through `doOnce`. No other place in `internal/graph` or `internal/retry` resends a body.
- The analysis did not mention that `http.NoBody` is not an `io.Seeker`: a fix that only checked for nil would have broken the body-less POST in `items_permissions.go`. Handled and tested.

## Deviations
- The retry package's helper and sentinel are not reused: it rewinds an `*http.Request` (GetBody, else Seek on `req.Body`), while the 401 retry builds a new request from the caller's `io.Reader`, and `http.NewRequest` wraps a non-bytes seekable reader in `io.NopCloser`, hiding Seek. Graph keeps its own unexported sentinel with the same name and wording. No new exported API.
- The not-replayable 401 now surfaces as a plain wrapped error, not a `*GraphError`/`ErrUnauthorized`: it is a programmer error, and the brief asked for an error that names the problem. Only reachable by a caller that passes a non-seekable body.
- No files outside the brief's scope were touched (none of `client_preauth.go`, `upload_transfer.go`, `errors.go`, `retry_after.go`, `auth*.go`, `client_construction.go`).

## Recommendations (not done)
1. The analysis's stronger alternative: make `do`/`doRequest`/`doRequestVia` take `[]byte` (or build the retry from the first request's GetBody) so a non-rewindable body cannot be written at all.
2. `bytes.Reader.Seek(0, SeekStart)` rewinds to the slice start, not the reader's position at the call; harmless today (callers pass fresh readers), but a GetBody-based retry would be exact.

## Unfinished
- Nothing.

## Packages the reviewer should test
- `./internal/graph/...` was run. No other package's code changed; `internal/retry` untouched. Callers that might branch on the old silent behavior: none found (`internal/driveops`, `internal/sync` only see a new error on a path no caller reaches).
