# Track 3 — Trust boundaries and untrusted input

Repo: ~/src/onedrive-go at 26b3bc8b. Read-only review; no tests run. Bash was unavailable in this session, so all tracing was done by reading files and by grep sweeps delegated to read-only helpers; every claim below cites the file:line it was read from.

## 1. Per area: what was traced and the verdict

### 1.1 Graph client — pagination and delta expiry — HOLDS

Traced:
- `internal/graph/delta.go:54-101` (one page), `:156-210` (`deltaAllPages`, 10,000-page cap at `client.go:56`, fail-closed when a page has neither nextLink nor deltaLink).
- `internal/graph/items_fetch.go:38-74` (`fetchAllChildren`), `:271-333` (`listChildrenPage` follows `lcr.NextLink`), `:338-344` (`stripBaseURL` refuses a nextLink that does not start with the configured base URL — the bearer can never be sent to another host by a hostile link).
- `internal/graph/drives_identity.go:78-107` (`/me/drives`, `/sites/{id}/drives` follow nextLink), `internal/graph/drives_shared.go:16-78` (search follows nextLink), `internal/graph/items_permissions.go:266-279` (recycle bin via `fetchAllChildren`).
- NOT paginated: `SearchSites` (`drives_sites.go:16-40`, response struct `drives.go:96-98` has no NextLink) and `ListItemPermissions` (`items_permissions.go:243-263`, `listPermissionsResponse` has only `Value`). See finding 3-3.
- 410: `internal/sync/observer_remote.go:189-287` buffers the whole delta window and classifies it only when the DeltaLink page arrives (`:249-272`); any page error, 410 included (`:825-826`), returns `nil, ""` — nothing partial is emitted and no token is saved. Recovery is a fresh enumeration with an empty token (`engine_run_once.go:445-480`), orphans are confirmed per item (`:474`), and the 410 flavour decides whether a confirmed orphan is a delete (`errors.go:49-69`, `resyncKindUnknown` is treated as "never delete"). In watch mode a failed recovery keeps the expired token and backs off (`observer_remote.go:497-500`) instead of resetting. A partial walk can therefore never produce deletes. Verdict: HOLDS, matches R-2.15.3.

### 1.2 Graph client — response body bounds and decoding — HOLDS WITH CAVEAT

Traced:
- Every error body is `io.LimitReader(…, 64 KiB)`: `client_preauth.go:19`, `:97`, `upload_transfer.go:135`, `:205`.
- Success bodies buffered whole are capped at 1 MiB + 1 and an oversized body is an error, not a truncation: `items_mutation.go:53-60`, `:305-312` (`decodeItemBody`, used by create file, simple upload, final chunk, fileSystemInfo PATCH).
- Everything else is a streaming `json.NewDecoder` into fixed structs (delta `delta.go:76`, children `items_fetch.go:287`, move `items_mutation.go:233`, copy status `items_copy.go:115`, upload session `upload_session.go:133,241`, socket.io `socketio.go:33`, drives/me/org `drives_identity.go:24,119,198,228,258`, permissions `items_permissions.go:258`, restore `:301`, shares `shares.go:34`).
- Decoding: nested pointers are nil-checked (`items.go:198-229`, `:279-294`), timestamps validated and left zero when invalid (`items.go:320-355`), `TypeFacetMissing` instead of defaulting to "file" (`items.go:178`), dedup keeps the last occurrence and deletions are partitioned per parent (`normalize.go:54-172`). Content-Length is never trusted for correctness: `download.go:327` uses it only as corroborating evidence for hashless items; integrity is the QuickXorHash or the size of the bytes actually written (`driveops/transfer_download.go:143-156`).
- Caveat: a streaming decoder still buffers one JSON token, so a hostile page with a multi-hundred-MB string field is not bounded, and the sync remote observer deliberately buffers the whole delta window in memory (`observer_remote.go:221-234`, R-2.15.4). With 129k items each carrying a long `@microsoft.graph.downloadUrl`, the initial enumeration will hold a window in the low hundreds of MB. Not a trust failure, but R-6.7.30 is satisfied only for buffered-whole bodies. See finding 3-4.
- `Item.Size` is never range-checked (a negative `size` from the API flows into `Item.Size`); the download code guards with `size > 0` (`download.go:300,327`). Out of this track's scope for planner consequences.

