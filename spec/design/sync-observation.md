# Sync Observation

GOVERNS: internal/sync/observer_local.go, internal/sync/observer_local_handlers.go, internal/sync/observer_local_collisions.go, internal/sync/observer_remote.go, internal/sync/socketio.go, internal/sync/socketio_conn.go, internal/sync/socketio_protocol.go, internal/sync/item_converter.go, internal/sync/scanner.go, internal/sync/buffer.go, internal/sync/inotify_linux.go, internal/sync/inotify_other.go, internal/sync/symlink_observation.go, internal/sync/single_path.go

Implements: R-2.1.2 [verified], R-2.11 [verified], R-2.12 [verified], R-2.13.1 [verified], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.19 [verified], R-6.7.20 [verified], R-6.7.21 [verified], R-6.7.24 [verified], R-6.7.28 [verified], R-6.7.29 [verified]

## Overview

Observation turns local filesystem changes and Graph delta/enumeration results
into live observation facts. The low-level observers do not write the sync DB
directly. The engine-owned observation orchestration persists the resulting
observation batch as one coherent durable set. `observation_state` still owns
snapshot cadence and cursors; observation produces wake signals, dirty
path/scope hints, observation findings, and direct local-snapshot rows for
`local_state`.

In watch mode, that ownership is explicit: local observers emit only local
change hints, while remote observers and full-refresh workers emit one
`remoteObservationBatch` value that the watch loop applies durably. No remote
observer goroutine commits remote rows or observation cursors directly. Once the
engine starts applying that batch as durable current truth, failures in remote
row commits, cursor commits, or observation-findings reconciliation are
fail-closed for that run/session rather than best-effort warnings.

The observation stack has four main pieces:

- `RemoteObserver`: Graph delta/enumeration and wake-driven watch polling
- `LocalObserver`: fsnotify-driven watch observation
- `Scanner`: full local walk for one-shot bootstrap and rebuild cases
- `DirtyBuffer`: debounce/coalescing before snapshot refresh and replanning

## Ownership Contract

- Owns: local and remote change capture, path normalization, sparse-item recovery, skipped-item reporting, single-path observation, and dirty buffering
- Does Not Own: planning, retry policy, scope persistence, or user-facing issue interpretation
- Source of Truth: live filesystem state, Graph responses, and read-only baseline context supplied by the engine
- Allowed Side Effects: rooted filesystem reads, hashing, Graph observation calls, watch registration, and event emission
- Mutable Runtime Owner: `RemoteObserver`, `LocalObserver`, `Scanner`, and `DirtyBuffer` each own only their own invocation- or run-scoped mutable state, watches, and goroutines. The engine owns lifecycle and composition across those pieces.
- Error Boundary: observation surfaces filesystem, watch, and Graph observation failures as observation results and control signals; once the engine starts durably applying an observation batch, observation-finding reconciliation is fail-closed and execution paths do not invent observation-owned durable rows.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Whole-drive observation emits normalized observation facts and direct local snapshot rows for `local_state` without writing the sync DB directly. | `TestFullScan_NonexistentSyncRoot_ReturnsError`, `TestNosyncGuard_PreventsAllSync`, `TestResolveDebounce_DefaultIsFiveSeconds` |
| Normal drives ignore embedded shared-folder shortcut items instead of creating nested follow-up sync runtimes. | `internal/sync/observer_remote_test.go`, `internal/sync/remote_state_mirror_test.go` |
| Shared-root drives still support remote observation rooted at their configured shared root. | `internal/sync/engine_phase0_test.go` (`TestBootstrapSync_WithChanges`, `TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta`), `internal/sync/observer_remote_test.go` |

## Remote Observation

`RemoteObserver` supports:

- one-shot `FullDelta`
- watch-mode polling
- optional Socket.IO wakeups that tell the observer to poll sooner

Important properties:

- observation emits wake/dirty signals plus normalized remote facts
- zero-event delta responses do **not** advance the saved cursor
- HTTP 410 delta expiry is surfaced as a restart/re-enumerate condition
- sparse delta items recover omitted name/parent data from the baseline when possible
- deleted-item names may also be recovered from baseline context
- explicit remote read denial discovered during observation is emitted as an
  observation-owned finding (`remote_read_denied`) plus
  `perm:remote:read:<boundary>`
- observation-owned read-denial boundary facts are subtree boundaries:
  descendants become unavailable for planning and status through the tagged
  boundary issue rather than through synthetic per-descendant issue rows or
  execution-time permission retries

### Whole drives and shared-root drives

The product now has two observation shapes:

- drive-root observation for ordinary drives
- shared-root observation for separately configured shared-root drives

There is no nested shared-folder-following runtime inside another synced drive.
If a normal drive's delta stream contains an embedded shared-folder link item,
observation ignores it.

For shared-root drives, observation may use shared-root delta or recursive
enumeration depending on drive type and Graph support.

Remote read-denied boundaries are observation-owned facts. When root or shared-root
observation proves that remote truth is unreadable, the engine persists one
managed observation batch containing:

- `observation_issues` rows for `remote_read_denied`
- `ScopeKey` tags of `perm:remote:read:<boundary>` on those issue rows

Healthy observation batches reconcile that managed set away again. Worker `403`
results do not create these read-denied findings.

## Item Conversion

`item_converter.go` is the single item-to-observation normalization path.

It owns:

- NFC normalization
- drive/item identity handling
- path reconstruction from parent chains
- move detection against baseline
- malformed sparse-item rejection
- remote-root metadata propagation for configured shared-root drives

`ChangeEvent.TargetRootItemID` carries the configured remote root for shared-root
drives so later snapshot refresh, planning, and execution can derive the
correct target root.

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
The engine reconciles those findings through one `ObservationFindingsBatch`
boundary, and `observation_findings.go` is the single constructor path for
those batches. Whole-drive scans replace the managed family set for those
findings; single-path observation uses the same batch shape but manages only
the exact observed path set it proved.

Observation-owned read-denied subtree boundaries stay boundary-scoped. The
durable batch records one boundary issue tagged with the matching read
`ScopeKey`; descendants are made unavailable from that boundary fact during
planning instead of receiving synthetic per-descendant issue rows.

Observation-owned local read findings use these rules:

- unreadable file -> `observation_issue(path, local_read_denied)` only
- unreadable directory boundary ->
  `observation_issue(boundary, local_read_denied)` with boundary `ScopeKey`
  `perm:dir:read:<boundary>`
- invalid or unsyncable local content -> observation issue only

Single-path observation used by retry/trial flows follows the same ownership
rule. It may update `observation_issues` and their boundary `ScopeKey` tags
because it is still observation, not worker-result persistence. It must never
reconcile a one-path observation result as if it were a whole-drive managed
set; path-scoped reconciliation clears only the exact observed path set.

`Scanner.FullScan()` now also returns direct `LocalStateRow` values for every
admissible currently observed local path. `local_state` persistence therefore
comes from current disk truth, not by replaying local change events against
baseline.

Dry-run uses those same direct `LocalStateRow` values, but commits them only to
an isolated scratch store before SQLite comparison/reconciliation. Observation
findings discovered during preview also reconcile only into that scratch store.
The durable runtime store keeps its prior committed snapshots, observation
issues, and block scopes unchanged during preview.

## Dirty Buffer

`DirtyBuffer` owns short-lived replan scheduling only:

- debounce bursts of local or remote dirty signals
- wait 5 seconds from the last local or remote observation before triggering replanning by default
- keep one pending coarse dirty signal plus a full-refresh bit
- flush deterministically on shutdown/final drain

It is not a durable queue, not a semantic event journal, and not a second
source of truth.
