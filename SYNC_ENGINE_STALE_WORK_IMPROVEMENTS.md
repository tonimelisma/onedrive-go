# Sync Engine Stale Work Improvements

Status: design note. The watch-dispatch/outbox-retirement instrumentation pieces
are implemented in the merged watch-stale-work increments; the superseded result
foundation and incremental local-truth commit increments are implemented in the
stale-work roadmap, and worker/admission latest-truth validation is implemented
in the worker/admission truth-validation increment. Executor-precondition and
final observability groups remain follow-up work.

Date: 2026-04-29

Implemented so far:

1. Watch-mode worker dispatch is unbuffered, while worker completion reporting
   stays separately buffered.
2. When a pending replan is set, the engine retires not-yet-dispatched old
   outbox work instead of continuing to launch it.
3. If already-running old work releases dependents while a pending replan is
   waiting, the engine retires that newly-ready old frontier instead of
   appending it back into the old outbox.
4. Pending-replan debug events now carry timestamps plus outbox, running-worker,
   and idle-worker counts.
5. The prior PR #667 review comment is fixed: unknown-kind local watch events
   are treated as both file-capable and directory-capable for pre-stat content
   filtering, so included directory roots and ancestors are not dropped before
   observation can stat them.
6. A replan signal that is already ready wins before another old outbox action
   can be handed to a worker.
7. If a replan reaches local observation but fails before replacement runtime
   installation, the dirty intent is rescheduled through `DirtyBuffer` instead
   of being dropped. This applies both to pending replans that already retired
   old outbox work and to direct idle replans with no old work to retire.
8. `ErrActionPreconditionChanged` is classified as `superseded`, not ordinary
   retry. Superseded completions clear exact stale retry rows, retire old
   dependents without success semantics, and schedule watch replan.
9. Watch-mode local observation now emits engine-applied local observation
   batches. File create/modify observations upsert exact `local_state` rows,
   file deletes delete exact rows, directory deletes delete prefixes, and
   safety scans replace the full local snapshot.
10. `observation_state` records local truth confidence. Dropped local
    observation batches, dropped hash requests, watcher errors, and failed
    safety scans mark local truth suspect and schedule dirty replan through the
    watch runtime.
11. Worker-start validation rejects already-submitted stale actions, including
    move source/destination drift, before executor side effects.
12. Engine admission reuses the same freshness predicate and retires stale
    ready actions before dispatching them to workers.

Still follow-up work:

1. Executor-side live precondition checks for every dangerous local/remote side
   effect.
2. Final observability counters for superseded sources, scoped local commits,
   and aggregate worker-idle-by-replan-phase timing.

This note records a design analysis for improving the `onedrive-go` sync engine
when watch-mode truth changes while older actions are still queued or running.
It is intentionally verbose because the important distinction is not "should we
replan more often?" but "which component owns each kind of truth, and which
component is allowed to reject stale work?"

## 1. Baseline And Current State

The current watch runtime is deliberately linear:

1. There is one installed runtime plan at a time.
2. A dirty signal while `outbox` or workers are active becomes a pending replan,
   not a second plan.
3. Local filesystem events are observed at file/subtree scope and committed to
   durable `local_state` through engine-applied scoped observation batches.
4. Remote observer batches can be committed to `remote_state` before the next
   replan.
5. Workers execute concrete `Action` values from the currently installed plan.
   They do not have a newer plan to compare against.

That model is clean, deterministic, and close to the current single-owner watch
runtime design. Before this increment, the daemon-responsiveness weakness was
that if a large outbox contained stale uploads and a replan signal arrived, the
engine kept launching old outbox work until the old outbox and running work both
drained. After this increment, pending replan retires the not-yet-dispatched old
outbox immediately and waits only for work that has already reached a worker.

The key distinction is:

1. A newer dirty hint can exist.
2. Newer remote truth may exist in `remote_state`.
3. Newer local facts observed by the local watcher usually exist in
   `local_state` before the next replan unless local truth has been marked
   suspect because observation completeness is uncertain.
