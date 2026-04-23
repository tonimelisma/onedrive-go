# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go, internal/retry/throttle_gate.go, internal/retry/transport.go

Implements: R-6.8.1 [verified], R-6.8.2 [verified], R-6.8.7 [designed], R-6.8.8 [verified], R-6.8.10 [designed], R-6.8.11 [designed], R-6.6.8 [verified], R-2.10.3 [designed], R-2.10.5 [designed], R-2.10.6 [designed], R-2.10.7 [designed], R-2.10.8 [designed], R-2.10.14 [designed], R-6.8.16 [designed], R-6.10.6 [verified]

## Overview

`internal/retry/` is a small leaf package providing reusable exponential
backoff policies, the optional HTTP retry transport used by CLI-style callers,
and the narrow shared throttle-gate primitive that lets callers coordinate
`Retry-After` deadlines across multiple retry transports. Sync itself does not
rely on transport-level retry loops for action execution.

## Ownership Contract

- Owns: backoff policy definitions, retry timing, the optional CLI-oriented
  HTTP retry transport, and the narrow `ThrottleGate` coordination primitive
- Does Not Own: Graph wire normalization, sync result classification,
  scope activation, durable retry persistence, or user-facing status messaging
- Source of Truth: `retry.Policy` values plus the HTTP/request outcomes
  presented to `RetryTransport`
- Allowed Side Effects: sleeping/backoff and HTTP request redispatch inside
  `RetryTransport`
- Mutable Runtime Owner: backoff counters are request-scoped. Shared 429
  coordination is optional and explicitly caller-owned via `ThrottleGate`.
- Error Boundary: `retry` consumes already-normalized retryable signals from
  [error-model.md](error-model.md). It never upgrades actionable or fatal
  conditions into retryable ones on its own.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Retry policy curves and backoff timing remain package-owned value logic. | `internal/retry/backoff_test.go`, `internal/retry/policy_test.go`, `internal/retry/named_test.go` |
| Shared `Retry-After` coordination stays optional and caller-injected. | `internal/retry/throttle_gate_test.go`, `internal/retry/transport_test.go` |
| CLI/control-plane callers use retry without reinterpreting retryable versus fatal conditions themselves. | `internal/cli/control_client_additional_test.go`, `internal/multisync/orchestrator_test.go`, `internal/graph/client_test.go` |

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
| --- | --- | --- | --- | --- |
| `TransportPolicy()` | HTTP retry via `RetryTransport` for CLI callers | 5 | 1s | 60s |
| `DriveDiscoveryPolicy()` | Drive enumeration retry | 5 | 1s | 60s |
| `RootChildrenPolicy()` | Exact `root/children` quirk retry | 3 | 250ms | 1s |
| `DownloadMetadataPolicy()` | Exact item-by-ID download metadata quirk retry | 4 | 250ms | 2s |
| `SimpleUploadMtimePatchPolicy()` | Exact post-simple-upload `UpdateFileSystemInfo` retry | 7 | 250ms | 8s |
| `UploadSessionCreatePolicy()` | Exact create-upload-session fresh-parent quirk retry | 6 | 250ms | 4s |
| `SimpleUploadCreatePolicy()` | Final simple-upload create retry after session-path disambiguation | 7 | 250ms | 8s |
| `PathVisibilityPolicy()` | Post-success path-read/delete convergence at the CLI/session boundary | 10 | 250ms | 32s |
| `WatchLocalPolicy()` | Local observer error recovery | 0 (infinite) | 1s | 30s |
| `ReconcilePolicy()` | Engine-owned durable retry timing for `retry_work` | 0 (infinite) | 1s | 1h |
| `WatchRemotePolicy()` | Remote observer error recovery | 0 (infinite) | 5s | 5m |

`ReconcilePolicy()` is the single retry curve for durable transient work in the
sync engine. The engine computes `next_retry_at` from that policy and persists
the result into `retry_work`.

## RetryTransport (`transport.go`)

`RetryTransport` is used for CLI-style request/response flows that benefit from
automatic transport retry.

Features:

- exponential backoff with jitter per `Policy`
- `Retry-After` parsing for 429/503 responses
- optional caller-injected shared 429 deadline coordination via `ThrottleGate`
- seekable request-body rewinding between attempts
- `X-Retry-Count` header annotation on retried requests
- request-scoped log target override via `WithLogTarget(ctx, target)`
- retryable status codes: 408, 429, 500, 502, 503, 504, 509

