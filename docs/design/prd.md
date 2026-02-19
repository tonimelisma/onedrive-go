# Product Requirements Document: onedrive-go

## 1. Product Vision

**onedrive-go** is a fast, safe, and well-tested command-line OneDrive client for Linux and macOS. It provides both simple file operations (like scp) and robust bidirectional synchronization with conflict tracking — designed to be the best way to manage OneDrive from a terminal.

### Identity

OneDrive-first, extensible. We solve OneDrive deeply — every API quirk, every account type (Personal, Business, SharePoint). The architecture allows future backends but we don't promise them. We are not rclone.

### Value Proposition

- **Safe**: Conservative defaults, conflict ledger, big-delete protection, dry-run, never lose user data
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

Start small. Top-level verbs, no nested subcommands (except `config`). Familiar Unix-style names. Scriptable with `--json`. Add commands based on user demand, not speculation.

### Command Reference

#### File Operations (scp-style)

Direct API calls. No sync database involved. Work regardless of whether sync is running.

```
onedrive-go ls [path]                  # List files and folders
onedrive-go get <remote> [local]       # Download file or folder
onedrive-go put <local> [remote]       # Upload file or folder
onedrive-go rm <path>                  # Delete (to recycle bin by default)
onedrive-go mkdir <path>               # Create folder
```

#### Sync

One verb. Flags control mode. Only one sync process can run at a time (SQLite lock enforces this).

```
onedrive-go sync                       # One-shot bidirectional sync, exits when done
onedrive-go sync --watch               # Continuous sync — stays running, reacts to changes
onedrive-go sync --download-only       # One-way: download remote changes only
onedrive-go sync --upload-only         # One-way: upload local changes only
onedrive-go sync --dry-run             # Preview what would happen
onedrive-go sync --watch --quiet       # What you put in a systemd unit file
```

`sync --watch` is just sync that doesn't exit. Run it interactively and you get progress output. Run it from systemd with `--quiet` and it logs to file. Same binary, same code path. There is no separate "daemon" concept.

#### Sync Status and Conflicts

Read the sync database directly. No RPC needed.

```
onedrive-go status                     # Sync state, pending changes, unresolved conflict count
onedrive-go conflicts                  # List unresolved conflicts with details
onedrive-go resolve <id|path>          # Resolve conflicts (TBD: interactive and non-interactive modes)
```

`resolve` is TBD — it may support interactive resolution (prompting per conflict), non-interactive batch resolution (e.g. `--accept-remote`, `--accept-local`), or both. Design will be finalized in the sync algorithm spec.

#### Daemon Control (post-MVP, requires RPC)

These verbs talk to a running `sync --watch` process over a control socket. They ship when RPC ships. The same RPC API serves both CLI and GUI frontends.

```
onedrive-go pause [duration]           # Pause the running sync (2h / 4h / 8h / indefinitely)
onedrive-go resume                     # Resume a paused sync
```

When paused, the process stays alive and continues tracking changes (delta API, filesystem events) but does not transfer data. On resume, it has a complete picture of what changed and syncs efficiently.

#### Account Management

```
onedrive-go login                      # Authenticate (auto-detects best method)
onedrive-go login --headless           # Force headless auth (no browser needed)
onedrive-go logout                     # Clear credentials
```

Authentication auto-detects the environment:
- **Interactive with browser available**: Starts a temporary localhost HTTP server, opens the system browser to Microsoft OAuth, catches the redirect token automatically. Zero copy-paste.
- **Headless / SSH / no browser**: Falls back to device code flow (displays a URL and code to enter on any device). Forced with `--headless`.

Exact auth flow design is TBD and will be finalized post-MVP. The existing device code flow works for MVP; localhost redirect is a UX improvement to add later.

#### Configuration

```
onedrive-go config init                # Interactive setup wizard
onedrive-go config show                # Display current configuration
```

#### Migration

Auto-detects which tool to migrate from. Also detects if abraunegg or rclone is currently running/configured and warns about conflicts.

```
onedrive-go migrate                    # Auto-detect and migrate from abraunegg or rclone
onedrive-go migrate --from abraunegg   # Explicitly migrate from abraunegg/onedrive
onedrive-go migrate --from rclone      # Explicitly migrate from rclone OneDrive remote
```

