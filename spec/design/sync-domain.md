# Sync Domain

GOVERNS: internal/sync/syncdomain/actions.go, internal/sync/syncdomain/condition_keys.go, internal/sync/syncdomain/content_filter.go, internal/sync/syncdomain/core_types.go, internal/sync/syncdomain/domain_baseline_view.go, internal/sync/syncdomain/domain_decision_state.go, internal/sync/syncdomain/crash_points.go, internal/sync/syncdomain/domain_clock.go, internal/sync/syncdomain/domain_debug_events.go, internal/sync/syncdomain/domain_delete_safety.go, internal/sync/syncdomain/domain_graph_error.go, internal/sync/syncdomain/domain_observation_mode.go, internal/sync/syncdomain/domain_path_kind.go, internal/sync/syncdomain/domain_runtime_decision.go, internal/sync/syncdomain/domain_worker.go, internal/sync/syncdomain/enums.go, internal/sync/syncdomain/errors.go, internal/sync/syncdomain/issue_types.go, internal/sync/syncdomain/permission_capability.go, internal/sync/syncdomain/retry_work_key.go, internal/sync/syncdomain/scope_block.go, internal/sync/syncdomain/scope_key.go, internal/sync/syncdomain/scope_semantics.go, internal/sync/syncdomain/tracked_action.go, internal/sync/syncdomain/types.go, internal/sync/syncdomain/worker_result.go, internal/sync/syncdomain/domain_baseline_mutation.go, internal/sync/syncdomain/domain_conflict_stem.go, internal/sync/syncdomain/domain_file_change_time.go, internal/sync/syncdomain/domain_item_type.go, internal/sync/syncdomain/domain_local_hash_cache.go, internal/sync/syncdomain/domain_observation_findings.go, internal/sync/syncdomain/domain_observation_state.go, internal/sync/syncdomain/domain_pending_create.go, internal/sync/syncdomain/domain_poll_interval.go, internal/sync/syncdomain/domain_remote_item.go, internal/sync/syncdomain/domain_sqlite_rows.go, internal/sync/syncdomain/domain_stored_time.go, internal/sync/syncdomain/domain_sync_root_identity.go, internal/sync/syncdomain/domain_row_types.go, internal/sync/syncdomain/domain_truth_status.go, internal/sync/syncdomain/domain_local_rules.go, internal/sync/syncdomain/doc.go

Implements: R-2.1 [verified], R-2.7 [verified]

## Overview

This family owns the sync domain model: the vocabulary every other sync family
speaks. `Action` and its action types, `ScopeKey` and the scope taxonomy,
`ItemType`, `BaselineView`, `BaselineSnapshot` and `BaselineEntry`, `RetryWorkKey`, the condition and
issue keys, `SyncMode`, the content filter, the runtime decision vocabulary,
the engine clock capability, the decision table's input (`DecisionState`,
the contract between the store's comparison rows and the planner's table),
and the vocabulary of the engine's debug event stream
(`domain_debug_events.go`: the event, its type, the kinds the observers
report and the remote observer's name). The engine owns the stream's sink,
its own event kinds and the local observer's name (sync-engine.md,
`engine_debug_events.go`); the vocabulary sits here because the watch
observers, a lower family, report the inputs they drop into the same stream.

It is a leaf. It performs no I/O, holds no mutable runtime state, and depends on
no other sync family.

## What Belongs Here

An identifier lives here only when two or more sync families name it, or it
is a member of an enum they share; one family's own vocabulary is that
family's. Since 2026-11-24 the engine holds its debug event kinds, the Graph
seams it is configured with (`engine_graph_seams.go`), the one-shot pass's
cancel causes, the cancellation-echo check, the runtime condition key, the
held-delete counts, the remote findings batch, the rebind hint and the
block-scope row conversion; each consumer declares the Graph calls it makes
(syncobserve: `DeltaFetcher`, `SocketIOEndpointFetcher`, `ItemReader`;
syncexec: `ItemClient`), so no seam carries a method no consumer calls; the
executor and the observers hold their own sentinels (the worker pool's cancel
causes over the shared `CancelCauseError`, the path-escape, sync-root and
watch sentinels); the decision table, its output kinds and the
reconciliation row are the planner's (sync-planning.md); and the rule that
names a download's partial file is synctree's (`IsOwnedTransferArtifactName`),
so this package imports nothing of `driveops`.

