# OneDrive Sync Engine Pipeline

This note defines the recommended sync-engine shape for this repo as a OneDrive client. It is intentionally prescriptive. It focuses on the best-practice pipeline, state ownership, and deletion/retry rules relevant to Microsoft Graph delta behavior and a SQLite-backed engine.

## Design Goal

Build the engine around these principles:

- SQLite is the primary comparison surface.
- The filesystem is authoritative for current local truth.
- Microsoft Graph is authoritative for current remote truth.
- SQLite stores durable observations of local and remote truth plus the synced baseline.
- The planner derives intent from persisted observations plus baseline.
- The executor performs side effects, validates preconditions, and never invents new sync intent.

## Core State Model

The engine should persist and plan from these state buckets:

### Local observed state

What it means:

- the engine's last observed view of the local filesystem

Typical fields:

- path
- item type
- existence
- size
- modtime
- content identity or hash
- inode or local identity when useful
- policy/admission state relevant to the path

Authority:

- derived from filesystem observation
- not authoritative beyond "last observed"

### Remote observed state

What it means:

- the engine's last observed view of OneDrive state

Typical fields:

- path
- item id
- parent id
- item type
- deleted/live state
- ETag/cTag/revision identity
- size
- remote hashes when valid
- drive identity

Authority:

- derived from Graph observation
- not authoritative beyond "last observed"

### Baseline state

What it means:

- what the engine believes was synchronized previously

Typical fields:

- path
- item type
- synced local identity
- synced remote identity
- drive identity
- hierarchy information needed for child expansion

Authority:

- authoritative for prior synced agreement

Important rule:

- baseline may also serve as deletion memory
- a previously synced item can remain in baseline until authoritative current observation proves it is gone and the delete has been handled

### Policy/admission state

What it means:

- whether a path is sync-admitted at all

Typical fields:

- ignored/excluded
- denied by scope
- unsupported
- placeholder/projection state if relevant
- special sync mode flags

### Execution state

What it means:

- transient and durable state for in-flight work

Typical fields:

- operation kind
- operation preconditions
- attempt count
- backoff schedule
- temp/resume info
- superseded/canceled state

## Best-Practice Pipeline

### 1. Observe

Inputs:

- local watcher events
- remote delta notifications or polling results
- periodic scan/full-enumeration triggers
- policy/config changes

Purpose:

- invalidate scopes
- wake up the engine

Important rule:

- events do not define truth
- events only say what needs fresh observation

### 2. Refresh authoritative observation for the invalidated scope

For local:

- rescan the invalidated local scope
- write fresh local observed state to SQLite

For remote:

- process delta when safe
- run authoritative full enumeration when required
- write fresh remote observed state to SQLite

Important rule:

- planning must run from persisted current observation, not directly from raw event payloads

### 3. Mark authoritative scope coverage

The engine must know whether an absence is trustworthy.

For each relevant scope, track whether current observation is:

- fully enumerated
- partial
- stale
- denied/ignored

Important rule:

- absence only implies deletion when the observation is authoritative for that scope

### 4. Compare local + remote + baseline

The planner input is:

- current local observed state
- current remote observed state
- baseline
- policy/admission state

The comparison stage classifies each path as:

- unchanged
- local create/update/delete
- remote create/update/delete
- both changed
- conflict candidate
- ignored/policy-blocked

Important rule:

- planner input is snapshots plus baseline
- not watcher events

### 5. Infer remote deletes from authoritative absence

This is the OneDrive-specific deletion rule.

When:

- baseline says an item existed
- the current authoritative remote observation for that scope does not contain it

then:

- synthesize a remote delete

This is the correct model for missed Graph delete events.

Important rule:

- a missed delta delete does not require a separate tombstone table if baseline retains enough prior identity
- baseline plus authoritative absence is enough

This is effectively what orphan detection does.

### 6. Expand folder deletes from baseline

Graph may report only a parent folder delete and omit child delete entries.

When a folder delete is identified:

- use baseline hierarchy to synthesize child delete intent

Important rule:

- baseline must retain enough hierarchy information to expand deletes safely

### 7. Reconcile to desired outcomes

After classification, decide what should happen:

- upload local version
- download remote version
- delete local
- delete remote
- create conflict outcome
- no-op

Important rule:

- reconciliation is where sync intent is chosen
- executor must not redo this decision later

### 8. Build plan items with explicit preconditions