4. A newer `ActionPlan` does not exist until the runtime reaches the idle replan
   boundary.

Before the incremental-local-truth increment, local behavior was the biggest
asymmetry in the design. Remote deltas were applied incrementally to durable
remote truth, while local watch events were reduced to dirty hints and the
important truth update waited for the next full local snapshot replacement. The
current implementation closes that gap for ordinary watch observations: a
single file write re-observes that file and updates `local_state` without
rewalking the whole sync root.

Workers still cannot ask "am I still in the newer plan?" because no newer plan
has been built until the replan boundary. The practical improvement path is to
let workers and the engine reject stale work using exact action preconditions,
latest committed truth checks, and queue control without making workers into a
second planner. Queue control, worker-start latest-truth checks, and admission
latest-truth checks are now implemented; live executor preconditions remain
follow-up work.

## 2. External Client Patterns

The useful patterns from other clients are:

1. Nextcloud Desktop validates the concrete local file during upload. It checks
   whether the file still exists and whether it changed since discovery, and
   aborts or schedules another sync when the precondition no longer holds.
   Source:
   <https://github.com/nextcloud/desktop/blob/master/src/libsync/propagateuploadv1.cpp>

2. Syncthing rechecks queued work against current index state when popping from
   its queue. If an item disappeared, became deleted, became invalid, or changed
   type while it was waiting, the queued work is dropped. Syncthing also checks
   disk state against database/index state before touching local files; if disk
   state differs, it schedules a scan before proceeding. Source:
   <https://github.com/syncthing/syncthing/blob/main/lib/model/folder_sendrecv.go>

3. Maestral avoids creating huge stale batches by waiting for local event quiet
   and cleaning local events into the minimum necessary set. It keeps at most one
   event per path except for type changes, and collapses child events under
   moved or deleted folders. Source:
   <https://github.com/SamSchott/maestral/blob/main/src/maestral/sync.py>

4. Dropbox Nucleus is the larger architectural example: persist observations
   and derive operations, rather than persisting outstanding work as the primary
   truth. Its public writeup says the older Dropbox engine persisted outstanding
   sync activity, while Nucleus persists Local, Remote, and Synced trees and
   derives behavior from those observations. Source:
   <https://dropbox.tech/infrastructure/-testing-our-new-sync-engine>

## 3. Recommended Direction

The guiding rule should be:

> Workers should not need a newer plan to know that their exact action is unsafe.
> They should only need to prove that the preconditions of that exact action
> still hold.

That keeps ownership clean:

1. The planner owns sync intent.
2. The runtime owns action admission, dependency progression, retry/block state,
   and worker queue ownership.
3. The executor owns side-effect safety for the exact operation it is about to
   perform.
4. Observation state remains the durable truth.
5. Queued actions remain disposable runtime work, not truth.

For local observation specifically, the recommended direction is to make
watch-mode local truth incremental in the same broad sense that remote truth is
incremental:

1. An fsnotify event is not trusted as truth by itself.
2. The observer re-observes the affected local scope with normal filesystem
   reads: stat, hash when needed, and directory walk when the affected scope is
   a new or changed directory.
3. The store applies that re-observed scope to `local_state` in SQLite.
4. Any store-owned in-memory local-truth index is updated from the same commit,
   not by a separate writer.
5. A later replan reads the latest committed local and remote truth.
6. Periodic full scans remain as the repair mechanism for missed events,
   overflow, watcher errors, filter changes, startup, and root identity checks.

This does not mean trusting raw inotify/fsnotify events as a semantic journal.
It means using events to choose the smallest safe observation scope, then
committing the result of actual filesystem observation.

There should be two separate validation gates:

1. Before a worker begins meaningful work, it should compare the action's
   planned local and remote assumptions against latest committed
   `local_state`/`remote_state`. This is a cheap stale-action gate.
