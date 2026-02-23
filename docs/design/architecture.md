# System Architecture: onedrive-go

> **Blank-slate design**: This document describes the event-driven architecture
> designed from first principles. See
> [event-driven-rationale.md](event-driven-rationale.md) for the full rationale
> and comparison with alternative approaches considered.

---

## 1. System Overview

**onedrive-go** is a CLI-first OneDrive client that provides Unix-style file operations (`ls`, `get`, `put`) and robust bidirectional synchronization with conflict tracking. It targets Linux and macOS as primary platforms.

### Key Properties

- **Safe**: Conservative defaults, three-way merge conflict detection, big-delete protection, atomic file writes, never lose user data
- **Fast**: Parallel transfers, delta-driven sync, event-driven incremental processing, <100 MB memory for 100K files
- **Tested**: Pure-function planner is exhaustively testable without I/O. All external I/O behind interfaces for mocking. Comprehensive E2E tests against live OneDrive.

### Design Principles

1. **Event-driven pipeline**: Observers produce change events. A pure-function planner converts events + baseline into an action plan. An executor carries out the plan. A baseline manager atomically persists the results. The database is never the coordination mechanism between stages.
2. **Baseline-only persistence**: The only durable per-item state is the confirmed synced baseline (11-column `baseline` table). Remote and local observations are ephemeral -- rebuilt from the API and filesystem each cycle.
3. **Pure-function planning**: The planner has no I/O and no database access. It takes `([]PathChanges, *Baseline, SyncMode, SafetyConfig)` and returns `*ActionPlan`. Every decision is deterministic and reproducible.
4. **Watch-primary**: `sync --watch` is the primary runtime mode. One-shot sync is "collect all events, then process them as a single batch." The same planner, executor, and baseline manager serve both modes.
5. **Interface-driven testability**: Every component communicates via Go interfaces. All I/O (filesystem, network, database) is behind interfaces, enabling deterministic testing with mocks.

### Component Diagram

```
┌──────────────────┐    ┌──────────────────┐
│  Remote Observer │    │  Local Observer   │
│                  │    │                   │
│  * Delta fetch   │    │  * FS walk        │
│  * WebSocket     │    │  * inotify/FSE    │
│  * Polling       │    │  * Hash compute   │
└────────┬─────────┘    └────────┬──────────┘
         │ ChangeEvent            │ ChangeEvent
         └───────────┬────────────┘
                     ▼
            ┌────────────────┐
            │  Change Buffer │
            │                │
            │  * Debounce    │
            │  * Dedup       │
            │  * Batch       │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │    Planner     │    <-- reads Baseline (from DB, cached in memory)
            │                │    <-- reads Change Events
            │  * Merge       │    --> produces ActionPlan
            │  * Reconcile   │
            │  * Filter      │    <-- pure functions, no I/O
            │  * Safety      │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │   Executor     │    <-- executes actions against API + filesystem
            │                │    --> produces Outcomes
            │  * Downloads   │
            │  * Uploads     │    <-- worker pools (parallel)
            │  * Deletes     │
            │  * Moves       │    <-- sequential (ordering constraints)
            │  * Conflicts   │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │   Baseline     │    <-- applies Outcomes atomically to DB
            │   Manager      │    <-- saves delta token in same transaction
            │                │    <-- optionally writes to change journal
            └────────────────┘
```

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `internal/graph/` handles all API quirks internally -- callers never see raw API data. `internal/graph/` does NOT import `internal/config/` -- callers pass token paths directly.

---

## 2. Package Layout

```
cmd/onedrive-go/                    # CLI (Cobra commands)
  main.go                           # Entry point
  root.go                           # Root command, global flags, config loading
  auth.go                           # login, logout, whoami
  files.go                          # ls, get, put, rm, mkdir, stat
  sync.go                           # sync (one-shot + watch)
  status.go                         # status, conflicts, resolve, verify
  format.go                         # Output formatting (human + JSON)
  drive.go                          # drive add, drive remove

internal/
  graph/                            # Graph API client -- ALL API interaction + quirk handling
    client.go                       # Client struct, HTTP transport, retry, rate limiting
    auth.go                         # Device code + browser PKCE flow, token refresh
    types.go                        # Clean types: Item, DeltaPage, Drive, User, UploadSession
    raw.go                          # Unexported rawDriveItem + JSON deserialization types
    normalize.go                    # All quirk handlers (driveID, deletion reorder, timestamps, etc.)
    errors.go                       # Sentinel errors (ErrGone, ErrNotFound, ErrThrottled, etc.)
    items.go                        # GetItem, ListChildren, CreateFolder, MoveItem, DeleteItem
    delta.go                        # Delta with pagination + normalization pipeline
    upload.go                       # Simple + chunked uploads
    download.go                     # Streaming downloads
    drives.go                       # Me, Drives, Drive

  sync/                             # Sync engine -- event-driven pipeline
    engine.go                       # Orchestrator (RunOnce, RunWatch, wiring)
    observer_remote.go              # Remote observer: delta fetch / polling -> ChangeEvent[]
    observer_local.go               # Local observer: FS walk / inotify -> ChangeEvent[]
    buffer.go                       # Change buffer: debounce, dedup, batch by path
    planner.go                      # PURE FUNCTION: events + baseline -> ActionPlan
    executor.go                     # Actions -> Outcomes, no DB writes
    baseline.go                     # Sole DB writer: Load, Commit, schema, migrations
    types.go                        # ChangeEvent, BaselineEntry, PathView, Outcome, etc.
    filter.go                       # Three-layer filtering (sync_paths, config, .odignore)
    transfer.go                     # Worker pools, bandwidth limiting, hash verification
    conflict.go                     # Conflict detection, resolution, keep-both logic

  config/                           # TOML config with drives
    config.go                       # Types, loading, validation
    paths.go                        # XDG paths, data dir derivation

pkg/
  quickxorhash/                     # Copied from rclone (BSD-0 license)
```