### 1.3 Graph client — retry idempotency — HOLDS

Traced `internal/retry/transport.go`:
- Replayable methods are GET/HEAD/OPTIONS/TRACE/PUT/DELETE (`:334-346`); POST and PATCH are returned after one attempt unless the context carries `WithReplaySafe` (`:326-332`), which only `CreateFolder` (`items_mutation.go:45`) and `CreateFile` (`item_create.go:78`) set, both with `conflictBehavior=fail` so a replayed committed create answers 409. `CopyItem` (`items_copy.go:64`), `PermanentDeleteItem` (POST, `items_mutation.go:381`), `CreateUploadSessionForItem` (`upload_session.go:47`), `RestoreItem` and every PATCH are not retried by the transport.
- A body that is neither `GetBody`-able nor seekable is never resent (`:465-473`, `:480-506`); the raw PUT upload sets both `ContentLength` and `GetBody` over a fresh `SectionReader` (`upload_transfer.go:169-176`); chunk PUTs get `GetBody` from `doPreAuth` (`client_preauth.go:67-76`).
- 401 refresh in `client_auth.go:226-297` rewinds a `bytes.Reader` body (`:280`, `:302-314`) and resends once; forced refreshes are rate-limited to one per 30 s (`auth_token.go:145,186`).
- Retry-After is honoured for 429/503 (`transport.go:402-422`, integer seconds only — an HTTP-date value falls back to computed backoff), backoff is 1 s base, 60 s cap, ±25 % jitter, 6 attempts (`named.go:8,33,42-50`); no total time budget beyond the caller context. Sync workers use a non-retrying transport (`graphtransport/profiles.go:66-77`).
Verdict: HOLDS. Documented in `spec/design/retry.md:78-90` and matches code.

### 1.4 Token handling — HOLDS WITH CAVEAT

Traced:
- Token file: `tokenfile.Save` → `fsroot.AtomicWrite` with temp in the same dir, chmod 0600 before write, fsync, rename, dir fsync (`tokenfile.go:76-96`, `fsroot.go:432-475`, `:500-518`); directory 0700 (`tokenfile.go:26`, `cli/auth_login.go:29`). Load rejects unknown fields (`tokenfile.go:55-56`).
- Logs: `tokenBridge.Token` logs only expiry/validity (`auth_token.go:221-224`); request logs carry method/url/status only (`client_auth.go:124-157`); `TestDo_DebugLogsNeverExposeBearerToken` exists (`client_test.go:555`). `GraphError.Message/RawBody` are sanitised (`redaction.go:17-30`); `*url.Error` from pre-auth dispatch is scrubbed (`redaction.go:172-186`). `DownloadURL`, `UploadURL`, `SocketIONotificationURL` implement `slog.LogValuer` (`types.go:17,28,38`) but have no `String()`/`MarshalJSON`, so `%s`/`%v`/JSON print them raw — nothing in production does so today (sweep for `fmt.Errorf`/`Sprintf`/`slog` with those identifiers found nothing in `internal/sync` or `internal/driveops`).
- Persisted upload-session URLs: `driveops/session_store.go:49` stores `SessionURL` as a plain string in a 0600 file under a 0700 dir (`:26,29`). Acceptable.
- Perf capture bundle (`perf/capture.go`): pprof CPU/heap/block/mutex/goroutine + optional trace, 0600 in a 0700 dir; profiles carry stacks, not heap contents. No secrets by construction.
- Device-code login: the verification URI and user code go to stderr via `cc.Status()` (`cli/auth_flow.go:299-300`, `format.go:29`), success text to stdout or discarded under `--json`.
- OnTokenChange fork: `golang.org/x/oauth2` replaced by `github.com/tonimelisma/oauth2 v0.0.0-20260209…` (`go.mod:42`). `reuseTokenSource.Token` refreshes under its mutex and calls `notify` outside it (`oauth2.go:321-341`), so one process makes one refresh per expiry. Caveat: there is no cross-process coordination — a CLI command and the watch daemon each hold the refresh token in memory and each writes the file on its own refresh (last writer wins). This is benign as long as Microsoft keeps the previous refresh token valid after rotation (true for MSA and default Entra policy); it would become a lock-out under single-use refresh-token policies. PLAUSIBLE, low.
- Caveat: `graph-client.md:586-588` acknowledges the fork is unreviewed credential code depending on a personal repo; govulncheck is the compensating control.