2. Immediately before the dangerous side effect, the executor should validate
   live ground truth itself: current filesystem state for local effects and
   current Graph state for remote effects. This is the authoritative operation
   precondition.

The first gate answers: "does our own latest observation database already prove
this old action obsolete?" The second gate answers: "is the real world still
safe to mutate right now?"

## 4. Improvement Groups

The original list is easier to reason about if grouped by the question each
mechanism answers.

1. Is the real world still safe to mutate right now?
2. Does latest committed truth already prove this action obsolete before we do
   work?
3. Once we know a replan is due, how do we stop old work from continuing to
   launch?
4. How do local observations become fresh committed truth without a full root
   scan?
5. How should the engine classify and report skipped stale work?

### Group A. Live side-effect preconditions

This group covers the old ideas 1, 2, 3, 7, and 8.

The point is: just before a worker performs a dangerous side effect, the worker
must validate real ground truth itself. This validation does not depend on
`local_state` or `remote_state` being perfectly fresh. It asks the filesystem or
Graph directly whether the exact operation is still safe.

For uploads, the executor should verify that the local source still matches the
state the planner saw:

1. The path still resolves inside the sync root.
2. The path still exists.
3. The path is still a regular file.
4. If filesystem identity was observed, the device/inode still matches.
5. Cheap metadata such as size and mtime still matches when used as a
   prefilter.
6. A required content hash still matches before upload.

For downloads, the executor should verify that the local destination still
matches the planner's expectation:

1. If the planner thought the local path was absent, confirm it is still absent.
2. If the planner thought the local path contained baseline content, confirm it
   still matches baseline before replacing it.
3. If the path now exists as a directory, symlink, or different file, do not
   overwrite it from an old download action.
4. If the local parent changed in a way that makes the operation unsafe, stop and
   let replan decide.

For remote destructive or overwriting actions, the executor should verify Graph
truth at the point of mutation:

1. Upload overwrite: the remote item ID/eTag still matches the item the planner
   expected.
2. Remote delete: the remote item identity still matches the planned target.
3. Remote move: the source item still exists and still has the expected identity
   and freshness fields.
4. Remote create under a parent: the parent still exists and is still the
   intended parent.

Cheap worker-start checks such as "path still exists" or "size still looks
right" belong here only as prefilters. They are useful for avoiding obvious
waste, but they do not replace strong checks such as content hash, item ID, or
eTag for destructive side effects.

Chunked uploads need an additional version of the same idea: the file can change
after the start-time precondition passes. Large upload code should periodically
confirm that the source still matches the expected identity or content state. If
the file clearly changed during upload, abort and return a stale/superseded
outcome.

Pros:

1. This is the strongest correctness protection.
2. It works even if observation is delayed or incomplete.
3. It keeps conflict decisions in the planner instead of letting the executor
   reinterpret a newer local or remote edit.
4. It mirrors mature sync-client behavior: validate the concrete file or item
   before mutating it.

Cons:

1. It adds stat/hash/Graph-call cost.
2. It can cause more abort/replan cycles while applications are actively writing
   files.
3. Large-file validation needs careful design so it does not duplicate expensive
   hashing unnecessarily.

Recommendation:

Do this first for upload, download, and destructive/overwriting remote actions.
The executor is the last line of defense before mutation, so it must be able to
say "this action was valid for the old snapshot, but it is no longer safe."

### Group B. Latest committed truth rejection before work starts

This group covers the old ideas 6 and 11, plus the explicit worker-start gate
added after the local-truth discussion.

The point is: before a worker begins meaningful work, reject the action if
latest committed `local_state` or `remote_state` already proves the action's own
assumptions false.

This is different from Group A. Group A asks live ground truth. Group B asks the
engine's latest committed observation database.

Examples:

1. Upload action: the action expected local hash/size/identity A, but
   `local_state` now says the path is absent or has hash/size/identity B.
2. Upload overwrite: the action expected remote item ID/eTag A, but
   `remote_state` now says item ID/eTag B.
