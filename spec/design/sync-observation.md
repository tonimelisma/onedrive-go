# Sync Observation

GOVERNS: internal/syncobserve/observer_local.go, internal/syncobserve/observer_local_handlers.go, internal/syncobserve/observer_local_collisions.go, internal/syncobserve/observer_remote.go, internal/syncobserve/socketio.go, internal/syncobserve/socketio_conn.go, internal/syncobserve/socketio_protocol.go, internal/syncobserve/item_converter.go, internal/syncobserve/scanner.go, internal/syncobserve/buffer.go, internal/syncobserve/inotify_linux.go, internal/syncobserve/inotify_other.go

Implements: R-2.1.2 [verified], R-2.4 [verified], R-2.8.5 [verified], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.15 [verified], R-6.7.16 [verified], R-6.7.19 [verified], R-6.7.20 [verified], R-6.7.21 [planned], R-6.7.24 [verified], R-6.7.26 [verified], R-6.7.28 [verified], R-6.7.29 [verified], R-2.11 [verified], R-2.11.5 [verified], R-2.12 [verified], R-2.13.1 [verified], R-6.3.4 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Ownership Contract

- Owns: Local and remote observation, change normalization, skipped-item production, event buffering, and observation-only safety filters.
- Does Not Own: Planning, sync-store writes, retry scheduling, or permission-scope lifecycle decisions.
- Source of Truth: Live filesystem events, full scans, Graph delta responses, and the baseline snapshot supplied by the engine for read-only comparison.
- Allowed Side Effects: Filesystem walking and hashing under `synctree`, Graph observation calls, watch registration, and buffered event emission.
- Mutable Runtime Owner: `LocalObserver` watch loops own local watcher state, timer maps, and hash-request queues. `RemoteObserver.Watch` owns remote polling state, including the current delta token. `Buffer` owns its pending-path map under `Buffer.mu`.
- Error Boundary: Observation converts local/remote read quirks into `ChangeEvent`, `SkippedItem`, and observation sentinels such as `ErrGone`. The engine decides persistence, retry, and user-facing consequences via [error-model.md](error-model.md).

## Verified By

| Behavior | Evidence |
| --- | --- |
| Remote watch observation owns retry/backoff and delta-expiry recovery instead of pushing that policy into the engine. | `TestWatch_BackoffOnError`, `TestWatch_DeltaExpiredResets` |
| The debounced buffer drains cleanly on shutdown/final flush without deadlocking watch ownership. | `TestBuffer_FlushDebounced_FinalDrainNoDeadlock` |

## Remote Observer (`observer_remote.go`)

Produces `[]ChangeEvent` from the Graph API. Two modes: `FullDelta` (one-shot) and `Watch` (continuous delta observation driven by Socket.IO wakeups when configured, with periodic polling retained as fallback).

Key properties:
- Output is `[]ChangeEvent` — never writes to the database
- Path materialization uses the baseline (read-only) + in-flight parent tracking
- Normalization (driveId casing, missing fields, timestamps) happens here
- Within each delta page, deletions are buffered and processed before creations (API reordering bug)
- HTTP 410 (expired delta token) returns `ErrGone` sentinel; engine restarts with full delta
- Unknown remote timestamps stay unknown. If Graph omits `lastModifiedDateTime` or sends an invalid/out-of-range value, observation carries zero `Mtime` instead of fabricating a local clock value.
- Sparse non-delete updates reuse baseline path context. When delta omits unchanged `name` or `parentReference`, conversion recovers the missing leaf or parent directory from the existing baseline entry so modifies and moves stay correctly rooted.
- Watch-mode websocket frames are wake signals only. `RemoteObserver` still owns the current delta token, zero-event token guard, observation commit ordering, and event emission. Websocket notifications never carry authoritative remote state and never advance durable sync state by themselves.
- `SocketIOWakeSource` owns endpoint fetch, RFC6455 connection lifecycle, minimal Engine.IO / Socket.IO framing, ping/pong replies, reconnect, and endpoint renewal. It reduces all remote notifications to a buffered wake signal consumed by `RemoteObserver.Watch`.
- `SocketIOWakeSource` also owns its websocket-lifecycle callback stream (`started`, endpoint fetch/connect failures, connected, refresh requested, connection dropped, notification wake, wake coalesced, stopped). Observation emits these as internal runtime diagnostics only; they are not durable sync state and not user-facing watch truth.
- **Progress logging**: `FullDelta` emits periodic Info-level progress logs every 30 seconds during enumeration, reporting pages fetched and events accumulated so far. For 100K+ item drives where enumeration can take minutes, this gives operators visibility between the start and completion logs. Time-based rather than page-based to produce evenly-spaced logs regardless of page size or API latency.

