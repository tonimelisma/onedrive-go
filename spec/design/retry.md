# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/circuit.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go

Implements: R-6.8.1 [implemented], R-6.8.2 [implemented]

## Overview

Leaf package (stdlib only). Provides reusable retry infrastructure used by `graph/`, `sync/`, and `driveops/`.

## Policy

Immutable configuration for exponential backoff with jitter. Fields: `MaxAttempts` (0 = infinite), `InitialDelay`, `MaxDelay`, `Multiplier`. Safe for concurrent use.

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
|--------|----------|-------------|---------|-----|
| `Transport` | HTTP request retry in graph client | finite | short | moderate |
| `DriveDiscovery` | Drive enumeration retry | finite | short | moderate |
| `Action` | Sync action retry (download/upload/delete) | finite | short | moderate |
| `Reconcile` | Reconciler retry scheduling | 0 (infinite) | moderate | long |
| `WatchLocal` | Local observer error recovery | 0 (infinite) | short | long |
| `WatchRemote` | Remote observer error recovery | 0 (infinite) | short | long |

Watch-mode policies use `MaxAttempts = 0` (retry forever) because a daemon should never give up permanently.

## Backoff

Stateful wrapper around Policy for watch loops. Tracks consecutive error count. Not thread-safe — intended for single-goroutine loops. Supports dynamic max override (e.g., capped to poll interval for remote observer).

### Rationale

- **No circuit breaker**: Removed from graph client. Replaced by account-wide 429 throttle gate. Circuit breakers are appropriate for multi-service architectures, not a single-API client.
- **Transient failures retry forever**: In daemon mode, transient failures (network, 5xx) should always recover eventually. `Reconcile.MaxAttempts = 0`.
- **No escalation**: Removed. Transient failures don't become permanent. Actionable failures are classified at detection time based on HTTP status and error type.
