# Architectural Strategy: Alternative E — Event-Driven Architecture with Change Journal

**Date**: 2026-02-22
**Decision**: Alternative E selected as the long-term target architecture.
**Rationale**: No users, no launch, unlimited engineering effort. Build for the long-term product vision. Mission-critical data management — one chance to get it right.

---

## Part 1: First Principles

### 1.1 Why Not A, B, C, or D?

| Alternative | What it is | Why it's not enough |
|---|---|---|
| **A: Surgical Repair** | Patch the seams between modules | Preserves the root cause: shared mutable database as coordination mechanism. `local:` ID lifecycle remains. Dual-row class is mitigated, not eliminated. |
| **B: Three-Table Split** | Split the `items` table into `remote_state`, `local_state`, `synced_baseline` | Still uses the database as the coordination mechanism. Delta and scanner still write eagerly. Dry-run still has side effects. Three tables means three writers fighting for SQLite. |
| **C: Deferred Persistence** | In-memory reconciliation with deferred writes | Good data flow, but no answer for continuous mode (`sync --watch`). Rebuilds full snapshots on every trigger. Not designed for incremental processing. |
| **D: Pure Snapshot Pipeline** | Sync cycle as a pure function: `(Baseline, API, FS) → NewBaseline` | Correct for one-shot mode. But `sync --watch` (the PRD's "what you put in a systemd unit file") would require rebuilding full snapshots on every inotify event. Wasteful and high-latency for the continuous mode that production users will run 24/7. |

### 1.2 What the Product Vision Demands

From the PRD:

- `sync --watch` is the primary runtime mode — "same binary, same code path" as one-shot
- inotify/FSEvents for near-instant local change detection
- WebSocket subscription for near-instant remote change detection
- `< 1% CPU when idle` — the #1 abraunegg complaint
- `< 100 MB memory for 100K files`
- Crash recovery with zero data loss
- Pause/resume that continues tracking changes while paused

An architecture designed for batch processing (A through D) treats `--watch` as "run one-shot in a loop." An architecture designed for event-driven processing treats one-shot as "collect all events, then process them as a batch." The latter is the correct generalization.

### 1.3 The Six Fault Lines (Recap)

All six trace to one root cause: **the database is the coordination mechanism between pipeline stages**.

1. **Tombstone split**: Scanner and delta fight over shared mutable DB rows
2. **`local:` ID lifecycle**: Scanner invents fake IDs because it writes to an item_id-keyed table
3. **SQLITE_BUSY**: Concurrent workers write to DB during execution
4. **Incomplete folder lifecycle**: `isSynced()` depends on hash fields that folders don't have
5. **Pipeline phase ordering**: Intermediate DB writes create ordering dependencies
6. **Asymmetric filter application**: Filter only runs in the scanner, not on remote items

Alternative E eliminates all six by removing the database from the coordination path entirely.

### 1.4 Industry Precedent

| System | Architecture | Key insight |
|---|---|---|
| **Dropbox Nucleus** | Three-tree model with Server File Journal (append-only log) | State is derived from an event log. The planner converges three trees incrementally. |
| **Syncthing** | Version vectors + filesystem watcher + periodic rescan | Continuous mode is the primary mode. Events drive processing. |
| **Git** | Content-addressable objects + index as snapshot | The index (baseline) is the only mutable state. Everything else is derived. |
| **Database replication** | WAL-based: replicas apply a log of changes | Changes are events. State is the result of applying events to a baseline. |

Alternative E draws from all four: Dropbox's event journal, Syncthing's continuous observation model, Git's baseline-only persistence, and database replication's event-driven updates.

---

## Part 2: Architecture Overview

### 2.1 Component Diagram

```
┌──────────────────┐    ┌──────────────────┐
│  Remote Observer │    │  Local Observer   │
│                  │    │                   │
│  • Delta fetch   │    │  • FS walk        │
│  • WebSocket     │    │  • inotify/FSE    │
│  • Polling       │    │  • Hash compute   │
└────────┬─────────┘    └────────┬──────────┘
         │ ChangeEvent            │ ChangeEvent
         └───────────┬────────────┘
                     ▼
            ┌────────────────┐
            │  Change Buffer │
            │                │
            │  • Debounce    │
            │  • Dedup       │
            │  • Batch       │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │    Planner     │    ← reads Baseline (from DB, cached in memory)
            │                │    ← reads Change Events
            │  • Merge       │    → produces ActionPlan
            │  • Reconcile   │
            │  • Filter      │    ← pure functions, no I/O
            │  • Safety      │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │   Executor     │    ← executes actions against API + filesystem
            │                │    → produces Outcomes
            │  • Downloads   │
            │  • Uploads     │    ← worker pools (parallel)
            │  • Deletes     │
            │  • Moves       │    ← sequential (ordering constraints)
            │  • Conflicts   │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │   Baseline     │    ← applies Outcomes atomically to DB
            │   Manager      │    ← saves delta token in same transaction
            │                │    ← optionally writes to change journal
            └────────────────┘
```

### 2.2 Data Flow: One-Shot Mode

```
1. BaselineManager.Load()           → Baseline (from DB, cached in memory)
2. RemoteObserver.FullDelta()       → []ChangeEvent (remote)
3. LocalObserver.FullScan()         → []ChangeEvent (local)
4. ChangeBuffer.AddAll() + Flush()  → []PathChanges (batched by path)
5. Planner.Plan()                   → ActionPlan (pure function)
6. Executor.Execute()               → []Outcome (I/O)
7. BaselineManager.Commit()         → atomic DB transaction
```

### 2.3 Data Flow: Watch Mode

```
1. BaselineManager.Load()           → Baseline (from DB, cached in memory)
2. RemoteObserver.Watch()           → streams ChangeEvents (WebSocket/poll)
3. LocalObserver.Watch()            → streams ChangeEvents (inotify/FSEvents)
4. ChangeBuffer debounces (2s)      → []PathChanges (only changed paths)
5. Planner.Plan()                   → ActionPlan (only for changed paths)
6. Executor.Execute()               → []Outcome
7. BaselineManager.Commit()         → incremental baseline update
8. Go to step 4 (loop on buffer ready)
```

### 2.4 Data Flow: Dry-Run

```
1-5. Same as one-shot
6. STOP. Print ActionPlan. No Execute, no Commit. Zero side effects.
```

### 2.5 Data Flow: Pause/Resume

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
```

This matches the PRD requirement: "When paused, the process stays alive and continues tracking changes. On resume, it has a complete picture of what changed and syncs efficiently."

---

## Part 3: Type System

### 3.1 Design Principle

Each pipeline stage works with its own types. No shared `Item` struct. The type system makes it **impossible** to confuse remote, local, and baseline data at compile time.

### 3.2 Change Events

```go
// ChangeSource identifies where an observation came from.
type ChangeSource int

const (
    SourceRemote ChangeSource = iota  // from delta API or WebSocket push
    SourceLocal                       // from filesystem watcher or full scan
)

// ChangeType classifies the nature of the observed change.
type ChangeType int

const (
    ChangeCreate ChangeType = iota
    ChangeModify
    ChangeDelete
    ChangeMove
)

// ChangeEvent is an immutable observation of a change. Produced by observers,
// consumed by the change buffer and planner. Never stored in the database
// (except optionally in the change journal for debugging).
type ChangeEvent struct {
    Source    ChangeSource
    Type     ChangeType
    Path     string       // current path (relative to sync root, NFC-normalized)
    OldPath  string       // previous path (for moves only)
    ItemID   string       // server-assigned ID (remote events only; empty for local)
    ParentID string       // server parent ID (remote events only)
    DriveID  string       // normalized drive ID (lowercase, zero-padded to 16 chars)
    ItemType ItemType     // file, folder, root
    Name     string       // filename component (URL-decoded, NFC-normalized)
    Size     int64
    Hash     string       // QuickXorHash (base64); empty for folders
    Mtime    int64        // Unix nanoseconds (validated: no zero dates, no far-future)
    ETag     string       // remote only
    CTag     string       // remote only
    IsDeleted bool        // true for remote tombstones (Business may lack Name/Hash)
}
```

### 3.3 Baseline Types

```go
// BaselineEntry represents the confirmed synced state of a single path.
// This is the ONLY durable per-item state in the system. Everything else
// is ephemeral (rebuilt from API or filesystem each cycle).
type BaselineEntry struct {
    Path       string
    DriveID    string
    ItemID     string
    ParentID   string
    ItemType   ItemType
    // Name is derived from filepath.Base(Path) — not stored separately.

    // Per-side hashes: handles SharePoint enrichment natively.
    // For normal files, LocalHash == RemoteHash.
    // For enriched files, they diverge — both are recorded.
    LocalHash  string
    RemoteHash string

    Size       int64
    Mtime      int64     // local mtime at sync time (nanosecond precision)
    SyncedAt   int64     // when this entry was last confirmed synced

    // Remote metadata for conditional operations (If-Match on deletes)
    ETag       string
}

// Baseline is the in-memory representation of the entire baseline table.
// Loaded once at cycle start, used read-only during the pipeline.
type Baseline struct {
    ByPath map[string]*BaselineEntry   // primary lookup
    ByID   map[string]*BaselineEntry   // keyed by item_id, for remote move detection
}
```

### 3.4 Planner Types

```go
// PathChanges groups all observed changes for a single path.
// Produced by the change buffer, consumed by the planner.
type PathChanges struct {
    Path         string
    RemoteEvents []ChangeEvent   // all remote observations for this path
    LocalEvents  []ChangeEvent   // all local observations for this path
}

// PathView is the unified three-way view of a single path, constructed
// by the planner from change events + baseline. This is the input to
// the reconciliation decision matrix.
type PathView struct {
    Path     string
    Remote   *RemoteState    // derived from remote events; nil = no remote change observed
    Local    *LocalState     // derived from local events; nil = no local change observed
    Baseline *BaselineEntry  // from baseline DB; nil = never synced
}

// RemoteState is the current remote state of an item, derived from
// change events. Not a database row — an in-memory value.
type RemoteState struct {
    ItemID    string
    ParentID  string
    Name      string
    ItemType  ItemType
    Size      int64
    Hash      string
    Mtime     int64
    ETag      string
    CTag      string
    IsDeleted bool
}

// LocalState is the current local state of an item, derived from
// filesystem observation. Not a database row — an in-memory value.
type LocalState struct {
    Name     string
    ItemType ItemType
    Size     int64
    Hash     string
    Mtime    int64
}
```

### 3.5 Executor Types

```go
// Outcome is the result of executing a single action. Contains everything
// needed to update the baseline. Self-contained — no DB reads required
// to process an outcome.
type Outcome struct {
    Action     ActionType
    Success    bool
    Error      error

    Path       string
    OldPath    string     // for moves: the path being vacated
    DriveID    string
    ItemID     string     // server-assigned (from API response after upload)
    ParentID   string
    ItemType   ItemType

    LocalHash  string     // hash of local content after action
    RemoteHash string     // hash of remote content after action
    Size       int64
    Mtime      int64      // local mtime at sync time

    ETag       string     // from API response (for conditional operations)

    ConflictType string   // for ActionConflict: "edit_edit", "edit_delete", "create_create"
}
```

### 3.6 Action Types (Unchanged)

The action types and ActionPlan structure remain the same as the current design — they're well-designed:

```go
type ActionType int

const (
    ActionDownload     ActionType = iota
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
    View         *PathView        // full three-way context (replaces *Item)
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

Note: `Action.View` replaces `Action.Item`. The action carries the full `PathView` (three-way context) instead of a single `Item` struct. This gives the executor complete context about remote, local, and baseline state without needing to query the database.

### 3.7 Consumer-Defined Interfaces (Graph Client)

These are unchanged from the current design — they're clean and correct:

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

## Part 4: Data Model

### 4.1 Baseline Table

The only mutable per-item state in the system.

```sql
CREATE TABLE baseline (
    -- Dual key: path for local ops, item_id for remote ops
    -- Name is always filepath.Base(path) — no separate column needed.
    path            TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,    -- normalized: lowercase, zero-padded to 16 chars
    item_id         TEXT    NOT NULL,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),

    -- Per-side hashes (handles SharePoint enrichment natively)
    -- For normal files: local_hash == remote_hash
    -- For enriched files: they diverge, both recorded
    -- For iOS .heic files: remote hash may be unreliable (known API bug)
    local_hash      TEXT,
    remote_hash     TEXT,

    size            INTEGER,
    mtime           INTEGER,    -- local mtime at sync time (Unix nanoseconds)
                                -- OneDrive truncates to whole seconds; stored at full
                                -- nanosecond precision from local FS for fast-check
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),

    -- Remote metadata for conditional operations (If-Match on deletes)
    etag            TEXT
);

-- For remote move detection: look up baseline entry by item_id
CREATE UNIQUE INDEX idx_baseline_item ON baseline(drive_id, item_id);

-- For cascading path operations (folder renames update children)
CREATE INDEX idx_baseline_parent ON baseline(parent_id);
```

**11 columns.** Two columns removed during first-principles review: `name` (redundant
with `filepath.Base(path)`) and `ctag` (unused — not read by any code path; delta API
returns fresh ctags in responses). The PK B-tree on `path` supports prefix scans natively,
so no separate `idx_baseline_path_prefix` is needed.

What's absent and why:

| Absent field | Why not needed |
|---|---|
| `name` | Always `filepath.Base(path)`. Storing it separately creates a consistency invariant that could silently break. |
| `ctag` | Not used by planner (compares hashes), executor (no conditional operations use ctag), or verify command. Delta API returns fresh ctags. Can be added back via migration if ever needed. |
| `is_deleted` / `deleted_at` | Tombstones don't live in the baseline. The baseline only stores confirmed synced state. Deletions remove the row. |
| `local_mtime` / `remote_mtime` | The baseline records the local `mtime` at sync time. Per-side mtimes are ephemeral (in change events). |
| `local_size` / `remote_size` | Same — one confirmed `size`. |
| `LocalHash` as a separate concept from `SyncedHash` | The baseline stores both `local_hash` and `remote_hash` explicitly. No ambiguity about what `SyncedHash` means. |
| `created_at` / `updated_at` | Row metadata. The baseline has `synced_at` which serves the same purpose. |
| `remote_drive_id` / `remote_id` | For shared/remote items. These can be handled as a separate `shared_items` table if needed post-MVP. |
| `SHA256Hash` | Opportunistic Business-only field. Can be added to the baseline if needed. |

### 4.2 Delta Tokens Table

```sql
CREATE TABLE delta_tokens (
    drive_id    TEXT    PRIMARY KEY,
    token       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0)
);
```

**Critical property**: The delta token is saved in the same transaction as baseline updates. Never separately. If the process crashes after execution but before commit, the token is not advanced, and the same delta is re-fetched (idempotent).

### 4.3 Conflict Ledger

```sql
CREATE TABLE conflicts (
    id              TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,
    path            TEXT    NOT NULL,
    conflict_type   TEXT    NOT NULL CHECK(conflict_type IN (
                                'edit_edit', 'edit_delete', 'create_create'
                            )),
    detected_at     INTEGER NOT NULL CHECK(detected_at > 0),
    local_hash      TEXT,
    remote_hash     TEXT,
    local_mtime     INTEGER,
    remote_mtime    INTEGER,
    resolution      TEXT    NOT NULL DEFAULT 'unresolved'
                            CHECK(resolution IN ('unresolved', 'keep_both', 'keep_local', 'keep_remote', 'manual')),
    resolved_at     INTEGER,
    resolved_by     TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history         TEXT    -- JSON array of resolution events
);

CREATE INDEX idx_conflicts_resolution ON conflicts(resolution);
```

The `conflict_type` column persists the planner's classification (EF5/EF9/EF12) for display
in the `conflicts` CLI command. The `idx_conflicts_drive` index was removed (redundant — each
drive has its own DB file, so drive_id is constant across all rows).

### 4.4 Stale Files Ledger

```sql
CREATE TABLE stale_files (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL UNIQUE,  -- one entry per path
    reason      TEXT    NOT NULL,
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),
    size        INTEGER
);
```

Tracks files excluded by filter changes but still present locally. `UNIQUE(path)` prevents
duplicate entries when the same file becomes stale across multiple config changes.

### 4.5 Upload Sessions

```sql
CREATE TABLE upload_sessions (
    id              TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,           -- empty for new file uploads
    local_path      TEXT    NOT NULL,
    local_hash      TEXT    NOT NULL,  -- hash at session start (detect mutation on resume)
    session_url     TEXT    NOT NULL,
    expiry          INTEGER NOT NULL,
    bytes_uploaded  INTEGER NOT NULL DEFAULT 0,
    total_size      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL CHECK(created_at > 0)
);
```

Upload session persistence is essential for crash recovery of large file uploads. The
`local_hash` column detects local file mutation during the crash window: on resume, the
file's current hash is compared against the stored hash, and mismatches cause the session
to be discarded (the file will be re-uploaded from scratch on the next cycle).

### 4.6 Change Journal (Optional, for debugging)

```sql
CREATE TABLE change_journal (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   INTEGER NOT NULL,
    source      TEXT    NOT NULL CHECK(source IN ('remote', 'local')),
    change_type TEXT    NOT NULL CHECK(change_type IN ('create', 'modify', 'delete', 'move')),
    path        TEXT    NOT NULL,
    old_path    TEXT,              -- for moves
    item_id     TEXT,              -- for remote events
    hash        TEXT,
    size        INTEGER,
    mtime       INTEGER,          -- observed mtime (useful for debugging time-based issues)
    cycle_id    TEXT               -- groups events from the same sync cycle
);

CREATE INDEX idx_journal_timestamp ON change_journal(timestamp);
```

Append-only. Compacted periodically (drop entries older than N days, configurable). Provides a full audit trail of every observation — invaluable for debugging sync issues in production. Only the timestamp index is created; path and cycle queries can filter within time-bounded result sets. Additional indexes can be added via migration if query patterns warrant them.

### 4.7 Config Snapshots

```sql
CREATE TABLE config_snapshots (
    key         TEXT    PRIMARY KEY,
    value       TEXT    NOT NULL
);
```

For stale file detection on filter changes. Unchanged.

### 4.8 Schema Migrations

```sql
CREATE TABLE schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  INTEGER NOT NULL CHECK(applied_at > 0)
);
```

Tracks applied schema versions. Migration infrastructure from the current implementation is reused.

### 4.9 SQLite Pragmas

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA journal_size_limit = 67108864;  -- 64 MiB
```

Note the addition of `busy_timeout = 5000`. Even though E eliminates concurrent writes during execution (only the baseline manager writes), the busy_timeout is defense-in-depth against any unexpected concurrent access (e.g., `status` command reading while sync writes).

### 4.10 Timestamp Conventions

All timestamps are stored as INTEGER Unix nanoseconds (UTC). Validation rules:

| Condition | Action |
|---|---|
| `0001-01-01T00:00:00Z` or dates before 1970 | Fall back to `NowNano()` |
| Dates more than 1 year in the future | Fall back to `NowNano()` |
| Fractional seconds from OneDrive | Truncated to whole seconds for comparison |
| Local filesystem nanoseconds | Stored at full precision for fast-check |

**Racily-clean problem**: If a file is modified within the same second as the last sync, the mtime fast-check is ambiguous. Solution: when `truncateToSeconds(localMtime) == truncateToSeconds(baselineMtime)`, always compute the content hash to verify. This is the same approach as the enrichment guard in the current implementation but generalized.

### 4.11 Path Conventions

- All paths are relative to the sync root
- NFC-normalized (required for macOS APFS compatibility)
- URL-decoded (delta API returns URL-encoded names)
- Forward slash as separator (even on Windows, for DB consistency)
- No leading or trailing slashes

### 4.12 Total Tables

| Table | Purpose | Writer |
|---|---|---|
| `baseline` | Confirmed synced state per path | BaselineManager only |
| `delta_tokens` | Delta cursor per drive | BaselineManager only (same txn as baseline) |
| `conflicts` | Conflict ledger | BaselineManager (on conflict actions) |
| `stale_files` | Filter-change tracking | BaselineManager |
| `upload_sessions` | Crash recovery for large uploads | Executor (pre-upload) + BaselineManager (post-upload) |
| `change_journal` | Debugging audit trail (optional) | BaselineManager (append-only) |
| `config_snapshots` | Filter change detection | Engine (on config load) |
| `schema_migrations` | Schema version tracking | Engine (on startup) |

**8 tables total.** The dominant table (`baseline`) has 11 columns — lean and purpose-built for the event-driven architecture.

---

## Part 5: Component Design

### 5.1 Remote Observer

The remote observer produces `ChangeEvent` values from the Graph API. It has two modes: full delta (for one-shot) and watch (for continuous).

```go
type RemoteObserver struct {
    client   DeltaFetcher
    baseline *Baseline    // read-only reference for path materialization
    driveID  string
    logger   *slog.Logger
}

// FullDelta fetches all remote changes since the last delta token.
// Returns change events + the new delta token.
// Used by one-shot mode.
func (o *RemoteObserver) FullDelta(ctx context.Context, savedToken string) ([]ChangeEvent, string, error) {
    var events []ChangeEvent
    var newToken string

    token := savedToken
    for {
        page, err := o.client.Delta(ctx, o.driveID, token)
        if err != nil {
            return nil, "", fmt.Errorf("delta fetch: %w", err)
        }

        for _, item := range page.Items {
            event := o.convertToChangeEvent(item)
            events = append(events, event)
        }

        if page.DeltaLink != "" {
            newToken = extractToken(page.DeltaLink)
            break
        }
        token = page.NextLink
    }

    return events, newToken, nil
}

// Watch starts a long-running loop that polls the delta API (or listens
// on a WebSocket) and sends change events to the provided callback.
// Used by watch mode.
func (o *RemoteObserver) Watch(ctx context.Context, savedToken string, emit func(ChangeEvent)) error {
    // Poll-based implementation (WebSocket is a future optimization)
    token := savedToken
    ticker := time.NewTicker(pollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            events, newToken, err := o.FullDelta(ctx, token)
            if err != nil {
                o.logger.Warn("remote watch: delta failed", "error", err)
                continue
            }
            for _, e := range events {
                emit(e)
            }
            token = newToken
        }
    }
}

// convertToChangeEvent transforms a graph.Item (from delta) into a ChangeEvent.
// Handles all normalization: driveID casing, missing fields on deleted items,
// timestamp validation, path materialization.
func (o *RemoteObserver) convertToChangeEvent(item graph.Item) ChangeEvent {
    event := ChangeEvent{
        Source:   SourceRemote,
        ItemID:   item.ID,
        ParentID: item.ParentReference.ID,
        Name:     item.Name,
        ItemType: classifyGraphItem(item),
        Size:     item.Size,
        Hash:     item.QuickXorHash,
        ETag:     item.ETag,
        CTag:     item.CTag,
        Mtime:    toUnixNano(item.LastModifiedDateTime),
    }

    if item.Deleted {
        event.Type = ChangeDelete
        event.IsDeleted = true
        // Look up missing fields from baseline (Business deleted items lack name)
        if event.Name == "" {
            if base := o.baseline.ByID[event.ItemID]; base != nil {
                event.Name = filepath.Base(base.Path)
            }
        }
    } else if base := o.baseline.ByID[item.ID]; base != nil {
        // Existing item: check if moved
        newPath := o.materializePath(event)
        if newPath != base.Path {
            event.Type = ChangeMove
            event.OldPath = base.Path
            event.Path = newPath
        } else {
            event.Type = ChangeModify
            event.Path = base.Path
        }
    } else {
        event.Type = ChangeCreate
        event.Path = o.materializePath(event)
    }

    return event
}

// materializePath reconstructs the full relative path for an item by walking
// the parent chain. Checks in-flight events first (parent may have arrived
// earlier in the same delta), falls back to baseline.
func (o *RemoteObserver) materializePath(event ChangeEvent) string {
    // ... walk parent chain using baseline.ByID + in-flight parent map ...
}
```

**Key properties**:
- Produces `[]ChangeEvent`, not database writes
- Path materialization uses the baseline (read-only) + in-flight parent tracking
- Normalization (driveID casing, missing fields, timestamps) happens here, same as current `delta.go`
- The same code works for both one-shot and watch mode

**API quirk handling in `convertToChangeEvent`** (all handled in the observer, invisible to downstream):

| Quirk | Handling |
|---|---|
| **DriveID casing** | Lowercase + zero-pad to 16 chars (e.g., `24470056F5C3E43` → `024470056f5c3e43`) |
| **URL-encoded names** | `url.PathUnescape(item.Name)` on every item |
| **Missing hashes on deleted items** | Business deletes lack hash/name; look up from baseline by ItemID |
| **`Prefer` header** | Personal delta requires `Prefer: deltashowremoteitemsaliasid` header |
| **iOS .heic hash mismatch** | Known API bug — log warning, don't fail. Mark as known-unreliable in event |
| **Timestamp validation** | Reject `0001-01-01T00:00:00Z`, dates before 1970, far-future; fall back to `NowNano()` |
| **NFC normalization** | `norm.NFC.String(item.Name)` for macOS compatibility |
| **parentReference.path absence** | Delta never includes `parentReference.path` — always reconstruct from parent chain |
| **Dedup within page** | Keep last occurrence of `(driveId, itemId)` per page (API sends dups) |
| **Clear bogus hashes on deleted items** | Some deleted items have stale hashes — clear them |

**Deletion reordering**: Within each delta page, deletions are buffered and processed before creations. This handles the known API bug where a delete and create at the same path arrive in the wrong order within a single page. Cross-page reordering is NOT done because it would break the parent-before-child guarantee for creations.

**HTTP 410 handling**: When the delta API returns HTTP 410 (token expired), the Remote Observer:
1. Discards the expired token
2. Returns a special error indicating full re-enumeration is needed
3. The engine catches this and restarts with no saved token (full delta = full enumeration)
4. The two resync types (Microsoft distinguishes them) are handled based on the response body

### 5.2 Local Observer

The local observer produces `ChangeEvent` values from the filesystem. Two modes: full scan and watch.

```go
type LocalObserver struct {
    baseline     *Baseline    // read-only reference for fast-path mtime comparison
    filter       Filter
    skipSymlinks bool
    syncRoot     string
    checkerPool  *CheckerPool // worker pool for parallel hash computation
    logger       *slog.Logger
}

// FullScan walks the entire sync directory and produces change events
// by comparing the filesystem against the baseline.
// Used by one-shot mode.
func (o *LocalObserver) FullScan(ctx context.Context) ([]ChangeEvent, error) {
    // Check .nosync guard before any work (S2 safety).
    if _, err := os.Stat(filepath.Join(o.syncRoot, ".nosync")); err == nil {
        return nil, ErrNosyncGuard
    }

    var events []ChangeEvent
    observed := make(map[string]bool) // track which baseline paths were seen

    err := filepath.WalkDir(o.syncRoot, func(path string, d fs.DirEntry, walkErr error) error {
        if walkErr != nil {
            o.logger.Warn("local scan: walk error", "path", path, "error", walkErr)
            return nil // skip inaccessible entries, don't abort entire scan
        }
        if ctx.Err() != nil { return ctx.Err() }

        fsRelPath := relativize(path, o.syncRoot)
        if fsRelPath == "" || fsRelPath == "." { return nil }

        // NFC-normalize for DB/baseline lookup (macOS APFS uses NFD).
        dbRelPath := norm.NFC.String(fsRelPath)

        // Symlink handling: never follow symlinks.
        if d.Type()&fs.ModeSymlink != 0 {
            if !o.skipSymlinks {
                o.logger.Warn("local scan: skipping symlink", "path", dbRelPath)
            }
            return nil
        }

        // Name validation: reject OneDrive-invalid names.
        if !isValidName(d.Name()) {
            o.logger.Debug("local scan: skipping invalid name", "path", dbRelPath)
            if d.IsDir() { return filepath.SkipDir }
            return nil
        }

        // DirEntry.Info() can fail (permission errors, race conditions where
        // file disappears between readdir and stat). Handle gracefully.
        info, err := d.Info()
        if err != nil {
            o.logger.Warn("local scan: stat failed", "path", dbRelPath, "error", err)
            return nil // skip this entry
        }

        // Filter cascade (uses dbRelPath for consistent pattern matching).
        result := o.filter.ShouldSync(dbRelPath, d.IsDir(), info.Size())
        if !result.Included {
            if d.IsDir() { return filepath.SkipDir }
            return nil
        }

        observed[dbRelPath] = true

        entry := ChangeEvent{
            Source:   SourceLocal,
            Path:     dbRelPath,
            Name:     norm.NFC.String(d.Name()),
            ItemType: itemTypeFromDirEntry(d),
            Size:     info.Size(),
            Mtime:    info.ModTime().UnixNano(),
        }

        base := o.baseline.ByPath[dbRelPath]

        if base == nil {
            // New local file/folder — not in baseline.
            entry.Type = ChangeCreate
            if info.Mode().IsRegular() {
                entry.Hash = o.checkerPool.ComputeHash(path)
            }
            events = append(events, entry)
            return nil
        }

        if info.Mode().IsRegular() {
            // Fast path: if mtime clearly differs from baseline, definitely changed.
            // Slow path: always hash to verify.
            //
            // Racily-clean guard: if mtime matches at truncated-seconds precision,
            // the file MAY have been modified within the same second the baseline
            // was committed. In this case we CANNOT trust the mtime match and must
            // compute the hash to verify. This is the same-second ambiguity problem
            // documented in git's index design and our enrichment guard.
            localSec := truncateToSeconds(entry.Mtime)
            baseSec := truncateToSeconds(base.Mtime)

            if localSec == baseSec {
                // Same-second window: hash to verify (racily-clean).
                entry.Hash = o.checkerPool.ComputeHash(path)
                if entry.Hash != base.LocalHash {
                    entry.Type = ChangeModify
                    events = append(events, entry)
                }
                // Hash matches: genuinely unchanged despite same-second mtime.
                return nil
            }

            if localSec != baseSec {
                // Mtime clearly changed: compute hash to confirm real change.
                entry.Hash = o.checkerPool.ComputeHash(path)
                if entry.Hash != base.LocalHash {
                    entry.Type = ChangeModify
                    events = append(events, entry)
                }
                // Hash matches despite mtime change: timestamp drift, no event.
            }
        }

        return nil
    })

    if err != nil { return nil, err }

    // Detect local deletions: baseline entries not observed during walk.
    for bPath, base := range o.baseline.ByPath {
        if !observed[bPath] && base.ItemType != ItemTypeRoot {
            events = append(events, ChangeEvent{
                Source:   SourceLocal,
                Type:     ChangeDelete,
                Path:     bPath,
                Name:     filepath.Base(bPath),
                ItemType: base.ItemType,
            })
        }
    }

    return events, nil
}

// Watch starts a filesystem watcher that emits change events in real-time.
// Used by watch mode.
func (o *LocalObserver) Watch(ctx context.Context, emit func(ChangeEvent)) error {
    // Use rjeczalik/notify for cross-platform FS events
    c := make(chan notify.EventInfo, 256)
    if err := notify.Watch(o.syncRoot+"/...", c, notify.All); err != nil {
        return fmt.Errorf("watch: %w", err)
    }
    defer notify.Stop(c)

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case ei := <-c:
            relPath := relativize(ei.Path(), o.syncRoot)

            // Filter
            info, err := os.Stat(ei.Path())
            if err != nil && !os.IsNotExist(err) { continue }

            isDir := info != nil && info.IsDir()
            size := int64(0)
            if info != nil { size = info.Size() }

            if !o.filter.ShouldSync(relPath, isDir, size).Included {
                continue
            }

            event := ChangeEvent{
                Source: SourceLocal,
                Path:   relPath,
                Name:   filepath.Base(relPath),
            }

            if os.IsNotExist(err) || info == nil {
                event.Type = ChangeDelete
            } else if o.baseline.ByPath[relPath] == nil {
                event.Type = ChangeCreate
                event.Size = info.Size()
                event.Mtime = info.ModTime().UnixNano()
                if info.Mode().IsRegular() {
                    event.Hash = o.checkerPool.ComputeHash(ei.Path())
                }
                event.ItemType = itemTypeFromFileInfo(info)
            } else {
                event.Type = ChangeModify
                event.Size = info.Size()
                event.Mtime = info.ModTime().UnixNano()
                if info.Mode().IsRegular() {
                    event.Hash = o.checkerPool.ComputeHash(ei.Path())
                }
                event.ItemType = itemTypeFromFileInfo(info)
            }

            emit(event)
        }
    }
}
```

**Key properties**:
- Produces `[]ChangeEvent`, not database writes
- No `local:` prefixed IDs — `LocalState` has no item_id field
- No `GetItemByPath` DB query — compares against in-memory baseline
- Dual-path threading: `fsRelPath` (filesystem I/O) and `dbRelPath` (NFC-normalized for baseline lookup)
- `.nosync` guard checked before any work (S2 safety)
- Racily-clean guard: same-second mtime triggers hash verification (not skip)
- DirEntry.Info() errors handled gracefully (skip entry, don't abort scan)
- Local deletion detection by diffing observed paths against baseline — no tombstone interaction
- Same code structure works for both full scan and watch mode

**Additional local observer responsibilities** (all demonstrated in the code above):

| Concern | Handling |
|---|---|
| **NFC/NFD normalization** | macOS APFS uses NFD; `dbRelPath = norm.NFC.String(fsRelPath)` for baseline lookup. `fsRelPath` used for I/O operations |
| **`.nosync` guard file** | Checked before `WalkDir` begins. Returns `ErrNosyncGuard` sentinel. Prevents syncing unmounted volumes (S2 safety) |
| **Symlink handling** | Detected via `d.Type()&fs.ModeSymlink`. Never followed. `skipSymlinks` controls warning suppression. Broken symlinks caught by subsequent stat |
| **Name validation** | `isValidName(d.Name())` rejects: `.lock`, `desktop.ini`, reserved names (`CON`, `PRN`, `AUX`, `NUL`, `COM0-9`, `LPT0-9`), names starting with `~$`, names containing `_vti_`, trailing dots, newlines |
| **Path length validation** | Personal: 400 character limit. Business: 400 URL-encoded byte limit. Different calculations per drive type |
| **Temp file exclusion** | Handled by filter cascade: `.partial`, `.tmp`, `.swp`, `~*`, `.~*`, `.crdownload` files excluded (S7 safety invariant) |
| **DirEntry.Info() errors** | Logged as warning, entry skipped. Handles permission errors and race conditions (file disappears between readdir and stat) |
| **Racily-clean handling** | When `truncateToSeconds(localMtime) == truncateToSeconds(baseMtime)`, always computes hash. Prevents false negatives from same-second modifications |

### 5.3 Change Buffer

Collects events from both observers, deduplicates, debounces, and produces batches grouped by path.

```go
type ChangeBuffer struct {
    pending map[string][]ChangeEvent
    mu      sync.Mutex
    ready   chan struct{} // signaled when debounce timer fires
    timer   *time.Timer
    window  time.Duration // debounce window (default 2 seconds)
}