During `config init`, the wizard also checks for existing abraunegg/rclone configurations and offers to import them. It detects existing ignore/filter files (abraunegg's `sync_list`, rclone's filter config) in the OneDrive directory and offers to convert them to `.odignore` / config patterns.

#### Future Commands (added on demand)

These are not in the MVP. They may be added later based on user requests:

```
cp, mv                                # Server-side copy/move
find, du, stat, cat, share            # Utility commands
whoami                                # Show current user and account type
config edit, config validate           # Config management
```

### Global Flags

```
--profile <name>       # Select account profile (default: "default")
--config <path>        # Override config file location
--json                 # Machine-readable JSON output
--verbose / -v         # Verbose output (stackable: -vv for debug)
--quiet / -q           # Suppress non-error output
--dry-run              # Preview operations without executing
```

---

## 5. Process Model

### MVP: SQLite Lock, No RPC

The sync database (SQLite) enforces single-writer exclusivity. Only one sync process runs at a time.

| Command type | Another sync running? | What happens |
|---|---|---|
| File ops (`ls`, `get`, `put`, `rm`, `mkdir`) | Doesn't matter | Direct API call, no DB involved |
| `sync` (one-shot or `--watch`) | No | Open DB, do work |
| `sync` | Yes | Error: database is locked |
| `status`, `conflicts`, `resolve` | Doesn't matter | Read DB directly (SQLite supports concurrent readers) |

There is no RPC, no control socket, no daemon concept at MVP. `sync --watch` is just sync that keeps running. If you want to stop it, Ctrl-C or `systemctl stop`.

### Post-MVP: RPC for GUI and Daemon Control

When RPC is added, `sync --watch` exposes a JSON-over-HTTP API on a Unix domain socket (same pattern as Docker, Tailscale, Syncthing). This enables:

- `pause` / `resume` CLI commands
- GUI frontends (real-time status, pause/resume, conflict resolution)
- `sync` while `--watch` is running could delegate instead of erroring

Two access patterns, same socket:

- **Polling**: `GET /status` returns JSON and closes. Simple scripts and status bar widgets poll this as often as they want.
- **Push**: `GET /events` is an SSE (Server-Sent Events) stream. The connection stays open and the daemon pushes events (transfer progress, sync complete, conflict detected, paused/resumed) the instant they happen. GUIs use this for real-time updates.

The RPC API serves CLI and GUI identically — same socket, same endpoints, same capabilities. Protocol details (SSE vs alternatives) finalized in the architecture spec. This is a deliberate improvement over abraunegg, where OneDriveGUI must parse stdout with regex.

---

## 6. Account Types

### MVP Scope

All three OneDrive account types supported from day one:

- **OneDrive Personal**: Consumer Microsoft accounts
- **OneDrive Business**: Microsoft 365 / Azure AD work accounts
- **SharePoint Document Libraries**: Via drive ID targeting

### Multi-Account Support

A single config file holds multiple named profiles. A single `sync --watch` process can sync all profiles simultaneously.

```toml
[profile.personal]
account_type = "personal"
sync_dir = "~/OneDrive"
remote_path = "/"

[profile.work]
account_type = "business"
sync_dir = "~/OneDrive-Work"
remote_path = "/"

[profile.sharepoint-docs]
account_type = "sharepoint"
drive_id = "b!abc123..."
sync_dir = "~/SharePoint-Docs"
remote_path = "/Shared Documents"
```

CLI usage:

```
onedrive-go sync --profile work
onedrive-go ls /Documents --profile personal
onedrive-go sync --watch                 # syncs all profiles
```

---

## 7. Sync Modes

All sync modes are flags on the `sync` verb. No separate verbs for direction or continuity.

### Bidirectional Sync (default)

`onedrive-go sync` — full two-way synchronization. Changes on either side are detected and propagated. Conflicts are detected and tracked in the conflict ledger.

### Download-Only

`onedrive-go sync --download-only` — downloads remote changes to local. Local changes are ignored. Useful for read-only mirrors, backups, or shared resource consumption.

Options:
- `--cleanup-local`: Delete local files that were deleted remotely (off by default — archive mode)

### Upload-Only

`onedrive-go sync --upload-only` — uploads local changes to remote. Remote changes are ignored. Useful for backup, publishing, or one-way deployment.

