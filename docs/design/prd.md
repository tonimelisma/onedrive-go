# Product Requirements Document: onedrive-go

## 1. Product Vision

**onedrive-go** is a fast, safe, and well-tested command-line OneDrive client for Linux and macOS. It provides both simple file operations (like scp) and robust bidirectional synchronization with conflict tracking — designed to be the best way to manage OneDrive from a terminal.

### Identity

OneDrive-first, extensible. We solve OneDrive deeply — every API quirk, every account type (Personal, Business, SharePoint). The architecture allows future backends but we don't promise them. We are not rclone.

### Value Proposition

- **Safe**: Conservative defaults, conflict tracking, big-delete protection, dry-run, never lose user data
- **Fast**: Parallel transfers, parallel checking, efficient delta processing, <100MB memory for 100K files
- **Tested**: Comprehensive automated E2E tests against live OneDrive, not just unit tests against mocks

### License

MIT

---

## 2. Target Audience

- Developers and sysadmins who work from the terminal
- Linux server/NAS operators who need OneDrive sync without a GUI
- macOS power users who want scriptable OneDrive access
- Teams using OneDrive Business / SharePoint who need automated file management
- Users migrating from abraunegg/onedrive or rclone who want something purpose-built

---

## 3. Target Platforms

- **Linux** (primary): x86_64, ARM64. Filesystem monitoring via inotify.
- **macOS** (primary): x86_64 (Intel), ARM64 (Apple Silicon). Filesystem monitoring via FSEvents.
- **FreeBSD**: Best-effort, not a primary target. kqueue for filesystem monitoring.
- **Windows**: Explicit non-goal. Microsoft ships a native OneDrive client.

### Packaging Targets

- Static Go binary (single file, no dependencies)
- Homebrew formula (macOS)
- Debian/Ubuntu .deb package (apt)
- Fedora/RHEL .rpm package
- Arch Linux AUR
- Docker image (Alpine-based, multi-arch)
- systemd unit file (Linux)
- launchd plist (macOS)

---

## 4. CLI Design

### Design Principles

Start small. Top-level verbs, no nested subcommands (except `drive` and `service`). Familiar Unix-style names. Scriptable with `--json`. Add commands based on user demand, not speculation.

Two separate flags for two separate concepts: `--account` for authentication commands (identifies a Microsoft account by email), `--drive` for everything else (identifies a drive by canonical ID, display name, or substring match). See [accounts.md §7](accounts.md) for full details.

### Command Reference

#### File Operations (scp-style)

Direct API calls. No sync database involved. Work regardless of whether sync is running. Work on all drives including disabled ones (tokens are still valid).

```
onedrive-go ls [path]                  # List remote files
onedrive-go get <remote> [local]       # Download file or folder
onedrive-go put <local> [remote]       # Upload file or folder
onedrive-go rm <path>                  # Delete (to recycle bin by default)
onedrive-go mkdir <path>               # Create folder
onedrive-go stat <path>                # Show file/folder metadata
```

#### Sync

One verb. Flags control mode. Only one sync process can run at a time (SQLite lock enforces this).

```
onedrive-go sync                       # One-shot bidirectional sync, exits when done
onedrive-go sync --watch               # Continuous sync — stays running, re-syncs on interval
onedrive-go sync --download-only       # One-way: download remote changes only
onedrive-go sync --upload-only         # One-way: upload local changes only
onedrive-go sync --dry-run             # Preview what would happen
onedrive-go sync --watch --quiet       # What you put in a systemd unit file
```

`sync --watch` is just sync that doesn't exit. Run it interactively and you get progress output. Run it from systemd with `--quiet` and it logs to file. Same binary, same code path. There is no separate "daemon" concept.

`sync --watch` reloads config.toml on SIGHUP. CLI commands write config and send SIGHUP to the daemon via PID file. Drives added/removed/paused while running take effect immediately. It idles gracefully with no drives — can be installed as a service before any login.

#### Sync Status and Conflicts

