# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go, internal/retry/transport.go

Implements: R-6.8.1 [verified], R-6.8.2 [verified], R-6.8.7 [planned], R-6.8.8 [verified], R-6.8.10 [planned], R-6.8.11 [planned]

## Overview

Leaf package (stdlib + `net/http` + `log/slog` only). Provides reusable retry infrastructure used by `graph/`, `sync/`, and `driveops/`.

## Policy

Immutable configuration for exponential backoff with jitter. Fields: `MaxAttempts` (0 = infinite), `InitialDelay`, `MaxDelay`, `Multiplier`. Safe for concurrent use.

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
|--------|----------|-------------|---------|-----|
| `Transport` | HTTP retry via `RetryTransport` for CLI callers | 5 | 1s | 60s |
| `DriveDiscovery` | Drive enumeration retry | 3 | 1s | 60s |
| `SyncTransport` | Sync action dispatch — single attempt, no retry (workers never block) | 0 (single attempt) | — | — |
| `Reconcile` | Reconciler retry scheduling | 0 (infinite) | 30s | 1h |
| `WatchLocal` | Local observer error recovery | 0 (infinite) | 1s | 30s |
| `WatchRemote` | Remote observer error recovery | 0 (infinite) | 5s | 5m |

Watch-mode policies use `MaxAttempts = 0` (retry forever) because a daemon should never give up permanently.

`Action` policy was deleted — superseded by `SyncTransport` (zero retries at transport layer) + engine-level result classification + tracker re-queue.

## RetryTransport (`transport.go`)

Implements: R-6.8.8 [verified]

Standard `http.RoundTripper` that wraps an inner transport with automatic retry on transient failures. Separates retry responsibility from the graph client — the client is a pure API mapper, the transport handles resilience.

Architecture:
- **CLI callers**: `RetryTransport{Inner: http.DefaultTransport, Policy: Transport}` → 5 retries with exponential backoff
- **Sync callers**: raw `http.DefaultTransport` → failed requests return immediately for engine-level classification and tracker re-queuing (R-6.8.7: workers never block on retry backoff)

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

- **No circuit breaker**: `circuit.go` is dead code — never imported in production. Will be deleted. Superseded by scope-based blocking with trial actions. Circuit breakers are appropriate for multi-service architectures, not a single-API client.
- **Transient failures retry forever**: In daemon mode, transient failures (network, 5xx) should always recover eventually. `Reconcile.MaxAttempts = 0`.
- **No escalation**: Removed. Transient failures don't become permanent. Actionable failures are classified at detection time based on HTTP status and error type.

## Planned: Scope-Based Retry with Trial Actions

Implements: R-2.10.3 [planned], R-2.10.4 [planned], R-2.10.5 [planned], R-2.10.6 [planned], R-2.10.7 [planned], R-2.10.8 [planned], R-2.10.14 [planned], R-6.8.7 [planned], R-6.8.10 [planned], R-6.8.11 [planned]

- **SyncTransport policy**: `MaxAttempts: 0` (single attempt). Each sync dispatch = one HTTP request. Workers never block on retry backoff. Failed actions returned to the tracker with `NotBefore` + incremented attempt counter.
- **In-memory retry budget**: Default 5 attempts per action. Budget exhaustion → escalate to `sync_failures` with `next_retry_at` for reconciler-level retry.
- **Unified non-compounding backoff**: Tracker re-queue (1s, 2s, 4s, 8s, 16s) then reconciler (30s, 60s, 120s… max 1h). Sequential phases, not nested loops. A single action traverses tracker backoff first; only after budget exhaustion does it enter reconciler backoff.
- **Trial actions**: Scope block recovery via real action probes. When a scope is blocked, one held action is dispatched as a trial after a scope-type-specific delay. Success → release all held actions. Failure → extend block with backoff. Per-scope-type timing: rate_limited uses `Retry-After`; quota: 5m→1h; service_outage: 60s→10m.
- **Action policy deletion**: `Action` policy superseded by `SyncTransport` + engine error classification + tracker re-queue. Will be removed.
- **Circuit breaker deletion**: `circuit.go` is dead code (never imported in production). Scope-based blocking with trial actions supersedes classic circuit breaker. Will be deleted.
