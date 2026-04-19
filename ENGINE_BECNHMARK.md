# Ideal Sync Engine Design

This note defines the ideal future-state sync engine for this repo.

It keeps the overall benchmark engine shape:

1. observe current local and remote truth
2. commit comprehensive snapshots
3. compare local, remote, and baseline truth in SQLite
4. reconcile structural outcomes
5. build the actionable set in Go
6. admit, order, and execute actions in Go
7. persist success, retry, blocker, and issue outcomes
8. replan whenever new truth arrives

But it makes the durable ownership model explicit and strict.

The future-state design has three durable concepts for issue, retry, and
blocking state:

- `observation_issues`
- `retry_work`
- `block_scopes`

Those names are deliberate. They describe what each durable surface really is,
not how the current code happened to evolve.

There is also one important derived user-facing surface:

- `visible_issues`

`visible_issues` is a projection, not a durable authority table.

The current repo still uses the names `retry_state`, `scope_blocks`, and
`sync_failures`. This document treats those as legacy/current implementation
names, not the ideal conceptual vocabulary.

Whenever this note mentions current code shape, it does so only in explicitly
labeled legacy/current-state sections. The intended architecture is the
future-state model.

## Design Goal

Build the engine around these principles:

- the filesystem is authoritative for current local truth
- Microsoft Graph is authoritative for current remote truth
- local current truth must be rebuilt at startup by full scan
- remote current truth may resume from durable mirror plus delta when safe
- SQLite owns current committed truth, prior synced agreement, and structural
  comparison
- Go owns the live actionable set, scheduling, execution, and side effects
- exact delayed work, shared blockers, observation-discovered issues, and
  user-facing issue projection must be modeled
  separately
- short-term transport retry and long-term persisted retry must be distinct
- retries never invent sync intent
- reporting state never owns execution state

## What Other Tools Teach

This design is informed by the public behavior of other sync tools.

## Dropbox wisdom

Useful lessons:

- rate limits are real shared conditions, not independent per-file failures
- `429` plus `Retry-After` should be treated as a shared backoff signal
- quota and permission problems should become visible blocker states, not quiet
  infinite retries

What to copy:

- account-wide throttling behavior
- targeted shared backoff instead of retry storms
- explicit user-facing "sync is blocked" reporting

What to avoid:

- opaque ownership where user-visible state and retry behavior are mixed

## Nextcloud wisdom

Useful lessons:

- some failures should become delayed per-item retries with bounded backoff
- local low-disk state needs special blocker semantics, not generic retries
- skipped work can remain owed without blocking the whole sync

What to copy:

- delayed retry for exact work after local short-term retry is exhausted
- low-disk blocker semantics

What to avoid:

- ad hoc client-specific blacklist behavior without clear ownership boundaries

## Seafile wisdom

Useful lessons:

- read-only or permission-denied remote subtrees are scope blockers
- file sync problems and permission blockers should be shown clearly to the
  user
- shared-folder permission problems are not just many independent item retries

What to copy:

- strong permission blocker modeling
- read-only subtree thinking
- issue reporting separate from retry ownership

What to avoid:

- relying on issue UI alone to encode blocker semantics

## rclone wisdom

Useful lessons:

- request-level retries and higher-level retries are different layers
- transient transport failures should be retried close to the request boundary

What to copy:

- short-term edge retry at the Graph / transfer / executor boundary
- clear separation between local transient retry and longer-lived engine retry

What to avoid:

- no durable retry ledger in an always-on sync engine

## Syncthing wisdom

Useful lessons:

- visible issue reporting should be straightforward and grouped
- the error surface should explain what is wrong without pretending to be
  planner truth

What to copy:

- clear visible issue summaries
- grouped user-facing error state

What to avoid:

- relying on the steady-state loop alone as the only retry model when restart
  recovery matters

## Future-State Vision

This section is the target architecture. It is the design the engine should be
moving toward.

## Core Invariants

### 1. Local startup scan is mandatory

When the app was not running, nothing was watching local disk. Therefore:

- startup must do a full local scan before planning
- persisted local snapshot rows are not fresh startup truth by themselves
- local current truth after downtime comes from the scan, not from SQLite alone

### 2. Remote truth may resume from durable mirror plus delta

The remote side is different:

