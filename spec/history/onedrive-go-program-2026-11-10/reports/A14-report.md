# A14 report: a create whose answer was lost keeps its claim

Commit `0a9dac02` on `fix/create-answer-lost` (base `68ca8f3b`), 14 files, local only. Nothing pushed, installed or run live.

## What changed
- `internal/graph/errors.go`: new sentinel `ErrCreateOutcomeUnknown`.
- `internal/graph/item_create.go`: `CreateFile` wraps it (`%w`) on no response, a canceled or expired context, 5xx, 408, and a 2xx whose item could not be decoded or read back. 409, 429 and other 4xx never carry it.
- `internal/sync/executor_transfer.go`: `uploadNewFile` reads the prior intent before recording its own (`priorCreateIntent`) and keeps the intent after an unknown outcome. `resolveCreateConflict` takes `priorIntent`, returns `keepIntent`, and claims through `claimLostCreate`, `finishIntoPlaceholder` and `isEmptyLiveFile`.
- `internal/sync/store_pending_creates.go`: header and `ClearPendingCreateIntent` comments only. No schema change, no generation bump.
- Tests and fakes: `internal/graph/item_create_test.go`, `internal/sync/create_answer_lost_test.go` (new), `chaos_remote_test.go` (fault `create:response-lost`), `scenario_remote_test.go` (knob `loseCreateAnswers`), `dispatch_order_test.go` (scenario listed).
- Docs: `spec/requirements/transfers.md` (R-5.6.15), `spec/design/sync-execution.md`, `drive-transfers.md`, `sync-store.md`, `graph-client.md`.

## Tests added (all `// Validates: R-5.6.15`)
- `TestCreateFile_MarksOnlyAFailureThatMayHaveCreatedTheFile`: 13 cases; the 9 unknown ones failed before the fix, the 4 refusals passed.
- `TestUploadNewFile_CreateWhoseAnswerWasLostIsFinishedByTheRetry` (content, empty file): failed before (intent had been cleared).
- `TestUploadNewFile_ConflictOnAFileNotListedYetKeepsThePriorIntent`: failed before.
- `TestRunOnce_CreateWhoseAnswerWasLostFinishesIntoItsOwnEmptyFile`: failed before with the defect itself, a conflict copy of the user's file next to an empty one.
- `TestUploadNewFile_ConflictNeverClaimsAFileThatIsNotItsOwnEmptyCreate`: guard, passed before by design. Five cases: no intent, expired intent, content, another spelling, a folder. Two of them were added after the fix.
- `TestUploadNewFile_ClaimNeverOverwritesContentWrittenMeanwhile`: written after the fix. Content written after the listing gets no upload; written after the read, the If-Match refuses it.
- `TestChaosGraph_LostCreateAnswerLeavesTheFileAndNeverRunsTheHook`: checks the new fault. `TestModel_ChaosGraphConverges` passed before and after, so it does not show the defect.

## Checks run
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: clean.
- `./internal/graph/...` and `./internal/driveops/...`: pass.
- `internal/sync`, targeted: the new tests and their pending-create neighbours with `-race` (pass); `TestReverseDispatch` (pass, 178 s); `TestModel_ChaosGraphConverges` (pass, 146 s).
- Lint: the brief's full `devtool verify lint` overlapped the reviewer's gate, timed out after 22 minutes and reported one testifylint finding, now fixed. Then, as the coordinator instructed, the targeted run on `./internal/graph/... ./internal/sync/...`: `0 issues.` (`A14-lint.log`). Only `transfers.md` changed after it.
- Not run: `devtool verify quick`, so the repo rules (traceability, spec governs, test conventions) were checked by hand only: every cited test exists, no new production file, no banned test calls.

## Design point 4 (`doDocumentedGraphQuirkRetry`)
Inside one sync `CreateFile` call the only replays are the quirk loop after a 404 `itemNotFound` and the 401 token refresh. Both follow a definite refusal, and sync's transport retries nothing. So no 409 can follow an unknown outcome within one call, and nothing was needed. The CLI's retrying transport does resend the create and can get such a 409, but the CLI has no pending-create store; left alone.

## Deviations and decisions
- R-5.6.15 was not a free ID: it is the existing pending-create requirement. Amended in place.
- A zero-size local file is not finished without a write. The claim reuses `UploadFileToItem` (a conditional write of no bytes, then the mtime PATCH), because the lost create never set the modification time and this keeps the If-Match guard.
- The mark also covers failures after a 2xx (decode error, read-back failure, file not listed). The design listed only the request failures.
- `uploadNewFile` checks the unknown outcome before the conflict, so a 2xx whose read-back got 409 is unknown.
- The intent is now kept after an unknown outcome even when nothing landed, the same exposure a canceled attempt already had. Each attempt refreshes `intended_at`, so repeated attempts extend the 72 h window.
- On a 409, a failed store read or listing keeps the intent only when a prior intent existed. A failed `RecordPendingCreate` in the claim keeps it too.
- The safety rule holds as written: no prior intent, an expired one, content, a folder or another spelling claims and writes nothing.
- The chaos layer now marks its `before` faults on creates as `CreateFile` does, and its extra RNG draw changes fault trajectories.

## Recommendations (not done)
1. `driveops` may drop the mark where it maps `ErrUploadCompletedWithoutItem` (`settleUploadedItem`); check that path.
2. The item row of a claimed zero-byte file stays while the file stays empty, as it already does after an observation-side claim. A later remote mtime-only change is then absorbed into the baseline. Consider dropping the row once the upload commits.
3. `createOutcomeUnknown` also marks token and request-building errors (never sent). Harmless: the intent just expires.
4. A live e2e test of the lost answer needs fault injection on the wire; none was written.

## Unfinished
Nothing. For the reviewer to run: the full `internal/sync` package with `-race`, `devtool verify quick`, and the live e2e tier.