### 1.5 TLS and HTTP client — HOLDS

`graphtransport/profiles.go:108-136`: `http.DefaultTransport.Clone()` (so `ProxyFromEnvironment` and the system root pool are kept), dial 30 s, TLS handshake 15 s, response-header 2 min, TCP keepalive, HTTP/2 ping 30 s/15 s, `http.Client.Timeout = 0`; stall watchdog 2 min on transfer bodies (`transfer_watchdog.go:24`). Repo-wide grep for `InsecureSkipVerify|RootCAs|tls.Config` returned nothing. Only `requestContent` overrides `CheckRedirect` (to not follow the /content 302, `download.go:479-482`); every other client follows redirects by default — relevant to finding 3-1.

### 1.6 Path and name handling — HOLDS (names) / HOLDS (containment) / GAP (pre-auth download host)

Names from the API:
- `internal/sync/item_converter.go:1153-1165`: `.`, `..`, any `/`, NUL, invalid UTF-8 are rejected with a Warn (`:573-590`); `parentReference.path` segments get the same check before `path.Join` (`:931-937`); an empty name on a live item without a baseline entry is skipped (`:510-518`); an empty item ID is skipped (`:490-498`). Backslash is a legal Linux filename byte and is passed through (correct on ext4). Names are never URL-decoded (`normalize.go:14-18`, `items.go:243-268`, R-6.7.8/R-6.7.23) — the earlier `%XX` decoding bug is fixed and `grep PathUnescape|QueryUnescape` in non-test code is empty. NFC/NFD and case/ignored-character equivalence are handled by `namekey.Key` (`namekey.go:57-91`) and `synctree/spelling.go`; the local-collision observer uses the same key (`observer_local_collisions.go:112-116`).
- URL building: every path-addressed segment goes through `url.PathEscape` (`items.go:44-51`, `items_fetch.go:139-171`), item IDs too (`items_fetch.go:144`, `download.go:484`); search queries use `url.QueryEscape`; file creates never put the name in a URL (`item_create.go:18-25`).

Root confinement (`internal/synctree/synctree.go`):
- Every operation opens `os.OpenRoot(dir)` fresh (`:152-162`, `:366`, `:475`, `:978`) and resolves a cleaned relative name inside it; absolute paths and `..` are refused with `ErrUnsafePath` (`:297-314`); rename resolves both ends in the root (`:968-1003`); `normalizeNotExist` refuses to turn a containment failure into `ErrNotExist` (`:1115-1144`); no-follow variants exist for mkdir/remove/tree validation (`:839-914`, `:781-827`). Hard links are not special-cased (a hard link inside the root is an ordinary file, which is the right answer). Residual `os.Rename/os.Remove` outside the root are only in `detach_unix.go` (copy-out of a detached mount, into a destination the caller supplied) and `localpath` (explicitly uncontained by design).
- Local trash restore (`localtrash.go:339-370`): refuses ids with separators, rejects `..`/absolute in the stored relative path (`:504-514`), refuses symlinked ancestors (`:480-500`), restores with rename-noreplace. `entry.SyncRoot` is read from the JSON record (0700 dir, same user) and the symlink check is a TOCTOU against the subsequent rename; both are only exploitable by the owning user against themselves. Low, informational.

