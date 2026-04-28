# Sync Observation

GOVERNS: internal/sync/observer_local.go, internal/sync/observer_local_handlers.go, internal/sync/observer_local_collisions.go, internal/sync/observer_remote.go, internal/sync/socketio.go, internal/sync/socketio_conn.go, internal/sync/socketio_protocol.go, internal/sync/item_converter.go, internal/sync/scanner.go, internal/sync/buffer.go, internal/sync/inotify_linux.go, internal/sync/inotify_other.go, internal/sync/symlink_observation.go, internal/sync/single_path.go

Implements: R-2.1.2 [verified], R-2.11 [verified], R-2.12 [verified], R-2.13.1 [verified], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.19 [verified], R-6.7.20 [verified], R-6.7.21 [verified], R-6.7.24 [verified], R-6.7.28 [verified], R-6.7.29 [verified]

## Overview

Observation turns local filesystem changes and Graph delta/enumeration results
into live observation facts. The low-level observers do not write the sync DB
directly. The engine-owned observation orchestration persists the resulting
observation batch as one coherent durable set. `observation_state` owns the
remote cursor plus remote full-refresh cadence; local watch safety-scan cadence
is rebuildable watch runtime state. Observation produces wake signals, dirty
path/scope hints,
observation findings, and direct local-snapshot rows for `local_state`.

In watch mode, that ownership is explicit: local observers emit only local
change hints, while remote observers and full-refresh workers emit one
`remoteObservationBatch` value that the watch loop applies durably. No remote
observer goroutine commits remote rows or observation cursors directly. Once the
engine starts applying that batch as durable current truth, failures in remote
row commits, cursor commits, or observation-findings reconciliation are
fail-closed for that run/session rather than best-effort warnings.
Those observer outputs are runtime-owned too: the watch runtime owns the local
event stream, remote-batch stream, skipped-item stream, observer error stream,
refresh channels, and active-observer count, while the pipeline shell keeps
only the non-runtime dependencies needed by the loop.

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
| Whole-drive observation emits normalized observation facts and direct local snapshot rows for `local_state` without writing the sync DB directly, including filesystem identity for files and directories when available. | `TestFullScan_NonexistentSyncRoot_ReturnsError`, `TestFullScan_StoresFilesystemIdentityForFilesAndDirectories`, `TestNosyncGuard_PreventsAllSync`, `TestResolveDebounce_DefaultIsFiveSeconds` |
| Normal drive content observation suppresses embedded shared-folder shortcut placeholders from content events, including Graph items whose local placeholder is not a folder but whose `remoteItem.folder` target is a folder. Parent drive engines convert those placeholders into parent-owned shortcut-root state before publishing child work commands to the control plane. | `TestFullDeltaWithShortcutTopology_EmitsShortcutFactsAndSuppressesContent`, `TestClassifyItem_EmbeddedSharedPlaceholdersIgnored`, `internal/sync/remote_state_mirror_test.go` |
| Mount-root runtimes still support remote observation rooted at their configured remote root. Separately configured shared folders and managed shortcut child mounts use this path when their content root is below the backing drive root. | `internal/sync/engine_phase0_test.go` (`TestBootstrapSync_WithChanges`, `TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta`), `internal/sync/observer_remote_test.go` |

## Remote Observation

`RemoteObserver` supports:

- one-shot `FullDelta`
- watch-mode polling
- optional Socket.IO wakeups that tell the observer to poll sooner

Important properties:

- observation emits wake/dirty signals plus normalized remote facts
- zero-event delta responses do **not** advance the saved cursor
- HTTP 410 delta expiry is surfaced as a restart/re-enumerate condition
- sparse delta items recover omitted name/parent data from baseline when
  possible, and missing parent IDs trigger one best-effort item-by-ID enrich
  before baseline fallback
- deleted-item names may also be recovered from baseline context
- explicit remote read denial discovered during observation is emitted as an
  observation-owned finding (`remote_read_denied`) plus
  `perm:remote:read:<boundary>`
- observation-owned read-denial boundary facts are subtree boundaries:
  descendants become unavailable for planning and status through the tagged
  boundary issue rather than through synthetic per-descendant issue rows or
  execution-time permission retries

### Drive-root and mount-root observation

The engine now has two remote observation shapes:

- drive-root observation for ordinary drives
- mount-root observation for runtimes scoped below the remote drive root

