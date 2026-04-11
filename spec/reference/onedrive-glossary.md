# OneDrive Glossary

Project-specific vocabulary. For standard Microsoft Graph API terms (DriveItem, delta token, facet, etc.), see [Microsoft's official documentation](https://learn.microsoft.com/en-us/graph/api/resources/driveitem).

## Drive Identity

### Canonical ID
A structured string identifying a drive configuration: `type:email` (personal/business) or `type:email:site:library` (SharePoint) or `type:email:sourceDriveID:sourceItemID` (shared). Parsed and validated by the `driveid.CanonicalID` type. Used as the key in TOML config drive sections.

### Display Name
The human-facing identity for a drive, auto-derived at `drive add` time. Personal/business = email, SharePoint = `"site / lib"`, shared = `"{FirstName}'s {FolderName}"`. User-editable. CLI `--drive` flag matches display names first.

### Drive ID (`driveid.ID`)
A type-safe wrapper around the raw drive ID string from Microsoft. Normalizes to lowercase and zero-pads to 16 characters on construction, preventing the driveId truncation and casing bugs documented in `graph-api-quirks.md`.

### ItemKey
A composite key `(DriveID, ItemID)` representing a globally unique item identity across drives. Replaces ad-hoc `driveID+":"+itemID` string concatenation.

### Token Canonical ID
The canonical ID used for token file resolution. SharePoint drives share their business account's token. Shared drives share their parent account's token. Resolved by `config.TokenCanonicalID()`.

## Sync Engine

### Baseline
The authoritative record of what has been successfully synced. Each entry records the item's ID, drive ID, path, hash, size, and mtime at the time of last successful sync. The planner uses the baseline as the "common ancestor" for three-way merge decisions.

### Remote State
The authoritative record of what the server has, populated from delta query observations. Each item has a state machine: `observed` → `syncing` → `synced`. Used by the planner alongside the baseline and local scan.

### Sync Failure
A persistent record of a file that failed to sync. Two categories: **transient** (retried automatically with exponential backoff) and **actionable** (requires user intervention or an explicit safety approval such as `resolve deletes`, depending on the issue type). Stored in the `sync_failures` table.

### Action
The planner's output — a decision about what to do with a specific path. Types: `ActionDownload`, `ActionUpload`, `ActionLocalDelete`, `ActionRemoteDelete`, `ActionConflict`, `ActionMkdir`, `ActionRmdir`, `ActionNoop`, `ActionCleanup`.

### PathView
A unified view of a path's state across all three data sources (local scan, remote state, baseline). The planner operates on PathViews.

### Observation
The process of scanning for changes — either locally (filesystem walk + inotify) or remotely (delta query). Observations produce `ChangeEvent` values that feed into the buffer and planner.

### Reconciler
The component that detects items stuck in `syncing` state after a crash and resets them to `observed` for re-planning. Runs at the start of each sync cycle.

### Orchestrator
The top-level multi-drive sync coordinator. Manages multiple DriveRunners, owns the Unix control socket, handles config reload, and coordinates pause/resume. Implemented in `internal/multisync`. Even single-drive `sync` still goes through the control plane so watch and one-shot share the same drive-lifecycle rules.

### DriveRunner
A per-drive goroutine wrapping a sync Engine. Provides panic recovery and error isolation between drives. Implemented in `internal/multisync`.

### Full Reconciliation
A sync mode (`sync --full`) that runs a fresh delta with no token (enumerates ALL remote items) and compares against baseline to detect orphans — items present in baseline but absent from the server. Corrects deletions missed by incremental delta.

## Data Architecture

### Sync Store
The unified SQLite database per drive containing three tables: `remote_state`, `baseline`, and `sync_failures`. Accessed through typed sub-interfaces (`ObservationWriter`, `OutcomeWriter`, `FailureRecorder`, `StateReader`, `StateAdmin`).

### Session Store
File-based persistence for upload session resume. Session files are keyed by a hash of `(driveID, localPath)` and store the upload URL, expiration, and byte offset.

### Transfer Manager
The unified download/upload component shared between CLI (`get`/`put`) and sync engine. Handles simple upload vs chunked upload routing, download resume via `.partial` files, and hash verification.

## CLI

### CLIContext
The per-command context struct stored in `cmd.Context()`. Contains flags, logger, config path, and optionally the resolved config and session provider. Two-phase initialization: Phase 1 (always) sets flags/logger, Phase 2 (data commands only) loads config.

### Session Provider
Caches `TokenSource` instances by token file path. Multiple drives sharing a token path share one `TokenSource`, preventing OAuth2 refresh token rotation races.