3. Download action: the action expected remote hash/size A, but `remote_state`
   now says the item changed or disappeared.
4. Download-to-absent action: the action expected no local file, but
   `local_state` now says a local item exists there.
5. Delete action: the action expected a local or remote item to still match
   baseline, but current truth says it changed independently.

There are two places to apply the same predicates:

1. Worker-start check: when a worker receives an action, but before hashing,
   opening upload sessions, downloading content, creating folders, or deleting
   anything.
2. Engine admission check: before the engine sends an action from the outbox to
   a worker.

The worker-start check should come first because it also protects work already
submitted to the worker channel. Engine-side admission is a later optimization
that keeps stale work out of workers in the first place.

Pros:

1. Avoids CPU, disk I/O, hashing, upload-session setup, and Graph calls for work
   already known to be obsolete.
2. Lets workers reject stale actions without needing a newer plan.
3. Uses the same current-truth tables the planner will read on the next replan.
4. Helps the large-stale-outbox case because old uploads can be skipped quickly.

Cons:

1. Requires per-action predicates that say which fields are strong enough to
   disprove the action.
2. Adds store reads to the dispatch/start path.
3. Must respect local truth confidence. If local observation is suspect, absence
   or mismatch may require a full scan instead of immediate rejection.
4. Must not treat missing rows as proof unless the observation scope makes
   absence authoritative.

Recommendation:

Do this after local and remote current truth are both fresh enough to trust. Put
the first version in the worker-start path, then move the same predicates earlier
into engine admission when queue ownership is clearer.

### Group C. Stop stale queued work when a replan is due

This group covers the old ideas 4, 5, and 10.

The point is: once the runtime knows a replan is due, it should stop launching
more work from the old plan. Observations can continue while workers run, and a
quick replan from already-fresh local/remote truth is robust and cheap. The
engine does not need to keep draining a huge old outbox before it can converge.

There are two separate ideas here:

1. Policy: when pending replan exists, stop feeding old outbox work.
2. Mechanism: keep the worker dispatch buffer short enough that very little work
   escapes engine ownership before the policy can take effect.

A shorter dispatch channel buffer gives much of the practical benefit, but it
does not replace the policy. If the runtime continues dispatching while a
pending replan exists, a short buffer merely slows the stale work down. It still
lets old work keep launching one item at a time. The policy is what says "we
know a replan is due, so do not launch more work from the old plan."

The simplest version should be:

1. When a pending replan is set, stop dispatching old outbox actions. A replan
   signal that is already ready is consumed before dispatch is enabled for the
   current watch-loop step.
2. Retire the not-yet-dispatched outbox actions from the old plan as superseded
   work. Do not mark them successful, do not admit their dependents, and do not
   create ordinary retry rows for them.
3. Reduce the dispatch channel buffer to zero or one, unless measurements prove
   that a larger buffer matters.
4. Keep the worker completion buffer separate from the dispatch buffer. The
   completion channel can remain buffered so workers do not block while reporting
   results.
5. Let already-running actions finish unless they fail their own preconditions
   or receive cancellation.
6. Let worker-start latest-truth checks validate actions that already escaped
   into the worker channel. If that action's own local/remote source and
   destination truth still matches the plan, the worker may run it. If that
   exact truth changed, reject it as superseded.
7. Replan as soon as running work settles enough for the runtime to install a
   fresh plan.

"Old plan" and "semantically stale action" are not the same thing. A pending
replan means the installed plan is no longer complete enough to keep launching
from its outbox. It does not mean every already-submitted action is wrong. An
already-submitted action may still be safe and useful if its own pre-start
latest-truth check and live side-effect preconditions pass.

Retiring the outbox is necessary because the current watch runtime only replans
when both outbox and running work are empty. If dispatch is merely paused while
the outbox remains populated, the runtime can get stuck waiting for an outbox it
will no longer dispatch. The action objects are not durable truth, so discarding
not-yet-dispatched actions is correct as long as the engine records a clear
superseded/retired outcome for debug and does not treat those actions as
successful.