**Dependency rule**: `cmd/` -> `internal/*` -> `pkg/*`. No `internal/` package may import from `cmd/`. No `pkg/` package may import from `internal/`.

---

## 3. Component Responsibilities

### 3.1 Graph API Client (`internal/graph/`)

Handles ALL Microsoft Graph API communication -- authentication, CRUD operations, delta queries, upload sessions, download URLs. Also handles ALL API quirk normalization internally. Callers receive clean, consistent data and never need to worry about API inconsistencies.

`graph/` exposes **concrete types, not interfaces**:
- `graph.Client` is a concrete struct with methods for every API operation.
- `graph.Item` is the clean, normalized item type. All quirks (driveID casing, missing fields, timestamp validation, etc.) are handled before `Item` is returned to callers.
- No interfaces are exported from `graph/`.

### 3.2 Remote Observer (`observer_remote.go`)

Produces `[]ChangeEvent` from the Graph API. Two modes: `FullDelta` (one-shot) and `Watch` (continuous polling / future WebSocket).

**Key properties**:
- Output is `[]ChangeEvent` -- never writes to the database
- Path materialization uses the baseline (read-only) + in-flight parent tracking
- Normalization (driveID casing, missing fields, timestamps) happens here
- Within each delta page, deletions are buffered and processed before creations (known API reordering bug)
- HTTP 410 (expired delta token) returns a sentinel error; engine restarts with full delta

See [event-driven-rationale.md](event-driven-rationale.md) Part 5.1 for full implementation details and API quirk handling table.

### 3.3 Local Observer (`observer_local.go`)

Produces `[]ChangeEvent` from the filesystem. Two modes: `FullScan` (one-shot) and `Watch` (inotify/FSEvents via `rjeczalik/notify`).

**Key properties**:
- Output is `[]ChangeEvent` -- never writes to the database
- Local events have no `ItemID` field -- local observations are keyed by path
- Compares against in-memory baseline, not DB queries
- `.nosync` guard checked before any work (S2 safety)
- Racily-clean guard: same-second mtime triggers hash verification
- Local deletion detection by diffing observed paths against baseline
- Dual-path threading: `fsRelPath` (filesystem I/O) and `dbRelPath` (NFC-normalized for baseline lookup)

See [event-driven-rationale.md](event-driven-rationale.md) Part 5.2 for full implementation details.

### 3.4 Change Buffer (`buffer.go`)

Collects events from both observers, deduplicates, debounces, and produces batches grouped by path.

**Key properties**:
- Thread-safe (mutex-protected)
- Debounce window (default 2 seconds) prevents processing the same file multiple times during rapid edits
- Groups events by path so the planner sees the full picture per path (`PathChanges`)
- Move events are dual-keyed: stored at the new path AND a synthetic delete at the old path, ensuring the planner sees both paths
- `FlushImmediate` for one-shot mode (no debounce wait)

### 3.5 Planner (`planner.go`) -- Pure Function

The intellectual core of the sync engine. Takes change events + baseline and produces an `ActionPlan`. Composed entirely of pure functions -- no I/O, no database access.

```
Plan(changes []PathChanges, baseline *Baseline, mode SyncMode, config *SafetyConfig) -> (*ActionPlan, error)
```

**Pipeline within the planner**:
1. Build `PathView` values from changes + baseline
2. Detect moves (remote: from ChangeMove events; local: hash-based correlation)
3. Classify each PathView using the decision matrix (EF1-EF14 for files, ED1-ED8 for folders)
4. Apply filters symmetrically to both remote and local items
5. Order the plan (folder creates before files, depth-first for deletes)
6. Safety checks (big-delete, etc.) as pure functions on ActionPlan + Baseline

**File decision matrix:**