- Graph provides a change feed
- remote current truth may resume from durable mirror plus cursor
- when delta is not safe or not available, the engine falls back to a full
  remote refresh

### 3. Planning only sees committed comprehensive snapshots

The planner must never read:

- raw watcher events as truth
- half-written snapshot tables
- a remote cursor that advanced without the matching mirror rows
- startup local rows from before the full scan

Planning always runs from:

- committed current local snapshot
- committed current remote snapshot
- committed baseline
- current mode and policy

### 4. Baseline is prior synced agreement, not current truth

`baseline` means the last converged synced agreement.

It is durable memory for:

- delete inference
- move continuity
- conflict continuity

It does not outrank current truth.

If current local and current remote now authoritatively agree, baseline yields.

### 5. Structural diff is not the executable plan

SQLite should own:

- structural comparison
- reconciliation outputs over current local, current remote, and baseline

Go should own:

- actionable-set construction
- blocker admission
- dependency ordering
- worker scheduling

### 6. Observation issues, exact delayed work, shared blockers, and visible issue projection must stay separate

The engine must answer four different questions:

- what current-truth problems were discovered during observation or structural
  validation
- what exact work do we still owe later
- what shared condition blocks a scope and when do we retry or trial it again
- what should we show and explain to the user

Those map to:

- `observation_issues`
- `retry_work`
- `block_scopes`
- `visible_issues` projection

If one durable surface answers two of the first three questions, the model is
muddy. If the user-facing surface becomes a fourth durable authority instead of
a projection, the model is also muddy.

### 7. Short-term retry and long-term retry are different layers

Short-term retry belongs near Graph/transport/executor I/O.

Long-term retry belongs in durable engine state.

Those layers must not be collapsed into one.

### 8. Shared blockers must not create thundering-herd retries

If 10,000 files are blocked by the same condition:

- do not schedule 10,000 independent free-running retries
- do not let each one rediscover the same blocker forever

Instead:

- persist the exact owed work items
- persist one shared blocker scope
- schedule one shared trial/backoff timeline

## Durable Surfaces

## Current local snapshot

Conceptual role:

- current authoritative local snapshot for planning

Future-state meaning:

- the latest comprehensive local snapshot produced by startup scan or later
  refresh logic

Stores:

- path
- item type
- content identity or hash
- size
- mtime
- comparison facts useful for reconciliation

For:

- SQLite comparison against remote and baseline
- uniform current-truth schema
- inspection and debugging

Not for:

- skipping the startup full scan

## Current remote snapshot

Conceptual role:

- current authoritative remote snapshot for planning

Future-state meaning:

- the latest comprehensive durable remote mirror after delta continuation or
  full remote refresh

Stores:

- item ID
- parent ID
- path
- item type
- content identity or hash
- size
- mtime
- ETag or equivalent revision identity

For:

- SQLite comparison against local and baseline
- durable remote mirror and delta continuation

## Baseline

Conceptual role:

- prior synced agreement

Stores:

- path
- hierarchy facts
- synced local identity tuple
- synced remote identity tuple

For:

- delete inference
- move continuity
- conflict continuity

## Observation state

Conceptual role:

- restart-safe observation scheduling and remote resume state

Stores:

- configured drive identity
- remote delta cursor
- last remote reconcile time
- next full remote reconcile due

Does not store:

- dirty scopes
- raw event queues
- partial coverage flags under this comprehensive-snapshot model

## `observation_issues`

Conceptual meaning:

- "current truth reveals this path or content is inherently wrong, unsyncable,
  or locally unreadable under current policy"

This is the durable observation-owned issue ledger.

Stores:

- `path`
- issue type
- first seen / last seen
- optional file size or path facts needed for rendering
- optional observation-owned reason text or normalized reason fields

May own:

- observation-discovered actionable problems
- observation-time validation failures
- observation-owned local file problems that should be shown, not retried

Must not own:

- retry scheduling
- shared blocker timing
- desired intent
- exact execution obligations

Key invariant:

- `observation_issues` records what current truth itself makes impossible or
  invalid
- it is not the retry ledger and not the blocker ledger

## `retry_work`

Conceptual meaning:

- "we still owe this exact semantic work later"

This is the durable long-term execution obligation ledger.

Stores:

- exact semantic work key
- `path`
- `old_path` when relevant
- `action_type`
- `attempt_count`
- `next_retry_at`
- `blocked`
- `scope_key`
- last error classification or reason

May own:

- exact delayed work identity
- ready retry timing
- blocked/unblocked state
- linkage to one shared blocker scope

Must not own:

- desired intent by itself
- current planner truth
- user-facing issue reporting
- shared blocker timing

Key invariant:

- `retry_work` rows are subordinate to the latest actionable set
- if the latest actionable set no longer wants the work, the row is deleted

## `block_scopes`

Conceptual meaning:

- "this scope is blocked, and here is its restart-safe backoff, trial, or
  preserve timing"

This is the durable shared blocker timing and lifecycle ledger.

Stores:

- `scope_key`
- scope kind
- `blocked_at`
- `next_trial_at`
- `retry_after_until`
- trial interval or backoff facts
- trial count
- preserve deadline when independently needed

May own:

- shared blocker timing
- scope release/discard lifecycle
- restart-safe trial scheduling

Must not own:

- the blocked item list
- executable action payloads
- planner truth
- visible issue reporting

Key invariant:

- blocked work stays in `retry_work`
- `block_scopes` never becomes a second source of truth for exact owed work

## `visible_issues`

Conceptual meaning:

- "the user-facing explanation of what is wrong right now"

This is a projection over the durable authorities, not a fourth durable truth
table.

Derived from:

- `observation_issues`
- `retry_work`
- `block_scopes`

It should power:

- the `issues` command
- status summaries
- watch-mode visible issue reporting

It must not own:

- retry scheduling
- blocker timing
- exact owed work
- observation truth

Key invariant:

- user-facing issue rendering is derived from authoritative sources instead of
  being a competing durable authority

## Full Taxonomy

The full future-state taxonomy has three source categories:

1. observation issues
2. retry-derived issues
3. blocker-derived issues

The user-facing `visible_issues` surface is the projection over those three.

## Observation issues

These are discovered from current truth during:

- observation
- hashing
- local validation
- structural validation during or immediately after snapshot assembly

They are not retries and not shared blocker scopes.

Exhaustive future-state observation issue families:

- `invalid_filename`
  Meaning: the filename is invalid for OneDrive or the target namespace.
  Specific current subcases already proven in code include:
  - empty filename
  - trailing period
  - trailing space
  - leading space
  - component too long
  - reserved Windows device name
  - reserved OneDrive pattern
  - forbidden OneDrive characters
  - reserved root-library names
- `path_too_long`
  Meaning: the full path exceeds the remote path limit.
- `file_too_large`
  Meaning: the file exceeds the remote upload size limit.
- `case_collision`
  Meaning: the local tree contains names that differ only by case and cannot be
  represented safely on OneDrive.
- `hash_error`
  Meaning: hashing or file inspection failed unexpectedly during observation.
- `local_file_read_denied`
  Meaning: one specific file cannot be read during observation, but this is not
  a subtree/shared blocker situation.
- `policy_disallowed`
  Meaning: product or sync policy says this path/content must never sync, and
  waiting longer will never change that.

Notes:

- the first five map directly onto current repo issue families
- `local_file_read_denied` and `policy_disallowed` are the right future-state
  conceptual names even if current code still folds them into older families or
  does not yet model them separately

## Retry-derived issues

These come from exact work that the engine still owes after short-term retry
was exhausted. They are derived from `retry_work`.

They should stay a small user-facing set even if the internal transient reason
space is richer.

User-facing retry-derived families:

- `retrying_automatically: transient_timeout_or_visibility`
  Covers request timeout, transient not-found, transient precondition conflict,
  and transient locked-resource style failures.
- `retrying_automatically: transient_service_issue`
  Covers item-scoped transient service failures that have not widened into a
  shared blocker.
- `retrying_automatically: transient_rate_limit`
  Covers item-scoped transient rate limiting before or unless it is promoted to
  a shared throttling blocker.

Internal transient subreasons may include:

- request timeout
- transient not found
- transient conflict / precondition mismatch
- resource locked
- item-scoped service outage
- item-scoped rate limiting

Those subreasons matter for retry policy, but the user-facing retry taxonomy
should stay smaller and easier to understand.

## Blocker-derived issues