Options:
- `--no-remote-delete`: Don't propagate local deletions to remote (off by default)

### One-Shot vs. Continuous

- **One-shot** (default): `onedrive-go sync` runs once and exits.
- **Continuous**: `onedrive-go sync --watch` keeps running, detecting changes via:
  - Local: inotify (Linux) / FSEvents (macOS) for near-instant local change detection
  - Remote: WebSocket subscription for near-instant remote change detection
  - Fallback: Configurable polling interval (default 5 minutes) when WebSocket unavailable

`--watch` combines with direction flags: `sync --watch --download-only` is a continuous one-way mirror.

### Pause / Resume (post-MVP, requires RPC)

When paused via `onedrive-go pause [duration]`:
- The process stays alive
- Filesystem events continue to be collected
- Delta API continues to be polled (at a reduced rate)
- No data transfers occur
- On resume, the process has a complete picture of all changes and syncs efficiently

Pause durations: `2h`, `4h`, `8h`, or indefinitely (until `onedrive-go resume`).

### Dry-Run Mode

`onedrive-go sync --dry-run` previews all operations without executing them. Available for all sync modes. MVP must-have.

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
3. The conflict is recorded in the conflict ledger (part of the sync database)
4. The conflict ledger tracks: file path, conflict timestamp, local hash, remote hash, resolution status

### Conflict Ledger

Unlike other tools that create conflict files and forget about them, onedrive-go tracks every conflict and ensures the user addresses them:

- `onedrive-go conflicts` lists all unresolved conflicts
- `onedrive-go resolve <id|path>` resolves conflicts (TBD: may support interactive per-conflict prompting, non-interactive batch resolution via `--accept-remote`/`--accept-local`, or both — design finalized in sync algorithm spec)
- `sync --watch` periodically reminds the user of unresolved conflicts (configurable)
- The `status` output always shows the unresolved conflict count

This is a differentiating feature. No competing tool does this well.

---

## 9. Filtering

### Config-Based Filtering

Global filtering rules in the TOML config file:

```toml
[filter]
skip_dotfiles = false                    # Skip .hidden files and folders
skip_symlinks = false                    # Skip symbolic links (default: follow them)
max_file_size = "50GB"                   # Skip files larger than this
skip_files = ["~*", "*.tmp", "*.partial", "*.crdownload", ".DS_Store", "Thumbs.db"]
skip_dirs = ["node_modules", ".git", "__pycache__", ".Trash-*"]

# Per-profile overrides
[profile.work.filter]
skip_dirs = ["node_modules", ".git", "vendor"]
```

### Inclusion Lists (Selective Sync)

For syncing only specific directories:

```toml
[profile.personal]
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
[filter]
ignore_marker = ".odignore"    # default
```

### Symlink Handling

- **Default**: Follow symlinks (sync the target as a regular file/folder)
- **Option**: `skip_symlinks = true` to ignore all symlinks
- Circular symlink detection built in (track visited inodes)

---

## 10. Transfers

### Parallel Transfers

- Configurable parallelism (default: 8 simultaneous transfers)
- Separate worker pools for uploads, downloads, and checking (like rclone)
- Transfer count configurable independently:

```toml
[transfers]
parallel_downloads = 8
parallel_uploads = 8
parallel_checkers = 8           # Hash comparison workers
```

### Resumable Transfers

- Files > 4MB use resumable upload sessions (SDK support)
- Download resumption via HTTP Range requests (SDK support)
- Upload session state persisted to disk — survives process restart
- Interrupted transfers resume automatically on next sync

### Bandwidth Limiting

Global bandwidth limit with time-of-day scheduling:

```toml
[transfers]
bandwidth_limit = "0"                           # No limit (default)
# Or with schedule:
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },        # Throttle during work hours
    { time = "18:00", limit = "50MB/s" },        # More bandwidth evenings
    { time = "23:00", limit = "0" },             # Unlimited overnight
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
[safety]
big_delete_threshold = 1000     # Abort if deleting more than this many items
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
[safety]
use_recycle_bin = true          # Remote: use OneDrive recycle bin (default)
use_local_trash = true          # Local: use OS trash for remote-triggered deletes (default)
```