Once old outbox work has been retired, a recoverable local observation failure
cannot simply drop the pending batch. The engine must keep the retired work
retired, but it also must reschedule the same dirty/full-refresh intent through
`DirtyBuffer` so a later steady-state replan can install the replacement runtime
from current truth.

The earlier idea of splitting outbox/submitted/running into separate formal
states is now optional. A short or unbuffered dispatch channel plus a
worker-start stale check may remove most of the need for a full submitted-state
protocol. A submitted/running split is still useful if we want precise status,
precise cancellation of not-yet-started work, or a larger buffer for measured
throughput reasons.

Pros:

1. Stops the million-stale-uploads case from continuing to launch old work.
2. Keeps the design simple if the dispatch buffer is small.
3. Avoids making workers compare against a newer plan.
4. Lets observation proceed concurrently while work drains or is skipped.

Cons:

1. Some old work that was still valid may be discarded and recomputed by the
   next plan.
2. Dependency graph accounting must stay correct when old actions are discarded
   or superseded.
3. Held retry/scope state must be reconciled with the replacement plan.
4. If we later need a larger dispatch buffer, submitted/running state may become
   necessary again.

Recommendation:

Use both the policy and the shorter buffer. Do not start with a large
submitted/running protocol. Pending replan stops old outbox dispatch, retires
the not-yet-dispatched outbox as superseded runtime work, makes the watch
dispatch channel unbuffered, and worker-start validation now protects work that
already escaped into a worker.

### Group D. Incrementally update local truth from re-observed watch scopes

This group is the old idea 9, but with the important clarification that there is
no separate local event journal. This group is implemented for ordinary file and
directory watch observations, safety scans, and the explicit suspect-local-truth
recovery triggers listed below.

The desired behavior is exactly this:

1. A local filesystem event arrives.
2. The event is used only to pick the affected scope.
3. The observer re-observes that file, directory, or subtree using the
   filesystem.
4. The store updates `local_state` directly from that re-observation.
5. The periodic full local scan remains the safety and repair mechanism.

The current local observer already performs narrow work:

1. File create: stat and hash that file.
2. File modify: coalesce writes, then stat and hash that file.
3. File delete: emit a delete for that path and update watch/cache state.
4. Directory create: add watches and scan that new directory subtree.
5. Periodic safety scan: walk the full sync root as a backstop.

Before this increment, those narrow observations did not become durable
planner-visible local truth. They were reduced to dirty signals, and the next
steady-state replan called `FullScan` and `ReplaceLocalState`, which rebuilt the
entire `local_state` table. The watch loop now applies scoped
`localObservationBatch` values to `local_state` as they arrive and still uses
full snapshots for startup, safety scans, and suspect-truth recovery.

Concrete update rules:

1. File create or modify: upsert one `local_state` row for the exact path after
   stat/hash observation.
2. File delete or rename-away: delete the exact path row.
3. Directory create: scan that new subtree and upsert the discovered rows.
4. Directory delete or rename-away: delete all `local_state` rows under that
   path prefix.
5. Directory modify where the platform does not provide child detail: ordinary
   folder mtime/write noise is ignored, while child events or safety scans carry
   the concrete re-observed rows.
6. Rename/move without a reliable paired event: model as delete old path plus
   create new path after observing the new path if present.
7. Watch overflow, dropped local event channel send, watcher error, root
   identity change, filter/policy change, or uncertain recursive rename: mark
   local truth suspect and run a full local scan promptly.

This changes the meaning of `local_state` from "the last complete local snapshot
installed by a full scan" to "the current best-known local mirror, maintained by
scoped observation and periodically repaired by full scans."

Useful store APIs:

1. `UpsertLocalStateRows(ctx, rows)` for observed file rows or scoped directory
   scan rows.