| E# | Local | Remote | Baseline | Action |
|----|-------|--------|----------|--------|
| EF1 | unchanged | unchanged | exists | no-op |
| EF2 | unchanged | changed | exists | download |
| EF3 | changed | unchanged | exists | upload |
| EF4 | changed | changed (same hash) | exists | update synced (convergent edit) |
| EF5 | changed | changed (diff hash) | exists | **conflict** (edit-edit) |
| EF6 | deleted | unchanged | exists | remote delete |
| EF7 | deleted | changed | exists | download (remote wins) |
| EF8 | unchanged | deleted | exists | local delete |
| EF9 | changed | deleted | exists | **conflict** (edit-delete) |
| EF10 | deleted | deleted | exists | cleanup (both gone) |
| EF11 | new | new (same hash) | none | update synced (convergent create) |
| EF12 | new | new (diff hash) | none | **conflict** (create-create) |
| EF13 | new | absent | none | upload |
| EF14 | absent | new | none | download |

**Folder decision matrix:**

| E# | Local | Remote | Baseline | Action |
|----|-------|--------|----------|--------|
| ED1 | exists | exists | exists | no-op |
| ED2 | exists | exists | none | adopt (update synced) |
| ED3 | absent | exists | none | create locally |
| ED4 | absent | exists | exists | recreate locally |
| ED5 | exists | absent | none | create remotely |
| ED6 | exists | deleted | exists | delete locally |
| ED7 | absent | deleted | exists | cleanup |
| ED8 | absent | absent | exists | cleanup |

Folders use existence-based reconciliation: `Baseline != nil` means "was synced." Folder reconciliation requires no hash check because folder identity is determined by path and presence in the baseline.

**Change detection** uses per-side baselines for SharePoint enrichment correctness:
- `detectLocalChange`: compares `Local.Hash` against `Baseline.LocalHash`
- `detectRemoteChange`: compares `Remote.Hash` against `Baseline.RemoteHash`

See [event-driven-rationale.md](event-driven-rationale.md) Parts 5.4-5.7 for full implementation details.

### 3.6 Executor (`executor.go`)

Takes an `ActionPlan` and executes it against the API and filesystem. Produces `[]Outcome` -- never writes to the database.

**Execution phases** (ordered for correctness):
1. Folder creates (sequential, top-down by depth)
2. Moves (sequential -- ordering matters for nested moves)
3. Downloads (parallel via worker pool)
4. Uploads (parallel via worker pool)
5. Local deletes (sequential, files first, then folders bottom-up)
6. Remote deletes (sequential)
7. Conflicts (sequential)
8. Synced updates (batch -- no I/O)
9. Cleanups (batch)

**Key properties**:
- Database writes happen only in the BaselineManager after execution completes, cleanly separating execution from persistence
- Workers collect Outcomes via a mutex-protected callback
- Each Outcome is self-contained: has everything the baseline manager needs
- Retries happen INSIDE the executor with exponential backoff before producing the final Outcome

**Download safety**: `.partial` file -> stream with `TeeReader` hash -> verify QuickXorHash -> set timestamps -> atomic rename.

**Upload strategy**: Files <=4 MB use simple PUT. Files >4 MB use resumable sessions with 320 KiB-aligned chunks. `fileSystemInfo` included in session creation to avoid double-versioning on Business/SharePoint.

See [event-driven-rationale.md](event-driven-rationale.md) Part 5.8 for full implementation details.

### 3.7 Baseline Manager (`baseline.go`)

The **sole writer** to the database. Loads the baseline at cycle start and commits outcomes at cycle end.

**Key properties**:
- Single writer -- all database concurrency concerns are structurally avoided
- Atomic transaction: all outcomes + delta token commit together or none do
- After commit, the in-memory baseline cache is refreshed for the next cycle
- Delta token is NEVER saved separately from the baseline

**Operations**:
- `Load()`: Reads entire baseline table into memory (`Baseline.ByPath` + `Baseline.ByID` maps)
- `Commit(outcomes, deltaToken, driveID)`: Applies all successful outcomes and saves delta token in a single transaction
- `GetDeltaToken()`: Returns saved delta token for a drive

See [event-driven-rationale.md](event-driven-rationale.md) Part 5.9 for full implementation details.

### 3.8 Config (`internal/config/`)

Loads, validates, and provides access to TOML configuration. Manages multi-drive configuration with flat global settings and per-drive sections. See [configuration.md](configuration.md) for full specification.

**Key types**: `Config`, `Drive`, `ResolvedDrive`, `ResolveDrive(env, cli)`, `DriveTokenPath()`, `DriveStatePath()`.

### 3.9 QuickXorHash (`pkg/quickxorhash/`)

QuickXorHash algorithm implementation. Leaf utility with zero project dependencies. Copied from rclone under BSD-0 license.

---

## 4. Type System

Each pipeline stage works with its own types. No shared `Item` struct. The type system makes it impossible to confuse remote, local, and baseline data at compile time.

### 4.1 ChangeEvent

Immutable observation of a change. Produced by observers, consumed by the change buffer and planner. Never stored in the database (except optionally in the change journal for debugging).

