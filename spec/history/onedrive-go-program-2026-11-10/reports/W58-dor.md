# Definition of Ready: every metadata body read is bounded, and a lost connection has a type

Worktree `~/src/onedrive-go-wt/metadata-body-reads-bounded`, branch
`fix/metadata-body-reads-bounded`, HEAD `e248aae5`, status clean but for the
untracked `.testdata` symlink.

1. PASS **Task intent**: no response body of a Graph client request waits
   without end: a body that brings no byte for the stall window (2 minutes,
   the transfer window) fails with a typed, retryable `graph.ErrResponseStalled`;
   a request or body read on an HTTP/2 connection closed for an unanswered
   ping fails with `graph.ErrConnectionLost`, matched by its text once, in
   `internal/graph`; the sync classifier reads both as a request OneDrive never
   answered. Riders: a requirement row for an inner `serviceReadOnly`, R-6.7.30
   says what an error body read does, `TestHandleChunkResponse_Unexpected2xx`
   goes through `UploadChunk`.
2. PASS **Non-goals**: transfer bodies (bounded by the stall watchdog, which
   keeps its arming); the sign-in client's requests (its `http.Client.Timeout`
   of 60 s, `graphtransport/profiles.go:31,81`, bounds the whole request, body
   included; nothing changes); the transports' numbers; `internal/cli`; the
   planner and executor; `delta.go`'s page loop and `items_fetch.go`'s listing
   loops (no line of either changes); no stored field, store generation, issue
   type, condition family, retry count, wait, outage window or command limit;
   no e2e test or live check (neither silence can be provoked on OneDrive).
3. PASS **Governing docs**: repo `CLAUDE.md` (routing table, conventions, DoR);
   `spec/design/graph-client.md` (Transfers `:355-393`, Response Body Bounds
   `:555`, Error Handling `:569-611`, Verified By `:82-99`);
   `spec/design/sync-engine.md:113` (the request-never-answered row) and
   `:1354-1361`; `spec/design/sync-domain.md` governs
   `internal/sync/domain_graph_error.go`; `spec/design/system.md:25`;
   `spec/requirements/non-functional.md` R-6.7.13 (`:123`), R-6.7.30 (`:140`),
   R-6.8.20 (`:167`), R-6.8.29 (`:175`).
4. PASS **Authority owner**: `internal/graph` owns the response body
   (`dispatchRequestWith`, `client_preauth.go:193`, the one place every
   response comes back), its silence bound and the meaning of a transport
   failure (the lost-connection type). `internal/sync`
   (`domain_graph_error.go`, `isRequestTimeout`) owns what a failure means for
   the work; it reads the two types only. No durable state, no runtime owner
   changes; user-facing output is unchanged in code.