These come from `block_scopes`.

A blocker scope is not "an issue type discovered only after an action fails."
A blocker scope is a shared-condition fact, and it may be discovered from:

- observation
- execution

That distinction matters.

Observation-born scopes:

- local subtree permission denial
- remote subtree read denial discovered during refresh
- remote subtree write denial when observation/probe can prove read-only
  semantics before dispatch
- other shared subtree blockers that current truth proves before execution

Execution-born scopes:

- account throttling
- target-drive throttling
- remote quota exceeded
- remote write denial first proven by a failed write
- local disk full first proven by a failed local write/download
- broader service outage widened from action failures

User-facing blocker-derived families are:

- `authentication_required`
- `rate_limited`
- `service_outage`
- `quota_exceeded`
- `remote_write_denied`
- `remote_read_denied`
- `local_read_denied`
- `local_write_denied`
- `disk_full`
- `file_too_large_for_space`

The scope taxonomy below defines the concrete scope families that back those
blocker-derived issues.

## Scope Taxonomy

The ideal future-state scope taxonomy should be explicit and directional.

## `account:throttled`

Meaning:

- the remote service is rate limiting at the account level

Use for:

- broad `429` or equivalent rate-limiting conditions that should suppress
  account-wide hammering

Behavior:

- one shared blocker scope
- blocked retry work links to it
- `visible_issues` reports this as a blocker-derived issue
- respect `Retry-After`

Why:

- this copies the right lesson from Dropbox-style rate limits

## `throttle:target:drive:<driveID>`

Meaning:

- one target drive or one remote target is rate limited

Behavior:

- block only work aimed at that drive
- keep unrelated work flowing
- respect `Retry-After`

Why:

- this is narrower than account throttling and avoids unnecessary global stall

## `perm:remote:write:<boundary>`

Meaning:

- the remote subtree can be read but not written

Examples:

- read-only shared folder
- remote subtree write denied

Behavior:

- uploads, remote creates, remote moves, and remote deletes under that boundary
  are blocked
- blocked work lives in `retry_work`
- shared blocker timing and lifecycle live in `block_scopes`
- `visible_issues` reports this as `remote_write_denied`

Why:

- this copies the best lesson from Seafile-style read-only subtree behavior

## `perm:remote:read:<boundary>`

Meaning:

- the engine cannot even read the remote subtree or parent needed for planning
  or mutation under that boundary

Behavior:

- any work requiring remote reads under that boundary is blocked
- surfaced distinctly from remote write denial
- unrelated work outside the boundary continues

Why:

- remote read denial is a stronger blocker than remote write denial and must be
  modeled separately

## `perm:dir:<path>`

Meaning:

- the local directory subtree cannot be read and/or written safely

Behavior:

- subtree-level local permission denial becomes one shared blocker scope
- blocked descendants remain exact work items in `retry_work`
- `visible_issues` reports this as local read or local write denial depending
  on the blocked capability

Why:

- this prevents local subtree permission problems from degenerating into
  disconnected file-level retry spam

## `quota:own`

Meaning:

- the remote drive is out of quota for writes

Behavior:

- block uploads and other remote-creating writes
- keep unrelated safe work flowing when possible

Why:

- quota is a shared blocker, not a plain per-file transient retry

## `disk:local`

Meaning:

- local disk space is insufficient for local writes/downloads

Behavior:

- block downloads and local writes requiring disk
- keep unrelated remote-only work flowing when safe

Why:

- this copies the right Nextcloud lesson from low-disk behavior

## `service`

Meaning:

- broader Graph or service outage outside a narrower throttle scope

Behavior:

- only use when the failure is genuinely broad
- do not upgrade every transient request hiccup into a durable global blocker

## Engine Pipeline And Where These Concepts Fit

## 1. Observation

Observation owns:

- startup full local scan
- remote delta continuation or full remote refresh

Observation writes:

- current local snapshot
- current remote snapshot
- observation state

Observation may detect:

- observation issues, which persist in `observation_issues`
- observation-born shared blocker scopes, which persist in `block_scopes`

Observation-born scopes are important. Shared blocker scopes are not only
execution-time discoveries.

Observation must not:

- build executable actions
- own retry work

## 2. Structural comparison in SQLite

SQLite owns structural comparison and reconciliation over:

