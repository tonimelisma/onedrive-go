# OneDrive / Microsoft Graph throttling: rules, prior art, and what the onedrive-go fix should look like

Research date: 2026-11-01. All quotes are verbatim from the cited page or source file as fetched today unless marked *paraphrase*. Items I could not confirm are marked **unconfirmed**. Opinions are marked **opinion**.

Sources fetched (primary):

- G-THR: https://learn.microsoft.com/en-us/graph/throttling (ms.date 2025-01-14, updated_at 2025-08-06)
- G-LIM: https://learn.microsoft.com/en-us/graph/throttling-limits
- SPO: https://learn.microsoft.com/en-us/sharepoint/dev/general-development/how-to-avoid-getting-throttled-or-blocked-in-sharepoint-online (ms.date 2025-10-02, updated_at 2026-08-10; source commit `71e7d784fd`, 2026-08-10)
- SCAN: https://learn.microsoft.com/en-us/onedrive/developer/rest-api/concepts/scan-guidance (ms.date 2025-10-02)
- BATCH: https://learn.microsoft.com/en-us/graph/json-batching (ms.date 2025-02-21); known-issues include `concepts/includes/known-issues/json-batching.md` (ms.date 02/06/2026)
- UPL: https://learn.microsoft.com/en-us/graph/api/driveitem-createuploadsession (ms.date 2025-10-15, updated 2026-08-11)
- ERR: https://learn.microsoft.com/en-us/graph/errors and https://learn.microsoft.com/en-us/onedrive/developer/rest-api/concepts/errors
- SDK-SPEC: https://raw.githubusercontent.com/microsoftgraph/msgraph-sdk-design/main/middleware/RetryHandler.md
- SDK-GO: https://raw.githubusercontent.com/microsoft/kiota-http-go/main/retry_handler.go (used by msgraph-sdk-go-core `GetDefaultMiddlewaresWithOptions` → `khttp.GetDefaultMiddlewares()`)
- https://learn.microsoft.com/en-us/graph/sdks/throttling returned **HTTP 404**; the SDK behaviour below comes from SDK-SPEC and SDK-GO instead.
- rclone: `backend/onedrive/onedrive.go`, `lib/pacer/pacer.go`, `lib/pacer/pacers.go`, `fs/accounting/tpslimit.go`, `fs/config.go`, `docs/content/docs.md`, `docs/content/onedrive.md` (master, fetched 2026-11-01)
- abraunegg/onedrive: `src/onedrive.d`, `src/curlEngine.d`, `src/config.d`, `docs/application-config-options.md` (master); issues #815, #1304, #3862
- Theory: Jacobson, "Congestion Avoidance and Control", SIGCOMM '88 (text extracted from https://ee.lbl.gov/papers/congavoid.pdf); Chiu & Jain, *Computer Networks and ISDN Systems* 17(1), June 1989, pp. 1–14, doi:10.1016/0169-7552(89)90019-6 (abstract from https://www.cse.wustl.edu/~jain/papers/cong_av.htm; full PDF fetched but my text extraction failed, so body quotes are **unavailable**); RFC 5681 (https://www.rfc-editor.org/rfc/rfc5681.txt); Brooker, AWS Architecture Blog, "Exponential Backoff and Jitter", 2015-03-04 (formulas on that page are images; prose quoted); Fowler, "CircuitBreaker", 2014-03-06 (https://martinfowler.com/bliki/CircuitBreaker.html); Google SRE book, "Handling Overload" and "Addressing Cascading Failures" (https://sre.google/sre-book/handling-overload/, https://sre.google/sre-book/addressing-cascading-failures/)

---

## Part 1 — Answers to questions a–h

### a. Do requests made while throttled count, and do they extend the window?

Yes, in three places, in slightly different words.

- G-THR: "Avoid immediate retries, because all requests accrue against your usage limits." and "Backing off requests using the `Retry-After` delay is the fastest way to recover from throttling because Microsoft Graph continues to log resource usage while a client is being throttled."
- SPO: "Throttled requests count towards usage limits, so failure to honor `Retry-After` may result in more throttling. In other words, aggressive retries work against calling applications because even though the calls fail, they still count toward usage limits. Honoring the `Retry-After` HTTP header will ensure the shortest delay and reduce wasting quotas in throttled requests."
- SCAN (the OneDrive-specific page, aimed at "sync engines, backup providers"): "When waiting for 429 or 503 recovery you should ensure that you pause all further requests you are making to the service. This is especially important in multi-threaded scenarios. Making additional calls while receiving throttle responses will extend the time it takes for your app to become unthrottled." Also: "If throttling persists, the Retry-After value may become longer over time, allowing the system to recover. Apps that do not honor the retry after duration before calling back will be blocked due to abusive calling patterns."
- SPO (search section, same principle): "Making more calls while receiving throttle responses will extend the time it takes for your app to become unthrottled."
- SPO escalation: "If the offending application continues to exceed usage limits, SharePoint Online may completely block the application or specific request patterns from the application; in this case, the application will keep getting HTTP status code 503".