```go
type ChangeEvent struct {
    Source    ChangeSource   // SourceRemote or SourceLocal
    Type     ChangeType     // ChangeCreate, ChangeModify, ChangeDelete, ChangeMove
    Path     string         // NFC-normalized, relative to sync root
    OldPath  string         // for moves only
    ItemID   string         // server-assigned (remote only; empty for local)
    ParentID string         // server parent ID (remote only)
    DriveID  string         // normalized (lowercase, zero-padded to 16 chars)
    ItemType ItemType       // file, folder, root
    Name     string         // URL-decoded, NFC-normalized
    Size     int64
    Hash     string         // QuickXorHash (base64); empty for folders
    Mtime    int64          // Unix nanoseconds
    ETag     string         // remote only
    CTag     string         // remote only
    IsDeleted bool          // true for remote deletion events
}
```

### 4.2 BaselineEntry

The confirmed synced state of a single path. The ONLY durable per-item state in the system.

```go
type BaselineEntry struct {
    Path       string
    DriveID    string
    ItemID     string
    ParentID   string
    ItemType   ItemType
    LocalHash  string    // per-side: handles SharePoint enrichment natively
    RemoteHash string    // for normal files, LocalHash == RemoteHash
    Size       int64
    Mtime      int64     // local mtime at sync time
    SyncedAt   int64     // when this entry was last confirmed synced
    ETag       string
}
```

### 4.3 PathView

Unified three-way view of a single path. Constructed by the planner from change events + baseline. Input to the reconciliation decision matrix.

```go
type PathView struct {
    Path     string
    Remote   *RemoteState    // nil = no remote change observed
    Local    *LocalState     // nil = no local change observed / locally deleted
    Baseline *BaselineEntry  // nil = never synced
}

type RemoteState struct {
    ItemID, ParentID, Name string
    ItemType               ItemType
    Size                   int64
    Hash                   string
    Mtime                  int64
    ETag, CTag             string
    IsDeleted              bool
}

type LocalState struct {
    Name     string
    ItemType ItemType
    Size     int64
    Hash     string
    Mtime    int64
}
```

### 4.4 Outcome

Result of executing a single action. Self-contained -- has everything the baseline manager needs to update the database. No DB reads required.

```go
type Outcome struct {
    Action       ActionType
    Success      bool
    Error        error
    Path         string
    OldPath      string     // for moves
    DriveID      string
    ItemID       string     // from API response after upload
    ParentID     string
    ItemType     ItemType
    LocalHash    string
    RemoteHash   string
    Size         int64
    Mtime        int64      // local mtime at sync time
    ETag         string
    ConflictType string     // "edit_edit", "edit_delete", "create_create" (conflicts only)
}
```

### 4.5 Action Types and ActionPlan

```go
type ActionType int

const (
    ActionDownload ActionType = iota
    ActionUpload
    ActionLocalDelete
    ActionRemoteDelete
    ActionLocalMove
    ActionRemoteMove
    ActionFolderCreate
    ActionConflict
    ActionUpdateSynced
    ActionCleanup
)

type Action struct {
    Type         ActionType
    DriveID      string
    ItemID       string
    Path         string
    NewPath      string           // for moves
    CreateSide   FolderCreateSide // for folder creates
    View         *PathView        // full three-way context
    ConflictInfo *ConflictRecord
}

type ActionPlan struct {
    FolderCreates []Action
    Moves         []Action
    Downloads     []Action
    Uploads       []Action
    LocalDeletes  []Action
    RemoteDeletes []Action
    Conflicts     []Action
    SyncedUpdates []Action
    Cleanups      []Action
}
```

Note: `Action.View` carries the full `PathView` (three-way context) so the executor has complete information about remote, local, and baseline state without querying the database.

### 4.6 Consumer-Defined Interfaces (Graph Client)

Defined in `sync/`, satisfied by `*graph.Client`:

```go
type DeltaFetcher interface {
    Delta(ctx context.Context, driveID, token string) (*graph.DeltaPage, error)
}

type ItemClient interface {
    GetItem(ctx context.Context, driveID, itemID string) (*graph.Item, error)
    ListChildren(ctx context.Context, driveID, itemID string) ([]graph.Item, error)
    CreateFolder(ctx context.Context, driveID, parentID, name string) (*graph.Item, error)
    MoveItem(ctx context.Context, driveID, itemID, newParentID, newName string) (*graph.Item, error)
    DeleteItem(ctx context.Context, driveID, itemID string) error
}

type TransferClient interface {
    Download(ctx context.Context, driveID, itemID string, w io.Writer) (int64, error)
    SimpleUpload(ctx context.Context, driveID, parentID, name string, r io.Reader, size int64) (*graph.Item, error)
    CreateUploadSession(ctx context.Context, driveID, parentID, name string, size int64, mtime time.Time) (*graph.UploadSession, error)
    UploadChunk(ctx context.Context, session *graph.UploadSession, chunk io.Reader, offset, length, total int64) (*graph.Item, error)
}
```

---

## 5. Data Flow

### 5.1 CLI File Operations (Bypass Sync)

