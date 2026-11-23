# W56 Definition of Ready: a failed Graph answer names its error code

1. PASS **Task intent**: a failed Graph answer leaves its payload evidence. `GraphError.Error()` names the most specific code after the status (`graph: HTTP <status> <code> (request-id: <id>): <message>`, without the request-id part when there is none); with no code the text is unchanged. The debug record `request failed` gains `code`, `inner_codes`, `content_type`, `body_bytes`, `body_kind` (`graph_error`/`other`/`empty`). Codes are sanitized like the message. Docs: LI-20261115-01, LI-20261111-01 updated, the bare-400 quirk section generalized, a recurring family, graph-client.md.
2. PASS **Non-goals**: no retry rule, quirk, classification or outcome change in CLI or sync; a bare 400 stays as today; no e2e print/mask change; nothing in `internal/devtool` or `.github/workflows`.
3. PASS **Governing docs**: `spec/design/graph-client.md` (Error Handling, Design Constraints, line 793 "captured payload evidence"); `spec/reference/graph-api-quirks.md` ("A Folder Create Can Be Refused With A Bare 400"); `spec/reference/live-incidents.md` (rules, index, LI-20261111-01); `spec/reference/live-recurring-families.md`; requirements R-2.10.68, R-6.6.13, R-6.6.22, R-6.6.26, R-6.8.17, R-6.8.21. No requirement states the error text or the debug record (searched all of `spec/requirements/`); nothing to amend.
4. PASS **Authority owner**: `internal/graph` owns the wire-to-domain step: `buildGraphError` (`client_preauth.go`) builds and sanitizes every production `GraphError` (the only struct literal outside tests), `buildError` owns the one debug record, `errors.go` owns `Error()` and the code chain. User-facing output stays with `internal/cli/error_presentation.go`, which prints `Error()` only for failures it has no sentence for (a 400 is one).
5. PASS **Code scope**: `internal/graph/errors.go`, `client_preauth.go`, `redaction.go`, tests in `client_test.go`, `redaction_test.go`, new `error_evidence_test.go`; the four spec files above. Not touched: `internal/retry`, `internal/sync`, `internal/cli`, `e2e/`, `internal/devtool`.
6. PASS **Regression plan**: red first (seen failing 2026-11-15, logs `W56-red1.log`, `W56-red2.log`): `TestGraphError_ErrorString` (three code cases fail; the two no-code cases pin today's text and pass before and after), `TestBuildError_DebugLineRecordsWhatTheBodyHeld` (three bodies), `TestBuildGraphError_RedactsSecretsInCodes`. Kept as written now and passing today: `TestBuildError_DebugLineNamesTheRequest`.
7. PASS **Verification plan**: build, vet (plain and all tags), `go test ./internal/graph/...` plain and `-race` under the lock, `./internal/cli/... ./internal/driveops/... ./cmd/...`, untagged `./e2e/`, whole `./internal/sync/...` once under the lock, `RealRepoIsClean`, targeted lint last under the lock.
8. PASS **Data safety**: text and a Debug record only; no state, file or remote mutation; no classification read changes (every matched code is alphabetic, for which sanitizing is the identity).
9. PASS **Secret and credential safety**: codes now pass `sanitizeGraphErrorText` before printing or logging, closing a path where a code was logged raw (quirk retry evidence). Tests use made-up ids; no token file, `.env` or `.testdata` read.
10. PASS **Live provider and destructive operations**: none; no live test is run.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns**: Question: is any code Graph sends a JSON literal (a bare number) that the sanitizer's JSON path would re-render? Current assumption: matched codes are identifiers; a numeric code would be re-rendered exactly as a numeric message is today. Validation step: the first recorded code. Owner if wrong: `internal/graph/redaction.go`.

## Readers of the error text (`grep -rl "graph: HTTP"`, then per file)

| File | Matches | Changes |
| --- | --- | --- |
| `internal/graph/errors.go` | 2 (the format strings) | yes, the writer |
| `internal/devtool/bench_live.go` | 3, `Contains "graph: HTTP 50x"` | no: the prefix survives (out of scope anyway) |
| `internal/devtool/bench_live_test.go` | 2 fixtures | no |
| `e2e/e2e_test.go` | 3, `isRetryableGraphGatewayFailure` prefix match | no |
| `e2e/remote_read_rules_test.go` (untagged) | 6: one prefix match `"graph: HTTP 404"`, five fixtures | no |
| `e2e/sync_e2e_test.go` | 4 fixtures | no |
| `e2e/shared_items_helpers_test.go` | 3 fixtures | no |
| `e2e/quirk_helpers_test.go` | 1 fixture (`"error":"graph: HTTP 404"`) | no |
| `spec/reference/live-incidents.md` | 5 historical quotes | no (history) |
| `cmd/`, `docs/`, other `spec/` | 0 | - |

No product reader decides from `Error()` of a `GraphError`: CLI (`ls.go`, `status_live.go`, `error_presentation.go`) reads `StatusCode`, `Message` and sentinels; sync reads the typed error and only stores or logs the text. Every matcher is a `graph: HTTP <status>` prefix, which the new text keeps.

**Tagged e2e tests whose assertion reads the text**: `TestE2E_Shared_FolderNameDriveAdd_HonorsAccountFilter` (asserts through `waitForSharedFolderNameDriveAdd`); fixed-string tests `TestClassifyFixturePutCommandFailure_KnownConvergenceFamilies`, `TestClassifyFixturePutCommandFailure_RejectsUnrelatedFailures`, `TestClassifyFixtureMkdirCommandFailure_KnownConvergenceFamilies`, `TestClassifyFixtureMkdirCommandFailure_RejectsUnrelatedFailures`, `TestIsSharedFolderNameDiscoveryOmission`, `TestSharedFolderNameProviderOmittedFixtureRequiresIndependentProviderSignal`, `TestRecordCLIQuirkEventsParsesStructuredJSONLogs`. Branching (retry) readers, not assertions: `pollCLIWithConfigRetryingTransientGraphFailures`, the fixture `put`/`mkdir` classifiers, `waitForDriveListSharedSelectorVisible`, `sync_edge_cases_full_test.go:105`, `drive_list_full_test.go:71`.

**Debug record today** (`buildError`, `client_preauth.go:40`, and `TestBuildError_DebugLineNamesTheRequest`): `method`, `path` (never a pre-authenticated URL), `status`, `request_id`, `ags_diagnostic`; no code, no content type, no body size or shape, never the body or message. A read failure of the body is replaced by a placeholder message and its cause is logged nowhere. The retrying transport adds no record for a 400 (`isRetryable` excludes it, `transport.go:538`).

**Sync log of a failed action**: the executor's `action failed` (`executor.go:825`, `error` = the error value), `retry_work recorded` (`engine_runtime_completion.go:640`, `error` = `ErrMsg` = `Error()`, plus `http_status`, `condition_key`) and `sync retry work` (`engine_results.go:178`, `error`) all carry the full error text, so the code follows from the new text; no sync attribute is added.

**`sanitizeGraphErrorText`** (`redaction.go:29`): "" stays ""; a text that is a JSON document is decoded, values of sensitive keys become `[REDACTED]` and every string has bearer tokens and sensitive URLs redacted, then re-encoded; any other text gets `Bearer <token>` to `Bearer [REDACTED]`, quoted token pairs redacted and sensitive URLs to `[REDACTED_URL]`. A code is plain text, so the second path applies. `buildGraphError` will sanitize `Code` and each inner code, so `MostSpecificCode()` (printed by `Error()`), the record's `code`/`inner_codes` and quirk evidence all read the sanitized copy.
