# Sync Engine Benchmark

This document defines the target-state sync architecture for `onedrive-go`.
It is the benchmark the sync refactor series should converge on.

The architecture is intentionally strict about ownership:

- SQLite owns durable truth and durable sync-control state.
- Go owns the live actionable set, dependency ordering, admission, scheduling,
  and side effects.
- `status` is the only sync-health command. It shows all status; there is no
  second sync-health command or `resolve` command family.

## Design Goal

The target engine should be easy to reason about after restart, during watch
mode, and during failure recovery. The model is:

1. observe current local and remote truth
2. commit comprehensive snapshots plus observation resume metadata
3. compare current truth and baseline in SQLite
4. build the current actionable set in Go
5. reconcile durable retry/blocker authorities to that actionable set
6. add runnable work to the dependency graph
7. apply final scope admission on the ready frontier
8. execute, persist outcomes, and replan when new truth arrives

The critical split is:

- SQLite answers structural questions.
- Go answers live execution questions.

The durable model must not mix user-visible conditions, exact owed work, and
shared blocker timing into one table.

## Core Invariants

### Local startup scan is mandatory

When the app was not running, nothing was watching local disk. Startup must
therefore perform a full local scan before planning. Persisted `local_state`
rows are not sufficient startup truth on their own.

### Remote truth may resume from cursor plus durable mirror

Remote observation may resume from `remote_state` plus
`observation_state.cursor` when the cursor is still valid. When that is not
safe, the engine falls back to a full remote refresh.

### Planning only reads committed snapshots

Planning runs only from committed `local_state`, committed `remote_state`,
committed `baseline`, and current policy/mode. It never treats raw watcher
events or half-written snapshot rows as truth.

### Baseline is prior synced agreement, not current truth

`baseline` stores the last converged synced agreement. It exists for delete
inference, move continuity, and conflict continuity. It does not outrank
current local or remote truth.

### Structural comparison is not the executable plan

SQLite owns comparison and reconciliation. Go owns actionable-set
construction, dependency ordering, admission, and worker scheduling.

### Durable authorities stay separate

The engine must answer four different questions without one table trying to
answer two of them:

- what current-truth problems were discovered
- what exact work is still owed later
- what shared condition blocks a scope and when to retry/trial it
- what should `status` show

Those questions map to:

- `observation_issues`
- `retry_work`
- `block_scopes`
- read-time `status` projection

### Short-term retry and long-term retry are different layers

Transport retry belongs near Graph and transfer I/O. Durable retry belongs in
the engine. Those two layers must stay separate.

### Shared blockers must not create retry storms

If thousands of items are blocked by the same condition, the engine must not
create thousands of independent free-running retry loops. It should persist the
exact owed work in `retry_work`, persist the shared blocker in `block_scopes`,
and schedule one shared trial/backoff timeline.

## Durable Surfaces

| Table | Owner | Purpose |
| --- | --- | --- |
| `baseline` | sync store | last converged synced agreement |
| `local_state` | sync store | latest admissible local snapshot |
| `remote_state` | sync store | latest remote mirror truth |
| `observation_state` | sync store | observation resume and full-refresh cadence metadata |
| `observation_issues` | sync store | durable current-truth/content problems |
| `retry_work` | sync store | exact delayed work the engine still owes |
| `block_scopes` | sync store | shared blocker timing and lifecycle |
| `run_status` | sync store | one-shot summary metadata for `status` |

The design intentionally has no `sync_failures` table.

## `observation_issues`

`observation_issues` stores durable problems where current truth itself is
invalid, unsyncable, or requires user action.

Examples:

- invalid filename
- path too long
- file too large
- case collision
- local file read denied
- hash/inspection failure
- policy-disallowed content

These rows are not retry scheduling. They are the durable ledger for
"something about current truth is wrong and the user needs to know."

Execution-discovered problems also belong here when they are really current
truth/content problems rather than transient execution failures. For example, a
write that proves a file is permanently unsupported should normalize into
`observation_issues`, not stay in a retry lane.

## `retry_work`

`retry_work` stores only exact delayed work:

- semantic work identity: `work_key`, `path`, `old_path`, `action_type`
- block linkage: `scope_key`, `blocked`
- retry timing: `attempt_count`, `next_retry_at`
- operator/debug facts: `last_error`
- timestamps: `first_seen_at`, `last_seen_at`

`retry_work` answers exactly one question: what concrete work does the engine
still owe later?

It is not a reporting table and not a durable executable plan. Replans prune it
against the newly built actionable set.

## `block_scopes`

`block_scopes` stores only shared blocking conditions and their timing:

- `scope_key`
- blocker kind / visible condition type
- `blocked_at`
- timing source
- trial interval / next trial time / trial count