2. `DeleteLocalStatePath(ctx, path)` for exact file deletion.
3. `DeleteLocalStatePrefix(ctx, path)` for directory deletion or rename-away.
4. `ReplaceLocalState(ctx, rows)` retained for startup, safety scan, and
   recovery from suspect truth.
5. A local truth confidence bit and recovery reason in `observation_state`.

Pros:

1. Makes local observation symmetric with remote delta observation in the way
   that matters: both update durable current truth before replan.
2. Avoids walking the whole sync root for ordinary file writes.
3. Gives worker-start latest-truth checks useful local data.
4. Makes the five-minute safety scan a real repair backstop instead of the
   normal path for watch-mode truth freshness.

Cons:

1. Adds store mutation APIs.
2. Prefix deletes and scoped directory reconciliation need careful transactions.
3. Local event streams are lossy, so recovery paths must be explicit.
4. The store must remain the single writer for SQLite and any memory projection.

Recommendation:

Do this. Use inotify/fsnotify events as scope selectors, not as truth. Store the
result of re-observation in `local_state`. Do not create another event journal.

### Group E. Superseded action result

This group explains the old idea 12.

`superseded` is not another retry state and not another kind of plan. It is an
engine result category for an action that was valid when the plan was built, but
should not be executed anymore because newer truth or a newer runtime decision
made that exact action obsolete.

Plain English version:

1. The planner said "upload `a.txt` because local has version A and remote has no
   file."
2. Before the worker starts, latest `local_state` says `a.txt` was deleted.
3. Or, immediately before upload, the filesystem says `a.txt` now has different
   content.
4. Retrying the exact old upload action is wrong, because the old action is about
   version A.
5. Calling it a scary transfer failure is also wrong, because nothing external
   failed.
6. The right engine-level meaning is: "discard this old action; keep or trigger
   replan; let the planner decide what current truth now requires."

That is what `resultSuperseded` means.

It should be produced by cases like:

1. Worker-start latest-truth check says `local_state` or `remote_state` already
   disproves the action.
2. Live side-effect precondition says the local file or remote item no longer
   matches the planned source/destination.
3. Pending replan policy retires a not-yet-dispatched engine-owned outbox action
   from the old plan so the runtime can install a replacement plan.

It should not mean:

1. The upload failed because the network is down.
2. Graph returned a transient 503.
3. The disk returned an I/O error.
4. The action succeeded.
5. The engine knows the replacement action without replanning.

Engine behavior should be:

1. Do not treat the exact old action as successful.
2. Do not admit dependent actions from the old dependency graph as if the action
   completed normally.
3. Do not create an ordinary retry row that will run the exact same stale action
   later.
4. Keep or set pending replan.
5. Let the next plan from current `local_state`, `remote_state`, and `baseline`
   produce replacement intent.
6. Report this as normal daemon convergence, not as a user-visible transfer
   failure, unless repeated supersession prevents convergence.

Why not just use `ErrActionPreconditionChanged` forever? It may be enough for a
first implementation, but the name "precondition changed" describes the local
executor fact, not the runtime policy. If the engine maps it into normal retry
machinery, it can keep durable retry rows for actions that should simply be
discarded. `superseded` names the runtime decision: this old action is obsolete;
do not retry it as-is.

Pros:

1. Cleaner logs and status.
2. Avoids retrying exact stale actions.
3. Separates expected daemon races from real transfer failures.
4. Gives the dependency graph a clear non-success outcome for old work.

Cons:

1. Adds result-classification policy.
2. Needs explicit interaction with `retry_work`, `block_scopes`, dependency
   graph completion, and watch-mode replan.
3. Needs tests for one-shot and watch behavior.

Recommendation:

The stale-precondition path now maps into a first-class `superseded` result.
Future pre-start and side-effect gates should return the same runtime meaning.
The important rule is no ordinary retry of the exact stale action.

### Group F. Observability for stale work and pending replan latency

This group is the old idea 15.

Useful counters and timings:

1. Number of actions rejected by latest `local_state`.
2. Number of actions rejected by latest `remote_state`.
3. Number of actions rejected by live local precondition.
4. Number of actions rejected by live remote precondition.
5. Number of actions skipped because pending replan existed.
6. Number of actions skipped before worker start by latest committed truth.
7. Number of not-yet-dispatched outbox actions retired because pending replan
   existed.
8. Time from dirty signal observed to pending replan set.
9. Time from pending replan set to dispatch pause.
10. Time from dispatch pause to old outbox retirement.
11. Time from outbox retirement to running worker count reaching zero.
12. Time from pending replan set to actual replan start.
13. Time spent observing local truth during replan.
14. Time spent observing or applying remote truth during replan.
15. Time spent planning.
16. Time from replan start to new outbox installed.
17. Time from new outbox installed to first new action dispatched.
18. Outbox count when pending replan was set.
19. Outbox count retired from the old plan.
20. Dispatch buffer depth when pending replan was set.
21. Running count when pending replan was set.
22. Worker idle time while pending replan exists.
23. Worker idle time while local observation is running.
24. Worker idle time while remote observation/application is running.
25. Worker idle time while planning is running.
26. Worker idle time while the engine is waiting for old running actions to
   finish.
27. Number of superseded outcomes that later converged cleanly.
28. Number of superseded outcomes that became user-visible failures.
29. Number of scoped local commits.
30. Number of full local scans triggered by suspect local observation.

Useful timestamped lifecycle events:

1. `dirty_signal_observed`: local event, remote batch, retry timer, trial timer,
   or maintenance trigger entered the watch loop.
2. `pending_replan_set`: the runtime accepted that current work needs a
   replacement plan.
3. `dispatch_paused_for_replan`: dispatch from old outbox was disabled.
4. `old_outbox_retired`: not-yet-dispatched old actions were removed from the
   outbox and counted as superseded/retired.
5. `waiting_for_running_actions`: runtime is waiting only for already-running or
   already-submitted work.
6. `running_actions_drained`: old running/submitted work no longer blocks replan.
7. `steady_state_replan_started`: current truth refresh and planning begin.
8. `local_truth_refresh_started` and `local_truth_refresh_finished`.
9. `remote_refresh_started`, `remote_refresh_committed`, and
   `remote_refresh_applied` for full remote refresh activity. A future narrower
   remote-batch apply event may still be useful if debug traces need that
   separation.
10. `planning_started` and `planning_finished`.
11. `new_plan_installed`: fresh dependency graph/outbox is installed.
12. `first_post_replan_dispatch`: first action from the new plan is handed to a
   worker.

This increment implements events 2 through 8 and 10 through 12 for the
pending-replan path, and reuses the existing remote refresh debug events for
full-refresh activity. `dirty_signal_observed` and finer remote-batch apply
events remain follow-up instrumentation.

Worker-idle instrumentation should answer a specific question: "were workers
idle because replanning was legitimately happening, or because the coordinator
held work longer than necessary?" The simplest implementation is not per-worker
tracing. Keep aggregate counters:

1. `workers_total`
2. `workers_running`
3. `workers_idle = workers_total - workers_running`
4. current replan phase, such as `none`, `pending_replan`, `retiring_outbox`,
   `waiting_running`, `observing_local`, `applying_remote`, `planning`, or
   `installing_plan`
5. accumulated idle duration per phase

This can be measured at phase transitions using the runtime's existing
single-owner loop: when phase changes, add `idleWorkers * elapsed` to the
previous phase bucket. That avoids per-action polling and avoids lots of checks
inside the worker hot path.

Pros:

1. Makes daemon behavior explainable.
2. Shows whether stale-work rejection is a correctness path or a performance
   hot path.
3. Helps tune dispatch buffer size.
4. Helps prove that scoped local observation actually reduced full scans.

Cons:

1. More instrumentation surface.
2. Bulk sync logs can get noisy if per-action warnings are overused.
3. Metrics need stable names and ownership.