### Disk Space Reservation

Reserve minimum free disk space before downloading:

```toml
[safety]
min_free_space = "1GB"          # Don't download if less than this free
```

### Crash Recovery

The sync database uses SQLite with WAL mode and FULL synchronous writes. Every operation is transactional. If the process is killed mid-sync, the next run picks up cleanly from the last checkpoint.

---

## 12. Configuration

### Format

TOML. Human-readable, supports comments, hierarchical. File location:

- Linux: `~/.config/onedrive-go/config.toml`
- macOS: `~/Library/Application Support/onedrive-go/config.toml`
- Override: `ONEDRIVE_GO_CONFIG` environment variable or `--config` flag

### Interactive Setup

`onedrive-go config init` runs an interactive wizard:

1. Authenticate with Microsoft (device code flow)
2. Select account type (Personal / Business / SharePoint)
3. Choose or create local sync directory
4. Set basic filtering preferences
5. **Auto-detect existing tools**: check for abraunegg config (`~/.config/onedrive/`), rclone OneDrive remotes (`~/.config/rclone/rclone.conf`), and existing ignore files in the sync directory. Offer to import.
6. **Detect running instances**: warn if abraunegg or rclone is currently syncing the same OneDrive account to avoid conflicts.
7. Write config.toml with comments explaining each option

### Example Config

```toml
# onedrive-go configuration

[profile.default]
account_type = "personal"       # personal, business, sharepoint
sync_dir = "~/OneDrive"         # Local directory to sync
remote_path = "/"               # Remote path to sync from
# drive_id = ""                 # Required for SharePoint

[filter]
skip_dotfiles = false
skip_files = ["~*", "*.tmp", "*.partial", ".DS_Store", "Thumbs.db"]
skip_dirs = ["node_modules", ".git", "__pycache__"]
ignore_marker = ".odignore"
# max_file_size = "50GB"
# sync_paths = ["/Documents", "/Photos"]    # Selective sync (empty = sync all)

[transfers]
parallel_downloads = 8
parallel_uploads = 8
parallel_checkers = 8
chunk_size = "10MB"
# bandwidth_limit = "0"
# bandwidth_schedule = [
#     { time = "08:00", limit = "5MB/s" },
#     { time = "23:00", limit = "0" },
# ]

[safety]
big_delete_threshold = 1000
use_recycle_bin = true
use_local_trash = true
min_free_space = "1GB"

[sync]
poll_interval = "5m"            # Fallback polling interval
conflict_reminder_interval = "1h"
# websocket = true              # Near-real-time remote changes (default: true)

[logging]
level = "info"                  # debug, info, warn, error
file = ""                       # Log file path (empty = stderr only in interactive, auto with --quiet)
format = "text"                 # text (interactive) or json (--quiet/structured)
```

---

## 13. Service Integration

`sync --watch --quiet` is what you run as a service. It's the same binary, same code path — just continuous sync with machine-friendly output.

### Linux (systemd)

Ship a systemd unit file:

```ini
[Unit]
Description=onedrive-go continuous sync
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/onedrive-go sync --watch --quiet
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

Usage:
```
systemctl --user enable onedrive-go
systemctl --user start onedrive-go
journalctl --user-unit onedrive-go -f
```

### macOS (launchd)

Ship a launchd plist for `~/Library/LaunchAgents/`. Runs `sync --watch --quiet`.

### Docker

Alpine-based multi-arch image. Config and data as volumes:

```
docker run -d \
  -v ~/.config/onedrive-go:/config \
  -v ~/OneDrive:/data \
  onedrive-go sync --watch --quiet
```

---

## 14. Observability

### Interactive Mode

Human-readable output to stderr. Progress bars for transfers. Color-coded status indicators.

```
$ onedrive-go sync
Syncing profile "default" (OneDrive Personal)...
↓ report-v2.pdf                    2.3 MB  [===========] 100%  3.2 MB/s
↑ IMG_001.jpg                      4.1 MB  [======>    ]  62%  5.1 MB/s
! 1 conflict: Notes/meeting.md
Sync complete: 3 downloaded, 2 uploaded, 1 conflict
  Unresolved conflicts: 1 (run `onedrive-go conflicts`)