5. PASS **Code scope**: `internal/graph/response_body.go` (body wrapper, the
   two sentinels, the one text match), `client_preauth.go` (the wrap and the
   round-trip typing), `transfer_watchdog.go` (the one window constant, the
   shared stall explanation, a watched body hands its bound to the watchdog),
   `client.go` (the window seam's name); `internal/sync/domain_graph_error.go`;
   tests in both packages; the spec lines above. Not touched: `delta.go`,
   `items_fetch.go`, `download.go`, `upload_transfer.go`, `internal/cli`,
   `internal/graphtransport`.
6. PASS **Regression plan** (each seen failing first): a metadata body that
   sends headers and then nothing fails within the window with
   `ErrResponseStalled` (fails today: it waits for its context); a body that
   sends one byte at a time with gaps below the window, longer in total than
   the window, completes; a stalled error body returns its `*GraphError` and a
   stalled drain `ErrResponseStalled`, both within the window; a raw HTTP/2
   server that answers no ping produces the lost connection from the real
   client on the round trip and on a body read, both carrying
   `ErrConnectionLost` (these two pin the library's text: they fail when Go
   rewords it); a body the transfer watchdog reads arms no bound of its own; in
   `internal/sync` the timed-out-requests table gains a stalled body, a lost
   connection on the round trip and on a body read, built from the types.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, vet with
   `e2e,e2e_full,integration,stress`; `go test -count=1
   ./internal/graph/... ./internal/graphtransport/...` and with `-race` behind
   the lock; `./internal/sync/...` behind the lock, then `./internal/sync/`
   with `-race` behind the lock; lint over the changed packages last, behind
   the lock, into `W58-lint.log`; `RealRepoIsClean` (4 PASS); every cited test
   name checked by hand.
8. PASS **Data safety**: no local or remote write path changes. A stalled body
   fails a read that would otherwise hang; a download's body keeps the watchdog
   alone, so a stalled download still keeps its partial
   (`driveops/transfer_download.go:677` reads `ErrTransferStalled`). No store
   access changes.
9. PASS **Secret and credential safety**: no tokens, `.env` or account names;
   test servers are loopback, drive and item IDs invented.
10. PASS **Live provider and destructive operations**: none; fake servers only.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does closing a response body from the timer goroutine end a
      read blocked in it, for both protocols? Current assumption: yes (HTTP/1
      `bodyEOFSignal.Close` takes the early-close path, which ends the read
      loop and closes the connection, `transport.go:2417-2420,2456-2470` of
      go1.26.11; HTTP/2 `transportResponseBody.Close` breaks the pipe,
      `h2_bundle.go:10047-10052`). Validation step: the stalled-body tests run
      over `httptest` HTTP/1 and the raw HTTP/2 server. Owner if wrong:
      `internal/graph/response_body.go`.
    - Question: can a test body that is not net/http's be bounded? Current
      assumption: only a body whose Close ends a pending Read is; every
      production body is net/http's. Validation step: none needed beyond the
      above. Owner if wrong: same file.

## The DoR questions of the source row

(i) **Headers, then nothing.** A probe (`GetItem` against `httptest` that
writes 200 and flushes, then blocks; context 300 ms) returned at 301 ms with
`graph: decoding item response: graph: reading the response: context deadline
exceeded` (`*ResponseReadError`, `context.DeadlineExceeded`). The read is in
`items_fetch.go:27`; the only bound is the caller's context. With no context
deadline nothing ends it on HTTP/1.1; on HTTP/2 the health check ends it after
`http2SendPingTimeout` + `http2PingTimeout` (30 s + 15 s,
`graphtransport/profiles.go:24-25`) only when the server also stops answering
pings; a server that answers pings and sends no data holds it forever.

(ii) **Every body read in `internal/graph`** (31 lines of the grep, not 30;
"wrap" = covered by one wrap at `dispatchRequestWith`, `client_preauth.go:211`):

| file:line | reads | bounded today by | one wrap covers it |
| --- | --- | --- | --- |
| client_preauth.go:26 | error body (`buildError`), 64 KiB `LimitReader` | context only | yes |
| delta.go:76 | decoded delta page | context only | yes |
| drives_identity.go:24, :120, :199, :229, :259 | decoded `Me`, drives page, `PrimaryDrive`, `Drive`, `Organization` | context only | yes |
| drives_shared.go:55 | decoded search page | context only | yes |
| drives_sites.go:72 | decoded sites page | context only | yes |
| items_copy.go:71 | drain of the copy POST answer | context only | yes |
| items_copy.go:117 | decoded copy monitor status (pre-auth) | context only | yes |
| items_fetch.go:27, :410 | decoded item, children page | context only | yes |
| items_mutation.go:53, :307 | buffered item body, 1 MiB `LimitReader` | context only | yes |
| items_mutation.go:233 | decoded moved item | context only | yes |
| items_mutation.go:417 | drain of a delete answer | context only | yes |
| items_permissions.go:295, :344 | decoded permissions page, restored item | context only | yes |
| shares.go:35 | decoded shared item | context only | yes |
| socketio.go:34 | decoded Socket.IO endpoint | context only | yes |
| upload_session.go:97 | drain of a session cancel (pre-auth) | context only | yes |
| upload_session.go:142, :272 | decoded session status, session create | context only | yes |
| upload_transfer.go:109 | drain of a fragment's 202 (pre-auth) | context only (the watchdog disarmed at the request body's end) | yes |
| upload_transfer.go:140 | an unexpected 2xx fragment body, 64 KiB | context only | yes |
| upload_transfer.go:235 | simple upload error body, 64 KiB | context only | yes |
| download.go:270, :393, :475 | transfer copies through `watchdog.reader` | stall watchdog (`transfer_watchdog.go:133`) | wrapped, but the watchdog alone bounds them (the body hands its bound over) |

