# Sync Observation

GOVERNS: internal/sync/observer_local.go, internal/sync/observer_local_handlers.go, internal/sync/observer_remote.go, internal/sync/observer_shortcut.go, internal/sync/scanner.go, internal/sync/buffer.go, internal/sync/shortcuts.go, internal/sync/permissions.go, internal/sync/inotify_linux.go, internal/sync/inotify_other.go

Implements: R-2.1.2 [verified], R-2.4 [implemented], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.15 [planned], R-6.7.16 [planned], R-6.7.19 [planned], R-6.7.20 [verified], R-6.7.21 [planned], R-6.7.24 [verified], R-2.11 [implemented], R-2.11.5 [implemented], R-2.12 [planned], R-2.13.1 [verified], R-2.14.1 [verified], R-2.14.3 [verified], R-2.14.4 [verified]

## Remote Observer (`observer_remote.go`)

Produces `[]ChangeEvent` from the Graph API. Two modes: `FullDelta` (one-shot) and `Watch` (continuous polling).

Key properties:
- Output is `[]ChangeEvent` — never writes to the database
- Path materialization uses the baseline (read-only) + in-flight parent tracking
- Normalization (driveId casing, missing fields, timestamps) happens here
- Within each delta page, deletions are buffered and processed before creations (API reordering bug)
- HTTP 410 (expired delta token) returns `ErrGone` sentinel; engine restarts with full delta

**Server-trusted observation**: The remote observer does NOT filter items by name validity or always-excluded patterns. If OneDrive sends an item in a delta response, it exists on OneDrive — filtering it would be silent data loss. Name-based filtering is a local-only concern (upload validation). Only root items and vault descendants are excluded from remote observation.

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

**ScanResult Return Type** — `FullScan` returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. `SkippedItem` struct: `{Path string, Reason string, Detail string}`. Invalid files become `SkippedItem` entries instead of silent DEBUG logs. Scanner remains a pure observer with no DB dependency — it reports what it found; the engine decides what to record. Implements: R-2.11.5 [implemented]

**`shouldObserve()` — Unified Local Observation Filter** — Single entry point replacing scattered `isAlwaysExcluded()` + `isValidOneDriveName()` calls across scanner, watch handlers, and watch setup. Returns `(observe bool, reason string, detail string)`. When `observe` is false, the caller either skips silently (always-excluded suffixes, dotfiles) or records a `SkippedItem` (invalid names, path too long). Used by `FullScan`, `processEntry`, watch event handlers (`handleCreate`, `handleWrite`, `handleRename`), and watch setup (`setupWatches`).

**Two-Stage Filter** — Validation is split into two stages based on when information is available:
- **Stage 1 (before stat):** Name-based checks (always-excluded suffixes, dotfiles, user skip patterns, invalid OneDrive names via `validateOneDriveName()`) and path length check (> 400 chars). Runs in `shouldObserve()`. Avoids unnecessary `Lstat` calls on files that will be skipped anyway.
- **Stage 2 (after stat):** File size check (> 250 GB). Runs in `processEntry` (full scan), `handleCreate`/`hashAndEmit`/`scanNewDirectory` (watch mode). Requires `Lstat` result to know the file size.

**`validateOneDriveName()`** — Returns `(reason string, detail string)` for names that violate OneDrive naming restrictions. Called directly at all call sites — no convenience wrapper. The `reason` field maps to issue types (e.g., `"invalid_filename"`, `"reserved_name"`). The `detail` field provides a human-readable explanation (e.g., `"contains ':' (invalid character)"`).

**`SkippedItem`** — Defined in `types.go`: `{Path string, Reason string, Detail string, FileSize int64}`. Represents a file that was observed but excluded from the event stream due to validation failure. `FileSize` is populated for `IssueFileTooLarge` (after stat). Collected during full scan and returned in `ScanResult.Skipped`. The engine processes these via `recordSkippedItems()` and `clearResolvedSkippedItems()`.

## Change Buffer (`buffer.go`)

Collects events from both observers, deduplicates, debounces (default 2 seconds), and produces `[]PathChanges` batches grouped by path. Thread-safe (mutex-protected). Move events are dual-keyed: stored at new path AND synthetic delete at old path.

`FlushImmediate` for one-shot mode (no debounce wait).

## Permissions (`permissions.go`)

Read-only detection for shared content. When a write attempt returns 403, the path prefix is recorded and subsequent writes to that prefix are suppressed (download-only for that subtree).

## Design Constraints

Implements: R-6.2.7 [verified]

- Filtering is asymmetric by design: the local observer filters by always-excluded patterns and OneDrive naming rules (upload validation), while the remote observer trusts the server. If OneDrive sends an item in a delta response, it exists remotely — filtering it would cause silent data loss. Remote-only temp files (e.g., uploaded via web UI) are intentionally allowed through to the planner.
- The always-excluded suffix checks in `isAlwaysExcluded()` do NOT include `.db`/`.db-wal`/`.db-shm` — those caused false positives on legitimate data files. The sync engine's state DB lives outside the sync root by design.
- Delta processing uses two-pass page handling to ensure vault parent folders are classified before their children, preventing path materialization failures.
- Personal Vault items (`specialFolder.name == "vault"`) are unconditionally excluded. Vault auto-locks after 20 minutes → locked items appear deleted in delta → phantom deletions.
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
- Remote observer name validation removal: removed — remote observer now trusts server data. [verified]