- current local snapshot
- current remote snapshot
- baseline

SQLite should determine structural outcomes such as:

- upload candidate
- download candidate
- local delete candidate
- remote delete candidate
- equal-again
- conflict class
- baseline removal

SQLite does not own:

- runtime scheduling
- blocker admission
- dependency ordering

## 3. Actionable-set construction in Go

Go converts structural outcomes into the actionable set.

This stage applies:

- sync mode
- ignore and policy filtering
- conflict expansion
- delete safety rules
- folder delete expansion
- dependency detection
- blocker admission

This is where the durable recovery model touches current desired intent:

- `retry_work` is pruned against the latest actionable set
- `block_scopes` is consulted for held/trialed work
- `observation_issues` is not planner intent
- `visible_issues` is not planner input

## 4. Runtime admission and scheduling

Go runtime owns:

- ready queue
- blocked queue
- dep graph
- in-flight work
- retry timers
- trial timers
- debounce and dirty scheduling

Durable relationships here:

- due unblocked work comes from `retry_work`
- shared blocker timing comes from `block_scopes`
- observation-backed user-help-needed problems come from `observation_issues`
- visible status comes from the `visible_issues` projection

## 5. Execution

Execution owns:

- side effects
- item-level revalidation
- short-term edge retry

Short-term retry belongs here and in Graph/transport.

Use it for:

- connection reset
- brief timeout
- short transient `403`, `404`, `408`, `423`, `429`, `5xx`
- chunk-level retry
- request-level exponential backoff with jitter

Characteristics:

- in-memory only
- local to the request or transfer
- small budget
- not persisted unless exhausted

Execution must not:

- invent new sync intent
- use observation/reporting rows as retry/control truth

## 6. Failure classification and persistence

After execution returns an outcome:

- transient item failure persists exact `retry_work`
- shared blocker failure persists or extends `block_scopes` and blocks exact
  `retry_work`
- actionable execution-discovered issue should become either:
  - an `observation_issues`-style durable actionable issue if it is really a
    current-truth/content problem, or
  - blocker/retry state if it is really execution-owned retry/blocker state

The user-facing explanation is then derived by `visible_issues` rather than
stored as an independent durable authority table.

This is the critical separation-of-concerns boundary.

## Lifecycle Rules

## Normal transient failure

1. Short-term edge retry budget is exhausted.
2. Classification says item-transient.
3. Persist:
   - exact `retry_work`
4. Later retry when due.
5. On success or stale revalidation:
   - delete exact `retry_work`

## Shared blocker failure

1. Classification detects a shared blocker.
2. Persist:
   - one `block_scopes` row
   - exact blocked `retry_work`
3. Schedule one shared trial timeline.
4. On release:
   - delete or resolve the scope
   - unblock exact retry work
5. On discard:
   - delete the scope
   - delete blocked retry work for that scope

## Actionable user-help-needed issue

1. Classification or observation proves this is not "retry later" but "show the
   user a durable issue".
2. Persist:
   - actionable `observation_issues`
3. Remove:
   - any exact matching `retry_work`

Examples:

- invalid filename
- file too large
- permanent policy rejection
- local file permission denial that should be shown, not retried

## Revalidation And Execute

Every action must carry enough identity to detect staleness.

Examples:

- overwrite upload: expected local content identity plus expected remote ETag
- local delete: expected local hash before delete
- remote delete: expected remote identity
- download overwrite: expected remote identity plus local overwrite
  preconditions when needed

Executor result kinds should include:

- success
- stale-precondition / changed
- retryable transient
- scope-blocking transient
- actionable issue

Critical rule:

- executor-time revalidation may refuse or reclassify work
- executor-time revalidation may not invent a new action type

## The `issues` Command

The `issues` command should be explicitly designed around `visible_issues`.

It should answer:

- what is wrong
- where is it wrong
- is the engine retrying automatically
- is the work blocked behind a shared condition
- does the user need to fix something

It should not expose planner internals as if they were the user model.

The right mental model is:

- `observation_issues` answers what current truth itself says is wrong or
  unsyncable
- `retry_work` answers what the engine still owes
- `block_scopes` answers what shared condition is blocking progress
- `visible_issues` answers what the user should see