GAP: the pre-authenticated download URL is never validated — finding 3-1.

### 1.7 Config and CLI input — HOLDS

- Unknown TOML keys are errors in strict mode and warnings in lenient mode (`config/unknown.go:79-127`, `decoder.go:146-153`); drive sections are decoded from primitives so type errors name the line (`decoder.go:52-63`).
- Sizes: `ParseSize` SI/IEC with non-negative check (`size.go:28-90`; a float such as `1.5MB` is accepted). Durations: `time.ParseDuration` with a minimum (`validate.go:60-71`). `paused_until` must be RFC3339 and a corrupt value keeps the drive paused (`validate_drive.go:47-62`).
- `~` expansion only for a leading `~/` (`drive.go:394-411`); after expansion `sync_dir` must be absolute (`resolved_validator.go:48-49`, `:71-73`; `cli/sync_dir.go:19-20`). Filter paths must be root-relative, normalised, no `..` (`validate_drive.go:74-122`).
- Env overrides are limited to `ONEDRIVE_GO_CONFIG` and `ONEDRIVE_GO_DRIVE` (`env.go:10-13`), logged only as set/unset.
- `--verbose/--debug/--quiet` are mutually exclusive (`root.go:495`); console default NOTICE, `--debug` DEBUG (`root.go:799-815`); file level from config with a warning on unknown values (`:817-830`). `--json` discards the human success text (`json_output.go:31-37`). Exit codes were not traced in depth in this track.
- Browser login: `exec.CommandContext(ctx, "xdg-open"|"open", validatedURL)` with the URL host-validated first (`cli/auth_login.go:103-123`, `:136-160`), no shell.

### 1.8 Control socket — HOLDS

`internal/multisync/control_socket.go`: directory created 0700 (`:25,90`) and then checked for other-write-without-sticky and for ownership by this uid or root (`:175-202`, covering the predictable `$TMPDIR/odgo-*` fallback); owner `flock` on `<socket>.lock` with inode re-check (`owner_lock.go:45-91`); socket chmod 0600 right after bind (`:106`, there is a bind→chmod window but the 0700 directory closes it); `ReadHeaderTimeout` 5 s (`:117`); perf-capture body capped at 64 KiB (`:387`). No `SO_PEERCRED`; authentication is the 0600 socket in a 0700 directory, i.e. same uid. Commands: `GET /v1/status`, `POST /v1/reload`, `POST /v1/stop`, `GET /v1/perf`, `POST /v1/perf/capture` (`synccontrol/protocol.go:17-21`, `control_socket.go:427-438`). Nothing deletes data, resets state or edits config; `reload` re-reads the config file the user already controls; `perf/capture` writes pprof files to any absolute directory (`:531-532`) as the daemon's uid — equivalent to what the same uid can already do. A malicious same-uid process can stop the sync and write 0600 profile files; nothing more. Matches `spec/design/sync-control-plane.md:7-35`.

### 1.9 Logging — HOLDS WITH CAVEAT

`internal/logfile/logfile.go`: append-only, dir 0755, file 0644 (`:16-17`), retention deletes only this log's own rotations (`:60-106`). Caveat: 0644 log files carry item names and paths at Info/Warn, and the config file (0644, `config/write.go:21`) uses `personal:<email>` / `business:<email>` section keys (`driveid/canonical.go:45-46,203`). The threat model names "local users reading logs and managed-state files" as an actor. Finding 3-5. No field at any level carries a token; the one pre-auth URL leak is finding 3-2.

### 1.10 Threat-model claims — mostly HOLD, two GAPs

