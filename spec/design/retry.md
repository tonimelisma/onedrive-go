# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go, internal/retry/transport.go

Implements: R-6.8.1 [verified], R-6.8.2 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.10 [verified], R-6.8.11 [verified], R-6.6.8 [verified]

## Overview

Leaf package (stdlib + `net/http` + `log/slog` only). Provides reusable retry infrastructure used by `graph/`, `sync/`, and `driveops/`.

## Policy

Immutable configuration for exponential backoff with jitter. Fields: `MaxAttempts` (0 = infinite), `Base`, `Max`, `Multiplier`, `Jitter`. Safe for concurrent use.

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
|--------|----------|-------------|---------|-----|
| `Transport` | HTTP retry via `RetryTransport` for CLI callers | 5 | 1s | 60s |
| `DriveDiscovery` | Drive enumeration retry | 3 | 1s | 60s |
| `WatchLocal` | Local observer error recovery | 0 (infinite) | 1s | 30s |
| `Reconcile` | Single retry curve for all sync failures (`sync_failures` + `FailureRetrier`) | 0 (infinite) | 1s | 1h |
| `WatchRemote` | Remote observer error recovery | 0 (infinite) | 5s | 5m |

Watch-mode and reconcile policies use `MaxAttempts = 0` (retry forever) because a daemon should never give up permanently.

The `Reconcile` policy defines the single retry curve for all transient sync failures. Parameters: 1s base, 2× multiplier, ±25% jitter, 1h max, infinite attempts. Curve: 1s, 2s, 4s, 8s, ..., 3600s cap. The engine passes `retry.Reconcile.Delay` as the `delayFn` argument to `SyncStore.RecordFailure()` for transient failures. The store computes `next_retry_at` from the failure count without importing the retry package — it receives `delayFn` as a `func(int) time.Duration`.

**Deleted policies:**
- `Action` — superseded by engine-level classification + `sync_failures` recording.
- `SyncTransport` — sync callers use raw `http.DefaultTransport` directly (no named policy constant needed). The graph client is constructed without a retry transport for sync, so failed requests return immediately for engine classification.

## RetryTransport (`transport.go`)

Implements: R-6.8.8 [verified]

Standard `http.RoundTripper` that wraps an inner transport with automatic retry on transient failures. Separates retry responsibility from the graph client — the client is a pure API mapper, the transport handles resilience.

Architecture:
- **CLI callers**: `RetryTransport{Inner: http.DefaultTransport, Policy: Transport}` → 5 retries with exponential backoff
- **Sync callers**: raw `http.DefaultTransport` (no RetryTransport wrapper) → failed requests return immediately for engine-level classification and `sync_failures` recording (R-6.8.7: workers never block on retry backoff)

Features:
- Exponential backoff with jitter per `Policy`
- `Retry-After` header parsing for 429/503 responses
- Account-wide 429 throttle coordination: when any request gets 429, all subsequent requests through the same transport wait until the deadline passes
- Seekable body rewinding between attempts (via `req.GetBody` or `io.Seeker` fallback)
- `X-Retry-Count` header annotation on retried requests
- Retryable status codes: 408, 429, 500, 502, 503, 504, 509
- ERROR log on retry exhaustion: when all attempts are spent (network error or retryable HTTP status), logs "request failed after all retries" at ERROR with method, URL, attempt count, and error/status. Implements: R-6.6.8 [verified]

Thread-safe. All mutable state (throttle deadline) is mutex-protected.

## Backoff

Stateful wrapper around Policy for watch loops. Tracks consecutive error count. Not thread-safe — intended for single-goroutine loops. Supports dynamic max override (e.g., capped to poll interval for remote observer).

### Rationale

- **No circuit breaker**: `circuit.go` deleted — was dead code (never imported in production). Superseded by scope-based blocking with trial actions. Circuit breakers are appropriate for multi-service architectures, not a single-API client.
- **Single retry mechanism**: The `FailureRetrier` + `sync_failures` table is the sole retry mechanism for sync actions. The tracker is purely a dependency graph + scope-aware dispatch gate — no retry logic (no ReQueue, no delayed queue, no NotBefore, no Attempt, no MaxAttempts). Failed items are recorded in `sync_failures` with `next_retry_at` computed via `retry.Reconcile.Delay`, and the `FailureRetrier` re-injects due items via buffer → planner → tracker.
- **Transient failures retry forever**: The `Reconcile` policy has `MaxAttempts = 0` (infinite) with exponential backoff capped at 1 hour. Crash recovery is handled by `ResetInProgressStates` on startup.
- **No escalation**: Transient failures don't become permanent. Actionable failures are classified at detection time based on HTTP status and error type.

## Scope-Based Retry with Trial Actions

Implements: R-2.10.3 [verified], R-2.10.5 [verified], R-2.10.6 [planned], R-2.10.7 [planned], R-2.10.8 [planned], R-2.10.14 [planned], R-6.8.7 [verified], R-6.8.10 [verified], R-6.8.11 [verified]

- **No transport-level retry for sync**: Sync callers use raw `http.DefaultTransport`. Each sync dispatch = one HTTP request. Workers never block on retry backoff. Failed actions are recorded in `sync_failures` with `next_retry_at` computed via `retry.Reconcile.Delay`, and the `FailureRetrier` re-injects due items.
- **Single retry mechanism**: `sync_failures` (SQLite) + `FailureRetrier` (reconciler). The tracker is purely a dependency graph + scope-aware dispatch gate — no retry logic. The engine always calls `tracker.Complete()` on every result (never `ReQueue`). Transient failures are recorded with `retry.Reconcile.Delay` (1s base, 2× multiplier, ±25% jitter, 1h max, infinite attempts). Actionable/fatal failures are recorded with nil `delayFn` (no `next_retry_at`).
- **FailureRetrier**: Periodically sweeps `sync_failures` for items whose `next_retry_at` has expired. Re-injects them as synthetic `ChangeEvent`s via buffer → planner → tracker (Alternative A from §12.4). Skips items already in-flight. Arms a timer for the earliest future retry. Safety sweep every 2 minutes. Kick signal from engine on each failure recording.
- **Trial actions**: Scope block recovery via real action probes. `TrackedAction` has `IsTrial` and `TrialScopeKey` fields. `DispatchTrial(key)` pops one from held queue and marks it as trial. `NextDueTrial(now)` returns the first due trial. `ExtendTrial(key, nextAt)` extends on failure. Success → `ReleaseScope` (unblock all held). Failure → `ExtendTrial` with 2× backoff. Per-scope-type timing: rate_limited uses `Retry-After`; quota: 5m→1h; service_outage: 60s→10m.
- **Deleted code**: `circuit.go` + `circuit_test.go` (dead code). `SyncTransport` policy (unused). `isRetryable()` from graph/errors.go (orphaned).
