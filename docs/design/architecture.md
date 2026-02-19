# System Architecture: onedrive-go

## 1. System Overview

**onedrive-go** is a CLI-first OneDrive client that provides Unix-style file operations (`ls`, `get`, `put`) and robust bidirectional synchronization with conflict tracking. It targets Linux and macOS as primary platforms, with FreeBSD as best-effort.

### Key Properties

- **Safe**: Conservative defaults, three-way merge conflict detection, big-delete protection, atomic file writes, never lose user data
- **Fast**: Parallel transfers (3 independent worker pools), delta-driven sync, parallel initial enumeration, <100MB memory for 100K files
- **Tested**: All I/O behind interfaces for mocking, comprehensive E2E tests against live OneDrive

### Design Principles

1. **Safety-first**: Every destructive operation has a guard. Downloads use `.partial` + hash verify + atomic rename. Remote deletes are never based on download state. Big-delete protection uses count AND percentage thresholds.
2. **Delta-driven**: Steady-state sync uses the Graph API delta endpoint for efficient change detection. Only initial sync and recovery paths use full enumeration.
3. **ID-based tracking**: Items are tracked by `(driveId, itemId)` composite key -- the only stable identity across renames, moves, and API inconsistencies. Paths are materialized in the database but derived from the ID graph.
4. **Parallel-by-default**: Three separate worker pools (uploads, downloads, checkers) each default to 8 workers. Initial sync uses parallel recursive `/children` enumeration.
5. **Interface-driven testability**: Every component communicates via Go interfaces. All I/O (filesystem, network, database) is behind interfaces, enabling deterministic testing with mocks.

### Component Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                      cmd/onedrive-go/ (Cobra CLI)                   │
│  ls, get, put, rm, mkdir, sync, status, conflicts, resolve, login  │
│  logout, config, migrate, verify                                   │
└──────────┬───────────────────────────────────────┬──────────────────┘
           │ file ops (direct API)                 │ sync operations
           │                                       │
           ▼                                       ▼
┌──────────────────────┐             ┌──────────────────────────────┐
│   internal/graph/    │◄────────────│       internal/sync/         │
│   Graph API client   │             │  engine, db, scanner, delta, │
│   + quirk handling   │             │  reconciler, executor,       │
│   + auth             │             │  conflict, filter, transfer  │
└──────────────────────┘             └──────────────┬───────────────┘
                                                    │
                                     ┌──────────────┴───────────────┐
                                     │       internal/config/       │
                                     │       TOML + profiles        │
                                     └──────────────────────────────┘

           ┌────────────────┐
           │ pkg/           │
           │ quickxorhash/  │
           │ (vendored)     │
           └────────────────┘
```

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `internal/graph/` handles all API quirks internally -- callers never see raw API data.

---

## 2. Package Layout

```
cmd/onedrive-go/                    # CLI (Cobra commands)
  main.go                           # Entry point
  root.go                           # Root command, global flags, config loading
  auth.go                           # login, logout, whoami
  files.go                          # ls, get, put, rm, mkdir, stat, cat
  sync.go                           # sync (one-shot + watch)
  conflicts.go                      # conflicts, resolve, verify
  config.go                         # config init, config show, migrate

internal/
  graph/                            # Graph API client -- ALL API interaction + quirk handling
    client.go                       # Client struct, HTTP transport, retry, rate limiting
    auth.go                         # Device code flow, token refresh, persistingTokenSource
    types.go                        # Clean types: Item, DeltaPage, Drive, User, UploadSession, ChunkResult
    raw.go                          # Unexported rawDriveItem + JSON deserialization types
    normalize.go                    # All 12+ quirk handlers (driveID, deletion reorder, timestamps, etc.)
    errors.go                       # Sentinel errors (ErrGone, ErrNotFound, ErrThrottled, etc.)
    items.go                        # GetItem, ListChildren, CreateFolder, MoveItem, DeleteItem
    delta.go                        # Delta with pagination + normalization pipeline
    upload.go                       # Simple + chunked uploads
    download.go                     # Streaming downloads
    drives.go                       # Me, Drives, Drive

  sync/                             # Sync engine -- everything sync-related in one package
    engine.go                       # Orchestrator (RunOnce, RunWatch)
    db.go                           # SQLite state (schema, migrations, CRUD)
    scan.go                         # Local filesystem scanning
    delta.go                        # Delta processing (calls graph/, stores in db)
    reconcile.go                    # Three-way merge decision matrix (F1-F14, D1-D7)
    execute.go                      # Action execution (downloads, uploads, deletes, moves)
    conflict.go                     # Conflict detection + resolution
    safety.go                       # S1-S7 safety invariants
    filter.go                       # Three-layer filtering
    transfer.go                     # Download/upload with hash verification, worker pools

  config/                           # TOML config with profiles
    config.go                       # Types, loading, validation
    paths.go                        # XDG paths, profile derivation

pkg/
  quickxorhash/                     # Copied from rclone (BSD-0 license)