func NewChangeBuffer(debounceWindow time.Duration) *ChangeBuffer {
    return &ChangeBuffer{
        pending: make(map[string][]ChangeEvent),
        ready:   make(chan struct{}, 1),
        window:  debounceWindow,
    }
}

// Add records a change event. Resets the debounce timer.
//
// Move events are indexed under BOTH the old and new paths so the Planner
// sees the full picture for each path. The event is stored under the new
// path (where the item lives now). A synthetic ChangeDelete is stored
// under the old path (so the Planner knows that path is vacated).
func (b *ChangeBuffer) Add(event ChangeEvent) {
    b.mu.Lock()
    defer b.mu.Unlock()

    b.pending[event.Path] = append(b.pending[event.Path], event)

    // For moves: also register a synthetic delete at the old path.
    // Without this, the old path never enters the Planner and its
    // baseline entry would become orphaned.
    if event.Type == ChangeMove && event.OldPath != "" {
        syntheticDelete := ChangeEvent{
            Source:   event.Source,
            Type:     ChangeDelete,
            Path:     event.OldPath,
            Name:     filepath.Base(event.OldPath),
            ItemID:   event.ItemID,
            DriveID:  event.DriveID,
            ItemType: event.ItemType,
        }
        b.pending[event.OldPath] = append(b.pending[event.OldPath], syntheticDelete)
    }

    // Reset debounce timer
    if b.timer != nil {
        b.timer.Stop()
    }
    b.timer = time.AfterFunc(b.window, func() {
        select {
        case b.ready <- struct{}{}:
        default:
        }
    })
}

