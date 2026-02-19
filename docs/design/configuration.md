# Configuration Specification: onedrive-go

This document specifies the complete configuration system for onedrive-go — file format, option catalog, multi-profile mechanics, filtering, validation, hot reload, migration, and the interactive setup wizard. It is the definitive reference for every configurable behavior in the system.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Config File Structure](#2-config-file-structure)
3. [Multi-Account Profiles](#3-multi-account-profiles)
4. [Authentication Options](#4-authentication-options)
5. [Sync Behavior Options](#5-sync-behavior-options)
6. [Filtering Options](#6-filtering-options)
7. [Transfer Options](#7-transfer-options)
8. [Safety Options](#8-safety-options)
9. [Logging Options](#9-logging-options)
10. [Network Options](#10-network-options)
11. [CLI Flag Reference](#11-cli-flag-reference)
12. [Config Init Wizard](#12-config-init-wizard)
13. [Config Validation](#13-config-validation)
14. [Hot Reload (SIGHUP)](#14-hot-reload-sighup)
- [Appendix A: Complete Options Reference Table](#appendix-a-complete-options-reference-table)
- [Appendix B: Migration Mapping Tables](#appendix-b-migration-mapping-tables)
- [Appendix C: Decision Log](#appendix-c-decision-log)

---

## 1. Overview

### 1.1 Purpose and Scope

This specification defines every configurable aspect of onedrive-go: how configuration is stored, loaded, validated, overridden, reloaded at runtime, and migrated from other tools. It covers:

- Config file format and locations
- Override precedence (defaults, file, env, CLI)
- Multi-account profile mechanics
- Every configuration option with type, default, validation, and hot-reload status
- Filtering system (sync_paths, config patterns, .odignore markers)
- Stale file handling when filters change
- Interactive setup wizard
- Migration from abraunegg and rclone
- Config validation and error reporting

### 1.2 Design Philosophy

**Convention over configuration.** Sensible defaults that work for 90% of users. A new user should be able to run `onedrive-go config init`, answer a few questions, and have a working sync. Advanced users can tune every knob.

**Safe defaults.** Every default errs on the side of caution: big-delete protection on, recycle bin on, conservative timeouts, parallel workers within API guidance. A user who never touches the config file should never lose data.

**Explicit over implicit.** Unknown config keys are fatal errors. Changed filters require explicit user action. No silent behavior changes between versions.

**Single source of truth.** One TOML config file, one place to look. No config scattered across multiple files, registries, or environment variables (environment variables exist only for path overrides and profile selection).

### 1.3 Config File Format

**Format**: [TOML v1.0](https://toml.io/en/v1.0.0) via the [BurntSushi/toml](https://github.com/BurntSushi/toml) Go library.

TOML was chosen for:
- Human-readable and human-writable (unlike JSON)
- Supports comments (unlike JSON)
- Hierarchical sections map naturally to Go structs (unlike flat key-value)
- Well-specified grammar (unlike YAML's implicit typing pitfalls)
- Strong Go ecosystem support

### 1.4 File Locations

Config file locations follow platform conventions:

| Platform | Config Directory | Config File |
|----------|-----------------|-------------|
| **Linux** | `~/.config/onedrive-go/` | `~/.config/onedrive-go/config.toml` |
| **macOS** | `~/Library/Application Support/onedrive-go/` | `~/Library/Application Support/onedrive-go/config.toml` |

On Linux, `XDG_CONFIG_HOME` is respected: if set, the config directory is `$XDG_CONFIG_HOME/onedrive-go/` instead of `~/.config/onedrive-go/`.

Additional data directories ([architecture §16.2](architecture.md)):

| Purpose | Linux | macOS |
|---------|-------|-------|
| State databases | `~/.local/share/onedrive-go/state/` | `~/Library/Application Support/onedrive-go/state/` |
| Logs | `~/.local/share/onedrive-go/logs/` | `~/Library/Application Support/onedrive-go/logs/` |
| Tokens | `~/.config/onedrive-go/tokens/` | `~/Library/Application Support/onedrive-go/tokens/` |
| Cache | `~/.cache/onedrive-go/` | `~/Library/Caches/onedrive-go/` |

If no config file exists, the application runs with built-in defaults for all options. A profile is still required for authentication (via `config init` or `login`).

### 1.5 Override Precedence

Configuration values are resolved in the following order (later sources override earlier):

```
1. Built-in defaults (hardcoded in Go)
        ↓
2. Config file (config.toml)
        ↓
3. Environment variables (ONEDRIVE_GO_*)
        ↓
4. CLI flags (--option value)
```

**Rules**:
- CLI flags **replace** config file values entirely (no merging)
- Environment variables override config file values for the specific keys they control
- If a config file option and CLI flag both exist, the CLI flag wins
- Per-profile sections override global sections (see [§2.3](#23-per-profile-overrides))

### 1.6 Environment Variables

Only three environment variables are supported. They provide path and profile overrides for deployment scenarios (containers, CI, systemd) where modifying the config file is impractical:

| Variable | Purpose | Equivalent CLI Flag |
|----------|---------|-------------------|
| `ONEDRIVE_GO_CONFIG` | Override config file path | `--config` |
| `ONEDRIVE_GO_PROFILE` | Select active profile | `--profile` |
| `ONEDRIVE_GO_SYNC_DIR` | Override sync directory | `--sync-dir` |

**Design rationale**: We deliberately limit env var support to key paths only. Exposing every option as an env var creates a parallel configuration surface that is hard to document, validate, and debug. For Docker/container deployments, mount a config file or use CLI flags.

---

## 2. Config File Structure

### 2.1 Complete Annotated Example

```toml
# =============================================================================
# onedrive-go configuration
# =============================================================================
# Generated by: onedrive-go config init
# Documentation: https://github.com/tonimelisma/onedrive-go
#
# Override precedence: defaults < config file < env vars < CLI flags
# Unknown keys cause a fatal error. Remove or comment out options you don't use.

# =============================================================================
# Profiles — at least one profile is required
# =============================================================================

[profile.default]
account_type = "personal"           # personal, business, sharepoint
sync_dir = "~/OneDrive"             # local directory to sync
remote_path = "/"                   # remote path to sync from
# drive_id = ""                     # required for SharePoint, optional otherwise
# application_id = ""               # custom Azure app ID (uses built-in default)

[profile.work]
account_type = "business"
sync_dir = "~/OneDrive-Work"
remote_path = "/"
# azure_ad_endpoint = ""            # national cloud: USL4, USL5, DE, CN
# azure_tenant_id = ""              # Azure AD tenant GUID or domain

# Per-profile filter override (completely replaces global [filter])
# [profile.work.filter]
# skip_files = ["*.tmp", "*.partial"]
# skip_dirs = ["node_modules", ".git", "vendor"]

# =============================================================================
# Filtering — global defaults (overridden by per-profile [profile.NAME.filter])
# =============================================================================

[filter]
skip_dotfiles = false               # skip files/dirs starting with .
skip_symlinks = false               # skip symbolic links (default: follow them)
max_file_size = "50GB"              # skip files larger than this (0 = no limit)
skip_files = [                      # file name patterns to exclude
    "~*",
    ".~*",
    "*.tmp",
    "*.swp",
    "*.partial",
    "*.crdownload",
    ".DS_Store",
    "Thumbs.db",
]
skip_dirs = [                       # directory name patterns to exclude
    "node_modules",
    ".git",
    "__pycache__",
    ".Trash-*",
]
ignore_marker = ".odignore"         # per-directory marker file name
# sync_paths = ["/Documents", "/Photos"]  # selective sync (empty = sync all)

# =============================================================================
# Transfers — parallel workers and bandwidth
# =============================================================================

[transfers]
parallel_downloads = 8              # simultaneous download workers (max 16)
parallel_uploads = 8                # simultaneous upload workers (max 16)
parallel_checkers = 8               # simultaneous hash check workers (max 16)
chunk_size = "10MB"                 # upload chunk size (320KiB multiples, 10-60MB)
bandwidth_limit = "0"               # global bandwidth limit (0 = unlimited)
transfer_order = "default"          # default, size_asc, size_desc, name_asc, name_desc

# Time-of-day bandwidth schedule (local system time, 24h format)
# bandwidth_schedule = [
#     { time = "08:00", limit = "5MB/s" },
#     { time = "18:00", limit = "50MB/s" },
#     { time = "23:00", limit = "0" },
# ]

# =============================================================================
# Safety — thresholds and protective defaults
# =============================================================================

[safety]
big_delete_threshold = 1000         # abort if deleting more than N items
big_delete_percentage = 50          # abort if deleting more than N% of total items
big_delete_min_items = 10           # skip big-delete check if total items < N
min_free_space = "1GB"              # minimum free disk space before downloading
use_recycle_bin = true              # remote: use OneDrive recycle bin (not permanent delete)
use_local_trash = true              # local: use OS trash for remote-triggered deletes
disable_download_validation = false # skip hash verification on downloads (SharePoint workaround)
disable_upload_validation = false   # skip hash verification on uploads (SharePoint workaround)
sync_dir_permissions = "0700"       # POSIX permissions for created directories
sync_file_permissions = "0600"      # POSIX permissions for created files
tombstone_retention_days = 30       # days to keep tombstone records for move detection

# =============================================================================
# Sync behavior
# =============================================================================

[sync]
poll_interval = "5m"                # remote change polling interval (min 5m)
fullscan_frequency = 12             # full scan every N poll intervals (0 = disabled)
websocket = true                    # near-real-time remote change detection
conflict_strategy = "keep_both"     # keep_both (default)
conflict_reminder_interval = "1h"   # nag interval for unresolved conflicts in --watch
dry_run = false                     # preview operations without executing
verify_interval = "0"               # periodic full-tree hash verification (0 = disabled)
shutdown_timeout = "30s"            # time to wait for in-flight transfers on shutdown

# =============================================================================
# Logging
# =============================================================================

[logging]
log_level = "info"                  # debug, info, warn, error
log_file = ""                       # explicit log file path (empty = auto)
log_format = "auto"                 # text, json, auto (auto = text if interactive, json if quiet)
log_retention_days = 30             # days to keep log files

# =============================================================================
# Network
# =============================================================================

[network]
connect_timeout = "10s"             # TCP connection timeout
data_timeout = "60s"                # data transfer timeout (no data received)
user_agent = ""                     # custom User-Agent (empty = default ISV format)
force_http_11 = false               # force HTTP/1.1 instead of HTTP/2
```

### 2.2 Section Hierarchy

The config file uses TOML sections to organize options by functional area:

```
config.toml
├── [profile.NAME]              # Per-account settings (required, at least one)
│   ├── account_type
│   ├── sync_dir
│   ├── remote_path
│   ├── drive_id
│   ├── application_id
│   ├── azure_ad_endpoint
│   ├── azure_tenant_id
│   └── [profile.NAME.section]  # Per-profile overrides for any section
│       ├── [profile.NAME.filter]
│       ├── [profile.NAME.transfers]
│       ├── [profile.NAME.safety]
│       ├── [profile.NAME.sync]
│       ├── [profile.NAME.logging]
│       └── [profile.NAME.network]
├── [filter]                    # Global filtering defaults
├── [transfers]                 # Global transfer defaults
├── [safety]                    # Global safety defaults
├── [sync]                      # Global sync behavior defaults
├── [logging]                   # Global logging defaults
└── [network]                   # Global network defaults
```

### 2.3 Per-Profile Overrides

Any configuration section can be overridden at the profile level by nesting it under `[profile.NAME.section]`. Per-profile sections **completely replace** the global section — they do not merge with it.

**Example: per-profile filter override**

```toml
# Global filter defaults
[filter]
skip_files = ["~*", "*.tmp", "*.partial"]
skip_dirs = ["node_modules", ".git"]
skip_dotfiles = false

# Work profile overrides the entire [filter] section
[profile.work.filter]
skip_files = ["*.tmp", "*.partial"]      # Different list — global defaults NOT merged
skip_dirs = ["node_modules", ".git", "vendor"]
skip_dotfiles = true                      # Different value
```

For the `work` profile, the effective filter config is exactly what is specified in `[profile.work.filter]`. The global `skip_files` list (which includes `~*`) is NOT merged in. This is a deliberate design choice: merge semantics for arrays and maps are confusing and error-prone. Complete replacement is predictable.

**Resolution algorithm**:

```
function ResolveConfig(profileName, sectionName):
    if [profile.NAME.section] exists:
        return profile section    # Complete replacement
    else:
        return global section     # Fall back to global
```

### 2.4 Merge Rules

| Scenario | Behavior |
|----------|----------|
| Global section only | Global values used |
| Profile section only | Profile values used, defaults for any missing keys within the section |
| Both global and profile | Profile section **completely replaces** global section |
| Neither (section absent everywhere) | Built-in defaults for all keys in that section |

**Within a section**, individual keys that are omitted use their built-in defaults. Only the section-level choice (global vs profile) is all-or-nothing.

### 2.5 Unknown Key Handling

Unknown keys in the config file cause a **fatal error** at startup. The application refuses to start and suggests the closest matching known key.

```
Error: unknown config key "skip_file" in [filter]
Did you mean "skip_files"? (note: arrays use plural names)
```

The closest-match suggestion uses Levenshtein distance (edit distance) against all known keys in the same section. If the edit distance is <= 3, the suggestion is shown. This catches common typos and helps users migrating from other tools (e.g., `skip_file` from abraunegg vs our `skip_files`).

**Rationale** (Decision C1): Silent ignoring of unknown keys leads to configuration that appears to work but does not. A user who types `skip_fles` instead of `skip_files` would wonder why their filter is not working. Fatal error with suggestion is the safest approach.

---

## 3. Multi-Account Profiles

### 3.1 Profile Structure

Each OneDrive account is represented as a named profile under the `[profile]` section. At least one profile must be defined for sync operations (file operations like `ls` and `get` can work without a profile if credentials exist).

```toml
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"
remote_path = "/"

[profile.work]
account_type = "business"
sync_dir = "~/OneDrive-Work"
remote_path = "/"
azure_tenant_id = "contoso.onmicrosoft.com"

[profile.sharepoint-docs]
account_type = "sharepoint"
drive_id = "b!abc123def456..."
sync_dir = "~/SharePoint-Docs"
remote_path = "/Shared Documents"
```

### 3.2 Per-Profile Fields

These fields can only appear inside a `[profile.NAME]` section:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `account_type` | String | Yes | `"personal"`, `"business"`, or `"sharepoint"` |
| `sync_dir` | String | Yes | Local directory to sync (tilde-expanded) |
| `remote_path` | String | No | Remote path to sync from (default: `"/"`) |
| `drive_id` | String | Conditional | Drive ID to sync. Required for `sharepoint`, optional for others. |
| `application_id` | String | No | Custom Azure application ID (default: built-in) |
| `azure_ad_endpoint` | String | No | National cloud endpoint (`USL4`, `USL5`, `DE`, `CN`) |
| `azure_tenant_id` | String | No | Azure AD tenant GUID or domain |

### 3.3 Database Isolation

Each profile gets its own SQLite database file ([data-model §2](data-model.md)):

| Platform | Database Path |
|----------|--------------|
| Linux | `~/.local/share/onedrive-go/state/{profile}.db` |
| macOS | `~/Library/Application Support/onedrive-go/state/{profile}.db` |

This ensures complete isolation: profiles cannot interfere with each other's sync state, delta tokens, conflict ledgers, or stale file tracking.

### 3.4 Token Isolation

Each profile stores its OAuth tokens in a separate file ([architecture §9.1](architecture.md)):

| Platform | Token Path |
|----------|-----------|
| Linux | `~/.config/onedrive-go/tokens/{profile}.json` |
| macOS | `~/Library/Application Support/onedrive-go/tokens/{profile}.json` |

Token files are created with `0600` permissions (owner read/write only). The directory is created with `0700` permissions.

### 3.5 CLI Usage

```bash
# Use a specific profile
onedrive-go sync --profile work
onedrive-go ls /Documents --profile personal

# Default profile if --profile is omitted
onedrive-go sync                     # uses "default" profile

# Sync all profiles in continuous mode
onedrive-go sync --watch             # syncs all profiles concurrently

# Profile selection via environment variable
ONEDRIVE_GO_PROFILE=work onedrive-go sync
```

When `--profile` is omitted:
- **`sync --watch`**: Syncs ALL configured profiles concurrently, each with its own sync loop and worker pools
- **All other commands**: Use the profile named `"default"`. If no `"default"` profile exists and only one profile is defined, that profile is used. If multiple profiles exist and none is named `"default"`, an error is raised.

### 3.6 Daemon Mode

`sync --watch` runs a single process that manages all profiles concurrently ([architecture §18.2](architecture.md)):

- Each profile gets its own sync loop goroutine
- Each profile gets its own set of worker pools (uploads, downloads, checkers)
- Each profile has an independent database connection
- Failures in one profile do not affect other profiles
- SIGHUP reloads configuration for all profiles

---

## 4. Authentication Options

### 4.1 Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `application_id` | String | (built-in) | Profile | Azure application (client) ID for OAuth2. Users may register their own app in Azure Portal. Empty value uses the built-in default. |
| `account_type` | String | (required) | Profile | Account type: `"personal"`, `"business"`, or `"sharepoint"`. Determines API behavior, hash availability, and quirk handling. |
| `azure_ad_endpoint` | String | `""` | Profile | National cloud endpoint. Valid values: `"USL4"` (US Gov), `"USL5"` (US Gov DoD), `"DE"` (Germany), `"CN"` (China/21Vianet). Empty string uses global Azure AD. |
| `azure_tenant_id` | String | `""` | Profile | Azure AD tenant ID (GUID) or fully qualified domain name. Locks authentication to a specific tenant. Required when `azure_ad_endpoint` is set. |

### 4.2 Authentication Flow

The `login` command authenticates a profile:

```bash
onedrive-go login                    # Interactive: auto-detect best method
onedrive-go login --headless         # Device code flow (no browser required)
onedrive-go login --profile work     # Authenticate a specific profile
```

**Auto-detection logic**:
1. If a browser is available (interactive TTY, `DISPLAY`/`WAYLAND_DISPLAY` set on Linux, always on macOS): localhost redirect flow
2. Otherwise: device code flow (display URL + code for manual entry on any device)

The `--headless` flag forces device code flow regardless of environment. This is the primary method for SSH sessions, containers, and headless servers.

### 4.3 Token Storage and Refresh

- Tokens are stored as JSON files with `0600` permissions, one per profile
- Refresh tokens are used to obtain new access tokens automatically
- Token refresh happens transparently before each API call when the access token is expired
- If the refresh token itself is expired or revoked, the user is prompted to re-authenticate
- Token files contain: access token, refresh token, expiry timestamp, and token type
- Bearer tokens are NEVER written to log files at any log level ([architecture §9.2](architecture.md))

---

## 5. Sync Behavior Options

### 5.1 Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `sync_dir` | String | `"~/OneDrive"` | Profile | Local directory to sync. Tilde is expanded. Must be an absolute path after expansion. Changes require restart + resync. |
| `remote_path` | String | `"/"` | Profile | Remote path within the drive to sync from. Must start with `/`. |
| `drive_id` | String | `""` | Profile | OneDrive drive ID. Required for `sharepoint` accounts. For `personal` and `business`, auto-detected from the authenticated user's default drive. |
| `poll_interval` | Duration | `"5m"` | Sync | Interval between remote change polling cycles in `--watch` mode. Minimum 5 minutes. Format: Go duration string (`"5m"`, `"10m"`, `"1h"`). |
| `fullscan_frequency` | Integer | `12` | Sync | Number of poll intervals between full filesystem scans. Default: every 12 intervals (1 hour at default poll_interval). `0` disables full scans. Minimum non-zero value: `2`. |
| `dry_run` | Boolean | `false` | Sync | Preview all operations without executing. No downloads, uploads, moves, or deletes. Equivalent to `--dry-run` CLI flag. |
| `conflict_strategy` | String | `"keep_both"` | Sync | Conflict resolution strategy. Currently only `"keep_both"` is supported. Future: `"keep_local"`, `"keep_remote"`, `"newest"`. |
| `conflict_reminder_interval` | Duration | `"1h"` | Sync | How often to remind the user about unresolved conflicts in `--watch` mode. `"0"` disables reminders. |
| `websocket` | Boolean | `true` | Sync | Enable WebSocket subscriptions for near-real-time remote change detection. When disabled, relies on poll_interval only. |
| `verify_interval` | Duration | `"0"` | Sync | Periodic full-tree hash verification interval. `"0"` disables. Example: `"168h"` for weekly. When enabled, all local files are re-hashed and compared against the state DB and remote hashes. |
| `shutdown_timeout` | Duration | `"30s"` | Sync | Maximum time to wait for in-flight transfers to complete during graceful shutdown (SIGINT/SIGTERM). After this timeout, operations are canceled. |

### 5.2 Duration Format

Duration values use Go's duration string format:

| Format | Meaning |
|--------|---------|
| `"5m"` | 5 minutes |
| `"1h"` | 1 hour |
| `"30s"` | 30 seconds |
| `"2h30m"` | 2 hours and 30 minutes |
| `"168h"` | 1 week (168 hours) |
| `"0"` | Disabled / no duration |

---

## 6. Filtering Options

### 6.1 Three-Layer Cascade

Filtering uses a three-layer cascade where each layer can only exclude more items. A file must pass all three layers to be included in sync. This matches [sync-algorithm §6.1](sync-algorithm.md) and [architecture §11](architecture.md).

```
Item path
  |
  v
+---------------------+
| 1. sync_paths       |  If set, only these paths and their children
|    allowlist         |  are considered. Everything else is excluded
|                     |  immediately. Parent directories of allowed
|                     |  paths are traversed (not synced).
+---------+-----------+
          | (passes)
          v
+---------------------+
| 2. Config patterns  |  skip_files, skip_dirs, skip_dotfiles,
|                     |  skip_symlinks, max_file_size
+---------+-----------+
          | (passes)
          v
+---------------------+
| 3. .odignore        |  Per-directory marker files with
|    marker files     |  gitignore-style patterns
+---------+-----------+
          | (passes)
          v
      INCLUDED
```

**Key property**: Each layer can only EXCLUDE more. No layer can include back an item excluded by a previous layer. This guarantees that the filter outcome is predictable and composable.

### 6.2 Filter Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `skip_files` | Array of strings | `["~*", ".~*", "*.tmp", "*.swp", "*.partial", "*.crdownload", ".DS_Store", "Thumbs.db"]` | Filter | File name patterns to exclude. Case-insensitive. Supports `*` (any chars) and `?` (single char) wildcards. |
| `skip_dirs` | Array of strings | `[]` | Filter | Directory name patterns to exclude. Case-insensitive. Same wildcard support. A bare name matches anywhere in the tree. A path starting with `/` matches only at that position relative to sync root. |
| `skip_dotfiles` | Boolean | `false` | Filter | Exclude all files and directories whose names start with `.` |
| `skip_symlinks` | Boolean | `false` | Filter | Exclude all symbolic links. When `false`, symlinks are followed (target synced as regular file/dir). Broken and circular symlinks are always skipped. |
| `max_file_size` | String | `"0"` | Filter | Skip files larger than this size. `"0"` = no limit. Format: human-readable size (`"50MB"`, `"1GB"`, `"500KB"`). |
| `sync_paths` | Array of strings | `[]` | Filter/Profile | Selective sync: if non-empty, ONLY these paths and their children are synced. Paths are relative to the sync root and must start with `/`. Empty array = sync everything. |
| `ignore_marker` | String | `".odignore"` | Filter | Filename of per-directory ignore marker files. |

**Note on `skip_files` and `skip_dirs` format**: These are TOML arrays of strings, NOT pipe-delimited strings. This is a deliberate improvement over a bespoke pipe-delimited format used by other tools. TOML arrays are standard, easy to parse, and do not require escaping pipe characters in patterns.

### 6.3 Pattern Matching Semantics

#### skip_files

- Patterns are matched against the **basename** of each file
- Case-insensitive matching
- `*` matches zero or more characters (including path separators when matching full paths)
- `?` matches exactly one character
- Applied in both upload and download directions
- Applied to files only, not directories

#### skip_dirs

- In **default mode** (no leading `/`): pattern matches against each individual path component. A pattern `temp` matches a directory named `temp` at any depth.
- In **anchored mode** (leading `/`): pattern matches the full path relative to the sync root. `/Documents/temp` only matches that specific directory.
- Case-insensitive matching
- Same wildcard support as skip_files
- Applied in both upload and download directions
- Applied to directories only, not files
- When a directory is excluded, all its descendants are automatically excluded without individual evaluation

#### max_file_size

- Parsed as a human-readable size string
- Valid suffixes: `KB` (1000), `KiB` (1024), `MB` (1000^2), `MiB` (1024^2), `GB` (1000^3), `GiB` (1024^3), `TB` (1000^4), `TiB` (1024^4)
- Bare number is interpreted as bytes
- Applied to files only (size is not meaningful for directories)
- Applied in both directions (local size for uploads, API-reported size for downloads)

### 6.4 sync_paths (Selective Sync)

`sync_paths` provides an inclusion allowlist. When set, ONLY the listed paths and their children are synced. Everything else is excluded.

```toml
[filter]
sync_paths = ["/Documents", "/Photos/Camera Roll", "/Work/Projects"]
```

**Semantics**:
- Paths must start with `/` (relative to the sync root)
- A path includes all its descendants (files and subdirectories)
- Parent directories of included paths are traversed (for directory walking) but NOT synced themselves
- An empty array means "sync everything" (the default)
- `sync_paths` is evaluated as the FIRST filter layer, before config patterns and .odignore

**Interaction with other filters**: Items that pass sync_paths are still subject to skip_files, skip_dirs, skip_dotfiles, max_file_size, and .odignore. sync_paths narrows the scope; other filters further exclude within that scope.

### 6.5 .odignore Marker Files

Drop an `.odignore` file in any directory to control filtering within that directory. The file uses **full gitignore syntax**, implemented via a Go gitignore library (e.g., `go-gitignore` or `sabhiram/go-gitignore`).

**Supported syntax**:

| Feature | Example | Description |
|---------|---------|-------------|
| Basic glob | `*.log` | Exclude all .log files in this directory |
| Directory-only | `build/` | Exclude directories named `build` |
| Negation | `!important.log` | Re-include a file excluded by a previous pattern |
| Double-star | `**/temp` | Exclude `temp` at any depth below this directory |
| Anchored pattern | `/dist` | Only match at this directory level, not deeper |
| Comment | `# Build artifacts` | Ignored line |
| Blank line | | Ignored |

**Exclude entire directory** (common pattern):
```
# .odignore — exclude everything in this directory
*
```

**Selective exclude** (common pattern):
```
# .odignore — exclude build artifacts but keep source
*.o
*.a
build/
dist/
```

The marker filename is configurable via `ignore_marker`:

```toml
[filter]
ignore_marker = ".odignore"    # default
# ignore_marker = ".syncignore"  # alternative name
```

**Scope**: .odignore files apply to the directory they are in and all its descendants. They do NOT apply to parent directories.

**Direction**: .odignore rules apply in both upload and download directions.

### 6.6 Name Validation

OneDrive enforces naming restrictions that differ from POSIX filesystems. These are always applied and cannot be disabled by the user ([sync-algorithm §6.4](sync-algorithm.md)).

| Rule | Details |
|------|---------|
| **Disallowed names** (case-insensitive) | `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9` |
| **Disallowed patterns** | Names starting with `~$`; names containing `_vti_`; `forms` at root level |
| **Invalid characters** | `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `\|` |
| **Invalid formatting** | Leading whitespace, trailing whitespace, trailing dot (`.`) |
| **Path length** | 400 characters maximum (entire path) |
| **Component length** | 255 bytes per path component (filesystem limit) |
| **Newline in filename** | Rejected (OneDrive API cannot handle these) |
| **Control characters** | ASCII 0x00-0x1F and 0x7F rejected |
| **HTML entities** | Patterns like `&#169;` in filenames rejected |

Items failing name validation are skipped with a warning log entry. They are not synced and do not cause fatal errors.

### 6.7 Filter Evaluation Order (Detailed)

During **local scanning** (upload direction):

```
1. Name validation (hardcoded, always applied)
   - OneDrive naming restrictions
   - Invalid characters, control codes, HTML entities
   - Path length check
2. skip_symlinks (if item is a symlink)
   - Broken symlink check (always)
   - Circular symlink check (always)
3. sync_paths allowlist (if configured)
4. skip_dotfiles (if enabled)
5. skip_dirs (directories only)
6. skip_files (files only)
7. max_file_size (files only)
8. .odignore evaluation
```

During **delta processing** (download direction):

```
1. sync_paths allowlist (if configured)
2. skip_dirs (directories only)
3. skip_files (files only)
4. skip_dotfiles (if enabled)
5. max_file_size (files only, using API-reported size)
6. .odignore evaluation
```

If any check excludes an item, subsequent checks are short-circuited.

### 6.8 Stale Files Handling

When filter rules change (patterns added/removed, sync_paths modified, .odignore files changed), files that were previously synced but are now excluded become "stale." The sync engine detects this automatically and handles them safely.

**Detection**: The sync engine stores a hash of the current filter configuration in the `config_snapshot` table ([data-model §8](data-model.md)). On each sync cycle, the current config hash is compared against the stored hash. If they differ, a stale file scan is triggered.

**Behavior when stale files are detected**:

1. The sync engine stops syncing stale files (they are excluded by the new filters)
2. Stale files are NOT auto-deleted (safety-critical invariant)
3. Each stale file is recorded in the `stale_files` ledger with its path, reason, and size
4. The user is nagged about stale files at the broadest level possible:
   - If an entire directory is stale, show one nag for the directory (not for every file inside it)
   - The nag appears on every `sync` run and in `status` output
5. The user must explicitly dispose of stale files

**Stale file disposition commands**:

```bash
# List all stale files
onedrive-go stale list
onedrive-go stale list --profile work

# Delete stale files (moves to OS trash if use_local_trash is enabled)
onedrive-go stale delete [path-or-id]
onedrive-go stale delete --all

# Keep stale files (remove from ledger, file stays on disk, no longer nagged)
onedrive-go stale keep [path-or-id]
onedrive-go stale keep --all
```

**Example nag output**:

```
$ onedrive-go sync
Syncing profile "default" (OneDrive Personal)...
  3 stale items (excluded by filter changes, still on disk):
    ~/OneDrive/old-project/ (247 files, 1.2 GB) — excluded by skip_dirs pattern "old-project"
    ~/OneDrive/notes.tmp — excluded by skip_files pattern "*.tmp"
  Run `onedrive-go stale list` for details, `stale delete` or `stale keep` to resolve.
Sync complete: 5 downloaded, 2 uploaded, 0 conflicts
```

**Rationale** (Decision C3): Auto-deleting files that were previously synced but are now filtered would be surprising and potentially destructive. The user added the filter to stop syncing the files, not to delete them. Explicit disposition gives the user control.

---

## 7. Transfer Options

### 7.1 Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `parallel_downloads` | Integer | `8` | Transfers | Number of concurrent download workers. Range: 1-16. |
| `parallel_uploads` | Integer | `8` | Transfers | Number of concurrent upload workers. Range: 1-16. |
| `parallel_checkers` | Integer | `8` | Transfers | Number of concurrent hash computation workers. Range: 1-16. |
| `chunk_size` | String | `"10MB"` | Transfers | Upload chunk size for resumable upload sessions. Must be a multiple of 320 KiB. Range: 10MB-60MB. Format: human-readable size. |
| `bandwidth_limit` | String | `"0"` | Transfers | Global bandwidth limit. `"0"` = unlimited. Format: `"5MB/s"`, `"500KB/s"`. Applied as a token bucket across all workers. |
| `bandwidth_schedule` | Array of tables | `[]` | Transfers | Time-of-day bandwidth schedule. Overrides `bandwidth_limit` when active. |
| `transfer_order` | String | `"default"` | Transfers | Order for processing transfer queue. Values: `"default"` (FIFO), `"size_asc"` (smallest first), `"size_desc"` (largest first), `"name_asc"` (alphabetical A-Z), `"name_desc"` (alphabetical Z-A). |

### 7.2 Worker Pool Design

Three independent worker pools, each with configurable size ([architecture §5.2](architecture.md)):

| Pool | Default | Max | Purpose |
|------|---------|-----|---------|
| Downloads | 8 | 16 | File downloads from OneDrive |
| Uploads | 8 | 16 | File uploads to OneDrive |
| Checkers | 8 | 16 | Local hash computation for change detection |

The default of 8 aligns with Microsoft Graph API guidance recommending 5-10 concurrent requests. Values above 16 are rejected because higher concurrency provides diminishing returns and increases the risk of HTTP 429 throttling.

### 7.3 Chunk Size Validation

The OneDrive upload API requires fragments to be multiples of 320 KiB (327,680 bytes). The `chunk_size` option is validated at startup:

```
Valid values: 10MB, 15MB, 20MB, 25MB, 30MB, 35MB, 40MB, 45MB, 50MB, 55MB, 60MB
```

These are the values in the 10-60 MB range that are exact multiples of 320 KiB. Values outside this range or that are not multiples of 320 KiB are rejected with an error message listing valid values.

### 7.4 Bandwidth Schedule

The bandwidth schedule allows different bandwidth limits at different times of day, using the local system time:

```toml
[transfers]
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },    # Throttle during work hours
    { time = "18:00", limit = "50MB/s" },    # More bandwidth in evenings
    { time = "23:00", limit = "0" },          # Unlimited overnight
]
```

**Semantics**:
- Times are in 24-hour format, local system time
- Each entry defines the limit that takes effect FROM that time until the next entry
- The schedule wraps around midnight: the last entry's limit applies until the first entry's time the next day
- `"0"` means unlimited
- The schedule is evaluated every minute; changes take effect within 60 seconds
- If `bandwidth_schedule` is non-empty, it overrides `bandwidth_limit` entirely
- An empty schedule means `bandwidth_limit` is always in effect

**Timezone**: Local system time is used. No timezone configuration is needed — the user's system clock determines when schedule entries activate. This avoids confusion from timezone mismatches.

### 7.5 Bandwidth Limit Format

Bandwidth limits use a human-readable format with a required `/s` suffix:

| Format | Meaning |
|--------|---------|
| `"0"` | Unlimited |
| `"100KB/s"` | 100 kilobytes per second |
| `"5MB/s"` | 5 megabytes per second |
| `"1GB/s"` | 1 gigabyte per second |

The limit is applied globally across all workers via a shared token bucket. Individual workers draw tokens from the bucket before each chunk transfer.

---

## 8. Safety Options

### 8.1 Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `big_delete_threshold` | Integer | `1000` | Safety | Abort if a sync would delete more than this many items. |
| `big_delete_percentage` | Integer | `50` | Safety | Abort if a sync would delete more than this percentage of total items. |
| `big_delete_min_items` | Integer | `10` | Safety | Skip big-delete check if total tracked items is below this count. Prevents false positives on small drives. |
| `min_free_space` | String | `"1GB"` | Safety | Minimum free disk space. Downloads are skipped if free space would drop below this. Format: human-readable size. |
| `use_recycle_bin` | Boolean | `true` | Safety | When deleting items on OneDrive (due to local deletion propagation), send to the OneDrive recycle bin instead of permanent deletion. |
| `use_local_trash` | Boolean | `true` | Safety | When deleting local files (due to remote deletion), move to OS trash instead of permanent deletion. Linux: FreeDesktop.org Trash spec. macOS: Finder Trash. |
| `disable_download_validation` | Boolean | `false` | Safety | Skip QuickXorHash verification after downloads. Use only as a workaround for SharePoint libraries that modify files server-side (enrichment). |
| `disable_upload_validation` | Boolean | `false` | Safety | Skip hash verification after uploads. Use only as a workaround for SharePoint libraries that modify files post-upload. |
| `sync_dir_permissions` | String | `"0700"` | Safety | POSIX permission mode for directories created during sync. Octal format as a string. |
| `sync_file_permissions` | String | `"0600"` | Safety | POSIX permission mode for files created during sync. Octal format as a string. |
| `tombstone_retention_days` | Integer | `30` | Safety | Days to keep tombstone records in the state DB for move detection. Longer retention improves move detection across long offline periods. |

### 8.2 Big-Delete Protection

Big-delete protection uses **OR logic**: if EITHER the absolute count OR the percentage threshold is exceeded, the sync is aborted ([sync-algorithm §8.1](sync-algorithm.md)).

```
Big delete triggered if:
    (totalItems >= big_delete_min_items) AND
    (deleteCount > big_delete_threshold OR deletePercentage > big_delete_percentage)
```

The `big_delete_min_items` guard prevents false positives on small drives. Deleting 3 of 5 files (60%) should not trigger protection if the user intentionally deleted them.

When triggered:

```
ERROR: This sync would delete 2,847 files (57% of 4,995 total).
This exceeds the big-delete threshold (1000 items OR 50%).
Run with --force to proceed, or adjust safety thresholds in config.
```

### 8.3 Permissions

Permissions are specified as octal strings to avoid TOML integer parsing ambiguity:

```toml
[safety]
sync_dir_permissions = "0700"    # drwx------
sync_file_permissions = "0600"   # -rw-------
```

These permissions are applied to files and directories **created** by the sync engine. Existing files are not modified. The default values (0700/0600) restrict access to the file owner only, matching the security principle of least privilege.

### 8.4 Validation Bypass

The `disable_download_validation` and `disable_upload_validation` options are **last-resort escape hatches only**. The primary mechanism for handling SharePoint enrichment is per-side hash baselines in the three-way merge: the sync engine records separate `local_sha256` and `remote_quick_xor_hash` baselines so that server-side modifications (enrichment) are detected and handled automatically without disabling validation. See [SHAREPOINT_ENRICHMENT.md](../../SHAREPOINT_ENRICHMENT.md) for the per-side baseline design that handles enrichment automatically.

These options exist solely as workarounds for edge cases that the per-side baseline approach cannot cover:

- **Azure Information Protection**: AIP-protected files may report different sizes than actual content
- **Unrecognized server-side transforms**: Future SharePoint behaviors not yet covered by per-side baselines

These options should NEVER be enabled unless the user encounters validation failures that persist even with per-side baselines active. When enabled, a warning is logged on every sync:

```
WARN: Download validation disabled. Data integrity cannot be guaranteed.
```

---

## 9. Logging Options

### 9.1 Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `log_level` | String | `"info"` | Logging | Minimum log level. Values: `"debug"`, `"info"`, `"warn"`, `"error"`. |
| `log_file` | String | `""` | Logging | Explicit log file path. Empty string = automatic path (see below). |
| `log_format` | String | `"auto"` | Logging | Log format. `"text"` = human-readable, `"json"` = structured JSON, `"auto"` = text if interactive TTY, json if `--quiet` or non-interactive. |
| `log_retention_days` | Integer | `30` | Logging | Days to keep log files before automatic cleanup. Minimum: 1. |

### 9.2 Always-On File Logging

File logging is **always enabled**, regardless of mode ([architecture §14.3](architecture.md)):

| Mode | stderr Output | Log File Output |
|------|--------------|----------------|
| Interactive | Text format, human-readable | JSON format, all levels |
| `--quiet` | Suppressed (errors only) | JSON format, all levels |
| `--debug` | Trace-level text | Trace-level JSON |

**Automatic log file location** (when `log_file` is empty):

| Platform | Log Directory | File Pattern |
|----------|--------------|-------------|
| Linux | `~/.local/share/onedrive-go/logs/` | `onedrive-go-2026-02-17.log` |
| macOS | `~/Library/Application Support/onedrive-go/logs/` | `onedrive-go-2026-02-17.log` |

Log files are daily files. A new file is created at midnight (local time). Old files are automatically deleted after `log_retention_days`.

### 9.3 Token Scrubbing

Bearer tokens and pre-authenticated URLs are NEVER written to log files, even at debug level ([architecture §9.2](architecture.md)):

- `Authorization: Bearer ...` headers are replaced with `Authorization: Bearer [REDACTED]`
- Pre-authenticated download URLs are truncated to show only the path (query parameters containing tokens are removed)
- Upload session URLs are similarly truncated
- This scrubbing happens at the logging layer, before any output

### 9.4 Structured Log Fields

Every log entry includes structured fields for filtering and analysis ([architecture §14.4](architecture.md)):

| Field | When Present | Example |
|-------|-------------|---------|
| `profile` | Always | `"default"` |
| `drive` | During sync | `"abc123def456"` |
| `path` | Item operations | `"/Documents/report.pdf"` |
| `op` | Transfer/delete | `"download"`, `"upload"`, `"delete"` |
| `duration` | After operation | `"2.3s"` |
| `size` | File operations | `2457600` |
| `error` | Warnings/errors | `"connection timeout"` |

---

## 10. Network Options

### 10.1 Options

| Option | TOML Type | Default | Scope | Description |
|--------|-----------|---------|-------|-------------|
| `connect_timeout` | Duration | `"10s"` | Network | TCP connection timeout for HTTPS connections. |
| `data_timeout` | Duration | `"60s"` | Network | Timeout for receiving data on an active connection. If no data is received for this duration, the connection is dropped and the operation retried. |
| `user_agent` | String | `""` | Network | Custom User-Agent header. Empty string uses the default ISV format: `"ISV\|tonimelisma\|onedrive-go/vX.Y.Z"`. The ISV format is recommended by Microsoft for traffic classification. |
| `force_http_11` | Boolean | `false` | Network | Force HTTP/1.1 instead of HTTP/2. Use only if experiencing HTTP/2-related issues with corporate proxies or network equipment. |

### 10.2 User-Agent Format

The default User-Agent follows Microsoft's ISV traffic decoration requirements:

```
ISV|tonimelisma|onedrive-go/v0.1.0
```

Format: `ISV|<publisher>|<application>/<version>`

This format helps Microsoft distinguish ISV (Independent Software Vendor) traffic from malicious or unauthorized API usage. Changing this may affect how Microsoft classifies and throttles the client's traffic. Custom user agents should follow the same ISV format.

---

## 11. CLI Flag Reference

### 11.1 Global Flags

These flags apply to all commands:

| Flag | Config Key | Type | Description |
|------|-----------|------|-------------|
| `--profile NAME` | (env: `ONEDRIVE_GO_PROFILE`) | String | Select account profile. Default: `"default"` |
| `--config PATH` | (env: `ONEDRIVE_GO_CONFIG`) | String | Override config file path |
| `--json` | (none) | Boolean | Machine-readable JSON output |
| `--verbose` / `-v` | (none) | Boolean | Verbose output. Stackable: `-vv` for debug level |
| `--quiet` / `-q` | (none) | Boolean | Suppress non-error output |
| `--dry-run` | `sync.dry_run` | Boolean | Preview operations without executing |
| `--debug` | (none) | Boolean | Trace-level logging |

### 11.2 Sync Flags

| Flag | Config Key | Description |
|------|-----------|-------------|
| `--watch` | (CLI-only) | Continuous sync mode |
| `--download-only` | (CLI-only) | Download remote changes only |
| `--upload-only` | (CLI-only) | Upload local changes only |
| `--cleanup-local` | (CLI-only) | Delete local files removed remotely (with `--download-only`) |
| `--no-remote-delete` | (CLI-only) | Do not propagate local deletes remotely (with `--upload-only`) |
| `--force` | (CLI-only) | Proceed past big-delete protection |
| `--sync-dir PATH` | `profile.NAME.sync_dir` | Override sync directory |

### 11.3 Complete Flag-to-Config Mapping

| CLI Flag | Config Key | Override Behavior |
|----------|-----------|-------------------|
| `--profile NAME` | (profile selection) | Selects active profile |
| `--config PATH` | (file path) | Overrides config file location |
| `--dry-run` | `sync.dry_run` | CLI replaces config |
| `--sync-dir PATH` | `profile.NAME.sync_dir` | CLI replaces config |
| `--force` | (CLI-only) | No config equivalent |
| `--watch` | (CLI-only) | No config equivalent |
| `--download-only` | (CLI-only) | No config equivalent |
| `--upload-only` | (CLI-only) | No config equivalent |
| `--cleanup-local` | (CLI-only) | No config equivalent |
| `--no-remote-delete` | (CLI-only) | No config equivalent |
| `--verbose` / `-v` | (CLI-only) | Overrides log_level for stderr |
| `--quiet` / `-q` | (CLI-only) | Suppresses stderr |
| `--debug` | (CLI-only) | Overrides log_level to trace |
| `--json` | (CLI-only) | Changes output format |
| `--headless` | (CLI-only) | Forces device code auth flow |

### 11.4 Config-Only Options

These options have NO CLI flag equivalent and can only be set in the config file:

| Config Key | Section | Reason |
|-----------|---------|--------|
| `application_id` | Profile | Rarely changed, security-sensitive |
| `account_type` | Profile | Set during config init, rarely changed |
| `azure_ad_endpoint` | Profile | Enterprise-only, set during config init |
| `azure_tenant_id` | Profile | Enterprise-only, set during config init |
| `drive_id` | Profile | Set during config init, rarely changed |
| `remote_path` | Profile | Set during config init, rarely changed |
| `ignore_marker` | Filter | Rarely changed |
| `bandwidth_schedule` | Transfers | Complex structure, impractical as CLI flag |
| `transfer_order` | Transfers | Rarely changed at runtime |
| `sync_dir_permissions` | Safety | Rarely changed |
| `sync_file_permissions` | Safety | Rarely changed |
| `tombstone_retention_days` | Safety | Rarely changed |
| `disable_download_validation` | Safety | Dangerous, should require deliberate config edit |
| `disable_upload_validation` | Safety | Dangerous, should require deliberate config edit |
| `connect_timeout` | Network | Rarely changed |
| `data_timeout` | Network | Rarely changed |
| `user_agent` | Network | Rarely changed |
| `force_http_11` | Network | Rarely changed |
| `log_file` | Logging | Rarely changed at runtime |
| `log_retention_days` | Logging | Rarely changed |
| `websocket` | Sync | Rarely changed |
| `verify_interval` | Sync | Rarely changed |
| `shutdown_timeout` | Sync | Rarely changed |

### 11.5 Override Semantics

CLI flags **replace** config values entirely. There is no merging:

```bash
# Config file has: skip_files = ["~*", "*.tmp", "*.partial"]
# CLI overrides the entire array — config values are NOT merged
onedrive-go sync --skip-files '["*.log"]'
# Effective: skip_files = ["*.log"] (config values gone)
```

This matches the convention established by other OneDrive sync tools and avoids the complexity and confusion of merge semantics.

---

## 12. Config Init Wizard

### 12.1 Overview

`onedrive-go config init` runs an interactive setup wizard that creates a working `config.toml`. The wizard guides the user through authentication, account configuration, and basic filtering setup.

### 12.2 Step-by-Step Flow

```
$ onedrive-go config init

Welcome to onedrive-go setup!

Step 1: Authentication
  Opening browser for Microsoft authentication...
  (Or use --headless for device code flow)
  Authenticated as: user@example.com

Step 2: Account Type
  Detected account type: OneDrive Personal
  [1] OneDrive Personal (user@example.com)
  [2] OneDrive Business (if available)
  [3] SharePoint Document Library
  Selection: 1

Step 3: Profile Name
  Profile name [default]: default

Step 4: Sync Directory
  Local sync directory [~/OneDrive]: ~/OneDrive
  Directory does not exist. Create it? [Y/n]: Y

Step 5: Selective Sync (optional)
  Sync all files and folders? [Y/n]: Y
  (Or specify paths: /Documents, /Photos, etc.)

Step 6: Basic Filters
  Skip dotfiles (.hidden files)? [y/N]: N
  Skip common temp files (~*, *.tmp, *.partial)? [Y/n]: Y
  Additional directories to skip (comma-separated, or empty):
    > node_modules, .git, __pycache__

Step 7: Write Configuration
  Writing config to ~/.config/onedrive-go/config.toml
  Configuration saved!

Next steps:
  onedrive-go sync              # Run initial sync
  onedrive-go sync --watch      # Start continuous sync
  onedrive-go config show       # View current configuration
```

### 12.3 Auto-Detection of Existing Tools

During config init, the wizard checks for existing sync tool configurations:

**abraunegg/onedrive detection**:
- Check for `~/.config/onedrive/config` (user config)
- Check for `/etc/onedrive/config` (system config)
- Check for `~/.config/onedrive/sync_list` (selective sync)
- Check for running `onedrive` process

**rclone detection**:
- Check for `~/.config/rclone/rclone.conf`
- Look for remotes with `type = onedrive`
- Check for running `rclone` process with OneDrive remote

If either is detected:

```
Detected existing abraunegg/onedrive configuration at ~/.config/onedrive/config
Would you like to import settings? [Y/n]: Y
  Imported sync_dir: ~/OneDrive
  Imported skip_file patterns: ~*, *.tmp, *.partial
  Imported skip_dir patterns: node_modules
  Imported monitor_interval as poll_interval: 5m
  Note: Authentication tokens cannot be migrated. You will need to log in again.
  Note: Sync state database cannot be migrated. An initial full sync will be required.

WARNING: abraunegg/onedrive process is running (PID 12345).
Stop it before starting onedrive-go sync to avoid conflicts.
```

### 12.4 Migration Mapping (abraunegg)

The wizard maps abraunegg config options to onedrive-go equivalents:

| abraunegg Option | onedrive-go Option | Notes |
|-----------------|-------------------|-------|
| `sync_dir` | `profile.NAME.sync_dir` | Direct mapping |
| `skip_file` | `filter.skip_files` | Pipe-delimited string to TOML array |
| `skip_dir` | `filter.skip_dirs` | Pipe-delimited string to TOML array |
| `skip_dotfiles` | `filter.skip_dotfiles` | Direct mapping |
| `skip_symlinks` | `filter.skip_symlinks` | Direct mapping |
| `skip_size` | `filter.max_file_size` | MB integer to human-readable string |
| `sync_list` | `filter.sync_paths` | Line-per-path file to TOML array |
| `threads` | `transfers.parallel_downloads` + `transfers.parallel_uploads` | Single value split to separate pools |
| `rate_limit` | `transfers.bandwidth_limit` | Bytes/s to human-readable format |
| `file_fragment_size` | `transfers.chunk_size` | MB integer to human-readable string |
| `transfer_order` | `transfers.transfer_order` | `size_dsc` renamed to `size_desc` |
| `monitor_interval` | `sync.poll_interval` | Seconds integer to duration string |
| `monitor_fullscan_frequency` | `sync.fullscan_frequency` | Direct mapping |
| `classify_as_big_delete` | `safety.big_delete_threshold` | Direct mapping |
| `space_reservation` | `safety.min_free_space` | MB integer to human-readable string |
| `use_recycle_bin` | `safety.use_recycle_bin` | Direct mapping |
| `sync_dir_permissions` | `safety.sync_dir_permissions` | Octal int to octal string |
| `sync_file_permissions` | `safety.sync_file_permissions` | Octal int to octal string |
| `connect_timeout` | `network.connect_timeout` | Seconds int to duration string |
| `data_timeout` | `network.data_timeout` | Seconds int to duration string |
| `force_http_11` | `network.force_http_11` | Direct mapping |
| `user_agent` | `network.user_agent` | Direct mapping (but user should update ISV format) |
| `drive_id` | `profile.NAME.drive_id` | Direct mapping |
| `application_id` | `profile.NAME.application_id` | Direct mapping |
| `azure_ad_endpoint` | `profile.NAME.azure_ad_endpoint` | Direct mapping |
| `azure_tenant_id` | `profile.NAME.azure_tenant_id` | Direct mapping |
| `disable_download_validation` | `safety.disable_download_validation` | Direct mapping |
| `disable_upload_validation` | `safety.disable_upload_validation` | Direct mapping |
| `enable_logging` + `log_dir` | `logging.log_file` | Combine enable flag and path |
| `dry_run` | `sync.dry_run` | Direct mapping |
| `disable_websocket_support` | `sync.websocket` (inverted) | Negated boolean |
| `download_only` | (noted in output) | CLI flag, not config |
| `upload_only` | (noted in output) | CLI flag, not config |
| `no_remote_delete` | (noted in output) | CLI flag, not config |
| `local_first` | (rejected) | No equivalent; our three-way merge handles this differently |
| `bypass_data_preservation` | (rejected) | Unsafe; not supported |
| `remove_source_files` | (rejected) | Dangerous; not supported |
| `remove_source_folders` | (rejected) | Dangerous; not supported |
| `permanent_delete` | (rejected) | Dangerous; use recycle bin instead |
| `check_nomount` | (rejected) | Handled via systemd mount dependencies |
| `check_nosync` | (rejected) | Use .odignore instead |
| `create_new_file_version` | (rejected) | SharePoint enrichment handled differently |
| `force_session_upload` | (rejected) | Always use session upload for files > 4MB |
| `cleanup_local_files` | (noted in output) | CLI flag `--cleanup-local` |
| `read_only_auth_scope` | (rejected) | Not supported at MVP |
| `use_device_auth` | (noted in output) | CLI flag `--headless` |
| `use_intune_sso` | (rejected) | Not supported |
| `sync_root_files` | (rejected) | sync_paths semantics handle this implicitly |
| `sync_business_shared_items` | (rejected) | Post-MVP feature |
| `webhook_*` | (rejected) | We use built-in WebSocket, not HTTP webhooks |
| `display_*` / `debug_*` | (rejected) | Use `--verbose`, `--debug`, `--json` instead |
| `notify_*` / `disable_notifications` | (rejected) | Desktop notifications not in scope |
| `display_manager_integration` | (rejected) | File manager integration not in scope |
| `write_xattr_data` | (rejected) | Not supported |
| `inotify_delay` / `delay_inotify_processing` | (rejected) | Handled by 2-second debounce |
| `ip_protocol_version` | (rejected) | Go handles this automatically |
| `dns_timeout` | (rejected) | Go handles this automatically |
| `operation_timeout` | (rejected) | Use data_timeout instead |
| `max_curl_idle` | (rejected) | Go HTTP client manages connection pooling |
| `recycle_bin_path` | (rejected) | OS trash location auto-detected |

### 12.5 Migration Mapping (rclone)

| rclone Config Key | onedrive-go Option | Notes |
|------------------|-------------------|-------|
| `[remote-name]` | `profile.NAME` | Remote name becomes profile name |
| `type = onedrive` | (validation) | Confirms this is a OneDrive remote |
| `drive_id` | `profile.NAME.drive_id` | Direct mapping |
| `drive_type` | `profile.NAME.account_type` | `personal` / `business` / `documentLibrary` mapped |
| `region` | `profile.NAME.azure_ad_endpoint` | `global`/`us`/`de`/`cn` mapped |
| `token` | (not migrated) | Different OAuth app ID; must re-authenticate |
| rclone filter flags | `filter.*` | Best-effort conversion of `--include`/`--exclude` patterns |
| `--transfers` | `transfers.parallel_downloads` + `transfers.parallel_uploads` | Split to separate pools |
| `--checkers` | `transfers.parallel_checkers` | Direct mapping |
| `--bwlimit` | `transfers.bandwidth_limit` | Direct format mapping |
| `--bwlimit-file` | (rejected) | Per-file limits not supported |

### 12.6 Config Validation on Write

The wizard validates the generated config before writing:

1. TOML syntax is valid
2. All required fields are present (`account_type`, `sync_dir`)
3. `sync_dir` path is valid and parent directory exists
4. `drive_id` is present for SharePoint profiles
5. Filter patterns are valid (no empty patterns, no bare `*` in skip_dirs)
6. Numeric ranges are within bounds
7. No cross-field conflicts (e.g., `azure_ad_endpoint` without `azure_tenant_id`)

### 12.7 Detection of Running Instances

Before writing config, the wizard checks for running instances of competing sync tools:

```bash
# Check for running abraunegg
pgrep -f '/usr/bin/onedrive' || pgrep -f 'onedrive --monitor'

# Check for running rclone with OneDrive
pgrep -f 'rclone.*onedrive'

# Check for running onedrive-go
pgrep -f 'onedrive-go sync'
```

If any are found, a warning is displayed:

```
WARNING: abraunegg/onedrive is currently running (PID 12345).
Running two sync tools against the same OneDrive account simultaneously
may cause conflicts, duplicate files, and data corruption.
Stop the other tool before starting onedrive-go sync.
```

---

## 13. Config Validation

### 13.1 Validation Rules

Every option is validated at startup. The application refuses to start if any validation fails.

| Option | Type | Range/Constraint | Dependencies |
|--------|------|-----------------|-------------|
| `account_type` | Enum | `personal`, `business`, `sharepoint` | Required for each profile |
| `sync_dir` | Path | Must be absolute after tilde expansion | Required for each profile |
| `remote_path` | Path | Must start with `/` | - |
| `drive_id` | String | Non-empty when present | Required if `account_type = "sharepoint"` |
| `application_id` | String | Non-empty when present | - |
| `azure_ad_endpoint` | Enum | `""`, `USL4`, `USL5`, `DE`, `CN` | Requires `azure_tenant_id` |
| `azure_tenant_id` | String | Non-empty when present | Required if `azure_ad_endpoint` is set |
| `skip_files` | Array | Valid patterns, no empty strings | - |
| `skip_dirs` | Array | Valid patterns, no empty strings, not `.*` | - |
| `skip_dotfiles` | Boolean | - | - |
| `skip_symlinks` | Boolean | - | - |
| `max_file_size` | Size | >= 0, valid size string | - |
| `sync_paths` | Array | Each path starts with `/` | - |
| `ignore_marker` | String | Non-empty, valid filename | - |
| `parallel_downloads` | Integer | 1-16 | - |
| `parallel_uploads` | Integer | 1-16 | - |
| `parallel_checkers` | Integer | 1-16 | - |
| `chunk_size` | Size | 10MB-60MB, multiple of 320KiB | - |
| `bandwidth_limit` | BW | `"0"` or valid rate string | - |
| `bandwidth_schedule` | Array | Valid time format, valid limit | - |
| `transfer_order` | Enum | `default`, `size_asc`, `size_desc`, `name_asc`, `name_desc` | - |
| `big_delete_threshold` | Integer | >= 1 | - |
| `big_delete_percentage` | Integer | 1-100 | - |
| `big_delete_min_items` | Integer | >= 1 | - |
| `min_free_space` | Size | >= 0, valid size string | - |
| `use_recycle_bin` | Boolean | - | - |
| `use_local_trash` | Boolean | - | - |
| `disable_download_validation` | Boolean | - | Warning if true |
| `disable_upload_validation` | Boolean | - | Warning if true |
| `sync_dir_permissions` | String | Valid octal (3-4 digits) | - |
| `sync_file_permissions` | String | Valid octal (3-4 digits) | - |
| `tombstone_retention_days` | Integer | >= 1 | - |
| `poll_interval` | Duration | >= 5m | - |
| `fullscan_frequency` | Integer | 0 or >= 2 | - |
| `websocket` | Boolean | - | - |
| `conflict_strategy` | Enum | `keep_both` | - |
| `conflict_reminder_interval` | Duration | >= 0 | - |
| `dry_run` | Boolean | - | - |
| `verify_interval` | Duration | >= 0 | - |
| `shutdown_timeout` | Duration | >= 5s | - |
| `log_level` | Enum | `debug`, `info`, `warn`, `error` | - |
| `log_file` | Path | Valid path when non-empty | - |
| `log_format` | Enum | `text`, `json`, `auto` | - |
| `log_retention_days` | Integer | >= 1 | - |
| `connect_timeout` | Duration | >= 1s | - |
| `data_timeout` | Duration | >= 5s | - |
| `user_agent` | String | Non-empty when set | - |
| `force_http_11` | Boolean | - | - |

### 13.2 Cross-Field Validation

| Rule | Error Message |
|------|--------------|
| `azure_ad_endpoint` set but `azure_tenant_id` empty | `azure_ad_endpoint requires azure_tenant_id to be set` |
| `drive_id` empty for `account_type = "sharepoint"` | `SharePoint profiles require drive_id` |
| `disable_download_validation = true` | Warning: `Download validation disabled. Data integrity cannot be guaranteed.` |
| `disable_upload_validation = true` | Warning: `Upload validation disabled. Post-upload corruption cannot be detected.` |
| Multiple profiles with the same `sync_dir` | `Profiles "foo" and "bar" have the same sync_dir — this will cause conflicts` |
| `bandwidth_schedule` entries not in chronological order | Warning: `bandwidth_schedule entries should be in chronological order for clarity` |
| `big_delete_min_items` > `big_delete_threshold` | Warning: `big_delete_min_items exceeds big_delete_threshold — big-delete protection will never trigger` |

### 13.3 Shadow Validation for Filters

At startup, the filter engine validates that `sync_paths` entries are not completely shadowed by `skip_dirs` or `skip_files` patterns (shadow validation):

```
function ValidateFilterShadows(config):
    for path in config.SyncPaths:
        if matchesSkipDirs(path):
            error("sync_paths entry %q is shadowed by skip_dirs pattern — it will never sync", path)
        if matchesSkipFiles(basename(path)):
            error("sync_paths entry %q is shadowed by skip_files pattern — it will never sync", path)
```

This prevents configurations where the user has specified a path to sync but a filter pattern excludes it, which would be silently confusing.

### 13.4 Startup Validation Sequence

The complete validation sequence at application startup:

```
1. Load config file (TOML parse)
     - Reject malformed TOML with parse error + line number
2. Check for unknown keys
     - Fatal error with Levenshtein-based suggestion
3. Profile validation
     - At least one profile exists
     - Required fields present
     - No duplicate sync_dirs
4. Type validation
     - All values are correct TOML types
     - Enums are valid values
     - Ranges are within bounds
5. Cross-field validation
     - Dependency checks (azure_ad_endpoint requires azure_tenant_id)
6. Filter shadow validation
     - sync_paths not shadowed by skip patterns
7. Warnings (non-fatal)
     - Validation bypass options enabled
     - Unusual configurations
8. Profile-specific resolution
     - Resolve per-profile overrides
     - Apply defaults for missing sections
```

### 13.5 Unknown Key Detection

Unknown keys are detected using the TOML library's strict decoding mode. When an unknown key is found:

1. Identify the section containing the unknown key
2. Collect all known keys in that section
3. Compute Levenshtein distance between the unknown key and each known key
4. If any distance is <= 3, suggest the closest match
5. Emit a fatal error with the suggestion

```
Error: unknown config key "parralel_downloads" in [transfers]
Did you mean "parallel_downloads"?

Error: unknown config key "skip_file" in [filter]
Did you mean "skip_files"? (note: arrays use plural names in onedrive-go)

Error: unknown config key "webhook_enabled" in top-level
This option from abraunegg/onedrive is not supported. onedrive-go uses
built-in WebSocket support (sync.websocket option) instead.
```

For keys that are recognized as abraunegg or rclone option names, a specific migration hint is provided.

---

## 14. Hot Reload (SIGHUP)

### 14.1 Overview

In `--watch` mode, sending SIGHUP to the process reloads the configuration file without stopping the sync engine ([sync-algorithm §11.5](sync-algorithm.md)). This allows changing runtime settings without downtime.

### 14.2 Hot-Reloadable Options

The following options take effect immediately after SIGHUP:

| Section | Options | Effect |
|---------|---------|--------|
| **Filter** | `skip_files`, `skip_dirs`, `skip_dotfiles`, `skip_symlinks`, `max_file_size`, `sync_paths`, `ignore_marker` | Filter engine re-initialized. Stale file detection triggered. |
| **Transfers** | `bandwidth_limit`, `bandwidth_schedule`, `transfer_order` | Bandwidth scheduler updated. Next transfer uses new settings. |
| **Sync** | `poll_interval`, `fullscan_frequency`, `conflict_reminder_interval`, `websocket` | Timers and schedulers updated. |
| **Logging** | `log_level`, `log_format` | Log output updated immediately. |
| **Safety** | `big_delete_threshold`, `big_delete_percentage`, `big_delete_min_items`, `min_free_space` | Safety checks updated for next sync cycle. |

### 14.3 Non-Reloadable Options (Require Restart)

These options require stopping and restarting the `sync --watch` process:

| Section | Options | Reason |
|---------|---------|--------|
| **Profile** | `account_type` | Determines API behavior, quirk handling, and authentication |
| **Profile** | `sync_dir` | Requires re-initializing filesystem watcher and potentially a resync |
| **Profile** | `drive_id` | Requires re-initializing delta token and potentially a resync |
| **Profile** | `remote_path` | Requires re-initializing delta token |
| **Profile** | `application_id`, `azure_ad_endpoint`, `azure_tenant_id` | Requires re-authentication |
| **Transfers** | `parallel_downloads`, `parallel_uploads`, `parallel_checkers`, `chunk_size` | Worker pools are initialized at startup |
| **Safety** | `sync_dir_permissions`, `sync_file_permissions` | Applied at file creation time |
| **Safety** | `tombstone_retention_days` | Affects DB cleanup, safe to wait for restart |
| **Network** | `connect_timeout`, `data_timeout`, `force_http_11`, `user_agent` | HTTP client initialized at startup |
| **Logging** | `log_file`, `log_retention_days` | File handle management |

When a non-reloadable option is changed, SIGHUP logs a warning:

```
WARN: Config option "sync_dir" changed but requires restart to take effect.
WARN: Config option "parallel_downloads" changed but requires restart to take effect.
```

### 14.4 Filter Changes on Reload

When filter rules change during a hot reload:

1. The filter engine is re-initialized with the new config
2. The new config hash is computed and compared against the stored `config_snapshot`
3. If filters changed, a stale file scan is triggered ([§6.8](#68-stale-files-handling))
4. Newly stale files are added to the stale files ledger
5. The user is nagged about stale files on the next sync cycle

This is the same detection mechanism used at startup ([sync-algorithm §6.3](sync-algorithm.md)), ensuring consistent behavior.

### 14.5 Bandwidth Schedule on Reload

Changes to `bandwidth_schedule` take effect immediately. The bandwidth scheduler checks the current time against the new schedule and applies the appropriate limit. No in-flight transfers are interrupted; the new limit applies to the next chunk transfer.

---

## Appendix A: Complete Options Reference Table

Every configuration option in a single reference table.

| Option | Section | TOML Type | Default | CLI Flag | Validation | Hot-Reload | Description |
|--------|---------|-----------|---------|----------|-----------|:----------:|-------------|
| `account_type` | Profile | String | (required) | - | `personal`/`business`/`sharepoint` | No | Account type |
| `sync_dir` | Profile | String | `"~/OneDrive"` | `--sync-dir` | Absolute path | No | Local sync directory |
| `remote_path` | Profile | String | `"/"` | - | Starts with `/` | No | Remote path to sync |
| `drive_id` | Profile | String | `""` | - | Non-empty for SharePoint | No | Drive ID |
| `application_id` | Profile | String | (built-in) | - | Non-empty when set | No | Azure app ID |
| `azure_ad_endpoint` | Profile | String | `""` | - | `""`,`USL4`,`USL5`,`DE`,`CN` | No | National cloud |
| `azure_tenant_id` | Profile | String | `""` | - | Required with azure_ad_endpoint | No | Azure AD tenant |
| `skip_files` | Filter | Array | `["~*",".~*","*.tmp","*.swp","*.partial","*.crdownload",".DS_Store","Thumbs.db"]` | - | Valid patterns | Yes | File exclusion patterns |
| `skip_dirs` | Filter | Array | `[]` | - | Valid patterns, not `.*` | Yes | Dir exclusion patterns |
| `skip_dotfiles` | Filter | Boolean | `false` | - | - | Yes | Skip dotfiles |
| `skip_symlinks` | Filter | Boolean | `false` | - | - | Yes | Skip symlinks |
| `max_file_size` | Filter | String | `"0"` | - | Valid size, >= 0 | Yes | Max file size |
| `sync_paths` | Filter | Array | `[]` | - | Each starts with `/` | Yes | Selective sync paths |
| `ignore_marker` | Filter | String | `".odignore"` | - | Valid filename | Yes | Ignore marker name |
| `parallel_downloads` | Transfers | Integer | `8` | - | 1-16 | No | Download workers |
| `parallel_uploads` | Transfers | Integer | `8` | - | 1-16 | No | Upload workers |
| `parallel_checkers` | Transfers | Integer | `8` | - | 1-16 | No | Hash check workers |
| `chunk_size` | Transfers | String | `"10MB"` | - | 10-60MB, 320KiB multiple | No | Upload chunk size |
| `bandwidth_limit` | Transfers | String | `"0"` | - | `"0"` or valid rate | Yes | Bandwidth limit |
| `bandwidth_schedule` | Transfers | Array | `[]` | - | Valid times and limits | Yes | Time-of-day bandwidth |
| `transfer_order` | Transfers | String | `"default"` | - | `default`,`size_asc`,`size_desc`,`name_asc`,`name_desc` | Yes | Transfer processing order |
| `big_delete_threshold` | Safety | Integer | `1000` | - | >= 1 | Yes | Big-delete count limit |
| `big_delete_percentage` | Safety | Integer | `50` | - | 1-100 | Yes | Big-delete % limit |
| `big_delete_min_items` | Safety | Integer | `10` | - | >= 1 | Yes | Big-delete min items |
| `min_free_space` | Safety | String | `"1GB"` | - | Valid size, >= 0 | Yes | Min free disk space |
| `use_recycle_bin` | Safety | Boolean | `true` | - | - | Yes | Remote: use recycle bin |
| `use_local_trash` | Safety | Boolean | `true` | - | - | Yes | Local: use OS trash |
| `disable_download_validation` | Safety | Boolean | `false` | - | Warning if true | Yes | Skip download hash check |
| `disable_upload_validation` | Safety | Boolean | `false` | - | Warning if true | Yes | Skip upload hash check |
| `sync_dir_permissions` | Safety | String | `"0700"` | - | Valid octal | No | Dir permissions |
| `sync_file_permissions` | Safety | String | `"0600"` | - | Valid octal | No | File permissions |
| `tombstone_retention_days` | Safety | Integer | `30` | - | >= 1 | No | Tombstone retention |
| `poll_interval` | Sync | Duration | `"5m"` | - | >= 5m | Yes | Polling interval |
| `fullscan_frequency` | Sync | Integer | `12` | - | 0 or >= 2 | Yes | Full scan frequency |
| `websocket` | Sync | Boolean | `true` | - | - | Yes | WebSocket support |
| `conflict_strategy` | Sync | String | `"keep_both"` | - | `keep_both` | Yes | Conflict resolution |
| `conflict_reminder_interval` | Sync | Duration | `"1h"` | - | >= 0 | Yes | Conflict nag interval |
| `dry_run` | Sync | Boolean | `false` | `--dry-run` | - | Yes | Dry-run mode |
| `verify_interval` | Sync | Duration | `"0"` | - | >= 0 | No | Periodic verify interval |
| `shutdown_timeout` | Sync | Duration | `"30s"` | - | >= 5s | No | Shutdown drain timeout |
| `log_level` | Logging | String | `"info"` | `-v`/`--debug` | `debug`,`info`,`warn`,`error` | Yes | Log level |
| `log_file` | Logging | String | `""` | - | Valid path | No | Log file path |
| `log_format` | Logging | String | `"auto"` | - | `text`,`json`,`auto` | Yes | Log format |
| `log_retention_days` | Logging | Integer | `30` | - | >= 1 | No | Log file retention |
| `connect_timeout` | Network | Duration | `"10s"` | - | >= 1s | No | TCP connect timeout |
| `data_timeout` | Network | Duration | `"60s"` | - | >= 5s | No | Data receive timeout |
| `user_agent` | Network | String | `""` | - | Non-empty when set | No | User-Agent header |
| `force_http_11` | Network | Boolean | `false` | - | - | No | Force HTTP/1.1 |

---

## Appendix B: Migration Mapping Tables

### B.1 abraunegg/onedrive to onedrive-go

Complete mapping of configuration options from abraunegg/onedrive to onedrive-go.

#### Adopted (with mapping)

| abraunegg Option | onedrive-go Option | Transformation |
|-----------------|-------------------|----------------|
| `sync_dir` | `profile.NAME.sync_dir` | Direct |
| `skip_file` | `filter.skip_files` | Pipe-delimited string to TOML array |
| `skip_dir` | `filter.skip_dirs` | Pipe-delimited string to TOML array |
| `skip_dotfiles` | `filter.skip_dotfiles` | Direct |
| `skip_symlinks` | `filter.skip_symlinks` | Direct |
| `skip_size` | `filter.max_file_size` | `50` (MB) to `"50MB"` |
| `threads` | `transfers.parallel_downloads` + `transfers.parallel_uploads` + `transfers.parallel_checkers` | One value to three (all get same value) |
| `rate_limit` | `transfers.bandwidth_limit` | Bytes/s to `"NMB/s"` |
| `file_fragment_size` | `transfers.chunk_size` | `10` (MB) to `"10MB"` |
| `transfer_order` | `transfers.transfer_order` | `size_dsc` to `size_desc` |
| `monitor_interval` | `sync.poll_interval` | `300` (seconds) to `"5m"` |
| `monitor_fullscan_frequency` | `sync.fullscan_frequency` | Direct |
| `classify_as_big_delete` | `safety.big_delete_threshold` | Direct |
| `space_reservation` | `safety.min_free_space` | `50` (MB) to `"50MB"` |
| `use_recycle_bin` | `safety.use_recycle_bin` | Direct |
| `sync_dir_permissions` | `safety.sync_dir_permissions` | `700` (octal int) to `"0700"` |
| `sync_file_permissions` | `safety.sync_file_permissions` | `600` (octal int) to `"0600"` |
| `disable_download_validation` | `safety.disable_download_validation` | Direct |
| `disable_upload_validation` | `safety.disable_upload_validation` | Direct |
| `dry_run` | `sync.dry_run` | Direct |
| `connect_timeout` | `network.connect_timeout` | `10` (seconds) to `"10s"` |
| `data_timeout` | `network.data_timeout` | `60` (seconds) to `"60s"` |
| `force_http_11` | `network.force_http_11` | Direct |
| `user_agent` | `network.user_agent` | Direct (update ISV format) |
| `application_id` | `profile.NAME.application_id` | Direct |
| `azure_ad_endpoint` | `profile.NAME.azure_ad_endpoint` | Direct |
| `azure_tenant_id` | `profile.NAME.azure_tenant_id` | Direct |
| `drive_id` | `profile.NAME.drive_id` | Direct |
| `disable_websocket_support` | `sync.websocket` | Inverted: `true` becomes `false` |
| `sync_list` file | `filter.sync_paths` | File with paths to TOML array |
| `enable_logging` + `log_dir` | `logging.log_file` | Combined (always-on in onedrive-go) |

#### CLI-Only Mappings

| abraunegg Option | onedrive-go Equivalent | Notes |
|-----------------|----------------------|-------|
| `download_only` | `sync --download-only` | CLI flag |
| `upload_only` | `sync --upload-only` | CLI flag |
| `no_remote_delete` | `sync --upload-only --no-remote-delete` | CLI flag |
| `cleanup_local_files` | `sync --download-only --cleanup-local` | CLI flag |
| `resync` | (automatic) | onedrive-go handles resync automatically via delta token management |
| `resync_auth` | (not needed) | No interactive resync prompt |
| `--confdir` | `--profile` | Multi-account via profiles, not directories |
| `--sync` / `--monitor` | `sync` / `sync --watch` | Commands, not flags |
| `--verbose` | `-v` / `-vv` / `--debug` | Same concept, different flags |
| `--single-directory PATH` | `filter.sync_paths` | Config option |
| `--force` | `--force` | Same flag |
| `use_device_auth` | `login --headless` | CLI flag |

#### Explicitly Rejected

| abraunegg Option | Reason for Rejection |
|-----------------|---------------------|
| `local_first` | Three-way merge handles conflict resolution; no "source of truth" concept |
| `bypass_data_preservation` | Unsafe: allows overwriting local files without backup |
| `remove_source_files` | Dangerous: deletes local files after upload. Use a separate tool for this. |
| `remove_source_folders` | Dangerous: depends on `remove_source_files` |
| `permanent_delete` | Unsafe: bypasses recycle bin. Use `use_recycle_bin = false` if intentional. |
| `check_nomount` | Use systemd mount dependencies (`RequiresMountsFor=`) instead |
| `check_nosync` | Use `.odignore` marker files instead (more flexible) |
| `create_new_file_version` | SharePoint enrichment handled differently (detect + accept) |
| `force_session_upload` | Always use session upload for files > 4MB (no config needed) |
| `read_only_auth_scope` | Not supported at MVP |
| `use_intune_sso` | Not supported (enterprise-only D-Bus integration) |
| `sync_root_files` | sync_paths semantics handle this implicitly |
| `sync_business_shared_items` | Post-MVP feature |
| `skip_dir_strict_match` | Always use default behavior; anchored paths with `/` prefix for strict matching |
| `webhook_enabled` | Built-in WebSocket support replaces HTTP webhook approach |
| `webhook_public_url` | Not needed (WebSocket is client-initiated, no inbound connections) |
| `webhook_listening_host` | Not needed |
| `webhook_listening_port` | Not needed |
| `webhook_expiration_interval` | Not needed |
| `webhook_renewal_interval` | Not needed |
| `webhook_retry_interval` | Not needed |
| `disable_notifications` | Desktop notifications not in scope |
| `notify_file_actions` | Desktop notifications not in scope |
| `display_manager_integration` | File manager integration not in scope |
| `write_xattr_data` | Extended attributes not supported |
| `delay_inotify_processing` | 2-second debounce handles this automatically |
| `inotify_delay` | Fixed debounce window, not configurable |
| `monitor_log_frequency` | Use `--quiet` + structured logging instead |
| `ip_protocol_version` | Go handles dual-stack automatically |
| `dns_timeout` | Go handles DNS caching automatically |
| `operation_timeout` | Use `data_timeout` instead |
| `max_curl_idle` | Go HTTP client manages connection pooling |
| `recycle_bin_path` | OS trash location auto-detected (FreeDesktop.org / macOS Trash) |
| `disable_permission_set` | Not needed; permissions always set per config |
| `disable_version_check` | No version check feature |
| `debug_https` | Use `--debug` flag for trace-level logging |
| `display_running_config` | Use `config show` command |
| `display_transfer_metrics` | Use `--verbose` or structured logging |
| `display_memory` | Developer tool only; use Go profiling tools |
| `monitor_max_loop` | Developer tool only; use test framework |
| `display_sync_options` | Developer tool only; use `config show` |
| `force_children_scan` | Developer tool only |
| `display_processing_time` | Developer tool only; use Go profiling tools |

#### Deprecated abraunegg Options (Already Removed)

| abraunegg Option | Status |
|-----------------|--------|
| `force_http_2` | Removed in reference. HTTP/2 default in onedrive-go. |
| `min_notify_changes` | Removed in reference. No equivalent needed. |
| `sync_business_shared_folders` | Replaced by `sync_business_shared_items` in reference. Not supported at MVP. |

### B.2 rclone to onedrive-go

| rclone Config/Flag | onedrive-go Option | Notes |
|-------------------|-------------------|-------|
| Remote name (`[myonedrive]`) | Profile name (`[profile.myonedrive]`) | Direct mapping |
| `type = onedrive` | Validation only | Confirms OneDrive remote |
| `drive_id` | `profile.NAME.drive_id` | Direct |
| `drive_type = personal` | `profile.NAME.account_type = "personal"` | Value mapping |
| `drive_type = business` | `profile.NAME.account_type = "business"` | Value mapping |
| `drive_type = documentLibrary` | `profile.NAME.account_type = "sharepoint"` | Value mapping |
| `region = global` | `profile.NAME.azure_ad_endpoint = ""` | Empty = global |
| `region = us` | `profile.NAME.azure_ad_endpoint = "USL4"` | Value mapping |
| `region = de` | `profile.NAME.azure_ad_endpoint = "DE"` | Value mapping |
| `region = cn` | `profile.NAME.azure_ad_endpoint = "CN"` | Value mapping |
| `token` | (not migrated) | Must re-authenticate |
| `--transfers N` | `transfers.parallel_downloads = N` + `transfers.parallel_uploads = N` | Split |
| `--checkers N` | `transfers.parallel_checkers = N` | Direct |
| `--bwlimit RATE` | `transfers.bandwidth_limit = "RATE"` | Format mapping |
| `--bwlimit "08:00,5M 18:00,50M"` | `transfers.bandwidth_schedule` | Complex mapping |
| `--include PATTERN` | `filter.skip_files` (inverted) | Best-effort inversion |
| `--exclude PATTERN` | `filter.skip_files` or `filter.skip_dirs` | Pattern analysis |
| `--filter-from FILE` | (manual conversion) | Complex; suggest manual review |
| `--min-size SIZE` | (not supported) | No minimum file size option |
| `--max-size SIZE` | `filter.max_file_size` | Direct |
| `--max-age AGE` | (not supported) | No age-based filtering |
| `--dry-run` | `sync.dry_run` or `--dry-run` | Direct |

---

## Appendix C: Decision Log

| # | Decision | Rationale |
|---|----------|-----------|
| C1 | Unknown config keys are fatal errors with Levenshtein suggestion | Silent ignoring leads to hidden misconfigurations. Users typing `skip_file` (singular, from abraunegg) instead of `skip_files` (plural) would be silently broken. Fatal error with suggestion is the safest approach. |
| C2 | Per-profile sections completely replace (not merge with) global sections | Merge semantics for arrays and maps are confusing and error-prone. If `[filter]` has `skip_files = ["~*", "*.tmp"]` and `[profile.work.filter]` has `skip_files = ["*.log"]`, should the result be `["~*", "*.tmp", "*.log"]` or `["*.log"]`? Complete replacement is unambiguous. |
| C3 | Stale files are never auto-deleted | The user added a filter to stop syncing files, not to delete them. Auto-deletion after a filter change would be surprising. Explicit disposition (stale delete / stale keep) gives the user control. |
| C4 | TOML arrays for skip patterns instead of pipe-delimited strings | Pipe-delimited strings are a bespoke format that requires escaping pipe characters in patterns. TOML arrays are standard, well-tooled, and do not conflate the delimiter with pattern content. |
| C5 | Three environment variables only (CONFIG, PROFILE, SYNC_DIR) | Exposing every option as an env var creates a parallel config surface that is hard to document, validate, and debug. Env vars are for deployment path overrides only. For Docker, mount a config file. |
| C6 | Bandwidth schedule uses local system time | Adding a timezone config option adds complexity and confusion (what if the system timezone changes?). Local time is what users expect: "throttle during work hours" means their local work hours. |
| C7 | .odignore uses full gitignore syntax via library | Gitignore syntax is well-known and well-documented. A Go library implementation avoids reimplementing the complex negation, double-star, and anchoring rules. Users can leverage existing gitignore knowledge. |
| C8 | Config init wizard with auto-detection of abraunegg/rclone | Lowering the migration barrier is critical for adoption. Auto-detection reduces the chance of users running two sync tools simultaneously (which causes corruption). One-click conversion makes migration painless. |
| C9 | Single TOML file for all profiles (not separate files per profile) | One file to manage, one file to back up, one file to version-control. Separate files per profile add filesystem management overhead. TOML sections cleanly namespace profiles. |
| C10 | Duration values as strings ("5m", "10s") not integers | Go duration strings are well-known, unambiguous, and self-documenting. An integer `300` could be seconds, milliseconds, or minutes. A string `"5m"` is unambiguous. |
| C11 | Human-readable sizes ("10MB", "1GB") not raw bytes | A config value of `10485760` is not human-readable. `"10MB"` is. For a user-facing config file, readability trumps precision. |
| C12 | Permissions as octal strings ("0700") not integers | TOML integers are decimal by default. A bare `700` would be interpreted as decimal 700, not octal 0700. Using a string with explicit `0` prefix makes the octal intent clear. |
| C13 | skip_dirs default mode matches anywhere in tree (no strict mode option) | The reference's `skip_dir_strict_match` option is confusing. Instead, we use anchored paths (leading `/`) for strict matching and bare names for anywhere matching. This is more intuitive and matches gitignore conventions. |
| C14 | No check_nosync option | The `.nosync` marker file approach from the reference is replaced by `.odignore` files, which are more flexible (gitignore patterns) and apply in both directions. A `.odignore` file containing `*` achieves the same effect as `.nosync`. |
| C15 | No webhook configuration | The reference uses HTTP webhooks (requiring a public HTTPS URL) for remote change notification. We use WebSocket subscriptions (client-initiated, no inbound connections required), which are simpler and work behind NAT/firewalls. No configuration needed beyond the `websocket` toggle. |
| C16 | Worker pool sizes not hot-reloadable | Changing pool sizes requires draining existing workers and creating new ones. This is complex and error-prone during active transfers. Restarting the process is simpler and safer. |
| C17 | `--watch` syncs all profiles by default | This matches the common deployment pattern: a single systemd/launchd service syncs all accounts. Per-profile `--watch` is still available via `--profile`. |
