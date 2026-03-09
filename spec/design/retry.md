# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go

Implements: R-6.8.1 [verified], R-6.8.2 [verified], R-6.8.7 [planned], R-6.8.8 [planned], R-6.8.10 [planned], R-6.8.11 [planned]

## Overview

Leaf package (stdlib only). Provides reusable retry infrastructure used by `graph/`, `sync/`, and `driveops/`.

## Policy

Immutable configuration for exponential backoff with jitter. Fields: `MaxAttempts` (0 = infinite), `InitialDelay`, `MaxDelay`, `Multiplier`. Safe for concurrent use.

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
|--------|----------|-------------|---------|-----|
| `Transport` | HTTP request retry in graph client | finite | short | moderate |
| `DriveDiscovery` | Drive enumeration retry | finite | short | moderate |
| `Action` | Sync action retry (download/upload/delete) — **to be deleted**, superseded by SyncTransport + engine-level retry | finite | short | moderate |
| `SyncTransport` | Sync action dispatch — single attempt, no retry | 0 (single attempt) | — | — |
| `Reconcile` | Reconciler retry scheduling | 0 (infinite) | moderate | long |
| `WatchLocal` | Local observer error recovery | 0 (infinite) | short | long |
| `WatchRemote` | Remote observer error recovery | 0 (infinite) | short | long |

Watch-mode policies use `MaxAttempts = 0` (retry forever) because a daemon should never give up permanently.

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