// AddAll adds multiple events (used by one-shot mode).
func (b *ChangeBuffer) AddAll(events []ChangeEvent) {
    b.mu.Lock()
    defer b.mu.Unlock()
    for _, e := range events {
        b.pending[e.Path] = append(b.pending[e.Path], e)

        // Same move-event dual-keying as Add().
        if e.Type == ChangeMove && e.OldPath != "" {
            syntheticDelete := ChangeEvent{
                Source:   e.Source,
                Type:     ChangeDelete,
                Path:     e.OldPath,
                Name:     filepath.Base(e.OldPath),
                ItemID:   e.ItemID,
                DriveID:  e.DriveID,
                ItemType: e.ItemType,
            }
            b.pending[e.OldPath] = append(b.pending[e.OldPath], syntheticDelete)
        }
    }
}

// Ready returns a channel that signals when the debounce window has elapsed
// and there are pending events to process.
func (b *ChangeBuffer) Ready() <-chan struct{} {
    return b.ready
}

// Flush returns all pending changes grouped by path and clears the buffer.
func (b *ChangeBuffer) Flush() []PathChanges {
    b.mu.Lock()
    defer b.mu.Unlock()

    changes := make([]PathChanges, 0, len(b.pending))
    for path, events := range b.pending {
        pc := PathChanges{
            Path:         path,
            RemoteEvents: filterBySource(events, SourceRemote),
            LocalEvents:  filterBySource(events, SourceLocal),
        }
        changes = append(changes, pc)
    }

    b.pending = make(map[string][]ChangeEvent)
    return changes
}

// FlushImmediate flushes without waiting for debounce (for one-shot mode).
func (b *ChangeBuffer) FlushImmediate() []PathChanges {
    return b.Flush()
}

func filterBySource(events []ChangeEvent, source ChangeSource) []ChangeEvent {
    var filtered []ChangeEvent
    for _, e := range events {
        if e.Source == source {
            filtered = append(filtered, e)
        }
    }
    return filtered
}
```

**Key properties**:
- Thread-safe (mutex-protected)
- Debounce window prevents processing the same file multiple times during rapid edits
- Groups events by path so the planner sees the full picture for each path
- Move events are dual-keyed: stored at the new path AND a synthetic delete at the old path. This ensures the Planner sees both paths and the old baseline entry isn't orphaned.
- `FlushImmediate` for one-shot mode (no need to wait)

### 5.4 Planner

The planner is the intellectual core. It takes change events + baseline and produces an ActionPlan. It is composed of pure functions.

```go
type Planner struct {
    filter Filter
    logger *slog.Logger
}

// Plan takes a batch of path changes and the current baseline,
// and produces an ordered ActionPlan.
// This is a pure function: no I/O, no database access, deterministic.
func (p *Planner) Plan(
    changes []PathChanges,
    baseline *Baseline,
    mode SyncMode,
    config *config.SafetyConfig,
) (*ActionPlan, error) {
    // Step 1: Build PathViews from changes + baseline
    views := p.buildPathViews(changes, baseline)

    // Step 2: Detect moves
    //
    // Remote moves: The Remote Observer already identified these as ChangeMove
    // events and the ChangeBuffer dual-keyed them (new path + synthetic delete
    // at old path). Here we extract them from PathViews: any remote event with
    // ChangeMove type produces a LocalMove action (rename local file to match).
    //
    // Local moves: Hash-based correlation. When a baseline path has a local
    // delete event AND a new path has a local create event with a matching
    // unique hash, they are combined into a single RemoteMove action.
    moves := p.detectMoves(changes, views, baseline)

    // Step 3: Classify each PathView using the decision matrix
    plan := &ActionPlan{}
    for _, view := range views {
        if moves.involves(view.Path) {
            continue // already handled as a move
        }

        // Apply mode-specific filtering
        if mode == SyncDownloadOnly && view.Local != nil && view.Baseline == nil {
            continue
        }
        if mode == SyncUploadOnly && view.Remote != nil && view.Baseline == nil {
            continue
        }

        // Apply filter SYMMETRICALLY to both remote-only and local-only items.
        // This fixes Fault Line 6: in the current architecture, filters only run
        // in the scanner. Here, the planner applies the three-layer filter cascade
        // (sync_paths → config patterns → .odignore) to all items uniformly.
        // Monotonic exclusion: each layer can only exclude more, never include back.
        if view.Remote != nil && view.Local == nil && view.Baseline == nil {
            isDir := view.Remote.ItemType == ItemTypeFolder
            if !p.filter.ShouldSync(view.Path, isDir, view.Remote.Size).Included {
                continue
            }
        }
        if view.Local != nil && view.Remote == nil && view.Baseline == nil {
            isDir := view.Local.ItemType == ItemTypeFolder
            if !p.filter.ShouldSync(view.Path, isDir, view.Local.Size).Included {
                continue
            }
        }

        actions := p.classifyPathView(view, mode)
        appendActions(plan, actions)
    }

    plan.Moves = append(plan.Moves, moves.actions...)

    // Step 4: Order the plan
    orderPlan(plan)

    // Step 5: Safety checks (also pure functions)
    plan, err := p.safetyCheck(plan, baseline, config)
    if err != nil {
        return nil, err
    }

    return plan, nil
}

// detectMoves finds both remote and local moves.
//
// Remote moves: Identified by the Remote Observer (ChangeMove events with
// OldPath). The ChangeBuffer created a synthetic delete at OldPath, so the
// Planner sees both paths. This function correlates them into LocalMove
// actions (rename the local file to match the remote's new location).
//
// Local moves: Hash-based correlation. A locally-deleted baseline item
// whose hash matches a locally-created new item (unique match constraint)
// is combined into a RemoteMove action (tell the server about the rename).
func (p *Planner) detectMoves(
    changes []PathChanges, views []PathView, baseline *Baseline,
) *moveSet {
    ms := &moveSet{}

    // --- Remote moves ---
    for _, change := range changes {
        for _, event := range change.RemoteEvents {
            if event.Type != ChangeMove || event.OldPath == "" {
                continue
            }
            ms.add(Action{
                Type:    ActionLocalMove,
                Path:    event.OldPath,
                NewPath: event.Path,
                ItemID:  event.ItemID,
                DriveID: event.DriveID,
                View:    viewForPath(views, event.Path),
            })
            // Mark both old and new paths as handled.
            ms.markInvolved(event.OldPath)
            ms.markInvolved(event.Path)
        }
    }

    // --- Local moves (hash-based, same as current design) ---
    // Find locally-deleted items (baseline exists, local absent)
    // and locally-created items (no baseline, local present).
    // Match by unique hash.
    deleted := map[string]PathView{} // hash → view of deleted item
    created := map[string]PathView{} // hash → view of new item

    for _, view := range views {
        if ms.involves(view.Path) { continue }

        if view.Baseline != nil && view.Local == nil && view.Remote == nil {
            // Locally deleted, no remote change → candidate source of move
            if view.Baseline.LocalHash != "" {
                if _, dup := deleted[view.Baseline.LocalHash]; !dup {
                    deleted[view.Baseline.LocalHash] = view
                } else {
                    delete(deleted, view.Baseline.LocalHash) // ambiguous, skip
                }
            }
        }
        if view.Baseline == nil && view.Local != nil && view.Remote == nil {
            // Locally created, no remote presence → candidate target of move
            if view.Local.Hash != "" {
                if _, dup := created[view.Local.Hash]; !dup {
                    created[view.Local.Hash] = view
                } else {
                    delete(created, view.Local.Hash) // ambiguous, skip
                }
            }
        }
    }

    for hash, src := range deleted {
        if dst, ok := created[hash]; ok {
            ms.add(Action{
                Type:    ActionRemoteMove,
                Path:    src.Path,
                NewPath: dst.Path,
                ItemID:  src.Baseline.ItemID,
                DriveID: src.Baseline.DriveID,
                View:    &dst,
            })
            ms.markInvolved(src.Path)
            ms.markInvolved(dst.Path)
        }
    }

    return ms
}