Some identifiers stay although one family names them in production, each for
a reason: the members of a shared enum stay with it; the encoding pairs
(`StoredTimeNano` and `RemoteModTime`, which decodes it) stay together; a
decision over every member of `IssueType` or `ConditionKey`
(`IssueTypeIsRootReadFailure`, `ConditionClearsAutomatically`) stays with the
enum and its completeness test; the two retry-work key derivations
(`RetryWorkKeyForAction`, `RetryWorkKeyForCompletion`) must agree, so they
share one helper here; values the tests of several families build
(`MutationFromActionOutcome`) or the e2e suite reads (`ConflictStemExt`) stay
where every one can import them; and the meaning of a Graph failure
(`ServerAnswer`, `IsRequestTimeout`, `IsServiceUnreached`) is read by the
engine, the scheduler and the completion record. A completion's
`ServerAnswer()` is derived from its status and error on each call rather
than stored, so a completion a test builds from a status cannot disagree with
it.

## Why This Is A Family And Not A Bag Of Types

The repo's own guardrails warn against "fake intermediate boundaries" and
against abstraction without authority, and a vocabulary that is only a struct
bag would be exactly that. This family clears that bar because the types own
their invariants rather than merely carrying fields.

`ScopeKey` is the clearest case: it decides `PersistsInBlockScopes()`,
`BlocksAction()`, `CoveredPath()`, `IsGlobal()`, and its own human rendering --
nineteen methods of policy that would otherwise be duplicated across the store,
the planner, and the runtime. `ActionType` answers `isMember()` and `Value()`.
`PermissionCapability` answers `IsLocal()`. Moving these apart from their data
is what would produce an anemic model.

`ActionType` (`enums.go`) is held by the `exhaustive` linter: every switch on
it names every member and none carries a `//nolint:exhaustive`, so lint
passes only once a new member is decided in each. `isMember()` is the
membership list; `Value()` refuses a value outside it, and
`parseActionType` refuses a stored name outside it, so no store row carries
one, and the planner refuses a built action outside it, or a folder create
whose side is outside `FolderCreateSide` (sync-planning.md). No decision can
therefore meet such a value. Where one returns a plain value, its default
panics with `OutsideTheEnum`; where one returns an error, its default is one
built with `OutsideTheEnum`. The linter sees only the member lists, so
`TestActionTypeSwitches_EveryDefaultRefuses` holds the defaults: it reads the
package's product files, takes every switch whose case clauses name a
constant of `ActionType` or `FolderCreateSide`, and fails when its default
does not end in a panic or a return that calls `OutsideTheEnum`, or when it
has none; `String()` and `isMember()` are its named exceptions. Until
2026-11-12 most defaults answered: not admitted by any mode, not a local
write, creates nothing, fresh, no permission capability, not blocked by a
write scope, no display size, nothing to ledger, a remote delete side for
anything not a local delete. Which side a delete removes content from is one
switch, `DeleteSafetySideOf` (`domain_delete_safety.go`), for the planner's
hold and its confirmation matching and for the store's delete ledger. A
`Direction()` method classified action types as upload, download or delete;
no product code read it, and it is gone with its type, as is a change event's
source (`changeSource`), which every event carried and only one check read: a
remote projection skipping events that were not remote, which no caller ever
handed it. The local observer's entries are `LocalEntry` values, with only the
fields a local entry has: the change type the remote side once set and the
remote-only fields (item and parent IDs, drive, name, tags, the unknown-size
mark, the re-upload mark) went with the old shared change event, which the
remote side stopped using once it reported `RemoteItem` values.

