# H2 Definition of Ready: One Retry-After parser

Worktree `~/src/onedrive-go-wt/retry-after-parser`, branch
`fix/retry-after-parser`, base `main` at `68ca8f3b`.

1. PASS **Task intent**: one parser for the `Retry-After` field, in
   `internal/retry`, used by both callers that read it today
   (`retry.RetryTransport` and the graph client's three error builders).
   Product-visible: an HTTP-date `Retry-After` is honored (measured against the
   response's `Date`), a seconds value too long for a `time.Duration`
   saturates instead of wrapping to a negative or tiny wait, and surrounding
   whitespace is tolerated.
2. PASS **Non-goals**: no cap or ceiling on the wait (decision D3, R-2.10.7,
   R-2.10.14); no change to which statuses carry a usable `Retry-After` (429
   and 503), to who honors the value, to the sync engine's use of
   `GraphError.RetryAfter`, to the transport's throttle-gate logic, or to any
   fallback backoff. No injected clock is added to `RetryTransport` or
   `graph.Client`. No live or e2e runs.
3. PASS **Governing docs**: `spec/design/retry.md` (owns `internal/retry`,
   "RetryTransport" features list, trial timing); `spec/design/graph-client.md`
   ("RetryAfter Header", Transport-Layer Retry); `spec/reference/graph-api-quirks.md`
   (Throttling: names the two old parser functions);
   `spec/requirements/non-functional.md` R-6.8.1, R-6.8.6, R-6.8.8;
   `spec/requirements/sync.md` R-2.10.7, R-2.10.14; repo `CLAUDE.md` lines
   55 to 236; analysis `08-outer-packages.md` Item 3 (checked against code:
   the two bodies are identical as claimed; `graph.Client` has no clock).
4. PASS **Authority owner**: `internal/retry` owns the reading of the
   `Retry-After` field (new `retry_after.go`), as it already owns retry timing.
   `graph` keeps owning `GraphError.RetryAfter` population; `RetryTransport`
   keeps owning its throttle deadline. No durable state, no runtime lifecycle,
   no user-facing output changes.
5. PASS **Code scope**: new `internal/retry/retry_after.go` and
   `internal/retry/retry_after_test.go`; edits to `internal/retry/transport.go`,
   `internal/retry/transport_test.go`, `internal/retry/doc.go`,
   `internal/graph/client_preauth.go`, `internal/graph/upload_transfer.go`,
   `internal/graph/client_test.go`; docs listed in item 3. Not touched:
   `internal/sync`, `internal/driveops`, `internal/cli`, any other worktree.
6. PASS **Regression plan**: table test `TestResponseRetryAfter` in
   `internal/retry/retry_after_test.go` (seconds, spaces, zero, negative,
   garbage, overflow saturates, HTTP-date in the three RFC 9110 forms against
   `Date`, skewed local clock, `Date` missing or unparsable, past date, a year
   ahead, status gate), written and seen failing to compile before the parser
   exists. New rows in `TestRetryTransport_RetryAfterParsing` (HTTP-date,
   saturated seconds, spaces) and `TestTerminalError_RetryAfter` (HTTP-date,
   saturated seconds) prove each caller honors them.
7. PASS **Verification plan**: `go -C <wt> test -race -count=1 ./internal/retry/...`
   and `./internal/graph/...`; `go build ./...`; `go vet ./...`; formatting
   with gofumpt and goimports; `go run ./cmd/devtool verify lint` once before
   the commit when the lint slot is free. The reviewer runs the full verify
   and `internal/sync` / `internal/driveops` tests.
8. PASS **Data safety**: no filesystem, state DB or remote mutation paths
   change. The only behavior change is a longer, correct wait where the server
   asked for one by date, and a saturated rather than wrapped wait.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or
   credential-bearing logs are touched; tests use synthetic headers and
   `httptest` servers.
10. PASS **Live provider and destructive operations**: none. No live OneDrive
    calls, no state reset, no release artifacts, no history rewrite, no
    dependency change (stdlib `net/textproto` and `net/http` only).
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does a saturated wait (about 292 years) break any downstream
      arithmetic? Current assumption: `time.Time.Add` and `Duration.Round`
      saturate, `perf` sums milliseconds in `int64`; the sync store persists
      `NextTrialAt.UnixNano()`, which is undefined beyond 2262. Validation step:
      read the consumers (done); the store wrap is outside this brief and goes
      to the report as a recommendation. Owner document/code path affected if
      wrong: `spec/design/sync-store.md`, `internal/sync/store_write_block_scopes.go`.
    - Question: should `+5` (accepted today by `strconv.Atoi`) stay accepted?
      Current assumption: yes; `strconv.ParseInt` keeps it, so nothing outside
      the brief's listed changes moves. Validation step: a table row.
      Owner path if wrong: `internal/retry/retry_after.go`.