// buildPathViews constructs PathView values from change events + baseline.
func (p *Planner) buildPathViews(changes []PathChanges, baseline *Baseline) []PathView {
    views := make([]PathView, 0, len(changes))

    for _, change := range changes {
        view := PathView{
            Path:     change.Path,
            Baseline: baseline.ByPath[change.Path],
        }

        // Derive RemoteState from the latest remote event
        if len(change.RemoteEvents) > 0 {
            latest := lastEvent(change.RemoteEvents)
            view.Remote = &RemoteState{
                ItemID:    latest.ItemID,
                ParentID:  latest.ParentID,
                Name:      latest.Name,
                ItemType:  latest.ItemType,
                Size:      latest.Size,
                Hash:      latest.Hash,
                Mtime:     latest.Mtime,
                ETag:      latest.ETag,
                CTag:      latest.CTag,
                IsDeleted: latest.Type == ChangeDelete,
            }
        }

        // Derive LocalState from the latest local event
        if len(change.LocalEvents) > 0 {
            latest := lastEvent(change.LocalEvents)
            if latest.Type == ChangeDelete {
                // Local deletion: Local is nil (absent)
                view.Local = nil
            } else {
                view.Local = &LocalState{
                    Name:     latest.Name,
                    ItemType: latest.ItemType,
                    Size:     latest.Size,
                    Hash:     latest.Hash,
                    Mtime:    latest.Mtime,
                }
            }
        } else if view.Baseline != nil {
            // No local change event, but item is in baseline.
            // In full-scan mode, absence of a local event means unchanged.
            // In watch mode, absence means we haven't observed a change.
            // Either way: local state = baseline's local state (unchanged).
            view.Local = &LocalState{
                Name:     filepath.Base(view.Baseline.Path),
                ItemType: view.Baseline.ItemType,
                Size:     view.Baseline.Size,
                Hash:     view.Baseline.LocalHash,
                Mtime:    view.Baseline.Mtime,
            }
        }

        views = append(views, view)
    }

    return views
}
```

### 5.5 Decision Matrix (File Classification)

E uses the same reconciliation logic as the current F1-F14/D1-D7 matrix from
sync-algorithm.md, but reorganized to reflect E's three-way `PathView` inputs.
Moves (F13/F14 in the spec) are handled separately in `detectMoves` above and
are excluded before this function runs. The numbering below is E's own — the
mapping table shows correspondence.

**File decision matrix:**

| E# | Local | Remote | Baseline | Action | Spec equivalent |
|----|-------|--------|----------|--------|-----------------|
| EF1 | unchanged | unchanged | exists | no-op | F1 |
| EF2 | unchanged | changed | exists | download | F2 |
| EF3 | changed | unchanged | exists | upload | F3 |
| EF4 | changed | changed (same hash) | exists | update synced | F5 sub-case (convergent edit) |
| EF5 | changed | changed (diff hash) | exists | **conflict** (edit-edit) | F5 |
| EF6 | deleted | unchanged | exists | remote delete | F6 |
| EF7 | deleted | changed | exists | download (remote wins) | F7 |
| EF8 | unchanged | deleted | exists | local delete | F4 |
| EF9 | changed | deleted | exists | **conflict** (edit-delete) | F9 |
| EF10 | deleted | deleted | exists | cleanup (both gone) | F8 |
| EF11 | new | new (same hash) | none | update synced | F11 sub-case (convergent create) |
| EF12 | new | new (diff hash) | none | **conflict** (create-create) | F11 |
| EF13 | new | absent | none | upload | F10 |
| EF14 | absent | new | none | download | F12 |

Note: Spec F13 (local move) and F14 (remote move) are handled by `detectMoves`
above, not by `classifyFile`. EF4 and EF11 are convergent sub-cases not
explicitly numbered in the spec — both sides independently arrived at the same
content, so no data transfer is needed, just a baseline update.

```go
func (p *Planner) classifyFile(v PathView, mode SyncMode) []Action {
    localChanged := p.detectLocalChange(v)
    remoteChanged := p.detectRemoteChange(v)

    // Mode filtering
    if mode == SyncDownloadOnly { localChanged = false }
    if mode == SyncUploadOnly { remoteChanged = false }

    hasRemote := v.Remote != nil && !v.Remote.IsDeleted
    hasLocal := v.Local != nil
    hasBaseline := v.Baseline != nil
    remoteDeleted := v.Remote != nil && v.Remote.IsDeleted
    localDeleted := hasBaseline && !hasLocal

    switch {
    // --- Baseline exists (previously synced) ---

    case hasBaseline && !localChanged && !remoteChanged:
        return nil // EF1: both sides unchanged — in sync

    case hasBaseline && !localChanged && remoteChanged && hasRemote:
        return []Action{{Type: ActionDownload, Path: v.Path, View: &v}} // EF2

    case hasBaseline && localChanged && !remoteChanged:
        return []Action{{Type: ActionUpload, Path: v.Path, View: &v}} // EF3

    case hasBaseline && localChanged && remoteChanged && hasRemote:
        if v.Local.Hash == v.Remote.Hash {
            // EF4: convergent edit — both sides arrived at same content
            return []Action{{Type: ActionUpdateSynced, Path: v.Path, View: &v}}
        }
        // EF5: divergent edit — genuine conflict
        return []Action{{Type: ActionConflict, Path: v.Path, View: &v,
            ConflictInfo: &ConflictRecord{Type: ConflictEditEdit}}}

    case hasBaseline && localDeleted && !remoteChanged && !remoteDeleted:
        return []Action{{Type: ActionRemoteDelete, Path: v.Path, View: &v}} // EF6

    case hasBaseline && localDeleted && remoteChanged && hasRemote:
        return []Action{{Type: ActionDownload, Path: v.Path, View: &v}} // EF7

    case hasBaseline && !localChanged && remoteDeleted:
        return []Action{{Type: ActionLocalDelete, Path: v.Path, View: &v}} // EF8

    case hasBaseline && localChanged && remoteDeleted:
        // EF9: local edited but remote deleted — conflict
        return []Action{{Type: ActionConflict, Path: v.Path, View: &v,
            ConflictInfo: &ConflictRecord{Type: ConflictEditDelete}}}

    case hasBaseline && localDeleted && remoteDeleted:
        return []Action{{Type: ActionCleanup, Path: v.Path, View: &v}} // EF10

    // --- No baseline (never synced) ---

    case !hasBaseline && hasLocal && hasRemote:
        if v.Local.Hash == v.Remote.Hash {
            // EF11: convergent create — same file appeared on both sides
            return []Action{{Type: ActionUpdateSynced, Path: v.Path, View: &v}}
        }
        // EF12: divergent create — genuine conflict
        return []Action{{Type: ActionConflict, Path: v.Path, View: &v,
            ConflictInfo: &ConflictRecord{Type: ConflictCreateCreate}}}

    case !hasBaseline && hasLocal && !hasRemote && !remoteDeleted:
        return []Action{{Type: ActionUpload, Path: v.Path, View: &v}} // EF13

    case !hasBaseline && !hasLocal && hasRemote:
        return []Action{{Type: ActionDownload, Path: v.Path, View: &v}} // EF14
    }

    return nil
}
```

### 5.6 Decision Matrix (Folder Classification)

Folders use existence-based reconciliation — no hash comparison, no content
change detection. A folder is "changed" only if it was created, deleted, or
moved. Folder moves are handled by `detectMoves` (same as files).

**Folder decision matrix:**

| E# | Local | Remote | Baseline | Action | Spec equivalent |
|----|-------|--------|----------|--------|-----------------|
| ED1 | exists | exists | exists | no-op | D1 |
| ED2 | exists | exists | none | adopt (update synced) | D6 (merge) |
| ED3 | absent | exists | none | create locally | D2 |
| ED4 | absent | exists | exists | recreate locally | — (missing → re-create) |
| ED5 | exists | absent | none | create remotely | D3 |
| ED6 | exists | deleted | exists | delete locally | D5 |
| ED7 | absent | deleted | exists | cleanup | — (both gone) |
| ED8 | absent | absent | exists | cleanup | — (both gone) |

```go
func (p *Planner) classifyFolder(v PathView, mode SyncMode) []Action {
    hasRemote := v.Remote != nil && !v.Remote.IsDeleted
    hasLocal := v.Local != nil
    hasBaseline := v.Baseline != nil
    remoteDeleted := v.Remote != nil && v.Remote.IsDeleted

    switch {
    case hasBaseline && hasLocal && hasRemote:
        return nil // ED1: in sync

    case !hasBaseline && hasLocal && hasRemote:
        // ED2: folder exists on both sides but no baseline — adopt it
        return []Action{{Type: ActionUpdateSynced, Path: v.Path, View: &v}}

    case !hasLocal && hasRemote && !hasBaseline:
        // ED3: new remote folder, doesn't exist locally — create locally
        return []Action{{Type: ActionFolderCreate, Path: v.Path, View: &v,
            CreateSide: FolderCreateLocal}}

    case hasBaseline && !hasLocal && hasRemote:
        // ED4: was synced, local is missing, remote exists — recreate locally
        return []Action{{Type: ActionFolderCreate, Path: v.Path, View: &v,
            CreateSide: FolderCreateLocal}}

    case !hasBaseline && hasLocal && !hasRemote && !remoteDeleted:
        // ED5: new local folder, doesn't exist remotely — create remotely
        return []Action{{Type: ActionFolderCreate, Path: v.Path, View: &v,
            CreateSide: FolderCreateRemote}}

    case hasBaseline && hasLocal && remoteDeleted:
        // ED6: remote deleted a previously-synced folder — delete locally
        return []Action{{Type: ActionLocalDelete, Path: v.Path, View: &v}}

    case hasBaseline && !hasLocal && remoteDeleted:
        // ED7: both sides deleted — cleanup baseline entry
        return []Action{{Type: ActionCleanup, Path: v.Path, View: &v}}

    case hasBaseline && !hasLocal && !hasRemote && !remoteDeleted:
        // ED8: was synced, now missing from both sides — cleanup
        return []Action{{Type: ActionCleanup, Path: v.Path, View: &v}}
    }

    return nil
}
```

**Why ED1 is now reachable for folders**: In the current architecture, `isSynced()` checks `SyncedHash != "" || LastSyncedAt != nil`. Folders have no hash, and `executeSyncedUpdate` never sets `LastSyncedAt` for folders. So `isSynced()` always returns false, and all folders loop through D2 (adopt) every cycle, producing spurious DB writes.

In E, `hasBaseline` is simply `v.Baseline != nil`. If a folder was previously synced and has a baseline entry, ED1 fires. No hash check. No special folder handling. The bug is structurally eliminated.

### 5.7 Change Detection

```go
// detectLocalChange determines whether the local state has changed since
// the last sync. Uses per-side baseline for SharePoint enrichment correctness.
func (p *Planner) detectLocalChange(v PathView) bool {
    if v.Baseline == nil {
        return v.Local != nil // new local = changed
    }
    if v.Local == nil {
        return true // locally deleted = changed
    }
    if v.Baseline.ItemType == ItemTypeFolder {
        return false // folders: existence-based, always "unchanged" if present
    }
    return v.Local.Hash != v.Baseline.LocalHash
}

// detectRemoteChange determines whether the remote state has changed since
// the last sync. Uses per-side baseline for SharePoint enrichment correctness.
func (p *Planner) detectRemoteChange(v PathView) bool {
    if v.Baseline == nil {
        return v.Remote != nil && !v.Remote.IsDeleted // new remote = changed
    }
    if v.Remote == nil {
        return false // no remote observation = unchanged
    }
    if v.Remote.IsDeleted {
        return true // remote tombstoned = changed
    }
    if v.Baseline.ItemType == ItemTypeFolder {
        return false // folders: existence-based
    }
    return v.Remote.Hash != v.Baseline.RemoteHash
}
```

**Per-side baselines**: After a SharePoint upload where enrichment modifies the file (local=AAA, remote=BBB), the baseline stores `LocalHash=AAA, RemoteHash=BBB`. On the next cycle:
- `detectLocalChange`: local hash AAA == baseline LocalHash AAA → no change
- `detectRemoteChange`: remote hash BBB == baseline RemoteHash BBB → no change
- Result: EF1 (in sync). No infinite re-upload loop.

### 5.8 Executor

The executor takes an ActionPlan and executes it against the API and filesystem. It produces Outcomes, not database writes.

```go
type Executor struct {
    apiClient   GraphClient
    syncRoot    string
    safetyCfg   *config.SafetyConfig
    transferCfg *config.TransfersConfig
    transferMgr *TransferManager
    logger      *slog.Logger
}

func (e *Executor) Execute(ctx context.Context, plan *ActionPlan) ([]Outcome, error) {
    var outcomes []Outcome
    var mu sync.Mutex

    collectOutcome := func(o Outcome) {
        mu.Lock()
        outcomes = append(outcomes, o)
        mu.Unlock()
    }

    // Phase 1: Folder creates (sequential, top-down by depth)
    for _, action := range plan.FolderCreates {
        outcome := e.executeFolderCreate(ctx, action)
        collectOutcome(outcome)
    }

    // Phase 2: Moves (sequential — ordering matters for nested moves)
    // ActionLocalMove: rename local file/folder from action.Path to action.NewPath
    // ActionRemoteMove: call MoveItem API to rename on server
    // Both produce Outcome with OldPath + Path for baseline update
    for _, action := range plan.Moves {
        outcome := e.executeMove(ctx, action)
        collectOutcome(outcome)
    }

    // Phase 3: Downloads (parallel via worker pool)
    e.transferMgr.ExecuteDownloads(ctx, plan.Downloads, collectOutcome)

    // Phase 4: Uploads (parallel via worker pool)
    e.transferMgr.ExecuteUploads(ctx, plan.Uploads, collectOutcome)

    // Phase 5: Local deletes (sequential, files first, then folders bottom-up)
    for _, action := range plan.LocalDeletes {
        outcome := e.executeLocalDelete(ctx, action)
        collectOutcome(outcome)
    }

    // Phase 6: Remote deletes (sequential)
    for _, action := range plan.RemoteDeletes {
        outcome := e.executeRemoteDelete(ctx, action)
        collectOutcome(outcome)
    }

    // Phase 7: Conflicts (sequential)
    for _, action := range plan.Conflicts {
        outcome := e.executeConflict(ctx, action)
        collectOutcome(outcome)
    }

    // Phase 8: Synced updates (batch — no I/O, just baseline updates)
    for _, action := range plan.SyncedUpdates {
        outcome := e.executeSyncedUpdate(action)
        collectOutcome(outcome)
    }

    // Phase 9: Cleanups (batch)
    for _, action := range plan.Cleanups {
        collectOutcome(Outcome{
            Action:  ActionCleanup,
            Success: true,
            Path:    action.Path,
        })
    }

    return outcomes, nil
}
```

**Key properties**:
- No database writes. Workers collect Outcomes via a mutex-protected slice.
- SQLITE_BUSY is structurally impossible: no DB writes happen during execution.
- The `collectOutcome` callback is safe for concurrent use by worker pool goroutines.
- Each outcome is self-contained: it has everything the baseline manager needs.

**Error classification in Outcomes** (four-tier model from architecture.md):

| Tier | Examples | Outcome.Error | Retry? |
|---|---|---|---|
| **Fatal** | Auth failure (401 after refresh), impossible state | Stops entire cycle | No |
| **Retryable** | Network timeout, HTTP 429/500/502/503/504/408 | Exponential backoff with jitter within executor | Yes (max 5) |
| **Skip** | Permission denied (403), invalid filename (400), locked (423) | `Outcome{Success: false, Error: ...}` | No |
| **Deferred** | Parent dir not yet created, file locked locally | Queued for retry at end of cycle | Once |

**Retry strategy** (fixes B-048: retryable errors currently treated as skip):
- Base: 1 second, Factor: 2 (exponential), Max backoff: 120 seconds
- Jitter: ±25% of calculated backoff
- For HTTP 429: Use `Retry-After` header directly
- Global rate awareness: shared token bucket across all workers prevents thundering herd
- Retries happen WITHIN the executor (before producing the Outcome), not after

**Download execution detail**:
1. Check malware flag on remote item (skip if flagged)
2. Check disk space (S6: `min_free_space`, default 1GB)
3. Create `.partial` file in target directory
4. Stream content, computing QuickXorHash via `io.TeeReader`
5. Verify hash matches expected (special handling for iOS `.heic` — known API bug)
6. Set file timestamps (`os.Chtimes` with remote mtime)
7. Atomic rename `.partial` → target (S3 safety invariant)
8. Produce Outcome with verified hashes and server metadata

**Upload execution detail**:
- Files ≤4MB: simple PUT upload
- Files >4MB: resumable upload session
  - Chunk size must be multiple of 320 KiB (default 10 MiB)
  - Zero-byte files ALWAYS use simple upload (sessions require non-empty chunks)
  - Include `fileSystemInfo` in session creation (preserves mtime, avoids double-versioning on Business)
  - Session persisted to `upload_sessions` table BEFORE starting (for crash recovery)
  - Fragment upload URLs are pre-authenticated — do NOT send Authorization header
  - Session deleted from table on completion
- Post-upload: compare local hash vs server response hash
  - If hashes match: normal Outcome
  - If hashes diverge AND SharePoint library: enrichment detected, store per-side hashes
  - If hashes diverge AND NOT SharePoint: log warning (potential corruption)

**Remote deletion execution**:
- Uses If-Match ETag header for conditional delete
- HTTP 404: already deleted remotely (not an error)
- HTTP 403: permission denied (SharePoint retention policy) — skip
- HTTP 423: locked (SharePoint) — skip
- HTTP 412: ETag stale — fetch fresh item, retry once with new ETag
- `config.UseRecycleBin`: delete to OneDrive recycle bin (default) vs permanent

**Local deletion execution** (S4: hash-before-delete guard):
1. Compute current local hash
2. Compare against baseline local_hash
3. If hashes match: safe to delete (file unchanged since sync)
4. If hashes differ: local was modified — back up as conflict copy, not delete
5. `config.UseLocalTrash`: use OS trash (FreeDesktop / macOS Finder) vs permanent delete

### 5.9 Baseline Manager

The baseline manager is the sole writer to the database. It loads the baseline at cycle start and commits outcomes at cycle end.

```go
type BaselineManager struct {
    db     *sql.DB
    cached *Baseline
    logger *slog.Logger
}

