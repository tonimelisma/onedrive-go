# Sync Domain

GOVERNS: internal/sync/actions.go, internal/sync/condition_keys.go, internal/sync/content_filter.go, internal/sync/core_types.go, internal/sync/crash_points.go, internal/sync/domain_clock.go, internal/sync/domain_debug_events.go, internal/sync/domain_delete_safety.go, internal/sync/domain_graph_error.go, internal/sync/domain_io_seams.go, internal/sync/domain_observation_mode.go, internal/sync/domain_path_kind.go, internal/sync/domain_runtime_decision.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/issue_types.go, internal/sync/permission_capability.go, internal/sync/retry_work_key.go, internal/sync/scope_block.go, internal/sync/scope_key.go, internal/sync/scope_semantics.go, internal/sync/tracked_action.go, internal/sync/types.go, internal/sync/worker_result.go

LAYER: 0

Implements: R-2.1 [verified], R-2.7 [verified]

## Overview

This family owns the sync domain model: the vocabulary every other sync family
speaks. `Action` and its action types, `ScopeKey` and the scope taxonomy,
`ItemType`, `Baseline` and `BaselineEntry`, `RetryWorkKey`, the condition and
issue keys, `SyncMode`, the content filter, the runtime decision vocabulary,
the engine clock capability, and the vocabulary of the engine's debug event
stream (`domain_debug_events.go`: the event, its kinds the observers report,
the observer names). The engine owns the stream's sink and its own event
kinds (sync-engine.md); the vocabulary sits here because the watch observers,
a lower family, report the inputs they drop into the same stream.

It is a leaf. It performs no I/O, holds no mutable runtime state, and depends on
no other sync family.

## Why This Is A Family And Not A Bag Of Types

The repo's own guardrails warn against "fake intermediate boundaries" and
against abstraction without authority, and a vocabulary that is only a struct
bag would be exactly that. This family clears that bar because the types own
their invariants rather than merely carrying fields.

`ScopeKey` is the clearest case: it decides `PersistsInBlockScopes()`,
`BlocksAction()`, `CoveredPath()`, `IsGlobal()`, and its own human rendering --
nineteen methods of policy that would otherwise be duplicated across the store,
the planner, and the runtime. `actionType` answers `isMember()` and `Value()`.
`permissionCapability` answers `IsLocal()`. Moving these apart from their data
is what would produce an anemic model.

`actionType` (`enums.go`) is held by the `exhaustive` linter: every switch on
it names every member and none carries a `//nolint:exhaustive`, so lint
passes only once a new member is decided in each. `isMember()` is the
membership list; `Value()` refuses a value outside it, and
`parseActionType` refuses a stored name outside it, so no store row carries
one, and the planner refuses a built action outside it, or a folder create
whose side is outside `folderCreateSide` (sync-planning.md). No decision can
therefore meet such a value. Where one returns a plain value, its default
panics with `outsideTheEnum`; where one returns an error, its default is one
built with `outsideTheEnum`. The linter sees only the member lists, so
`TestActionTypeSwitches_EveryDefaultRefuses` holds the defaults: it reads the
package's product files, takes every switch whose case clauses name a
constant of `actionType` or `folderCreateSide`, and fails when its default
does not end in a panic or a return that calls `outsideTheEnum`, or when it
has none; `String()` and `isMember()` are its named exceptions. Until
2026-11-12 most defaults answered: not admitted by any mode, not a local
write, creates nothing, fresh, no permission capability, not blocked by a
write scope, no display size, nothing to ledger, a remote delete side for
anything not a local delete. Which side a delete removes content from is one
switch, `deleteSafetySideOf` (`domain_delete_safety.go`), for the planner's
hold and its confirmation matching and for the store's delete ledger. A
`Direction()` method classified action types as upload, download or delete;
no product code read it, and it is gone with its type, as is a change event's
source (`changeSource`), which every event carried and only one check read: a
remote projection skipping events that were not remote, which no caller ever
handed it.

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
(`conditionKeyForIssueType`), the retry curve (`retryDelayForCondition`), the
permission boundary scope (`scopeKeyForPermissionEvidence`), the
block-scope activation wording (`blockScopeActivationWording`) and whether an
observation issue leaves a file unsynced, which fails a one-shot sync
(`issueTypeLeavesFileUnsynced`, R-6.6.28: every member does except
`deletes_held`, a deliberate hold, `local_interrupted_removal`, a report
about a leftover at an internal name sync never syncs, and
`removed_folder_kept`, what a removed shared folder's release kept: content
sync does not sync, a synced folder that cannot be written to or a synced
entry the local delete could not move to the trash or delete, both already
synced, or a change that can no longer be sent to the shared folder, whose
own `remote_read_denied` issue already counts;
`TestIssueTypes_EveryTypeIsClassifiedForOneShotExit`). None of them
carries a `//nolint:exhaustive`; until 2026-11-11 the last three did, remote
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
user (`conditionClearsAutomatically`, here: throttling and an outage) is one
switch over every key; the block-scope line takes its level from it through
the issue type's family, and the watch condition summary reads it to stay
below a warning, so the two cannot disagree. A key outside the set is not
known to clear by itself. Each key's console label is a switch over every key
too (`conditionLabelText`, sync-engine.md). Their tests read the keys from
`condition_keys.go` (`stringConstantsOfType`).