(iii) **A body that may legitimately stay silent longer than the window**:
none. No Graph request of the client is a long poll; the notification channel
is a websocket the sync package dials itself (`internal/sync/socketio_conn.go:35`,
`coder/websocket`), never through `graph.Client`; `SocketIOEndpoint`
(`socketio.go:26`) is an ordinary small GET. No exemption by name is needed.

(iv) **The lost connection.** Yes, from the real client, no sleep, about
30 ms: a TLS listener with `NextProtos: h2` that reads the preface, sends an
empty SETTINGS frame and never answers PING, with a test transport
`HTTP2: {SendPingTimeout: 5ms, PingTimeout: 5ms}`. Round trip: `dispatching
request: Get "https://127.0.0.1:…": http2: client connection lost`, innermost
`*errors.errorString`, inside `*url.Error`. Body read (the server also sends
response HEADERS `:status 200`): `graph: decoding item response: graph:
reading the response: http2: client connection lost`, `*ResponseReadError`
around the bare `*errors.errorString`, with no `*url.Error`: the claim from
reading holds (`closeForLostPing`, `h2_bundle.go:8673-8678`; the stream's
abort error reaches the body pipe at `h2_bundle.go:9138`), and the classifier's
text match (`domain_graph_error.go:207`, `*url.Error` only) misses it today.
The text is pinned by these two tests: they fail if Go rewords it.

(v) **What commands do today.** A one-shot command has no deadline of its own
(only the signal context, `internal/cli/signal.go`): a stalled metadata body
holds it until the user stops it (HTTP/1.1, or HTTP/2 with pings answered); a
lost connection fails it at once, and the CLI says the connection to OneDrive
was interrupted (`error_presentation.go:164-173` matches `http2: ` text).
`status` ends either within its 10 s budget (`status_live.go:25,66`), as "did
not answer within 10 s". A one-shot or watch sync has no deadline: a stalled
metadata body holds the run forever; a lost connection on the round trip is a
request timeout (W48's text match, `domain_graph_error.go:207`), one on a body
read is not and falls through to `classifyLocalResult`
(`engine_result_classify.go:110`).

(vi) **The window and the transfer stall.** `defaultTransferStallTimeout = 2 *
time.Minute` (`transfer_watchdog.go:26`), read through
`Client.transferStallTimeout` (`client.go:132`, zero means the default,
`transfer_watchdog.go:59-62`); tests shorten the field. A stalled transfer
returns `ErrTransferStalled` (`transfer_watchdog.go:22`) as `explain` builds it
(`:157`, the inner error as text only); `isRequestTimeout` reads it with
`errors.Is` (`domain_graph_error.go:182`): a request timeout, outage evidence
like a 5xx, issue `request_timeout`.

**Design taken (decision (a)).** The watchdog does not fit a metadata read: it
bounds by canceling a request context that must exist before the request is
built, while the one place responses come back sees the request after it left,
and its attempt bookkeeping serves retry waits inside a transfer, which a
metadata body read (after the transport returned) never has. So a small body
wrapper beside it: each Read arms a timer for the window, its return stops it;
a timer that fires closes the body, and the read fails with
`ErrResponseStalled`, explained the way a transfer stall is (one shared
function, inner error as text). One constant for both windows. A body handed
to `stallWatchdog.reader` (the three download copies) arms no timer of its
own, so a stalled download stays `ErrTransferStalled` and keeps its partial.