// Load reads the entire baseline table into memory.
// Called once at engine start and after each commit.
func (m *BaselineManager) Load(ctx context.Context) (*Baseline, error) {
    baseline := &Baseline{
        ByPath: make(map[string]*BaselineEntry),
        ByID:   make(map[string]*BaselineEntry),
    }

    rows, err := m.db.QueryContext(ctx,
        `SELECT path, drive_id, item_id, parent_id, item_type,
                local_hash, remote_hash, size, mtime, synced_at, etag
         FROM baseline`)
    if err != nil { return nil, err }
    defer rows.Close()

    for rows.Next() {
        var e BaselineEntry
        err := rows.Scan(&e.Path, &e.DriveID, &e.ItemID, &e.ParentID,
            &e.ItemType, &e.LocalHash, &e.RemoteHash,
            &e.Size, &e.Mtime, &e.SyncedAt, &e.ETag)
        if err != nil { return nil, err }

        baseline.ByPath[e.Path] = &e
        baseline.ByID[e.ItemID] = &e
    }

    m.cached = baseline
    return baseline, nil
}

// Commit applies outcomes to the baseline table and saves the delta token,
// all in a single atomic transaction.
func (m *BaselineManager) Commit(
    ctx context.Context,
    outcomes []Outcome,
    deltaToken string,
    driveID string,
) error {
    tx, err := m.db.BeginTx(ctx, nil)
    if err != nil { return err }
    defer tx.Rollback()

    now := time.Now().UnixNano()

    for _, o := range outcomes {
        if !o.Success { continue }

        switch o.Action {
        case ActionDownload, ActionUpload, ActionUpdateSynced, ActionFolderCreate:
            _, err = tx.ExecContext(ctx,
                `INSERT INTO baseline
                    (path, drive_id, item_id, parent_id, item_type,
                     local_hash, remote_hash, size, mtime, synced_at, etag)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                 ON CONFLICT(path) DO UPDATE SET
                    drive_id=excluded.drive_id, item_id=excluded.item_id,
                    parent_id=excluded.parent_id,
                    item_type=excluded.item_type,
                    local_hash=excluded.local_hash, remote_hash=excluded.remote_hash,
                    size=excluded.size, mtime=excluded.mtime,
                    synced_at=excluded.synced_at,
                    etag=excluded.etag`,
                o.Path, o.DriveID, o.ItemID, o.ParentID, o.ItemType,
                o.LocalHash, o.RemoteHash, o.Size, o.Mtime, now, o.ETag)

        case ActionLocalMove, ActionRemoteMove:
            // Move = delete old path entry + insert at new path.
            _, err = tx.ExecContext(ctx,
                `DELETE FROM baseline WHERE path = ?`, o.OldPath)
            if err != nil { return fmt.Errorf("commit move delete for %s: %w", o.OldPath, err) }
            _, err = tx.ExecContext(ctx,
                `INSERT INTO baseline
                    (path, drive_id, item_id, parent_id, item_type,
                     local_hash, remote_hash, size, mtime, synced_at, etag)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
                o.Path, o.DriveID, o.ItemID, o.ParentID, o.ItemType,
                o.LocalHash, o.RemoteHash, o.Size, o.Mtime, now, o.ETag)

        case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
            _, err = tx.ExecContext(ctx,
                `DELETE FROM baseline WHERE path = ?`, o.Path)

        case ActionConflict:
            // Record conflict in ledger
            _, err = tx.ExecContext(ctx,
                `INSERT INTO conflicts (id, drive_id, item_id, path, conflict_type,
                    detected_at, local_hash, remote_hash, resolution)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'unresolved')`,
                generateID(), o.DriveID, o.ItemID, o.Path, o.ConflictType,
                now, o.LocalHash, o.RemoteHash)
            // Also update baseline if keep-both resolution created files
            // (handled by conflict executor)
        }

        if err != nil { return fmt.Errorf("commit outcome for %s: %w", o.Path, err) }
    }

    // Save delta token in the same transaction
    if deltaToken != "" {
        _, err = tx.ExecContext(ctx,
            `INSERT INTO delta_tokens (drive_id, token, updated_at)
             VALUES (?, ?, ?)
             ON CONFLICT(drive_id) DO UPDATE SET token=excluded.token, updated_at=excluded.updated_at`,
            driveID, deltaToken, now)
        if err != nil { return fmt.Errorf("save delta token: %w", err) }
    }

    // Optionally write to change journal (for debugging)
    // ... append journal entries ...

    if err := tx.Commit(); err != nil {
        return fmt.Errorf("commit transaction: %w", err)
    }

    // Refresh in-memory cache
    m.cached, err = m.Load(ctx)
    return err
}
```

**Key properties**:
- Single writer to the database (no concurrency issues)
- Atomic transaction: all outcomes + delta token commit together or none do
- After commit, the in-memory baseline is refreshed for the next cycle
- The delta token is NEVER saved separately from the baseline. If the process crashes during execution, the token is not advanced.

### 5.10 Engine (Orchestrator)

The engine wires all components together and provides the `RunOnce` and `RunWatch` entry points.

```go
type Engine struct {
    remoteObs   *RemoteObserver
    localObs    *LocalObserver
    buffer      *ChangeBuffer
    planner     *Planner
    executor    *Executor
    baselineMgr *BaselineManager

    driveID     string
    syncRoot    string
    mode        SyncMode
    config      *config.ResolvedDrive
    logger      *slog.Logger
}

// RunOnce executes a single sync cycle.
func (e *Engine) RunOnce(ctx context.Context, mode SyncMode, opts SyncOptions) (*SyncReport, error) {
    startedAt := time.Now().UnixNano()

    // Step 1: Load baseline
    baseline, err := e.baselineMgr.Load(ctx)
    if err != nil { return nil, fmt.Errorf("load baseline: %w", err) }

    // Update observers with current baseline
    e.remoteObs.baseline = baseline
    e.localObs.baseline = baseline

    // Step 2: Fetch remote changes (skip for upload-only)
    var deltaToken string
    if mode != SyncUploadOnly {
        savedToken, _ := e.baselineMgr.GetDeltaToken(ctx, e.driveID)
        remoteEvents, token, err := e.remoteObs.FullDelta(ctx, savedToken)
        if err != nil { return nil, fmt.Errorf("delta fetch: %w", err) }
        e.buffer.AddAll(remoteEvents)
        deltaToken = token
    }

    // Step 3: Scan local changes (skip for download-only)
    if mode != SyncDownloadOnly {
        localEvents, err := e.localObs.FullScan(ctx)
        if err != nil { return nil, fmt.Errorf("local scan: %w", err) }
        e.buffer.AddAll(localEvents)
    }

    // Step 4: Plan (pure function)
    changes := e.buffer.FlushImmediate()
    safetyCfg := NewSafetyConfig(e.config)
    plan, err := e.planner.Plan(changes, baseline, mode, safetyCfg)
    if err != nil { return nil, fmt.Errorf("plan: %w", err) }

    // Step 5: Dry-run gate
    if opts.DryRun {
        return buildDryRunReport(plan, startedAt, mode), nil
    }

    // Step 6: Execute
    outcomes, err := e.executor.Execute(ctx, plan)
    if err != nil { return nil, fmt.Errorf("execute: %w", err) }

    // Step 7: Commit baseline + delta token atomically
    if err := e.baselineMgr.Commit(ctx, outcomes, deltaToken, e.driveID); err != nil {
        return nil, fmt.Errorf("commit: %w", err)
    }

    return buildReport(outcomes, startedAt, mode), nil
}

// RunWatch runs continuous sync, processing changes as they arrive.
func (e *Engine) RunWatch(ctx context.Context, mode SyncMode) error {
    baseline, err := e.baselineMgr.Load(ctx)
    if err != nil { return fmt.Errorf("load baseline: %w", err) }

    e.remoteObs.baseline = baseline
    e.localObs.baseline = baseline

    // Get saved delta token for remote observer
    savedToken, _ := e.baselineMgr.GetDeltaToken(ctx, e.driveID)

    // Start observers in background
    go func() {
        if mode != SyncUploadOnly {
            e.remoteObs.Watch(ctx, savedToken, func(event ChangeEvent) {
                e.buffer.Add(event)
            })
        }
    }()

    go func() {
        if mode != SyncDownloadOnly {
            e.localObs.Watch(ctx, func(event ChangeEvent) {
                e.buffer.Add(event)
            })
        }
    }()

    // Process batches as they arrive
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-e.buffer.Ready():
            changes := e.buffer.Flush()
            if len(changes) == 0 { continue }

            baseline = e.baselineMgr.Cached()
            e.remoteObs.baseline = baseline
            e.localObs.baseline = baseline

            safetyCfg := NewSafetyConfig(e.config)
            plan, err := e.planner.Plan(changes, baseline, mode, safetyCfg)
            if err != nil {
                e.logger.Error("watch: plan failed", "error", err)
                continue
            }

            outcomes, err := e.executor.Execute(ctx, plan)
            if err != nil {
                e.logger.Error("watch: execute failed", "error", err)
                continue
            }

            if err := e.baselineMgr.Commit(ctx, outcomes, "", e.driveID); err != nil {
                e.logger.Error("watch: commit failed", "error", err)
                continue
            }
        }
    }
}
```

---

## Part 6: How E Addresses Each Fault Line

| # | Fault Line | Current Root Cause | E's Solution |
|---|---|---|---|
| 1 | **Tombstone split** | Scanner and delta fight over shared mutable DB rows. Scanner creates `local:` rows when it can't see tombstoned rows. | Remote and local observers produce independent event streams. Neither touches the database. The planner sees both events for the same path in a single `PathChanges` struct. No DB rows to conflict. Structurally impossible. |
| 2 | **`local:` ID lifecycle** | Scanner invents `local:` prefixed IDs because it writes to an `(drive_id, item_id)`-keyed table. Every module must handle the fake IDs. | `LocalEntry` and `ChangeEvent` from local source have no `ItemID` field (it's empty). Local changes are keyed by path. The baseline maps paths to item IDs. After upload, the real server ID comes from the API response and goes into the Outcome → baseline commit. No temporary IDs ever exist. |
| 3 | **SQLITE_BUSY** | Concurrent transfer workers write to DB via `store.UpsertItem`. Without `busy_timeout`, concurrent writes return SQLITE_BUSY immediately. | No DB writes during execution. Transfer workers produce Outcomes (in-memory structs). A mutex protects the outcomes slice — Go mutex, not SQLite. The baseline manager is the sole DB writer, running sequentially after execution completes. |
| 4 | **Folder lifecycle** | `isSynced()` checks `SyncedHash != ""` which is always empty for folders. `executeSyncedUpdate` never sets `LastSyncedAt` for folders. D1 is dead code. | `hasBaseline` is `v.Baseline != nil`. If a folder has a baseline entry, it was synced. No hash check. ED1 fires correctly for all synced folders. The bug is structurally eliminated by the type system. |
| 5 | **Phase ordering** | Delta writes to DB in Phase 1, scanner writes in Phase 2, both before reconciliation in Phase 3. Dry-run writes 24+ items and a delta token to DB. | No intermediate DB writes. Delta produces events. Scanner produces events. Planner operates on events + baseline (read-only). Dry-run stops before execution. Zero side effects in any phase before Execute. |
| 6 | **Filter asymmetry** | Filter only runs in the scanner (`scanner.go:250`). Remote items from delta are never filtered. | The planner applies the filter to both remote-only items (new downloads) and local-only items (new uploads) symmetrically. The filter is checked in the classification step, not in the observation step. |

### 6.1 Move Detection Without Tombstones

The current design uses 30-day tombstones in the `items` table for remote move detection: when the delta reports an item deleted at path A and appearing at path B with the same item ID, the tombstone at A provides the "old path" signal.

In Alternative E, tombstones are eliminated from the baseline. Move detection works through a three-component pipeline:

**Remote moves** — Observer + ChangeBuffer + Planner:
1. The Remote Observer detects the move during `convertToChangeEvent`: the delta API reports the same `item_id` at a new location, and `baseline.ByID[item.ID]` still points to the old path. The observer produces a `ChangeEvent{Type: ChangeMove, Path: newPath, OldPath: oldPath}`.
2. The ChangeBuffer receives this event and dual-keys it: the ChangeMove event is stored under `newPath`, and a synthetic `ChangeDelete` is created at `oldPath`. This ensures the Planner sees both paths.
3. The Planner's `detectMoves` function finds ChangeMove events in the remote events and produces `ActionLocalMove` actions (rename the local file from old path to new path). Both paths are marked as "involved" so they're excluded from the per-path classification step.

No tombstone needed because the baseline IS the "old state" — it hasn't been updated yet (it's read-only during observation).

**Local moves** — Planner hash-based correlation:
1. The Local Observer emits a `ChangeDelete` event at the old path (baseline entry exists, file absent) and a `ChangeCreate` event at the new path (no baseline, new file present).
2. The Planner's `detectMoves` function correlates these: a locally-deleted item whose `baseline.LocalHash` matches a locally-created item's hash (unique match constraint) is combined into a single `ActionRemoteMove` (tell the server about the rename).
3. Same as current design, but comparing against in-memory data instead of DB queries.

**Why tombstones were needed before but not now**: In the current architecture, the delta processor eagerly writes to the DB. A deletion tombstones the row, and a creation at a new path creates a new row. The reconciler must look at the tombstoned row to see the "old" state. In E, the delta produces events against a frozen baseline — the baseline entry at the old path is still there, providing the "before" view naturally.

---

## Part 7: Crash Recovery

| Crash Point | What Happened | Recovery |
|---|---|---|
| During `LoadBaseline` | Nothing changed. | Re-read baseline. |
| During `FetchRemote` | Events collected in memory. No DB writes. | Re-run cycle. Delta re-fetched from saved token. |
| During `ScanLocal` | Events collected in memory. No DB writes. | Re-run cycle. Filesystem re-scanned. |
| During `Plan` | Pure function. No state. | Re-run cycle. |
| During `Execute` | Some actions completed on disk/API. Outcomes collected in memory. Baseline NOT updated. Delta token NOT advanced. | Re-run cycle. Delta returns same changes (token not advanced). Scanner sees completed downloads. Planner produces EF4/EF11 (convergent edit/create → update synced) for items that are now identical. No duplicate work. No data loss. |
| During `Commit` | SQLite transaction. Either commits or rolls back. | If rolled back: same as "during Execute." If committed: success. |
| During `Watch` (between cycles) | Buffer has pending events. | Events are re-observed by the watchers. Debounce/dedup handles redundancy. |

**Worst case**: A 30-minute initial sync crashes at minute 29 during execution. All 29 minutes of downloads are on disk but the baseline is not updated. On restart, the full delta is re-fetched (same token), the scanner sees the downloaded files, and the planner quickly classifies them as EF4/EF11 (both sides identical → update synced). The commit writes the baseline. Time wasted: a few minutes of re-classification, not 29 minutes of re-downloading.

**Mitigation for very large initial syncs**: Batch processing with intermediate baseline commits (same approach as D). After each batch of N items, commit a partial baseline. This bounds the re-work window.

**Upload session resume**: The `upload_sessions` table persists session state BEFORE the upload begins. On crash recovery:
1. Load all active upload sessions from `upload_sessions` table
2. Check each session's expiry (`expiry` field, typically 48 hours from creation)
3. Expired sessions: delete from table, file will be re-uploaded next cycle
4. Valid sessions: resume from `bytes_uploaded` offset — the API supports this natively
5. On successful completion: delete session from table, include in Outcomes for baseline commit

**`.partial` file cleanup**: On startup, glob for `**/*.partial` in the sync root and remove them. These are incomplete downloads that are safe to delete — they'll be re-downloaded.

**Delta token integrity**: If the process crashes during execution, the delta token is NOT advanced (it's only saved in the Commit transaction). On restart, the same delta is re-fetched. This is idempotent because:
- Files already downloaded are found by the scanner and classified as EF4/EF11 (convergent → update synced)
- Files already uploaded appear in the next delta as new remote items with matching hashes
- Files partially transferred are cleaned up (partial downloads) or resumed (upload sessions)

---

## Part 8: Memory Analysis

### 8.1 Per-Item Sizes

| Type | Estimated Size | Notes |
|---|---|---|
| `ChangeEvent` | ~280 bytes | strings + ints |
| `BaselineEntry` | ~200 bytes | path + IDs + hashes + metadata |
| `PathView` | ~24 bytes | three pointers |
| `RemoteState` | ~200 bytes | derived from event |
| `LocalState` | ~120 bytes | path + hash + metadata |
| `Outcome` | ~250 bytes | self-contained result |

### 8.2 One-Shot Mode (100K items, initial sync)

| Component | Count | Memory |
|---|---|---|
| Baseline (empty on first run) | 0 | 0 MB |
| Remote events | 100,000 | ~27 MB |
| Local events (empty on first run) | 0 | 0 MB |
| PathViews | 100,000 | ~2.4 MB |
| RemoteState | 100,000 | ~19 MB |
| Action plan | 100,000 | ~5 MB |
| Outcomes | 100,000 | ~24 MB |
| **Peak** | | **~77 MB** |

Within the PRD budget of `< 100 MB` for 100K files.

### 8.3 One-Shot Mode (100K items, steady state)

| Component | Count | Memory |
|---|---|---|
| Baseline | 100,000 | ~19 MB |
| Remote events (delta changes only) | ~100 | ~0.03 MB |
| Local events (changed files only) | ~50 | ~0.01 MB |
| PathViews | ~150 | ~0.004 MB |
| **Peak** | | **~20 MB** |

Dramatically lower in steady state because the delta returns only changes.

### 8.4 Watch Mode (100K items, steady state)

| Component | Count | Memory |
|---|---|---|
| Baseline (cached) | 100,000 | ~19 MB |
| Pending events in buffer | ~10 | ~0.003 MB |
| PathViews per batch | ~10 | ~0.0002 MB |
| **Sustained** | | **~20 MB** |

Watch mode processes individual changes, not full snapshots. Memory is proportional to the cached baseline, not to the number of pending changes.

### 8.5 Large Drives (500K items, initial sync)

| Component | Count | Memory |
|---|---|---|
| Remote events | 500,000 | ~134 MB |
| PathViews + RemoteState | 500,000 | ~107 MB |
| **Peak** | | **~241 MB** |

Exceeds the 100 MB target. Mitigation: batch processing during initial sync. Process delta pages in batches of 50K items, committing intermediate baselines.

---

## Part 9: What Gets Reused

**Important**: The delete-first strategy (Increment 0) deletes all old `internal/sync/` code before building the new engine. The "reuse" described below is **conceptual** — design patterns, algorithms, and architectural insights carry forward into freshly written code. No old sync code is adapted or refactored in place.

### Packages retained as-is

| Package | Reuse | Notes |
|---|---|---|
| `internal/graph/` | **100%** | No changes. All API client code, auth, normalization, types. |
| `internal/config/` | **~99%** | `tombstone_retention_days` removed (Option E eliminates tombstones). Everything else unchanged. |
| `pkg/quickxorhash/` | **100%** | No changes. |
| `cmd/onedrive-go/` | **~90%** | `sync.go` rewired to new engine API. All other commands unchanged. |
| `e2e/` | **~95%** | E2E tests exercise the CLI, not internal types. Minor adjustments. |

### Design patterns carried forward into new code

These percentages describe how much of each new component's *design logic* draws from patterns proven in the old engine. The code itself is written from scratch.

| Design pattern | Influence | How it informs new code |
|---|---|---|
| Filter engine | **High** | Same `Filter` interface and three-layer cascade. New planner applies it symmetrically. |
| Transfer pipeline | **High** | Worker pool patterns, bandwidth limiting, hash verification. New executor produces Outcomes instead of DB writes. |
| Safety invariants (S1-S7) | **High** | Same invariants, now implemented as pure functions on baseline + plan. |
| Decision matrix (F1-F14, D1-D7) | **High** | Same reconciliation logic, reorganized as EF1-EF14 and ED1-ED8 on PathView inputs. |
| Delta normalization | **Moderate** | API quirk handling (driveID casing, URL-decode, Prefer header, dedup) informs Remote Observer. |
| Filesystem walk/hash | **Moderate** | Walk patterns, NFC normalization, racily-clean handling inform Local Observer. |
| SQLite infrastructure | **Low** | Migration runner, pragma setup reused. Schema and CRUD completely new. |

---

## Part 10: Implementation Sequence

**Note**: This is a high-level sequence. The authoritative implementation plan is in [roadmap.md](../../docs/roadmap.md) (Phase 4 v2, increments 4v2.0-4v2.8). All old sync code is deleted in Increment 0 (clean slate); everything below is written from scratch.

### Phase 1: Foundation (1-2 increments)

1. Define all new types: `ChangeEvent`, `BaselineEntry`, `PathView`, `RemoteState`, `LocalState`, `Outcome`
2. Create fresh `baseline` table schema (no migration from old `items` table — app has no users)
3. Implement `BaselineManager.Load()` and `BaselineManager.Commit()`
4. Write comprehensive tests for type conversions and baseline CRUD

### Phase 2: Observers (2-3 increments)

5. Write `RemoteObserver.FullDelta()` — produce ChangeEvents from delta API responses (informed by delta normalization patterns)
6. Write `LocalObserver.FullScan()` — produce ChangeEvents from filesystem walk (informed by walk/hash patterns)
7. Implement `ChangeBuffer` with debounce/dedup/batch
8. Write tests for each observer independently (no DB needed — pure output)

### Phase 3: Planner (2-3 increments)

9. Implement `Merge` (change events + baseline → PathViews)
10. Implement `classifyFile` (EF1-EF14) and `classifyFolder` (ED1-ED8) on PathView inputs
11. Implement `detectMoves` (remote moves from ChangeMove events + local moves via hash correlation)
12. Implement safety checks as pure functions on ActionPlan + Baseline
13. Write exhaustive table-driven tests for the planner (every matrix cell)

### Phase 4: Executor (1-2 increments)

14. Write executor that produces Outcomes (informed by transfer pipeline patterns)
15. Worker pools collect Outcomes via callback
16. Executor uses `PathView` context — no database queries

### Phase 5: Engine Wiring (1 increment)

17. Implement `Engine.RunOnce()` — wire all components
18. Implement dry-run (stop before Execute)
19. Wire CLI `sync` command to new engine

### Phase 6: Watch Mode (2-3 increments)

20. Implement `RemoteObserver.Watch()` (polling-based, WebSocket later)
21. Implement `LocalObserver.Watch()` (rjeczalik/notify)
22. Implement `Engine.RunWatch()` — event loop with buffer
23. Implement pause/resume (buffer accumulates, executor paused)

### Phase 7: Validation (1-2 increments)

24. Run full E2E suite against new architecture
25. Fix any integration issues
26. Performance benchmarking (memory, CPU, latency)
27. Change journal implementation (optional debugging aid)

**Total: ~10-14 increments.**

---

## Part 11: Comparison with All Alternatives

| Criterion | A | B | C | D | **E** |
|---|---|---|---|---|---|
| **Eliminates all 6 fault lines** | No (3/6) | Yes (5/6) | Yes (6/6) | Yes (6/6) | **Yes (6/6)** |
| **Watch mode native** | No | No | No | No | **Yes** |
| **Pause/resume native** | No | No | No | No | **Yes** |
| **Type safety** | None | Moderate | Strong | Maximum | **Maximum** |
| **Testability** | Minimal | Moderate | Major | Maximum | **Maximum** |
| **Crash recovery** | Resume from partial | Resume from partial | Re-run cycle | Re-run cycle | **Re-run cycle** |
| **Memory (100K steady)** | ~0 MB extra | ~0 MB extra | ~80 MB | ~80 MB | **~20 MB** |
| **Memory (100K initial)** | ~0 MB extra | ~0 MB extra | ~80 MB | ~80 MB | **~77 MB** |
| **DB writes per cycle** | Many | Many | Few | One txn | **One txn** |
| **Delta token safety** | Before exec | Before exec | With exec | Same txn | **Same txn** |
| **Dry-run side effects** | Partial | Yes | None | None | **None** |
| **Filter symmetry** | Partial | JOIN-time | Merge-time | Reconcile-time | **Plan-time** |
| **Incremental processing** | No | No | No | No | **Yes** |
| **Audit trail** | No | No | No | No | **Change journal** |
| **Effort** | 2-3 incr | 4-5 incr | 6-8 incr | 8-10 incr | **10-14 incr** |
| **Design pattern reuse** | 90% | 60% | 40% | 30% | **~60%** (conceptual; code written from scratch) |

---

## Part 12: Design Decisions

| # | Decision | Rationale |
|---|---|---|
| E1 | Events are the coordination mechanism, not the database | Eliminates all shared-mutable-state bugs. Enables watch mode. |
| E2 | Baseline is the only durable per-item state | Remote and local observations are ephemeral — rebuilt each cycle. Only confirmed sync state needs persistence. |
| E3 | Delta token commits with baseline | Prevents token-advancement-without-execution crash bug. |
| E4 | Per-side hash baselines (`local_hash`, `remote_hash`) | Handles SharePoint enrichment natively without special code paths. |
| E5 | No `local:` IDs | Local observations are keyed by path. Remote observations have server IDs. The baseline maps between them. |
| E6 | No tombstones in baseline | Baseline only stores confirmed synced state. Deletions remove the row. Remote tombstones are events, not persistent state. |
| E7 | Folders use existence-based reconciliation | `Baseline != nil` means "was synced." No hash check. Eliminates the `isSynced()` bug. |
| E8 | Planner is pure functions | No I/O, no DB access. Maximum testability. Every decision is deterministic and reproducible. |
| E9 | Executor produces Outcomes, not DB writes | Decouples execution from persistence. Enables atomic commit. Eliminates SQLITE_BUSY. |
| E10 | Change buffer with debounce | Prevents processing the same file multiple times during rapid edits. Groups events by path for planner. |
| E11 | Same code path for one-shot and watch | One-shot = "observe everything, process as one batch." Watch = "observe incrementally, process as small batches." Same planner, same executor, same baseline manager. |
| E12 | Change journal is append-only and optional | Debugging aid, not a functional requirement. Can be enabled/disabled without affecting correctness. |
| E13 | Safety checks are pure functions in the planner | S1-S7 invariants operate on ActionPlan + Baseline, not on DB queries. Testable without a database. |
| E14 | Action carries PathView context | Each action has the full three-way context (remote, local, baseline) so the executor never needs to query the database. |
| E15 | DriveID normalization at observer boundary | All driveIDs are lowercased and zero-padded to 16 chars at the API boundary. Downstream code never sees inconsistent casing. Prevents the entire class of driveID mismatch bugs documented in tier1 research. |
| E16 | Retries happen inside the executor, not after | Fixes B-048 (retryable errors silently abandoned). The executor retries with exponential backoff before producing the final Outcome. Failed Outcomes mean "gave up after max retries." |
| E17 | No tombstones — baseline IS the "old state" | Remote move detection compares delta events against the frozen baseline. The baseline entry at the old path provides the "before" view naturally. Eliminates tombstone management, retention, cleanup. |
| E18 | Batch processing for large initial syncs | Process in 50K-item batches with intermediate baseline commits. Bounds memory to ~50 MB even for 500K-item drives. |
| E19 | Two-signal graceful shutdown | First signal: drain + checkpoint. Second signal: immediate exit. WAL ensures consistency. Same protocol as current architecture. |
| E20 | Filter applied in Planner, not observers | Filters are checked during classification, not during observation. This ensures both remote AND local items are filtered symmetrically (fixes Fault Line 6) and that filter changes can be hot-reloaded without restarting observers. |
| E21 | Conflict copies use timestamp naming | `file.conflict-YYYYMMDD-HHMMSS.ext` — self-documenting, shorter than hostname-based. |

---

## Part 13: Risks and Mitigations

| Risk | Severity | Mitigation |
|---|---|---|
| Memory budget exceeded during initial sync of very large drives (500K+) | Medium | Batch processing: commit intermediate baselines every 50K items during initial sync. Steady-state memory is ~20 MB regardless of drive size. |
| Re-running entire cycle on crash wastes work | Low | For initial sync, intermediate baseline commits bound the re-work window. For steady-state, cycles are fast (seconds). |
| Watch mode event storms (rapid file changes) | Medium | Debounce window (2 seconds). Deduplication in change buffer. Backpressure: if executor is still running, buffer accumulates. |
| Path materialization in remote observer needs parent chain | Low | Maintain in-flight parent map during delta processing. Fall back to baseline for parents not in current delta. |
| Baseline cache may become stale between cycles in watch mode | Low | Refresh baseline from DB after each commit. The baseline manager owns the cache lifecycle. |
| ~80% test rewrite | Medium | New tests are simpler (pure function tests vs mock-heavy integration tests). E2E tests are ~95% reusable. Net test count likely increases. |
| 10-14 increments of effort | Acceptable | No users, no launch, unlimited effort. Building for the long term. |
| Shared folder edge cases | Medium | Cross-drive parent references, inconsistent driveIDs, missing items in delta. Mitigated by thorough driveID normalization and `Prefer` header. Requires dedicated E2E tests with shared folder scenarios. |
| Case-insensitive collision on Linux | Low | OneDrive is case-insensitive, Linux is not. Two local files differing only in case will collide remotely. Mitigated by collision detection in the Planner using case-folded path index. |
| NFS/network filesystem in watch mode | Low | inotify is unreliable on network filesystems. Mitigated by detecting network FS and falling back to periodic full scan with configurable interval. |
| Delta response race conditions | Low | Files modified during paginated delta response can appear inconsistent across pages. Mitigated by dedup (keep last occurrence per item ID) and idempotent commit model. |
| National cloud delta support | Low | US Gov, Germany, China clouds don't support delta API. Future concern — fall back to `/children` enumeration (already needed for parallel initial enum). |

---

## Part 14: Non-Goals

These are explicitly NOT part of Alternative E:

1. **Event sourcing as the primary data model**: The change journal is a debugging aid, not the source of truth. The baseline table is the source of truth. We are not building a CQRS system.
2. **Distributed sync**: E is designed for a single process syncing one or more drives. It does not address multi-device coordination (that's the Graph API's job).
3. **Sub-file delta transfers**: The Graph API operates on whole files. Block-level transfers are not supported.
4. **Backward compatibility with the current `items` table**: Clean break. No users, no migration. Old table deleted in Increment 0.
5. **Real-time streaming reconciliation**: Events are batched (debounce window), not processed individually. This is simpler and avoids race conditions.

---

## Part 15: Safety Invariants (S1-S7) in Alternative E

The seven safety invariants from the architecture spec are preserved in E, but their implementation is cleaner because they operate on typed data structures rather than DB queries.

| ID | Invariant | Current Implementation | E's Implementation |
|----|-----------|----------------------|-------------------|
| **S1** | Never delete remote on local absence without synced base | Orphan detection checks `synced_hash IS NOT NULL` in DB query | Planner checks `view.Baseline != nil` before emitting `ActionRemoteDelete`. If no baseline exists, the item was never synced — local absence is not a deletion signal. Structurally enforced by type system: `PathView.Baseline == nil` means "never synced." |
| **S2** | Never process deletions from incomplete enumeration | Delta token saved only after complete response (all pages through deltaLink) | Same: Remote Observer returns the new delta token alongside events. The token is only passed to Commit after execution succeeds. Additionally, the `.nosync` guard fires in the Local Observer before any events are produced. |
| **S3** | Atomic file writes for downloads | `.partial` + hash verify + atomic rename | Identical: Executor downloads to `.partial`, verifies QuickXorHash, atomically renames. The Outcome contains the verified hash. |
| **S4** | Hash-before-delete guard | Executor computes local hash before deleting, backs up if changed | Identical: Executor computes current local hash, compares against `action.View.Baseline.LocalHash`. If hashes differ, creates conflict copy instead of deleting. |
| **S5** | Big-delete protection | SafetyChecker counts deletes vs total items in DB | Planner counts `ActionLocalDelete` + `ActionRemoteDelete` in the plan and compares against baseline size. Pure function: `bigDeleteTriggered(plan, baseline, config) bool`. Threshold: count > 1000 OR percentage > 50% of baseline entries, with minimum items guard (10). |
| **S6** | Disk space check before downloads | Executor checks `min_free_space` before each download | Executor checks available disk space against `config.MinFreeSpace` (default 1GB) before downloading. If insufficient, the download is skipped with a warning and produces a failed Outcome. |
| **S7** | Never upload partial/temp files | Scanner's filter excludes `.partial`, `.tmp`, `.swp`, etc. | Local Observer's filter excludes the same patterns. Additionally, the Planner applies filters symmetrically (Fault Line 6 fix), so remote items matching these patterns are also filtered. |

**Key improvement in E**: Safety invariants S1 and S5 are now pure functions in the Planner, testable without a database. In the current design, the SafetyChecker queries the DB for total item counts — in E, it computes directly from `len(baseline.ByPath)` and `len(plan.LocalDeletes) + len(plan.RemoteDeletes)`.

---

## Part 16: Error Handling & Retry Strategy

### 16.1 Error Flow in Event-Driven Architecture

```
Observer error → abort cycle (delta/scan failure)
Planner error → abort cycle (impossible state)
Executor error → per-item Outcome with Success=false
Commit error → abort cycle (DB failure)
```

**Observer errors** (network failures during delta, filesystem errors during scan) are fatal to the cycle. The cycle is re-run from scratch on the next trigger (one-shot) or after the debounce window (watch mode). This is safe because observers produce no side effects.

**Planner errors** should never occur in practice (it's a pure function on valid inputs). If they do, it indicates a bug — the cycle aborts with a detailed error.

**Executor errors** are per-item. Each action produces an Outcome, which may have `Success: false`. Failed outcomes are NOT committed to the baseline. The item will be retried on the next cycle.

**Commit errors** (SQLite write failure) are extremely rare and indicate a serious problem. The cycle aborts. On restart, the same delta is re-fetched (token not advanced).

### 16.2 Retry Strategy Within Executor

Retries happen INSIDE the executor, before producing the Outcome. This fixes B-048 (retryable errors silently abandoned in current implementation).

```go
func (e *Executor) executeWithRetry(ctx context.Context, op func() (*Outcome, error)) Outcome {
    for attempt := 0; attempt <= maxRetries; attempt++ {
        outcome, err := op()
        if err == nil {
            return *outcome
        }

        tier := classifyError(err)
        switch tier {
        case ErrorFatal:
            return Outcome{Success: false, Error: err}
        case ErrorSkip:
            return Outcome{Success: false, Error: err}
        case ErrorRetryable:
            delay := calculateBackoff(attempt) // 1s, 2s, 4s, 8s, 16s (max 120s)
            if retryAfter := getRetryAfter(err); retryAfter > 0 {
                delay = retryAfter
            }
            select {
            case <-ctx.Done():
                return Outcome{Success: false, Error: ctx.Err()}
            case <-time.After(delay + jitter(delay)):
            }
        }
    }
    return Outcome{Success: false, Error: MaxRetriesExceeded{...}}
}
```

### 16.3 Error Classification (Extended)

| HTTP Status | Classification | Notes |
|---|---|---|
| 400 | Skip | Invalid filename, bad request |
| 401 | Fatal (after token refresh attempt) | Auth failure |
| 403 | Skip | Permission denied, retention policy |
| 404 | Skip (for delete) / Retryable (for GET) | Item may have been deleted concurrently |
| 408 | Retryable | Request timeout |
| 409 | Retryable (move) / Skip (other) | Conflict — for moves, delete target and retry |
| 412 | Retryable | ETag stale — fetch fresh ETag, retry |
| 423 | Skip | Locked (SharePoint) |
| 429 | Retryable | Rate limited — use `Retry-After` header |
| 500-504 | Retryable | Server errors |
| 507 | Fatal | Insufficient storage on server |
| 509 | Retryable (long backoff) | Bandwidth limit exceeded |
| Network error | Retryable | Timeout, connection reset |
| SQLite error | Fatal | Should never happen (no DB writes during execution) |

---

## Part 17: API Quirk Handling

All API quirks are handled in the observers (normalization layer), making them invisible to the Planner and Executor. This follows the existing `internal/graph/` pattern where callers never see raw API data.

### 17.1 DriveID Normalization (Critical)

**The problem**: Microsoft's Graph API returns driveIDs in inconsistent casing across endpoints. Personal accounts use uppercase (`24470056F5C3E43`), some responses have 15-char truncated IDs, and new backend migration introduced `sea8cc6beffdb43d7` format root IDs.

**The solution**: The Remote Observer normalizes ALL driveIDs at the boundary:
- Lowercase
- Zero-pad to 16 characters (handles 15-char truncation bug)
- Store normalized form in baseline

This is the single most impactful defensive measure from the tier1 research.

### 17.2 Shared Folder Handling

Shared folders present unique challenges:
- Items may reference OTHER users' drives in `parentReference.driveId`
- Delta doesn't return shared folder items without `Prefer: deltashowremoteitemsaliasid` header (Personal)
- URL-encoded spaces (`%20`) in path fields for shared items
- Different driveIds for the same shared content viewed from different perspectives

In E, shared folder items are handled by:
1. The Remote Observer includes the `Prefer` header on all Personal delta requests
2. URL-decoding is applied uniformly to all item names
3. Cross-drive references store the item's own driveId (which may differ from the syncing drive)
4. The baseline's `drive_id` field always stores the item's authoritative driveId

### 17.3 SharePoint Enrichment (Detailed)

**The enrichment lifecycle**:
1. User uploads `document.pdf` (local hash = AAA)
2. SharePoint injects library metadata into the file
3. Server reports hash = BBB (differs from AAA)
4. API response may show different size too

**In E's Outcome model**:
```go
// After upload to SharePoint with enrichment:
Outcome{
    Success:    true,
    LocalHash:  "AAA",   // what was on disk (and still is)
    RemoteHash: "BBB",   // what server reports after enrichment
    Size:       serverSize,
    // ... other fields from API response
}
```

The BaselineManager stores both hashes. Next cycle:
- `detectLocalChange`: local AAA == baseline.LocalHash AAA → unchanged
- `detectRemoteChange`: remote BBB == baseline.RemoteHash BBB → unchanged
- Result: EF1 (in sync). No infinite loop. No enrichment-specific code paths.

**Escape hatches** (last resort for unidentified API bugs):
- `disable_download_validation`: skip hash check on downloads
- `disable_upload_validation`: skip hash check on uploads

---

## Part 18: Watch Mode Details

### 18.1 Event Loop Architecture

```
┌─────────────────────────────────────────────────────┐
│                    Engine.RunWatch                    │
│                                                       │
│  ┌──────────┐   ┌──────────┐   ┌──────────────────┐ │
│  │ Remote   │   │ Local    │   │ Config Reload    │ │
│  │ Observer │   │ Observer │   │ (SIGHUP)         │ │
│  │ goroutine│   │ goroutine│   │                  │ │
│  └────┬─────┘   └────┬─────┘   └────┬─────────────┘ │
│       │ emit()       │ emit()       │                │
│       └──────┬───────┘              │                │
│              ▼                      │                │
│  ┌───────────────────┐              │                │
│  │  Change Buffer    │              │                │
│  │  (2s debounce)    │              │                │
│  └────────┬──────────┘              │                │
│           │ Ready()                 │                │
│           ▼                         ▼                │
│  ┌─────────────────────────────────────────────────┐ │
│  │ select {                                        │ │
│  │   case <-buffer.Ready():  → Plan + Execute      │ │
│  │   case <-configReload:    → Re-init filters     │ │
│  │   case <-ctx.Done():      → GracefulShutdown    │ │
│  │ }                                               │ │
│  └─────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
```

### 18.2 Debounce Edge Cases

| Scenario | Handling |
|---|---|
| **Rapid edits** (IDE auto-save) | 2-second debounce window coalesces into single batch |
| **vim atomic save** (delete + create pattern) | Coalesced within debounce window; final state = new file |
| **LibreOffice temp files** | Filtered out by `.~*`, `~*` patterns (S7) |
| **File created and deleted within window** | Both events arrive, but final state is "absent" → no action needed |
| **Large copy operation** (many files) | Events accumulate in buffer; processed as one large batch when debounce fires |

### 18.3 Remote Change Polling

In watch mode, remote changes arrive via polling (WebSocket is a future optimization):
- Default poll interval: 5 minutes (`config.PollInterval`)
- Remote Observer calls `FullDelta` on each tick
- Events emitted to the same buffer as local events
- Debounce window applies: if both local and remote changes arrive within 2s, they're processed together

### 18.4 Graceful Shutdown (Two-Signal Protocol)

| Signal | Action |
|---|---|
| **First SIGINT/SIGTERM** | Stop accepting new events. Drain in-flight executor operations (configurable timeout). Save delta token checkpoint. Clean up `.partial` files. Exit 0. |
| **Second SIGINT/SIGTERM** | Cancel all operations immediately. SQLite WAL ensures DB consistency even on abrupt termination. Exit 1. |
| **SIGHUP** | Reload configuration. Re-initialize filter engine. Detect stale files from filter changes. Update bandwidth limiter settings. Continue running. |

### 18.5 SIGHUP Config Reload

Hot-reloadable options (changed without restart):
- Filter rules (`skip_dotfiles`, `skip_dirs`, `skip_files`, `max_file_size`)
- Bandwidth settings (`bandwidth_limit`)
- Poll interval
- Safety thresholds (`big_delete_threshold`, `min_free_space`)
- Log level

NOT hot-reloadable (require restart):
- `sync_dir`, `drive_id`
- Worker pool sizes (`parallel_downloads`, `parallel_uploads`, `parallel_checkers`)
- Network settings

**Filter change → stale file detection**: When SIGHUP changes filter rules, the engine compares the new filter config against `config_snapshots`. Files that were previously included but are now excluded are recorded in the `stale_files` ledger. The user is warned and can choose to clean them up.

### 18.6 Pause/Resume Detail

```
Pause signal received:
  1. Set engine.paused = true
  2. Observers CONTINUE running (collecting events into buffer)
  3. Buffer CONTINUES accepting events (no debounce timer fires)
  4. Planner/Executor do NOT run

Resume signal received:
  1. Set engine.paused = false
  2. Fire debounce timer immediately
  3. Buffer flushes all accumulated events (potentially large batch)
  4. Plan + Execute + Commit as normal
  5. Normal watch loop resumes

Invariant: buffer must not grow unboundedly during long pause.
Mitigation: if buffer exceeds a high-water mark (e.g., 100K events),
collapse to "do a full sync on resume" flag.
```

---

## Part 19: Initial Sync & Large Drive Handling

### 19.1 Initial Sync Detection

```go
func (e *Engine) isInitialSync(ctx context.Context) bool {
    token, _ := e.baselineMgr.GetDeltaToken(ctx, e.driveID)
    return token == ""
}
```

When no delta token exists, the Graph API delta endpoint returns **every item** in the drive. This is functionally a full enumeration.

### 19.2 Parallel Initial Enumeration (Optimization)

For drives with >100K items, delta-based full enumeration can be slow (sequential pagination). An alternative uses parallel `/children` API calls:

```go
func (o *RemoteObserver) ParallelEnumerate(ctx context.Context) ([]ChangeEvent, error) {
    queue := make(chan string, 256) // parent IDs to walk
    queue <- "root"

    var events []ChangeEvent
    var mu sync.Mutex

    g, ctx := errgroup.WithContext(ctx)
    for i := 0; i < 8; i++ { // 8 walkers (configurable)
        g.Go(func() error {
            for parentID := range queue {
                children, err := o.client.ListChildren(ctx, o.driveID, parentID)
                // ... convert to ChangeEvents, enqueue folders ...
            }
            return nil
        })
    }
    // ... coordination logic ...
}
```

The engine falls back to delta-based enumeration if `/children` enumeration fails or items exceed 300K (Microsoft's recommended delta limit).

### 19.3 Batch Processing for Large Initial Syncs

For drives with many items, holding all events in memory exceeds the 100MB budget. Batch processing bounds memory:

```
For initial sync:
  1. Fetch delta page by page
  2. Every 50K items:
     a. Flush buffer
     b. Plan (only these items)
     c. Execute (downloads/uploads)
     d. Commit partial baseline + intermediate delta token
  3. After all pages: commit final delta token

Memory bounded to: baseline(steady) + 50K events(~14 MB) + plan + outcomes ≈ ~50 MB
```

This ensures the 100MB PRD budget is met even for 500K item drives.

### 19.4 Saving Initial Delta Token

After initial sync completes, the engine needs a delta token representing "now":

```go
// If full enumeration used (not delta), request latest token
if usedParallelEnumeration {
    _, deltaLink := api.Delta(ctx, driveID, "latest")
    token = extractToken(deltaLink)
    // Save in baseline commit transaction
}
```

---

## Part 20: Edge Cases & Platform Considerations

### 20.1 Case-Insensitive Collision Detection

OneDrive is case-insensitive: `File.txt` and `file.txt` are the same item. Linux is case-sensitive: they're different files.

**In E**: The Planner detects case collisions by maintaining a case-folded index of paths during planning. If two PathViews have the same `strings.ToLower(path)` but different actual paths, one is flagged as a conflict.

### 20.2 NFC/NFD Unicode Normalization

macOS APFS stores filenames in NFD (decomposed). Most other systems use NFC (composed). OneDrive does not normalize.

**In E**: All paths in ChangeEvents and BaselineEntries are NFC-normalized. The Local Observer maintains dual paths: `fsRelPath` (filesystem truth for I/O operations) and `dbRelPath` (NFC-normalized for baseline lookup and event emission).

### 20.3 OneNote Files

OneNote files (`.one`, `.onetoc2`, `package` facet) are special:
- Detected via `package.type == "oneNote"` or MIME type or extension
- NOT synced (OneDrive manages them separately via sync API)
- Filtered out in the Remote Observer before event emission

### 20.4 Zero-Byte Files

- Must use simple upload (upload sessions require non-empty first chunk)
- QuickXorHash of empty file is well-defined (empty hash)
- Handled as a special case in the executor's upload logic

### 20.5 File Permissions

OneDrive has no concept of POSIX permissions. In E:
- Downloaded files get configurable permission masks (`sync_dir_permissions`, `sync_file_permissions`)
- Upload doesn't include permission metadata
- The baseline does not store permissions (they're not synced state)

### 20.6 Network Filesystem Considerations

- Sync database must be on LOCAL filesystem (not NFS/network mount)
- inotify is unreliable on NFS — the watch mode falls back to periodic full scan
- The Local Observer detects network filesystem and logs a warning

### 20.7 Container/Docker Considerations

- Don't assume home directory or passwd entries exist
- SSL cert paths may differ
- All paths configurable via environment or config file
- No hardcoded home directory assumptions

---

## Part 21: Verify Command Design

The `verify` command performs a full-tree integrity check against the baseline. Read-only operation with zero side effects.

### 21.1 Verify Algorithm with Baseline Model

```go
func (e *Engine) Verify(ctx context.Context) (*VerifyReport, error) {
    baseline, err := e.baselineMgr.Load(ctx)
    if err != nil { return nil, err }

    report := &VerifyReport{}

    for path, entry := range baseline.ByPath {
        if entry.ItemType != ItemTypeFile { continue }

        fullPath := filepath.Join(e.syncRoot, path)

        // Check 1: File exists locally
        info, err := os.Stat(fullPath)
        if os.IsNotExist(err) {
            report.Missing = append(report.Missing, path)
            continue
        }

        // Check 2: Size matches baseline
        if info.Size() != entry.Size {
            report.SizeMismatch = append(report.SizeMismatch, VerifyMismatch{
                Path: path, Expected: entry.Size, Actual: info.Size(),
            })
        }

        // Check 3: Local hash matches baseline local_hash
        localHash := computeQuickXorHash(fullPath) // via checker pool
        if localHash != entry.LocalHash {
            report.LocalHashMismatch = append(report.LocalHashMismatch, VerifyMismatch{
                Path: path, Expected: entry.LocalHash, Actual: localHash,
            })
        }

        // Check 4: Local hash matches baseline remote_hash
        // (divergence indicates enrichment — informational, not error)
        if entry.RemoteHash != "" && localHash != entry.RemoteHash {
            if localHash == entry.LocalHash {
                // Expected: enriched file, local matches local baseline
                report.Enriched = append(report.Enriched, path)
            } else {
                report.RemoteHashMismatch = append(report.RemoteHashMismatch, VerifyMismatch{
                    Path: path, Expected: entry.RemoteHash, Actual: localHash,
                })
            }
        }

        report.Verified++
    }

    return report, nil
}
```

### 21.2 Periodic Verification (Watch Mode)

```toml
[sync]
verify_interval = "7d"  # Run verify weekly (default: disabled)
```

When enabled, the verify command runs automatically during `--watch` mode. Results are logged and included in the sync report.

---

## Part 22: Configuration Integration

### 22.1 Config Flow

```
TOML config file
    ↓ (parsed by internal/config/)
ResolvedDrive struct
    ↓ (passed to NewEngine)
Engine distributes to components:
    → FilterConfig → FilterEngine → Planner + LocalObserver
    → SafetyConfig → Planner (pure function input)
    → TransfersConfig → Executor + TransferManager
    → PollInterval → RemoteObserver (watch mode)
    → SyncDir, DriveID → Engine fields
```

### 22.2 Config Options That Affect Sync Architecture

| Option | Where It's Used | Notes |
|---|---|---|
| `sync_dir` | Engine, Local Observer, Executor | Sync root path. NOT hot-reloadable |
| `skip_dotfiles` | FilterEngine | Applied in Planner (symmetric) |
| `skip_dirs`, `skip_files` | FilterEngine | Applied in Planner (symmetric) |
| `max_file_size` | FilterEngine | Applied in Planner (symmetric) |
| `sync_paths` | FilterEngine | Scope within drive. Restricts what's observed |
| `parallel_downloads/uploads` | TransferManager | Worker pool sizes. NOT hot-reloadable |
| `parallel_checkers` | LocalObserver | Hash computation pool. NOT hot-reloadable |
| `chunk_size` | Executor | Must be 320 KiB aligned. Default 10 MiB |
| `bandwidth_limit` | BandwidthLimiter | Token bucket. Hot-reloadable |
| `big_delete_threshold` | Planner (safety check) | Hot-reloadable |
| `min_free_space` | Executor | Pre-download check. Hot-reloadable |
| `use_trash` | Executor | Local deletion via OS trash. Hot-reloadable |
| `poll_interval` | RemoteObserver (watch) | Default 5 minutes. Hot-reloadable |
| `tombstone_retention_days` | N/A | **Eliminated in E** — no tombstones in baseline |
| `skip_symlinks` | Local Observer | Default: skip |

### 22.3 Stale File Detection on Filter Changes

When filter configuration changes (via SIGHUP or between runs):

```go
func (e *Engine) detectStaleFiles(ctx context.Context, newFilter Filter) {
    oldSnapshot := e.loadConfigSnapshot(ctx) // from config_snapshots table
    newSnapshot := e.buildConfigSnapshot(newFilter)

    if oldSnapshot.Equal(newSnapshot) { return }

    // Walk baseline: find items that pass old filter but fail new filter
    baseline := e.baselineMgr.Cached()
    for path, entry := range baseline.ByPath {
        if !newFilter.ShouldSync(path, entry.ItemType == ItemTypeFolder, entry.Size).Included {
            e.store.RecordStaleFile(ctx, path, "filter_change", entry.Size)
        }
    }

    e.saveConfigSnapshot(ctx, newSnapshot)
    e.logger.Warn("stale files detected from filter change",
        "count", staleCount, "run `onedrive-go stale` to review")
}
```

---

## Part 23: Multi-Drive & Shared Folder Support

### 23.1 One Engine Per Drive

Each drive gets its own:
- SQLite database file (named by canonical drive ID)
- Engine instance
- Baseline
- Delta token
- Worker pools

Multiple drives sync independently. No cross-drive state sharing.

### 23.2 Drive Database Naming

```
~/.local/share/onedrive-go/personal_toni_outlook.com.db
~/.local/share/onedrive-go/business_user_company.com.db
~/.local/share/onedrive-go/sharepoint_user_company.com_teamsite_Documents.db
```

Canonical ID with colons replaced by underscores for filesystem safety.

### 23.3 Shared Folder Challenges

Shared folders in OneDrive are architecturally complex:
- Items may reference OTHER users' drives in `parentReference.driveId`
- The same item has different `itemId` values when viewed from different drives
- Delta may not return shared items without the `Prefer: deltashowremoteitemsaliasid` header
- Cross-drive parent references mean path materialization must handle multi-drive parent chains

**In E**: Shared items are identified by their `remote_drive_id` and `remote_id` fields in the delta response. The Remote Observer materializes paths within the syncing drive's namespace, not the source drive's namespace. The baseline stores the item using the syncing drive's perspective.

### 23.4 SharePoint Token Sharing

SharePoint document libraries share the business account's OAuth token. The canonical drive ID includes the site and library name, but the token path points to the parent business account's token file.

---

## Part 24: Conflict Resolution Details

### 24.1 Conflict File Naming

Timestamp-based: `<stem>.conflict-YYYYMMDD-HHMMSS.<ext>`

Example: `report.pdf` → `report.conflict-20260222-143052.pdf`

Properties:
- Self-documenting (conflict time visible in filename)
- Shorter than hostname-based alternatives
- Not tied to a specific machine
- Unique per second (sufficient granularity)

### 24.2 Conflict Types and Default Resolutions

| Type | Trigger | Default Resolution |
|---|---|---|
| **Edit-edit** (EF5) | Both sides modified same file, different content | Keep-both: rename local, download remote |
| **Edit-delete** (EF9) | Local edited, remote deleted | Keep-both: preserve local as conflict copy |
| **Create-create** (EF12) | New file at same path on both sides, different content | Keep-both: rename local, download remote |
| **Delete-edit** (EF7) | Local deleted, remote edited | Not a conflict: download remote (remote wins) |

### 24.3 Conflict Ledger Integration

When the Executor resolves a conflict, it produces TWO outcomes:
1. The conflict action outcome (recorded in `conflicts` table via BaselineManager)
2. The baseline update for the downloaded/preserved file

The `conflicts` table provides:
- `onedrive-go conflicts` — list all conflicts with resolution status
- `onedrive-go resolve <id> --keep-local|--keep-remote|--keep-both` — resolve interactively
- Batch mode: `--all --keep-both` for automatic resolution

---

## Part 25: Sync Report Construction

### 25.1 Building Report from Outcomes

```go
func buildReport(outcomes []Outcome, startedAt int64, mode SyncMode) *SyncReport {
    report := &SyncReport{StartedAt: startedAt, Mode: mode}

    for _, o := range outcomes {
        if !o.Success {
            report.Errors = append(report.Errors, SyncError{
                Path: o.Path, Action: o.Action, Error: o.Error,
            })
            continue
        }

        switch o.Action {
        case ActionDownload:
            report.Downloaded++
            report.BytesDownloaded += o.Size
        case ActionUpload:
            report.Uploaded++
            report.BytesUploaded += o.Size
        case ActionLocalDelete:
            report.LocalDeleted++
        case ActionRemoteDelete:
            report.RemoteDeleted++
        case ActionLocalMove, ActionRemoteMove:
            report.Moved++
        case ActionFolderCreate:
            report.FoldersCreated++
        case ActionConflict:
            report.Conflicts++
        case ActionUpdateSynced:
            report.SyncedUpdates++
        case ActionCleanup:
            report.Cleanups++
        }
    }

    report.CompletedAt = NowNano()
    return report
}
```

### 25.2 Output Formats

**Interactive** (human-readable to stderr):
```
Sync complete (drive "personal:toni@outlook.com", bidirectional)
  ↓ 3 downloaded (12.4 MB)    ↑ 2 uploaded (8.1 MB)
  × 1 conflict                 ⊘ 1 deleted locally
  Duration: 4.2s
  Unresolved conflicts: 1 (run `onedrive-go conflicts`)
```

**JSON** (`--json` or `--quiet`): structured `SyncReport` for scripting and monitoring.

---

## Part 26: Testing Strategy for Alternative E

### 26.1 Pure Function Testing (New)

The biggest testing improvement in E: the Planner, merge logic, and safety checks are pure functions testable without any I/O or mocks.

```go
// Example: Testing EF9 (edit-delete conflict) — no mocks, no DB
func TestEF9_EditDeleteConflict(t *testing.T) {
    view := PathView{
        Path:     "file.txt",
        Remote:   &RemoteState{IsDeleted: true},
        Local:    &LocalState{Hash: "newHash", Mtime: 1000},
        Baseline: &BaselineEntry{LocalHash: "oldHash", SyncedAt: 500},
    }
    actions := classifyFile(view, SyncBidirectional)
    require.Len(t, actions, 1)
    assert.Equal(t, ActionConflict, actions[0].Type)
    assert.Equal(t, ConflictEditDelete, actions[0].ConflictInfo.Type)
}
```

Every cell in the EF1-EF14 and ED1-ED8 matrices can be tested this way. No mock stores, no mock query behavior, no risk of B-051 (mock filtering making paths unreachable).

### 26.2 Observer Testing

Observers produce `[]ChangeEvent` from I/O sources. Test with:
- **Remote Observer**: Mock `DeltaFetcher` interface returning `graph.DeltaPage` fixtures. Verify correct normalization, dedup, path materialization.
- **Local Observer**: Real filesystem in `t.TempDir()`. Create files, run `FullScan`, verify events.

### 26.3 Integration Testing

The full pipeline (`RunOnce`) is tested with a real SQLite database and mock Graph API client. Same as current E2E test approach but simpler because fewer components write to the DB.

### 26.4 Coverage Targets

| Component | Target | Rationale |
|---|---|---|
| Planner (pure functions) | ≥95% | Every matrix cell, every edge case |
| Observers | ≥90% | Normalization, filtering, edge cases |
| Executor | ≥85% | I/O-heavy, but error paths must be covered |
| BaselineManager | ≥90% | CRUD + atomic commit |
| Engine wiring | ≥80% | Integration-level coverage |
| Overall `internal/sync/` | ≥90% | Matches current target |

---

## Part 27: BACKLOG Item Resolution

Alternative E addresses or resolves these open BACKLOG items:

| BACKLOG ID | Issue | E's Resolution |
|---|---|---|
| **B-048** | `ErrorRetryable` treated as `ErrorSkip` in `dispatchPhase` | Fixed: retries happen inside executor with exponential backoff before producing Outcome (Part 16) |
| **B-046** | `ConflictRecord.RemoteHash` always holds QuickXorHash; needs DB migration | Clean break: E's conflict table uses explicit `local_hash`/`remote_hash` naming. No ambiguity. |
| **B-049** | Graph mock `GetItem` returns `(nil, nil)` but real client returns `(nil, ErrNotFound)` | Observers produce events — mock returns `graph.DeltaPage` fixtures, not individual items. Mock surface area reduced. |
| **B-030** | Review whether `internal/graph/` should be split | E does NOT affect `internal/graph/`. 100% reused as-is. |
| **B-036** | Extract CLI service layer for testability | E improves this: `Engine.RunOnce` and `Engine.RunWatch` are clean entry points. CLI layer becomes thinner. |
| **B-052** | E2E tests disabled in CI | E2E tests exercise CLI binary — ~95% reusable. Minor wiring changes in `sync.go`. |

### 27.1 Items Made Obsolete by E

The following concerns from LEARNINGS.md and BACKLOG are structurally eliminated:

- **Tombstone management** — no tombstones in E
- **`local:` ID lifecycle** — no temporary IDs in E
- **SQLITE_BUSY during execution** — no DB writes during execution
- **`isSynced()` for folders** — replaced by `baseline != nil`
- **Dry-run side effects** — zero DB writes before Execute
- **Scanner-delta row conflicts** — separate event streams, no shared mutable state
- **B-050 crash-recovery gap** — no upsert-then-delete pattern