`hash_panic` is narrower than its use: the scanner records it for a panic in
a hash worker and for a file it could not read to hash, and users see it as
the condition `hash_error` (`ConditionHashError`). The stored value keeps its
name, because a rename changes what `observation_issues` holds and so needs a
store generation of its own; the comment at the constant says so. The stores
convert to and from the stored string at their own boundary and refuse a
value that is not a member (sync-store.md); elsewhere a value becomes a plain
string only as a log attribute or in CLI output.

## Ownership Contract

- Owns: the sync domain vocabulary and the invariants those values enforce.
- Does Not Own: persistence, planning policy, observation, execution, or
  runtime orchestration.
- Source of Truth: none -- these are values, not state.
- Allowed Side Effects: none. The one capability here, `syncClock`, is an
  interface; its production implementation reads the wall clock, and that is
  the only effect any file in this family performs.
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
and makes the family order a strict downward chain, which is what
`devtool verify layering` enforces. See the layering section in
[system.md](system.md).

## Verified By

| Behavior | Evidence |
| --- | --- |
| Scope keys own their own blocking, path-coverage, and rendering policy rather than exporting raw fields for callers to interpret. | `TestScopeKey_BlocksAction`, `TestScopeKey_CoveredPath`, `TestScopeKey_CoveredPathMatchesFamilyAccessors`, `TestScopeKey_Humanize`, `TestScopeKey_IsGlobal`, `TestDescribeScopeKey_Service` |
| Every decision on the action type panics on a value outside the enum instead of answering it, and every one that returns an error refuses such a value with one (the row value, mutation classification, the publication mutation, the delete ledger, the remote mirror, worker dispatch). | `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `TestActionTypeRefusals_ATypeOutsideTheEnumIsAnError`, `TestActionTypeSwitches_EveryDefaultRefuses` |
| Every declared `IssueType` constant is a member and parses back to itself, with no two sharing a value; any other stored string, the empty one included, does not parse. | `TestIssueType_EveryDeclaredConstantIsAMemberAndParses`, `TestParseIssueType_RefusesAValueNoMemberHas` |
| Every issue type, read from source, has a stated condition family or none (the transient runtime members, whose retried results then carry no `condition_key` rather than the unexpected one); a value outside the enum is the unexpected condition under any scope, never "no family"; every declared condition key has a display rank of its own. The engine's per-issue-type decisions are held the same way (sync-engine.md). | `TestConditionKeyForIssueType_EveryMemberIsDecided`, `TestConditionKeyForStoredCondition_ValueOutsideTheEnumIsUnexpectedUnderAnyScope`, `TestRuntimeConditionKey_TransientConditionsHaveNoFamily`, `TestConditionKeyRank_EveryDeclaredKeyHasItsOwnRank`, `TestConditionClearsAutomatically_EveryKeyIsDecided` |
| `Baseline.GetCaseVariants` returns a copy of its sibling index under the read lock, so the watch observer can iterate it while workers commit outcomes through `Put` (R-6.3.4). | `TestBaseline_GetCaseVariantsCopyIsSafeAgainstConcurrentPut` |
| The bundled junk list (`isBundledJunkName`, on by default through config, R-2.4.14) covers OS metadata, editor/IDE temporaries, Office `~$` owner files and SQLite `-wal`/`-shm`/`-journal` sidecars of databases whose name has an extension, so ordinary names such as `travel-journal` keep syncing. The `4913`, `~*` and `*~` rules match only a file leaf (`isBundledJunkFileName`), never a folder or a path component. | `TestContentFilter_ExtendedJunkList`, `TestContentFilter_FileOnlyJunkRulesKeepFolders` |
| The runtime arms its timers through the engine clock, so a test clock controls retry, trial, and refresh timing coherently. | `TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionArmsRetryTimerInWatchMode`, `TestHandleRemoteObservationBatch_MountRootEnumerateClampRearmsRefreshTimerImmediately`, `TestCancelPendingTimers` |