File operations (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`) are completely independent of the sync engine. Direct API calls through `internal/graph/`, no database interaction.

```
cmd/onedrive-go/  -->  graph.Client  -->  Microsoft Graph API
                            |
                            v
                      []graph.Item (clean, normalized)
                            |
                            v
                    cmd/ formats and prints
```

### 5.2 One-Shot Sync

```
1. BaselineManager.Load()           -> Baseline (from DB, cached in memory)
2. RemoteObserver.FullDelta()       -> []ChangeEvent (remote)
3. LocalObserver.FullScan()         -> []ChangeEvent (local)
4. ChangeBuffer.AddAll() + Flush()  -> []PathChanges (batched by path)
5. Planner.Plan()                   -> ActionPlan (pure function)
6. Executor.Execute()               -> []Outcome (I/O)
7. BaselineManager.Commit()         -> atomic DB transaction
```

### 5.3 Watch Mode

```
1. BaselineManager.Load()           -> Baseline (from DB, cached in memory)
2. RemoteObserver.Watch()           -> streams ChangeEvents (poll/WebSocket)
3. LocalObserver.Watch()            -> streams ChangeEvents (inotify/FSEvents)
4. ChangeBuffer debounces (2s)      -> []PathChanges (only changed paths)
5. Planner.Plan()                   -> ActionPlan (only for changed paths)
6. Executor.Execute()               -> []Outcome
7. BaselineManager.Commit()         -> incremental baseline update
8. Go to step 4 (loop on buffer ready)
```

### 5.4 Dry-Run (Zero Side Effects)

```
Steps 1-5: Same as one-shot
Step 6: STOP. Print ActionPlan. No Execute, no Commit. Zero side effects.
```

No database writes occur before the executor runs, so dry-run has genuinely zero side effects.

### 5.5 Pause/Resume

```
Pause:
  - Observers continue running (collecting events)
  - ChangeBuffer continues accepting events
  - Planner/Executor do NOT run
  - Events accumulate in the buffer

Resume:
  - Flush buffer (potentially large batch)
  - Plan + Execute + Commit
  - Normal watch loop resumes

High-water mark: if buffer exceeds 100K events during pause,
collapse to "full sync on resume" flag.
```

### 5.6 Initial Sync (Batched)

On first run, no delta token exists. The delta API returns every item. For large drives, batch processing bounds memory:

```
1. Fetch delta page by page
2. Every 50K items:
   a. Flush buffer
   b. Plan (only these items)
   c. Execute (downloads/uploads)
   d. Commit partial baseline + intermediate delta token
3. After all pages: commit final delta token
```

Memory bounded to ~50 MB even for 500K-item drives. For maximum speed, parallel `/children` enumeration is available as an alternative to sequential delta pagination.

---

## 6. Concurrency Model

### 6.1 Database Writer

**Sole writer**: Only the `BaselineManager` writes to the database, running sequentially after execution completes. No concurrent write contention during sync.

**Concurrent readers**: SQLite WAL mode enables `status`, `conflicts`, and `verify` commands to read while sync writes.

### 6.2 Worker Pools

Three independent worker pools for parallel transfers:

| Pool | Default Workers | Purpose |
|------|----------------|---------|
| Downloads | 8 | File downloads from OneDrive |
| Uploads | 8 | File uploads to OneDrive |
| Checkers | 8 | Local hash computation for change detection |

Worker pools use `errgroup` for goroutine management. Each worker gets its own context derived from the sync run's root context.

### 6.3 Context Tree

One root context per sync run. Cancellation propagates to all stages:

```
rootCtx
|-- remoteObserverCtx
|-- localObserverCtx
|-- executorCtx
    |-- downloadWorker[0..N]
    |-- uploadWorker[0..N]
    +-- checkWorker[0..N]
```

### 6.4 Graceful Shutdown

- **First signal** (SIGINT/SIGTERM): Cancel root context. In-flight transfers finish up to a configurable timeout. Baseline + delta token checkpoint saved. Exit cleanly.
- **Second signal**: Immediate cancellation. No checkpoint save. SQLite WAL ensures DB consistency.
- **SIGHUP**: Reload configuration. Re-initialize filter engine and bandwidth limiter. Detect stale files from filter changes. Continue running.

---

## 7. State Management

### 7.1 Database Engine

- **SQLite** via `modernc.org/sqlite` (pure Go, no CGO dependency)
- **WAL mode** for concurrent readers + single writer
- **FULL synchronous** -- durability on crash
- **Separate database file per drive** -- complete isolation between accounts

### 7.2 Baseline Table

The only mutable per-item state in the system. 11 columns storing the confirmed synced state of each item.

```sql
CREATE TABLE baseline (
    path            TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash      TEXT,
    remote_hash     TEXT,
    size            INTEGER,
    mtime           INTEGER,
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),
    etag            TEXT
);

