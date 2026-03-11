# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go, internal/retry/transport.go

Implements: R-6.8.1 [verified], R-6.8.2 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.10 [verified], R-6.8.11 [verified]

## Overview

Leaf package (stdlib + `net/http` + `log/slog` only). Provides reusable retry infrastructure used by `graph/`, `sync/`, and `driveops/`.

## Policy

Immutable configuration for exponential backoff with jitter. Fields: `MaxAttempts` (0 = infinite), `InitialDelay`, `MaxDelay`, `Multiplier`. Safe for concurrent use.

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
|--------|----------|-------------|---------|-----|
| `Transport` | HTTP retry via `RetryTransport` for CLI callers | 5 | 1s | 60s |
| `DriveDiscovery` | Drive enumeration retry | 3 | 1s | 60s |
| `WatchLocal` | Local observer error recovery | 0 (infinite) | 1s | 30s |
| `WatchRemote` | Remote observer error recovery | 0 (infinite) | 5s | 5m |

Watch-mode policies use `MaxAttempts = 0` (retry forever) because a daemon should never give up permanently.

**Deleted policies:**
- `Action` — superseded by engine-level classification + tracker re-queue.
- `SyncTransport` — sync callers use raw `http.DefaultTransport` directly (no named policy constant needed). The graph client is constructed without a retry transport for sync, so failed requests return immediately for engine classification.
- `Reconcile` — the reconciler retry loop has been eliminated. The tracker is the sole retry mechanism. `sync_failures` is diagnostic-only.

## RetryTransport (`transport.go`)

Implements: R-6.8.8 [verified]

Standard `http.RoundTripper` that wraps an inner transport with automatic retry on transient failures. Separates retry responsibility from the graph client — the client is a pure API mapper, the transport handles resilience.

Architecture:
- **CLI callers**: `RetryTransport{Inner: http.DefaultTransport, Policy: Transport}` → 5 retries with exponential backoff
- **Sync callers**: raw `http.DefaultTransport` (no RetryTransport wrapper) → failed requests return immediately for engine-level classification and tracker re-queuing (R-6.8.7: workers never block on retry backoff)

Features:
- Exponential backoff with jitter per `Policy`
- `Retry-After` header parsing for 429/503 responses
- Account-wide 429 throttle coordination: when any request gets 429, all subsequent requests through the same transport wait until the deadline passes
- Seekable body rewinding between attempts (via `req.GetBody` or `io.Seeker` fallback)
- `X-Retry-Count` header annotation on retried requests
- Retryable status codes: 408, 429, 500, 502, 503, 504, 509

Thread-safe. All mutable state (throttle deadline) is mutex-protected.

## Backoff

Stateful wrapper around Policy for watch loops. Tracks consecutive error count. Not thread-safe — intended for single-goroutine loops. Supports dynamic max override (e.g., capped to poll interval for remote observer).

### Rationale

- **No circuit breaker**: `circuit.go` deleted — was dead code (never imported in production). Superseded by scope-based blocking with trial actions. Circuit breakers are appropriate for multi-service architectures, not a single-API client.
- **Transient failures retry forever in watch mode**: The tracker retries indefinitely (MaxAttempts = 0) with exponential backoff capped at 5 minutes.
- **One retry mechanism**: The tracker is the sole retry mechanism. `sync_failures` is diagnostic-only. The `FailureRetrier` retry loop has been eliminated. Crash recovery is handled by `ResetInProgressStates` on startup.
- **No escalation**: Transient failures don't become permanent. Actionable failures are classified at detection time based on HTTP status and error type.

## Scope-Based Retry with Trial Actions

Implements: R-2.10.3 [verified], R-2.10.5 [verified], R-2.10.6 [verified], R-2.10.7 [verified], R-2.10.8 [verified], R-2.10.14 [verified], R-6.8.7 [verified], R-6.8.10 [verified], R-6.8.11 [verified]

- **No transport-level retry for sync**: Sync callers use raw `http.DefaultTransport`. Each sync dispatch = one HTTP request. Workers never block on retry backoff. Failed actions returned to the tracker with `NotBefore` + incremented attempt counter.
- **In-memory retry budget**: One-shot mode: 5 attempts per action. Budget exhaustion → `tracker.Complete()` as failed, recorded in `sync_failures` as diagnostic. Next `onedrive sync` run replans naturally. Watch mode: unlimited attempts (MaxAttempts = 0), retries forever with increasing backoff.
- **Unified non-compounding backoff**: `min(1s * 2^attempt, 5min)` → 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 300s, 300s... Single mechanism, no reconciler handoff.
- **Trial actions**: Scope block recovery via real action probes. When a scope is blocked, one held action is dispatched as a trial after a scope-type-specific delay. Success → release all held actions. Failure → extend block with backoff. Per-scope-type timing: rate_limited uses `Retry-After`; quota: 5m→1h; service_outage: 60s→10m.
- **Deleted code**: `circuit.go` + `circuit_test.go` (dead code). `SyncTransport` policy (unused). `Reconcile` policy (no longer drives retry). `isRetryable()` from graph/errors.go (orphaned). `FailureRetrier` retry loop from `reconciler.go` (tracker replaces it).