| Claim (`spec/design/threat-model.md`) | Code | Verdict |
| --- | --- | --- |
| `synctree` routes every mutation through `os.Root`, never accepts absolute mutation paths (`:55`) | `synctree.go:297-314`, `:461-495` | HOLDS |
| `fsroot` does the same for managed state (`:55`) | `fsroot.go:164-181`, `:192-225` — note `Remove`/`Rename`/`AtomicWrite` use `filepath.Join` + `os.Rename`, not the `os.Root` handle (`:355-383`, `:460`); names are cleaned and `..`-checked, but a symlinked managed-state directory component would be followed. Managed dirs are 0700 and user-owned, so only self-attack. | HOLDS WITH CAVEAT |
| Graph payloads: bounded reads, nil/shape validation (`:56`) | §1.2 | HOLDS WITH CAVEAT |
| Token persistence: validation plus managed-root writes (`:57`) | §1.4 | HOLDS |
| Pre-auth URLs: redaction, host/scheme validation, one raw dispatch boundary (`:58`) | Upload, copy-monitor, socket.io and /content-redirect URLs are validated (`url_validation.go:56-125`, `download.go:498-516`); the `@microsoft.graph.downloadUrl` is not (`download.go:241`, `:363`) | GAP — finding 3-1 |
| Logs: redaction types, no secret logging (`:60`) | websocket dial errors embed the notification URL (`sync/socketio.go:311-333`) | GAP — finding 3-2 |
| Resource exhaustion: response-body caps, page/depth guards (`:62`) | caps §1.2; `defaultMaxListPages`/`maxDeltaPages` 10,000, recursion depth 100 (`client.go:52-61`), `deltaLoopGuard` (`observer_remote.go:767-800`) | HOLDS WITH CAVEAT (3-4) |
| Verified By: `TestDo_DebugLogsNeverExposeBearerToken`, `TestDoPreAuth_ErrorBodyCappedAt64KiB` (`:81`) | both exist (`client_test.go:555`, `:956`) | HOLDS |

## 2. Findings