CREATE UNIQUE INDEX idx_baseline_item ON baseline(drive_id, item_id);
CREATE INDEX idx_baseline_parent ON baseline(parent_id);
```

The baseline stores confirmed synced state. Each row represents a live, synced item. Deletions remove the baseline row -- remote deletion events are ephemeral observations processed in the pipeline. Local observations are keyed by path; the baseline maps paths to server item IDs. The `mtime` column stores the local mtime at sync time; per-side mtimes are ephemeral (in change events).

Per-side hashes (`local_hash`, `remote_hash`) handle SharePoint enrichment natively: after enrichment, local and remote hashes diverge, and both are recorded. No infinite re-upload loop.

### 7.3 Supporting Tables

| Table | Purpose | Writer |
|---|---|---|
| `delta_tokens` | Delta cursor per drive | BaselineManager (same txn as baseline) |
| `conflicts` | Conflict ledger with resolution tracking | BaselineManager |
| `stale_files` | Filter-change tracking | BaselineManager |
| `upload_sessions` | Crash recovery for large uploads | Executor (pre-upload) + BaselineManager (post-upload) |
| `change_journal` | Debugging audit trail (optional, append-only) | BaselineManager |
| `config_snapshots` | Filter change detection | Engine (on config load) |
| `schema_migrations` | Schema version tracking | Engine (on startup) |

**Critical property**: The delta token is saved in the same transaction as baseline updates. Never separately. If the process crashes after execution but before commit, the token is not advanced and the same delta is re-fetched (idempotent).

See [data-model.md](data-model.md) for complete schema definitions.

### 7.4 Move Detection

**Remote moves**: The Remote Observer detects moves during `convertToChangeEvent` by comparing the delta item's location against `baseline.ByID`. The baseline entry at the old path provides the "before" view naturally because it has not been updated yet.

**Local moves**: The Planner correlates a locally-deleted baseline item whose `LocalHash` matches a locally-created new item's hash (unique match constraint). Combined into a single `ActionRemoteMove`.

The baseline serves as the "old state" for move detection -- it is read-only during observation and planning, so it naturally preserves the pre-move location of every item.

### 7.5 Crash Recovery

| Crash Point | Recovery |
|---|---|
| During Load/FetchRemote/ScanLocal/Plan | No state changed. Re-run cycle. |
| During Execute | Some actions completed on disk/API. Baseline NOT updated. Delta token NOT advanced. Re-run cycle: Local Observer sees completed downloads, planner produces EF4/EF11 (convergent -> update synced). No duplicate downloads. |
| During Commit | SQLite transaction: either commits or rolls back. If rolled back: same as "during Execute." |
| During Watch (between cycles) | Events re-observed by watchers. Debounce/dedup handles redundancy. |

Upload sessions are persisted BEFORE upload begins. On crash recovery: check expiry, resume valid sessions, delete expired ones.

---

## 8. Error Handling

### 8.1 Four-Tier Classification

| Tier | Examples | Response |
|------|----------|----------|
| **Fatal** | Auth failure, DB corruption, impossible state | Stop entire sync, alert user, exit non-zero |
| **Retryable** | Network timeout, HTTP 429/500/503/504 | Exponential backoff + jitter + Retry-After, max 5 retries |
| **Skip** | Permission denied on single file, invalid filename, locked (423) | Log warning, skip item, continue sync |
| **Deferred** | Parent dir not yet created, file locked locally | Queue for retry at end of current cycle |

Retries happen INSIDE the executor before producing the final Outcome. A failed Outcome means "gave up after max retries."

### 8.2 Error Flow

```
Observer error  -> abort cycle (delta/scan failure; zero side effects)
Planner error   -> abort cycle (impossible state; pure function, should never occur)
Executor error  -> per-item Outcome with Success=false (item retried next cycle)
Commit error    -> abort cycle (DB failure; delta token not advanced)
```

### 8.3 Safety Invariants (S1-S7)

| ID | Invariant | Implementation |
|----|-----------|----------------|
| **S1** | Never delete remote on local absence without synced base | Planner checks `view.Baseline != nil` before emitting `ActionRemoteDelete`. Structurally enforced: `PathView.Baseline == nil` means "never synced." |
| **S2** | Never process deletions from incomplete enumeration | Remote Observer returns delta token alongside events. Token only passed to Commit after execution. `.nosync` guard fires in Local Observer before events produced. |
| **S3** | Atomic file writes for downloads | `.partial` + hash verify + atomic rename. Outcome contains verified hash. |
| **S4** | Hash-before-delete guard | Executor computes current local hash, compares against `action.View.Baseline.LocalHash`. If hashes differ, creates conflict copy. |
| **S5** | Big-delete protection | Planner counts delete actions vs `len(baseline.ByPath)`. Pure function: `bigDeleteTriggered(plan, baseline, config) bool`. |
| **S6** | Disk space check before downloads | Executor checks available space against `config.MinFreeSpace` (default 1 GB). Insufficient space produces failed Outcome. |
| **S7** | Never upload partial/temp files | Local Observer's filter excludes `.partial`, `.tmp`, `.swp`, etc. Planner applies filters symmetrically to both local and remote items. |

S1 and S5 are pure functions in the planner, testable without a database or any I/O.

### 8.4 HTTP Error Handling

| Status Code | Classification | Action |
|-------------|---------------|--------|
| 400 | Skip | Invalid request |
| 401 | Fatal (after token refresh) | Auth failure |
| 403 | Skip | Permission denied, retention policy |
| 404 | Skip | Item no longer exists |
| 408 | Retryable | Timeout |
| 410 | Special | Delta token expired -- full re-enumeration |
| 412 | Retryable | eTag stale, refresh and retry |
| 423 | Skip | File locked (SharePoint) |
| 429 | Retryable | Rate limited, use Retry-After header |
| 500-504 | Retryable | Server error |
| 507 | Fatal | Insufficient storage on server |
| 509 | Retryable (long backoff) | Bandwidth exceeded (SharePoint) |

---

## 9. API Quirk Normalization

All known API quirks are handled at the observer boundary, making them invisible to the Planner and Executor. This follows the `internal/graph/` pattern where callers never see raw API data.

| Quirk | Handling |
|-------|----------|
| driveId casing inconsistency | `strings.ToLower()` + zero-pad to 16 chars on every driveId |
| Deletions after creations at same path | Buffer full delta page, process deletions before creations |
| Missing `name` on deleted items (Business) | Look up from baseline by ItemID |
| Missing `size` on deleted items (Personal) | Look up from baseline by ItemID |
| `parentReference.path` absent in delta | Reconstruct from parent chain via baseline + in-flight parent map |
| URL-encoded spaces in paths | URL-decode all path fields |
| Items appearing multiple times in delta | Keep last occurrence per item ID |
| iOS `.heic` hash mismatch | Known API bug -- log warning, mark as known-unreliable |
| SharePoint post-upload enrichment | Per-side hash baselines (`local_hash`, `remote_hash`). See [sharepoint-enrichment.md](sharepoint-enrichment.md) |
| HTTP 410 delta token expired | Handle both resync types based on response body |
| Zero-byte file hashes | Simple upload only, skip hash verification |
| Invalid timestamps | Validate on ingestion, fall back to current UTC |
| NFC/NFD Unicode normalization | `norm.NFC.String()` on all paths for macOS APFS compatibility |
| Upload fragment alignment | Enforce 320 KiB multiples |
| Double-versioning on Business/SharePoint | Include `fileSystemInfo` in upload session creation |
| `Prefer` header for Personal delta | Include `Prefer: deltashowremoteitemsaliasid` in all Personal delta requests |
| Upload session resume | Query session status endpoint for accepted byte ranges; handle HTTP 416 |
| SharePoint file lock check | Check lock status before upload; HTTP 423 = skip |
| OneNote package items | Detect via `package` facet, skip entirely |
| National Cloud delta unsupported | Fall back to `/children` enumeration |

---

## 10. Security Model

### 10.1 Token Storage

- **Separate token file per drive**: `~/.local/share/onedrive-go/token_{type}_{email}.json` (Linux) or `~/Library/Application Support/onedrive-go/token_{type}_{email}.json` (macOS)
- File permissions: `0600` (owner read/write only)
- Keychain integration: post-MVP

### 10.2 Logging Safety

- Bearer tokens scrubbed from all log output including debug level
- Pre-authenticated download/upload URLs truncated in logs
- No secrets in structured log fields

### 10.3 Transfer Verification

- **Downloads**: Always verify QuickXorHash. Exception: iOS `.heic` (known API bug, warning only).
- **Uploads**: Compare local hash vs server response hash. SharePoint enrichment divergence stored as per-side baselines.
- **Streaming hash**: `io.TeeReader` computes hash during transfer (no second pass).

---

## 11. CLI and Process Model

### 11.1 Framework

- **CLI framework**: spf13/cobra
- **Module path**: `github.com/tonimelisma/onedrive-go`
- **Go version**: 1.24+
- **Binary name**: `onedrive-go`

### 11.2 Commands

| Command | Description | Sync DB? | API? |
|---------|-------------|----------|------|
| `ls [path]` | List files and folders | No | Yes |
| `get <remote> [local]` | Download file or folder | No | Yes |
| `put <local> [remote]` | Upload file or folder | No | Yes |
| `rm <path>` | Delete (to recycle bin by default) | No | Yes |
| `mkdir <path>` | Create folder | No | Yes |
| `stat <path>` | Display file/folder metadata | No | Yes |
| `sync` | One-shot or continuous sync | Yes (write) | Yes |
| `status` | Sync state and pending changes | Yes (read) | No |
| `conflicts` | List unresolved conflicts | Yes (read) | No |
| `resolve <id\|path>` | Resolve a conflict | Yes (write) | Maybe |
| `verify` | Full-tree hash verification | Yes (read) | Yes |
| `login` | Authenticate (device code + browser PKCE) | No | Yes |
| `logout` | Clear credentials | No | No |
| `whoami` | Display authenticated user and drive info | No | Yes |

### 11.3 Global Flags

```
--account <id>         # Account for auth commands
--drive <selector>     # Drive selector (canonical ID, alias, or partial match)
--config <path>        # Override config file location
--json                 # Machine-readable JSON output (all commands)
--verbose / -v         # Info-level output
--debug                # Debug-level output
--quiet / -q           # Suppress non-error output
--dry-run              # Preview operations without executing
```

### 11.4 Process Model

- SQLite lock enforces single sync writer per drive
- `status`, `conflicts`, `verify` can run concurrently with sync via WAL
- File operations (`ls`, `get`, `put`, etc.) are completely independent -- no database, no lock contention
- `sync --watch` is just sync that keeps running -- no separate daemon concept

### 11.5 Filtering

Three-layer monotonic exclusion (each layer can only exclude more, never include back):

```
Item path
  |
  v