`IssueType` (`issue_types.go`) names every sync condition: the issue type of
an observation issue, the condition type of a retry_work row, a scope's
default condition, and the condition of a runtime result decision, including
the transient ones (`request_timeout`, `transient_conflict`, ...) that no
store keeps. A member's value is the persisted string. `isMember()` is the
membership list. Every decision made per issue type is a switch that names
every member, its `default` answering only a value outside the enum, so the
exhaustive linter fails until a new member is decided in each: membership
(`isMember`), truth
availability (`truthVerdictForIssueType`, sync-store.md), the condition family
(`ConditionKeyForIssueType`), the retry curve (`retryDelayForCondition`), the
permission boundary scope (`scopeKeyForPermissionEvidence`), the
block-scope activation wording (`blockScopeActivationWording`), whether an
observation issue leaves a file unsynced, which fails a one-shot sync
(`IssueTypeLeavesFileUnsynced`, R-6.6.28: every member does except
`deletes_held`, a deliberate hold, `local_interrupted_removal`, a report
about a leftover at an internal name sync never syncs, and
`removed_folder_kept`, what a removed shared folder's release kept: content
sync does not sync, a synced folder that cannot be written to or a synced
entry the local delete could not move to the trash or delete, both already
synced, or a change that can no longer be sent to the shared folder, each
of which holds the folder's local copy through the release's own report.
The change that could not be sent still fails the run, counted from that
report and not from its row (`LeftWork.NotUploaded`, sync-engine.md);
`TestIssueTypes_EveryTypeIsClassifiedForOneShotExit`), and whether an issue
at the root read scope is the read failure of a whole tree
(`IssueTypeIsRootReadFailure`, only `remote_read_denied` and
`remote_root_not_found`;
`TestIssueTypeIsRootReadFailure_EveryMemberIsDecided`). None of them
carries a `//nolint:exhaustive`; until 2026-11-11 the boundary scope, the
activation wording and the one-shot exit verdict did, remote
truth was two equality tests and the condition family a lookup table, so a
member they did not name fell through to "not blocking", "no family", the
one-second curve or a warning. Each switch has a test that reads the members
from `issue_types.go` and states every member's verdict, and each states a
verdict for a value outside the enum, which only a programming error can
make: unavailable truth on both sides of its path,
`ConditionUnexpectedCondition` (under any scope, rather than the scope's own
condition), the durable curve, no boundary scope, the generic line, a file
left unsynced.

`ConditionKey` is held the same way. Whether a condition clears without the
user (`ConditionClearsAutomatically`, here: throttling and an outage) is one
switch over every key; the block-scope line takes its level from it through
the issue type's family, and the watch condition summary reads it to stay
below a warning, so the two cannot disagree. A key outside the set is not
known to clear by itself. Each key's console label is a switch over every key
too (`conditionLabelText`, sync-engine.md). Their tests read the keys from
`condition_keys.go` (`stringConstantsOfType`).

An action's item type has one rule (`domain_item_type.go`). `ResolveItemType`
reads the view's observed side: the remote observation, then the local one,
then the baseline, the type the item had when it was last synced, which
decides only for a path neither side holds (a cleanup). A file is a type like
any other: an observed file never yields to a folder the baseline recorded,
so a folder both sides replaced with the same file publishes as that file.
`DeletedItemType` narrows it for a delete to the side the delete removes the
item from, which differs across a type change. The baseline-mutation
publication, a delete's outcome and a move with no baseline all read these
two; a move with a baseline is typed by that baseline, the item it moves
(`movedItemIsFolder`, sync-planning.md). Until 2026-11-23 a second rule took
the first non-file type among remote, baseline and local, reading a file as
"unknown", and `ResolveItemType` preferred the baseline over a deleted remote
observation, a shape no planner view carries: remote truth comes from
`remote_state` rows, which hold live items only
(`TestResolveItemType_ReadsTheObservedSideOverTheProducersShapes`).

`hash_error` is what the scanner records for a panic in a hash worker and for
a file it could not read to hash, and users see it as the condition of the
same name (`ConditionHashError`). Before store generation 29 the stored value
had a name that covered only the panic; the rename changed what
`observation_issues` holds, which is why it waited for a generation. The stores
convert to and from the stored string at their own boundary and refuse a
value that is not a member (sync-store.md); elsewhere a value becomes a plain
string only as a log attribute or in CLI output.

`PlanningSnapshot` (`domain_sqlite_rows.go`) is the value the store's planning
read hands the planner (sync-store.md, `ReadPlanningSnapshot`): the content
drive, the planner-visible local and remote rows, their comparison rows, the
filtered observation issues and the `BaselineSnapshot`, all from one store
transaction. It lives here because the planner may not import the store
(depguard): the two meet on a domain value. Its comparison rows
(`ComparisonRow`) are raw facts and cannot carry a comparison kind; the
planner decides them into its own `DecidedComparison` (sync-planning.md,
`DecideComparisons`), which the store's tests that check the comparison SQL
call too. The decision table's input, `DecisionState` with its validity rules
(`domain_decision_state.go`), stays here: the store's rows produce it and the
store's test support and the engine's invariant check hold its rules, while
the table that decides it (`Decide`), its output kinds and the
reconciliation row are the planner's.

## Ownership Contract

- Owns: the sync domain vocabulary and the invariants those values enforce.
- Does Not Own: persistence, planning policy, observation, execution, or
  runtime orchestration.
- Source of Truth: none -- these are values, not state.
- Allowed Side Effects: none. The one capability here, `SyncClock`, is an
  interface; its production implementation reads the wall clock and starts
  goroutines (`Go`), and those are the only effects any file in this family
  performs.
