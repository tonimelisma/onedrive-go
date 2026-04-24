# OneDrive Glossary

Project-specific vocabulary. For standard Microsoft Graph API terms such as
`DriveItem`, delta, and facets, see Microsoft's official documentation.

## Drive Identity

### Configured Drive

One user-configured sync target. A configured drive owns exactly one config
entry. Sync compiles configured drives into standalone runtime mounts before
engine construction.

### Canonical ID

The structured config identity string for a configured drive, such as
`personal:email@example.com` or `shared:email@example.com:<driveID>:<itemID>`.

### Backing Drive ID

The Microsoft Graph drive ID of the remote drive behind a configured drive.
For standalone shared-folder drives this is still the sharer's real drive ID.

### Drive Root

The actual root of the backing drive in Microsoft Graph.

### Mount Root

The configured remote root item for a standalone mount. This can be below the
backing drive root and is identified by `RemoteRootItemID`.

### Standalone Shared-Folder Drive

A separately configured shared folder added with `drive add`. It remains an
explicit configured drive at the CLI/config edge, then compiles into its own
standalone runtime mount with its own engine and DB.

### Embedded Shared-Folder Shortcut Item

A shared-folder link item that appears inside another drive's delta stream.
Sync ignores these instead of creating nested sync runtimes.

## Sync Engine

### Baseline

The authoritative durable record of successfully converged item truth for one
mount. It stores per-item identity plus local and remote comparison facts used
by planning.

### Remote State

The durable mirror of the latest observed remote truth for one mount. It
stores only the latest remote facts, not a per-row state machine.

### Primary Observation Cursor

The one persisted remote observation cursor for a mount. It lives in
`observation_state.cursor`; `observation_state.content_drive_id` records the
remote drive for the mounted content root that owns the cursor.

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

The per-mount SQLite database containing `baseline`, `local_state`,
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
