# Sync Shortcut

GOVERNS: internal/sync/protected_roots.go, internal/sync/shortcut_alias_mutation_types.go, internal/sync/shortcut_protected_root_types.go, internal/sync/shortcut_root_planner.go, internal/sync/shortcut_root_planner_ack.go, internal/sync/shortcut_root_planner_cleanup.go, internal/sync/shortcut_root_planner_local.go, internal/sync/shortcut_root_planner_topology.go, internal/sync/shortcut_root_state.go, internal/sync/shortcut_root_status.go, internal/sync/shortcut_root_status_read.go, internal/sync/shortcut_root_store.go, internal/sync/shortcut_root_transition.go, internal/sync/shortcut_topology.go

Implements: R-2.10 [designed], R-2.14 [designed]

## Overview

This family owns the parent-side shortcut subsystem: the lifecycle state
machine for a shortcut root, its transition table, the deterministic planners
that choose transitions from observed facts, the `shortcut_roots` persistence,
the protected-root vocabulary, and the read-only status projection.

It is the functional core plus its own durable state. The imperative shell --
the `*Engine` methods that call Graph, mutate the filesystem, and publish child
work to the control plane -- stays with [sync-engine.md](sync-engine.md).
`sync-engine.md` already stated that division ("shortcut-root lifecycle
decisions are expressed as deterministic planner helpers before the engine
shell performs I/O"); this family is that sentence made checkable.

## Ownership Contract

- Owns: shortcut-root lifecycle state, its validated transitions, the
  deterministic planners that pick them, `shortcut_roots` rows, protected-root
  identity and path reservations, and the status view the CLI renders.
- Does Not Own: Graph alias mutation, filesystem projection, child runner
  lifecycle, or multi-mount orchestration.
- Source of Truth: the `shortcut_roots` table in the parent mount's sync store.
- Allowed Side Effects: SQLite reads and writes against its own table, through
  the store's transaction helpers.
- Mutable Runtime Owner: none. The engine shell owns the runtime; this family
  owns values and rows.
- Error Boundary: transition validation rejects illegal lifecycle edges by
  returning an error naming the state and event, rather than silently
  normalizing.

## Why It Is Its Own Family

The subsystem is 3,672 lines across thirteen files and is structurally a
miniature of the whole sync architecture: a deterministic planner, its own
persistence, a thirteen-state machine with a validated transition table, a
status projection, and an effectful shell. It also accounts for a large share
of what other packages consume from `internal/sync`.

Before this split it was spread across the store and engine families, which
produced two edges that looked structural and were not: the store appeared to
depend on the engine (sixteen symbols, all shortcut) and observation appeared
to as well (fourteen, likewise). Naming the family removed both.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The lifecycle transition table covers every state and event pair, and rejects edges outside it. | `TestShortcutRootTransitionTableCoversStates`, `TestShortcutRootTransitionMatrixEnumeratesEveryStateAndEvent`, `TestValidateShortcutRootTransitionAllowsKnownLifecycleEdges`, `TestValidateShortcutRootTransitionRejectsIllegalLifecycleEdges` |
| Parent shortcut-root state is persisted and rebuilt into parent-owned observation protection. | `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots` |
| Status leaves the family as a display-ready view rather than raw policy columns. | `TestBuildChildStatusMount_RendersLifecycleState`, `TestShortcutRootStatusMetadataCoversNonActiveStates` |