**Server-trusted observation**: The remote observer does NOT filter items by
name validity or always-excluded patterns. If OneDrive sends an item in a
delta response, it exists on OneDrive — filtering it would be silent data
loss. Name-based filtering is a local-only concern (upload validation). Only
root items and vault descendants are excluded at the raw remote-observation
boundary. Bidirectional sync scope (`sync_paths`, `ignore_marker`) is applied
after observation as an engine-owned scope policy, not by pretending the
server never reported the item.

**Malformed remote-item guard**: Remote observation still trusts server-sent names that are odd but valid on OneDrive. What it does not trust is missing identity or unmaterializable deletes. Non-root items with empty `id`, non-deleted items with empty `name`, and delete entries whose path cannot be recovered from the baseline or surviving delta name/parent data are warned and skipped instead of being emitted as empty-ID or empty-path `ChangeEvent`s. That keeps malformed sparse payloads from poisoning buffer/planner state while preserving legitimate deleted-item name recovery from either the baseline or the current delta payload.

### Item Conversion Pipeline (`item_converter.go`)

The `itemConverter` struct is the single code path for converting `[]graph.Item` into `[]ChangeEvent`. Both the primary drive observer (`RemoteObserver.fetchPage`) and shortcut observation (`convertShortcutItems`) delegate to it with different configuration:

- **Primary drive** (`newPrimaryConverter`): vault exclusion enabled, shortcut detection enabled, no path prefix
- **Shortcut scope** (`newShortcutConverter`): path prefix set to shortcut's local path, scope root ID set (items with this ID are skipped), nested shortcut skip enabled, `shortcutDriveID`/`shortcutItemID` set from Shortcut fields — propagated to all content ChangeEvents as `RemoteDriveID`/`RemoteItemID` for downstream scope identification (D-5)

Key properties:
- Two-pass processing: register all items in inflight map, then classify (handles child-before-parent ordering)
- NFC normalization applied to all item names
- Move detection via baseline comparison (existing item at different path → ChangeMove)
- Deleted-item name recovery from baseline (Business API items may lack Name on delete)
- Malformed sparse items with empty identity are skipped before event emission
- Sparse remote timestamps are normalized at the graph boundary; item conversion preserves zero/unknown timestamps instead of synthesizing current time
- Sparse non-delete items recover omitted `name` / `parentReference` from the baseline when possible, instead of treating partial delta payloads as malformed
- The inflight parent map also recovers sparse parent metadata from the baseline, so descendants in the same delta batch keep the correct parent leaf and ancestor chain even when the parent item omits unchanged `name` or `parentReference`
- Orphan items (missing parent) produce a warning log and partial path
- The inflight map is a parameter, not a field — RemoteObserver accumulates it across pages; shortcuts populate it once per batch

### Shortcut Observation (`item_converter.go`)

Shared folder shortcuts require separate delta scopes. Personal drives use delta (shortcuts appear in the delta stream). Business drives use folder enumeration (no shortcut support in Business delta). Observation method recorded per-shortcut for correct dispatch. Shortcut observation uses the unified `itemConverter` pipeline via `convertShortcutItems`, ensuring identical NFC normalization, move detection, and deleted-item name recovery as the primary drive.

Shortcut helper functions are co-located with their primary concern: `detectShortcutOrphans` and `resolveItemDriveIDWithFallback` live in `item_converter.go` (DriveID resolution and baseline comparison); `filterOutShortcuts` and concurrency scaffolding live in `engine_shortcuts.go` (governed by [sync-engine.md](sync-engine.md), not this doc — engine orchestration, not observation). The former `observer_shortcut.go` was dissolved — its functions were redistributed to eliminate a vestigial single-purpose file.