- Mutable Runtime Owner: none.
- Error Boundary: sentinel errors are declared here so callers across families
  can branch on one identity; no file here produces an error from I/O.

## Why It Exists

Before this family, the vocabulary lived in `sync-planning.md`. Because every
other family needs `Action`, `ScopeKey`, and `Baseline`, every other family
appeared to depend on planning -- 28 symbols from the store, 32 from
observation, 24 from execution. That inverted the real relationship: planning
is a deterministic transform that sits *above* the store, not a foundation
beneath it.

Separating the vocabulary drops planning's inbound coupling to a single symbol
and makes the order a strict downward chain. Since the split each family is
a package, and the compiler and depguard enforce the chain. See the layering
section in [system.md](system.md).

## Verified By

| Behavior | Evidence |
| --- | --- |
| An action's item type is read from the observed side, remote then local then baseline, and a delete's from the side it deletes; a file never reads as "unknown". | `TestResolveItemType_ReadsTheObservedSideOverTheProducersShapes`, `TestPublicationMutation_SyncedUpdate_TypeFromObservedSide` |
| Scope keys own their own blocking, path-coverage, and rendering policy rather than exporting raw fields for callers to interpret. | `TestScopeKey_BlocksAction`, `TestScopeKey_CoveredPath`, `TestScopeKey_CoveredPathMatchesFamilyAccessors`, `TestScopeKey_Humanize`, `TestScopeKey_IsGlobal`, `TestDescribeScopeKey_Service` |
| Every decision on the action type panics on a value outside the enum instead of answering it, and every one that returns an error refuses such a value with one (the row value, mutation classification, the publication mutation, the delete ledger, the remote mirror, worker dispatch). | `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `TestPlannerActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `TestExecutorActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `TestEngineActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `TestActionTypeRefusals_ATypeOutsideTheEnumIsAnError`, `TestActionTypeSwitches_EveryDefaultRefuses` |
| Every declared `IssueType` constant is a member and parses back to itself, with no two sharing a value; any other stored string, the empty one included, does not parse. | `TestIssueType_EveryDeclaredConstantIsAMemberAndParses`, `TestParseIssueType_RefusesAValueNoMemberHas` |
| Every issue type, read from source, has a stated condition family or none (the transient runtime members, whose retried results then carry no `condition_key` rather than the unexpected one); a value outside the enum is the unexpected condition under any scope, never "no family"; every declared condition key has a display rank of its own. The engine's per-issue-type decisions are held the same way (sync-engine.md). | `TestConditionKeyForIssueType_EveryMemberIsDecided`, `TestConditionKeyForStoredCondition_ValueOutsideTheEnumIsUnexpectedUnderAnyScope`, `TestRuntimeConditionKey_TransientConditionsHaveNoFamily`, `TestConditionKeyRank_EveryDeclaredKeyHasItsOwnRank`, `TestConditionClearsAutomatically_EveryKeyIsDecided` |
| Code outside the store holds the baseline as `BaselineView`, whose lookups return entries by value, so no holder can change what another reads; the store alone writes its cache (`baselineCache`: unexported maps and mutators), and an immutable `BaselineSnapshot` owns copies of the entries it was built from. The cache copies entries under its read lock, so an observer can read them while workers commit outcomes (R-6.3.4). The baseline holds no name index: case collisions are judged against the names on disk (`syncobserve.DirNames`). | `TestBaselineSnapshot_HandsOutValuesItOwns`, `TestBaselineCache_GetByPathCopyIsSafeAgainstConcurrentPut` |
| The bundled junk list (`IsBundledJunkName`, on by default through config, R-2.4.14) covers OS metadata, editor/IDE temporaries, Office `~$` owner files and SQLite `-wal`/`-shm`/`-journal` sidecars of databases whose name has an extension, so ordinary names such as `travel-journal` keep syncing. The `4913`, `~*` and `*~` rules match only a file leaf (`IsBundledJunkFileName`), never a folder or a path component. | `TestContentFilter_ExtendedJunkList`, `TestContentFilter_FileOnlyJunkRulesKeepFolders` |
| The runtime arms its timers through the engine clock, so a test clock controls retry, trial, and refresh timing coherently. | `TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionArmsRetryTimerInWatchMode`, `TestHandleRemoteObservationBatch_MountRootEnumerateClampRearmsRefreshTimerImmediately`, `TestCancelPendingTimers` |
