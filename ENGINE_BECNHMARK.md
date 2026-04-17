# Comprehensive Snapshot Sync Pipeline

This note defines the recommended sync-engine shape for this repo as a
OneDrive client. It is intentionally prescriptive. It captures the design
direction where:

- local truth is always rebuilt from a full local scan at startup
- remote truth is resumed from a durable remote mirror plus delta when safe
- current snapshots are committed comprehensively and atomically before
  planning
- structural diff runs against SQLite
- retries never invent sync intent and never replay stale plan generations
- execution always revalidates item-level preconditions before mutation

The goal is not "store everything in SQLite." The goal is:

- store the durable facts and the restart-safe scheduling state
- keep live runtime coordination in Go
- ensure the planner only sees complete current truth

## Design Goal

Build the engine around these principles:

- The filesystem is authoritative for current local truth.
- Microsoft Graph is authoritative for current remote truth.
- Local downtime means local truth must be rebuilt by scanning. There is no
  shortcut around this.
- Remote truth may continue from a durable mirror plus delta, because the API
  provides a server-side change feed.
- SQLite is the comparison surface for the current comprehensive snapshots plus
  the synced baseline.
- SQLite persistence is not a substitute for observation. It is the durable
  surface the planner and recovery logic use after observation has produced a
  complete snapshot.
- The planner derives sync intent from current snapshots plus baseline plus
  current policy.
- Retries are subordinate to the latest desired plan. They do not become an
  independent planning model.
- The executor performs side effects, revalidates item-level preconditions, and
  never invents new sync intent.

## Core Invariants

These invariants define the recommended model.

### 1. Local startup scan is mandatory

When the process was not running, nothing was watching local disk. Therefore:

- previously persisted local snapshot rows are not trusted as current local
  truth at startup
- every startup performs a full local scan before planning
- that full local scan produces the current local snapshot for this run

Persisted `local_state` is therefore:

- useful as a SQL comparison surface
- useful for inspection/debugging
- useful for a consistent data model

but it is **not** the reason the engine knows current local truth after
downtime. The scan is.

### 2. Remote snapshot is resumed, not rediscovered from scratch by default

The remote side is different:

- Graph delta is the server-side watcher
- the process can resume from a durable remote mirror plus a saved cursor
- when delta is unavailable, expired, or explicitly bypassed, the engine falls
  back to a full remote refresh

Therefore the durable remote mirror is real restart-safe state in a way the
prior local snapshot is not.

### 3. Snapshots written to SQLite must be comprehensive

The store must not contain "maybe complete" current snapshots.

Recommended rule:

- `local_state` is replaced wholesale from the latest full local scan in one
  transaction
- `remote_state` is brought to the latest full known remote truth in one
  transaction, either by applying one complete delta continuation or by
  replacing from full enumeration
- the remote cursor is advanced atomically with the corresponding `remote_state`
  update

If these invariants hold, the database does not need extra per-scope coverage
flags or partial-snapshot markers.

### 4. Planning only sees committed comprehensive snapshots

The planner must never read:

- raw watcher events as truth
- half-written current snapshots
- a remote cursor that advanced without the corresponding mirror rows
- a local snapshot from before startup re-scan and call it current

Planning inputs are:

- current committed `local_state`
- current committed `remote_state`
- current `baseline`
- current policy/admission inputs

### 5. Baseline is prior synced agreement, not current truth

`baseline` means:

- what the engine last believes converged successfully
- the durable memory needed for delete inference, move continuity, and conflict
  checks

It does **not** outrank current truth.

If current local and current remote both authoritatively agree, and baseline
disagrees, then baseline is stale and must be updated or removed.

### 6. Retries are execution obligations, not planner inputs

The planner does not consume retry state to decide what the desired world
should be.

Planner input remains:

- local snapshot
- remote snapshot
- baseline
- policy

Retry state answers a different question:

- "which already-desired action failed and may run again later?"

### 7. Every delayed operation must be revalidated at the edge

Even if an action still exists in the latest desired plan, the world may have
changed again before mutation. Therefore:

- delayed retries are never replayed blindly
- the executor checks local and/or remote identity immediately before the side
  effect
- if preconditions no longer hold, the action is not applied

## State Model

## Current local snapshot (`local_state`)

What it means:

- the latest comprehensive local snapshot produced by this run's full local
  scan or later watch-refresh logic

What it stores:

- path
- item type
- current content identity or hash
- size
- mtime
- optional additional local comparison facts when useful

What it is for:

- SQL comparison against `remote_state` and `baseline`
- durable inspection/debugging
- a uniform current-truth schema

What it is **not** for:

- avoiding the mandatory startup full local scan
- proving anything about local truth across downtime before that scan runs

## Current remote snapshot (`remote_state`)

What it means:

- the latest comprehensive remote mirror after applying delta continuation or
  full remote refresh

What it stores:

- item identity
- parent identity
- materialized path
- item type
- current content identity or hash
- size
- mtime
- ETag or equivalent remote revision identity
- optional previous path when move continuity helps later planning

What it is for:

- current remote comparison surface
- crash-safe delta continuation
- delete inference through authoritative absence relative to baseline

## Baseline (`baseline`)

What it means:

- the last converged synced agreement

What it stores:

- item identity
- parent identity
- path
- item type
- synced local identity tuple
- synced remote identity tuple
- hierarchy information needed for delete expansion

Important rule:

- baseline remains until authoritative current truth proves it should change or
  disappear

## Observation state (`observation_state`)

What it should store durably:

- configured drive identity
- remote delta cursor
- next full remote reconcile due
- last full remote reconcile time
- remote refresh mode when cadence changes matter

What may be stored but is not essential to correctness:

- local refresh timestamps or degraded/local-watch cadence for diagnostics and
  scheduling convenience

What it should **not** store:

- dirty scopes
- raw events
- partial coverage markers under this comprehensive-snapshot model

## Executable actions

What they mean:

- the concrete actions the runtime currently wants to pursue after structural
  diff, reconciliation, filtering, blocking, dependency detection, and
  admission

Where they should live:

- in Go runtime memory, not as durable SQLite authority

Why:

- the structural diff is not the executable plan
- real executable work depends on runtime-owned blocking, ordering, and
  admission decisions
- this action set changes whenever the runtime replans against new truth

Optional diagnostic surface:

- if a `planned_actions` table exists, treat it as a disposable status
  projection only
- retries, scheduling, and execution must not rely on it as a durable authority

## Retry ledger (`retry_state`)

What it means:

- delayed execution obligations for semantic work that previously failed and may
  still be wanted later

What it should store:

- stable semantic work key
- path
- operation kind
- old path when relevant
- attempt count
- next eligible retry time
- blocked/unblocked flag
- scope linkage when blocked
- last error for diagnostics
- optional last known local/remote precondition identities when they help
  diagnostics or revalidation seed data

Important rule:

- retry rows do not create desired work
- after each replan, retry rows that do not match any current runtime action are
  deleted

## Scope timing (`scope_blocks`)

What it means:

- durable blocking conditions whose timing or restart semantics must outlive the
  current process

What it should store:

- scope identity
- visible issue type
- timing source
- blocked time
- next trial time
- trial interval
- trial count

What it should **not** store:

- the blocked work itself

Blocked work lives in `retry_state`.

## Policy/admission state

Current policy should be applied symmetrically to both local and remote current
snapshots.

Important consequence:

- if a path is absent from both current snapshots under current policy, that is
  a current-truth fact
- baseline must not force that path back into existence

This matters for ignore-rule changes:

- if a folder becomes ignored and is filtered out of both local and remote
  snapshots under the current policy, the correct outcome is baseline removal,
  not propagating deletes to either side

## Recommended Pipeline

### 1. Startup bootstrap

At startup:

- load baseline
- load remote observation state
- do a full local scan
- do remote delta continuation when safe, otherwise full remote refresh
- commit current snapshots atomically
- only then compute the next plan

This means the first plan of a run always uses comprehensive current truth.

### 2. Build the current local snapshot

Recommended shape:

- scan the whole configured local tree
- apply the current ignore/admission rules during observation
- build the full current local snapshot in memory or a temp structure
- replace `local_state` wholesale in one transaction

Important rule:

- the planner should never see a mix of old and new local rows

### 3. Build the current remote snapshot

Recommended shape:

- start from persisted `remote_state` plus persisted cursor
- fetch one complete delta continuation when safe
- apply all remote upserts/deletes to the mirror in one transaction
- advance the cursor in that same transaction

Fallback shape:

- clear or replace the mirror from a full remote enumeration in one transaction
- commit the new cursor with those rows

Important rule:

- either the old mirror plus old cursor survives, or the new mirror plus new
  cursor survives
- there is no committed half-state

### 4. No extra coverage/freshness flags under this model

Under the comprehensive-snapshot model, extra "coverage" metadata is not
required for correctness.

Reason:

- `local_state` is only trusted after the startup/full refresh that rebuilt it
- `remote_state` is only trusted after atomic delta application or full refresh
- planning only runs after those refresh steps

Therefore the trusted/untrusted decision is not stored per scope. It is a
runtime invariant:

- do not plan until current snapshots have been comprehensively refreshed

This is different from architectures that support partial per-scope snapshot
refresh. If the engine ever moves to partial refresh, this assumption breaks
and extra coverage metadata would become necessary.

### 5. Structural diff in SQLite

With `local_state`, `remote_state`, and `baseline` all present, structural diff
should be SQL-first.

Recommended SQL responsibilities:

- union the current path space
- compare current local vs baseline
- compare current remote vs baseline
- compare current local vs current remote
- infer remote delete through baseline plus remote absence
- infer local delete through baseline plus local absence
- recognize equal-again cases
- recognize move candidates when supported by durable identities

Recommended output:

- a comparison view or query result, not a new durable authority table

Important rule:

- pure structural comparison is a data problem and fits SQL well

### 6. Reconcile to desired outcomes

After SQL diff, determine the desired action per path:

- upload
- download
- local delete
- remote delete
- move
- conflict handling
- baseline remove
- no-op

This mapping must honor:

- sync mode
- ignore/admission policy
- delete safety rules
- conflict rules

Important rule:

- baseline disagreement alone does not force work if current local and current
  remote already agree
- in that case, the system should typically converge baseline to current truth

### 7. Folder delete expansion

When a folder delete is inferred:

- expand child work from baseline hierarchy
- emit child delete/cleanup work before the parent

Important rule:

- Graph may omit child delete entries
- baseline must carry enough hierarchy to expand the delete safely

### 8. Build the current actionable set in Go

After SQL diff, Go derives the concrete action set for this pass.

This stage applies what the structural diff alone does not know:

- sync-mode rules
- delete safety rules
- conflict handling
- ignore/admission filtering
- scope blocking
- dependency detection and ordering
- worker/runtime admission constraints

Important rule:

- the SQL diff is not the executable plan
- executable actions are runtime-owned values, not durable SQLite authority

### 9. Live runtime scheduling stays in Go

Go should remain the owner of:

- watcher event intake
- debounce buffer
- dirty signals
- outbox
- dependency graph
- in-flight action tracking
- worker pool
- retry timer
- trial timer
- runtime admission/blocking flags
- the current actionable set

Important rule:

- these are live coordination structures, not durable truth

### 10. Execute with explicit preconditions

A runtime action must carry enough identity to detect staleness.

Examples:

- overwrite upload: expected local content identity plus expected remote ETag
- local delete: expected local hash before delete
- remote delete: expected remote item identity or revision when needed
- download overwrite: expected remote identity, plus local preconditions when
  local overwrite would be unsafe

Important rule:

- plan items are more than `verb + path`

## Revalidate And Execute

This is the key execution contract.

### Revalidate means item-level live checks right before mutation

Before performing an effectful operation, the executor checks the specific item:

- the path is still admitted by current policy
- the local item still matches the expected local identity, when local safety
  matters
- the remote item still matches the expected remote identity, when remote
  overwrite/delete safety matters
- the action still belongs to the current runtime action set and has not been
  superseded by a newer replan

### Expected identity

Expected identity is the minimum fingerprint needed to know the action is still
safe.

Typical local identity:

- file hash or content identity
- sometimes size and mtime when hash is unavailable

Typical remote identity:

- item ID
- ETag or equivalent revision identity
- sometimes parent ID or path when move semantics matter

### Execution outcomes

After revalidation, the executor does one of four things:

1. perform the mutation and report success
2. return no-op/resolved because the work is already satisfied
3. return stale/conflict because preconditions no longer hold
4. return retryable/actionable/fatal failure according to classification

Important rule:

- the executor never silently mutates through stale preconditions

## Retry Model

The recommended retry model has exactly two layers.

### 1. Request-level retries inside one effect boundary

Examples:

- retry a transient HTTP read inside download setup
- retry hash mismatch download within one download attempt
- retry one exact effectful request path where the side-effect semantics are
  still owned by that boundary

These do **not** involve the planner.

### 2. Delayed operation-level retries

These are failures such as:

- upload failed, try later
- download failed, try later
- remote delete failed, try later

These are persisted in `retry_state`.

Important rule:

- delayed retries do not feed planner inputs
- planner input remains snapshots plus baseline plus policy

### What happens when an action fails

1. The current runtime action set remains the source of desired intent.
2. The failure is classified.
3. If retryable, create or update a `retry_state` row keyed to that semantic
   work.
4. If scope-blocking, mark the row blocked and, when needed, persist a
   `scope_blocks` row with trial timing.

### What happens when there is no new plan generation yet

If no new observation/replan has happened since the failure:

- the retry scheduler may retry the same planned action directly
- the executor must still revalidate item-level preconditions before mutation

No planner recomputation is required just because a timer fired.

### What happens when a new action set is created

When the engine refreshes truth and derives a new current action set:

- retry rows that do not correspond to any action in the new current action set
  are
  deleted
- blocked scope rows with no blocked retry rows are deleted unless they have an
  independently valid restart-safe timing reason to persist
- retry rows that still correspond to current desired work are updated or
  preserved as current rows

This is the key rule:

- retries survive only while the current runtime action set still wants the same
  semantic work

### "Retry goes back through planning" - the precise meaning

The recommended model is **not**:

- take an old failed action
- feed it into the planner as a new pseudo-observation input

The recommended model **is**:

- the engine periodically refreshes truth, computes structural diff, and derives
  a new current runtime action set
- retry rows are reconciled against that runtime action set
- if the same semantic work still exists in the current action set, it may be
  retried
- if not, the retry row is deleted as stale/superseded

So retries are subordinate to planning, but they are not planner inputs.

## Scope Blocks

Persist only the scope state that truly needs restart-safe timing or blocking
semantics.

Good candidates:

- explicit server `Retry-After` windows
- quota blocks
- disk-space blocks, if restart should preserve their timer semantics
- long-lived permission or service scopes when restart-safe preserve/retrial is
  part of the product behavior

Bad candidates:

- dirty signals
- pending buffer wakeups
- "we should probably look here soon" hints

Important rule:

- `scope_blocks` stores timing/blocking semantics
- `retry_state` stores the blocked work

## Commit Model

### Baseline advances per successful action

Baseline should advance transactionally for each successful action, not only at
end-of-pass.

Reason:

- partial progress is real progress
- crash recovery should not lose already-converged items
- successful actions should not wait for unrelated actions to finish before
  becoming durable truth

Recommended rule:

- after an action succeeds, commit its durable outcome in one transaction
- that transaction updates the affected baseline row(s)
- that transaction also clears or supersedes the related retry/failure state

### Remote mirror updates after execution

If an action changes remote truth and the executor now knows the post-mutation
remote identity, the durable remote mirror should be kept aligned as part of the
success path or explicitly invalidated for prompt refresh.

Important rule:

- do not let `remote_state` silently drift far behind successful executor
  mutations

## Delete And Ignore Rules

### Remote delete inference

Remote delete is inferred when:

- baseline says the item previously existed
- authoritative current remote snapshot does not contain it

No separate tombstone table is required by default.

### Ignore-rule changes

When current policy excludes a subtree symmetrically:

- local observation omits it
- remote observation omits it
- current snapshots therefore do not contain it

The correct outcome is:

- remove stale baseline rows for that subtree
- do not synthesize local or remote deletes merely because baseline still has
  older synced memory

This is a current-truth convergence case, not a delete-propagation case.

## What SQLite Must And Must Not Remember

### SQLite must remember

- `baseline`
- `remote_state`
- `local_state` for the current comprehensive snapshot
- remote cursor and remote full-reconcile cadence
- retry ledger for delayed execution obligations
- restart-safe scope timing/blocking semantics

### SQLite should not remember

- dirty scope flags
- raw watcher event inboxes
- generic "freshness" or "coverage" flags under this comprehensive-snapshot
  model
- the executable plan or dependency-ordered runtime action set, except as an
  optional disposable status projection
- dependency-graph runtime state
- ready-queue state
- in-flight worker state beyond durable retry/failure facts

## Recommended Practical Rules

1. Local startup truth always comes from a full local scan.
2. Remote startup truth comes from delta continuation when safe, otherwise full
   remote refresh.
3. `local_state` and `remote_state` must be committed comprehensively and
   atomically before planning.
4. Under that invariant, no extra coverage/trust flags are needed in SQLite.
5. Use SQLite as the pure structural diff surface over `local_state`,
   `remote_state`, and `baseline`.
6. Treat baseline as prior synced agreement, not current truth.
7. If current local and current remote agree, baseline loses.
8. If current policy excludes a path from both snapshots, baseline should be
   removed rather than forcing delete propagation.
9. The structural diff is not the executable plan.
10. Build the current actionable set in Go after reconciliation, blocking, and
    dependency detection.
11. Persist retry rows only as delayed execution obligations and prune them
    against the current action set after each replan.
12. Delete scope rows that no longer block any durable retry work unless they
    have an independent restart-safe timing reason.
13. Keep dirty signals, buffer state, dependency tracking, the current
    actionable set, and worker coordination in Go.
14. Revalidate item-level preconditions immediately before mutation.
15. Advance baseline transactionally per successful action.
