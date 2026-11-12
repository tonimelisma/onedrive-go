# Sync Domain

GOVERNS: internal/sync/actions.go, internal/sync/condition_keys.go, internal/sync/content_filter.go, internal/sync/core_types.go, internal/sync/crash_points.go, internal/sync/domain_clock.go, internal/sync/domain_delete_safety.go, internal/sync/domain_graph_error.go, internal/sync/domain_io_seams.go, internal/sync/domain_observation_mode.go, internal/sync/domain_path_kind.go, internal/sync/domain_runtime_decision.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/issue_types.go, internal/sync/permission_capability.go, internal/sync/retry_work_key.go, internal/sync/scope_block.go, internal/sync/scope_key.go, internal/sync/scope_semantics.go, internal/sync/tracked_action.go, internal/sync/types.go, internal/sync/worker_result.go

LAYER: 0

Implements: R-2.1 [verified], R-2.7 [verified]

## Overview

This family owns the sync domain model: the vocabulary every other sync family
speaks. `Action` and its action types, `ScopeKey` and the scope taxonomy,
`ItemType`, `Baseline` and `BaselineEntry`, `RetryWorkKey`, the condition and
issue keys, `SyncMode`, the content filter, the runtime decision vocabulary,
and the engine clock capability.

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
the planner, and the runtime. `actionType` answers `Direction()` and `Value()`.
`permissionCapability` answers `IsLocal()`. Moving these apart from their data
is what would produce an anemic model.

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
permission boundary scope (`scopeKeyForPermissionEvidence`) and the
block-scope activation wording (`blockScopeActivationWording`). None of them
carries a `//nolint:exhaustive`; until 2026-11-11 the last three did, remote
truth was two equality tests and the condition family a lookup table, so a
member they did not name fell through to "not blocking", "no family", the
one-second curve or a warning. Each switch has a test that reads the members
from `issue_types.go` and states every member's verdict, and each states a
verdict for a value outside the enum, which only a programming error can
make: unavailable truth on both sides of its path,
`ConditionUnexpectedCondition` (under any scope, rather than the scope's own
condition), the durable curve, no boundary scope, the generic line.

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
| Action types answer their own direction; an action type no case lists has none (the zero direction), never a guessed one. | `TestActionTypeDirection` |
| Every declared `IssueType` constant is a member and parses back to itself, with no two sharing a value; any other stored string, the empty one included, does not parse. | `TestIssueType_EveryDeclaredConstantIsAMemberAndParses`, `TestParseIssueType_RefusesAValueNoMemberHas` |
| Every issue type, read from source, has a stated condition family or none (the transient runtime members, whose retried results then carry no `condition_key` rather than the unexpected one); a value outside the enum is the unexpected condition under any scope, never "no family"; every declared condition key has a display rank of its own. The engine's per-issue-type decisions are held the same way (sync-engine.md). | `TestConditionKeyForIssueType_EveryMemberIsDecided`, `TestConditionKeyForStoredCondition_ValueOutsideTheEnumIsUnexpectedUnderAnyScope`, `TestRuntimeConditionKey_TransientConditionsHaveNoFamily`, `TestConditionKeyRank_EveryDeclaredKeyHasItsOwnRank`, `TestConditionClearsAutomatically_EveryKeyIsDecided` |
| `Baseline.GetCaseVariants` returns a copy of its sibling index under the read lock, so the watch observer can iterate it while workers commit outcomes through `Put` (R-6.3.4). | `TestBaseline_GetCaseVariantsCopyIsSafeAgainstConcurrentPut` |
| The bundled junk list (`isBundledJunkName`, on by default through config, R-2.4.14) covers OS metadata, editor/IDE temporaries, Office `~$` owner files and SQLite `-wal`/`-shm`/`-journal` sidecars of databases whose name has an extension, so ordinary names such as `travel-journal` keep syncing. The `4913`, `~*` and `*~` rules match only a file leaf (`isBundledJunkFileName`), never a folder or a path component. | `TestContentFilter_ExtendedJunkList`, `TestContentFilter_FileOnlyJunkRulesKeepFolders` |
| The runtime arms its timers through the engine clock, so a test clock controls retry, trial, and refresh timing coherently. | `TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionArmsRetryTimerInWatchMode`, `TestHandleRemoteObservationBatch_MountRootEnumerateClampRearmsRefreshTimerImmediately`, `TestCancelPendingTimers` |
