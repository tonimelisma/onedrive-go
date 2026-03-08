// Package retry provides unified retry policies, stateful backoff, and circuit
// breaker for the onedrive-go sync engine.
//
// The package consolidates six independent retry/backoff implementations into a
// single set of primitives:
//
//   - [Policy] defines an immutable exponential backoff configuration (base delay,
//     multiplier, jitter, max attempts, max delay). Named policy instances capture
//     the exact parameters used at each layer of the retry cascade.
//
//   - [Backoff] wraps a Policy with mutable state for long-running loops (observer
//     watch loops) that need to track consecutive errors across iterations.
//
//   - [CircuitBreaker] prevents futile requests during service-wide outages. When
//     failures exceed a threshold within a sliding window, the breaker trips open
//     and rejects requests until a cooldown elapses. A half-open probe allows a
//     single request through to test recovery.
//
// # Named Policies
//
// Six named policies preserve exact behavior from their original implementations:
//
//   - [Transport]: HTTP transport retries (graph/client.go) — 5 attempts, 1s-60s.
//   - [DriveDiscovery]: Transient 403 retry during drive enumeration — 3 attempts.
//   - [Action]: Executor-level retries (sync/executor.go) — 3 attempts, 1s+.
//   - [Reconcile]: Failure retrier scheduling (sync/baseline.go) — 10 attempts, 30s-1h.
//   - [WatchLocal]: Local observer error backoff — infinite, 1s-30s, no jitter.
//   - [WatchRemote]: Remote observer error backoff — infinite, 5s-dynamic, no jitter.
//
// # Exclusions
//
// The hash retry in driveops/transfer_manager.go (downloadWithHashRetry, max 2
// retries, no backoff) is intentionally excluded — it's a simple 10-line loop
// with no backoff, and wrapping it in Policy would add complexity for no benefit.
package retry