```

**Dependency rule**: `cmd/` -> `internal/*` -> `pkg/*`. No `internal/` package may import from `cmd/`. No `pkg/` package may import from `internal/`.

---

## 3. Component Responsibilities and Interfaces

### 3.1 Package Philosophy: Pragmatic Flat

This project uses a "pragmatic flat" package layout: few large packages with well-defined boundaries rather than many small packages with complex inter-dependencies. The benefits:

- **Fewer import cycles**: With only 4 packages (`graph/`, `sync/`, `config/`, `quickxorhash/`), dependency management is trivial.
- **Consumer-defined interfaces**: `sync/` defines narrow interfaces over `graph.Client` methods it actually uses. `graph/` exports concrete types, not interfaces.
- **Reduced boilerplate**: No interface adapter layers, no unnecessary abstraction boundaries.
- **Easier refactoring**: Moving code within a package is cheaper than moving it between packages.

### 3.2 Graph API Client (`internal/graph/`)

**Responsibility**: Handles ALL Microsoft Graph API communication -- authentication, CRUD operations, delta queries, upload sessions, download URLs. Also handles ALL API quirk normalization internally. Callers receive clean, consistent data and never need to worry about API inconsistencies.

`graph/` exposes **concrete types, not interfaces**:

- `graph.Client` is a concrete struct with methods for every API operation.
- `graph.Item` is the clean, normalized item type. All quirks (driveID casing, missing fields, timestamp validation, etc.) are handled before `Item` is returned to callers. Both the CLI and sync engine use this same clean type.
- No interfaces are exported from `graph/`.

### 3.3 Sync Engine (`internal/sync/`)

**Responsibility**: Orchestrates the entire sync lifecycle -- fetching remote deltas, scanning local changes, reconciling differences via three-way merge, and dispatching actions to the executor. Owns the main sync loop for both one-shot and continuous modes. Also owns the SQLite state database, filtering engine, and transfer pipeline (worker pools, bandwidth limiting, hash verification).

**Consumer-defined interfaces over graph.Client** (defined in `sync/`, satisfied by `*graph.Client`):

```go
// deltaClient is used by the delta processor to fetch remote changes.
type deltaClient interface {
    Delta(ctx context.Context, driveID, token string) (*graph.DeltaPage, error)
}

// remoteDeleter is used by the executor to delete remote items.
type remoteDeleter interface {
    DeleteItem(ctx context.Context, driveID, itemID string) error
}

// transferClient is used by the transfer pipeline for downloads and uploads.
type transferClient interface {
    Download(ctx context.Context, driveID, itemID string, w io.Writer) error
    SimpleUpload(ctx context.Context, driveID, parentID, name string, r io.Reader) (*graph.Item, error)
    CreateUploadSession(ctx context.Context, driveID, parentID, name string) (*graph.UploadSession, error)
    UploadChunk(ctx context.Context, session *graph.UploadSession, chunk io.Reader, offset, length, total int64) (*graph.ChunkResult, error)
}
```

**Exposed interface for the engine itself** (for testability and future RPC):

```go
type Engine interface {
    RunOnce(ctx context.Context, mode SyncMode) (*SyncReport, error)
    RunWatch(ctx context.Context, mode SyncMode) error
    Stop(ctx context.Context) error
}

// RPC-readiness: define at MVP even though RPC is post-MVP
type StatusReporter interface {
    Status(ctx context.Context) (*StatusReport, error)
    ListConflicts(ctx context.Context) ([]Conflict, error)
}

type SyncController interface {
    Pause(ctx context.Context) error
    Resume(ctx context.Context) error
}
```

### 3.4 Item Representations

Two item types exist in the system, each with a clear purpose:

| Type | Purpose | Where defined |
|------|---------|---------------|
| `graph.Item` | Clean API response type. All quirks handled. Used by CLI and sync. | `internal/graph/types.go` |
| `sync.Record` | Database row with local/remote/synced state tracking. Three hash columns, three timestamp columns. | `internal/sync/db.go` |

There is no `NormalizedItem` type. There is no `DriveItem` in any public API. `graph.Item` is the single clean representation that all callers work with. `sync.Record` extends this with sync-specific tracking state (last-known local hash, last-known remote hash, synced hash, local mtime, remote mtime, synced mtime).

### 3.5 Config (`internal/config/`)

**Responsibility**: Loads, validates, and provides access to TOML configuration. Manages multi-profile configuration and migration from abraunegg/rclone formats.

**Key interfaces exposed**:
```go
type Config interface {
    Profile(name string) (*Profile, error)
    ProfileNames() []string
    Global() *GlobalConfig
    FilterConfig() *FilterConfig
    TransferConfig() *TransferConfig
    SafetyConfig() *SafetyConfig
}
```

### 3.6 QuickXorHash (`pkg/quickxorhash/`)

**Responsibility**: Implements the QuickXorHash algorithm, the only hash available on both Personal and Business OneDrive accounts. Copied from rclone under BSD-0 license. This is a true leaf utility with zero dependencies on other project packages.

---

## 4. Data Flow

### 4.1 CLI Path (File Operations)

File operations (`ls`, `get`, `put`, `rm`, `mkdir`) are completely independent of the sync engine. They make direct API calls through `internal/graph/` and do not read or write the sync database. They work regardless of whether a sync process is running.

```
cmd/onedrive-go/  ──►  graph.Client  ──►  Microsoft Graph API
                            │
                            ▼
                      []graph.Item (clean, normalized)
                            │
                            ▼
                    cmd/ formats and prints
```

### 4.2 Sync Path (One-Shot Bidirectional Sync)

```
cmd/onedrive-go/  ──►  sync.Engine
```

The engine internally:

```
 1. Load config + open per-profile state DB
 2. graph.Delta() ──► []graph.Item (clean, normalized)
 3. Convert to sync.Record, store in SQLite
 4. Scan local filesystem ──► sync.Record updates
 5. Three-way reconcile: current-local vs current-remote vs last-known
 6. Detect conflicts (both sides changed since last sync)
 7. Plan actions: download / upload / delete / move / conflict / stale
 8. Safety checks: big-delete (count AND percentage, min threshold), disk space
 9. Execute via worker pools (3 parallel pools)
10. Verify every transfer (hash check via streaming TeeReader)
11. Update sync.Record in SQLite
12. Save delta token checkpoint
```

```
                    ┌─────────┐
                    │  Config  │
                    └────┬────┘
                         │
                    ┌────▼────┐
                    │  State  │◄──── open per-profile DB
                    │   DB    │
                    └────┬────┘
                         │
              ┌──────────▼──────────┐
              │   Delta Fetch       │ ← graph.Delta()
              │   (paged)           │
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Store as Records  │ ← sync.Record in SQLite
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Local FS Scan     │ ← compare against DB
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Three-Way         │
              │   Reconcile         │ ← local vs remote vs last-known
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Safety Checks     │ ← big-delete, disk space
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Worker Pools      │ ← 3 pools (up/down/check)
              │   (execute actions) │
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Verify + Update   │ ← hash check, DB update
              │   State DB          │
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Save Checkpoint   │ ← delta token
              └─────────────────────┘
```

### 4.3 Continuous Mode (`--watch`)

Same sync loop, triggered by change events rather than manual invocation:

- **Local changes**: `rjeczalik/notify` events -> 2-second batch debounce -> re-scan changed paths only
- **Remote changes**: WebSocket push OR 5-min poll -> delta fetch from last token
- **Graceful shutdown**: SIGINT/SIGTERM -> finish current transfers (configurable timeout) -> checkpoint -> exit
- **Config reload**: SIGHUP -> reload configuration, re-initialize filter engine

### 4.4 Initial Sync (First Run)

On first run, there is no delta token. The delta API returns every item in the drive. For maximum speed, initial sync uses two optimized code paths:

1. **Parallel recursive `/children` enumeration**: Multiple walker goroutines enumerate the remote tree concurrently, building the full item graph in the state DB
2. **Parallel downloads**: All discovered files are queued to the download worker pool immediately

After initial sync completes, a delta token is saved and all subsequent syncs use the efficient delta-based path.

---

## 5. Concurrency Model

### 5.1 Database Writer

- **Single writer goroutine per profile**. All write operations are serialized through a channel to this goroutine. This eliminates the need for SQLite busy handling or mutex contention on writes.
- **Concurrent readers** via SQLite WAL mode. Status queries, conflict listing, and other read operations can proceed without blocking the writer.

### 5.2 Worker Pools

Three independent worker pools, each with configurable size (default 8):

| Pool | Default Workers | Purpose |
|------|----------------|---------|
| Downloads | 8 | File downloads from OneDrive |
| Uploads | 8 | File uploads to OneDrive |
| Checkers | 8 | Local hash computation for change detection |

Worker pools use buffered channels as semaphores for token-based limiting. Each worker gets its own context derived from the sync run's root context.

### 5.3 Pipeline

```
Scanner ──channel──► Reconciler ──channel──► Executor
                                                │
                                    ┌───────────┼───────────┐
                                    ▼           ▼           ▼
                                Downloads   Uploads     Checkers
```

Bounded channels between pipeline stages provide backpressure. If the executor cannot keep up (all workers busy), the reconciler blocks, which in turn blocks the scanner.

### 5.4 Context Tree

One root context per sync run. Cancellation propagates to all stages:

```
rootCtx
├── deltaFetchCtx
├── scannerCtx
├── reconcilerCtx
└── executorCtx
    ├── downloadWorker[0..N]
    ├── uploadWorker[0..N]
    └── checkWorker[0..N]
```

### 5.5 Graceful Shutdown

- **First signal** (SIGINT/SIGTERM): Cancel root context. In-flight transfers finish up to a configurable timeout. Delta checkpoint is saved. Exit cleanly.
- **Second signal**: Immediate cancellation. No checkpoint save. SQLite WAL ensures DB consistency.
- **SIGHUP**: Reload configuration. Re-initialize filter engine and bandwidth scheduler. Do not restart sync -- the next cycle picks up new config.

---

## 6. State Management

### 6.1 Database Engine

- **SQLite** via `modernc.org/sqlite` (pure Go, no CGO dependency)
- **WAL mode** for concurrent readers + single writer
- **FULL synchronous** -- durability on crash at the cost of slightly higher write latency

### 6.2 Database Scope

- **Separate database file per profile** -- complete isolation between accounts
- Path: `~/.local/share/onedrive-go/state/{profile}.db` (Linux) or `~/Library/Application Support/onedrive-go/state/{profile}.db` (macOS)

### 6.3 Database Resilience

The Microsoft Graph API returns inconsistent data (truncated driveIds, missing fields, cross-drive orphans). The database schema must tolerate these without cascading failures:

- **Nullable fields** where the API may omit data (hash values, timestamps, type for `package` items)
- **Deferred foreign key handling**: Items may arrive before their parent is known (e.g., shared folder items reference a parent on another drive). Insert as orphans, reconcile parent references in a subsequent pass.
- **Normalized identifiers before storage**: All driveIds are lowercased + zero-padded before any DB operation
- **Graceful constraint violation handling**: FK violations log a warning and queue the item for retry, rather than aborting the batch

### 6.4 Item Identity

- **Primary key**: `(driveId, itemId)` composite -- the only stable identity in the OneDrive API
- All `driveId` values are normalized (lowercase, zero-padded) before storage
- Item IDs are treated as opaque strings; no format assumptions

### 6.5 Materialized Paths

- Full local paths are **stored in the database** alongside items
- Paths are **rebuilt when delta shows a parent change** -- if a folder is renamed or moved, all descendant paths are updated via cascading update
- This avoids the expensive O(depth) parent-chain walk on every access that a naive approach would require

### 6.6 Checkpoints

- **Configurable batch size** (default 500 items) for delta processing
- After each batch, a WAL checkpoint is performed to bound WAL file growth
- **Crash recovery**: On restart, re-fetch from the last saved delta token. At most one batch of items may need reprocessing.
- Delta tokens are stored per drive in the state DB

### 6.7 Tombstones

- Deleted records are kept as tombstones rather than immediately purged
- **Default retention**: 30 days (configurable)
- Tombstones enable reliable move detection across sync cycles (an item deleted from one parent and appearing under another can be recognized as a move)
- Explicit purge via age-based cleanup or manual trigger

### 6.8 Conflict Ledger

Per-file tracking stored in the state DB:

| Field | Description |
|-------|-------------|
| `id` | Unique conflict identifier |
| `path` | File path at time of conflict |
| `detected_at` | Timestamp of conflict detection |
| `local_hash` | QuickXorHash of local version |
| `remote_hash` | QuickXorHash of remote version |
| `local_mtime` | Local file modification time |
| `remote_mtime` | Remote file modification time |
| `resolution` | `unresolved`, `accepted_local`, `accepted_remote`, `kept_both` |
| `resolved_at` | Timestamp of resolution (null if unresolved) |
| `history` | JSON array of resolution events |

### 6.9 Stale Files Ledger

Tracks files that are excluded by filter changes but still present locally:

| Field | Description |
|-------|-------------|
| `path` | Local file path |
| `reason` | Why it became stale (e.g., "excluded by skip_files pattern *.tmp") |
| `detected_at` | When the stale status was detected |
| `size` | File size |

Users are nagged about stale files. An explicit user interface allows disposition of each (delete or keep). The sync engine **never auto-deletes** stale files.

---

## 7. Error Handling

### 7.1 Four-Tier Classification

| Tier | Examples | Response |
|------|----------|----------|
| **Fatal** | Auth failure, DB corruption, impossible state | Stop entire sync, alert user, exit non-zero |
| **Retryable** | Network timeout, HTTP 429/500/503/504 | Exponential backoff + jitter + Retry-After, max 5 retries |
| **Skip** | Permission denied on single file, invalid filename, path too long | Log warning, skip item, continue sync |
| **Deferred** | Parent dir not yet created, file locked by another process | Queue for retry at end of current sync cycle |

### 7.2 Retry Strategy

- **Backoff**: Exponential with jitter. Base: 1 second, factor: 2, max: 120 seconds.
- **Retry-After**: For HTTP 429, use the `Retry-After` header value directly instead of calculated backoff.
- **Max retries**: 5 per operation before promoting to skip.
- **Global rate awareness**: A shared token bucket across all workers prevents thundering herd on rate limit responses.

### 7.3 Safety Invariants

These invariants must never be violated:

1. **Never delete remote based on download state.** A failed download must not trigger deletion of the remote file. Download verification and remote deletion are separate, independent operations. This prevents a catastrophic bug pattern where failed downloads could lead to data loss.
2. **Never process deletions from incomplete delta fetches.** If a delta page fetch fails mid-stream, process the items already received but do not treat missing items as deletions. Resume from checkpoint on next cycle.
3. **Atomic file writes.** All downloads use: write to `.partial` file -> verify hash -> atomic rename to final path. A failed download never corrupts an existing file.
4. **Process what we got.** Partial delta results are processed and checkpointed. The next cycle resumes from the saved token rather than discarding partial work.

### 7.4 HTTP Error Handling

| Status Code | Classification | Action |
|-------------|---------------|--------|
| 400 | Skip | Log error, skip operation |
| 401, 403 | Fatal/Skip | 401 triggers re-auth; 403 on shared item = skip |
| 404 | Skip | Item no longer exists, remove from DB |
| 408 | Retryable | Timeout, retry with backoff |
| 410 | Special | Delta token expired -- handle both resync types |
| 412 | Retryable | eTag stale, refresh and retry |
| 423 | Skip | File locked (SharePoint), skip with warning |
| 429 | Retryable | Rate limited, use Retry-After header |
| 500, 502, 503, 504 | Retryable | Server error, retry with backoff |
| 509 | Retryable | Bandwidth exceeded (SharePoint), back off |

---

## 8. API Quirk Normalization

All known API quirks are handled inside `internal/graph/`. The normalization pipeline runs as part of every API response deserialization, so callers (both the CLI and the sync engine) always receive clean, consistent `graph.Item` values. No separate normalization layer is needed.

| Quirk | Handling |
|-------|----------|
| driveId casing inconsistency | `strings.ToLower()` on every driveId from every API response |
| driveId truncation (15 vs 16 chars) | Left-pad with `0` to 16 characters for Personal accounts |
| Deletions after creations at same path | Buffer full delta page, sort: process deletions before creations at the same path |
| Missing `name` on deleted items (Business) | Look up from state DB before processing deletion |
| Missing `size` on deleted items (Personal) | Look up from state DB before processing deletion |
| cTag absent on Business folders | Use eTag + children enumeration for folder change detection |
| cTag absent in Business delta create/modify | Fall back to eTag + hash comparison |
| `parentReference.path` never in delta | Reconstruct from parent chain in state DB (materialized paths) |
| Upload fragment alignment | Enforce 320KiB multiples in transfer pipeline, validate chunk size config |
| iOS `.heic` hash mismatch | Log warning, skip hash verification for known-affected MIME types |
| SharePoint post-upload enrichment | Per-side hash baselines in three-way merge; no enrichment-specific code path. See SHAREPOINT_ENRICHMENT.md. |
| HTTP 410 delta token expired | Handle both resync types: `resyncChangesApplyDifferences` (trust server) vs `resyncChangesUploadDifferences` (upload local unknowns) |
| Zero-byte file hashes | Always use simple upload (not session), skip hash verification |
| Invalid/missing timestamps | Validate on ingestion, fall back to current UTC time |
| URL-encoded spaces in `parentReference.path` | URL-decode all path fields |
| Items appearing multiple times in delta | Use last occurrence per item ID |
| Bogus hash on deleted items | Ignore server hash for items with deleted facet, compare against DB |
| HTTP 308 without Location header | Detect and handle gracefully; recreate webhook subscription |
| OneNote package items | Detect via `package` facet, skip entirely |
| `Prefer` header for Personal delta | Include `Prefer: deltashowremoteitemsaliasid` in all delta requests for Personal accounts; without it, shared folder items are invisible in delta responses |
| Upload session resume after interruption | On resume, query upload session status endpoint for accepted byte ranges; never blindly retry from last local position (causes HTTP 416). Handle 416 by re-querying session status. |
| Double-versioning on Business/SharePoint | Include `fileSystemInfo` (timestamps) in upload session creation request. Never PATCH timestamps separately after upload — SharePoint treats the PATCH as a modification and creates a second version. |
| NFC/NFD Unicode normalization (macOS) | macOS APFS uses NFD (decomposed) filenames; Linux uses NFC (composed). OneDrive does not normalize. Normalize to NFC before comparison on all platforms. |
| SharePoint file lock check | Before uploading to SharePoint, check lock status. HTTP 423 = file locked by co-author. Skip with warning to avoid overwriting active edits. |
| Missing hash on non-zero files (Business) | Some Business/SharePoint files lack any hash in API responses. Fall back to size + eTag + mtime comparison. Zero-byte files never have hashes — compare by size alone. |
| National Cloud delta unsupported | US Government, Germany, and China cloud deployments do not support `/delta`. Fall back to `/children` enumeration. Detect via config `account_type` or API error. |

---

## 9. Security Model

### 9.1 Token Storage

- **Separate token file per profile**: `~/.config/onedrive-go/tokens/{profile}.json` (Linux) or `~/Library/Application Support/onedrive-go/tokens/{profile}.json` (macOS)
- File permissions: `0600` (owner read/write only)
- Keychain integration: post-MVP

### 9.2 Logging Safety

- Bearer tokens are scrubbed from all log output, including debug level
- Pre-authenticated download URLs (which contain embedded tokens) are truncated in logs
- Upload session URLs (pre-authenticated) are truncated in logs

### 9.3 Transfer Verification

- **Downloads**: Always verify QuickXorHash of downloaded content against server-reported hash. Exception: known-broken MIME types (iOS `.heic`) where only a warning is logged.
- **Uploads**: After upload, verify server-reported hash matches local file hash. Upload verification logs INFO for SharePoint enrichment; per-side baselines prevent spurious re-upload/re-download. See SHAREPOINT_ENRICHMENT.md.
- **Streaming hash**: Hash computation uses `io.TeeReader` to compute the hash during transfer, avoiding a second pass over the file.

### 9.4 Signal Handling

| Signal | Action |
|--------|--------|
| SIGINT | Graceful shutdown: drain in-flight transfers (configurable timeout), save checkpoint, exit |
| SIGTERM | Same as SIGINT |
| SIGHUP | Reload configuration (filter rules, bandwidth schedule, etc.) |

---

## 10. CLI and Process Model

### 10.1 Framework and Identity

- **CLI framework**: spf13/cobra
- **Module path**: `github.com/tonimelisma/onedrive-go`
- **Go version**: 1.24+
- **Binary name**: `onedrive-go`

### 10.2 Commands

| Command | Description | Sync DB? | API? |
|---------|-------------|----------|------|
| `ls [path]` | List files and folders | No | Yes |
| `get <remote> [local]` | Download file or folder | No | Yes |
| `put <local> [remote]` | Upload file or folder | No | Yes |
| `rm <path>` | Delete (to recycle bin by default) | No | Yes |
| `mkdir <path>` | Create folder | No | Yes |
| `sync` | One-shot or continuous sync | Yes (write) | Yes |
| `status` | Sync state and pending changes | Yes (read) | No |
| `conflicts` | List unresolved conflicts | Yes (read) | No |
| `resolve <id\|path>` | Resolve a conflict | Yes (write) | Maybe |
| `verify` | Re-hash all local files, compare to DB and remote | Yes (read) | Yes |
| `login [--headless]` | Authenticate | No | Yes |
| `logout` | Clear credentials | No | No |
| `config init` | Interactive setup wizard | No | Yes |
| `config show` | Display current configuration | No | No |
| `migrate` | Import from abraunegg/rclone | No | No |

### 10.3 Global Flags

```
--profile <name>       # Select account profile (default: "default")
--config <path>        # Override config file location
--json                 # Machine-readable JSON output (all commands, from MVP)
--verbose / -v         # Verbose output (stackable: -vv for debug)
--quiet / -q           # Suppress non-error output
--dry-run              # Preview operations without executing
--debug                # Trace-level logging (from day 1)
```

### 10.4 Process Model

- **SQLite lock** enforces single sync writer per profile. A second `sync` process gets a "database is locked" error.
- **Concurrent readers**: `status`, `conflicts`, and `resolve` (read path) can run while sync is active, via SQLite WAL.
- **File operations** (`ls`, `get`, `put`, `rm`, `mkdir`) are completely independent -- no database, no lock contention.
- **`sync --watch`** is just sync that keeps running. No separate daemon concept. Run it interactively or from systemd/launchd with `--quiet`.

### 10.5 JSON Output

All commands support `--json` output from MVP. This enables scripting and GUI integration without parsing human-readable text.

### 10.6 RPC Readiness

The following interfaces are defined at MVP even though RPC is post-MVP:

- `StatusReport` -- current sync state, pending changes, conflict count
- `PauseSync` / `ResumeSync` -- pause/resume continuous sync
- `ListConflicts` -- list unresolved conflicts with details

When RPC ships (JSON-over-HTTP on a Unix domain socket), these interfaces are exposed directly. Zero refactoring needed.

---

## 11. Filtering

### 11.1 Layered Evaluation

Each layer can only EXCLUDE more; never include back. Evaluation order:

```
Item path
  │
  ▼
┌─────────────────────┐
│ 1. sync_paths       │  If set, only these paths considered.
│    allowlist         │  Everything else excluded immediately.
└─────────┬───────────┘
          │ (passes)
          ▼
┌─────────────────────┐
│ 2. Config patterns  │  skip_files, skip_dirs, skip_dotfiles,
│                     │  max_file_size
└─────────┬───────────┘
          │ (passes)
          ▼
┌─────────────────────┐
│ 3. .odignore        │  Per-directory marker files with
│    marker files     │  gitignore-style patterns
└─────────┬───────────┘
          │ (passes)
          ▼
      INCLUDED
```

### 11.2 Filter Change Handling

When filter rules change (new patterns added, sync_paths modified), files that are now excluded but still present locally are tracked in the **stale files ledger**:

- User is nagged about stale files on every sync and via `status`
- Explicit interface to disposition each file: delete or keep
- **Never auto-delete**. This is a safety-critical feature.

### 11.3 Cascading Exclusion

If a directory is excluded, all its descendants are automatically excluded without evaluating individual items. The filter engine maintains an excluded-parents set to short-circuit evaluation.

---

## 12. Move and Rename Detection

### 12.1 ID-Based Detection

The OneDrive API includes item IDs in delta responses. When the same `(driveId, itemId)` appears at a different `parentReference.id` or with a different `name`, it is a move or rename. This is detected by comparing the delta item against the existing state DB record.

### 12.2 Local Application

Detected moves are applied as local filesystem rename operations -- no re-download required. This is a significant advantage over tools without ID-based tracking (like rclone bisync, which would delete + re-download).

### 12.3 Cascading Path Updates

When a folder is moved or renamed:
1. The folder's own materialized path is updated in the state DB
2. All descendant paths are updated via cascading update
3. The local filesystem rename is performed once (at the folder level)

---

## 13. Safety Features

### 13.1 Big-Delete Protection

- **Dual threshold**: Both absolute count AND percentage of total items must exceed their thresholds to trigger
- **Minimum threshold**: Deleting 1 out of 5 files (20%) does not trigger -- a minimum absolute count applies
- **All configurable**: `big_delete_threshold` (count), `big_delete_percentage` (percentage)
- **Behavior**: Abort sync and require `--force` to proceed

### 13.2 Mount Guard (`.nosync` File)

When the sync directory is a mount point (NFS, CIFS, USB), the mount can disappear. The client would interpret an empty mount as "all files deleted" and propagate deletions. To prevent this:

- Place a `.nosync` guard file on the underlying filesystem before mounting
- When the mount is active, the file is hidden beneath the mount
- If the mount disappears, the file becomes visible
- The sync engine checks for `.nosync` at startup and before each sync cycle; if found, halt immediately
- This complements big-delete protection (S5) — `.nosync` catches the root cause, big-delete catches the symptom

### 13.3 Disk Space

- Check available disk space before each download
- Skip download if free space would drop below `min_free_space` (configurable, default 1GB)
- Log warning for skipped downloads

### 13.4 Local Trash

- **Default**: Use OS trash for local files deleted by remote changes
  - Linux: FreeDesktop.org Trash specification
  - macOS: Finder Trash
- **Configurable**: `use_local_trash = false` for permanent deletion
- **Remote**: Use OneDrive recycle bin by default (`use_recycle_bin = true`)

### 13.5 Verify Command

`onedrive-go verify` -- re-hash all local files, compare against state DB and remote hashes. Reports mismatches. Available from MVP.

**Periodic verification**: Opt-in configurable full-tree hash verification on a schedule (e.g., weekly).

### 13.6 Crash Recovery

- SQLite WAL mode ensures database consistency even on abrupt termination
- Resume from last saved delta token -- at most one batch (default 500 items) needs reprocessing
- Upload sessions are persisted to disk and resume automatically

---

## 14. Logging and Observability

### 14.1 Library

`log/slog` (Go standard library). No third-party logging dependency.

### 14.2 Output Modes

| Mode | stderr | Log file |
|------|--------|----------|
| Interactive | Text format, human-readable | JSON format, auto |
| `--quiet` | Suppressed (errors only) | JSON format, auto |
| `--debug` | Trace-level text | Trace-level JSON |

### 14.3 Automatic File Logging

- **Always enabled**: Log to file automatically, regardless of interactive/quiet mode
- **Location**: `~/.local/share/onedrive-go/logs/` (Linux) or `~/Library/Application Support/onedrive-go/logs/` (macOS)
- **Daily files**: `onedrive-go-2026-02-17.log`
- **Retention**: 30 days by default (configurable). Built-in old log deletion.

### 14.4 Structured Fields

Every log entry includes context fields as appropriate:

- `profile` -- which account profile
- `drive` -- which drive ID
- `path` -- item path
- `op` -- operation (download, upload, delete, move, conflict)
- `duration` -- operation timing
- `size` -- file size
- `error` -- error details (for warning/error levels)

### 14.5 Metrics

Internal counters tracked during sync: files downloaded, uploaded, deleted, moved, conflicts detected, errors by tier, bytes transferred, transfer speeds. Exposed via `status` command output and `--json`. Prometheus endpoint is post-MVP.

---

## 15. Relationship to Existing Code

### 15.1 Approach

**Clean slate**. Existing code is not sacred. Keep what is useful, delete what is not. No backward compatibility with the current CLI.

### 15.2 `internal/graph/` (New)

| Package | Action |
|---------|--------|
| `internal/graph/` | New package. All Graph API interaction, authentication, and quirk normalization in one place. Replaces `pkg/onedrive/` (API client) and absorbs `internal/normalize/` (quirk handling). Lives in `internal/` because it is tailored for this project's needs -- not a general-purpose SDK. |

### 15.3 `internal/sync/` (New)

| Package | Action |
|---------|--------|
| `internal/sync/` | New package. Sync orchestrator (engine, scanner, delta, reconciler, executor, conflict). Absorbs `internal/state/` (SQLite DB), `internal/filter/` (filtering engine), and `internal/transfer/` (worker pools, bandwidth). All sync-related functionality in one cohesive package. |

### 15.4 Rewrite

| Package | Action |
|---------|--------|
| `internal/config/` | Rewrite. TOML + profiles replaces JSON config entirely. |
| `cmd/onedrive-go/` | Rewrite. New command structure (Unix verbs + sync). |

### 15.5 Keep

| Package | Action |
|---------|--------|
| `pkg/quickxorhash/` | Keep as-is. True leaf utility with no project dependencies. |

### 15.6 Delete

Everything not listed above. No backward compatibility concerns.

### 15.7 Option B: SDK Carveout

If `internal/graph/` proves valuable as a standalone, reusable Graph API client, it can be extracted to `pkg/onedrive/` later. This is deferred until after the sync engine works end-to-end. The extraction would be mechanical (move files, update import paths) since `graph/` has no dependencies on other `internal/` packages. Until then, keeping it in `internal/` avoids premature API commitments and allows the types to evolve freely.

---

## 16. Platform and Filesystem

### 16.1 Target Platforms

| Platform | Priority | FS Monitoring |
|----------|----------|---------------|
| Linux (x86_64, ARM64) | Primary | inotify via `rjeczalik/notify` |
| macOS (x86_64, ARM64) | Primary | FSEvents via `rjeczalik/notify` |
| FreeBSD | Best-effort | kqueue via `rjeczalik/notify` |

### 16.2 Data Layout (XDG-Compliant)

| Purpose | Linux | macOS |
|---------|-------|-------|
| Config | `~/.config/onedrive-go/` | `~/Library/Application Support/onedrive-go/` |
| State (DBs) | `~/.local/share/onedrive-go/state/` | `~/Library/Application Support/onedrive-go/state/` |
| Logs | `~/.local/share/onedrive-go/logs/` | `~/Library/Application Support/onedrive-go/logs/` |
| Cache | `~/.cache/onedrive-go/` | `~/Library/Caches/onedrive-go/` |
| Tokens | `~/.config/onedrive-go/tokens/` | `~/Library/Application Support/onedrive-go/tokens/` |

### 16.3 Symlinks

- **Default**: Follow symlinks (sync the target as a regular file/folder)
- **Circular detection**: Track visited inodes to detect and break circular symlink chains
- **Configurable**: `skip_symlinks = true` to ignore all symlinks
- **Broken symlinks**: Detected and skipped with a warning

### 16.4 Filesystem Monitoring

`rjeczalik/notify` provides a unified interface across platforms:
- Linux: inotify (exposes `IN_CLOSE_WRITE` for reliable write completion detection)
- macOS: FSEvents (recursive watching built in)
- FreeBSD: kqueue

Events are batched with a 2-second debounce window before triggering a sync cycle.

### 16.5 Case Sensitivity

OneDrive uses a case-insensitive namespace. On case-sensitive filesystems (Linux, some macOS configurations):
- Before uploading, check for case-insensitive collisions at the target path
- Flag conflicts where two local items would collide under case-insensitive rules
- Apply OneDrive's naming restrictions (disallowed names, invalid characters, trailing dots)

### 16.6 Unicode Normalization

macOS APFS uses NFD (decomposed) Unicode form for filenames; Linux typically uses NFC (composed). OneDrive does not normalize Unicode. A file created on macOS may have a different byte representation than the same visually-identical name created on Linux.

- Normalize all filenames to NFC before comparison (both local paths and API-returned names)
- Use `golang.org/x/text/unicode/norm` for NFC normalization
- This prevents false-positive change detection when syncing across macOS and Linux

---

## 17. Key Libraries

| Library | Purpose | License |
|---------|---------|---------|
| `spf13/cobra` | CLI framework | Apache 2.0 |
| `modernc.org/sqlite` | SQLite driver (pure Go, no CGO) | BSD |
| `BurntSushi/toml` | TOML config parsing | MIT |
| `rjeczalik/notify` | Cross-platform filesystem monitoring | MIT |
| `log/slog` (stdlib) | Structured logging | Go |
| `pkg/quickxorhash` (vendored) | QuickXorHash algorithm | BSD-0 (from rclone) |

**Philosophy**: Use libraries freely. DRY and time-to-market over minimalism. Every dependency should solve a real problem better than we could in-house.

---

## 18. Cross-Cutting Concerns

### 18.1 Account Types

All three OneDrive account types are supported from MVP:

| Type | API Differences |
|------|----------------|
| Personal | SHA1+CRC32 hashes available, QuickXorHash primary. driveId normalization required. Shared folders via remoteItem. |
| Business | SHA256 hash available, QuickXorHash primary. cTag absent on folders and in delta. Different driveId format. |
| SharePoint | Same as Business, plus post-upload enrichment. Each document library is a separate drive. Quota may be restricted/absent. |

A common interface handles all three, with quirk dispatch based on the profile's `account_type` field. `internal/graph/` applies account-type-specific normalization rules internally.

### 18.2 Multi-Profile

- **Single config file** holds all profiles
- **Separate DB per profile** -- complete isolation
- **Separate token file per profile** -- independent authentication
- **Single `sync --watch`** manages all profiles simultaneously, each with its own sync loop and worker pools

### 18.3 Shared Folders

Shared folders are **post-MVP** but the architecture supports them from day one:
- State DB uses `(driveId, itemId)` as primary key -- shared folder items naturally live under a different `driveId`
- Each shared folder gets its own delta query
- Cross-drive references are tracked in the state DB

---

## 19. Downstream Document Constraints

This architecture defines constraints that downstream design documents must respect:

### For `data-model.md`
- Primary key must be `(driveId, itemId)`
- Must include materialized paths with cascading update on parent change
- Must include conflict ledger and stale files ledger tables
- Must use WAL mode with FULL synchronous
- Must define tombstone retention and purge strategy
- Must support separate DB per profile
- State DB is owned by `internal/sync/` (not a separate package)

### For `sync-algorithm.md`
- Must implement three-way merge (local vs remote vs last-known)
- Must process delta items in received order, with deletion reordering at page boundaries
- Must use configurable batch size (default 500) with checkpoints
- Must handle both resync types on HTTP 410
- Must implement the four-tier error model
- Must never delete remote based on download state
- Reconciler, executor, filter, and transfer are all within `internal/sync/`

### For `configuration.md`
- Must use TOML via BurntSushi/toml
- Must support multi-profile with `[profile.NAME]` sections
- Must define all safety thresholds (big_delete_threshold, min_free_space, etc.)
- Must define transfer pool sizes and bandwidth scheduling
- Must support XDG-compliant paths with platform detection

### For `test-strategy.md`
- `sync/` defines consumer interfaces over `graph.Client` -- test strategy must leverage these for unit testing with mocks
- E2E tests must cover all three account types
- Must test API quirk normalization within `internal/graph/` with known-bad inputs
- Must test conflict detection and resolution
- Must test crash recovery from mid-sync interruption

---

## Appendix: Decision Summary

| # | Area | Decision |
|---|------|----------|
| 1 | DB engine | modernc.org/sqlite, WAL mode, FULL synchronous |
| 2 | DB scope | Separate DB per profile |
| 3 | DB writer | Single writer goroutine per profile, readers concurrent via WAL |
| 4 | Item identity | (driveId, itemId) composite primary key |
| 5 | Paths | Materialized in DB, rebuilt on delta |
| 6 | Sync model | Three-way merge (local vs remote vs last-known) |
| 7 | Checkpoints | Configurable batch size, default 500 |
| 8 | Delta ordering | Buffer full page, reorder deletions before creations |
| 9 | Tombstones | Keep 30 days default, configurable, explicit purge |
| 10 | Conflict UX | Rename local, download remote as original name |
| 11 | Conflict ledger | Per-file tracking with resolution history |
| 12 | Stale files | Track excluded-but-present files, nag user, explicit disposition |
| 13 | Download safety | .partial + hash verify + atomic rename |
| 14 | Hash verification | Always verify transfers + opt-in periodic full-tree |
| 15 | Hash computation | Streaming via io.TeeReader |
| 16 | Worker pools | 3 separate (upload/download/check), default 8 each |
| 17 | FS watcher | rjeczalik/notify |
| 18 | Debouncing | Fixed 2-second batch window |
| 19 | Remote changes | WebSocket + 5-min polling fallback (both MVP) |
| 20 | Error model | Four-tier: Fatal / Retryable / Skip / Deferred |
| 21 | Retry strategy | Exponential backoff + jitter + Retry-After, max 5 |
| 22 | Normalization | Inside graph/ client (DRY: CLI and sync both get clean data) |
| 23 | Partial delta | Process what we got, resume from checkpoint |
| 24 | Disk space | Check before each download |
| 25 | Local trash | OS trash by default, configurable |
| 26 | Shutdown | SIGINT/SIGTERM = graceful (configurable), SIGHUP = reload |
| 27 | Big-delete | Count AND percentage, min threshold, configurable |
| 28 | Account types | Common interface, quirk dispatch |
| 29 | Initial sync | Parallel /children walking + parallel download (fastest) |
| 30 | CLI framework | Cobra |
| 31 | Module path | github.com/tonimelisma/onedrive-go |
| 32 | Go version | 1.24+ |
| 33 | TOML library | BurntSushi/toml |
| 34 | QuickXorHash | Copied from rclone (BSD-0) |
| 35 | Logging | log/slog, auto file logging, daily rotation, 30-day retention |
| 36 | Token storage | Separate file per profile, 0600 permissions |
| 37 | Data layout | XDG-compliant split |
| 38 | Symlinks | Follow by default, circular detection |
| 39 | Filter order | sync_paths -> config patterns -> .odignore |
| 40 | File ops | Completely independent of sync engine |
| 41 | Move detection | By item ID, apply as local rename |
| 42 | JSON output | All commands from MVP |
| 43 | Verify command | MVP |
| 44 | RPC prep | Design interfaces at MVP for post-MVP RPC |
| 45 | Existing code | Clean slate. Keep useful, delete rest. |
| 46 | Dependencies | Use libraries freely. DRY and time-to-market over minimalism. |
| 47 | Debug | --debug flag from day 1 |
| 48 | Testability | All I/O behind interfaces for mocking |
| 49 | Package philosophy | "Pragmatic Flat" -- few large packages, consumer-defined interfaces |
| 50 | Item representations | 2 types (graph.Item, sync.Record) -- no NormalizedItem, no public DriveItem |
| 51 | graph/ is internal/ | Tailored for this project, extractable to pkg/ later (Option B) |
| 52 | Upload session resume | Query session status endpoint for accepted byte ranges; never blindly retry |
| 53 | Upload session timestamps | Include fileSystemInfo in session creation to avoid double-versioning |
| 54 | Delta header for Personal | `Prefer: deltashowremoteitemsaliasid` in all Personal delta requests |
| 55 | Mount guard | `.nosync` guard file detects unmounted sync directories |
| 56 | Unicode normalization | NFC normalization before all path comparisons (macOS NFD compat) |
| 57 | Lock check before upload | Check HTTP 423 on SharePoint before uploading to avoid overwriting co-authors |
| 58 | Hash fallback chain | QuickXorHash → SHA256 → size+eTag+mtime (when no hash available) |
| 59 | Deferred FK handling | Database handles orphaned items gracefully, reconciles in later pass |