Recommendation:

Add observability alongside behavior changes. This increment adds the
pending-replan lifecycle events with timestamps and aggregate outbox/running/idle
worker counters. Per-action latest-truth rejection counters and accumulated
idle-duration buckets remain the final observability follow-up now that
worker-start validation and scoped local truth commits have landed.

## 5. Suggested Implementation Order

1. Add live side-effect preconditions for upload, download, local delete, remote
   delete, remote move, and remote overwrite.
2. Add chunk-boundary validation for large uploads after the start-time upload
   precondition exists.
3. Add scoped local-truth commits for watch-mode file and directory
   observations. Implemented in the incremental local-truth commit increment.
4. Add local truth suspect/recovery state for dropped events, watcher errors,
   uncertain recursive changes, root identity changes, and filter/policy
   changes. Implemented for dropped local observation batches, dropped hash
   requests, watcher errors, and failed safety scans in the incremental
   local-truth commit increment.
5. Add worker-start validation against latest committed `local_state` and
   `remote_state`, including move source/destination endpoints. Implemented in
   the worker/admission truth-validation increment.
6. Add a superseded/stale-action result path that does not retry the exact old
   action as-is. Implemented in the stale-work superseded-result increment.
7. Stop dispatching old outbox actions once a pending replan exists and retire
   the not-yet-dispatched old outbox as superseded work. Implemented in the
   merged watch-stale-work increments.
8. Reduce the dispatch channel buffer to zero or one unless measurement proves a
   larger buffer is needed. Implemented as an unbuffered dispatch channel in
   the merged watch-stale-work increments.
9. Add engine-side admission rejection using the same latest-truth predicates,
   after the worker-start path proves the predicates are correct. Implemented
   in the worker/admission truth-validation increment.
10. Add submitted/running state only if the shorter buffer and worker-start gate
   are insufficient for status, cancellation, or throughput.
11. Add observability for stale-precondition outcomes, superseded outcomes,
   pending replan latency, scoped local commits, and full-scan recovery causes.

## 6. Design Recommendation

The recommended design is not "workers should validate against a newer plan."
The recommended design is:

1. Keep one installed runtime plan at a time.
2. Stop launching old outbox work once the engine knows a replan is pending, and
   retire not-yet-dispatched old outbox actions as superseded work. Implemented
   in the merged watch-stale-work increments.
3. Keep the dispatch channel short enough that very little work escapes engine
   ownership before pending-replan policy can stop it. Implemented with an
   unbuffered watch dispatch channel in the merged watch-stale-work increments.
4. Let local and remote observers keep durable current truth fresh. Remote
   deltas already mostly do this; local watch events should do it by
   re-observing the affected scope and incrementally committing `local_state`.
5. Before a worker begins meaningful work, validate the action against latest
   committed `local_state` and `remote_state`; reject it only if that truth
   disproves the action's assumptions. Implemented in the worker/admission
   truth-validation increment.
6. Let workers prove live ground-truth preconditions before each dangerous side
   effect.
7. Return stale-precondition/superseded outcomes when either validation gate
   fails. Implemented for worker-start and admission validation; executor live
   preconditions remain follow-up.
8. Let the next normal replan derive replacement intent from current truth.

That keeps the planner as the owner of sync intent, the runtime as the owner of
queue/admission/lifecycle, and the executor as the owner of side-effect safety.

The important local-observation decision is this:

1. Raw fsnotify/inotify events are not truth.
2. Full local scans are still necessary, but they should not be the normal
   response to every ordinary local write.
3. Re-observed file/folder scopes should update `local_state` immediately in
   watch mode.
4. Workers may then reject stale actions against latest committed local truth
   before beginning work.
5. Workers still perform live filesystem preconditions before side effects,
   because committed truth can be behind the real filesystem by a small race.
6. Full scans remain the safety and recovery path when the engine has reason to
   doubt local event completeness.