1. sync_paths allowlist     If set, only these paths. Everything else excluded.
  |
  v
2. Config patterns          skip_files, skip_dirs, skip_dotfiles, max_file_size
  |
  v
3. .odignore marker files   Per-directory, gitignore-style patterns
  |
  v
INCLUDED
```

The filter is applied in the Planner to both remote and local items symmetrically. When filter rules change, previously-included files that are now excluded are tracked in the stale files ledger (user nagged, never auto-deleted).

---

## Appendix A: Memory Budget

### One-Shot Mode (100K items, initial sync)

| Component | Count | Memory |
|---|---|---|
| Baseline (empty first run) | 0 | 0 MB |
| Remote events | 100,000 | ~27 MB |
| PathViews + RemoteState | 100,000 | ~21 MB |
| Action plan | 100,000 | ~5 MB |
| Outcomes | 100,000 | ~24 MB |
| **Peak** | | **~77 MB** |

Within PRD budget of < 100 MB for 100K files. Batch processing (50K-item batches with intermediate commits) bounds memory to ~50 MB even for 500K-item drives.

### Steady-State (100K items)

| Component | Count | Memory |
|---|---|---|
| Baseline (cached) | 100,000 | ~19 MB |
| Delta events (changes only) | ~100 | ~0.03 MB |
| Local events (changes only) | ~50 | ~0.01 MB |
| **Peak** | | **~20 MB** |

### Watch Mode (100K items)

Sustained ~20 MB. Processes individual change batches, not full snapshots. Memory proportional to cached baseline, not pending changes.

---

## Appendix B: Decision Summary

| # | Decision | Rationale |
|---|---|---|
| E1 | Events are the coordination mechanism, not the database | Supports both one-shot and continuous watch mode. Clean separation between observation, planning, and execution. |
| E2 | Baseline is the only durable per-item state | Remote/local observations are ephemeral. Only confirmed sync state needs persistence. |
| E3 | Delta token commits with baseline (same transaction) | Prevents token-advancement-without-execution crash bug. |
| E4 | Per-side hash baselines (`local_hash`, `remote_hash`) | Handles SharePoint enrichment natively without special code paths. |
| E5 | Local observations keyed by path | Baseline maps paths to server IDs. Clean separation of local filesystem identity from server identity. |
| E6 | Deletions remove the baseline row | Baseline stores confirmed synced state. Deletion events are ephemeral observations. |
| E7 | Folders use existence-based reconciliation | `Baseline != nil` = was synced. Accurate reconciliation for folders without requiring hash checks. |
| E8 | Planner is pure functions | No I/O, no DB. Maximum testability. Every decision deterministic. |
| E9 | Executor produces Outcomes, not DB writes | Clean separation of execution from persistence. Enables atomic commit. |
| E10 | Change buffer with debounce | Prevents duplicate processing during rapid edits. Groups by path. |
| E11 | Same code path for one-shot and watch | One-shot = one big batch. Watch = many small batches. Same pipeline. |
| E12 | Change journal is append-only and optional | Debugging aid, not functional requirement. |
| E13 | Safety checks are pure functions in the planner | S1-S7 on ActionPlan + Baseline, not DB queries. |
| E14 | Action carries PathView context | Executor has full three-way context without querying DB. |
| E15 | DriveID normalization at observer boundary | Lowercase + zero-pad at API boundary. Downstream never sees inconsistent casing. |
| E16 | Retries inside executor before producing Outcome | Exponential backoff within executor. Failed Outcome = gave up after max retries. |
| E17 | Baseline serves as "old state" for move detection | Baseline entry at old path provides "before" view naturally during read-only observation. |
| E18 | Batch processing for large initial syncs | 50K-item batches with intermediate commits. Bounds memory to ~50 MB. |
| E19 | Two-signal graceful shutdown | First: drain + checkpoint. Second: immediate exit. WAL ensures consistency. |
| E20 | Filter applied in Planner, not observers | Symmetric filtering of remote AND local items. Hot-reloadable without restarting observers. |
| E21 | Conflict copies use timestamp naming | `file.conflict-YYYYMMDD-HHMMSS.ext`. Self-documenting, shorter than hostname-based. |
