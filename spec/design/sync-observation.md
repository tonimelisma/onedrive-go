# Sync Observation

GOVERNS: internal/sync/observer_local.go, internal/sync/observer_local_handlers.go, internal/sync/observer_local_collisions.go, internal/sync/observer_remote.go, internal/sync/socketio.go, internal/sync/socketio_conn.go, internal/sync/socketio_protocol.go, internal/sync/item_converter.go, internal/sync/scanner.go, internal/sync/buffer.go, internal/sync/inotify_linux.go, internal/sync/inotify_other.go, internal/sync/symlink_observation.go

Implements: R-2.1.2 [verified], R-2.11 [verified], R-2.12 [verified], R-2.13.1 [verified], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.19 [verified], R-6.7.20 [verified], R-6.7.21 [verified], R-6.7.24 [verified], R-6.7.28 [verified], R-6.7.29 [verified]

## Overview

Observation turns local filesystem changes and Graph delta/enumeration results
into normalized `ChangeEvent` values. It never writes the sync DB directly.
The engine owns snapshot persistence and persisted full-refresh cadence in
`observation_state`; observation only produces live facts and wake signals.

The observation stack has four main pieces:

- `RemoteObserver`: Graph delta/enumeration and wake-driven watch polling
- `LocalObserver`: fsnotify-driven watch observation
- `Scanner`: full local walk for one-shot bootstrap and rebuild cases
- `Buffer`: debounce/coalescing before planning

## Ownership Contract

- Owns: local and remote change capture, path normalization, sparse-item recovery, skipped-item reporting, and event buffering
- Does Not Own: planning, retry policy, scope persistence, or user-facing issue interpretation
- Source of Truth: live filesystem state, Graph responses, and read-only baseline context supplied by the engine
- Allowed Side Effects: rooted filesystem reads, hashing, Graph observation calls, watch registration, and event emission
- Mutable Runtime Owner: `RemoteObserver`, `LocalObserver`, `Scanner`, and `Buffer` each own only their own invocation- or run-scoped mutable state, watches, and goroutines. The engine owns lifecycle and composition across those pieces.
- Error Boundary: observation surfaces filesystem, watch, and Graph observation failures as observation results and control signals; it does not classify retries, user-facing issues, or durable failure state.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Whole-drive observation emits normalized change events without writing the sync DB directly. | `TestBuffer_WatchAndSafetyScanConflictingTypes`, `TestFullScan_NonexistentSyncRoot_ReturnsError`, `TestNosyncGuard_PreventsAllSync` |
| Normal drives ignore embedded shared-folder shortcut items instead of creating nested follow-up sync runtimes. | `internal/sync/observer_remote_test.go`, `internal/sync/remote_state_mirror_test.go` |
| Shared-root drives still support remote observation rooted at their configured shared root. | `internal/sync/engine_phase0_test.go` (`TestBootstrapSync_WithChanges`, `TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta`), `internal/sync/observer_remote_test.go` |

## Remote Observation

`RemoteObserver` supports:

- one-shot `FullDelta`
- watch-mode polling
- optional Socket.IO wakeups that tell the observer to poll sooner

Important properties:

- observation emits `ChangeEvent`s only
- zero-event delta responses do **not** advance the saved cursor
- HTTP 410 delta expiry is surfaced as a restart/re-enumerate condition
- sparse delta items recover omitted name/parent data from the baseline when possible
- deleted-item names may also be recovered from baseline context

### Whole drives and shared-root drives

The product now has two observation shapes:

- drive-root observation for ordinary drives
- shared-root observation for separately configured shared-root drives

There is no nested shared-folder-following runtime inside another synced drive.
If a normal drive's delta stream contains an embedded shared-folder link item,
observation ignores it.

For shared-root drives, observation may use shared-root delta or recursive
enumeration depending on drive type and Graph support.

## Item Conversion

`item_converter.go` is the single item-to-event normalization path.

It owns:

- NFC normalization
- drive/item identity handling
- path reconstruction from parent chains
- move detection against baseline
- malformed sparse-item rejection
- remote-root metadata propagation for configured shared-root drives

`ChangeEvent.TargetRootItemID` carries the configured remote root for shared-root
drives so later planning and execution can derive the correct target root.

## Local Observation

`LocalObserver` and `Scanner` observe the whole configured local root. There is
no user-configured bidirectional narrowing of the synced tree.

Built-in local observation policy remains:

- validate OneDrive-invalid names before they become upload work
- symmetrically ignore junk/temp names before they enter either snapshot surface
- exclude Personal Vault content
- support symlink following at the alias path with cycle protection
- keep scanner/watch behavior aligned for hashing and case-collision detection

Ignore invariants:

- ignore policy is symmetric across local and remote observation
- ignored items never enter `local_state` or `remote_state`
- if a path is absent from both snapshots, it is just absent from current truth
- later reconciliation removes baseline rows that are absent from both snapshots

Observation may emit `SkippedItem`s for invalid or unsupported local content.
The engine decides how those become durable actionable issues.

## Buffer

`Buffer` owns short-lived event coalescing only:

- debounce bursts of path changes
- wait 5 seconds from the last local or remote observation before triggering replanning by default
- keep one pending entry per path
- flush deterministically on shutdown/final drain

It is not a durable queue and not a second source of truth.