```
onedrive-go status                     # Show all accounts, drives, and sync state
onedrive-go conflicts                  # List unresolved conflicts with details
onedrive-go resolve <id|path>          # Resolve conflicts (interactive prompting or batch flags)
onedrive-go verify                     # Re-hash local files, compare to DB and remote
```

`status` shows an account/drive hierarchy with token status and per-drive sync state. See [accounts.md §12](accounts.md).

`resolve` supports two modes: **interactive** (prompts per conflict with `[L]ocal / [R]emote / [B]oth / [S]kip / [Q]uit`) and **batch** (flag-driven: `--keep-local`, `--keep-remote`, `--keep-both`, `--all`, `--dry-run`). Interactive mode is the default when no batch flags are passed. See [sync-algorithm.md §7.4](sync-algorithm.md) for full details.

#### Daemon Control (requires RPC)

These verbs talk to a running `sync --watch` process over a control socket. They ship when RPC ships. The same RPC API serves both CLI and GUI frontends.

```
onedrive-go pause [duration]           # Pause the running sync (2h / 4h / 8h / indefinitely)
onedrive-go resume                     # Resume a paused sync
```

When paused, the process stays alive and continues tracking changes (delta API, filesystem events) but does not transfer data. On resume, it has a complete picture of what changed and syncs efficiently.

#### Account Management

```
onedrive-go login [--browser]          # Sign in + auto-add primary drive (device code by default)
onedrive-go logout [--purge]           # Sign out (--purge: also delete state DBs + config sections)
onedrive-go whoami                     # Show authenticated accounts
```

Authentication defaults to device code flow (works everywhere — headless, SSH, containers). The `--browser` flag switches to authorization code flow with a localhost callback (opens browser, fewer steps for desktop users). Both methods block until auth completes or time out.

`login` auto-detects account type (personal vs business) and auto-adds the primary drive with sensible defaults. No interactive config prompts — just authenticate and go. Business logins mention SharePoint availability and suggest `drive add`.

See [accounts.md §9](accounts.md) for full login flow details, including `--json` output for GUI/scripting integration.

#### Drive Management

```
onedrive-go drive add                  # Add a SharePoint library or resume a paused drive
onedrive-go drive remove [--purge]     # Pause a drive (--purge: delete state DB + config section)
```

`drive add` is interactive — it shows available SharePoint libraries and shared folders. For non-interactive use: `drive add --site marketing --library Documents`. It does NOT offer new account sign-in — that's what `login` is for.

`drive remove` deletes the config section. State DB preserved for fast re-add (delta sync resumes from where it left off). `--purge` also deletes state DB. Token kept if shared with other drives. To temporarily stop syncing without removing, use `pause`/`resume`.

`drive list` shows `(read-only)` or `(read-write)` permission annotations for shared content (DP-10), giving users proactive visibility into access levels before sync encounters 403 errors.

See [accounts.md §10](accounts.md) for details.

#### Configuration

```
onedrive-go setup                      # Interactive guided configuration (menu-driven)
```

`setup` is the one interactive command for configuration. It covers: viewing drives/settings, changing sync directories, configuring exclusions, setting sync interval and log level, per-drive overrides, and display names. Everything `setup` does can also be done by editing `config.toml` directly. Power users edit the file.

There is no `config show` command. Users read the config file directly. `status` shows runtime state. `--debug` shows config resolution at startup.

#### Service Management

```
onedrive-go service install            # Generate and install systemd/launchd service file (does NOT enable)
onedrive-go service uninstall          # Remove the installed service file
onedrive-go service status             # Show whether service is installed, enabled, running
```

`service install` writes the appropriate service file for the platform and prints native commands to enable/disable. Never auto-enables. See [accounts.md §13](accounts.md).

#### Migration

Auto-detects which tool to migrate from. Also detects if abraunegg or rclone is currently running/configured and warns about conflicts.

```
onedrive-go migrate                    # Auto-detect and migrate from abraunegg or rclone
onedrive-go migrate --from abraunegg   # Explicitly migrate from abraunegg/onedrive
onedrive-go migrate --from rclone      # Explicitly migrate from rclone OneDrive remote
```

#### Future Commands (added on demand)

These may be added later based on user demand:

```
cp, mv                                # Server-side copy/move
find, du, cat, share                   # Utility commands
```

### Global Flags

```
--account <email>      # Select account by email (auth commands: login, logout)
--drive <id|name>      # Select drive by canonical ID, display name, or substring match (repeatable for sync/status)
--config <path>        # Override config file location
--json                 # Machine-readable JSON output
--verbose / -v         # Show individual file operations
--debug                # Show HTTP requests, internal state, config resolution
--quiet / -q           # Errors only
--dry-run              # Preview operations without executing
```

`--drive` uses shortest unique partial matching. Error messages show all available drives with their shortest unique identifiers. See [accounts.md §7](accounts.md) for full matching semantics.

### Sync-Specific Flags

```
--watch                # Continuous sync — stay running, re-sync on poll_interval
--download-only        # One-way: download remote changes only
--upload-only          # One-way: upload local changes only
```

### Login-Specific Flags

```
--browser              # Use authorization code flow (opens browser, localhost callback)
```

---

## 5. Process Model

### Default: SQLite Lock, No RPC

The sync database (SQLite) enforces single-writer exclusivity. Only one sync process runs at a time.

| Command type | Another sync running? | What happens |
|---|---|---|
| File ops (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`) | Doesn't matter | Direct API call, no DB involved |
| `sync` (one-shot or `--watch`) | No | Open DB, do work |
| `sync` | Yes | Error: database is locked |
| `status`, `conflicts`, `resolve`, `verify` | Doesn't matter | Read DB directly (SQLite supports concurrent readers) |

Without RPC, there is no control socket and no daemon concept. `sync --watch` is just sync that keeps running. If you want to stop it, Ctrl-C or `systemctl stop`.

### RPC for GUI and Daemon Control

When RPC is added, `sync --watch` exposes a JSON-over-HTTP API on a Unix domain socket (same pattern as Docker, Tailscale, Syncthing). This enables:

- `pause` / `resume` CLI commands
- GUI frontends (real-time status, pause/resume, conflict resolution)
- `sync` while `--watch` is running could delegate instead of erroring
- Status queries, force-sync, pause/resume

Two access patterns, same socket:

- **Polling**: `GET /status` returns JSON and closes. Simple scripts and status bar widgets poll this as often as they want.
- **Push**: `GET /events` is an SSE (Server-Sent Events) stream. The connection stays open and the daemon pushes events (transfer progress, sync complete, conflict detected, paused/resumed) the instant they happen. GUIs use this for real-time updates.

The RPC API serves CLI and GUI identically — same socket, same endpoints, same capabilities. Login is always done via the CLI, not via RPC — login requires user interaction. If a token expires while the service is running, it logs an error and tells the user to run `onedrive-go login` in a terminal.

---

## 6. Drive Types

All four drive types are supported:

- **OneDrive Personal**: Consumer Microsoft accounts
- **OneDrive Business**: Microsoft 365 / Azure AD work accounts
- **SharePoint Document Libraries**: Via drive management (one business login grants access to all)
- **Shared Folders**: Folders shared by other users, synced as separate drives (reuse primary account token)

### Multi-Account Support

A single config file holds multiple drive sections. A single `sync --watch` process syncs all non-paused drives simultaneously (each drive in its own goroutine with its own state DB). The daemon reloads `config.toml` on SIGHUP for immediate config pickup.

```toml
# ── Global settings ──
log_level = "info"

# ── Drives ──
# Filter settings (skip_dotfiles, skip_dirs, etc.) are per-drive only (DP-8)

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
skip_dotfiles = true

["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
skip_dirs = ["node_modules", ".git", "vendor"]

["sharepoint:alice@contoso.com:marketing:Documents"]
sync_dir = "~/Contoso/Marketing - Documents"
paused = true
```

CLI usage:

```
onedrive-go sync --drive "me@contoso.com"  # sync one drive (by display name)
onedrive-go ls /Documents --drive personal
onedrive-go sync --watch               # syncs all non-paused drives
```

SharePoint drives share the business account's OAuth token — same user, same session, same scopes. Only the state DB is per-drive.

---

## 7. Sync Modes

All sync modes are flags on the `sync` verb. No separate verbs for direction or continuity.

### Bidirectional Sync (default)

`onedrive-go sync` — full two-way synchronization. Changes on either side are detected and propagated. Conflicts are detected and tracked in the conflict tracking.

### Download-Only

`onedrive-go sync --download-only` — downloads remote changes to local. Local changes are ignored. Useful for read-only mirrors, backups, or shared resource consumption.

### Upload-Only

`onedrive-go sync --upload-only` — uploads local changes to remote. Remote changes are ignored. Useful for backup, publishing, or one-way deployment.

### One-Shot vs. Continuous

- **One-shot** (default): `onedrive-go sync` runs once and exits.
- **Continuous**: `onedrive-go sync --watch` keeps running, detecting changes via:
  - Local: inotify (Linux) / FSEvents (macOS) for near-instant local change detection
  - Remote: WebSocket subscription for near-instant remote change detection
  - Fallback: Configurable polling interval (default 5 minutes) when WebSocket unavailable

`--watch` combines with direction flags: `sync --watch --download-only` is a continuous one-way mirror.

### Pause / Resume (requires RPC)

When paused via `onedrive-go pause [duration]`:
- The process stays alive
- Filesystem events continue to be collected
- Delta API continues to be polled (at a reduced rate)
- No data transfers occur
- On resume, the process has a complete picture of all changes and syncs efficiently

Pause durations: `2h`, `4h`, `8h`, or indefinitely (until `onedrive-go resume`).

### Dry-Run Mode

`onedrive-go sync --dry-run` previews all operations without executing them. Available for all sync modes.

---

## 8. Conflict Handling

### Detection

A conflict exists when the same file has been modified on both the local filesystem and on OneDrive since the last successful sync. Detection uses:

1. Content hash comparison (QuickXorHash) against the stored state in the sync database
2. ETag/cTag comparison for metadata changes
3. Timestamp comparison as a secondary signal

### Resolution Strategy

**Default: keep both, track the conflict.**

1. The remote version is downloaded with the original filename
2. The local version is renamed to `<filename>.conflict-<YYYYMMDD-HHMMSS>.<ext>`
3. The conflict is recorded in the conflicts table (part of the sync database)
4. The conflicts table tracks: file path, conflict timestamp, local hash, remote hash, resolution status

### Conflict Tracking

Unlike other tools that create conflict files and forget about them, onedrive-go tracks every conflict and ensures the user addresses them:

- `onedrive-go conflicts` lists all unresolved conflicts
- `onedrive-go resolve <id|path>` resolves conflicts interactively (prompts per conflict) or in batch mode (`--keep-local`, `--keep-remote`, `--keep-both`, `--all`, `--dry-run`)
- `sync --watch` periodically reminds the user of unresolved conflicts (configurable)
- The `status` output always shows the unresolved conflict count

This is a differentiating feature. No competing tool does this well.

---

## 9. Filtering

### Config-Based Filtering

All filter settings are **per-drive only** — there are no global filter defaults (DP-8). Each drive gets built-in defaults (empty lists, `false`) unless it specifies its own. This prevents confusing inheritance where a global setting unexpectedly affects unrelated drives.

```toml
# Filter settings live inside each drive section
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
# No filter settings → syncs everything (built-in exclusions still apply)

["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
skip_dotfiles = true
skip_dirs = ["node_modules", ".git", "vendor"]
skip_files = ["~*", "*.tmp", "*.partial", ".DS_Store", "Thumbs.db"]
ignore_marker = ".odignore"
max_file_size = "50GB"
```

### Inclusion Lists (Selective Sync)

For syncing only specific directories, set `sync_paths` in a drive section:

```toml
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
sync_paths = ["/Documents", "/Photos/Camera Roll", "/Work"]
```

If `sync_paths` is set, only those paths (and their children) are synced. Everything else is ignored.

### Per-Directory Marker Files

Drop a configurable marker file (default: `.odignore`) in any directory to exclude it from sync. The marker file can optionally contain gitignore-style patterns for fine-grained control:

```
# .odignore — exclude this entire directory
*
```

```
# .odignore — exclude specific patterns within this directory
*.log
build/
dist/
```

The marker filename is configurable:

```toml
ignore_marker = ".odignore"
```

### Symlink Handling

- **Default**: Follow symlinks (sync the target as a regular file/folder)
- **Option**: `skip_symlinks = true` to ignore all symlinks
- Circular symlink detection built in (track visited inodes)

---

## 10. Transfers

### Parallel Transfers

- Configurable parallelism (default: `runtime.NumCPU()`, minimum 4 workers)
- Lane-based worker pools: interactive (small files, folder ops), bulk (large transfers), shared overflow
- Reserved workers per lane with shared pool preferring interactive for low-latency metadata operations
- Separate checker pool for local hash computation (CPU-bound, runs during observation)

```toml
parallel_downloads = 8  # total lane workers (interactive + bulk + shared)
parallel_uploads = 8    # total lane workers (shared across download/upload)
parallel_checkers = 8   # separate pool for hash computation
```

### Resumable Transfers

- Files > 4MB use resumable upload sessions (SDK support)
- Download resumption via HTTP Range requests (SDK support)
- Upload session state persisted to disk — survives process restart
- Interrupted transfers resume automatically on next sync

### Bandwidth Limiting

Global bandwidth limit with time-of-day scheduling:

```toml
bandwidth_limit = "0"                           # No limit (default)
# Or with schedule:
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },
    { time = "18:00", limit = "50MB/s" },
    { time = "23:00", limit = "0" },
]
```

### Upload Size Thresholds

- Files <= 4MB: Simple PUT upload (single request)
- Files > 4MB: Resumable upload session with configurable chunk size
- Chunk size: Configurable, default 10MB, must be multiple of 320KiB (API requirement)

---

## 11. Safety Features

### Big-Delete Protection

If a sync operation would delete more than N items (default: 1000), abort and require explicit confirmation:

```
WARNING: This sync would delete 2,847 files from OneDrive.
This exceeds the big-delete threshold (1000).
Run with --force to proceed, or adjust big_delete_threshold in config.
```

```toml
big_delete_threshold = 1000
```

### Dry-Run Mode

Available on all sync operations. Shows exactly what would happen without making any changes.

```
$ onedrive-go sync --dry-run
Would download: /Documents/report-v2.pdf (2.3 MB)
Would upload:   /Photos/vacation/IMG_001.jpg (4.1 MB)
Would delete:   /Temp/scratch.txt (remote)
Would conflict: /Notes/meeting.md (modified both locally and remotely)
Summary: 1 download, 1 upload, 1 delete, 1 conflict
```

### Recycle Bin

Remote deletions go to the OneDrive recycle bin (not permanent delete) by default.

Local deletions triggered by remote changes go to the OS trash (FreeDesktop.org Trash spec on Linux, Finder Trash on macOS) by default.

```toml
use_recycle_bin = true
use_local_trash = true
```

### Disk Space Reservation

Reserve minimum free disk space before downloading:

```toml
min_free_space = "1GB"
```

### Crash Recovery

The sync database uses SQLite with WAL mode and FULL synchronous writes. Every operation is transactional. If the process is killed mid-sync, the next run picks up cleanly from the last checkpoint.

---

## 12. Configuration

### Format

TOML. Human-readable, supports comments, flat top-level keys with drive sections. File location:

- Linux: `~/.config/onedrive-go/config.toml` (or `~/.local/share/onedrive-go/config.toml`)
- macOS: `~/Library/Application Support/onedrive-go/config.toml`
- Override: `ONEDRIVE_GO_CONFIG` environment variable or `--config` flag

### Config Auto-Creation

Config is auto-created by `login`. On first login, a complete config is written from a template string with all global settings as commented-out defaults (so users discover options without reading docs). The drive section is appended. See [configuration.md](configuration.md) and [accounts.md §4](accounts.md) for details.

### Config Modification

Config is modified by `login`, `drive add`, `drive remove`, and `setup`. Modifications use line-based text edits (not TOML round-trip serialization) to preserve all comments. Manual editing is always supported and encouraged. See [accounts.md §4](accounts.md) for the text-level manipulation approach.

### Interactive Setup

`onedrive-go setup` is the interactive guided configuration command. It covers all configuration tasks: viewing drives, changing sync directories, managing exclusions, setting poll intervals, log levels, per-drive overrides, and display names. Unlike `login` (which assumes defaults and tells you what it did), `setup` is menu-driven and lets users change anything.

### Example Config

```toml
# onedrive-go configuration
# Docs: https://github.com/tonimelisma/onedrive-go

# ── Global settings ──
# Uncomment and modify to override defaults.

# log_level = "info"
# poll_interval = "5m"

# Note: filter settings (skip_dotfiles, skip_dirs, etc.) are per-drive only (DP-8)

# ── Drives ──

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"

["business:alice@contoso.com"]
display_name = "alice@contoso.com"
sync_dir = "~/OneDrive - Contoso"
skip_dirs = ["node_modules", ".git", "vendor"]
```

---

## 13. Service Integration

`sync --watch` runs in the foreground. No `--daemon` flag — modern service managers handle process lifecycle.

### Linux (systemd)

`onedrive-go service install` generates:

```ini
[Unit]
Description=onedrive-go continuous sync
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/onedrive-go sync --watch
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

Usage:
```
onedrive-go service install
systemctl --user enable --now onedrive-go
journalctl --user-unit onedrive-go -f
```

### macOS (launchd)

`onedrive-go service install` generates a launchd plist for `~/Library/LaunchAgents/`.

### Docker

Alpine-based multi-arch image. Config and data as volumes:

```
docker run -d \
  -v ~/.config/onedrive-go:/config \
  -v ~/OneDrive:/data \
  onedrive-go sync --watch --quiet
```

### Service-First Setup

The service can be installed and enabled before any login. It idles with no drives. When a drive is added via `login`, the next sync cycle picks it up automatically. No restart needed.

---

## 14. Observability

### Two Independent Logging Channels

| Channel | What controls it | Default |
|---|---|---|
| **Console** (stderr) | CLI flags (`--quiet`, `--verbose`, `--debug`) | Operational summaries |
| **Log file** | `log_level` in config | `info` level, platform-default path |

Console flags and log file level are independent. See [accounts.md §5](accounts.md).

### Interactive Mode

Human-readable output to stderr. Progress bars for transfers. Color-coded status indicators.

```
$ onedrive-go sync
Syncing personal:toni@outlook.com...
↓ report-v2.pdf                    2.3 MB  [===========] 100%  3.2 MB/s
↑ IMG_001.jpg                      4.1 MB  [======>    ]  62%  5.1 MB/s
! 1 conflict: Notes/meeting.md
Sync complete: 3 downloaded, 2 uploaded, 1 conflict
  Unresolved conflicts: 1 (run `onedrive-go conflicts`)
```

### Quiet / Service Mode

With `--quiet`, only errors reach stderr. The log file captures everything:

```json
{"time":"2026-02-17T10:30:00Z","level":"info","msg":"sync_complete","drive":"personal:toni@outlook.com","downloaded":3,"uploaded":2,"conflicts":1,"duration":"12.4s"}
```

### Future: TUI

Interactive terminal UI (like lazygit/lazydocker) showing:
- Real-time sync status across all drives
- Transfer progress
- Conflict resolution interface
- Log viewer

---

## 15. API Quirk Handling

All 12+ known Microsoft Graph API quirks are handled from day one.

### Critical (Data Integrity)

| Quirk | Handling |
|-------|----------|
| Delta API delivers deletions AFTER creations at same path | Reorder operations: process deletions before creations |
| Personal driveId truncation (16->15 chars, leading zero dropped) | Normalize all driveIds: pad to expected length, lowercase |
| DriveId casing inconsistent across endpoints | Normalize all driveIds to lowercase on receipt |
| parentReference.path never returned in delta | Track all items by ID, reconstruct paths from parent chain |
| cTag not returned for folders on Business | Use eTag + child enumeration for folder change detection |
| Deleted items may lack name (Business) or size (Personal) | Look up missing fields from local database before processing |
| Failed downloads can trigger remote deletion in naive implementations | Never delete remote based on download state: separate concerns |
| HTTP 410 on delta = token expired | Handle both resync types: full re-enumeration vs. token refresh |

### Handled with Warnings

| Quirk | Handling |
|-------|----------|
| iOS .heic files: API reports original metadata but serves modified content | Log warning, skip hash verification for known-affected MIME types |
| SharePoint "enrichment" modifies uploaded files server-side | Detect server-side modification (hash mismatch after upload), log warning, don't re-upload |
| Upload fragments must be 320KiB multiples | Enforce in upload session creation, validate chunk size config |
| Hashes invalid for deleted items | Never compare hashes for items with deleted facet |

---

## 16. Migration

### Auto-Detection

`onedrive-go migrate` scans for existing configurations:

1. **abraunegg/onedrive**: Checks `~/.config/onedrive/config` and `~/.config/onedrive/sync_list`
2. **rclone**: Checks `~/.config/rclone/rclone.conf` for OneDrive remotes (type = onedrive)
3. Detects if either tool is currently running and warns about conflicts
4. Detects existing ignore files in the OneDrive sync directory

### From abraunegg/onedrive

**What it migrates:**
- `sync_dir` -> drive section `sync_dir`
- `skip_dir`, `skip_file` patterns -> `skip_dirs`, `skip_files`
- `skip_dotfiles` -> `skip_dotfiles`
- `download_only` / `upload_only` -> noted in output (these are CLI flags, not config)
- `rate_limit` -> `bandwidth_limit`
- `threads` -> `parallel_downloads` / `parallel_uploads`
- `monitor_interval` -> `poll_interval`
- `sync_list` -> `sync_paths` (best-effort conversion)
- `classify_as_big_delete` -> `big_delete_threshold`

### From rclone

**What it migrates:**
- Remote name -> drive display_name
- `drive_id` -> drive section `drive_id`
- `drive_type` -> auto-detected drive type
- Token -> NOT migrated (different OAuth application ID; must re-authenticate)
- rclone filter rules -> `skip_files`/`skip_dirs` (best-effort conversion)

### Common to Both

- Authentication tokens -> NOT migrated (must re-authenticate with `onedrive-go login`)
- Sync state database -> NOT migrated (fresh initial sync required)
- Generated config includes comments noting where each value came from
- Warnings for source options that have no equivalent
- Instructions for completing the migration

---

## 17. Competitive Positioning

### vs. rclone

| Aspect | rclone | onedrive-go |
|--------|--------|-------------|
| **Scope** | 70+ backends, jack-of-all-trades | OneDrive specialist |
| **Bidirectional sync** | bisync (experimental, alpha-quality) | First-class, production-grade |
| **Conflict handling** | 7 configurable strategies, no tracking | Keep-both + conflict tracking with resolution tracking |
| **Shared folders** | Not supported for OneDrive | Supported |
| **OneDrive API quirks** | Minimal workarounds | All 12+ known quirks handled |
| **Real-time sync** | No daemon, no filesystem watching | `sync --watch` with WebSocket + inotify/FSEvents |
| **Setup** | Complex config for OneDrive (client ID, etc.) | `login` auto-configures, `setup` for guided config |
| **Delta queries** | Only from drive root, no folder-scoped delta | Full delta support with token management |
| **Multi-account** | Separate remotes in config | Single config, single process, multiple drives |
| **GUI integration** | HTTP API (rclone rc) | Control socket API (purpose-built) |
| **FUSE mount** | Excellent (`rclone mount`) | Planned |
| **Encryption** | Built-in crypt backend | Not a goal (use rclone crypt or OS encryption) |

**When to use rclone instead:** If you need multi-cloud support, FUSE mount today, or client-side encryption. rclone is the Swiss Army knife.

**When to use onedrive-go instead:** If OneDrive is your primary cloud storage and you want reliable bidirectional sync with conflict tracking, real-time change detection, and proper handling of OneDrive's API quirks.

### vs. abraunegg/onedrive

| Aspect | abraunegg/onedrive | onedrive-go |
|--------|-------------------|-------------|
| **Language** | D (niche ecosystem, hard to contribute) | Go (large ecosystem, easy to build/contribute) |
| **Memory usage** | ~1GB per 100K items | Target: <100MB per 100K items |
| **CPU when idle** | #1 complaint: high CPU | Target: <1% idle CPU |
| **Initial sync** | Reports of 16+ hours | Parallel delta + parallel transfers from start |
| **Config changes** | Require destructive `--resync` | Config re-read each sync cycle, changes take effect automatically |
| **Filtering** | 7 layers with confusing interactions | Layered but predictable: config -> sync_paths -> .odignore markers |
| **Conflict handling** | Creates backup files, forgets about them | Conflict tracking with resolution tracking |
| **Multi-account** | Separate process per account | Single `sync --watch`, multiple drives |
| **CLI design** | Monolithic: flags control everything | Unix-style verbs: `ls`, `get`, `put`, `sync` |
| **GUI integration** | stdout parsing (fragile, one-way) | Control socket API (structured, bidirectional) |
| **Pause/resume** | Not supported | Built-in (works from CLI and GUI) |
| **Setup** | Manual config file creation | `login` auto-configures, `setup` for guided changes, `migrate` for import |
| **Real-time remote** | WebSocket (v2.5.8+) | WebSocket from day one |
| **Shared folders** | Supported (complex, fragile) | Designed to be robust |
| **Dry-run** | Supported | Supported |
| **Safety** | Big-delete, .nosync, recycle bin | Big-delete, .odignore, recycle bin, conflict tracking, disk space reservation |
| **Packaging** | deb/rpm/AUR/Docker/Homebrew | Same targets, plus static binary |
| **Testing** | Manual testing, limited CI | Automated E2E tests against live OneDrive |
| **Dependencies** | D runtime, libcurl, libsqlite3 | Zero runtime dependencies (static Go binary) |
| **Platforms** | Linux, FreeBSD (macOS broken) | Linux, macOS (first-class) |

**When to use abraunegg instead:** If you need shared folder sync today, or are on a platform we don't support yet.

**When to use onedrive-go instead:** If you want lower resource usage, better conflict handling, a cleaner CLI, easier setup, or macOS support.

---

## 18. Explicit Non-Goals

These are things we deliberately will NOT build:

1. **Multi-cloud backends**: We are not rclone. OneDrive only. The architecture may be extensible but we do not promise or plan other backends.
2. **GUI application**: We are a CLI tool. GUI frontends can connect to the control socket API, but we don't ship a GUI.
3. **Client-side encryption**: We don't encrypt files before uploading. Use rclone crypt, Cryptomator, or OS-level encryption (LUKS, FileVault).
4. **Mobile platforms**: No Android, no iOS. Linux and macOS desktop/server only.
5. **Windows**: Microsoft ships a native OneDrive client for Windows. We don't compete there.
6. **Email/SMS notifications**: Use external monitoring tools that consume our structured logs or control API events.
7. **Web UI / dashboard**: CLI and TUI only. Use Grafana + Prometheus metrics if you want dashboards.

---

## 19. Non-Functional Requirements

| Requirement | Target | Rationale |
|-------------|--------|-----------|
| Memory (100K synced files) | < 100 MB | abraunegg uses ~1GB. 10x improvement. Critical for NAS/Raspberry Pi. |
| CPU when idle (`--watch`) | < 1% | #1 abraunegg complaint. Must be invisible. |
| Initial sync (10K files) | < 10 minutes | abraunegg reports hours. Parallel delta + parallel transfers. |
| Startup time | < 1 second | Go binary, no interpreter, minimal initialization. |
| Binary size | < 20 MB | Single static binary, no runtime dependencies. |
| Crash recovery | Zero data loss | SQLite WAL + transactional operations. Resume from last checkpoint. |
| API rate limit handling | Automatic backoff | Respect 429 + Retry-After. Never hammer the API. |
| Network interruption | Graceful resume | All transfers resumable. State preserved. Exponential backoff on reconnect. |