### 3-1 — `@microsoft.graph.downloadUrl` is dispatched without scheme or host validation
- Severity: medium (SSRF from the home server, and attacker-chosen bytes for hashless items; not a root escape, not a token disclosure).
- Where: `internal/graph/download.go:240-250` (`downloadFromURLWithRange`) and `:362-371` (`downloadFromURL`): `http.NewRequestWithContext(ctx, http.MethodGet, string(item.DownloadURL), http.NoBody)` with no call to any validator. Compare `validatedUploadURL` (`client_preauth.go:156-168`), `validatedCopyMonitorURL` (`:170-182`), `validatedSocketIONotificationURL` (`socketio.go:61-72`) and `validatedContentRedirect` (`download.go:498-516`), which all enforce https, non-IP, Microsoft host. The `urlValidators` struct (`client.go:96-103`) has no `downloadURL` member.
- Rules violated: threat-model.md Attack Surfaces row "Pre-auth URLs … host/scheme validation"; graph-client.md:598 "Graph base URLs and pre-auth URLs are validated before a request reaches that call"; CLAUDE.md "Treat every API response as untrusted input".
- Scenario: a malformed or hostile Graph payload (the threat model's first listed actor) sets `@microsoft.graph.downloadUrl` to `http://192.168.0.2:2283/api/...` or `http://169.254.169.254/...`. The watch daemon on the dev host fetches it with the transfer client (which follows redirects to any host — only `requestContent` sets `CheckRedirect`), so the daemon becomes an SSRF relay into the LAN. For an item with a QuickXorHash the bytes are rejected after 3 attempts (`transfer_download.go:384-456`); for a hashless item whose size matches, `classifyDownload` returns `downloadUnverifiable` (`:151-152`) and the attacker-chosen bytes are published into the sync root. The 2027-04-01 retirement of tempauth URLs (`download.go:98-100`) means this path is exercised on every download today.
- Code evidence: `internal/graph/download_test.go:265-292` `TestDownload_NoAuthOnPreAuthURL` hands the client a plain `http://127.0.0.1:<port>/noauth` download URL and asserts the download succeeds — the suite documents that no scheme/host check exists.
- Existing coverage: none for rejection. `TestDownload_ContentRedirectToUntrustedHostRefused` (`download_content_test.go:106`) covers only the /content redirect leg.
- Proposed regression test: `TestDownload_RejectsUntrustedDownloadURL` — table of `http://` scheme, IP-literal host, non-Microsoft host; assert an error wrapping a new sentinel and that the download server received zero requests; and `TestDownloadRange_…` likewise. Route through a `validators.downloadURL` seam so the existing loopback-based tests keep working by overriding it, as `download_content_test.go:74` already does for `contentRedirect`.
- Confidence: CONFIRMED by tracing (and by the existing test's loopback URL).

### 3-2 — Socket.IO websocket dial failures log the notification URL with its credential
- Severity: medium (secret disclosure to another local principal through a 0644 log file; the secret is a change-notification subscription URL, not content access).
- Where: `internal/sync/socketio_conn.go:28-41` dials `wsURL` built by `socketIONotificationURLToWebsocketURL` (`socketio_protocol.go:125-148`), which keeps the original query (the `snthgk=` credential seen in `graph/socketio_test.go:20`) and adds `EIO`/`transport`. `github.com/coder/websocket@v1.8.15/dial.go:223-225` returns `fmt.Errorf("failed to send handshake request: %w", err)` where `err` is net/http's `*url.Error`, whose `Error()` renders `Get "wss://…svc.ms/socket.io/?EIO=4&snthgk=<token>&transport=websocket": dial tcp …`. `socketio.go:197-213` passes that error to `retryAfterError`, which logs it at Info the first time (`:329-331`, `slog.Any("error", err)`, message "Instant change notifications from OneDrive are unavailable…") and at Debug afterwards (`:325`, `:332`), and also copies `err.Error()` into the lifecycle event (`:276`, `:345-351`) that reaches the NDJSON debug sink when `ONEDRIVE_TEST_DEBUG_EVENTS_PATH` is set (`engine_watch.go:550-584`, `debug_event_sink.go:51`). The redaction layer would not catch it even if applied: `isSensitiveURL` redacts `svc.ms` only when the path contains `/callback` (`redaction.go:135`) and its query-key list (`:152-158`) lacks `snthgk`; the websocket path is `/socket.io/`.
- Rules violated: CLAUDE.md Logging "Never log secrets"; threat-model.md Attack Surfaces "Logs — token or pre-auth URL disclosure"; graph-client.md:199 "websocket callback URLs never appear in logs or surfaced error text"; types.go:33-34 "must never be logged verbatim".
- Scenario: any DNS/TLS/TCP failure during watch mode (an ordinary outage on a home server) writes the live notification URL to the log file (0644, finding 3-5) at Info — the file level default. Anyone who can read the log can subscribe to the drive's change feed until the endpoint expires (`ExpirationDateTime`).
- Existing coverage: `graph/socketio_redaction_test.go` covers only the Graph error-body path; `sync/socketio_test.go` does not assert on dial-error log content (no test name suggests it; not verified line by line).
- Proposed regression test: in `internal/sync`, build a `socketIOWakeSource` with `DialFunc` returning `nil, nil, fmt.Errorf("failed to WebSocket dial: failed to send handshake request: %w", &url.Error{Op: "Get", URL: "https://x.svc.ms/socket.io/?EIO=4&snthgk=SECRET&transport=websocket", Err: errors.New("dial tcp: i/o timeout")})`, capture the logger output and the lifecycle hook, and assert neither contains `SECRET`. Fix candidates (not prescribed): wrap dial errors with a redactor in `connect`, or extend `isSensitiveURL` to all `svc.ms` hosts and apply it in sync's socket.io error path.
- Confidence: CONFIRMED by tracing (library source read at `~/go/pkg/mod/github.com/coder/websocket@v1.8.15/dial.go:188-228`).

### 3-3 — Two collection endpoints read one page only
- Severity: low.
- Where: `internal/graph/items_permissions.go:243-263` (`ListItemPermissions`, struct at `:41-43` has no NextLink) and `internal/graph/drives_sites.go:16-40` (`SearchSites`, struct `drives.go:96-98`).
- Rule violated: CLAUDE.md "Pagination is mandatory, not optional. If an API can return `@odata.nextLink`, code must follow it."
- Scenario: `/permissions` on a widely shared folder pages; the caller's own grant lands on page 2. `EvaluateWriteAccess` (`items_permissions.go:85-118`) then sees only read/link grants and returns `ReadOnly`, so `handle403` (`permission_probe_remote.go:94-131`) converts a transient 403 into a persistent `IssueRemoteWriteDenied` block scope for the folder. Traced consequence is a block plus log line, never a delete (`engine_runtime_permissions.go:60-139`), so no data loss. `SearchSites` silently truncates the site list shown to the user.
- Existing coverage: none for multi-page permissions or sites (`drives_pagination_test.go` covers drives only).
- Proposed test: `TestListItemPermissions_FollowsNextLink` and `TestSearchSites_FollowsNextLink` with a two-page fake server.
- Confidence: CONFIRMED by tracing.

### 3-4 — Delta windows and streamed JSON are not byte-bounded
- Severity: low.
- Where: `internal/graph/delta.go:75-78` and `:165-176` (`deltaAllPages` accumulates all items), `internal/sync/observer_remote.go:221-234` (whole window buffered before classification, by design R-2.15.4); all `json.NewDecoder` sites listed in §1.2 buffer one token of unbounded size.
- Rule: CLAUDE.md "Bound all collections from external input"; R-6.7.30 says "every API response body it buffers in memory" — the design satisfies the letter (streamed bodies are not "buffered") but a hostile server still dictates memory through a single huge string field or a 10,000-page window of large items.
- Scenario: resource exhaustion only; guards are the 10,000-page cap, `deltaLoopGuard`, and the 2-core host's RAM. For the owner's 129k-file drive the initial window will be large but finite.
- Proposed test: a fuzz/limit test feeding a delta page with a 64 MiB `name` and asserting a bounded error (would require an `io.LimitReader` ahead of the decoder with a generous per-page cap).
- Confidence: CONFIRMED (behaviour), SPECULATIVE (actual memory figure).

### 3-5 — Log file and config file are world-readable
- Severity: low.
- Where: `internal/logfile/logfile.go:16-17` (0755/0644), `internal/config/write.go:21-24` (0644/0755).
- Rule: threat-model.md lists "local users or software reading logs and managed-state files" as a threat actor; the config carries `personal:<email>` section keys (`driveid/canonical.go:45-46`) and the log carries item names at Warn (`item_converter.go:583-587`) and, per 3-2, occasionally a credential.
- Scenario: another local account on the dev host reads the owner's account email and file names. The host's threat model (public SSH with password auth kept on, `AllowUsers toni`) makes extra local principals unlikely today.
- Proposed test: assert `0600` after `logfile.Open` and `config.Write`.
- Confidence: CONFIRMED.

### 3-6 — Browser-login callback can be aborted by any local process
- Severity: low.
- Where: `internal/graph/auth_browser.go:142-149`: a request with a wrong `state` delivers an error into the one-slot result channel, and `waitForCallback` (`:206-217`) returns it, ending the login. The listener is loopback-only (`:85`), so only local processes (or a browser tab coerced into fetching `http://localhost:<port>/?state=x`) can do this.
- Rule: none of the project's own rules; robustness of the login flow.
- Scenario: denial of the interactive login on a shared host or via a malicious page open in the same browser; no credential exposure (PKCE verifier never leaves the process).
- Proposed test: send a wrong-state request first, then the right one, and assert the login still succeeds (i.e. ignore mismatches instead of failing on the first).
- Confidence: CONFIRMED by tracing.

### 3-7 — Cross-process token refresh is uncoordinated
- Severity: low.
- Where: `internal/graph/auth_token.go:98-120` (`OnTokenChange` → `tokenfile.Save`), no lock around read-modify-write across processes.
- Scenario: see §1.4. Only matters under single-use refresh-token policies; otherwise last-writer-wins is harmless.
- Confidence: PLAUSIBLE.

## 3. Checked and found sound

- Bearer never reaches pre-auth hosts: `doPreAuth` adds no Authorization (`client_preauth.go:50-56`), `/content` first leg does not follow its 302 and the validated Location is fetched unauthenticated (`download.go:416-496`, `TestDownload_ContentEndpointNeverSendsBearerToRedirectHost`).
- nextLink/deltaLink confinement to the configured base URL (`items_fetch.go:338-344`), base URL validation at construction (`url_validation.go:13-54`).
- Pre-auth URL validation for upload sessions, copy monitors, socket.io endpoints and /content redirects: https only, no userinfo, no IP literal, Microsoft host suffix list (`url_validation.go:56-125`); malformed upload URLs are reported without echoing the URL (`:127-140`).
- Error-body redaction of bearer tokens, token JSON keys, and sensitive URLs in both structured and plain-text bodies (`redaction.go`), with tests `TestBuildGraphError_RedactsStructuredSecrets/PlainTextSecrets`, `TestDispatchRequest_RedactsPreAuthURLFromTransportError`.
- 401 handling: forced refresh once, rate-limited, `invalid_grant` is the only fatal outcome, server-side RPS faults are `ErrAuthTransient` (`client_auth.go:97-108`, `:226-297`).
- Upload integrity: creates use `conflictBehavior=fail` so a replay cannot overwrite (`item_create.go:18-44`); overwrites are `If-Match`-conditional on both routes (`upload_session.go:19-27`, `upload_transfer.go:15-24`); non-replayable bodies are never retried; the zero-byte PUT regression (D-033) is guarded by `requestBodyReplayable`.
- Download integrity: ranged resume requires 206 with an exact `Content-Range` and a bound version (`download.go:279-305`, `:38-50`); HTML/JSON error documents with a 200 are refused for hashless items (`:313-332`); hash verification with bounded retries and the HEIC exception explicitly logged (`transfer_download.go:140-187`, `:384-456`).
- Remote names: `.`/`..`/`/`/NUL/invalid-UTF-8 rejected, parent path segments checked, empty id/name skipped, no URL decoding anywhere; OneDrive name equivalence (`namekey`) used for all local/remote collision checks.
- `synctree`: fresh `os.OpenRoot` per operation, containment errors never normalised to not-exist, no-follow tree walks for mount moves and removals, rename confined on both ends.
- Local trash: id and path sanitisation, symlink-ancestor refusal, rename-noreplace restore.
- Control socket: owner `flock` with inode re-check, directory ownership/sticky checks covering the temp-dir fallback, 0600 socket, bounded capture body, command set limited to status/reload/stop/perf.
- Config: unknown-key detection, strict/lenient decode from the same primitives, non-negative sizes, minimum durations, absolute `sync_dir` after `~/` expansion, filter paths root-relative and `..`-free.
- TLS/HTTP: stdlib defaults cloned, no custom TLS config, proxy env honoured, dial/TLS/header/HTTP2-ping timeouts, transfer stall watchdog.
- 410/delta expiry: no partial window is ever applied, orphan deletes only under `resyncChangesApplyDifferences` and only after per-item confirmation, failed recoveries keep the expired token and back off.
- Panics: the only production `panic` in the reviewed packages is `graph.MustNewClient` (`client_construction.go:75`, test/setup helper); goroutine boundaries recover (`multisync/mount_runner.go:38`, `sync/scanner.go:277`, `sync/worker.go:153`, `sync/engine_watch_panic_recovery.go:29`).
