# A15 report: a stale precondition is superseded, whatever status it carries

Commit `034f939d` (amends `4ba2405b`) on `fix/stale-precondition-first`, base `34fb1a66`, not pushed. 19 files.

## What changed
- `internal/sync/engine_result_classify.go`: the stale check runs right after shutdown, before `classifyHTTPResult` (it sat in `classifyLocalResult`).
- `internal/sync/engine_runtime_permissions.go`: `maybeHandlePermissionFailure` returns early for a superseded decision.
- `internal/sync/executor_transfer.go`: a failed store read, folder listing or claim record during a create's 409 is wrapped as its own error, not joined to the 409. `isRemoteTruthMovedOn` is shared by `uploadTransferError` and `uploadTransferFailureCapability` (remote-write capability). Comment fixes.
- `internal/sync/executor.go`: the folder create's `adoptExistingRemoteFolder` wraps a failed listing alone (was `errors.Join(conflictErr, err)`). Its stale comment and the undocumented fallback in `resolvedParentIDForOutcome` are fixed. No other join in the file or its siblings.
- `internal/driveops/transfer_verify.go` and `transfer_session.go`: new `uploadOutcomeUnknownError`. Its text names the answer and says the outcome is unknown; `Unwrap` returns only the lookup's error. It is used at the three sites (`settleUncertainFinalFragment`, and the final-fragment and resume lookups). Session records are untouched. The small-file site still names the item the create made (`CreatedItemError`).
- `internal/sync/engine_runtime_completion.go`: `applyFatalAuthEffects` drops its unused `ctx`. Nothing inside takes one (`config.MarkAccountAuthRequired` has no context). Callers at :95 and :155 updated, doc comment added.
- Specs: `sync-execution.md`, `sync-engine.md`, `error-model.md` (new boundary rule plus a Verified By row), `drive-transfers.md` (Session Lifecycle), and the R-5.6.12 text in `requirements/transfers.md`.
- Other `errors.Join` calls in driveops, left: `transfer_download.go:390, :394, :690` and `session_store.go:276`. They join local filesystem failures (mode, flush, partial removal, record deletes) with a close or with each other, never an upload answer with a follow-up read.

## Proof that nothing depended on the old order
Every producer of `errActionPreconditionChanged` drops its cause or wraps a local one (`ErrDetachRaced`), except `uploadTransferError`. Its 409 and 412 are meant to supersede, and its joins are now gone. Shutdown, a stalled transfer and a name collision still come first. Scope detection is gated by `RunScopeDetection`. The trial path already handled superseded first.

## Tests (each seen failing first, for the right reason)
- `TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt` (flipped): superseded, replanned in the same run, the collision found then, no row of its own.
- `TestClassifyResult_StalePreconditionIsSupersededWhateverStatusItCarries`: stale with 409 (both codes), 412, 401, 403, 404, 423, 429, 503 and 507 classifies as superseded.
- `TestRunOnce_OverwriteRefusedWithA412IsReplannedInTheSameRun` (in `runOnceScenarios()`): the 412 is superseded and replanned, both edits are kept, and no retry work is left.
- `TestEngineFlow_ProcessNormalDecision_StaleCompletionIsNeverProbedForPermissions`: a stale 403 or `os.ErrPermission` is not probed.
- `TestExecutor_Upload_ListingFailureAfterACreateConflictIsClassifiedByItsOwnAnswer` and `TestExecutor_CreateRemoteFolder_ListingFailureAfterAConflictIsClassifiedByItsOwnAnswer`: a 503 listing after the 409 is an outage, a 429 a throttle. Neither carries the 409 or reads as stale.
- `TestUploadTransferError_AFailedOutcomeLookupIsNotAStalePrecondition`: a final fragment's 409 or 412 followed by a 503 or 429 lookup, through the real TransferManager, gives the lookup's outage or throttle, not stale.
- driveops: `TestUploadFile_FailedLookupAfterAnUncertainFinalFragmentIsAnUnknownOutcome`, `TestSessionUpload_FailedLookupAfterAnUncertainFinalFragmentIsAnUnknownOutcome` and `TestSessionUpload_FailedLookupAfterAnUncertainResumeIsAnUnknownOutcome`. Each crosses 409/412 with 503/429. The error carries no `ErrConflict` or `ErrPreconditionFailed` and the lookup's status, it names the answer, it is not final-fragment-uncertain, and the session record is kept (sessions; the small-file path has none).
- Capability assertions added to `TestExecutor_Upload_CreateConflictIsAStalePrecondition` and `…OverwriteRaceWithARemoteEditIsAStalePrecondition`.

## Checks
- Targeted sync and driveops tests under `-race`: pass, as do the `ListsEvery|Table|Governs|Exhaustive|Registered` tests and reverse dispatch for both RunOnce tests.
- `go build ./...`, `go vet ./...` and `go vet -tags 'e2e e2e_full' ./e2e/...`: clean.
- Full non-race `go test -count=1 -timeout 60m ./internal/sync/... ./internal/driveops/...`: ok (sync 485 s, driveops 10 s).
- golangci-lint `./internal/sync/... ./internal/driveops/...` after the last change: `0 issues.` (`A15-lint.log`).

## Deviations (behaviour)
- Beyond the classifier order: the permission guard and the joins (now sync and driveops alike). A failed follow-up read is classified by its own answer.
- Remote stale answers carry remote-write capability.
- driveops: a session upload whose lookup failed is no longer looked up a second time by `settleUploadedItem`. The joined error used to carry the `FinalFragmentError`, which re-entered the uncertain path. In that path an empty placeholder is no longer removed on that same attempt; the next attempt resumes the kept record or looks again.

## Also fixed
- `executor_transfer.go`: the duplicated `uploadTransferError` comment, the "baseline eTag" preflight comment, and the `downloadOutcome` dropped stat (now documented).
- `executor.go`: the `adoptExistingRemoteFolder` comment called a differently spelled sibling stale; it is a durable collision. The `resolvedParentIDForOutcome` fallback is documented.
- `engine_result_classify.go`: the doc comment used old type names.
- `engine_runtime_completion.go`: `_ = ctx` is gone.
- `TestExecutor_Upload_CreateConflictIsAStalePrecondition` only passed through the join.

## Not fixed
- None found beyond the above.

## For the reviewer
- Please also run `internal/cli` tests: `put` goes through driveops, and the text of the unknown-outcome error changed. The e2e tests compile without changes, since no API changed.
- An open branch also adds a line to `runOnceScenarios()`.