```

### Quiet / Service Mode

With `--quiet`, output switches to structured JSON logs to file (default: `~/.local/share/onedrive-go/onedrive-go.log`):

```json
{"time":"2026-02-17T10:30:00Z","level":"info","msg":"sync_complete","profile":"default","downloaded":3,"uploaded":2,"conflicts":1,"duration":"12.4s"}
```

### Future: TUI (post-1.0)

Interactive terminal UI (like lazygit/lazydocker) showing:
- Real-time sync status across all profiles
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
- `sync_dir` -> `profile.default.sync_dir`
- `skip_dir`, `skip_file` patterns -> `filter.skip_dirs`, `filter.skip_files`
- `skip_dotfiles` -> `filter.skip_dotfiles`
- `download_only` / `upload_only` -> noted in output (these are CLI flags, not config)
- `rate_limit` -> `transfers.bandwidth_limit`
- `threads` -> `transfers.parallel_downloads` / `transfers.parallel_uploads`
- `monitor_interval` -> `sync.poll_interval`
- `sync_list` -> `filter.sync_paths` + `filter.skip_*` (best-effort conversion)
- `classify_as_big_delete` -> `safety.big_delete_threshold`

### From rclone

**What it migrates:**
- Remote name -> profile name
- `drive_id` -> `profile.<name>.drive_id`
- `drive_type` -> `profile.<name>.account_type`
- Token -> NOT migrated (different OAuth application ID; must re-authenticate)
- rclone filter rules -> `filter.*` (best-effort conversion)

### Common to Both

- Authentication tokens -> NOT migrated (must re-authenticate with `onedrive-go login`)
- Sync state database -> NOT migrated (fresh initial sync required)
- Generated `config.toml` includes comments noting where each value came from
- Warnings for source options that have no equivalent
- Instructions for completing the migration

---

## 17. Competitive Positioning

### vs. rclone

| Aspect | rclone | onedrive-go |
|--------|--------|-------------|
| **Scope** | 70+ backends, jack-of-all-trades | OneDrive specialist |
| **Bidirectional sync** | bisync (experimental, alpha-quality) | First-class, production-grade |
| **Conflict handling** | 7 configurable strategies, no tracking | Keep-both + conflict ledger with resolution tracking |
| **Shared folders** | Not supported for OneDrive | Supported (post-MVP) |
| **OneDrive API quirks** | Minimal workarounds | All 12+ known quirks handled |
| **Real-time sync** | No daemon, no filesystem watching | `sync --watch` with WebSocket + inotify/FSEvents |
| **Setup** | Complex config for OneDrive (client ID, etc.) | Interactive wizard, just works |
| **Delta queries** | Only from drive root, no folder-scoped delta | Full delta support with token management |
| **Multi-account** | Separate remotes in config | Single config, single process, multiple profiles |
| **GUI integration** | HTTP API (rclone rc) | Control socket API (post-MVP, purpose-built) |
| **FUSE mount** | Excellent (`rclone mount`) | Planned post-MVP |
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
| **Config changes** | Require destructive `--resync` | Hot-reload where possible, graceful re-scan otherwise |
| **Filtering** | 7 layers with confusing interactions | Layered but predictable: config -> sync_paths -> .odignore markers |
| **Conflict handling** | Creates backup files, forgets about them | Conflict ledger with resolution tracking |
| **Multi-account** | Separate process per account | Single `sync --watch`, multiple profiles |
| **CLI design** | Monolithic: flags control everything | Unix-style verbs: `ls`, `get`, `put`, `sync` |
| **GUI integration** | stdout parsing (fragile, one-way) | Control socket API (post-MVP, structured, bidirectional) |
| **Pause/resume** | Not supported | Built-in (post-MVP, works from CLI and GUI) |
| **Setup** | Manual config file creation | Interactive wizard with auto-migration |
| **Real-time remote** | WebSocket (v2.5.8+) | WebSocket from day one |
| **Shared folders** | Supported (complex, fragile) | Post-MVP, designed to be robust |
| **Dry-run** | Supported | Supported (MVP) |
| **Safety** | Big-delete, .nosync, recycle bin | Big-delete, .odignore, recycle bin, conflict ledger, disk space reservation |
| **Packaging** | deb/rpm/AUR/Docker/Homebrew | Same targets, plus static binary |
| **Testing** | Manual testing, limited CI | Automated E2E tests against live OneDrive |
| **Dependencies** | D runtime, libcurl, libsqlite3 | Zero runtime dependencies (static Go binary) |
| **Platforms** | Linux, FreeBSD (macOS broken) | Linux, macOS (first-class) |

**When to use abraunegg instead:** If you need shared folder sync today, or are on a platform we don't support yet.

**When to use onedrive-go instead:** If you want lower resource usage, better conflict handling, a cleaner CLI, easier setup, or macOS support.

---

## 18. Milestones

### MVP (v0.1)

The minimum to be useful. Start small, add based on demand.

**CLI commands (13 verbs):**
- [ ] `sync` [--watch] [--download-only] [--upload-only] [--dry-run]
- [ ] `status`, `conflicts`, `resolve`
- [ ] `ls`, `get`, `put`, `rm`, `mkdir`
- [ ] `login`, `logout`
- [ ] `config` (init, show)
- [ ] `migrate` (from abraunegg + rclone, with auto-detection)

**Sync engine:**
- [ ] SQLite sync database with crash recovery (WAL mode)
- [ ] Bidirectional sync engine (delta-based)
- [ ] One-shot and continuous (`--watch`) modes
- [ ] Download-only and upload-only modes
- [ ] Local filesystem monitoring (inotify / FSEvents)
- [ ] Remote change detection (WebSocket + polling fallback)
- [ ] Conflict detection with conflict ledger
- [ ] Filtering: config-based patterns + .odignore marker files + selective sync paths
- [ ] Parallel transfers (configurable, default 8)
- [ ] Resumable uploads and downloads
- [ ] Dry-run mode for all sync operations
- [ ] Big-delete protection
- [ ] QuickXorHash for content verification

**Infrastructure:**
- [ ] TOML configuration with profiles
- [ ] Personal + Business + SharePoint account support
- [ ] All known API quirk workarounds
- [ ] Structured logging (text interactive, JSON with --quiet)
- [ ] E2E test suite against live OneDrive
- [ ] systemd unit file + launchd plist
- [ ] Docker image

### v0.2: Polish

- [ ] RPC: control socket (Unix domain socket, JSON-over-HTTP)
- [ ] `pause` / `resume` commands (requires RPC)
- [ ] `sync` delegates to running `--watch` via RPC (instead of SQLite lock error)
- [ ] Bandwidth limiting with time-of-day scheduling
- [ ] Disk space reservation
- [ ] OS trash integration (FreeDesktop.org, macOS Trash)
- [ ] Conflict reminder notifications in `--watch` mode
- [ ] `--json` output for all commands

### v1.0: Production

- [ ] Shared folder sync (Personal + Business)
- [ ] FUSE mount (read-only initially, then read-write)
- [ ] TUI interface
- [ ] Prometheus metrics endpoint
- [ ] Packaging: deb, rpm, AUR, Homebrew formula

### Someday / Maybe

- [ ] `cp`, `mv`, `find`, `du`, `stat`, `cat`, `share`, `whoami` commands
- [ ] On-demand files (FUSE with lazy download)
- [ ] Desktop notifications (libnotify / macOS Notification Center)
- [ ] File manager integration (Nautilus, Dolphin sidebar)
- [ ] National cloud support (US Gov, Germany, China)

---

## 19. Explicit Non-Goals

These are things we deliberately will NOT build:

1. **Multi-cloud backends**: We are not rclone. OneDrive only. The architecture may be extensible but we do not promise or plan other backends.
2. **GUI application**: We are a CLI tool. GUI frontends can connect to the control socket API (post-MVP), but we don't ship a GUI.
3. **Client-side encryption**: We don't encrypt files before uploading. Use rclone crypt, Cryptomator, or OS-level encryption (LUKS, FileVault).
4. **Mobile platforms**: No Android, no iOS. Linux and macOS desktop/server only.
5. **Windows**: Microsoft ships a native OneDrive client for Windows. We don't compete there.
6. **Email/SMS notifications**: Use external monitoring tools that consume our structured logs or control API events.
7. **Web UI / dashboard**: CLI and TUI only. Use Grafana + Prometheus metrics if you want dashboards.

---

## 20. Non-Functional Requirements

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
