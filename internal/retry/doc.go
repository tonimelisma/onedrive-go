// Package retry provides unified retry policies and stateful backoff for the
// onedrive-go sync engine.
//
// The package consolidates independent retry/backoff implementations into a
// single set of primitives:
//
//   - [Policy] defines an immutable exponential backoff configuration (base delay,
//     multiplier, jitter, max attempts, max delay). Named policy instances capture
//     the exact parameters used at each layer of the retry cascade.
//
//   - [Backoff] wraps a Policy with mutable state for long-running loops (observer
//     watch loops) that need to track consecutive errors across iterations.
//
// # Named Policies
//
// Named policies preserve exact behavior from their original implementations:
//
//   - [Transport]: HTTP transport retries (graph/client.go) — 5 attempts, 1s-60s.
//   - [DriveDiscovery]: Transient 403 retry during drive enumeration — 3 attempts.
//   - [WatchLocal]: Local observer error backoff — infinite, 1s-30s, no jitter.
//   - [WatchRemote]: Remote observer error backoff — infinite, 5s-dynamic, no jitter.
//
// # Exclusions
//
// The hash retry in driveops/transfer_manager.go (downloadWithHashRetry, max 2
// retries, no backoff) is intentionally excluded — it's a simple 10-line loop
// with no backoff, and wrapping it in Policy would add complexity for no benefit.
package retry
