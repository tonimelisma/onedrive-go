# Sync Observation

GOVERNS: internal/sync/observer_local.go, internal/sync/observer_local_handlers.go, internal/sync/observer_remote.go, internal/sync/observer_shortcut.go, internal/sync/scanner.go, internal/sync/buffer.go, internal/sync/shortcuts.go, internal/sync/permissions.go, internal/sync/inotify_linux.go, internal/sync/inotify_other.go

Implements: R-2.1.2 [verified], R-2.4 [implemented], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.15 [planned], R-6.7.16 [planned], R-6.7.19 [planned], R-6.7.20 [verified], R-6.7.21 [planned], R-6.7.24 [verified], R-2.11 [planned], R-2.12 [planned], R-2.13.1 [verified], R-2.14.1 [verified]

## Remote Observer (`observer_remote.go`)

Produces `[]ChangeEvent` from the Graph API. Two modes: `FullDelta` (one-shot) and `Watch` (continuous polling).

Key properties:
- Output is `[]ChangeEvent` — never writes to the database
- Path materialization uses the baseline (read-only) + in-flight parent tracking
- Normalization (driveId casing, missing fields, timestamps) happens here
- Within each delta page, deletions are buffered and processed before creations (API reordering bug)
- HTTP 410 (expired delta token) returns `ErrGone` sentinel; engine restarts with full delta

### Shortcut Observation (`observer_shortcut.go`)

Shared folder shortcuts require separate delta scopes. Personal drives use delta (shortcuts appear in the delta stream). Business drives use folder enumeration (no shortcut support in Business delta). Observation method recorded per-shortcut for correct dispatch.

## Local Observer (`observer_local.go`)

Produces `[]ChangeEvent` from the filesystem. Two modes: `FullScan` (one-shot walk) and `Watch` (inotify/FSEvents via `fsnotify/fsnotify`).

Key properties:
- Output is `[]ChangeEvent` — never writes to the database
- Local events keyed by path (no ItemID — that's a remote concept)
- Compares against in-memory baseline, not DB queries
- Racily-clean guard: same-second mtime triggers hash verification
- Dual-path threading: `fsRelPath` (filesystem I/O) and `dbRelPath` (NFC-normalized for baseline lookup)
- Symlinked directories always excluded from watch mode
- inotify watch limit detection on Linux (`inotify_linux.go`)

### Scanner (`scanner.go`)

Extracted filesystem walker for full-scan mode. Produces change events by walking the sync directory and comparing against baseline.

## Change Buffer (`buffer.go`)

Collects events from both observers, deduplicates, debounces (default 2 seconds), and produces `[]PathChanges` batches grouped by path. Thread-safe (mutex-protected). Move events are dual-keyed: stored at new path AND synthetic delete at old path.

`FlushImmediate` for one-shot mode (no debounce wait).

## Permissions (`permissions.go`)

Read-only detection for shared content. When a write attempt returns 403, the path prefix is recorded and subsequent writes to that prefix are suppressed (download-only for that subtree).

## Design Constraints

Implements: R-6.2.7 [verified]

- Filtering is symmetric: any exclusion applied to local items (always-excluded suffixes, invalid OneDrive names) is also applied to remote items in `classifyItem()`. This prevents remote-only files (e.g., temp files uploaded via web UI) from entering the planner.
- The `alwaysExcludedSuffixes` list does NOT include `.db`/`.db-wal`/`.db-shm` — those caused false positives on legitimate data files. The sync engine's state DB lives outside the sync root by design.
- Delta processing uses two-pass page handling to ensure vault parent folders are classified before their children, preventing path materialization failures.
- Personal Vault items (`specialFolder.name == "vault"`) are excluded by default. Vault auto-locks after 20 minutes → locked items appear deleted in delta → phantom deletions. `sync_vault = true` escape hatch.
- FullScan uses three sequential phases: (1) Walk (readdir + Lstat + classify), (2) Hash (parallel QuickXorHash via `errgroup.SetLimit(checkWorkers)`), (3) Deletion detection (compare observed paths vs baseline).

### Rationale

`fsnotify/fsnotify` (v1.9.0, 10.6k stars, used by Hugo/Docker/Kubernetes/Syncthing) was chosen over `rjeczalik/notify` (unmaintained since Jan 2023).

### Planned Improvements

- Debounce semantics under load: write coalescing implemented, load-testing and tuning remaining. [planned]
- Streaming delta processing: process pages as they arrive rather than buffering all pages. Modest win (~1ms per page vs ~100-300ms API call). [planned]
- Total item cap during delta enumeration to bound memory from unbounded API pages. [planned]
- Per-path event cap in Buffer: `defaultBufferMaxPaths` caps path count but not events-per-path. [planned]
- Panic recovery in scanner hash phase: worker pool has recovery, scanner does not. [planned]
- Nil guards on all Item field accesses in `observer_remote.go`: delta items may have missing `Name`, `ParentReference`, etc. [planned]
- NFC normalization idempotency test. [planned]
- Buffer overflow test with drop metric verification. [planned]
- inotify partial-watch cleanup verification: ensure already-added watches are cleaned up on setup failure. [planned]
- Remote filename validation: reject `..`, `/`, `\`, null bytes, control chars in `classifyAndConvert`. [planned]