## Local Observer (`observer_local.go`)

Produces `[]ChangeEvent` from the filesystem. Two modes: `FullScan` (one-shot walk) and `Watch` (inotify/FSEvents via `fsnotify/fsnotify`).

Key properties:
- Output is `[]ChangeEvent` — never writes to the database
- Local events keyed by path (no ItemID — that's a remote concept)
- Compares against in-memory baseline, not DB queries
- The scanner/watch path boundary is a rooted `internal/synctree.Root`, so the
  observer establishes the sync root once and then operates on rooted paths
  instead of rebuilding arbitrary local paths at each call site
- `synctree.Root` stores unexported injectable ops on the root value so local
  observation can test `Stat`, `ReadDir`, and tree-walk failure paths
  deterministically without exposing new production APIs
- Local-vs-baseline mtime comparisons truncate both sides to whole-second UTC to match OneDrive precision, but the racily-clean guard still uses the raw observed local mtime against scan start so recently-written files are always re-hashed
- Dual-path threading: `fsRelPath` (filesystem I/O) and `dbRelPath` (NFC-normalized for baseline lookup). `handleDelete` receives `fsPath` (the original `fsEvent.Name`) directly and passes it to `watcher.Remove()` — never reconstructs the filesystem path from NFC-normalized `dbRelPath` (B-312). On macOS HFS+ where fsnotify delivers NFD-encoded paths, reconstructing with NFC causes `watcher.Remove()` to silently fail and leak watch resources.
- `skip_symlinks = false` follows symlink targets at the alias path for full scans, watch-mode create/write handling, retry/trial single-path reconstruction, and watch setup. `skip_symlinks = true` excludes them entirely. Followed directory symlinks use a real-path ancestry guard so cycles stop at the alias boundary instead of recursing forever. Excluded symlink aliases stay silent: watch mode remembers skipped alias paths so later remove events and safety scans do not resurrect them as synthetic deletes.
- inotify watch limit detection on Linux (`inotify_linux.go`)
- The local observer also owns one effective sync-scope snapshot separate from
  `LocalFilterConfig`. The snapshot is built from normalized `sync_paths` plus
  locally discovered marker directories, and it gates full scans, watch setup,
  watch event admission, and retry/trial single-path reconstruction.
- Marker files are presence-only and never actionable. The observer watches
  the marker-bearing directory itself so marker deletion is visible, but it
  suppresses descendant observation while the marker is present. Marker
  create/delete/rename and parent-directory move candidates rebuild the
  snapshot, advance a watch-owned local scope generation, invalidate deferred
  hash work from older generations, adjust descendant watches, and publish
  one scope-change transition back to the engine.
- Raw fsnotify behavior for those marker and path-boundary cases is captured
  with the repo-owned diagnostic `go run ./cmd/devtool watch-capture`.
  Captured per-OS traces live under
  `internal/syncobserve/testdata/watch_capture/<goos>/` and are replayed
  through `LocalObserver.HandleFsEvent` in same-package tests so the watcher
  contract is grounded in real event order rather than mock assumptions. See
  [../reference/fsnotify-watch-capture.md](../reference/fsnotify-watch-capture.md).

### Scanner (`scanner.go`)

Extracted filesystem walker for full-scan mode. Produces change events by walking the sync directory and comparing against baseline.

**ScanResult Return Type** — `FullScan` returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. `SkippedItem` struct: `{Path string, Reason string, Detail string}`. Invalid files become `SkippedItem` entries instead of silent DEBUG logs. Scanner remains a pure observer with no DB dependency — it reports what it found; the engine decides what to record. The walk itself is rooted through `synctree.Root.WalkDir`, so directory traversal and relative-path reconstruction stay under one sync-root capability. Implements: R-2.11.5 [verified]

**Hash Phase Panic Recovery** — Each hash goroutine in `hashPhase` has a `defer/recover` that converts panics to `SkippedItem{Reason: IssueHashPanic}` entries. A single corrupted file or unexpected library error cannot crash the entire scan. The hash function is injectable (`LocalObserver.hashFunc`) for testing. Skipped items from panics are merged into `ScanResult.Skipped` alongside walk-phase skips. Implements: R-6.7.5 [verified]

**`shouldObserve()` — Unified Local Observation Filter** — Single entry point replacing scattered `isAlwaysExcluded()` + `isValidOneDriveName()` calls across scanner, watch handlers, and watch setup. Returns `*SkippedItem`: nil means observe, non-nil means skip. When `Reason` is empty, it's an internal exclusion (always-excluded suffixes or user-configured local filters such as `skip_dotfiles`, `skip_dirs`, `skip_files`, and `skip_symlinks`); when `Reason` is non-empty, the caller records it as a user-visible `SkippedItem`. Used by `FullScan`, `processEntry`, watch event handlers (`handleCreate`, `handleWrite`, `handleRename`), and watch setup (`setupWatches`). `LocalObserver.SetFilterConfig()` installs the resolved `synctypes.LocalFilterConfig` so one-shot scans and watch mode share the same filter semantics, while `LocalObserver.SetObservationRules()` installs platform-derived validation rules such as SharePoint's root-level `forms` restriction. Internal exclusions stay silent even when the baseline still contains the path: deletion detection suppresses synthetic deletes for currently excluded paths rather than treating exclusion as disappearance. Prefix exclusions remain narrowly scoped to editor backup files (`~foo`, `.~foo`) so OneDrive-reserved `~$...` names still surface as actionable invalid filenames instead of being swallowed as silent temp files.

**Two-Stage Filter** — Validation is split into two stages based on when information is available:
- **Stage 1 (before stat):** Name/path checks (always-excluded suffixes, `skip_dotfiles`, parent-directory `skip_dirs`, invalid OneDrive names via `validateOneDriveName()`) and path length check (> 400 chars). Runs in `shouldObserve()` before any syscall-heavy work. Avoids unnecessary `Lstat` calls on files that will be skipped anyway.
- **Stage 1.5 (when kind is known):** Root directory-name `skip_dirs`, file-glob `skip_files`, and symlink-target kind checks run as soon as the observer knows whether the candidate is a file or directory. Full scans and watch setup know that from `DirEntry` plus symlink target resolution; watch create/write and retry reconstruction apply the same rules immediately after `Stat`.
- **Stage 2 (after stat):** File size check (> 250 GB). Runs in `processEntry` (full scan), `handleCreate`/`hashAndEmit`/`scanNewDirectory` (watch mode). Requires `Lstat` result to know the file size.

**`validateOneDriveName()`** — Returns `(reason string, detail string)` for names that violate OneDrive naming restrictions. Called directly at all call sites — no convenience wrapper. The `reason` field maps to issue types (e.g., `"invalid_filename"`, `"reserved_name"`). The `detail` field provides a human-readable explanation (e.g., `"contains ':' (invalid character)"`).

**`SkippedItem`** — Defined in `types.go`: `{Path string, Reason string, Detail string, FileSize int64}`. Represents a file that was observed but excluded from the event stream due to validation failure. `FileSize` is populated for `IssueFileTooLarge` (after stat). Collected during full scan and returned in `ScanResult.Skipped`. The engine processes these via `recordSkippedItems()` and `clearResolvedSkippedItems()`.

**Shared local metadata fast path** — `CanReuseBaselineHash(info, base, observeStartNano)` centralizes the scanner's mtime+size fast path plus the 1-second racily-clean guard. The equality check truncates both local and baseline mtimes to whole-second UTC before comparing, because Graph/OneDrive do not preserve fractional seconds. The scanner uses the helper during full scans, and the sync engine reuses the same helper when retry/trial planner work reconstructs upload-side local observation. This keeps "unchanged local file" detection consistent across full scans, retrier sweeps, and trial redispatch without forcing unnecessary re-hash work after a lossless OneDrive round-trip.

**Single-path local reconstruction** — `ObserveSinglePathWithFilter()` rebuilds one local path from current truth for engine-owned retry/trial work. It takes a rooted `synctree.Root` instead of a raw sync-root string and applies the same per-path rules as normal observation: configured local filters, `ShouldObserve`, oversized-file rejection, baseline-hash reuse, and “emit with empty hash” behavior when hashing fails. Internal exclusions resolve the retry/trial candidate silently; actionable validation failures become `SkippedItem`s that the engine records into `sync_failures`. `ObserveSinglePath()` is the zero-filter convenience wrapper used by tests and call sites that do not need resolved config.

**Case Collision Detection** (`detectCaseCollisions`) — Post-walk pure function that detects local files whose names differ only in case. OneDrive uses a case-insensitive namespace — uploading both would cause one to silently overwrite the other. Implements: R-2.12 [verified]

- Runs as Phase 2.5 in `FullScan`, between hash completion and deletion detection. Groups event indices by `(directory, lowercase name)`. Keys with more than one entry are collisions — all colliders become `SkippedItem{Reason: IssueCaseCollision}` with `Detail` naming the other collider(s). O(n) time, O(n) memory.
- **Baseline cross-check**: `detectCaseCollisions` accepts a `*Baseline` parameter and checks single-event groups against the baseline's `byDirLower` index. A new local file that differs only in case from an already-synced file is a collision — even though there's only one local file, uploading it would overwrite the existing remote file. The `Detail` field names the baseline peer.
- **Directory child suppression**: When a directory is flagged as a case collision, all events whose path falls under that directory are also suppressed. Children of a colliding directory cannot be synced because the parent directory itself is unresolvable.
- Colliders stay in the `observed` map (populated in Phase 1). Only their `ChangeEvent` entries are removed. This prevents Phase 3 (`detectDeletions`) from generating spurious `ChangeDelete` events for files that exist locally but were excluded due to collision.
- **Watch mode**: `hasCaseCollisionCached()` in `handleCreate` and `hashAndEmit` (`observer_local_collisions.go`) performs early rejection using a per-directory name cache AND a baseline cross-check. The cache is built lazily on first access using `os.ReadDir` on the observed directory path, pre-populated by `scanNewDirectory` (avoiding redundant directory reads), invalidated on Create/Delete events, and cleared on each safety scan. The collision check applies to both files AND directories (moved before the file/directory branch in `handleCreate`; added for subdirectory entries in `scanNewDirectory`). A `recentLocalDeletes` set suppresses false-positive baseline collisions during case-only renames.
- **Collision peer tracking**: A `collisionPeers` map tracks N-way collision relationships (each path maps to a set of peer paths). When a collider is deleted (`handleDelete`), all surviving peers are re-emitted via `handleCreate` — each re-checks `hasCaseCollisionCached` and re-records any remaining collisions. Helper methods `addCollisionPeer` and `removeCollisionPeersFor` in `observer_local_collisions.go` manage the bidirectional sets.
- **SkippedItem forwarding**: A `skippedCh` channel forwards `SkippedItem` entries from safety scans to the engine for recording in `sync_failures`. The engine calls `clearResolvedSkippedItems()` to auto-clear items that are no longer flagged, ensuring collision resolution is reflected promptly.
- **`hashAndEmit` collision check**: Write events in watch mode are checked for case collisions via `hasCaseCollisionCached`, not just Create events. A write to a file that collides with another name in the same directory is suppressed.
- **Auto-clear**: `clearResolvedSkippedItems()` includes `IssueCaseCollision` in its scanner-detectable issue types. When the user renames one collider, the next `FullScan` won't flag either file, and both `sync_failures` entries are cleared.

## Change Buffer (`buffer.go`)

Collects events from both observers, deduplicates, debounces (default 2 seconds), and produces `[]PathChanges` batches grouped by path. Thread-safe (mutex-protected). Move events are dual-keyed: stored at new path AND synthetic delete at old path.

`FlushImmediate` for one-shot mode (no debounce wait).

## Runtime Ownership

Observation has a few explicit mutable-runtime owners:

- `Buffer.mu` guards exactly `pending` and `notify`. The debounce goroutine owns the lifetime of the output channel created by `FlushDebounced`.
- `LocalObserver.localWatchState` is the single mutable owner for one local
  watch run: pending timers, generation-tagged deferred hash work, watch
  registrations, collision caches, and the effective scope snapshot all live
  together there. Timer callbacks feed back into that observer-owned watch
  loop; they do not mutate engine state directly.
- Deferred hash work is generation-tagged. A marker-driven scope change does
  not need to race every queued timer callback individually; stale callbacks
  self-drop when their captured generation no longer matches the current
  watch-owned scope generation.
- `RemoteObserver` owns `deltaToken` for one watch run. The watch loop is the only writer; helper calls may read it concurrently through `CurrentDeltaToken`.
- `SocketIOWakeSource` owns exactly one outbound websocket session plus its reconnect/refresh timers for one watch run. It does not own any delta token or durable sync state.
- `SocketIOWakeSource` constructor options own the test seams for dial/sleep/clock/lifecycle hook injection. Tests do not mutate the wake source after construction.
- The engine owns the outer observer goroutines and cancellation context. Observation code never starts detached background work.

## Permission Interaction

Remote shared-folder permission handling lives in the sync engine (`permission_handler.go`; see [sync-engine.md](sync-engine.md)), not in `syncobserve`.

The observation layer only intersects with permission handling in two places:

- the local scanner can prove a previously inaccessible local path is accessible again, which lets the engine auto-clear `perm:dir` failures
- the shared `CanReuseBaselineHash` helper keeps upload retry/trial reconstruction logic consistent with normal local observation
- `ObserveSinglePath` keeps retry/trial reconstruction aligned with current local validation and hash-failure semantics instead of maintaining a separate engine-local observer variant

Remote 403 detection itself is engine-owned: the engine confirms denials with
`ListItemPermissions`, records one persisted `perm:remote:{localPath}`
boundary, and switches that subtree to download-only mode through the planner
and the watch loop's active-scope working set. There is no separate in-memory
permission cache in the observation layer.

## Design Constraints

Implements: R-6.2.7 [verified]

- Filtering is asymmetric by design: the local observer filters by always-excluded patterns, user-configured `skip_dotfiles` / `skip_dirs` / `skip_files` / `skip_symlinks`, and OneDrive naming rules (upload validation), while the remote observer trusts the server. If OneDrive sends an item in a delta response, it exists remotely — filtering it would cause silent data loss. Remote-only temp files (e.g., uploaded via web UI) are intentionally allowed through to the planner.
- `ignore_marker` and `sync_paths` are not part of that asymmetric local-only
  filter set. They define bidirectional sync scope: local observation uses
  them to decide what stays observable, while the engine/store use the same
  effective snapshot to filter remote work and to reactivate previously
  filtered remote rows when scope re-enters.
- Symlink following is a local-observation semantic choice, not a remote storage primitive. When `skip_symlinks = false`, the observer dereferences the local symlink and emits events at the alias path. Execution still uses contained local write paths for downloads and durable state paths; only upload-side local reads intentionally follow the user’s symlink target.
- The always-excluded suffix checks in `isAlwaysExcluded()` do NOT include `.db`/`.db-wal`/`.db-shm` — those caused false positives on legitimate data files. The sync engine's state DB lives outside the sync root by design.
- Delta processing uses two-pass page handling to ensure vault parent folders are classified before their children, preventing path materialization failures.
- Personal Vault items (`specialFolder.name == "vault"`) are unconditionally excluded. Vault auto-locks after 20 minutes → locked items appear deleted in delta → phantom deletions.
- FullScan uses four sequential phases: (1) Walk (readdir + Lstat + classify), (2) Hash (parallel QuickXorHash via `errgroup.SetLimit(checkWorkers)`), (2.5) Case collision detection (`detectCaseCollisions` — colliders removed from events, kept in observed map), (3) Deletion detection (compare observed paths vs baseline).

### Rationale

`fsnotify/fsnotify` (v1.9.0, 10.6k stars, used by Hugo/Docker/Kubernetes/Syncthing) was chosen over `rjeczalik/notify` (unmaintained since Jan 2023).

### Planned Improvements

- Debounce semantics under load: write coalescing implemented, load-testing and tuning remaining. [planned]
- Streaming delta processing: process pages as they arrive rather than buffering all pages. Modest win (~1ms per page vs ~100-300ms API call). [planned]
- Per-path event cap in Buffer: `defaultBufferMaxPaths` caps path count but not events-per-path. [planned]
- Nil guards on all Item field accesses in `observer_remote.go`: delta items may have missing `Name`, `ParentReference`, etc. [planned]
- Buffer overflow test with drop metric verification. [planned]
- inotify partial-watch cleanup verification: ensure already-added watches are cleaned up on setup failure. [planned]
