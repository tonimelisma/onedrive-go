# Retry

GOVERNS: internal/retry/backoff.go, internal/retry/doc.go, internal/retry/named.go, internal/retry/policy.go, internal/retry/throttle_gate.go, internal/retry/transport.go

Implements: R-6.8.1 [verified], R-6.8.2 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.10 [verified], R-6.8.11 [verified], R-6.6.8 [verified], R-2.10.3 [verified], R-2.10.5 [verified], R-2.10.6 [verified], R-2.10.7 [verified], R-2.10.8 [verified], R-2.10.14 [verified], R-6.8.16 [verified], R-6.10.6 [verified]

## Overview

`internal/retry/` is a small leaf package providing reusable exponential
backoff policies, the optional HTTP retry transport used by CLI-style
callers, and the narrow shared throttle-gate primitive that lets callers
coordinate `Retry-After` deadlines across multiple retry transports.
Sync itself does not rely on transport-level retry loops for action
execution.

## Ownership Contract

- Owns: Backoff policy definitions, retry timing, the optional CLI-oriented HTTP retry transport, and the narrow `ThrottleGate` coordination primitive.
- Does Not Own: Graph wire normalization, sync failure classification, scope activation, durable retry persistence, or user-facing error messaging.
- Source of Truth: `retry.Policy` values plus the HTTP/request outcomes presented to `RetryTransport`.
- Allowed Side Effects: Sleeping/backoff and HTTP request redispatch inside `RetryTransport`.
- Mutable Runtime Owner: Backoff counters are request-scoped. Shared 429 coordination is optional and explicitly caller-owned via `ThrottleGate`; `RetryTransport` only consults or mutates that gate when one is injected. The package has no package-level mutable state, background goroutines, or long-lived channels.
- Error Boundary: `retry` consumes already-normalized retryable signals from [error-model.md](error-model.md). It never upgrades actionable or fatal conditions into retryable ones on its own.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Retry policy curves and backoff timing remain package-owned value logic. | `internal/retry/backoff_test.go`, `internal/retry/policy_test.go`, `internal/retry/named_test.go` |
| Shared `Retry-After` coordination stays optional and caller-injected. | `internal/retry/throttle_gate_test.go`, `internal/retry/transport_test.go` |
| CLI/control-plane callers use retry without reinterpreting retryable versus fatal conditions themselves. | `internal/cli/control_client_additional_test.go`, `internal/multisync/orchestrator_test.go`, `internal/graph/client_test.go` |

## Named Policies

| Policy | Use Case | MaxAttempts | Initial | Max |
|--------|----------|-------------|---------|-----|
| `TransportPolicy()` | HTTP retry via `RetryTransport` for CLI callers | 5 | 1s | 60s |
| `DriveDiscoveryPolicy()` | Drive enumeration retry | 5 | 1s | 60s |
| `RootChildrenPolicy()` | Exact `root/children` quirk retry | 3 | 250ms | 1s |
| `DownloadMetadataPolicy()` | Exact item-by-ID download metadata quirk retry | 4 | 250ms | 2s |
| `UploadSessionCreatePolicy()` | Exact create-upload-session fresh-parent quirk retry | 6 | 250ms | 4s |
| `SimpleUploadCreatePolicy()` | Final simple-upload create retry after session-path disambiguation | 7 | 250ms | 8s |
| `PathVisibilityPolicy()` | Post-success path-read/delete convergence at the CLI/session boundary | 10 | 250ms | 32s |
| `WatchLocalPolicy()` | Local observer error recovery | 0 (infinite) | 1s | 30s |
| `ReconcilePolicy()` | Per-item transient retry in `sync_failures` | 0 (infinite) | 1s | 1h |
| `WatchRemotePolicy()` | Remote observer error recovery | 0 (infinite) | 5s | 5m |

`ReconcilePolicy()` is the single retry curve for transient item failures in
the sync engine. The store never imports the retry package directly; the engine
passes `retry.ReconcilePolicy().Delay` into `RecordFailure`.

`DriveDiscoveryPolicy()` is intentionally longer than the earlier
three-attempt budget because `/me/drives` is a discovery/catalog surface, not a
hot-path file transfer. The retry still belongs to the graph boundary; caller
degraded-mode behavior after exhaustion belongs to `internal/cli`, not
`internal/retry`.

`DownloadMetadataPolicy()` stays short and deterministic because it covers one
exact Graph quirk: a just-created file can appear in delta or path-based lookup
before `GET /drives/{driveID}/items/{itemID}` is readable enough to return the
download URL. The retry belongs to `graph.Download()` / `DownloadRange()`,
not to transfer-manager callers or generic transport retry.

`UploadSessionCreatePolicy()` is similarly narrow. It covers the observed case
where a freshly created parent folder is already path-resolvable, but
`POST /drives/{driveID}/items/{parentID}:/{name}:/createUploadSession` still
misfires with `404 itemNotFound` for longer than the original 2s budget on
live full E2E. The
retry belongs to `graph.CreateUploadSession()`, not to CLI callers or generic
upload retry logic.

`SimpleUploadCreatePolicy()` is the longer post-disambiguation create budget.
It only applies after the session-route permission oracle has already exhausted
on exact `itemNotFound`, so the graph boundary can spend a little longer
replaying the original simple upload without slowing the shared-folder
permission check itself.

`PathVisibilityPolicy()` is deterministic on purpose. `driveops` is the only
runtime owner that consumes it. `driveops.Session` reuses the policy for two
command-boundary convergence checks:

- destination-path readability after successful `mkdir`, `put`, and `mv`
- path-authoritative delete convergence after a successful path lookup followed
  by transient `DELETE .../items/{id} = 404 itemNotFound`

The current schedule keeps the same `250ms` exponential ramp but now holds the
`32s` ceiling for three capped sleeps, which yields about `95.75s` of
deterministic wait before request overhead and roughly a two-minute wall-clock
budget once the repeated Graph reads are included. Live Graph evidence has
shown minute-scale path-read model lag after successful writes and deletes, so
the shorter old budget was no longer enough to distinguish a real missing path
from delayed visibility.

The sync execution path does not carry an independent visibility schedule or sleep loop.
When sync wants best-effort confirmation after a successful remote create,
upload, or move, it delegates to the injected
`driveops.PathConvergenceFactory`, which returns the correct drive/root-scoped
capability for the action target. The retry schedule still lives entirely in
`driveops`; execution only decides which target session should consume it.

## RetryTransport (`transport.go`)

`RetryTransport` is used for CLI-style request/response flows that benefit from
automatic transport retry.

Features:
- exponential backoff with jitter per `Policy`
- `Retry-After` parsing for 429/503 responses
- optional caller-injected shared 429 deadline coordination via `ThrottleGate`
- seekable request-body rewinding between attempts
- `X-Retry-Count` header annotation on retried requests
- request-scoped log target override via `WithLogTarget(ctx, target)` so callers can replace sensitive URLs with redacted descriptors
- retryable status codes: 408, 429, 500, 502, 503, 504, 509
- retry-attempt logs are DEBUG-only; once retries are exhausted, the transport emits a single WARN with method, redacted target, attempt count, and final error or status

Sync action execution does not use `RetryTransport`. Workers issue one request,
return the result immediately, and let the engine classify it.

`ThrottleGate` is intentionally tiny. It owns only a shared deadline and
wait operation. Callers that understand the throttle domain create it and own
its lifetime. Today `driveops.SessionRuntime` creates one gate per interactive
metadata target profile: own-drive traffic keys by `(account email, driveID)`,
and shared-target traffic keys by `(account email, remoteDriveID, remoteItemID)`.
The retry package does not infer account, tenant, drive, or caller identity on
its own.

## Sync Retry Model

Sync has exactly two retry mechanisms:

1. per-item transient retry through `sync_failures.next_retry_at`
2. scope recovery through persisted `scope_blocks` plus real trial actions

The dependency graph is not a retry system. Workers never sleep for retry
backoff. Failed actions are recorded durably and reintroduced later by the
engine.

### Per-item transient retry

- transient failures are recorded in `sync_failures` with `failure_role='item'`
- `next_retry_at` is computed from `ReconcilePolicy()`
- the engine-owned retry sweep rebuilds planner input from durable state and feeds it back through the normal planner -> dispatch pipeline
- upload-side redispatch uses `ObserveSinglePath()` so retry/trial reconstruction follows the same local validation, oversized-file, and empty-hash-on-failure rules as normal observation

### Scope retry via trial actions

When the engine activates a scope block, blocked descendants are recorded in
`sync_failures` with `failure_role='held'`. Recovery happens through trial
actions:

- `runTrialDispatch()` picks a held row for each due scope via `PickTrialCandidate`
- `createEventFromDB()` / the retry-trial rebuild path reconstruct planner input from current durable state or single-path local observation
- success -> `releaseScope`
- matching-scope persistence evidence -> `extendScopeTrial`
- inconclusive trial outcomes -> `preserveScopeTrial`
- actionable current-local rejections during retry/trial reconstruction replace or re-home the candidate failure without silently clearing the original scope

`preserveScopeTrial` is intentionally different from backoff extension:

- it re-arms `next_trial_at` at the current interval
- it does not increment `trial_count`
- it keeps scope authority in `scope_blocks`, not in synthetic duplicate failure rows

`releaseScope` is the single “scope resolved” transition:

- delete the scope row
- delete the boundary row (`failure_role='boundary'`)
- convert held rows back to retryable item rows immediately

### Trial timing

Trial timing is scope-aware:

- `throttle:target:*` and `service` honor server `Retry-After` exactly when present
- quota and service scopes without server timing use 5s initial, 2x backoff, 5m max
- `disk:local` uses 5m initial, 2x backoff, 1h max

The persisted `timing_source` on `scope_blocks` records whether a scope was
timed by local backoff or explicit server `Retry-After`. The persisted
`preserve_until` timestamp records bounded restart-safe preserve state for
scoped-failure-backed scopes whose held rows may temporarily disappear or
change shape during preserve handling. It does not make locally timed
`throttle:target:*` or `service` scopes survive restart, and it does not apply
to non-trial `auth:account`.

### Restart semantics

Startup repair applies persisted-scope policy before any admission begins:

- `throttle:target:*` and `service` survive restart only when `timing_source='server_retry_after'`
- expired server-timed scopes are trialed immediately, not auto-released
- non-server-timed throttle/service scopes are cleared on startup
- legacy persisted `throttle:account` scopes are released on startup instead of being migrated, because they do not encode the throttled remote boundary
- scoped-failure-backed scopes may survive restart while `preserve_until` is still in the future even if no same-scope held rows remain
- `auth:account` is revalidated from one startup proof call instead of trial timing
- `disk:local` is revalidated against current free space instead of trusting stale persisted timing

## Deleted Mechanisms

The sync engine does not carry a circuit breaker, retry queue inside the graph,
or transport-level sync retry layer. The durable retry model is:

- `sync_failures`
- `scope_blocks`
- engine-owned retry/trial loops