Sync action execution does not use `RetryTransport`. Workers issue one request,
return the result immediately, and let the engine classify it.

## Sync Retry Model

Sync has exactly two durable retry mechanisms:

1. per-item delayed work through `retry_work.next_retry_at`
2. shared-scope recovery through persisted `block_scopes` plus real trial
   actions

The dependency graph is not a retry system. Workers never sleep for durable
backoff. Failed actions are recorded durably and reintroduced later by the
engine.

### Per-item transient retry

- transient exact work is persisted in `retry_work`
- delayed exact retry rows use the store's retry-failure helper; blocked scope
  rows use the store's blocked-retry helper, but both land in the same durable
  `retry_work` table
- `next_retry_at` is computed from `ReconcilePolicy()`
- current-plan preparation prunes stale retry rows against the latest
  actionable set and loads the surviving rows into runtime state
- when an exact action becomes dependency-ready, admission consults its
  persisted retry row and either dispatches it now or holds it in runtime
  until `next_retry_at`
- the engine-owned held retry release releases only those already-held exact
  actions whose retry time is now due; it does not refresh truth, rebuild
  plans, or reconstruct dependencies
- durable current-truth/content problems belong in `observation_issues`, not in
  retry scheduling

### Scope retry via trial actions

When the engine activates a block scope, blocked descendants remain in
`retry_work` with `blocked=true` and a matching `scope_key`. Recovery happens
through trial actions:

- only dependency-ready exact actions enter held blocked runtime state, so a
  due trial always chooses from actions that are runnable if the scope is
  released
- the engine-owned held trial release picks one deterministic held blocked
  candidate for each due scope and dispatches it as a trial without rebuilding
  plan structure
- success releases the scope
- matching-scope persistence evidence extends the scope
- inconclusive outcomes re-arm the current interval without inventing a new
  blocker row, but only while blocked `retry_work` still exists for the scope
- scope release makes blocked retry rows ready in store and immediately flips
  the corresponding held runtime entries into due exact retry work
- once a scope has no blocked `retry_work`, the engine discards the empty
  `block_scopes` row immediately instead of preserving timing with no work

Blocked retry rows use one engine-owned durable shape no matter which runtime
path persisted them: `blocked=true`, the exact `scope_key`, and the retry/trial
timing needed to re-admit the work. Condition family, HTTP status, and error
text stay in runtime classification and logs rather than durable retry rows.

`block_scopes` remains the only durable owner of shared trial timing.

### Trial timing

Trial timing is scope-aware:

- `throttle:target:*` and `service` honor server `Retry-After` exactly when
  present
- all locally timed scopes use a fixed 60-second interval when server timing
  is absent

### Restart semantics

Startup normalization applies persisted-scope policy before admission begins:

- current-plan preparation prunes stale `retry_work` against the latest
  actionable set before runtime startup
- active `block_scopes` are reloaded and may become immediate trials when due,
  but empty scopes are pruned during startup normalization
- observation-owned read boundaries are re-derived only from current
  observation, not from persisted `block_scopes`
- account-auth state remains catalog-owned, not a retry scope

Timer release never revalidates current truth. Once startup preparation has
loaded surviving `retry_work` / `block_scopes`, held exact work is released
only from runtime-owned held state. Released exact work re-enters the engine's
ready frontier reduction before any worker dispatch; timer callbacks do not own
or append directly to a worker queue. In code, the shared current-truth
observe/load/build/reconcile pipeline lives in `engine_current_plan.go`,
startup normalization lives in `engine_startup.go`, runtime admission and
publication drain live in `engine_runtime_start.go`, and completion-time
persistence, held release, and trial reclassification now live in one
`engine_runtime_*` family rather than being spread across controller-shaped
helpers.

## Deleted Mechanisms

The target sync engine does not carry:

- a mixed failure/reporting ledger
- failure-role transitions such as `item`, `held`, or `boundary`
- a retry queue inside the dependency graph
- transport-level retry for sync workers

The durable model is:

- `retry_work` for pending retryable or blocked work
- `block_scopes` for shared trial timing and blocker lifecycle
- `observation_issues` for durable current-truth/content problems
- engine-owned retry/trial loops