`block_scopes` is the durable owner of shared blocker lifecycle. It is not a
secondary owner of exact work.

Blocked work remains in `retry_work` with `blocked=true` and `scope_key`
pointing at the owning scope row.

### Scope taxonomy

The target scope families are:

- `service`
- `throttle:target:drive:<driveID>`
- `quota:own`
- `disk:local`
- `perm:dir:<path>`
- `perm:remote:read:<boundary>`
- `perm:remote:write:<boundary>`
- `account:throttled`

`account:throttled` is reserved for explicit classifier evidence. Ordinary
target throttles do not imply it automatically.

### Observation-born blockers

The engine should create `block_scopes` directly when current truth already
proves a shared blocker, for example:

- local subtree permission denial
- remote subtree read denial during refresh or probe
- remote subtree write denial when observation can prove read-only before
  dispatch

Not every scope is observation-born. Some will still be activated by execution.
The rule is simply that blocker lifecycle belongs in `block_scopes` whichever
path discovered it.

## `observation_state`

`observation_state` is not current truth. It is restart-safe observation
metadata:

- configured drive ID
- primary remote cursor
- local and remote refresh mode
- last full refresh time
- next full refresh time

`local_state` and `remote_state` answer "what do we currently believe exists?"
`observation_state` answers "where do we resume observing from, and when do we
force a full refresh again?"

## Status Surface

`status` is the only sync-health command.

`status` must project directly from the durable authorities and other current
runtime/account overlays:

- `observation_issues`
- `retry_work`
- `block_scopes`
- `run_status`
- drift/baseline counts derived from snapshot state
- auth/account/degraded discovery state
- optional live perf overlay

There is no second sync-health command. There is no manual `resolve` command.
User action happens by fixing the world, rerunning ordinary commands, or
letting the engine retry and reobserve.

## Persistence Rules

### Observation-time persistence

Observation writes:

- `local_state`
- `remote_state`
- `observation_state`
- `observation_issues` when current truth is invalid or unsyncable
- `block_scopes` when observation already proves a shared blocker

Observation does not write retry scheduling.

### Execution-time persistence

Execution writes:

- success publication to `baseline` and any needed mirror cleanup
- `retry_work` for exact transient work that remains owed
- `block_scopes` plus blocked `retry_work` for shared blockers
- `observation_issues` when execution proves a durable current-truth/content
  problem rather than a transient failure

The engine must never use a mixed failure table as durable control state.

## Admission And Execution

### Durable reconciliation happens before dispatch

After the current actionable set is rebuilt, the engine reconciles durable
state before dispatch:

- prune stale `retry_work` rows not present in the current actionable set
- ensure blocked rows are linked to active `block_scopes`
- release empty `block_scopes`
- normalize durable current-truth problems into `observation_issues`

This keeps durable state aligned with the current plan without creating a
durable executable-plan table.

### Final scope admission happens after dependency readiness

`DepGraph` remains scope-agnostic. It owns dependency ordering only.

Final scope admission still happens on the ready frontier after dependency
resolution:

- ready actions are checked against active `block_scopes`
- dispatchable actions go to workers
- blocked actions stay represented as blocked `retry_work`
- trial candidates also flow through the same runtime admission boundary

The benchmark does not require moving all blocker checks before the dependency
graph. It requires earlier durable reconciliation plus a final runtime gate on
ready work.

## Retry And Trial Model

- due unblocked rows in `retry_work` feed ordinary retry dispatch
- due `block_scopes` feed trial dispatch
- one shared blocker owns one shared trial timeline
- trial outcomes may release, re-arm, or extend the owning scope; if no blocked
  `retry_work` remains after refresh or cleanup, the scope is discarded

The dependency graph is not a retry system. Workers do not sleep for durable
retry backoff. They return one completion, and the engine persists the outcome.

## Restart Semantics

On startup the engine reloads:

- snapshots (`local_state`, `remote_state`)
- `baseline`
- `observation_state`
- `observation_issues`
- `retry_work`
- `block_scopes`
- catalog-owned account auth state

Then it reobserves current truth and replans. The engine does not require a
persisted in-flight action queue or a mixed failure ledger to recover.

The refactor series should prefer explicit state-store reset over compatibility
shims. Old stores that do not match the target architecture should require
`drive reset-sync-state` instead of migration glue.

## Refactor Guardrails

The target architecture requires these cleanup rules:

- no `sync_failures`
- no `retry_work`
- no `block_scopes`
- no store-owned competing status projection model
- no ghost vocabulary such as failure-role `item` / `held` / `boundary`
- no tests whose purpose is only to prove old names are gone
- no second sync-health command

The end state should make the old architecture invisible in docs, code, tests,
comments, error strings, and public status payloads.
