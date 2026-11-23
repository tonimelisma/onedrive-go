# H2 report: One Retry-After parser

Commit `756f12bb` on `fix/retry-after-parser` (base `68ca8f3b`), 13 files.

## What changed
- `internal/retry/retry_after.go` (new): `ResponseRetryAfter(resp, now)` (429/503 gate, `Date` as reference, `now` only when `Date` is missing or unparsable) and unexported `parseRetryAfter` / `delaySeconds` (trim, seconds or HTTP-date, saturate at `math.MaxInt64` ns, no cap).
- `internal/retry/transport.go`: `retryBackoff` reads one `time.Now()` and calls `ResponseRetryAfter`; `parseRetryAfterHeader` deleted.
- `internal/graph/client_preauth.go`: both error builders call `retry.ResponseRetryAfter(resp, time.Now())`; `parseRetryAfter` and the `strconv` import deleted.
- `internal/graph/upload_transfer.go`: raw-upload error path uses the same call.
- `internal/graph/errors.go`: `GraphError.RetryAfter` comment names the reader and both forms.
- `internal/retry/doc.go`: package doc lists `ResponseRetryAfter`.
- `spec/design/retry.md`: GOVERNS adds `retry_after.go`; Implements adds R-6.8.6; new section "Reading `Retry-After`"; Verified By row; features and trial-timing lines.
- `spec/design/graph-client.md`: "RetryAfter Header" names the reader and the test.
- `spec/reference/graph-api-quirks.md`: names `retry.ResponseRetryAfter` instead of the two deleted functions.
- `spec/requirements/non-functional.md`: R-6.8.6 states both forms, the `Date` reference, saturation, no cap. R-2.10.7 and R-2.10.14 already say "no cap" and speak of a duration, so they are unchanged.

## Tests
- `TestResponseRetryAfter` (new, 26 rows, fixed local clock 10 min ahead of `Date`): seconds, trimmed spaces, `+5` still read, zero/negative/garbage/fraction/absent/blank give 0, largest representable seconds exact, `9223372037` and `99999999999999999999` saturate, a negative beyond int64 gives 0, IMF/RFC 850/asctime dates give 90 s against `Date`, `Date` beats the skewed local clock, missing and unparsable `Date` fall back to `now`, past/equal date give 0, a year ahead is honored uncapped, year 9999 saturates, 503 honored, 200 and 500 ignored. Seen failing to compile before the parser existed.
- `TestRetryTransport_RetryAfterParsing` (+3 rows: spaces, HTTP-date against `Date`, saturated seconds): the transport sleeps exactly what the server asked.
- `TestTerminalError_RetryAfter` (+2 rows): `GraphError.RetryAfter` is 90 s for an HTTP-date against `Date`, and saturates for `9223372037`. Before the change these failed with `0s` and `-2562047h47m16.7s` (the wrap).

## Checks run
- `go test -race -count=1 ./internal/retry/...`: pass.
- `go test -race -count=1 ./internal/graph/...`: pass (44.6 s).
- `go build ./...`, `go vet ./...`: clean. gofumpt and goimports (local prefix) applied.
- `go run ./cmd/devtool verify lint`: pass, 0 issues (log `reports/H2-lint.log`).

## Deviations
- `ParseRetryAfter` is not exported: no caller outside `internal/retry` needs the string form (all three graph sites hold a response), and the repo rule is unexported by default. `ResponseRetryAfter` is the exported API.
- No injected clock was added to `RetryTransport` or `graph.Client` (the analysis proposed one): D3 measures an HTTP-date against `Date`, which makes the caller tests deterministic, and the `now` fallback is covered by the table test. Callers pass `time.Now()`.
- The analysis' one-hour ceiling and `Clamped` flag are not implemented, per D3.

## Recommendations (not done)
1. `internal/sync/store_write_block_scopes.go` persists `NextTrialAt.UnixNano()`, undefined after year 2262: a saturated (or any wait over about 236 years) `Retry-After` stores a wrapped time, and after a restart the trial is due at once. In memory the timing is right. Saturate or reject such times at the store boundary.
2. Same for any other `UnixNano` of `now.Add(retryAfter)` in sync (only this store write was checked).
3. `RetryTransport` would sleep the full saturated wait; that follows D3, but a Warn naming the absurd value may help a person who sees the command hang.

## Unfinished
Nothing in scope.

## Packages the reviewer should test
`internal/sync` (consumes `GraphError.RetryAfter`; unchanged code, but the values for HTTP-date and huge inputs differ), `internal/driveops` and `internal/cli` (build `RetryTransport`), and the repository rules (`verify quick`: GOVERNS, cited tests, requirement traceability), which `verify lint` does not run.