There is no nested shared-folder-following runtime inside the content engine. If
a normal drive's delta stream contains an embedded shared-folder link or
shortcut placeholder item, observation suppresses that item from ordinary
content events. The parent drive engine is still the only Graph observer for
that drive, so it converts shortcut placeholders into engine-internal raw
shortcut observation facts before committing remote observation progress. The parent
persists those facts in `shortcut_roots` and publishes parent-declared child
process commands; `internal/multisync` only starts, skips, final-drains, or
stops managed children as separate mount-root engines.

The engine applies topology batches with its internal `shouldApply` gate.
Any batch with facts applies, a complete batch applies even when it contains no
facts, and an empty incremental batch is a non-event. This lets a full parent
enumeration with no shortcut aliases retire old managed children while keeping
ordinary zero-event delta polls from advancing observation progress.

Separately configured shared folders and managed shortcut child mounts use the
mount-root path when their content root is below the backing drive root.
Mount-root observation may use folder delta or recursive enumeration depending
on drive type and Graph support.

Parent local observation rebuilds protected shortcut-root paths from the
parent sync store. A protected path records its boundary `local_state` identity
but suppresses descendants from normal create/move/delete planning. Shortcut
root rename/move/delete policy is then layered on top of the same generic
filesystem identity facts used for ordinary local move detection; a unique
identity match at a new path is a parent shortcut alias move, while ambiguous
matches become parent-owned recovery state. Parent alias mutations are executed
by the parent engine by binding item ID; multisync receives only the resulting
child work commands.

When remote shortcut topology moves or renames an existing binding inside the
same parent namespace, the parent keeps the previous protected path reserved
while it moves the local child projection to the new alias path. The child mount
ID and child content state stay bound to the shortcut binding item ID rather
than the alias path.

Remote read-denied boundaries are observation-owned facts. When drive-root or
mount-root observation proves that remote truth is unreadable, the engine
persists one managed observation batch containing:

- `observation_issues` rows for `remote_read_denied`
- `ScopeKey` tags of `perm:remote:read:<boundary>` on those issue rows

Healthy observation batches reconcile that managed set away again. Worker `403`
results do not create these read-denied findings.

## Item Conversion

`item_converter.go` is the single item-to-observation normalization path.

It owns:

- NFC normalization
- drive/item identity handling
- best-effort item-by-ID enrich for sparse delta items missing `ParentID`
  or known shortcut target fields
- path reconstruction from parent chains
- move detection against baseline
- malformed sparse-item rejection
- mount-root path materialization relative to the engine's configured root

Observation emits current truth for the mount root only. It does not carry a
separate per-event target-root field for later planning or execution.

Sparse parent recovery is intentionally layered:

- first use the delta item directly when it already carries `ParentID`
- otherwise try one best-effort `GetItem(driveID, itemID)` enrich before
  materializing the path
- if enrich still leaves the parent missing, fall back to baseline ancestry for
  path reconstruction when possible

Observation does not fail the batch when that enrich step misses or the direct
read returns an error. The enrich read exists only to reduce sparse-delta blind
spots before path materialization; durable ancestry still lives in `baseline`,
not in a second remote-observation store field.

Known managed shortcut aliases also use persisted protected-root target metadata
as a same-binding fallback when Graph returns a sparse moved/renamed shortcut
placeholder without `remoteItem` target fields. The parent engine still owns the
observation and mutation; this fallback only keeps the same shortcut binding
classified as parent-declared child work commands instead of treating an empty
placeholder delta as ordinary content or an unavailable new child.

## Local Observation

`LocalObserver` and `Scanner` observe the whole configured local root. There is
no user-configured bidirectional narrowing of the synced tree.

`LocalFilterConfig.SkipDirs` is interpreted as a list of root-relative slash
paths beneath the mount root. A skip entry excludes that exact subtree only; it
does not exclude every directory elsewhere with the same leaf name. Managed
shortcut child subtrees are not supplied by the control plane as skip dirs;
parent engines derive those protected roots from `shortcut_roots` and update
their own observer/filter state internally.

Full local scans persist `local_device`, `local_inode`, and
`local_has_identity` for files and directories on platforms that expose stable
device/inode identity. Unsupported platforms persist `local_has_identity=false`
and keep hash-based file move fallback behavior for files only.

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
