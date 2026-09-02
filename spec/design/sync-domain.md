# Sync Domain

GOVERNS: internal/sync/actions.go, internal/sync/condition_keys.go, internal/sync/content_filter.go, internal/sync/core_types.go, internal/sync/domain_clock.go, internal/sync/domain_graph_error.go, internal/sync/domain_io_seams.go, internal/sync/domain_observation_mode.go, internal/sync/domain_path_kind.go, internal/sync/domain_runtime_decision.go, internal/sync/enums.go, internal/sync/errors.go, internal/sync/issue_types.go, internal/sync/permission_capability.go, internal/sync/retry_work_key.go, internal/sync/scope_block.go, internal/sync/scope_key.go, internal/sync/scope_semantics.go, internal/sync/tracked_action.go, internal/sync/types.go, internal/sync/worker_result.go

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
| Action types answer their own direction. | `TestActionTypeDirection` |
| The runtime arms its timers through the engine clock, so a test clock controls retry, trial, and refresh timing coherently. | `TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionArmsRetryTimerInWatchMode`, `TestHandleRemoteObservationBatch_MountRootEnumerateClampRearmsRefreshTimerImmediately`, `TestCancelPendingTimers` |
