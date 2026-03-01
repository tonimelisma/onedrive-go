# Sync Algorithm Specification

> This document specifies the complete synchronization algorithm for
> onedrive-go. See [event-driven-rationale.md](event-driven-rationale.md) for
> the architectural rationale and [concurrent-execution.md](concurrent-execution.md)
> for the execution architecture.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Pipeline Architecture](#2-pipeline-architecture)
3. [Remote Observer](#3-remote-observer)
4. [Local Observer](#4-local-observer)
5. [Change Buffer](#5-change-buffer)
6. [Filtering](#6-filtering)
7. [Planner](#7-planner)
8. [Safety Checks](#8-safety-checks)
9. [Executor](#9-executor)
10. [Baseline Manager](#10-baseline-manager)
11. [Initial Sync](#11-initial-sync)
12. [Continuous Mode (`--watch`)](#12-continuous-mode---watch)
13. [Crash Recovery](#13-crash-recovery)
14. [Sync Report](#14-sync-report)
15. [Verify Command](#15-verify-command)

---

## 1. Overview

### 1.1 Purpose and Scope

This specification defines the complete synchronization algorithm for onedrive-go. It covers:

- How remote changes are observed and normalized (Remote Observer)
- How local changes are detected (Local Observer)
- How change events are collected, debounced, and batched (Change Buffer)
- How events are filtered symmetrically (Filter Engine)
- How local, remote, and baseline states are reconciled into an action plan (Planner)
- How safety invariants protect user data (Safety Checks)
- How planned actions are executed via parallel worker pools (Executor)
- How confirmed state is persisted atomically (Baseline Manager)
- How continuous mode operates with real-time change detection (`--watch`)
- How the system recovers from crashes and interruptions

The algorithm is designed to be **safe first, correct second, fast third**. Every destructive operation has a guard. Every optimization must not compromise safety or correctness.

### 1.2 Key Definitions

| Term | Definition |
|------|-----------|
| **Sync cycle** | One complete execution of the pipeline: observe remote + local, buffer, plan, execute, commit. One-shot mode runs exactly one cycle. Watch mode runs cycles repeatedly. |
| **Delta token** | An opaque cursor returned by the Microsoft Graph delta API. Represents a point in the remote change history. Passing a saved delta token to the API returns only changes since that point. |
| **deltaLink** | The URL in a delta API response that contains the delta token. Its presence signals that all pages of the current delta have been returned. |
| **Baseline** | The confirmed synced state of every file and folder. Loaded into memory at cycle start, used read-only by all pipeline stages, and updated atomically at cycle end. The baseline is the **only durable per-item state** in the system. |
| **Three-way merge** | The reconciliation technique that compares the current local state and current remote state against the baseline (the last known synced state) to determine what action, if any, is required for each path. |
| **Action plan** | The ordered set of actions (downloads, uploads, deletes, moves, conflict resolutions, baseline updates) produced by the planner. The executor consumes the plan. |
| **Worker pool** | A set of concurrent goroutines managed by `errgroup`. Separate pools handle downloads, uploads, and hash computation. |
| **Batch** | A group of `PathChanges` values flushed from the change buffer and processed together by the planner. In one-shot mode, the entire set of observations forms a single batch. In watch mode, each debounce window produces a batch. |
| **Stale file** | A local file that was synced while a filter rule included it, but a subsequent filter change now excludes it. The file remains on disk but is no longer synced. Detected on filter change and logged as a warning. |
| **False conflict** | A situation where both sides independently arrive at the same content (same hash). The planner detects this and classifies it as a convergent edit (EF4) or convergent create (EF11), requiring only a baseline update. |
| **Big delete** | A safety trigger: when the number of planned delete actions exceeds a configurable threshold (count > 1000 OR percentage > 50% of baseline entries, with a minimum items guard of 10), the sync cycle halts and requires user confirmation. |
| **ChangeEvent** | An immutable observation of a remote or local change. Produced by observers, consumed by the buffer and planner. Ephemeral -- never persisted to the database. |
| **PathView** | A three-way view of a single path: current remote state, current local state, and baseline. Constructed by the planner from change events + baseline. Input to the decision matrix. |
| **Outcome** | The result of executing a single action. Self-contained: carries everything the baseline manager needs to update the database. |

### 1.3 Safety Philosophy

User data is irreplaceable. The sync algorithm treats data loss as the worst possible failure mode. Every design decision is made with this hierarchy:

1. **Never lose user data** -- downloads use atomic writes, deletes use hash verification, conflicts create copies
2. **Be correct** -- three-way merge with per-side hash baselines handles every combination of local and remote changes
3. **Be fast** -- parallel transfers, delta-driven incremental sync, event-driven watch mode

When safety and performance conflict, safety wins. When correctness and performance conflict, correctness wins.

### 1.4 Safety Invariants (S1-S7)

Seven invariants protect user data at every stage of the pipeline:

| ID | Invariant | Description |
|----|-----------|-------------|
| **S1** | Never delete remote on local absence without synced base | A file absent locally is a deletion signal **only** if it has a baseline entry (meaning it was previously synced and the user deleted it). A file that never had a baseline entry is simply unknown -- not a candidate for remote deletion. The planner checks `view.Baseline != nil` before emitting `ActionRemoteDelete`. |
| **S2** | Never process deletions from incomplete enumeration | The delta token is advanced only after a complete delta response (all pages through the `deltaLink`). If the delta fetch is interrupted, the token stays at the old position, and the next cycle re-fetches from the same point. The `.nosync` guard file in the sync root prevents syncing unmounted volumes -- the local observer checks for it before producing any events. |
| **S3** | Atomic file writes for downloads | Every download writes to a `.partial` file first, computes the QuickXorHash via streaming `io.TeeReader`, verifies the hash against the expected value, sets file timestamps, and then atomically renames the `.partial` file to the target path. If the process crashes mid-download, only the `.partial` file exists -- never a corrupt target file. |
| **S4** | Hash-before-delete guard | Before deleting a local file, the executor computes the current local hash and compares it against `Baseline.LocalHash`. If the hashes match, the file is unchanged since the last sync and is safe to delete. If the hashes differ, the file was modified after the last sync -- the executor creates a conflict copy and preserves the user's changes. |
| **S5** | Big-delete protection | The planner counts `ActionLocalDelete` + `ActionRemoteDelete` in the plan and compares against `len(baseline.ByPath)`. If the count exceeds the configurable threshold (count > 1000 OR percentage > 50% of baseline entries, with a minimum guard of 10 items), the sync cycle halts and reports the situation to the user. This guards against accidental mass deletion from misconfigured filters, cloud-side bulk operations, or API bugs. |
| **S6** | Disk space check before downloads | Before each download, the executor checks available disk space against `config.MinFreeSpace` (default 1 GB). If insufficient space is available, the download produces a failed Outcome with a warning. This prevents filling the disk and destabilizing the operating system. |
| **S7** | Never upload partial or temp files | The filter cascade excludes temporary file patterns (`.partial`, `.tmp`, `.swp`, `~*`, `.~*`, `.crdownload`) from both local and remote item processing. This prevents uploading editor swap files, incomplete downloads, and other transient files that would pollute the remote drive. |

### 1.5 Sync Modes

The sync engine supports multiple modes, all sharing the same pipeline components:

| Mode | Description | Pipeline Behavior |
|------|-------------|-------------------|
| **Bidirectional** (default) | Full two-way sync. Local and remote changes are reconciled. Conflicts are detected and resolved. | Both observers run. All EF/ED matrix rules apply. |
| **Download-only** | Only remote changes are applied locally. Local changes are ignored. | Remote observer runs. Local observer runs (to detect convergent edits). Planner suppresses all upload and remote-delete actions. |
| **Upload-only** | Only local changes are pushed to remote. Remote changes are ignored. | Local observer runs. Remote observer is skipped. Planner suppresses all download and local-delete actions. |
| **Dry-run** | Preview mode. Steps 1-5 execute normally (load baseline, observe, buffer, plan). The action plan is printed. No executor runs. Zero side effects. | Observers and planner run. Executor and baseline manager are skipped. |
| **One-shot** (default) | Runs exactly one sync cycle and exits. | `Engine.RunOnce()` |
| **Continuous** (`--watch`) | Runs sync cycles repeatedly, processing changes as they arrive in real time. | `Engine.RunWatch()` with debounced event loop. |

### 1.6 Type System Design

Each pipeline stage works with its own types. The type system makes it impossible to confuse remote, local, and baseline data at compile time.

| Type | Produced By | Consumed By | Lifespan |
|------|-------------|-------------|----------|
| `ChangeEvent` | Remote Observer, Local Observer | Change Buffer, Planner | Ephemeral (one cycle) |
| `PathChanges` | Change Buffer | Planner | Ephemeral (one cycle) |
| `PathView` | Planner (internally) | Planner decision matrix, Action | Ephemeral (one cycle) |
| `RemoteState` | Planner (from remote events) | Decision matrix | Ephemeral (one cycle) |
| `LocalState` | Planner (from local events) | Decision matrix | Ephemeral (one cycle) |
| `BaselineEntry` | BaselineManager.Load | All components (read-only) | Persisted in SQLite |
| `Baseline` | BaselineManager.Load | Observers, Planner | One cycle (frozen snapshot) |
| `Action` | Planner | Executor | Ephemeral (one cycle) |
| `ActionPlan` | Planner | Executor | Ephemeral (one cycle) |
| `Outcome` | Executor | BaselineManager.CommitOutcome | Ephemeral (one cycle) |

**ChangeEvent** carries all information about an observed change:

```go
type ChangeEvent struct {
    Source    ChangeSource   // SourceRemote or SourceLocal
    Type     ChangeType     // ChangeCreate, ChangeModify, ChangeDelete, ChangeMove
    Path     string         // NFC-normalized, relative to sync root
    OldPath  string         // for moves only
    ItemID   string         // server-assigned (remote only; empty for local)
    ParentID string         // server parent ID (remote only)
    DriveID  driveid.ID     // normalized (lowercase, zero-padded to 16 chars)
    ItemType ItemType       // file, folder, root
    Name     string         // URL-decoded, NFC-normalized
    Size     int64
    Hash     string         // QuickXorHash (base64); empty for folders
    Mtime    int64          // Unix nanoseconds
    ETag     string         // remote only
    CTag     string         // remote only
    IsDeleted bool          // true for remote deletions
}
```

**Action** carries the full `PathView` context so the executor has complete information:

```go
type Action struct {
    Type         ActionType
    DriveID      driveid.ID
    ItemID       string
    Path         string
    OldPath      string           // source path (moves only)
    CreateSide   FolderCreateSide // for folder creates
    View         *PathView        // full three-way context
    ConflictInfo *ConflictRecord  // for conflict actions
}
```

**ActionPlan** contains a flat list of actions with explicit dependency edges for DAG-based concurrent execution:

```go
type ActionPlan struct {
    Actions []Action  // flat list of all actions
    Deps    [][]int   // Deps[i] = indices that action i depends on
    CycleID string    // UUID grouping actions from one planning pass
}
```

The `Deps` adjacency list encodes ordering constraints (parent-before-child, children-before-parent-delete, move-target-parent). The `DepTracker` uses `Deps` to dispatch ready actions to workers as their dependencies are satisfied.

**Consumer-defined interfaces** for the Graph API client (defined in `sync/`, satisfied by `*graph.Client`):

```go
type DeltaFetcher interface {
    Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
}

type ItemClient interface {
    GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
    ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
    CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
    MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
    DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
    PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}

// DriveVerifier verifies that a configured drive ID is reachable.
type DriveVerifier interface {
    Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
}

// Downloader streams a remote file by item ID.
type Downloader interface {
    Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

// Uploader uploads a local file, encapsulating the simple-vs-chunked decision
// and upload session lifecycle. content must be an io.ReaderAt for retry safety.
type Uploader interface {
    Upload(
        ctx context.Context, driveID driveid.ID, parentID, name string,
        content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
    ) (*graph.Item, error)
}

// SessionUploader provides session-based upload methods for resumable transfers.
// Type-asserted at runtime to avoid breaking the Uploader interface.
type SessionUploader interface {
    CreateUploadSession(ctx context.Context, driveID driveid.ID, parentID, name string,
        size int64, mtime time.Time) (*graph.UploadSession, error)
    UploadFromSession(ctx context.Context, session *graph.UploadSession,
        content io.ReaderAt, totalSize int64, progress graph.ProgressFunc) (*graph.Item, error)
    ResumeUpload(ctx context.Context, session *graph.UploadSession,
        content io.ReaderAt, totalSize int64, progress graph.ProgressFunc) (*graph.Item, error)
}

// RangeDownloader downloads a file starting from a byte offset.
// Type-asserted at runtime to avoid breaking the Downloader interface.
type RangeDownloader interface {
    DownloadRange(ctx context.Context, driveID driveid.ID, itemID string,
        w io.Writer, offset int64) (int64, error)
}
```

These interfaces follow the Go convention of consumer-defined contracts: the `sync/` package defines what it needs; the `graph/` package satisfies those needs without knowing about them.

---

## 2. Pipeline Architecture

### 2.1 Event-Driven Flow Diagram

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
            │  * Filter      │    --> produces ActionPlan
            │  * Merge       │
            │  * Reconcile   │    <-- pure functions, no I/O
            │  * Safety      │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │   Executor     │    <-- executes actions against API + filesystem
            │                │    --> produces Outcomes
            │  * Downloads   │
            │  * Uploads     │    <-- lane-based workers (parallel)
            │  * Deletes     │
            │  * Moves       │    <-- dependency-ordered (DAG)
            │  * Conflicts   │
            └────────┬───────┘
                     ▼
            ┌────────────────┐
            │   Baseline     │    <-- commits each Outcome per-action
            │   Manager      │    <-- saves delta token when cycle complete
            └────────────────┘
```

Each component has a single responsibility and a clean contract:

- **Observers** produce `[]ChangeEvent` -- they never write to the database.
- **Change Buffer** groups events by path into `[]PathChanges` -- thread-safe, debounced.
- **Planner** is a pure function: `([]PathChanges, *Baseline, SyncMode, SafetyConfig) -> *ActionPlan` -- no I/O.
- **Workers** execute actions and produce `Outcome` per action. Each outcome is committed to the baseline immediately via `BaselineManager.CommitOutcome()`.
- **Baseline Manager** is the sole database writer -- it commits each outcome individually via per-action atomic transactions.

### 2.2 Component Interaction Summary

**One-Shot Mode** (single sync cycle):

```
1. BaselineManager.Load()           -> Baseline (from DB, cached in memory)
2. RemoteObserver.FullDelta()       -> []ChangeEvent (remote)
3. LocalObserver.FullScan()         -> []ChangeEvent (local)
   (steps 2-3 run concurrently)
4. ChangeBuffer.AddAll() + Flush()  -> []PathChanges (batched by path)
5. Planner.Plan()                   -> ActionPlan with dependency DAG
6. Add actions to in-memory DepTracker
7. Workers execute concurrently     -> per-action baseline commits
8. All actions complete             -> commit delta token
```

**Watch Mode** (continuous sync):

```
1. BaselineManager.Load()           -> Baseline (from DB, cached in memory)
2. RemoteObserver.Watch()           -> streams ChangeEvents (poll / WebSocket)
3. LocalObserver.Watch()            -> streams ChangeEvents (inotify / FSEvents)
4. ChangeBuffer debounces (2s)      -> []PathChanges (only changed paths)
5. Planner.Plan()                   -> ActionPlan (only for changed paths)
6. Populate DepTracker with actions and dependency edges
7. Workers execute concurrently     -> per-action baseline commits
8. All actions complete             -> commit delta token
9. Go to step 4 (loop on buffer ready)
```

**Dry-Run Mode** (zero side effects):

```
Steps 1-5 execute normally. The action plan is printed. No executor runs.
Zero side effects.
```

**Pause/Resume** (watch mode only):

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

---

## 3. Remote Observer

The Remote Observer produces `[]ChangeEvent` from the Microsoft Graph delta API. It has two operating modes (FullDelta for one-shot, Watch for continuous) and handles all API quirk normalization at the boundary, making quirks invisible to downstream components.

### 3.1 FullDelta Mode

Used by one-shot sync. Fetches all remote changes since the last saved delta token.

```go
func (o *RemoteObserver) FullDelta(ctx context.Context, savedToken string) ([]ChangeEvent, string, error)
```

**Algorithm**:

1. Start with the saved delta token (empty string on first run = full enumeration).
2. Call the delta API page by page.
3. For each page:
   a. Buffer all items in the page.
   b. Process deletions before creations within the page (handles known API reordering bug where a delete and create at the same path arrive out of order within a single page).
   c. Convert each `graph.Item` to a `ChangeEvent` via `convertToChangeEvent`.
   d. Deduplicate: keep only the last occurrence of each `(driveID, itemID)` pair per page.
4. If the page contains a `deltaLink`, extract the new delta token and stop pagination.
5. If the page contains a `nextLink`, use it to fetch the next page.
6. Return `(events, newDeltaToken, nil)`.

Cross-page reordering is NOT performed because it would break the parent-before-child guarantee for folder creations.

**HTTP 410 handling**: When the delta API returns HTTP 410 (expired delta token):

1. The observer discards the expired token.
2. It returns a sentinel error indicating full re-enumeration is needed.
3. The engine catches this error and restarts with an empty token (triggering full enumeration).
4. The two resync types (token expiry vs. service-initiated resync) are distinguished based on the response body.

### 3.2 Watch Mode (Polling / Future WebSocket)

Used by continuous sync. Polls the delta API at a configurable interval (default 5 minutes) and emits change events to the buffer via a callback.

```go
func (o *RemoteObserver) Watch(ctx context.Context, savedToken string, emit func(ChangeEvent)) error
```

**Algorithm**:

1. Start with the saved delta token.
2. On each poll tick:
   a. Call `FullDelta` with the current token.
   b. Emit each returned event via the callback.
   c. Update the local token for the next poll.
3. If the delta call fails, log a warning and retry on the next tick.
4. Run until the context is canceled.

WebSocket-based real-time push notifications are a future optimization. The polling architecture is designed so that switching to WebSocket requires changing only the trigger mechanism, not the event processing pipeline.

### 3.3 Normalization and Quirk Handling

All API quirks are handled in `convertToChangeEvent`, making them invisible to downstream components.

| Quirk | Handling |
|-------|----------|
| **DriveID casing inconsistency** | `strings.ToLower()` + zero-pad to 16 characters. Example: `24470056F5C3E43` becomes `024470056f5c3e43`. Applied to every driveID at the API boundary. |
| **URL-encoded names** | `url.PathUnescape(item.Name)` on every item. Delta API returns URL-encoded names for items with special characters. |
| **Missing name on deleted items** | Business/SharePoint deleted items often lack the `name` field. The observer looks up the missing name from `baseline.ByID[event.ItemID]`. |
| **Missing hash on deleted items** | Some deleted items carry stale hashes. The observer clears hash fields on deleted items to prevent false matches. |
| **`Prefer` header for Personal delta** | Personal accounts require `Prefer: deltashowremoteitemsaliasid` header to include shared folder items in delta responses. Included on all Personal delta requests. |
| **iOS `.heic` hash mismatch** | Known API bug where server-reported hash does not match actual file content for `.heic` files uploaded from iOS. Logged as a warning, marked as known-unreliable. |
| **Timestamp validation** | Rejects `0001-01-01T00:00:00Z`, dates before 1970, and dates more than one year in the future. Falls back to current UTC time. |
| **NFC normalization** | `norm.NFC.String(item.Name)` for macOS APFS compatibility. All paths stored in NFC form. |
| **`parentReference.path` absence** | Delta responses never include `parentReference.path`. The observer reconstructs the full relative path from the parent chain using `baseline.ByID` + an in-flight parent map (for parents that arrived earlier in the same delta). |
| **Duplicate items within a page** | Keep only the last occurrence of each `(driveID, itemID)` pair. The API occasionally sends duplicates. |
| **Deletion reordering within a page** | Buffer the full page, then process deletions before creations. Handles known bug where delete and create for the same path arrive in the wrong order. |

### 3.4 ChangeEvent Production

The `convertToChangeEvent` function transforms a `graph.Item` into a `ChangeEvent`:

1. **Populate common fields**: `Source = SourceRemote`, `ItemID`, `ParentID`, `Name`, `ItemType`, `Size`, `Hash`, `ETag`, `CTag`, `Mtime`.
2. **Classify the change type**:
   - If `item.Deleted`: `Type = ChangeDelete`, `IsDeleted = true`. Look up missing fields from baseline.
   - If `baseline.ByID[item.ID]` exists and the materialized path differs from the baseline path: `Type = ChangeMove`, `OldPath = baseline.Path`, `Path = newPath`.
   - If `baseline.ByID[item.ID]` exists and paths match: `Type = ChangeModify`.
   - If item ID is not in the baseline: `Type = ChangeCreate`.
3. **Materialize the path**: Walk the parent chain using `baseline.ByID` and an in-flight parent map to reconstruct the full relative path from the sync root.

The output is a fully normalized, self-contained `ChangeEvent` that carries all the information downstream components need.

### 3.5 Shortcut Detection and Sub-Delta Tracking

The Remote Observer detects shortcuts (items with a `remoteItem` facet and `folder` type) in the primary delta response. Each shortcut represents a reference to a folder on another drive.

**Shortcut handling in FullDelta:**

1. Primary delta returns items for the user's own drive, including shortcut items
2. For each shortcut item (has `remoteItem` facet with `folder` type):
   a. Extract `remoteItem.driveId` and `remoteItem.id`
   b. Run a folder-scoped delta: `GET /drives/{remoteItem.driveId}/items/{remoteItem.id}/delta`
   c. Use a separate cached delta token keyed by `(drive_id, scope_id)` where `scope_id = remoteItem.id`
   d. Map source-drive paths to local paths by prefixing with the shortcut's position in the user's tree
   e. Emit ChangeEvents with source drive coordinates (`DriveID = remoteItem.driveId`) but local-tree-relative paths
3. Handle shortcut lifecycle:
   - New shortcut appears → trigger initial enumeration of the shared folder
   - Shortcut removed → emit delete events for all items under the shortcut
   - Shortcut moved → update local path prefix for all items

**Per-shortcut delta tokens** are stored in the `delta_tokens` table with composite primary key `(drive_id, scope_id)`. The primary delta uses `scope_id = ""`.

### 3.6 Personal Vault Exclusion

The Remote Observer excludes Personal Vault items from observation. Vault items are detected via `specialFolder.name == "vault"` in the delta response. When a vault item or any item under the vault folder is encountered:

1. Skip the item — do not emit a ChangeEvent
2. Log at INFO: "skipping Personal Vault item" with the item path
3. Never include vault items in baseline, planning, or execution

This exclusion is mandatory because the vault auto-locks after 20 minutes, causing items to appear and disappear from delta responses. Without exclusion, the planner would interpret locked-vault items as remote deletions and delete the user's most sensitive files locally.

The `sync_vault` config option (default `false`) can override this behavior, but a warning is logged when enabled.

---

## 4. Local Observer

The Local Observer produces `[]ChangeEvent` from the local filesystem. It has two operating modes (FullScan for one-shot, Watch for continuous) and handles all filesystem normalization at the boundary.

### 4.1 FullScan Mode

Used by one-shot sync. Walks the entire sync directory and compares the filesystem state against the baseline.

```go
func (o *LocalObserver) FullScan(ctx context.Context) ([]ChangeEvent, error)
```

**Algorithm**:

1. **`.nosync` guard check** (S2): If a `.nosync` file exists in the sync root, return `ErrNosyncGuard` immediately. This prevents syncing when the underlying volume is unmounted (e.g., removable drive not present).
2. **Walk the sync directory** using `filepath.WalkDir`:
   - For each entry, compute dual paths: `fsRelPath` (raw filesystem path for I/O) and `dbRelPath` (NFC-normalized for baseline lookup).
   - Skip symlinks (never followed).
   - Validate filenames against OneDrive name rules.
   - Apply the filter cascade using `dbRelPath`.
   - Record each observed path in an `observed` map.
3. **Classify each observed entry**:
   - If `baseline.ByPath[dbRelPath]` is nil: `Type = ChangeCreate` (new local file/folder).
   - If baseline exists and content hash differs from `Baseline.LocalHash`: `Type = ChangeModify`.
   - If baseline exists and content hash matches: no event emitted (unchanged).
4. **Detect local deletions**: Iterate `baseline.ByPath`. Any baseline path not in the `observed` map produces a `ChangeDelete` event.
5. Return all collected events.

### 4.2 Watch Mode (fsnotify)

Used by continuous sync. Monitors the sync directory for real-time filesystem events using `fsnotify/fsnotify` (cross-platform: inotify on Linux, FSEvents on macOS).

```go
func (o *LocalObserver) Watch(ctx context.Context, emit func(ChangeEvent)) error
```

**Algorithm**:

1. Set up a recursive watch on the sync root.
2. For each filesystem event:
   a. Compute the relative path and apply filter cascade.
   b. Stat the file to determine if it still exists.
   c. Classify:
      - `os.IsNotExist`: `Type = ChangeDelete`.
      - Not in baseline: `Type = ChangeCreate`. Compute hash for regular files.
      - In baseline: `Type = ChangeModify`. Compute hash for regular files.
   d. Emit the event via the callback.
3. Run until the context is canceled.

Watch mode events flow through the same change buffer and planner as FullScan events. The only difference is granularity: watch mode emits individual file events, while FullScan emits a complete snapshot.

### 4.3 mtime Comparison and Racily-Clean Detection

For files that exist in the baseline, the local observer uses a two-tier change detection strategy:

**Fast path (mtime differs)**: If the file's mtime, truncated to whole seconds, differs from the baseline's mtime (also truncated to whole seconds), the file has changed. The observer computes the content hash to confirm the change (mtime alone is not authoritative -- timestamps can drift without content changes).

**Racily-clean detection**: If `truncateToSeconds(localMtime) == truncateToSeconds(baseMtime)`, the file MAY have been modified within the same second that the baseline was committed. The mtime fast-check is ambiguous in this window. The observer ALWAYS computes the content hash in this case to verify whether the file actually changed. This is the same-second ambiguity problem documented in git's index design.

In both cases, a `ChangeModify` event is emitted only if the computed hash differs from `Baseline.LocalHash`. If the hash matches despite an mtime change, no event is produced (timestamp drift, not a content change).

### 4.4 NFC Normalization

macOS APFS stores filenames in NFD (decomposed Unicode). Most other systems and OneDrive use NFC (composed Unicode). The local observer maintains dual paths throughout:

- **`fsRelPath`**: The raw filesystem path, used for all I/O operations (reading, hashing, stat).
- **`dbRelPath`**: `norm.NFC.String(fsRelPath)`, used for all baseline lookups and event emission.

This ensures that baseline lookups succeed regardless of the host filesystem's normalization form, and that all events carry NFC-normalized paths for consistency across platforms.

### 4.5 ChangeEvent Production

Each local event carries:

| Field | Value |
|-------|-------|
| `Source` | `SourceLocal` |
| `Type` | `ChangeCreate`, `ChangeModify`, or `ChangeDelete` |
| `Path` | NFC-normalized relative path from sync root |
| `Name` | NFC-normalized filename component |
| `ItemType` | `file` or `folder` |
| `Size` | File size in bytes |
| `Hash` | QuickXorHash (base64) for regular files; empty for folders |
| `Mtime` | File modification time as Unix nanoseconds |
| `ItemID` | Empty string (local events have no server-assigned ID) |

Local events never carry `ItemID`, `ParentID`, `DriveID`, `ETag`, or `CTag` -- these are remote-only fields. The baseline maps local paths to server item IDs when needed.

**Additional concerns handled by the local observer**:

| Concern | Handling |
|---------|----------|
| **Symlinks** | Detected via `d.Type()&fs.ModeSymlink`. Never followed. Warning logged unless suppressed. |
| **Name validation** | Rejects OneDrive-invalid names: `.lock`, `desktop.ini`, reserved names (`CON`, `PRN`, `AUX`, `NUL`, `COM0-9`, `LPT0-9`), names starting with `~$`, names containing `_vti_`, trailing dots, newlines. |
| **Path length validation** | Personal: 400 character limit. Business: 400 URL-encoded byte limit. |
| **Temp file exclusion** | Handled by filter cascade: `.partial`, `.tmp`, `.swp`, `~*`, `.~*`, `.crdownload` excluded (S7). |
| **DirEntry.Info() errors** | Logged as warning, entry skipped. Handles permission errors and race conditions (file disappears between readdir and stat). |
| **OneNote files** | Detected via extension (`.one`, `.onetoc2`) or `package` facet. Filtered out -- OneDrive manages these separately. |

---

## 5. Change Buffer

The Change Buffer collects events from both observers, deduplicates them, applies debounce timing, and produces batches grouped by path.

### 5.1 Debounce and Dedup

**Debounce**: In watch mode, rapid filesystem events (e.g., IDE auto-save writing the same file multiple times per second) would trigger redundant sync cycles. The buffer maintains a configurable debounce window (default 2 seconds). After the last event arrives, the buffer waits for the debounce window to elapse before signaling readiness.

**Deduplication**: Events are indexed by path. Multiple events for the same path within a debounce window accumulate in the same `PathChanges` group. The planner sees all events for a path and uses the latest event to derive the current state.

**One-shot mode**: Uses `FlushImmediate()` which returns all pending events without waiting for the debounce timer. All observations from the full delta and full scan are processed as a single batch.

**Flush behavior**: `Flush()` returns all pending `PathChanges` and clears the buffer. Events are split by source: each `PathChanges` contains separate `RemoteEvents` and `LocalEvents` slices for the same path.

### 5.2 Move Dual-Keying

Move events require special handling because they affect two paths: the source (where the item was) and the destination (where the item is now). The buffer ensures both paths enter the planner:

1. When a `ChangeMove` event arrives (from the Remote Observer), it is stored under the **new path** (destination).
2. A synthetic `ChangeDelete` event is created at the **old path** (source).
3. This ensures the planner sees both paths: the move event at the destination and the delete signal at the source.

Without dual-keying, the source path would never enter the planner, and its baseline entry would become orphaned.

### 5.3 PathChanges Batching

The buffer produces `[]PathChanges`, where each element groups all events for a single path:

```go
type PathChanges struct {
    Path         string
    RemoteEvents []ChangeEvent   // all remote observations for this path
    LocalEvents  []ChangeEvent   // all local observations for this path
}
```

This grouping is the key input to the planner. For each path, the planner has the complete picture: what happened remotely, what happened locally, and what the baseline says.

**Thread safety**: The buffer is protected by a mutex. Observer goroutines (remote and local) can call `Add()` concurrently. The engine calls `Flush()` from the main goroutine after the debounce timer fires.

---

## 6. Filtering

> **Per-drive only.** All filter settings (`skip_dirs`, `skip_files`, `skip_dotfiles`, `max_file_size`, `sync_paths`, `ignore_marker`) are per-drive native — there are no global filter defaults. Each drive gets built-in defaults (empty lists, `false`) unless it specifies its own values. See [MULTIDRIVE.md §10](MULTIDRIVE.md#10-filter-scoping) and DP-8 for rationale.

### 6.1 Symmetric Three-Layer Cascade

The filter engine applies three layers of exclusion rules, evaluated in order. Each layer can only exclude -- a later layer cannot re-include an item excluded by an earlier layer (monotonic exclusion).

| Layer | Source | Scope | Examples |
|-------|--------|-------|----------|
| **1. sync_paths** | Config: `sync_paths` | Restricts sync to specific subtrees within the drive | `sync_paths = ["/Documents", "/Projects"]` |
| **2. Config patterns** | Config: `skip_dotfiles`, `skip_dirs`, `skip_files`, `max_file_size` | Per-drive exclusion rules (no global defaults, DP-8) | `skip_dotfiles = true`, `skip_dirs = ["node_modules", ".git"]`, `max_file_size = "100MB"` |
| **3. .odignore** | `.odignore` file in sync root (gitignore syntax) | User-defined per-directory rules | `*.log`, `build/`, `*.tmp` |

**Symmetric application**: The filter runs in the planner, not in the observers. This ensures that BOTH remote-only items (new downloads) AND local-only items (new uploads) are filtered through the same cascade. A file excluded by the filter is excluded regardless of which side it appears on.

**Built-in exclusions** (always active, cannot be overridden):

- `.partial` files (incomplete downloads, S3/S7)
- `.tmp`, `.swp` files (editor temporaries, S7)
- `~*`, `.~*` files (editor backup/lock files, S7)
- `.crdownload` files (browser downloads, S7)
- `.nosync` guard file
- Sync database files

### 6.2 PathView Context for Filter Decisions

The filter receives the full path context for each decision:

```go
type FilterResult struct {
    Included bool
    Reason   string  // human-readable explanation of why excluded
}

func (f *Filter) ShouldSync(path string, isDir bool, size int64) FilterResult
```

The planner calls `ShouldSync` during the classification step for items that appear on only one side (new remote or new local items). Items that already have a baseline entry are always processed -- they were included when first synced, and removing them requires the stale file detection mechanism (see below).

### 6.3 Stale File Detection on Filter Changes

When filter rules change (e.g., `skip_dotfiles` is enabled after `.dotfiles` were already synced), previously synced files may become excluded. These files are **not automatically deleted**. Instead:

1. The engine compares the current filter configuration against the previous in-memory config.
2. Files that were included under the old filter but are excluded under the new filter are detected by walking the baseline.
3. The user is warned about stale files via log messages. Stale files remain on disk but are no longer synced.

This approach prevents accidental data loss from filter changes. The user retains control over whether excluded-but-present files should be removed.

In watch mode, SIGHUP triggers configuration reload, including filter re-evaluation and stale file detection.

---

## 7. Planner

The planner is the intellectual core of the sync engine. It is composed entirely of pure functions -- no I/O, no database access, no side effects. Every decision is deterministic and reproducible from the same inputs.

### 7.1 Pure Function: (events + baseline) -> ActionPlan

```go
func (p *Planner) Plan(
    changes []PathChanges,
    baseline *Baseline,
    mode SyncMode,
    config *SafetyConfig,
) (*ActionPlan, error)
```

The planner executes the following pipeline:

1. **Build PathViews** from change events + baseline.
2. **Detect moves** (remote moves from ChangeMove events; local moves via hash-based correlation).
3. **Classify each PathView** using the decision matrix (EF1-EF14 for files, ED1-ED8 for folders).
4. **Apply filters** symmetrically to both remote-only and local-only items.
5. **Order the plan** (folder creates before files, depth-first for deletes).
6. **Safety checks** (big-delete, etc.) as pure functions on `ActionPlan + Baseline`.

### 7.2 PathView Construction

The planner constructs a `PathView` for each path that has at least one change event or baseline entry:

```go
type PathView struct {
    Path     string
    Remote   *RemoteState    // nil = no remote change observed
    Local    *LocalState     // nil = locally deleted or no local observation
    Baseline *BaselineEntry  // nil = never synced
}
```

**Deriving RemoteState**: If a path has remote events, the latest event's fields populate the `RemoteState`. If the latest event is a deletion, `RemoteState.IsDeleted = true`.

**Deriving LocalState**: If a path has local events:
- Latest event is a deletion: `Local = nil` (absent).
- Latest event is create or modify: `Local` is populated from the event's fields.

If a path has no local events but has a baseline entry, the local state is derived from the baseline (the item is unchanged locally).

**Nil semantics**:
- `Remote == nil`: No remote information about this path in the current delta.
- `Local == nil`: The item does not exist locally (either deleted or never present).
- `Baseline == nil`: The item has never been synced.

### 7.3 File Decision Matrix (EF1-EF14)

The file decision matrix classifies every file path into exactly one action based on the three-way view.

**Change detection** uses per-side baselines:
- `detectLocalChange`: compares `Local.Hash` against `Baseline.LocalHash`.
- `detectRemoteChange`: compares `Remote.Hash` against `Baseline.RemoteHash`.

This per-side approach handles SharePoint enrichment natively (see Section 7.6).

| E# | Local State | Remote State | Baseline | Action | Description |
|----|-------------|--------------|----------|--------|-------------|
| **EF1** | unchanged | unchanged | exists | no-op | Both sides match baseline. In sync. |
| **EF2** | unchanged | changed | exists | download | Remote was modified. Download the new version. |
| **EF3** | changed | unchanged | exists | upload | Local was modified. Upload the new version. |
| **EF4** | changed | changed (same hash) | exists | update synced | Convergent edit: both sides independently arrived at identical content. No data transfer needed. Update baseline. |
| **EF5** | changed | changed (diff hash) | exists | **conflict** (edit-edit) | Divergent edit: both sides modified the file differently. Conflict detected. |
| **EF6** | deleted | unchanged | exists | remote delete | User deleted the file locally. Propagate deletion to remote. |
| **EF7** | deleted | changed | exists | download (remote wins) | Local deleted but remote was modified. Remote version takes priority. Re-download. |
| **EF8** | unchanged | deleted | exists | local delete | Remote deleted the file. Propagate deletion to local (with S4 hash guard). |
| **EF9** | changed | deleted | exists | **conflict** (edit-delete) | Local was modified but remote deleted it. Conflict detected. |
| **EF10** | deleted | deleted | exists | cleanup | Both sides independently deleted the file. Remove baseline entry. |
| **EF11** | new | new (same hash) | none | update synced | Convergent create: same file appeared on both sides with identical content. No data transfer needed. Create baseline entry. |
| **EF12** | new | new (diff hash) | none | **conflict** (create-create) | Divergent create: different files appeared at the same path on both sides. Conflict detected. |
| **EF13** | new | absent | none | upload | New local file. Upload to remote. |
| **EF14** | absent | new | none | download | New remote file. Download to local. |

**Mode filtering**: In download-only mode, local changes are suppressed (`localChanged = false`). In upload-only mode, remote changes are suppressed (`remoteChanged = false`).

**Classification logic** (pseudo-code for the file decision matrix):

```go
func classifyFile(v PathView, mode SyncMode) []Action {
    localChanged := detectLocalChange(v)
    remoteChanged := detectRemoteChange(v)

    if mode == SyncDownloadOnly { localChanged = false }
    if mode == SyncUploadOnly { remoteChanged = false }

    hasRemote := v.Remote != nil && !v.Remote.IsDeleted
    hasLocal := v.Local != nil
    hasBaseline := v.Baseline != nil
    remoteDeleted := v.Remote != nil && v.Remote.IsDeleted
    localDeleted := hasBaseline && !hasLocal

    // ORDERING CONSTRAINT: localDeleted implies localChanged (detectLocalChange
    // returns true when Local is nil). localDeleted cases (EF6, EF7, EF10) must
    // be checked before the general localChanged cases (EF3, EF4, EF5, EF9) to
    // avoid EF3 stealing EF6's matches. The implementation splits these into
    // separate sub-functions for the same reason.
    switch {
    // --- Baseline exists (previously synced) ---
    case hasBaseline && !localChanged && !remoteChanged:
        return nil // EF1: both sides unchanged

    case hasBaseline && !localChanged && remoteChanged && hasRemote:
        return []Action{{Type: ActionDownload}} // EF2

    // Local deleted cases (must precede general localChanged cases).
    case hasBaseline && localDeleted && !remoteChanged && !remoteDeleted:
        return []Action{{Type: ActionRemoteDelete}} // EF6

    case hasBaseline && localDeleted && remoteChanged && hasRemote:
        return []Action{{Type: ActionDownload}} // EF7: remote wins

    case hasBaseline && localDeleted && remoteDeleted:
        return []Action{{Type: ActionCleanup}} // EF10

    // Local modified (not deleted) cases.
    case hasBaseline && localChanged && !remoteChanged:
        return []Action{{Type: ActionUpload}} // EF3

    case hasBaseline && localChanged && remoteChanged && hasRemote:
        if v.Local.Hash == v.Remote.Hash {
            return []Action{{Type: ActionUpdateSynced}} // EF4: convergent
        }
        return []Action{{Type: ActionConflict}} // EF5: divergent

    case hasBaseline && !localChanged && remoteDeleted:
        return []Action{{Type: ActionLocalDelete}} // EF8

    case hasBaseline && localChanged && remoteDeleted:
        return []Action{{Type: ActionConflict}} // EF9: edit-delete

    // --- No baseline (never synced) ---
    case !hasBaseline && hasLocal && hasRemote:
        if v.Local.Hash == v.Remote.Hash {
            return []Action{{Type: ActionUpdateSynced}} // EF11: convergent
        }
        return []Action{{Type: ActionConflict}} // EF12: divergent

    case !hasBaseline && hasLocal && !hasRemote && !remoteDeleted:
        return []Action{{Type: ActionUpload}} // EF13

    case !hasBaseline && !hasLocal && hasRemote:
        return []Action{{Type: ActionDownload}} // EF14
    }

    return nil
}
```

Each action carries a pointer to the full `PathView`, giving the executor complete three-way context without querying any external state.

**Conflict types**:

| Matrix Cell | Conflict Type | Description |
|-------------|---------------|-------------|
| EF5 | `ConflictEditEdit` | Both sides modified the file with different content. |
| EF9 | `ConflictEditDelete` | Local was modified, remote deleted. |
| EF12 | `ConflictCreateCreate` | Different files appeared at the same path on both sides. |

**Conflict resolution** (default: keep-both):
1. The remote version is downloaded to the target path.
2. The local version is renamed to `<name>.conflict-YYYYMMDD-HHMMSS.<ext>` (timestamp-based naming, self-documenting).
3. Both files are recorded in the baseline.
4. The conflict is logged in the `conflicts` table with resolution status `unresolved` (or `keep_both` if auto-resolved).

### 7.4 Folder Decision Matrix (ED1-ED8)

Folders use existence-based reconciliation. A folder is "in sync" when `Baseline != nil` and the folder exists on both sides. Folders have no content hash -- their reconciliation is purely about presence or absence.

| E# | Local State | Remote State | Baseline | Action | Description |
|----|-------------|--------------|----------|--------|-------------|
| **ED1** | exists | exists | exists | no-op | Folder is known and present on both sides. In sync. |
| **ED2** | exists | exists | none | adopt (update synced) | Folder exists on both sides but was never synced. Adopt it into the baseline. |
| **ED3** | absent | exists | none | create locally | New remote folder. Create it locally. |
| **ED4** | absent | exists | exists | recreate locally | Previously synced folder was deleted locally but still exists remotely. Recreate it. |
| **ED5** | exists | absent | none | create remotely | New local folder. Create it remotely. |
| **ED6** | exists | deleted | exists | delete locally | Remote deleted a previously-synced folder. Propagate deletion locally. |
| **ED7** | absent | deleted | exists | cleanup | Previously synced folder is gone from both sides. Remove baseline entry. |
| **ED8** | absent | unchanged | exists | remote delete | Locally deleted, remote unchanged. Propagate deletion remotely. |

**Why existence-based**: Folders have no content to hash. `Baseline != nil` is sufficient to determine "was synced." This is enforced by the type system: `PathView.Baseline` is either a pointer to a `BaselineEntry` or nil.

### 7.5 Move Detection

Moves are detected and handled before the per-path classification step. Both paths involved in a move (source and destination) are excluded from the decision matrix -- they are fully handled by the move detection logic.

**Remote moves** (from ChangeMove events):

1. The Remote Observer detects the move during `convertToChangeEvent`: the delta API reports an item ID at a new location, and `baseline.ByID[itemID]` points to the old path.
2. The observer produces a `ChangeEvent{Type: ChangeMove, Path: newPath, OldPath: oldPath}`.
3. The Change Buffer dual-keys this event: the move event at `newPath` and a synthetic `ChangeDelete` at `oldPath`.
4. The planner's `detectMoves` function finds `ChangeMove` events and produces `ActionLocalMove` actions (rename the local file from old path to new path).

**Local moves** (hash-based correlation via baseline snapshot):

1. The Local Observer emits a `ChangeDelete` at the old path (baseline entry exists, file absent) and a `ChangeCreate` at the new path (no baseline, file present).
2. The planner's `detectMoves` function correlates these: a locally-deleted item whose `Baseline.LocalHash` matches a locally-created item's hash is a candidate move pair.
3. **Unique match constraint**: If multiple deleted items share the same hash, or multiple created items share the same hash, the correlation is ambiguous. Ambiguous candidates are skipped and fall through to the per-path classification (which will handle them as separate delete + create actions).
4. A successful unique match produces an `ActionRemoteMove` action (tell the server about the rename).

The baseline serves as the "before" snapshot naturally -- it has not been updated during observation or planning, so the baseline entry at the old path is still present and available for comparison.

### 7.6 Change Detection (Per-Side Baselines)

The planner uses per-side hash comparison to detect changes:

```go
func detectLocalChange(v PathView) bool {
    if v.Baseline == nil { return v.Local != nil }
    if v.Local == nil { return true }  // deleted
    if v.Baseline.ItemType == ItemTypeFolder { return false }
    return v.Local.Hash != v.Baseline.LocalHash
}

func detectRemoteChange(v PathView) bool {
    if v.Baseline == nil { return v.Remote != nil && !v.Remote.IsDeleted }
    if v.Remote == nil { return false }  // no observation
    if v.Remote.IsDeleted { return true }
    if v.Baseline.ItemType == ItemTypeFolder { return false }
    return v.Remote.Hash != v.Baseline.RemoteHash
}
```

**Per-side baseline hashes**: The baseline stores both `LocalHash` and `RemoteHash` separately. For normal files, these are identical. For files affected by SharePoint enrichment (where the server modifies file content post-upload), they diverge:

- After uploading `document.pdf` with local hash `AAA`:
  - SharePoint enriches the file, changing its content.
  - Server reports hash `BBB`.
  - Baseline records `LocalHash = AAA`, `RemoteHash = BBB`.
- On the next cycle:
  - `detectLocalChange`: local hash `AAA` == `Baseline.LocalHash` `AAA` -- unchanged.
  - `detectRemoteChange`: remote hash `BBB` == `Baseline.RemoteHash` `BBB` -- unchanged.
  - Result: EF1 (in sync). No re-upload loop.

This per-side approach handles enrichment natively without any special-case code paths.

### 7.7 Action Ordering (Dependency Edges)

The planner computes explicit dependency edges (`ActionPlan.Deps`) to ensure correctness. Actions run concurrently when their dependencies are satisfied — there are no artificial barriers between action types.

| Dependency Type | Rule | Example |
|----------------|------|---------|
| **Parent-before-child** | Parent folder must exist before child operations | `upload /A/B/f.txt` depends on `mkdir /A/B` |
| **Children-before-parent-delete** | All children must be removed before parent folder deletion | `rmdir /A` depends on `delete /A/file1.txt` |
| **Move target parent** | Move target parent folder must exist | `move /X -> /A/Z` depends on `mkdir /A` |

Actions whose parent folders already exist in the baseline have no dependencies and are immediately eligible for execution. All action types (downloads, uploads, folder creates, deletes, moves, conflicts, synced updates, cleanups) can run concurrently when their dependency edges are satisfied.

---

## 8. Safety Checks

### 8.1 S1-S7 Implementation (Pure Functions in Planner)

All safety invariants are implemented as pure functions operating on the `ActionPlan` and `Baseline`. They require no I/O and no database access, making them exhaustively testable.

**S1 -- Never delete remote on local absence without synced base**:
The planner checks `view.Baseline != nil` before emitting `ActionRemoteDelete`. This is structurally enforced by the decision matrix: EF6 (remote delete) requires `Baseline exists`. Without a baseline entry, the path classification falls to EF13 (new local, absent remote = upload) or produces no action (both absent, no baseline = nothing to do).

**S2 -- Never process deletions from incomplete enumeration**:
The Remote Observer returns the new delta token alongside events. The engine passes this token to `BaselineManager.CommitDeltaToken()` only after all cycle actions complete successfully. If the delta fetch is interrupted (e.g., network failure mid-pagination), no events are produced and no token is advanced. The `.nosync` guard fires in the Local Observer before any events are produced, preventing sync against unmounted volumes.

**S3 -- Atomic file writes for downloads**:
Implemented in the executor (see Section 9.4).

**S4 -- Hash-before-delete guard**:
Implemented in the executor (see Section 9.6).

**S5 -- Big-delete protection**:
Implemented as a pure function in the planner (see Section 8.2).

**S6 -- Disk space check**:
Implemented in the executor (see Section 9.4).

**S7 -- Never upload partial/temp files**:
The filter cascade excludes temporary file patterns. The planner applies filters symmetrically to both local and remote items, ensuring temp files are excluded regardless of which side they appear on.

### 8.2 Big-Delete Threshold

```go
func bigDeleteTriggered(plan *ActionPlan, baseline *Baseline, config *SafetyConfig) bool {
    // Count delete actions in the flat action list
    deleteCount := countActionsByType(plan.Actions, ActionLocalDelete, ActionRemoteDelete)
    baselineCount := len(baseline.ByPath)

    // Skip check if baseline is too small to be meaningful
    if baselineCount < config.BigDeleteMinItems {  // default: 10
        return false
    }

    // Trigger if absolute count exceeds threshold
    if deleteCount > config.BigDeleteMaxCount {  // default: 1000
        return true
    }

    // Trigger if percentage exceeds threshold
    percentage := float64(deleteCount) / float64(baselineCount) * 100
    return percentage > config.BigDeleteMaxPercent  // default: 50%
}
```

When big-delete protection triggers:
1. The sync cycle halts.
2. The action plan is printed (same as dry-run output) so the user can review.
3. The user must explicitly confirm or re-run with `--force` to proceed.
4. The delta token is NOT advanced, so the same changes will be re-fetched if the user cancels.

### 8.3 Disk Space Verification

Before each download, the executor queries available disk space on the filesystem containing the sync root. The download proceeds only if:

```
available_space - file_size >= config.MinFreeSpace
```

Default `MinFreeSpace` is 1 GB. If insufficient space is available:
1. The download is skipped.
2. A warning is logged.
3. The Outcome is produced with `Success: false` and a descriptive error.
4. The item will be retried on the next sync cycle (when more space may be available).

---

## 9. Executor

The executor takes an `ActionPlan` and dispatches actions to lane-based workers via the DepTracker. Each worker produces an individual `Outcome` — a self-contained result record that carries everything the baseline manager needs. Workers commit each outcome to the baseline per-action.

### 9.1 DAG Execution with Dependency Tracking

Actions are dispatched based on dependency satisfaction, not fixed phase ordering. The planner emits explicit dependency edges:

| Dependency Type | Rule | Example |
|----------------|------|---------|
| **Parent-before-child** | Parent folder must exist before child operations | `upload /A/B/f.txt` depends on `mkdir /A/B` |
| **Children-before-parent-delete** | All children must be removed before parent folder deletion | `rmdir /A` depends on `delete /A/file1.txt` |
| **Move target parent** | Move target parent folder must exist | `move /X -> /A/Z` depends on `mkdir /A` |

Actions whose parent folders already exist in the baseline have no dependencies and are immediately ready for execution. All action types (downloads, uploads, folder creates, deletes, moves, conflicts) are eligible to run concurrently when their dependencies are met. There are no artificial barriers between action types --- a download to `/A/x.txt` and an upload from `/B/y.txt` run in parallel if they have no dependency relationship.

**In-memory dependency tracker (DepTracker)**: Actions are added to the in-memory DepTracker after planning. The DepTracker tracks dependency edges and dispatches ready actions to worker channels. When action X completes, the tracker immediately dispatches any action Y whose last remaining dependency was X. No polling. A bounded working window (default 10K actions) keeps memory usage predictable.

**Lane-based worker dispatch**: Ready actions are routed to an interactive lane (small files, folder ops, deletes, moves) or a bulk lane (large transfers) with reserved workers per lane and a shared overflow pool. See [concurrent-execution.md](concurrent-execution.md) sections 4-6 for details.

**Per-action commits**: Each completed action is committed to baseline individually in a single SQLite transaction. The delta token is committed separately when all cycle actions complete.

### 9.2 Outcome Production

Every action produces exactly one `Outcome`, regardless of success or failure:

```go
type Outcome struct {
    Action       ActionType
    Success      bool
    Error        error
    Path         string
    OldPath      string     // for moves
    DriveID      driveid.ID
    ItemID       string     // from API response after upload
    ParentID     string
    ItemType     ItemType
    LocalHash    string     // hash of local content after action
    RemoteHash   string     // hash of remote content after action
    Size         int64
    Mtime        int64      // local mtime at sync time
    RemoteMtime  int64      // remote mtime for conflict records
    ETag         string
    ConflictType string     // "edit_edit", "edit_delete", "create_create" (conflicts only)
    ResolvedBy   string     // ResolvedByAuto for auto-resolved conflicts, "" otherwise
}
```

**Self-contained design**: Each Outcome carries all the fields needed to update the baseline. The baseline manager processes Outcomes without querying the database or any other state. This decouples execution from persistence.

**Failed Outcomes**: When `Success: false`, the Outcome carries an `Error` explaining the failure. Failed Outcomes are NOT committed to the baseline -- the item retains its current baseline state and will be retried on the next sync cycle.

**Concurrency**: Workers report outcomes through an in-memory result channel. Each worker commits its outcome to the baseline per-action, then reports success/failure to the engine via `WorkerResult` for cycle tracking (failure suppression, delta token commit decisions).

**Cross-drive operations**: Items under shortcuts (shared folder content) target the SOURCE drive for all API operations. The action's `DriveID` field contains `remoteItem.driveId`, not the user's own drive ID. The user's OAuth token has access to the shared content because the sharing permission grants it — the `graph.Client` token is per-account, not per-drive.

### 9.3 Error Classification (Fatal, Retryable, Skip, Deferred)

The executor classifies errors into four tiers and handles them internally before producing the final Outcome:

| Tier | Examples | Behavior |
|------|----------|----------|
| **Fatal** | Auth failure (401 after token refresh), impossible state, insufficient storage on server (507) | Stops the entire sync cycle. |
| **Retryable** | Network timeout, HTTP 429/500/502/503/504/408/412/509 | Exponential backoff with jitter, max 5 retries. For HTTP 429: use `Retry-After` header directly. After max retries: produce failed Outcome. |
| **Skip** | Permission denied (403), invalid filename (400), file locked (423) | Produce failed Outcome immediately. Item retried on next cycle. |
| **Deferred** | Parent directory not yet created, file locked locally | Queued for retry at the end of the current cycle. |

**Retry strategy**:
- Base delay: 1 second
- Factor: 2 (exponential)
- Maximum backoff: 120 seconds
- Jitter: plus or minus 25% of calculated backoff
- For HTTP 429: use the `Retry-After` header value directly
- Global rate awareness: shared token bucket across all workers prevents thundering herd

**HTTP error classification**:

| Status | Classification | Notes |
|--------|---------------|-------|
| 400 | Skip | Invalid request, bad filename |
| 401 | Fatal (after token refresh) | Auth failure |
| 403 | Skip | Permission denied, SharePoint retention policy |
| 404 | Skip (for deletes) / Retryable (for GET) | Item may have been deleted concurrently |
| 408 | Retryable | Request timeout |
| 409 | Retryable (moves) / Skip (other) | Conflict on moves: delete target, retry |
| 412 | Retryable | ETag stale: fetch fresh ETag, retry |
| 423 | Skip | File locked (SharePoint) |
| 429 | Retryable | Rate limited, honor Retry-After header |
| 500-504 | Retryable | Server errors |
| 507 | Fatal | Insufficient storage on server |
| 509 | Retryable (long backoff) | Bandwidth limit exceeded (SharePoint) |

### 9.4 Download Safety (.partial + Hash Verify + Atomic Rename)

Every download follows a strict safety protocol (S3):

1. **Malware check**: If the remote item has a malware flag, skip the download and produce a failed Outcome.
2. **Disk space check** (S6): Verify `available_space - file_size >= config.MinFreeSpace`. Skip if insufficient.
3. **Create `.partial` file**: Write to `<target>.partial` in the same directory as the target. This ensures the `.partial` file is on the same filesystem, enabling atomic rename.
4. **Stream content with hash computation**: Download the file content through an `io.TeeReader` that simultaneously writes to the `.partial` file and computes the QuickXorHash. Single-pass -- no re-reading required.
5. **Verify hash**: Compare the computed QuickXorHash against the expected hash from the remote item. Special handling for iOS `.heic` files (known API bug: log warning, do not fail).
6. **Set timestamps**: Apply `os.Chtimes` with the remote `mtime` to the `.partial` file.
7. **Atomic rename**: `os.Rename` from `.partial` to the target path. On the same filesystem, this is an atomic metadata operation -- the target file either has the complete old content or the complete new content, never a partial state.
8. **Produce Outcome**: With verified hashes, size, mtime, and server metadata (ETag, ItemID).

If the process crashes at any point before step 7, only the `.partial` file exists. On startup, the engine cleans up stale `.partial` files -- they are safe to delete and will be re-downloaded.

### 9.5 Upload Strategy (Simple vs. Chunked)

**Simple upload** (files up to 4 MB):

```
PUT /drives/{driveId}/items/{parentId}:/{name}:/content
```

Used for small files and zero-byte files. Zero-byte files ALWAYS use simple upload because upload sessions require a non-empty first chunk.

**Chunked upload** (files larger than 4 MB):

1. **Create upload session**: Includes `fileSystemInfo` facet with the local `mtime` to preserve timestamps and avoid double-versioning on Business/SharePoint.
2. **Persist session**: Save session URL, expiry, and progress to the file-based SessionStore BEFORE starting the upload (for crash recovery).
3. **Upload chunks**: Each chunk must be a multiple of 320 KiB (API requirement). Default chunk size: 10 MiB. Fragment upload URLs are pre-authenticated -- do NOT send Authorization header on chunk uploads.
4. **Complete**: The last chunk upload returns the completed `graph.Item` with server metadata.
5. **Clean up session**: Delete the session from the SessionStore.

**Post-upload hash verification**:
- Compare local hash against the server response hash.
- If hashes match: normal Outcome with `LocalHash == RemoteHash`.
- If hashes diverge AND the drive is a SharePoint library: enrichment detected. Store per-side hashes in the Outcome (`LocalHash != RemoteHash`).
- If hashes diverge AND NOT SharePoint: log a warning (potential corruption). Configurable escape hatch: `disable_upload_validation`.

### 9.6 Delete Guards

**Remote deletion**:

1. Send DELETE request with `If-Match` ETag header for conditional deletion.
2. HTTP 404: Item already deleted remotely -- not an error, produce success Outcome.
3. HTTP 403: Permission denied (SharePoint retention policy) -- skip.
4. HTTP 423: Locked (SharePoint) -- skip.
5. HTTP 412: ETag stale. Fetch fresh item metadata, retry once with new ETag.
6. Recycle bin: `config.UseRecycleBin` controls whether items are soft-deleted to the OneDrive recycle bin (default) or permanently deleted.

**Local deletion** (with S4 hash-before-delete guard):

1. Compute the current local file hash (QuickXorHash).
2. Compare against `action.View.Baseline.LocalHash`.
3. **Hashes match**: File is unchanged since the last sync. Safe to delete.
4. **Hashes differ**: File was modified after the last sync -- the user made changes that haven't been synced. Instead of deleting, create a conflict copy to preserve the user's work.
5. `config.UseLocalTrash`: Controls whether local deletes use the OS trash (FreeDesktop on Linux, Finder trash on macOS) or permanently remove files.

**Folder deletion**: Folders are deleted bottom-up (deepest first), enforced by dependency edges. Before deleting a local folder, the executor verifies it is empty. If the folder contains unexpected files (e.g., new files created after the scan but before execution), the delete is skipped and deferred to the next cycle.

**Move execution details**:

- **ActionLocalMove**: Rename the local file/folder from `action.OldPath` (source) to `action.Path` (destination). Uses `os.Rename` which is atomic on the same filesystem. If source and destination are on different filesystems, falls back to copy + delete.
- **ActionRemoteMove**: Call the `MoveItem` API to rename the item on the server. The API call specifies the new parent ID and new name. On success, the Outcome carries both `OldPath` and `Path` for the baseline manager to update.

**Conflict execution**:

For each conflict action, the executor applies the configured resolution strategy:

| Strategy | Behavior |
|----------|----------|
| `keep_both` (default) | Download remote version to target path. Rename local version to `<name>.conflict-YYYYMMDD-HHMMSS.<ext>`. Both files remain. |
| `keep_local` | Upload local version to remote, overwriting the remote version. |
| `keep_remote` | Download remote version, overwriting local. Local changes are lost. |
| `manual` | Log the conflict. Take no action. User resolves via `conflicts` and `resolve` commands. |

Conflict copies use timestamp-based naming: `document.conflict-20260222-143052.pdf`. This format is self-documenting (the user can see when the conflict was detected) and shorter than hostname-based naming schemes.

**Synced update execution**:

Synced updates (EF4, EF11, ED2) require no I/O. The executor produces an Outcome populated from the PathView's remote and local state:

- `ItemID`, `ParentID`, `ETag`: from `PathView.Remote` (server is authoritative for metadata).
- `LocalHash`: from `PathView.Local.Hash` (what is on disk).
- `RemoteHash`: from `PathView.Remote.Hash` (what server reports).
- `Size`, `Mtime`: from whichever side is considered authoritative (remote for downloads, local for uploads; for convergent cases, they match).

**Transfer pipeline**:

The executor delegates transfer operations to lane-based workers managed by the dependency tracker:

| Lane | Reserved Workers | Purpose |
|------|-----------------|---------|
| Interactive | max(2, total/8) | Small files (<10 MB), folder ops, deletes, moves |
| Bulk | max(2, total/8) | Large file transfers (>=10 MB) |
| Shared | remainder | Dynamically assigned; interactive priority |
| Checkers | 8 (separate pool) | Local hash computation for change detection |

Total lane workers = `runtime.NumCPU()` or user-configured cap (minimum 4). Workers are persistent goroutines pulling from tracker channels. Each worker receives its own context for per-action cancellation. Canceling the root context propagates to all workers.

**Bandwidth limiting**: A token-bucket rate limiter optionally caps total transfer bandwidth (configured via `bandwidth_limit`). The limiter is shared across all download and upload workers, preventing any single worker from consuming the entire allowance. Scheduled throttling supports time-of-day rules.

**Dispatch order**: Actions are dispatched as their dependencies are satisfied. Within a lane, the tracker's ready channel provides FIFO ordering. The dependency DAG naturally ensures correctness (parents before children, children before parent deletes) without requiring explicit sorting.

---

## 10. Baseline Manager

The Baseline Manager is the **sole writer** to the SQLite database. It loads the baseline at cycle start and commits outcomes at cycle end. The single-writer design means database concurrency is never a concern.

### 10.1 Load (Snapshot for Planner)

```go
func (m *BaselineManager) Load(ctx context.Context) (*Baseline, error)
```

Reads the entire `baseline` table into memory and constructs two lookup maps:

```go
type Baseline struct {
    ByPath map[string]*BaselineEntry   // primary lookup for all components
    ByID   map[string]*BaselineEntry   // keyed by item_id, for remote move detection
}
```

**ByPath**: Used by the local observer (compare filesystem state against known state), the planner (construct PathViews), and the change buffer (move dual-keying).

**ByID**: Used by the remote observer to detect moves (delta API reports an item_id at a new location; baseline lookup reveals the old path).

The baseline is loaded once at cycle start and treated as read-only by all components during the pipeline. Observers, buffer, planner, and executor all reference the same frozen snapshot. This ensures consistency: no component sees partially-updated state.

### 10.2 Commit (Per-Action Atomic Transactions)

Each completed action is committed individually in a **per-action atomic SQLite transaction** that updates the baseline:

```sql
BEGIN;
  -- Baseline update (varies by action type)
  INSERT OR REPLACE INTO baseline (...) VALUES (...);
COMMIT;
```

**Per-action commit operations by type**:
- **Download, Upload, UpdateSynced, FolderCreate**: `INSERT ... ON CONFLICT(path) DO UPDATE` into the `baseline` table. All 11 columns are written from the Outcome fields.
- **LocalMove, RemoteMove**: `DELETE` the old path entry, then `INSERT` the new path entry.
- **LocalDelete, RemoteDelete, Cleanup**: `DELETE FROM baseline WHERE path = ?`.
- **Conflict**: `INSERT INTO conflicts` table. If keep-both resolution created files, also update baseline for the new paths.

**Delta token commit**: The delta token is committed in a **separate transaction** when all actions for a cycle complete successfully:

```sql
BEGIN;
  INSERT OR REPLACE INTO delta_tokens (drive_id, token, updated_at) VALUES (?, ?, ?);
COMMIT;
```

This is safe because all per-action commits have already made the baseline consistent. The token commit is the final step that "seals" the cycle.

**Composite delta token keys**: Delta tokens use a composite key `(drive_id, scope_id)` to support multiple tokens per configured drive. The primary delta token has `scope_id = ""`. Each shortcut has its own token with `scope_id = remoteItem.id` and `scope_drive = remoteItem.driveId`. All delta tokens for a drive are committed together when a cycle completes.

**Crash recovery via idempotent planner**: If the process crashes mid-cycle, individual per-action commits are already durable in the baseline. The delta token has not been advanced, so the same delta is re-fetched on restart. The planner is idempotent: it compares current state against the updated baseline and detects already-committed actions as convergent (EF4/EF11), skipping them automatically. Remaining actions are re-planned and executed normally.

**Failed Outcomes** (where `Success == false`) are not committed to the baseline. Their corresponding items retain their current baseline state and will be retried on the next sync cycle.

**Baseline table schema**:

```sql
CREATE TABLE baseline (
    -- Primary key: path relative to sync root (NFC-normalized, URL-decoded)
    -- The filename component is always filepath.Base(path), so no separate
    -- name column is needed.
    path            TEXT    PRIMARY KEY,

    -- Identity: server-assigned, used for remote operations and move detection
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

    -- Confirmed synced state
    size            INTEGER,
    mtime           INTEGER,    -- local mtime at sync time (Unix nanoseconds)
                                -- OneDrive truncates to whole seconds; stored at full
                                -- nanosecond precision from local FS for fast-check
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),

    -- Remote metadata for efficient delta processing
    etag            TEXT
);

-- For remote move detection: look up baseline entry by item_id
CREATE UNIQUE INDEX idx_baseline_item ON baseline(drive_id, item_id);

-- For cascading path operations (folder renames update children)
CREATE INDEX idx_baseline_parent ON baseline(parent_id);
```

**11 columns.** The baseline stores only confirmed synced state. Everything else is ephemeral (rebuilt from the API or filesystem each cycle).

**BaselineEntry type** (in-memory representation):

```go
type BaselineEntry struct {
    Path       string
    DriveID    driveid.ID
    ItemID     string
    ParentID   string
    ItemType   ItemType    // file, folder, root
    LocalHash  string      // hash of local content at last sync
    RemoteHash string      // hash of remote content at last sync
    Size       int64
    Mtime      int64       // local mtime at sync time (Unix nanoseconds)
    SyncedAt   int64       // when this entry was last confirmed synced
    ETag       string
}
```

**Supporting tables** (managed alongside the baseline):

| Table | Purpose | Writer |
|-------|---------|--------|
| `delta_tokens` | Delta cursor per drive | BaselineManager (same txn as baseline) |
| `conflicts` | Conflict tracking with resolution status | BaselineManager |
| `schema_migrations` | Schema version tracking | Engine (on startup) |
| SessionStore (file-based) | Crash recovery for large uploads (JSON files in data dir) | TransferManager (pre-upload) |

**SQLite configuration**:

```sql
PRAGMA journal_mode = WAL;           -- concurrent readers + single writer
PRAGMA synchronous = FULL;           -- durability on crash
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;          -- defense-in-depth (5 second wait)
PRAGMA journal_size_limit = 67108864; -- 64 MiB WAL size limit
```

WAL mode enables `status`, `conflicts`, and `verify` commands to read the database concurrently while a sync cycle is writing. The `busy_timeout` is defense-in-depth against any unexpected concurrent access.

**Separate database file per drive**: Each drive gets its own SQLite database file. This provides complete isolation between accounts. Cross-drive data contamination is structurally impossible.

**Timestamp conventions**: All timestamps are stored as INTEGER Unix nanoseconds (UTC). Validation rules applied at ingestion:

| Condition | Action |
|-----------|--------|
| `0001-01-01T00:00:00Z` or dates before 1970 | Fall back to current UTC time |
| Dates more than 1 year in the future | Fall back to current UTC time |
| Fractional seconds from OneDrive | Truncated to whole seconds for comparison |
| Local filesystem nanoseconds | Stored at full precision for mtime fast-check |

**Path conventions**:

- All paths are relative to the sync root.
- NFC-normalized (required for macOS APFS compatibility).
- URL-decoded (delta API returns URL-encoded names).
- Forward slash as separator (even on macOS, for database consistency).
- No leading or trailing slashes.

---

## 11. Initial Sync

### 11.1 Batched Processing for Large Drives

On the first sync run, no delta token exists. The delta API returns **every item** in the drive -- functionally a full enumeration. For drives with hundreds of thousands of items, holding all events in memory would exceed the 100 MB memory budget.

**Batch processing** solves this:

```
For initial sync:
  1. Fetch delta page by page
  2. Every 50K items:
     a. Flush buffer
     b. Plan (only these items)
     c. Execute (downloads/uploads)
     d. Commit partial baseline + intermediate delta token
  3. After all pages: commit final delta token
```

Each batch processes 50K items, commits the results, and frees memory before proceeding to the next batch. The intermediate delta token saved in step 2d is a "next link" token -- it represents progress through the paginated delta response.

**Parallel initial enumeration** (optimization for large drives): For drives with >100K items, sequential delta pagination can be slow. An alternative approach uses parallel `/children` API calls with 8 concurrent walkers to enumerate the directory tree in parallel. The engine falls back to delta-based enumeration if `/children` enumeration fails.

After initial sync completes using parallel enumeration, the engine requests a fresh delta token via `delta(latest)` to establish the baseline cursor.

### 11.2 Memory Budget

| Scenario | Baseline | Events | PathViews | Plan | Outcomes | **Peak** |
|----------|----------|--------|-----------|------|----------|----------|
| **Initial sync, 100K items** | 0 MB | ~27 MB | ~2.4 MB | ~5 MB | ~24 MB | **~77 MB** |
| **Steady state, 100K items** | ~19 MB | ~0.03 MB | ~0.004 MB | negligible | negligible | **~20 MB** |
| **Watch mode, 100K items** | ~19 MB | ~0.003 MB | negligible | negligible | negligible | **~20 MB** |
| **Initial sync, 500K items (batched)** | growing | ~14 MB per batch | ~2 MB | ~3 MB | ~12 MB | **~50 MB** |

All scenarios stay within the PRD budget of < 100 MB for 100K files. The 500K-item scenario uses batch processing to bound peak memory to ~50 MB.

**Per-item memory estimates**:

| Type | Estimated Size | Notes |
|------|---------------|-------|
| `ChangeEvent` | ~280 bytes | strings (path, name, hashes) + ints |
| `BaselineEntry` | ~200 bytes | path + IDs + hashes + metadata |
| `PathView` | ~24 bytes | three pointers |
| `RemoteState` | ~200 bytes | derived from event fields |
| `LocalState` | ~120 bytes | path + hash + metadata |
| `Outcome` | ~250 bytes | self-contained result |
| `Action` | ~80 bytes | type + path + pointer to PathView |

**Initial sync detection**:

```go
func (e *Engine) isInitialSync(ctx context.Context) bool {
    token, _ := e.baselineMgr.GetDeltaToken(ctx, e.driveID)
    return token == ""
}
```

When no delta token exists, the engine knows this is the first run and activates batch processing.

**Saving the initial delta token**: After initial sync completes, the baseline commit transaction saves the delta token extracted from the final `deltaLink`. If parallel `/children` enumeration was used instead of delta, the engine requests a fresh token via `delta(latest)` to establish the baseline cursor.

---

## 12. Continuous Mode (`--watch`)

### 12.1 fsnotify + Delta Polling

In watch mode, the engine runs two background observers concurrently:

**Local Observer** (fsnotify): Uses `fsnotify/fsnotify` to watch the sync root recursively. Filesystem events (create, modify, delete, rename) are converted to `ChangeEvent` values and emitted to the buffer in real time.

**Remote Observer** (delta polling): Polls the delta API at a configurable interval (default: 5 minutes via `config.PollInterval`). Each poll calls `FullDelta` and emits any new events to the buffer.

Both observer goroutines emit events to the same change buffer. If local and remote changes arrive within the same debounce window, they are processed together in a single batch.

```
┌──────────────────────────────────────────────────────────────────┐
│                        Engine.RunWatch                            │
│                                                                    │
│  ┌──────────┐   ┌──────────┐                                     │
│  │ Remote   │   │ Local    │   ┌──────────────────┐              │
│  │ Observer │   │ Observer │   │ Config Reload    │              │
│  │ goroutine│   │ goroutine│   │ (SIGHUP)         │              │
│  └────┬─────┘   └────┬─────┘   └────┬─────────────┘              │
│       │ emit()       │ emit()       │                             │
│       └──────┬───────┘              │                             │
│              ▼                      │                             │
│  ┌───────────────────┐              │                             │
│  │  Change Buffer    │              │                             │
│  │  (2s debounce)    │              │                             │
│  └────────┬──────────┘              │                             │
│           │ Ready()                 │                             │
│           ▼                         ▼                             │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │ select {                                                    │ │
│  │   case <-buffer.Ready():  -> Plan + DepTracker dispatch      │ │
│  │   case <-configReload:    -> Re-init filters                │ │
│  │   case <-ctx.Done():      -> GracefulShutdown               │ │
│  │ }                                                           │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                    │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │  Workers (persistent, always running)                        │ │
│  │    interactiveWorker[0..M] <-- tracker.interactive channel   │ │
│  │    bulkWorker[0..N]        <-- tracker.bulk channel          │ │
│  │    sharedWorker[0..K]      <-- both channels                 │ │
│  │                                                               │ │
│  │    on completion: per-action baseline commit                   │ │
│  │    tracker.Complete(id) -> dispatch newly ready actions       │ │
│  └─────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
```

### 12.2 Debounce Intervals

The default debounce window is 2 seconds. After the last event arrives, the buffer waits 2 seconds before signaling readiness.

| Scenario | Handling |
|----------|----------|
| **Rapid edits** (IDE auto-save) | 2-second debounce coalesces multiple saves into a single batch |
| **vim atomic save** (delete + create pattern) | Both events arrive within the debounce window; final state is the new file |
| **LibreOffice temp files** | Filtered out by `.~*`, `~*` patterns (S7) |
| **File created and deleted within window** | Both events arrive; final state is "absent"; planner produces no action |
| **Large copy operation** (many files) | Events accumulate in buffer; processed as one batch when debounce fires |

### 12.3 Same Infrastructure, Different Event Sources

Watch mode and one-shot mode share the same planner, tracker, workers, and baseline manager. The difference is in how events arrive and how the pipeline flows:

- **One-shot**: `FullDelta` + `FullScan` produce a complete snapshot. All events are planned at once, all actions added to the DepTracker at once, workers drain the full batch.
- **Watch mode**: `Watch` goroutines produce incremental events. Events arrive in small batches as the debounce window fires. New actions are added incrementally to the DepTracker while workers are already draining previous batches. Observers and workers run independently.

The same tracker and per-action commit infrastructure serves both modes. This means that every code path exercised in one-shot mode is also exercised in watch mode. There are no watch-mode-only code paths that could harbor untested bugs.

**Graceful shutdown** (two-signal protocol):

| Signal | Action |
|--------|--------|
| **First SIGINT/SIGTERM** | Stop accepting new events. Drain in-flight executor operations (configurable timeout). Save delta token checkpoint. Clean up `.partial` files. Exit 0. |
| **Second SIGINT/SIGTERM** | Cancel all operations immediately. SQLite WAL ensures DB consistency even on abrupt termination. Exit 1. |
| **SIGHUP** | Reload configuration. Re-initialize filter engine. Detect stale files from filter changes. Update bandwidth limiter settings. Continue running. |

**SIGHUP hot-reloadable options**:
- Filter rules (`skip_dotfiles`, `skip_dirs`, `skip_files`, `max_file_size`)
- Bandwidth settings (`bandwidth_limit`)
- Poll interval
- Safety thresholds (`big_delete_threshold`, `min_free_space`)
- Log level

**NOT hot-reloadable** (require restart):
- `sync_dir`, `drive_id`
- Worker pool sizes (`parallel_downloads`, `parallel_uploads`, `parallel_checkers`)
- Network settings

**Pause/Resume** (watch mode only):

When paused, observers continue running and the buffer accumulates events. Workers stop pulling from the DepTracker (paused flag). In-flight actions complete normally. When resumed, the buffer flushes all accumulated events, the planner generates new actions, they are added to the DepTracker, and workers resume pulling. If the buffer exceeds a high-water mark (100K events) during an extended pause, the engine collapses to a "full sync on resume" flag to avoid excessive memory consumption.

**Context tree** (watch mode goroutine hierarchy):

```
rootCtx (Engine.RunWatch)
|-- remoteObserverCtx
|-- localObserverCtx
|-- trackerCtx
    |-- interactiveWorker[0..M]
    |-- bulkWorker[0..N]
    +-- sharedWorker[0..K]
```

Workers are persistent goroutines pulling from tracker channels, not scoped to individual planning passes. Canceling `rootCtx` propagates to all goroutines, triggering graceful shutdown.

**Platform considerations for watch mode**:

| Platform | FS Events | Notes |
|----------|-----------|-------|
| **Linux** | inotify | Reliable for local filesystems. Unreliable on NFS/CIFS -- fall back to periodic full scan with configurable interval. See inotify watch limits below. |
| **macOS** | FSEvents | Reliable on APFS and HFS+. Events may arrive with NFD-encoded paths -- dual-path normalization handles this. No per-directory watch limit. |
| **NFS/network FS** | None reliable | Detected on startup. Warning logged. Engine falls back to periodic full scan (configurable interval, default 5 minutes). |

### 12.4 inotify Watch Limits (Linux)

Linux inotify requires one watch per directory. The default kernel limit is 8192 (`/proc/sys/fs/inotify/max_user_watches`), though many distributions set it higher.

**Detection at startup**: Before starting inotify watches, the engine reads `/proc/sys/fs/inotify/max_user_watches` and estimates the total watch count from the baseline directory counts. For multi-drive sync, the estimate sums across all enabled drives.

**Warning at 80%**: If estimated watches exceed 80% of the limit, the engine logs a warning with sysctl instructions for increasing the limit.

**Per-drive fallback on ENOSPC**: When `inotify_add_watch` returns `ENOSPC` (no watches available), the affected drive falls back to periodic full scan at `poll_interval`. Other drives retain their inotify watches. This is a per-drive decision — one drive exhausting watches does not degrade the others.

**No per-drive budget**: Watches are allocated first-come first-served. No reservation or quota system exists. See [MULTIDRIVE.md §9](MULTIDRIVE.md#9-operational-constraints) for multi-drive implications.

### 12.5 Multi-Drive Watch Mode

In multi-drive sync, each enabled drive runs its own watch loop (observer pair + buffer + planner + worker dispatch). The multi-drive orchestrator manages the lifecycle of these per-drive watch loops. See [MULTIDRIVE.md §11](MULTIDRIVE.md#11-multi-drive-orchestrator-design-gap) for the orchestrator design (currently an unresolved design gap — to be specified before Phase 7.0).

**Idle resource consumption**: In watch mode, CPU usage during idle is proportional to the remote polling interval (one delta API call per interval) plus inotify/FSEvents overhead (near-zero when no files change). Memory usage is the baseline cache (~19 MB for 100K items) plus buffer overhead (~0 when no pending events). The target is < 1% CPU when idle.

---

## 13. Crash Recovery

The event-driven architecture provides natural crash recovery because the baseline and delta token are only updated in the final atomic commit. A crash at any point before the commit means no persistent state was changed, and the next cycle re-fetches the same data.

### 13.1 Incomplete Downloads/Uploads

**Incomplete downloads**: On startup, the engine globs for `**/*.partial` in the sync root and removes them. These are incomplete downloads that are safe to delete -- they will be re-downloaded in the next cycle. The target file was never touched (atomic rename happens only after hash verification).

**Incomplete uploads** (simple upload): The API may not have received the file. No local side effects. The file will be re-uploaded in the next cycle.

**Incomplete uploads** (chunked session): Upload sessions are persisted to the file-based SessionStore BEFORE the upload begins. On crash recovery:

1. Load all active upload sessions from the SessionStore.
2. Check each session's expiry (typically 48 hours from creation).
3. **Expired sessions**: Delete from SessionStore. The file will be re-uploaded from scratch in the next cycle.
4. **Valid sessions**: Verify the local file's current hash matches the session's `local_hash`. If it matches, resume from the `bytes_uploaded` offset. If the file was modified since the session started, discard the session and re-upload from scratch. The Graph API supports byte-range continuation natively.
5. On successful completion: delete the session from the SessionStore and include the result in the Outcomes for baseline commit.

### 13.2 Baseline Consistency Guarantees

| Crash Point | State Changed | Recovery |
|-------------|---------------|----------|
| During `BaselineManager.Load()` | Nothing | Re-read baseline |
| During `RemoteObserver.FullDelta()` | Nothing (events are in-memory) | Re-run cycle. Delta re-fetched from saved token. |
| During `LocalObserver.FullScan()` | Nothing (events are in-memory) | Re-run cycle. Filesystem re-scanned. |
| During `Planner.Plan()` | Nothing (pure function) | Re-run cycle. |
| During execution | Completed actions already committed to baseline individually. In-memory DepTracker state lost. | On restart: delta token not advanced, so same delta is re-fetched. Idempotent planner detects already-committed actions as convergent (EF4/EF11) and skips them. Remaining actions are re-planned and executed. Upload sessions resumed via SessionStore. |
| During per-action commit | SQLite transaction: baseline update or nothing | If rolled back: action is re-planned on restart (idempotent). If committed: action is durable. |
| During watch mode (between cycles) | Buffer has pending events. Completed actions in baseline. | Events re-observed by the watchers. Debounce/dedup handles redundancy. Completed actions persist. |

**Worst-case scenario**: A 30-minute initial sync crashes at minute 29 during execution. 29 minutes of completed transfers are already committed to baseline individually. On restart:

1. Delta token has not been advanced, so the same delta is re-fetched.
2. Idempotent planner compares delta against updated baseline: already-committed actions appear as convergent (EF4/EF11) and are skipped.
3. Remaining actions are planned and added to the DepTracker.
4. For uploads with active sessions: the file-based SessionStore provides session URLs; the executor queries the upload endpoint and resumes from `bytes_uploaded` offset.
5. For downloads: `.partial` files enable resume via HTTP `Range` header.
6. Workers execute only the remaining actions. No re-downloading of completed transfers.
7. Time wasted: seconds to re-fetch delta and re-plan, not 29 minutes of re-downloading.

### 13.3 Upload Session Persistence

Upload sessions are persisted via the file-based **SessionStore**. Each active chunked upload session is stored as a JSON file in the sync metadata directory. The SessionStore tracks:

- `drive_id` -- which drive the upload targets
- `item_id` -- server-assigned ID (null for new file uploads, assigned after completion)
- `local_path` -- source file on the local filesystem
- `local_hash` -- QuickXorHash of the local file at session start (for change detection on resume)
- `session_url` -- pre-authenticated upload URL from the Graph API
- `expiry` -- session expiration timestamp
- `bytes_uploaded` -- progress offset for resume
- `total_size` -- total file size in bytes

The executor writes to the SessionStore BEFORE starting a chunked upload (to enable crash recovery). The session file is deleted when the upload outcome is committed to baseline.

**Delta token integrity**: The delta token is committed only when all actions for a cycle complete successfully. Individual per-action commits update the baseline but do not advance the delta token. This guarantees:

- If the process crashes mid-cycle: completed actions are already in baseline (committed individually). The delta token has not been advanced, so the same delta is re-fetched. The idempotent planner detects already-committed actions as convergent (EF4/EF11) and skips them. Remaining actions are re-planned and executed normally.
- If all actions complete but the token commit fails: all outcomes are in baseline. The token commit is retried on restart.
- Upload sessions are persisted in the file-based SessionStore and can be resumed from `bytes_uploaded` offset. Downloads resume via `.partial` file size + HTTP `Range` header.

---

## 14. Sync Report

Every sync cycle produces a `SyncReport` summarizing the results:

```go
type SyncReport struct {
    Mode        SyncMode
    StartedAt   int64
    CompletedAt int64
    Duration    time.Duration

    // Counts by action type
    Downloaded  int
    Uploaded    int
    Deleted     int       // local + remote deletes
    Moved       int       // local + remote moves
    Conflicts   int       // conflicts detected
    Synced      int       // convergent edits/creates (no transfer needed)
    Cleaned     int       // baseline entries removed

    // Errors
    Skipped     int       // items skipped due to errors
    Errors      []string  // human-readable error descriptions

    // Safety triggers
    BigDelete   bool      // big-delete protection was triggered

    // Transfer stats
    BytesDown   int64
    BytesUp     int64
}
```

The report is built from worker pool statistics and `WorkerResult` channel data. For dry-run mode, the report is built from the `ActionPlan` with counts reflecting planned (not executed) actions.

**Output formats**:
- **Human-readable** (default): Summary line for quick glance, followed by per-action details if `--verbose`.
- **JSON** (`--json`): Machine-readable output for scripting and monitoring integration.

**Example human-readable output** (one-shot, bidirectional):

```
Sync complete: 12 downloaded, 5 uploaded, 2 deleted, 1 conflict
  Transferred: 45.2 MB down, 12.8 MB up in 23s
  Skipped: 1 (permission denied: /Documents/locked.xlsx)
```

**Example human-readable output** (dry-run):

```
Dry-run: 12 downloads, 5 uploads, 2 deletes, 1 conflict planned
  No changes made. Run without --dry-run to execute.
```

**Example human-readable output** (big-delete triggered):

```
WARNING: Big-delete protection triggered.
  1,247 deletions planned (62.4% of 2,000 synced items).
  This exceeds the safety threshold (50%).
  Review the planned deletions and re-run with --force to proceed.
```

---

## 15. Verify Command

The `verify` command performs a full-tree integrity check by comparing local files against the baseline. It is a read-only operation with zero side effects.

```go
func (e *Engine) Verify(ctx context.Context) (*VerifyReport, error)
```

**Algorithm**:

1. Load the baseline.
2. For each file entry in the baseline:
   a. **Check existence**: Does the file exist locally at `sync_root/path`?
   b. **Check size**: Does the local file size match `Baseline.Size`?
   c. **Check local hash**: Compute QuickXorHash of the local file. Compare against `Baseline.LocalHash`.
   d. **Check enrichment**: If `Baseline.RemoteHash` differs from `Baseline.LocalHash` and the local hash matches `Baseline.LocalHash`, the file is enriched (informational, not an error).
3. Produce a `VerifyReport`:

```go
type VerifyReport struct {
    Verified           int                // files successfully verified
    Missing            []string           // baseline entries with no local file
    SizeMismatch       []VerifyMismatch   // size doesn't match baseline
    LocalHashMismatch  []VerifyMismatch   // local hash doesn't match baseline local_hash
    RemoteHashMismatch []VerifyMismatch   // unexpected hash divergence
    Enriched           []string           // expected enrichment (informational)
}

type VerifyMismatch struct {
    Path     string
    Expected string
    Actual   string
}
```

**Usage scenarios**:

- **Post-sync validation**: Run `verify` after initial sync to confirm all files were transferred correctly.
- **Periodic integrity check**: In watch mode, `verify_interval` (config option, default disabled) triggers automatic verification on a schedule (e.g., weekly).
- **Troubleshooting**: When a user suspects data corruption, `verify` provides a detailed report of discrepancies without modifying any files.

**Performance**: Verify is I/O-bound (computing hashes for every file in the baseline). For large drives, hash computation uses the checker worker pool for parallelism. Progress is reported via structured logging.

**Verify does NOT**:
- Modify any files.
- Contact the Graph API (local-only check against baseline).
- Update the baseline or any database tables.
- Trigger sync actions for discrepancies found.

If discrepancies are found, the user can run a regular sync cycle to reconcile them. The verify report provides the information needed to decide whether to re-sync, investigate further, or accept the differences.

**Remote verification** (future enhancement): A future version may optionally query the Graph API to compare `Baseline.RemoteHash` against the server's current hash, detecting remote-side corruption. This would require API calls proportional to the number of baseline entries and is not implemented in the initial version.

**Exit codes**:

| Code | Meaning |
|------|---------|
| 0 | All files verified successfully |
| 1 | One or more discrepancies found (details in report) |
| 2 | Fatal error (could not load baseline, filesystem error) |