The visible issue projection should group and summarize those reporting facts in
the spirit of Syncthing and Seafile: clear, grouped, user-facing.

## Why This Separation Is Correct

If a reporting surface also owns retries:

- clearing a visible issue could accidentally erase owed work
- retry timers would depend on reporting rows

If `block_scopes` also owns blocked item payloads:

- shared blocker timing and exact owed work get mixed together
- one scope becomes a second source of truth for the blocked work set

If `retry_work` also owns current desired intent:

- stale failed work could outvote the latest actionable set
- retries would start inventing sync intent

This design avoids all three mistakes.

## Mapping To Current Repo Names

Ideal future-state concept to current implementation name:

- `observation_issues` -> partly current observation-time actionable rows in
  `sync_failures`; no clean first-class table yet
- `retry_work` -> current `retry_state`
- `block_scopes` -> current `scope_blocks`
- `visible_issues` -> current visible issue projection over `sync_failures`
  and scope/retry summaries

This mapping is only for orientation. The ideal design should be discussed and
documented using the future-state names above.

## Legacy / Current-State Notes

This section is descriptive, not normative. It exists only to help map the
future-state design onto the current repo.

## Current durable names

The current repo names are:

- `retry_state`
- `scope_blocks`
- `sync_failures`

Those names are acceptable as implementation details today, but they are not
the clearest conceptual names for the future architecture.

## Current scope vocabulary is narrower than the target vocabulary

Today the repo already has useful scope families and prior lineage for broader
scope handling, including:

- target-drive throttling
- account throttling in recent history
- local directory permission scopes
- remote permission scopes
- quota
- local disk
- service-wide blockers

But the future-state vocabulary should be cleaner and more directional than the
current mixed legacy surface.

In particular, the target model should clearly distinguish:

- `account:throttled`
- `throttle:target:drive:<driveID>`
- `perm:remote:write:<boundary>`
- `perm:remote:read:<boundary>`
- `perm:dir:<path>`
- `quota:own`
- `disk:local`
- `service`

If current code still collapses some of those distinctions, that is legacy
shape, not the desired end state.

## Current implementation may still encode issue, retry, and blocker behavior with older boundaries

The current codebase has already moved substantially toward the benchmark
shape, but the future-state rule remains:

- observation-owned durable actionable issues belong to `observation_issues`
- exact owed work belongs to `retry_work`
- shared blocker timing belongs to `block_scopes`
- user-facing reporting belongs to `visible_issues`

Any current helper, schema name, or code path that mixes those responsibilities
should be read as legacy implementation detail to be retired, not as a reason
to weaken the future-state ownership model.

## Current names vs future-state names

The future-state document should be read with this translation in mind:

- current observation-time actionable rows inside `sync_failures` are partial
  legacy shape for future-state `observation_issues`
- current `retry_state` is legacy naming for future-state `retry_work`
- current `scope_blocks` is legacy naming for future-state `block_scopes`
- current visible issue/status projection over `sync_failures` and scope/retry
  summaries is partial legacy shape for future-state `visible_issues`

The future-state names are preferred because they communicate ownership more
clearly:

- `observation_issues` says "current truth itself says this is wrong or
  unsyncable"
- `retry_work` says "exact owed work"
- `block_scopes` says "shared blocker timing/lifecycle"
- `visible_issues` says "user-facing explanation, derived not owned"

That is the language the design should standardize on, even if the code and
schema still lag behind for a while.

## Completion Criteria

The engine matches this ideal design when all of these are true:

1. planning reads only committed comprehensive snapshots plus baseline
2. SQLite structural comparison is not confused with executable actions
3. short-term Graph/transport retry stays separate from long-term persisted
   retry
4. observation-discovered durable actionable problems live only in
   `observation_issues`
5. exact delayed work lives only in `retry_work`
6. shared blocker timing lives only in `block_scopes`
7. user-visible issue state is derived from `visible_issues`, not stored as a
   competing durable authority
8. shared blockers such as `account:throttled` do not degrade into per-file
   retry storms
9. remote read and remote write permission scopes are modeled explicitly
10. blocker scopes can be discovered in observation as well as execution
11. the `issues` command is explicitly user-facing and backed by issue
   projection
12. clearing visible issues never accidentally mutates retry ownership
13. executor-time precondition changes never invent new sync intent
