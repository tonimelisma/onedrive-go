# Survey: GUI/IPC Integration Patterns for Sync Daemons

This document surveys how existing file sync tools expose control interfaces to GUI frontends, and evaluates IPC mechanisms suitable for a Go-based sync daemon. It covers OneDriveGUI's integration with abraunegg/onedrive, rclone's remote control API, Syncthing's REST API, and general IPC patterns for Go daemons.

---

## Table of Contents

1. [How OneDriveGUI Talks to abraunegg/onedrive](#1-how-onedrivegui-talks-to-abrauneggonedrive)
   - [IPC Mechanism: Subprocess + Stdout Parsing](#ipc-mechanism-subprocess--stdout-parsing)
   - [Configuration Management](#configuration-management)
   - [Desktop Notifications via D-Bus](#desktop-notifications-via-d-bus)
   - [Limitations of This Approach](#limitations-of-this-approach)
2. [How rclone's Remote Control Works](#2-how-rclones-remote-control-works)
   - [Architecture Overview](#architecture-overview)
   - [API Protocol](#api-protocol)
   - [Endpoint Categories (80+ endpoints)](#endpoint-categories-80-endpoints)
   - [Async Job System](#async-job-system)
   - [Authentication](#authentication)
   - [Web GUI Integration](#web-gui-integration)
3. [How Syncthing's API Works](#3-how-syncthings-api-works)
   - [Architecture Overview](#syncthing-architecture-overview)
   - [Endpoint Categories](#syncthing-endpoint-categories)
   - [Event System (Long Polling)](#event-system-long-polling)
   - [Authentication](#syncthing-authentication)
   - [Design Qualities](#syncthing-design-qualities)
4. [rclone's OneDrive Configuration](#4-rclones-onedrive-configuration)
   - [Config File Format](#config-file-format)
   - [Example OneDrive Section](#example-onedrive-section)
   - [Token Storage](#token-storage)
   - [Migration Considerations](#migration-considerations)
5. [abraunegg's Filter/Ignore File Formats](#5-abrauneggs-filterignore-file-formats)
   - [Config File Format](#abraunegg-config-file-format)
   - [skip_file](#skip_file)
   - [skip_dir](#skip_dir)
   - [sync_list File](#sync_list-file)
   - [Filter Evaluation Order](#filter-evaluation-order)
6. [IPC Options for Go Daemons](#6-ipc-options-for-go-daemons)
   - [Unix Domain Sockets](#unix-domain-sockets)
   - [localhost HTTP/REST API](#localhost-httprest-api)
   - [gRPC over Unix Socket](#grpc-over-unix-socket)
   - [D-Bus](#d-bus)
   - [Named Pipes / FIFOs](#named-pipes--fifos)
   - [What Go Tools Actually Do](#what-go-tools-actually-do)
7. [Recommendations for Our Daemon](#7-recommendations-for-our-daemon)

---

## 1. How OneDriveGUI Talks to abraunegg/onedrive

### IPC Mechanism: Subprocess + Stdout Parsing

OneDriveGUI (Python/PySide6) communicates with the abraunegg/onedrive daemon using the **simplest possible IPC**: it spawns the onedrive binary as a child process and parses its stdout line by line.

The core pattern, from `workers.py`:

```python
class WorkerThread(QThread):
    def __init__(self, profile, options=""):
        self._command = f"exec {client_bin_path} --confdir='{config_dir}' --monitor -v {options}"

    def run(self):
        self.onedrive_process = subprocess.Popen(
            self._command,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            shell=True,
            universal_newlines=True,
        )
        while self.onedrive_process.poll() is None:
            if self.onedrive_process.stdout:
                self.read_stdout()
```

The `read_stdout()` method reads one line at a time and pattern-matches against known strings:

| Stdout Pattern | GUI Action |
|---|---|
| `"Sync with Microsoft OneDrive is complete"` | Set status to "sync complete" |
| `"Remaining Free Space"` | Parse bytes via regex, display human-readable size |
| `"Account Type"` | Extract and display account type |
| `"Downloading file"` / `"Uploading file"` | Show transfer progress |
| `"authorise this application"` | Kill process, prompt for re-auth |
| `"--resync is required"` | Emit signal for resync dialog |
| `"To delete a large volume of data"` | Emit signal for big-delete confirmation |
| `"Error:"` lines | Parse multi-line errors, display truncated |
| `"Failed to upload:"` / `"Failed to download:"` | Collect failed file list |

The GUI controls the daemon exclusively through:
- **Starting**: `subprocess.Popen(...)` with `--monitor -v`
- **Stopping**: `self.onedrive_process.kill()`
- **No bidirectional communication**: The GUI cannot send commands to a running daemon. It can only start, stop, and observe.

### Configuration Management

OneDriveGUI manages configuration by directly reading and writing the abraunegg config files on disk:

- Profiles stored at `~/.config/onedrive-gui/profiles` (INI format via Python ConfigParser)
- Each profile points to an abraunegg config file (e.g., `~/.config/onedrive/accounts/user@live.com/config`)
- The GUI reads config files using ConfigParser with a synthetic `[onedrive]` section header prepended (since abraunegg's config is flat key=value, not INI)
- Multi-line `skip_file` and `skip_dir` entries are consolidated into pipe-separated single lines
- Config writes strip the synthetic section header before saving
- Before each save, the existing config is backed up to `config_backup`

### Desktop Notifications via D-Bus

The abraunegg daemon uses libnotify (D-Bus `org.freedesktop.Notifications`) for desktop notifications. This is one-way (daemon to desktop) and is not used for GUI integration. The notification module (`dnotify.d`) calls `notify_init()` and `notify_get_server_info()` to check D-Bus availability, then sends notifications for sync events.

OneDriveGUI does **not** use D-Bus to communicate with the daemon. D-Bus is only used by the daemon itself for desktop notification bubbles.

### Limitations of This Approach

1. **No runtime control**: Cannot pause/resume sync, change bandwidth limits, or trigger actions on a running daemon
2. **Fragile parsing**: Every change to stdout message format can break the GUI
3. **No structured data**: Progress percentages, transfer speeds, and file counts must be regex-extracted from human-readable text
4. **No event subscription**: The GUI must poll stdout continuously rather than subscribing to specific event types
5. **No status queries**: Cannot ask "what is your current state?" -- must infer from accumulated stdout messages
6. **One-way only**: The GUI can observe but cannot influence daemon behavior after launch

---

## 2. How rclone's Remote Control Works

### Architecture Overview

rclone exposes a comprehensive HTTP-based remote control API when started with `--rc` (or when running `rclone rcd` which starts a pure API daemon). The API server binds to `localhost:5572` by default.

The Go implementation lives in `fs/rc/` and `fs/rc/rcserver/`. Commands are registered via `rc.Add(path, function)` where each function takes and returns a `Params` map (JSON objects). The server is built on rclone's internal HTTP library.

### API Protocol

- **Transport**: HTTP/HTTPS on localhost (configurable address)
- **Method**: All calls use POST
- **Content-Type**: `application/json` for request body
- **Input**: JSON body, URL parameters, or form-encoded POST parameters (all three accepted)
- **Output**: JSON response object, or error
- **CORS**: Configurable `Access-Control-Allow-Origin` for browser-based GUIs

### Endpoint Categories (80+ endpoints)

| Category | Key Endpoints | Purpose |
|---|---|---|
| **sync/** | `sync/copy`, `sync/move`, `sync/sync`, `sync/bisync` | Trigger sync operations |
| **operations/** | `operations/list`, `operations/stat`, `operations/copyfile`, `operations/movefile`, `operations/delete`, `operations/mkdir`, `operations/about`, `operations/check`, `operations/uploadfile`, `operations/publiclink` | File/directory CRUD, space info |
| **config/** | `config/create`, `config/update`, `config/delete`, `config/get`, `config/dump`, `config/listremotes`, `config/paths`, `config/providers` | Remote configuration management |
| **core/** | `core/stats`, `core/transferred`, `core/bwlimit`, `core/version`, `core/pid`, `core/quit`, `core/memstats`, `core/gc`, `core/command` | Runtime stats, bandwidth control, lifecycle |
| **job/** | `job/list`, `job/status`, `job/stop`, `job/stopgroup`, `job/batch` | Async job management |
| **mount/** | `mount/mount`, `mount/unmount`, `mount/unmountall`, `mount/listmounts`, `mount/types` | FUSE mount management |
| **vfs/** | `vfs/list`, `vfs/refresh`, `vfs/forget`, `vfs/stats`, `vfs/queue`, `vfs/poll-interval` | Virtual filesystem management |
| **options/** | `options/get`, `options/set`, `options/blocks`, `options/info` | Global option management |
| **cache/** | `cache/expire`, `cache/fetch`, `cache/stats` | Cache management |
| **backend/** | `backend/command` | Backend-specific commands |
| **fscache/** | `fscache/clear`, `fscache/entries` | Filesystem cache |
| **debug/** | `debug/set-block-profile-rate`, `debug/set-gc-percent`, `debug/set-mutex-profile-fraction`, `debug/set-soft-memory-limit` | Runtime profiling |
| **rc/** | `rc/list`, `rc/noop`, `rc/noopauth` | API introspection |

### Async Job System

Long-running operations (sync, copy, move) support async execution via a special `_async=true` parameter:

```json
POST /sync/copy
{
    "srcFs": "onedrive:Documents",
    "dstFs": "/local/backup",
    "_async": true
}
```

Response:
```json
{
    "jobid": 42
}
```

The job can then be monitored:
```json
POST /job/status
{"jobid": 42}
```

Response:
```json
{
    "id": 42,
    "finished": false,
    "success": false,
    "duration": 15.5,
    "startTime": "2024-01-15T10:30:00Z",
    "output": {}
}
```

Other special parameters:
- `_config`: Override config flags for this call
- `_filter`: Override filter flags for this call
- `_group`: Assign operation to a stats group for grouped monitoring

### `core/stats` Response (Key for GUI)

```json
{
    "bytes": 1234567890,
    "checks": 100,
    "deletes": 5,
    "elapsedTime": 45.2,
    "errors": 0,
    "eta": 120,
    "speed": 1048576,
    "totalBytes": 2468000000,
    "totalTransfers": 50,
    "transfers": 25,
    "transferring": [
        {
            "bytes": 524288,
            "eta": 10,
            "name": "Documents/report.pdf",
            "percentage": 50,
            "speed": 1048576,
            "size": 1048576
        }
    ],
    "checking": ["folder1/", "folder2/"]
}
```

### Authentication

- **Default**: No auth required for local-only calls; auth required for calls that access remotes
- **Basic auth**: `--rc-user` and `--rc-pass` flags
- **htpasswd**: `--rc-htpasswd` for file-based auth
- **`--rc-no-auth`**: Disable all auth checks (for trusted environments)

### Web GUI Integration

rclone can serve a web GUI alongside the API:
- `--rc-web-gui` enables the React-based web interface
- `--rc-files /path` serves custom static files
- `--rc-serve` exposes remote objects via HTTP browsing
- `--rc-enable-metrics` adds Prometheus/OpenMetrics endpoint at `/metrics`

---

## 3. How Syncthing's API Works

### Syncthing Architecture Overview

Syncthing (written in Go) exposes a REST API on its GUI port (default 8384). The same HTTP server serves both the web GUI (an AngularJS single-page application) and the REST API. The API is the **only** way the GUI communicates with the daemon -- there is no separate internal channel.

### Syncthing Endpoint Categories

| Category | Endpoints | Purpose |
|---|---|---|
| **System** | `GET /rest/system/status`, `GET /rest/system/connections`, `GET /rest/system/browse`, `POST /rest/system/restart`, `POST /rest/system/shutdown`, `POST /rest/system/reset`, `GET /rest/system/version`, `GET /rest/system/upgrade`, `POST /rest/system/pause`, `POST /rest/system/resume` | System state, lifecycle control |
| **Config** | `GET /rest/config`, `PUT /rest/config`, `GET /rest/config/folders`, `PUT /rest/config/folders/{id}`, `GET /rest/config/devices`, `PUT /rest/config/devices/{id}`, `GET /rest/config/options`, `PUT /rest/config/options` | Granular config CRUD (get whole config, modify parts, put back) |
| **Database** | `GET /rest/db/browse`, `GET /rest/db/completion`, `GET /rest/db/file`, `GET /rest/db/status`, `POST /rest/db/override`, `POST /rest/db/revert`, `POST /rest/db/scan` | Sync database queries, folder status, manual scans |
| **Events** | `GET /rest/events`, `GET /rest/events/disk` | Long-polling event stream |
| **Stats** | `GET /rest/stats/device`, `GET /rest/stats/folder` | Device/folder transfer statistics |
| **Debug** | `GET /rest/debug/peerCompletion`, `GET /rest/debug/httpmetrics`, `GET /rest/debug/cpuprof`, `GET /rest/debug/heapprof` | Profiling and debug info |
| **No-Auth** | `GET /rest/noauth/health` | Health check (no auth required) |

### Event System (Long Polling)

Syncthing's most distinctive feature for GUI integration is its **event API**:

```
GET /rest/events?since=0&events=StateChanged,FolderCompletion
```

Behavior:
- Returns a JSON array of event objects
- `since=<lastSeenID>` parameter: only returns events after this ID (default 0 = all events)
- **Blocks** (long polls) for up to 60 seconds if no new events are available
- Returns empty array `[]` on timeout
- Supports filtering by event type via `events=` parameter
- `/rest/events/disk` is a filtered endpoint returning only local/remote change events

Event object format:
```json
{
    "id": 42,
    "globalID": 42,
    "type": "StateChanged",
    "time": "2024-01-15T10:30:00.000000Z",
    "data": {
        "folder": "default",
        "from": "syncing",
        "to": "idle",
        "duration": 15.5
    }
}
```

Key event types:
- `ConfigSaved` -- Configuration was modified
- `DeviceConnected` / `DeviceDisconnected` -- Peer connectivity
- `FolderCompletion` -- Sync progress for a folder
- `FolderSummary` -- Current folder state summary
- `ItemFinished` / `ItemStarted` -- Individual file transfer events
- `StateChanged` -- Folder state transitions (idle, scanning, syncing, error)
- `LocalChangeDetected` / `RemoteChangeDetected` -- File change events
- `FolderErrors` -- Errors within a folder

### Syncthing Authentication

- **API Key**: Set in config, passed via `X-API-Key` request header or `Bearer` token in `Authorization` header
- **Username/Password**: Optional HTTP basic auth for the GUI
- **CSRF Token**: Required for browser-based access (prevents cross-site request forgery)
- The `/rest/noauth/health` endpoint is explicitly exempt from auth

### Syncthing Design Qualities

1. **Config is a first-class API resource**: GET the config, modify it, PUT it back. Granular sub-endpoints for folders, devices, options
2. **Events replace polling**: Instead of repeatedly querying status, clients subscribe to events and get pushed updates via long polling
3. **Idempotent design**: Most operations are safe to retry
4. **Single port**: API and GUI share one HTTP server
5. **Clean separation**: The web GUI uses only the public REST API -- no privileged internal channels

---

## 4. rclone's OneDrive Configuration

### Config File Format

rclone stores all remote configurations in `~/.config/rclone/rclone.conf` (path configurable). The format is INI-style with one section per remote:

```ini
[remote-name]
type = onedrive
client_id =
client_secret =
token = {"access_token":"...","token_type":"Bearer","refresh_token":"...","expiry":"2024-08-26T22:39:52Z"}
drive_id = b!Eqwertyuiopasdfghjklzxcvbnm-7mnbvcxzlkjhgfdsapoiuytrewqk
drive_type = business
```

### Example OneDrive Section

```ini
[my-onedrive]
type = onedrive
token = {"access_token":"eyJ0eXAiOi...","token_type":"Bearer","refresh_token":"OAQ...","expiry":"2024-08-26T22:39:52.486512262+08:00"}
drive_id = b!Eqwertyuiopasdfghjklzxcvbnm-7mnbvcxzlkjhgfdsapoiuytrewqk
drive_type = business
```

### Token Storage

The `token` field is a JSON blob stored inline within the INI value. It contains:

| Field | Description |
|---|---|
| `access_token` | The OAuth2 access token (JWT) |
| `token_type` | Always `"Bearer"` |
| `refresh_token` | Used to obtain new access tokens when the current one expires |
| `expiry` | ISO 8601 timestamp of when the access token expires |

Additional optional fields:
- `client_id` -- Custom application ID (blank = rclone's default)
- `client_secret` -- Custom application secret (blank = rclone's default)
- `region` -- For national cloud deployments (e.g., `us` for US Government, `de` for Germany, `cn` for China)

### Migration Considerations

If we want to support migrating from rclone:
- Parse `~/.config/rclone/rclone.conf` as INI
- Filter for sections where `type = onedrive`
- Extract `drive_id`, `drive_type`, and the JSON `token` blob
- The `drive_type` values are: `personal`, `business`, `documentLibrary` (SharePoint)
- Token format is standard OAuth2 -- the `refresh_token` is the key piece for re-authentication
- Note: rclone uses its own application_id by default (`client_id` field), which differs from abraunegg's and from ours. Migrating tokens may require re-authentication if application IDs differ.

---

## 5. abraunegg's Filter/Ignore File Formats

### abraunegg Config File Format

The reference implementation uses a bespoke flat key=value format (not INI, TOML, YAML, or JSON):

```
sync_dir = "~/OneDrive"
skip_file = "~*|.~*|*.tmp|*.swp|*.partial"
skip_dir = "Desktop|Documents/IISExpress"
monitor_interval = "300"
```

Rules:
- Parsed by regex: `^(\w+)\s*=\s*"(.*)"\s*$`
- All values in double quotes (even booleans and integers)
- No section headers
- Comments: lines starting with `#` or `;`
- `skip_file` and `skip_dir` can appear multiple times; values are merged with `|`

Config file location: `~/.config/onedrive/config` (or `--confdir` override, or `/etc/onedrive/config` fallback).

### skip_file

Excludes files by filename pattern.

**Default:** `~*|.~*|*.tmp|*.swp|*.partial`

**Pattern syntax:** Pipe-separated glob patterns. `*` matches any characters, `?` matches a single character. Case insensitive. Compiled to regex via `wild2regex()`.

**Matching modes:**
- `*.txt` -- Matches any file ending in `.txt` anywhere
- `filename.ext` -- Matches this exact filename anywhere
- `/path/to/file/name.ext` -- Matches this exact path relative to sync_dir

```
skip_file = "~*|/Documents/OneNote*|/Documents/config.xlaunch|myfile.ext"
```

### skip_dir

Excludes directories by name or path pattern.

**Default:** Empty (no directories skipped)

**Matching modes:**
- `DirectoryName` -- Matches this directory name anywhere in the tree
- `/Explicit/Path/To/Dir` -- Matches only this exact path relative to sync_dir

```
skip_dir = "Desktop|Documents/Visual Studio*|.cache"
```

Can be specified multiple times:
```
skip_dir = "SkipThisAnywhere"
skip_dir = "/Explicit/Path"
```

Related options:
- `skip_dir_strict_match = "true"` -- Only match full path, not partial names
- `skip_dotfiles = "true"` -- Skip all files/folders starting with `.`
- `skip_size = "50"` -- Skip files larger than 50 MB

### sync_list File

A separate file (`~/.config/onedrive/sync_list`) that defines a **whitelist** of paths to sync. Everything not listed is excluded.

**Key semantics:**
1. Excludes everything by default
2. Exclusions override inclusions (put exclusions first)
3. Comments: lines starting with `#` or `;`
4. Empty lines ignored

**Rule types:**

| Rule Pattern | Meaning | Performance |
|---|---|---|
| `/Documents/` | Include `/Documents/` directory from root only | Fast (path-scoped) |
| `/Documents/*.pdf` | Include PDFs in root Documents | Fast |
| `/Parent/Blog/*` | Include all contents of specific directory | Fast |
| `Documents/` | Include any directory named `Documents` anywhere | Slow (exhaustive scan) |
| `notes.txt` | Include any file named `notes.txt` anywhere | Slow (exhaustive scan) |
| `!/Secret_data/*` | Exclude `/Secret_data/` from root | N/A (exclusion) |
| `!Documents/temp*` | Exclude temp dirs inside any Documents | N/A (exclusion) |

**Wildcard support:**
- `*` matches any characters within a single path segment
- `**` matches directories recursively across any depth

**Prefix meanings:**
- `/` prefix: matches only from OneDrive root
- No `/` prefix: matches anywhere (expensive -- forces full tree scan)
- `!` or `-` prefix: exclusion rule
- Trailing `/`: matches directories only

**Example:**
```
# Exclusions first
!Documents/temp*
!/Secret_data/*
!node_modules/*
!__pycache__/*

# Inclusions
/Documents/
/Pictures/Camera Roll/*
/Programming
Work/Project*
notes.txt
```

### Filter Evaluation Order

The reference implementation evaluates filters in this order:
1. `check_nosync` (presence of `.nosync` file)
2. `skip_dotfiles`
3. `skip_symlinks`
4. `skip_dir`
5. `skip_file`
6. `sync_list`
7. `skip_size`

After any filter change, a `--resync` is required.

---

## 6. IPC Options for Go Daemons

### Unix Domain Sockets

**Mechanism:** File-based socket on the local filesystem (e.g., `/tmp/onedrive-sync.sock` or `~/.local/run/onedrive-sync/control.sock`).

**Pros:**
- No network port allocation needed
- Filesystem permissions provide access control
- Slightly lower overhead than TCP loopback
- Works on Linux, macOS, and modern Windows (AF_UNIX support added in Windows 10)
- Go has native support via `net.Listen("unix", path)` and `net.Dial("unix", path)`

**Cons:**
- Socket file cleanup needed on unclean shutdown (stale `.sock` files)
- No browser access without a proxy
- Path conventions vary by OS

**Protocol on top:** JSON-RPC 2.0 or a custom JSON line protocol are common choices.

### localhost HTTP/REST API

**Mechanism:** Standard HTTP server bound to `127.0.0.1:<port>`.

**Pros:**
- Universally understood by every language and platform
- Browser-accessible (enables web GUI)
- Rich tooling (curl, Postman, browser devtools)
- Can serve static files for web GUI alongside API
- Go's `net/http` is excellent
- Prometheus metrics easily added
- Easy to add TLS if needed

**Cons:**
- Port allocation (must pick a port, handle conflicts)
- Slightly more overhead than Unix sockets
- CORS needed for browser-based GUIs
- Need to implement authentication

**This is what rclone and Syncthing use.** It is the proven pattern for Go sync daemons.

### gRPC over Unix Socket

**Mechanism:** Protocol Buffers with gRPC framework over a Unix domain socket.

**Pros:**
- Strongly typed API via `.proto` files
- Bidirectional streaming (ideal for real-time events)
- Efficient binary serialization
- Generated client libraries for any language
- Built-in deadline/timeout semantics

**Cons:**
- Adds a heavy dependency (protobuf + gRPC libraries)
- Not browser-accessible without grpc-web proxy
- Debugging is harder (binary protocol)
- Overkill for a single-daemon scenario
- Proto file management overhead

### D-Bus

**Mechanism:** Linux desktop IPC bus (session bus for user apps, system bus for system services).

**Pros:**
- Native Linux desktop integration
- Standard for desktop application communication
- Supports signals (event broadcasting) and method calls
- Service activation (D-Bus can start your daemon on demand)

**Cons:**
- Linux-only (no macOS or Windows support)
- Complex API
- Go D-Bus libraries exist but are not as polished as HTTP
- Not suitable for cross-platform applications
- Performance not great for bulk data transfer

### Named Pipes / FIFOs

**Mechanism:** Special filesystem objects for unidirectional byte streams.

**Pros:**
- Simple concept
- No port allocation

**Cons:**
- Unidirectional (need two pipes for request/response)
- No multiplexing
- Awkward for concurrent access
- Rarely used in modern Go applications

### What Go Tools Actually Do

| Tool | IPC Mechanism | Notes |
|---|---|---|
| **Syncthing** | HTTP REST on localhost | Single port for API + web GUI, long-polling events, API key auth |
| **rclone** | HTTP REST on localhost | POST-only JSON API, async job system, basic auth or htpasswd |
| **Docker daemon** | Unix socket (`/var/run/docker.sock`) | HTTP REST protocol over Unix socket; also supports TCP |
| **Kubernetes (kubectl)** | HTTP REST over TCP | API server with rich resource model |
| **Tailscale** | Unix socket + HTTP REST | `tailscaled` exposes HTTP API over Unix socket |
| **CockroachDB** | gRPC + HTTP REST | gRPC for inter-node, HTTP for admin UI |
| **Prometheus** | HTTP REST | Query API + web UI on same port |
| **HashiCorp Vault** | HTTP REST | API-first design, CLI is a thin HTTP client |
| **HashiCorp Consul** | HTTP REST + gRPC | HTTP for user-facing, gRPC for internal |

The dominant pattern for Go daemons is **HTTP REST on localhost**, with optional Unix socket transport for tighter security (Docker, Tailscale).

---

## 7. Recommendations for Our Daemon

Based on this survey, the recommended approach for our sync daemon's control interface:

### Primary: HTTP REST API on localhost

Follow the Syncthing/rclone model:
- Single HTTP server on `127.0.0.1:<configurable-port>` (default e.g., 8726)
- JSON request/response bodies
- API key authentication (generated on first run, stored in config)
- CORS headers for browser-based GUI access

### Event System: Long Polling (Syncthing model)

```
GET /api/v1/events?since=<lastID>&types=SyncProgress,TransferComplete,Error
```

- Returns JSON array of events
- Blocks up to 60s if no new events
- Supports event type filtering
- Each event has a monotonic ID for reliable sequencing
- This replaces the need for WebSockets while being simpler to implement

### Suggested Endpoint Structure

```
# Lifecycle
POST   /api/v1/daemon/shutdown
POST   /api/v1/daemon/restart
GET    /api/v1/daemon/version
GET    /api/v1/daemon/status

# Sync Operations
POST   /api/v1/sync/start          -- Trigger one-time sync
POST   /api/v1/sync/stop           -- Stop current sync
GET    /api/v1/sync/status          -- Current sync state
GET    /api/v1/sync/progress        -- Transfer stats (like rclone core/stats)

# Configuration
GET    /api/v1/config               -- Full config
PUT    /api/v1/config               -- Replace config
PATCH  /api/v1/config               -- Partial update
GET    /api/v1/config/filters       -- Current filter rules
PUT    /api/v1/config/filters       -- Update filters

# Account
GET    /api/v1/account              -- Account info, drive type, space used
GET    /api/v1/account/quota        -- Storage quota

# Events
GET    /api/v1/events               -- Long-polling event stream

# Database / Files
GET    /api/v1/db/browse?path=/     -- Browse synced file tree
GET    /api/v1/db/file?path=/x      -- File sync status and metadata
GET    /api/v1/db/status            -- Overall database stats

# Debug
GET    /api/v1/debug/log            -- Recent log entries
GET    /api/v1/debug/goroutines     -- Goroutine dump
GET    /metrics                     -- Prometheus metrics
```

### Optional: Unix Socket Transport

For tighter security (Docker-style), also listen on a Unix socket:
```
~/.local/run/onedrive-client/control.sock
```

This allows GUI frontends to connect without needing a network port, and filesystem permissions control access. The same HTTP API runs over both transports.

### What NOT to Do

1. **Do not use stdout parsing** (OneDriveGUI model) -- fragile, one-way, no structured data
2. **Do not use gRPC** -- overkill for our use case, blocks browser access
3. **Do not use D-Bus as primary IPC** -- Linux-only, we need cross-platform
4. **Do not require WebSockets for events** -- long polling is simpler and sufficient; can add WebSocket support later as an enhancement

### CLI as First API Client

Design the CLI (`onedrive-client sync`, `onedrive-client status`, etc.) to communicate with the daemon via the same REST API. This means:
- The CLI is a thin HTTP client
- Any GUI frontend can do exactly what the CLI does
- The API is tested by every CLI invocation
- This is exactly how rclone, Syncthing, Docker, and Vault work
