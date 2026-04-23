# OneDrive Glossary

Project-specific vocabulary. For standard Microsoft Graph API terms such as
`DriveItem`, delta, and facets, see Microsoft's official documentation.

## Drive Identity

### Configured Drive

One user-configured sync target. A configured drive owns exactly one config
entry, one sync engine, one state DB, and one primary remote observation root.

### Canonical ID

The structured config identity string for a configured drive, such as
`personal:email@example.com` or `shared:email@example.com:<driveID>:<itemID>`.

### Backing Drive ID

The Microsoft Graph drive ID of the remote drive behind a configured drive.
For shared-root drives this is still the sharer's real drive ID.

### Drive Root

The actual root of the backing drive in Microsoft Graph.

### Shared Root

The configured remote root item for a shared-root drive. This is below the
backing drive root and is identified by `RootItemID`.

### Shared-Root Drive

A separately configured shared folder added with `drive add`. It is mounted as
its own configured drive and synced as its own engine and DB.

### Embedded Shared-Folder Shortcut Item

A shared-folder link item that appears inside another drive's delta stream.
Sync ignores these instead of creating nested sync runtimes.

## Sync Engine

### Baseline

The authoritative durable record of successfully converged item truth for this
configured drive. It stores per-item identity plus local and remote comparison
facts used by planning.

### Remote State

The durable mirror of the latest observed remote truth for this configured
drive. It stores only the latest remote facts, not a per-row state machine.

### Primary Observation Cursor

The one persisted remote observation cursor for a configured drive. It lives in
`observation_state.cursor`.

### Full Remote Refresh

The full primary remote observation path that re-enumerates remote truth and
detects remote orphans. Its restart-safe cadence is owned by
`observation_state`.

### Scope

Reserved for shared blocking scope only, such as `block_scopes`, permission
scope, throttle scope, quota scope, or disk scope. Observation roots are not
called scopes.

## Data Architecture

### Sync Store

The per-drive SQLite database containing `baseline`, `local_state`,
`remote_state`, `observation_issues`, `retry_work`, `block_scopes`,
and `observation_state`.

### Observation Issues

Durable current-truth/content problems that need to be shown to the user but do
not represent automatic retry scheduling.

### Retry Work

The durable ledger of exact delayed work the engine still owes later.

### Block Scopes

The durable ledger of shared blocker timing and lifecycle. Concrete blocked
work still lives in `retry_work`.

### Sync Status

The typed singleton status row used by `status` output. It stores the last
successful bidirectional sync batch time, duration, success/failure counts,
and last error.

### Session Store

File-based persistence for resumable upload sessions. Separate from the sync
store.

## CLI

### CLIContext

Per-command context containing flags, logger, and resolved config/runtime
inputs.

### Session Provider

The token-source cache keyed by token file path so multiple configured drives
that share credentials do not race refreshes.