So tonight's 191 cycles of "probe → release thousands → 429" is exactly the pattern Microsoft says lengthens the penalty and, if sustained, leads to a block.

### b. Is Retry-After always present, in seconds, and what if absent?

- SPO: "For requests that an application makes, including Microsoft Graph, CSOM, or REST calls, SharePoint Online returns HTTP status code 429 ("Too many requests") or 503 ("Server Too Busy") … In both cases, a `Retry-After` header is included in the response, indicating how long the calling application should wait before retrying or making a new request." and "SharePoint Online returns a `Retry-After` HTTP header in the request indicating how long in seconds the calling application should wait".
- G-THR: "All the resources and APIs described in the Service-specific limits provide a `Retry-After` header except where indicated." The only "indicated" exception in G-LIM is OneNote: "The resources listed earlier don't return a `Retry-After` header on `429 Too Many Requests` responses." Files/drive resources carry no such note.
- Absent: G-THR: "If no `Retry-After` header is provided by the response, we recommend implementing an exponential backoff retry policy."
- Format: the Graph sample shows `Retry-After: 10` (integer seconds). The Kiota Go handler also accepts an HTTP-date (`time.Parse(time.RFC1123, …)`), which is the RFC 7231 alternative; I found no Microsoft doc saying OneDrive ever sends the date form (**unconfirmed**, treat as a defensive parse).
- Caveat for upload sessions: UPL's "Best practices" say "Use an exponential back off strategy if any 5xx server errors are returned when resuming or retrying upload requests. For other errors, you shouldn't use an exponential back off strategy but limit the number of retry attempts made." That text does not mention 429; read with G-THR it means: 429 → honour Retry-After; 5xx without Retry-After → exponential backoff.
- Observed sizes: a documented SPO example shows `Retry-After: 31`; a OneDrive Personal report (onedrive-api-docs issue #1777, 2024-01-19, `createLink`) shows `Retry-After: 3515` with error `accessDenied` / inner `throttledRequest`. So values from seconds to about an hour occur in the wild.

### c. RateLimit-Limit / -Remaining / -Reset: returned today?

The documentation reversed itself in August 2026. From the sp-dev-docs git history of this page:

- 2022-06-29 (`9de8d4d19f`, "A major update to SharePoint rate limiting") through the 2025-10-02 version (`aab87ceb09`): section "RateLimit headers - preview": "SharePoint Online also returns the IETF RateLimit headers for selected limits in certain conditions … The `RateLimit` headers are returned on a **best-efforts** basis, so applications may not receive the headers under all conditions. Additionally, there are other limits that aren't presented in the `RateLimit` headers". The one supported limit: "App 1-minute resource unit | Usage >= 80% of the limit | … When an application consumes 80% or more of its app 1-minute limit, the limit, remaining, and reset are returned." Example: `RateLimit-Limit: 1200 / RateLimit-Remaining: 120 / RateLimit-Reset: 5`, and on 429 "The `Retry-After` matches the `RateLimit-Reset`. There are instances where the `Retry-After` returns a smaller value. In such cases, the general rule of thumb is to honor the greater of the two values."
- Live page (commit `71e7d784fd`, 2026-08-10): "### RateLimit headers — SharePoint Online does not return or support `IETF RateLimit` headers. Although these headers may be used by other services, applications should not depend on them for SharePoint Online and should instead honor the `Retry-After` header when throttling occurs."
- The live page is internally inconsistent: its summary list still says "Use the `Retry-After` and `RateLimit` HTTP headers" and "The `RateLimit` HTTP headers send you early signals when you're close to limits". The dedicated section is the newer, more specific statement.

Conclusion: the repo's 2026 spec decision ("SharePoint Online does not return them") is now literally Microsoft's own statement for business/SharePoint drives. For **OneDrive personal (consumer)** no Microsoft page mentions RateLimit headers at all — **unconfirmed** either way. A Microsoft Q&A answer (2023-05-04, "Microsoft External Staff") about a business tenant reported seeing `RateLimit-Limit: 1250` at 80 % quota and 429s with `activityLimitReached`, `retry-after=5` — consistent with the 2022–2025 docs, now superseded.

How to use them if they do appear: the 2022–2025 text implied "when Remaining approaches 0, slow down until Reset; on 429 honour max(Retry-After, RateLimit-Reset)". Given the 2026 wording, a client should treat the headers as opportunistic telemetry (log them, optionally feed Remaining/Reset into a pacer as an upper bound) and never depend on them. **Opinion:** do not build proactive slowing on them; build it on a client-side rate model (Part 3).

### d. Documented limits and what counts as one request

G-LIM has no numbers for files; it says: "For service limits for OneDrive and SharePoint, see Avoid getting throttled or blocked in SharePoint" (resources listed include `driveItem`, `drive`, `driveItemVersion`, …). Global: "130,000 requests per 10 seconds" per app across all tenants.

SPO (business / OneDrive for Business; "Displayed limits are default values. Microsoft may change these limits at any time."):

- Resource units (RU) per Graph request: 1 = "Single item query, such as get item; Delta with a token; Download file from drive item"; 2 = "Multi item query, such as list children, except delta with a token; Create, update, delete, and upload"; 5 = "All permission resource operations, including `$expand=permissions`". "We reserve the right to change the API resource unit cost."
- Per user: "Requests | 5 min | 3,000"; "Ingress | 1 H | 50 GB"; "Egress | 1 H | 100 GB". (3,000 per 5 min = 10 requests/s sustained; tonight's 22–28 req/s exhausts that in about two minutes.)
- Per app per tenant (RU, 1 min): 1,250 (0–1,000 licences), 2,500, 3,750, 5,000, 6,250 (50,000+). Per 24 h: 1,200,000 up to 6,000,000. Ingress/egress 400 GB/h.
- Per tenant (RU, 5 min): 18,750 up to 93,750.
- Rate estimate: "you can estimate the request rate using an average of 2 resource units per request, and divide resource unit limits by 2 to get the estimated request rate." → for the smallest tenant about 625 requests/min ≈ 10/s per app.
- "Every request that an application makes across all API endpoints, including Microsoft Graph, CSOM, and REST, counts towards the application's usage."
- "Microsoft Reserves the right to lower limits on Unpaid/Unlicensed usage."

Upload-session fragments: UPL says fragments go as `PUT` to the `uploadUrl` (e.g. `https://sn3302.up.1drv.com/up/…`) without an `Authorization` header, "must be uploaded sequentially in order", each "MUST be a multiple of 320 KiB", "less than 60 MiB", recommended "between 5-10 MiB". **Unconfirmed:** whether each fragment PUT is metered as a 2-RU "upload" request or whether only `createUploadSession` is; no Microsoft page says. The byte-based ingress limits (50 GB/h per user, 400 GB/h per app) certainly apply to fragment bytes.

OneDrive personal (consumer): **no published numeric limits** anywhere I could find (G-LIM defers to SPO, which is written for SharePoint Online / OneDrive for Business; SCAN explicitly scopes some features to "SharePoint and OneDrive for Business but not consumer OneDrive accounts"). Field reports (onedrive-api-docs #1777) show a 429 after about 12 `createLink` calls with `Retry-After: 3515` on a personal account, with no Microsoft reply. Error codes: ERR lists `activityLimitReached` = "The app or user has been throttled." and inner `throttledRequest` = "Too many requests."; 509 = "Your app has been throttled for exceeding the maximum bandwidth cap."

### e. Traffic decoration

SPO: "Well-decorated traffic will be prioritized over traffic that isn't properly decorated." Undecorated = "no AppID/AppTitle and User Agent string in API calls to SharePoint Online." Format table:

- ISV: `ISV|CompanyName|AppName/Version` — "Identify as ISV and include Company Name, App Name separated by a pipe character and then add Version number separated with a slash character"
- Enterprise: `NONISV|CompanyName|AppName/Version`
- "The format of the user agent string is expected to follow RFC2616 … It is also fine to append the existing user agent string with the requested information."
- Recommendation also to "register and use AppID and AppTitle".

Microsoft says prioritised, not "throttled earlier" in so many words; the operational effect is the same. Scope: the page is about SharePoint Online (which backs OneDrive for Business); whether consumer OneDrive applies the same prioritisation is **unconfirmed**. Prior art: abraunegg's default is `ISV|abraunegg|OneDrive Client for Linux/<version>` (`src/config.d`: `defaultUserAgent = isvTag ~ "|" ~ companyName ~ "|" ~ appTitle ~ "/" ~ …`, with a comment "Comply with OneDrive traffic decoration requirements"); rclone's default is `"rclone/" + Version` and its OneDrive docs say "If you experience excessive throttling or is being blocked on SharePoint then it may help to set the user agent explicitly with a flag like this: `--user-agent "ISV|rclone.org|rclone/v1.55.1"`".

### f. Batching

- Limit: "Microsoft Graph supports batching up to 20 requests into the JSON object." / "JSON batch requests are currently limited to 20 individual requests."
- Throttling accounting: BATCH: "Requests in a batch are evaluated individually against the applicable throttling limits and if any request exceeds the limits, it fails with a status of `429`." G-THR: "Requests in a batch are evaluated individually against throttling limits … The batch itself succeeds with a status code of `200` (OK). Multiple requests can be throttled in a single batch. You should retry each failed request from the batch using the value provided in the `retry-after` response header from the JSON content. You might retry all the failed requests in a new batch after the longest `retry-after` value." and "throttled requests that were part of a batch aren't retried automatically" by the SDKs. SPO: "In batching, requests in a batch are evaluated individually by resource units." → a batch of 20 folder creates costs 20 × 2 RU, the same as 20 separate calls. Batching saves round trips and latency, not quota.
- Folder creation: a JSON `POST …/children` body is an ordinary batchable request (BATCH allows `body` as "a JSON object"). Small-file upload: BATCH says body "Might be a JSON object or a base64 URL-encoded value, for example, when the body is an image." A Microsoft Q&A thread reports driveItem content uploads inside a batch work but return the response body base64-encoded "by design" — community-level evidence; **not confirmed by a Microsoft doc for drive content specifically**. Upload-session fragment PUTs go to a different host and cannot be batched.
- Known issues (2026-02-06 include): "requests can only depend on a single other request, and must follow one of these three patterns: Parallel … Serial … Same". No documented batch body size limit was found (**unconfirmed**; the often-quoted 4 MB figure did not appear in any fetched Microsoft page).
- Each sub-request carries `x-ms-resource-unit` in its response headers (visible in the BATCH example), which is useful telemetry.

### g. Recommended backoff and concurrency; how rclone realises it

Microsoft:

- G-THR algorithm: "1. Wait the number of seconds specified in the `Retry-After` header. 2. Retry the request. 3. If the request fails again with a 429 error code, you're still being throttled. Continue to use the recommended `Retry-After` delay and retry the request until it succeeds." Best practices: "Reduce the number of operations per request. Reduce the frequency of calls. Avoid immediate retries".
- SPO summary: "Reduce the number of concurrent requests; Avoid request spikes; … Use the `Retry-After` and `RateLimit` HTTP headers; Decorate your traffic". And: "If you do run into throttling, we require using the `Retry-After` HTTP header to ensure a minimum delay until the throttle is removed."
- SCAN: "pause all further requests … especially important in multi-threaded scenarios."
- SPO (people search, but the only place Microsoft states a concrete concurrency rule): "Avoid making concurrent requests. For example, instead of issuing 10 requests all at once, issue them consecutively".
- G-LIM (Teams section, general design note): "Size your steady-state traffic below the listed limit and use exponential backoff."

Microsoft SDK:

- SDK-SPEC: handles "429 | Too many requests", "503 | Server Unavailable", "504 | Gateway Timeout"; "Retries MUST respect the `retry-after` response header if provided"; defaults "maximum retry attempts: 10", "initial retry interval: 3 seconds", "maximum interval: 180 seconds", "retry interval multiplier: (retry number)^2", "maximum elapsed time: 1800 seconds"; "Only requests with payloads that are buffered/rewindable are supported."; "Where no `retry-after` header is provided by the server, perform retry with the retry handler options … or fall back to the default handler implemented as exponential backoff handler."
- SDK-GO (the actual Go implementation): `defaultMaxRetries = 3`, `absoluteMaxRetries = 10`, `defaultDelaySeconds = 3`, `absoluteMaxDelaySeconds = 180`; retries 429/503/504; Retry-After parsed as float seconds or RFC1123 date and used exactly; otherwise `delay := delaySeconds*(1<<exp)` seconds plus `jitter := rand.Float64()*1000 ms`; bodied requests retried only when `ContentLength != -1`; stops when `cumulativeDelay >= 180 s`. Note the spec and the Go code disagree (10 vs 3 retries; quadratic vs doubling).

rclone (master):

- Per-backend shared pacer: `pacer.NewDefault(pacer.MinSleep(10ms), pacer.MaxSleep(2s), pacer.DecayConstant(2))` with default `attackConstant: 1`.
- `Calculate`: if the last error carries Retry-After → `sleep = max(RetryAfter, minSleep)` (no cap); else on a retry → `sleep = (sleep << 1) / 1` i.e. doubled, capped at `maxSleep`; on success → `sleep = max((sleep*4 - sleep)/4, minSleep)` i.e. multiplied by 3/4.
- Sharing: `beginCall` takes a single token from `p.pacer` when `SleepTime > 0` and returns it `SleepTime` later in a goroutine, so **every call from every goroutine on that remote is spaced ≥ SleepTime apart**; `endCall` updates the shared `SleepTime`. Default `retries: 3` inside the pacer, overridden by `--low-level-retries` (default 10).
- `shouldRetry` (onedrive.go): `case 429, 503:` parse `Retry-After` with `strconv.Atoi`, `err = pacer.RetryAfterError(err, duration)`; `retryErrorCodes = 429, 500, 502, 503, 504, 509`; 507 fatal; 504 logs "upload chunks may be taking too long - try reducing --onedrive-chunk-size or decreasing --transfers".
- Global option `--tpslimit` (default 0 = off): "Limit transactions per second to this number … For HTTP based backends it is an HTTP PUT/GET/POST/etc and its response." Implemented as `rate.NewLimiter(rate.Limit(ci.TPSLimit), tpsBurst)` with `--tpslimit-burst` default 1, applied in `fs/fshttp` for every HTTP request (a process-wide token bucket). `--transfers` default 4, `--checkers` default 8.
- rclone's author's advice on `activityLimitReached` (issue #2406, 2018): "experiment with `--tpslimit` … reducing `--checkers` and `--transfers` to say 1 and 2 … decorating rclone's user agent".

### h. The restart case

What the sources imply:

- Microsoft: pausing all requests during Retry-After is required; throttled requests count and extend the window; the Retry-After "may become longer over time" under persistent offence. A client that restarts and immediately fires its backlog is indistinguishable, server-side, from the one that ignored Retry-After.
- TCP: RFC 5681 §3.1: "Beginning transmission into a network with unknown conditions requires TCP to slowly probe the network to determine the available capacity, in order to avoid congesting the network with an inappropriately large burst of data. The slow start algorithm is used for this purpose at the beginning of a transfer, or after repairing loss detected by the retransmission timer." and §4.1: "[Jac88] recommends that a TCP use slow start to restart transmission after a relatively long idle period." A restart is exactly "unknown conditions".
- Prior art: none of rclone, abraunegg or the Graph SDK persists any throttle state; all three begin a fresh process at full configured concurrency and rely on the first 429 to slow down (verified by reading the code: rclone's pacer state is an in-memory struct initialised to `minSleep`; abraunegg's `retryAttempts`/`thisBackOffInterval` are locals of one call; Kiota's state is per request). So "persist the Retry-After deadline" would be ahead of all three.

Implications (**opinion**, derived from the above): on restart (1) if a persisted Retry-After deadline is in the future, do not send anything to that drive before it (cost of honouring a stale deadline is bounded by one Retry-After, typically seconds to an hour; make it overridable); (2) start in slow start regardless, with a small initial admission (1–2 in flight), doubling per success round up to a ceiling; (3) keep the "N until M consecutive successes" rule only as the way to leave slow start, not as the steady state — steady state should be additive growth up to a per-drive rate cap (Part 2 explains why).

---

## Part 2 — Prior art, judged

For each implementation: control model, state scope, Retry-After fidelity, restart behaviour, failure modes, verdict. Facts come from the source files named above; verdicts are **opinion**.

### rclone (lib/pacer Default + backend/onedrive)

- **Control model:** a shared pacer that controls the *interval between request starts*, not concurrency. Interval doubles on a retryable failure without Retry-After (capped 2 s), is set exactly to Retry-After when present (uncapped), and decays by ×3/4 on every success down to 10 ms. In rate terms that is multiplicative decrease (rate halves) and multiplicative increase (rate ×4/3 per success): **MIMD on rate**, plus an optional process-wide **token bucket** (`--tpslimit`, off by default). Concurrency (`--transfers`, `--checkers`) is fixed by flags and never adapted.
- **State shared across workers:** yes — one pacer per remote; a 429 seen by any goroutine slows every goroutine's next start.
- **Honours Retry-After exactly:** yes, as the global gap before the *next* start anywhere. Because the gap then decays geometrically on each success (10 s → 7.5 → 5.6 → 4.2 …), rclone effectively re-admits gradually; the sum of the decaying gaps is about 4× Retry-After before it is back to full speed. This ramp is a consequence of the decay constant, not a designed slow start — the same formula serves every backend.
- **Restart:** nothing persisted; starts at a 10 ms gap (≤ 100 starts/s) with full `--transfers`/`--checkers`.
- **Failure modes:** (1) controls spacing but not in-flight count, so 12 default goroutines can all be mid-request when the 429 lands, and each of their retries is itself paced but still counted; (2) no jitter in the Default pacer (the GoogleDrive pacer adds random milliseconds; OneDrive's does not) — immaterial inside one process because the single token serialises starts, material across many rclone instances; (3) when Retry-After is absent the attack cap is 2 s, so a persistent header-less 429 would be re-probed every 2 s for 10 low-level retries; (4) the geometric decay means recovery speed is coupled to the number of successes, so a backlog of thousands of small files recovers in a few dozen requests — which is fast, but it will re-hit the limit if the limit is a rate rather than a transient; (5) Retry-After uncapped: a 3515 s header stalls the whole remote for an hour, which is correct per Microsoft but surprising to users.
- **Verdict (opinion):** principled in shape (shared state, honour the server's wait, gradual return), accidental in parameters (the 2 s cap and ×3/4 decay were tuned for other backends). It is MIMD, which Chiu–Jain show does not converge to fairness between competing flows; for a single client against its own quota that objection is weaker, but MIMD still oscillates more than AIMD. Worth copying: the single shared pacer and "Retry-After sets the gap for everyone". Not worth copying: the fixed concurrency, and the uncapped-vs-capped asymmetry.

### abraunegg/onedrive

- **Control model:** per-request retry loop (`oneDriveErrorHandlerWrapper`): 408/429 → sleep exactly `Retry-After` (120 s default if the header is missing: "Use a 120 second delay as a default given header value was zero … based on log files"); 500/502/503/504 → fixed 30 s; anything else transient → `calculateBackoff = min(1 s × 2^min(attempts,10), 120 s)`; up to `maxRetryCount = 175200` ("Approx 365 days"); no jitter; no shared pacer; no token bucket; no circuit breaker. The only rate control is static: `threads` default 8, max 16, and (per the docs) non-transfer operations run serially on one thread.
- **State shared across workers:** none. Each thread sleeps on its own; other threads keep sending until they are individually throttled.
- **Honours Retry-After exactly:** yes, today. Issues #815 (2020: "currently onedrive-client uses a hard-coded 'int backoffInterval = 2'") and #1304 (2021: "does not honor the Retry-After header value sent, but instead tries again in 120 seconds") show it was fixed reactively, twice. Issue #3862 (2026-09) is the maintainer's own audit noting "multiple layers involved in upload retry handling, and the resulting behaviour is not sufficiently clear or symmetrical."
- **Restart:** nothing persisted; starts with all threads.
- **Failure modes:** (1) per-request backoff lets N workers each take a 429 (N × wasted, counted requests) before the process as a whole is quiet; (2) threads that were throttled at the same instant wake at the same instant → synchronised burst of up to 16, no jitter; (3) the 120 s default is arbitrary; (4) backoff "that never recovers" is prevented only by the 120 s cap, and "retry for a year" means a dead account spins forever; (5) the docs admit "using more than 16 threads … frequently caused the Microsoft OneDrive service to become blocked", i.e. the thread cap is the real limiter and was found empirically.
- **Verdict (opinion):** honours the letter of G-THR per request and nothing more; the herd is bounded only by the thread count. It works because 8–16 single-item workers cannot generate thousands of simultaneous requests — which is also why onedrive-go's design (thousands of admitted items released at once) is a different regime and cannot borrow abraunegg's approach. Do not copy: per-thread independence, fixed defaults, unbounded retry count.

### Microsoft Graph SDK retry handler (Kiota, Go)

- **Control model:** per-request exponential backoff with jitter (3 s × 2^(n−1) + U(0, 1 s)), Retry-After honoured exactly without jitter, 3 retries by default (max 10), cumulative delay cap 180 s; only 429/503/504; only rewindable bodies. Nothing shared, no pacing, no concurrency control, no circuit breaker.
- **State shared across workers:** none — by design it is a middleware on one request.
- **Honours Retry-After exactly:** yes (seconds or HTTP-date).
- **Restart:** n/a.
- **Failure modes:** (1) N concurrent requests each receive the same Retry-After and all wake at the same instant (no jitter on the Retry-After path) — the synchronised-herd case; (2) nothing stops the application from issuing *new* requests during the wait, which is precisely what SCAN says must not happen; Microsoft's own guidance therefore assumes application-level pacing above the SDK; (3) spec/implementation drift (10 vs 3 retries, quadratic vs doubling), so "what the SDK does" is language-dependent — the PowerShell SDK is reported (practical365, secondary) as 10 retries, 180 s max.
- **Verdict (opinion):** a correct retry *primitive*, deliberately not a load controller. Copying only it reproduces tonight's incident. Its one idea worth adopting verbatim: jitter on the computed backoff path, none on the server-given wait.

### What the theory says, and where the in-flight fix sits

Established results (sources):

- **Slow start / self-clocking (Jacobson 1988):** "To start the `clock', we developed a slow-start algorithm to gradually increase the amount of data in-transit." He called it "soft-start, a reference to an electronics engineer's technique to limit in-rush current." The conservation principle: "A new packet isn't put into the network until an old packet leaves." RFC 5681 adds that slow start is for "unknown conditions" and after idle.
- **AIMD (Jacobson 1988):** "On any timeout, set cwnd to half the current window size (this is the multiplicative decrease). On each ack for new data, increase cwnd by 1/cwnd (this is the additive increase)." Why halve: "you should reduce your window by half because the bandwidth available to you has been reduced by half. And, if there are more than two conversations sharing the bandwidth, halving your window is conservative". RFC 5681 eq. (4): `ssthresh = max(FlightSize / 2, 2*SMSS)` — after loss, slow start resumes only up to half the previous flight, then additive increase.
- **Chiu & Jain 1989 (abstract):** "It is shown that a simple additive increase and multiplicative decrease algorithm satisfies the sufficient conditions for convergence to an efficient and fair state regardless of the starting state of the network." The standard reading (Wikipedia, citing the paper) is that MIMD and AIAD "do not reach stability" in the fairness sense. I could not extract the body text, so the precise statement about MIAD is **unconfirmed** here.
- **Jitter (Brooker 2015):** "backoff helps a small amount, but doesn't solve the problem … Instead of reducing the number of clients competing in every round, we've just introduced times when no client is competing." "The solution isn't to remove backoff. It's to add jitter." Full Jitter "uses less work, but slightly more time"; Equal Jitter "is the loser". Formulas are images on the page; as commonly transcribed: Full Jitter `sleep = random_between(0, min(cap, base·2^attempt))`, Decorrelated `sleep = min(cap, random_between(base, prev_sleep·3))` (**transcription, not quoted from the page**). Google SRE (cascading failures): "Always use randomized exponential backoff when scheduling retries … If retries aren't randomly distributed over the retry window, a small perturbation … can cause retry ripples to schedule at the same time, which can then amplify themselves."
- **Circuit breaker (Nygard via Fowler 2014):** open → after an interval "the circuit is ready to make a real call as trial" (half-open) → "a trial call, which will either reset the breaker if successful or restart the timeout if not"; rationale "a broken circuit avoids putting load on a struggling server."
- **Token bucket:** rclone's `--tpslimit` is the concrete example (golang.org/x/time/rate, rate r, burst b). It bounds steady-state rate and burst independently of how many workers exist.
- **Adaptive throttling (Google SRE, Handling Overload):** client tracks `requests` and `accepts` over two minutes; "Clients can continue to issue requests to the backend until requests is K times as large as accepts. Once that cutoff is reached, the client begins to self-regulate and new requests are rejected locally"; rejection probability max(0, (requests − K·accepts)/(requests + 1)) (formula rendered as an image on the page; text confirms K = 2 and the 2-minute window); retry budgets: "a per-request retry budget of up to three attempts" and "A request will only be retried as long as this ratio is below 10%."

Mapping the in-flight fix (facts about the fix are as described to me; the mapping is **opinion**):

| Fix element | Corresponds to | Deviation / comment |
|---|---|---|
| Retire admitted work when a throttle activates | Circuit breaker **open**; SCAN's "pause all further requests"; Jacobson's conservation ("a new packet isn't put into the network until an old packet leaves") | None. This is the piece every prior-art client lacks and the one Microsoft explicitly requires. Correct. |
| Single probe after Retry-After | Circuit breaker **half-open** trial | Correct. The incident was that half-open success closed the breaker fully; the fix replaces "closed" with "ramping". |
| Bounded batch that doubles on success | TCP **slow start** (exponential growth of the window) | Correct as the *start*. Deviation: TCP leaves slow start at `ssthresh` and grows additively afterwards; a pure doubling ramp overshoots by up to 2× at the moment of failure, and here every overshoot costs a full Retry-After plus "throttled requests extend the window". |
| Reset to the initial batch on a 429 | TCP's **timeout** reaction (cwnd → 1), i.e. full multiplicative decrease | Harsher than AIMD's halving. Justified *for the first 429 after a wait*, because Retry-After is a hard stop the server has told us about, not an inferred loss. Not justified as the steady state: with no `ssthresh` the ramp is a sawtooth 1→2→4→…→429→wait→1 whose peak is always one doubling past the limit. |
| Slow start on restart | RFC 5681 §4.1 restart-after-idle | Correct. Add: persist the deadline so the restart does not probe inside a known penalty (none of the prior art does). |
| Jitter | — | Not needed on the server-given wait; recommended on the re-admission of a batch (spread a batch of B over a short window rather than firing B at once) and on any computed backoff when Retry-After is absent. |

Is deviating from AIMD justified because the server gives an explicit Retry-After? Partly. Retry-After makes the *decrease* side easy and exact (stop; no need to guess), which is a better signal than TCP ever has — that justifies "stop fully, then probe" over "halve". It does not help the *increase* side: nothing in the 429 tells the client what the sustainable rate is, so the increase policy still has to discover it, and here Chiu–Jain's reasoning applies: multiplicative increase oscillates with amplitude proportional to the limit, additive increase converges. **Opinion:** keep the fix as written for the ship date (it removes the catastrophic mode), and in the follow-up add an `ssthresh`-style memory (slow start up to half the batch size that last drew a 429, then +1 per success round) or, equivalently, make the ramp AIMD on a per-drive rate cap rather than on batch size alone.

One more theory point: the Google SRE K-factor is the right *accounting*. Count requests and accepts per drive over a window; if the ratio of 429s to accepts rises while the breaker is nominally closed, that is the signal to lower the ceiling, independently of any single Retry-After. The 10 % retry budget maps directly onto "retire the backlog rather than retry it".

---

## Part 3 — Recommendation for onedrive-go (≤ 15 lines)

1. The in-flight fix is right where it matters: retire everything on activation (Microsoft: "pause all further requests … especially important in multi-threaded scenarios"), one probe after an exactly-honoured Retry-After (circuit breaker), bounded doubling re-admission (slow start), reset on 429, slow start on restart. No prior-art client does the first or the last; ship it.
2. Follow-up, in order: (a) one **per-drive pacer** shared by all workers — a token bucket with an adaptive rate ceiling, zero tokens while a Retry-After deadline is pending; the ramp becomes AIMD on that ceiling (slow start to `ssthresh` = half the rate that last drew a 429, then +1 per success round), which ends the sawtooth; (b) **persist the Retry-After deadline** per drive and refuse to send before it on restart (bounded cost, overridable); (c) **User-Agent decoration** `ISV|<company>|onedrive-go/<version>` appended to the existing UA (Microsoft: "Well-decorated traffic will be prioritized"); (d) log `RateLimit-*` and `x-ms-resource-unit` when present but depend on neither — Microsoft's page now says SPO "does not return or support" them; (e) jitter the release of each re-admitted batch over a short window and jitter computed backoff when Retry-After is absent; (f) batch folder creates for latency only — they cost the same RU and are throttled individually, so this does not reduce 429s; (g) seed the business ceiling from the per-user 3,000/5 min (≈10 req/s) figure, personal from discovery only.
3. Testing: the in-process fake must model the two documented facts that make the incident expensive — a token bucket per drive *and* "throttled requests count towards usage limits", i.e. requests inside the penalty extend it; assert zero requests between a 429 and its deadline, batch sizes 1,2,4,… after it, and the ssthresh/AIMD behaviour once added. Also run the client through Microsoft's **Dev Proxy** (`devproxy config get microsoft-graph-rate-limiting`), which simulates 429/Retry-After on the real URLs with no account at all. A live POC is still worth one short run because fragment metering and personal-account behaviour are unconfirmed: test account only, 2 workers, ≤ 200 files of a few KB, ≤ 10 minutes, off-peak, stop at the first 429 and verify the pause and the ramp from the log timestamps; do not try to provoke a second throttle — Microsoft: "Load testing isn't allowed on SharePoint Online", personal Retry-After values of about an hour have been observed, and persistent offenders "will be blocked due to abusive calling patterns".