A plan item must be more than `verb + path`.

Each operation should carry enough identity to detect staleness:

- path
- operation kind
- local observed identity
- remote observed identity
- relevant baseline identity
- policy/admission snapshot or generation

Important rule:

- retries are valid only while the preconditions still match current truth

### 9. Execute effectful operations

The executor owns:

- uploads
- downloads
- local file mutation
- remote mutation
- temp/resume handling
- request retries

Important rule:

- executor performs side effects
- executor does not invent new sync intent

### 10. Revalidate before mutation

Before an effectful operation, especially a retry, revalidate the specific item:

- local identity still matches
- remote identity still matches
- path is still admitted by policy
- operation has not been superseded by newer planning

Important rule:

- revalidate the item, not the whole world
- do not blindly replay delayed work

### 11. Handle delete conflicts explicitly

Local or remote absence is not always "just delete the other side".

If a delete would remove newer content on the other side:

- treat it as an edit/delete conflict
- do not silently propagate the delete over newer content

Important rule:

- baseline is enough for this
- a separate tombstone layer is not required just for conflict detection

### 12. Commit side effects, then advance baseline

After successful execution:

- update local/remote observed state as needed
- advance baseline to the new converged truth
- clear or supersede execution state

Important rule:

- baseline advances after successful commit, not before

## Deletion Best Practice for This Repo

The recommended deletion model is:

- do not require a separate tombstone table by default
- use baseline as the durable memory of what was synced
- infer deletes from authoritative absence relative to baseline

This works for a OneDrive client because:

- Graph delta deletes can be missed
- full remote enumeration can later prove authoritative absence
- baseline can carry the identity needed to synthesize delete intent

This means the important design question is not "do we have tombstones?" but:

- does baseline retain enough information long enough to infer delete safely?

Baseline must retain enough to recover:

- path
- item type
- remote identity as needed
- hierarchy for child expansion
- prior synced content identity where conflict checks need it

## Retry Best Practice for This Repo

Retries should happen in the executor, not in planner logic.

Two retry layers are useful:

### Request-level retry

Examples:

- transient HTTP retry
- transient local I/O retry

These stay inside one effect boundary.

### Operation-level retry

Examples:

- retry upload later
- retry download later

These must be checked against current truth before mutation.

Important rules:

- planner chooses intent
- executor performs attempts
- retries survive only if newer planning still wants the same operation
- otherwise mark the old operation superseded

## Ignore Best Practice for This Repo

Ignore/exclusion policy should be applied in three places:

### Observation

- avoid unnecessary scan/enumeration work where possible

### Comparison

- represent ignored/policy-blocked state explicitly so absence is not misread

### Planning/execution

- never emit or execute mutations for ignored paths

Important rule:

- absence under a denied or ignored scope must not be treated as deletion

## Why SQLite-Centered Planning Makes Sense Here

With local observed state, remote observed state, baseline, and execution state already durable, SQLite should be the main comparison surface.

Benefits:

- simpler comparison logic
- crash recovery without rebuilding intent from scratch
- queryable need/planning state
- durable deletion memory through baseline
- cleaner scope-local replanning

Main caveat:

- keep authority boundaries explicit so persisted observation is never mistaken for current filesystem or remote truth

## Recommended Practical Rules

1. Events invalidate scope; they do not define truth.
2. Refresh local and remote observation into SQLite before planning.
3. Treat absence as deletion only when the observation is authoritative for that scope.
4. Use baseline as the durable memory of what was previously synced.
5. Synthesize remote deletes from baseline plus authoritative remote absence.
6. Expand folder deletes from baseline hierarchy when Graph omits children.
7. Plan from SQLite state, not from raw events.
8. Carry explicit preconditions on plan items.
9. Revalidate specific items before effectful mutation and before retries.
10. Advance baseline only after successful commit.

## Compact Pipeline

1. Receive local/remote invalidation.
2. Refresh authoritative local/remote observation for the affected scope.
3. Persist observation and scope-authoritativeness in SQLite.
4. Compare local observed state, remote observed state, baseline, and policy.
5. Synthesize delete intent from authoritative absence relative to baseline.
6. Reconcile desired outcomes.
7. Build plan items with explicit identities and preconditions.
8. Execute operations with narrow request retries.
9. Revalidate item preconditions before mutation or delayed retry.
10. Commit side effects.
11. Update baseline and clear/supersede execution state.
