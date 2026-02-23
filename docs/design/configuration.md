# Configuration Specification: onedrive-go

This document specifies the complete configuration system for onedrive-go — file format, option catalog, drive sections, filtering, validation, hot reload, migration, and the interactive setup command. It is the definitive reference for every configurable behavior in the system.

The authoritative source for the account/drive model, CLI commands, and login flows is [accounts.md](accounts.md). This document covers only the configuration system.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Config File Structure](#2-config-file-structure)
3. [Drive Sections](#3-drive-sections)
4. [Global Settings Catalog](#4-global-settings-catalog)
5. [Per-Drive Settings](#5-per-drive-settings)
6. [Filtering Options](#6-filtering-options)
7. [Transfer Options](#7-transfer-options)
8. [Safety Options](#8-safety-options)
9. [Logging Options](#9-logging-options)
10. [Network Options](#10-network-options)
11. [CLI Flag Reference](#11-cli-flag-reference)
12. [Setup Command](#12-setup-command)
13. [Config Validation](#13-config-validation)
14. [Hot Reload](#14-hot-reload)
- [Appendix A: Complete Options Reference Table](#appendix-a-complete-options-reference-table)
- [Appendix B: Migration Mapping Tables](#appendix-b-migration-mapping-tables)
- [Appendix C: Decision Log](#appendix-c-decision-log)

---

## 1. Overview

### 1.1 Purpose and Scope

This specification defines every configurable aspect of onedrive-go: how configuration is stored, loaded, validated, overridden, reloaded at runtime, and migrated from other tools. It covers:

- Config file format and locations
- Override precedence (defaults, file, env, CLI)
- Drive section mechanics (per-drive settings)
- Every configuration option with type, default, validation, and hot-reload status
- Filtering system (sync_paths, config patterns, .odignore markers)
- Stale file handling when filters change
- Interactive setup command
- Migration from abraunegg and rclone
- Config validation and error reporting

### 1.2 Design Philosophy

**Convention over configuration.** Sensible defaults that work for 90% of users. A new user runs `onedrive-go login`, gets auto-configured defaults, and syncs. Advanced users can tune every knob.

**Safe defaults.** Every default errs on the side of caution: big-delete protection on, recycle bin on, conservative timeouts, parallel workers within API guidance. A user who never touches the config file should never lose data.

**Explicit over implicit.** Unknown config keys are fatal errors. Changed filters require explicit user action. No silent behavior changes between versions.

**Single source of truth.** One TOML config file, one place to look. No config scattered across multiple files, registries, or environment variables (environment variables exist only for path and drive overrides).

**Flat structure.** All global settings are flat top-level TOML keys — no `[filter]`, `[transfers]`, `[safety]` sub-sections. Drive sections identified by `:` in the section name hold per-drive settings. This is simpler to read, write, and manipulate programmatically.

**Text-level manipulation.** The config file is read with a TOML parser but written with line-based text edits. This preserves all comments — both the initial defaults template and any the user adds. TOML libraries strip comments on round-trip; we avoid that entirely. See [accounts.md §4](accounts.md) for details.

### 1.3 Config File Format

**Format**: [TOML v1.0](https://toml.io/en/v1.0.0) via the [BurntSushi/toml](https://github.com/BurntSushi/toml) Go library.

TOML was chosen for:
- Human-readable and human-writable (unlike JSON)
- Supports comments (unlike JSON)
- Well-specified grammar (unlike YAML's implicit typing pitfalls)
- Strong Go ecosystem support

### 1.4 File Locations

Config file locations follow platform conventions:

| Platform | Config File |
|----------|-------------|
| **Linux** | `~/.config/onedrive-go/config.toml` |
| **macOS** | `~/Library/Application Support/onedrive-go/config.toml` |

On Linux, `XDG_CONFIG_HOME` is respected: if set, the config file is at `$XDG_CONFIG_HOME/onedrive-go/config.toml`.

Data files (tokens, state DBs, logs) live in a flat data directory:

| Platform | Data Directory |
|----------|---------------|
| **Linux** | `~/.local/share/onedrive-go/` |
| **macOS** | `~/Library/Application Support/onedrive-go/` |

On Linux, `XDG_DATA_HOME` is respected. Config and data may share the same directory on macOS. See [accounts.md §3](accounts.md) for the complete file layout.

If no config file exists, the application runs with built-in defaults. `login` creates the config file automatically.

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
- Per-drive settings override global settings for that specific drive (see [§5](#5-per-drive-settings))

### 1.6 Environment Variables

Only two environment variables are supported. They provide drive and path overrides for deployment scenarios (containers, CI, systemd) where modifying the config file is impractical:

| Variable | Purpose | Equivalent CLI Flag |
|----------|---------|-------------------|
| `ONEDRIVE_GO_CONFIG` | Override config file path | `--config` |
| `ONEDRIVE_GO_DRIVE` | Select active drive | `--drive` |

**Design rationale**: We deliberately limit env var support to key paths only. Exposing every option as an env var creates a parallel configuration surface that is hard to document, validate, and debug. For Docker/container deployments, mount a config file or use CLI flags.

---

## 2. Config File Structure

### 2.1 Complete Annotated Example

```toml
# =============================================================================
# onedrive-go configuration
# =============================================================================
# Docs: https://github.com/tonimelisma/onedrive-go
#
# Override precedence: defaults < config file < env vars < CLI flags
# Unknown keys cause a fatal error. Remove or comment out options you don't use.

# =============================================================================
# Global settings — flat top-level keys
# =============================================================================

# ── Logging ──
# log_level = "info"                     # debug, info, warn, error
# log_file = ""                          # log file path (empty = platform default)

# ── Filtering ──
# skip_dotfiles = false                  # skip files/dirs starting with .
# skip_symlinks = false                  # skip symbolic links (default: follow them)
# max_file_size = "0"                    # skip files larger than this (0 = no limit)
# skip_files = []                        # file name patterns to exclude
# skip_dirs = []                         # directory name patterns to exclude
# ignore_marker = ".odignore"            # per-directory marker file name

# ── Transfers ──
# parallel_downloads = 8                 # simultaneous download workers (1-16)
# parallel_uploads = 8                   # simultaneous upload workers (1-16)
# parallel_checkers = 8                  # simultaneous hash check workers (1-16)
# chunk_size = "10MB"                    # upload chunk size (320KiB multiples, 10-60MB)
# bandwidth_limit = "0"                  # global bandwidth limit (0 = unlimited)
# transfer_order = "default"             # default, size_asc, size_desc, name_asc, name_desc

# ── Safety ──
# big_delete_threshold = 1000            # abort if deleting more than N items
# big_delete_percentage = 50             # abort if deleting more than N% of total items
# big_delete_min_items = 10              # skip big-delete check if total items < N
# min_free_space = "1GB"                 # minimum free disk space before downloading
# use_recycle_bin = true                 # remote: use OneDrive recycle bin
# use_local_trash = true                 # local: use OS trash for remote-triggered deletes
# disable_download_validation = false    # skip hash verification on downloads
# disable_upload_validation = false      # skip hash verification on uploads
# sync_dir_permissions = "0700"          # POSIX permissions for created directories
# sync_file_permissions = "0600"         # POSIX permissions for created files

# ── Sync behavior ──
# poll_interval = "5m"                   # remote change polling interval (min 5m)
# fullscan_frequency = 12                # full scan every N poll intervals (0 = disabled)
# websocket = true                       # near-real-time remote change detection
# conflict_strategy = "keep_both"        # conflict resolution strategy
# conflict_reminder_interval = "1h"      # nag interval for unresolved conflicts
# verify_interval = "0"                  # periodic full-tree hash verification (0 = disabled)
# shutdown_timeout = "30s"               # time to wait for in-flight transfers on shutdown

# ── Network ──
# connect_timeout = "10s"                # TCP connection timeout
# data_timeout = "60s"                   # data transfer timeout
# user_agent = ""                        # custom User-Agent (empty = default ISV format)
# force_http_11 = false                  # force HTTP/1.1 instead of HTTP/2

# =============================================================================
# Drives — any section with ":" in the name is a drive
# =============================================================================
# Added automatically by 'login' and 'drive add'.
# Each section name is the canonical drive identifier.

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
alias = "work"
skip_dirs = ["node_modules", ".git", "vendor"]

["sharepoint:alice@contoso.com:marketing:Documents"]
sync_dir = "~/Contoso/Marketing - Documents"
enabled = false
```

### 2.2 Layout

The config file has two kinds of content:

1. **Global settings** — flat top-level TOML keys (no section headers). These are the defaults for all drives.
2. **Drive sections** — TOML sections identified by `:` in the section name (e.g., `["personal:toni@outlook.com"]`). These hold per-drive settings.

There are NO sub-sections like `[filter]`, `[transfers]`, `[safety]`, etc. All global settings are flat top-level keys. This simplifies reading, writing, and text-level manipulation.

Quotes around section names are required by TOML because `@` and `:` are not valid bare key characters. This is a TOML spec requirement.

### 2.3 Auto-Creation by Login

On first `login`, the app writes a complete config from a template string constant baked into the code. All global settings are present as commented-out defaults, so users can discover every option without reading docs. The drive section is appended at the end.

See [accounts.md §4](accounts.md) for the full template and write operation details.

### 2.4 Write Operations

Config is modified by `login`, `drive add`, `drive remove`, `setup`, and email change detection. All modifications use line-based text edits — never TOML round-trip serialization:

| Operation | When | How |
|-----------|------|-----|
| Append drive section | `login`, `drive add` | Append new `["type:email"]` block at end of file |
| Set `enabled = false` | `drive remove` | Find section header, find or insert `enabled` key |
| Delete section | `drive remove --purge` | Find section header, delete lines through next header or EOF |
| Rename section header | Email change detection | Find-and-replace one `["old"]` -> `["new"]` line |

User comments survive every operation because no line is touched unless it's the specific target of the edit.

### 2.5 Unknown Key Handling

Unknown keys in the config file cause a **fatal error** at startup. The application refuses to start and suggests the closest matching known key.

```
Error: unknown config key "skip_file"
Did you mean "skip_files"? (note: arrays use plural names)
```

The closest-match suggestion uses Levenshtein distance (edit distance) against all known keys. If the edit distance is <= 3, the suggestion is shown. This catches common typos and helps users migrating from other tools.

For keys that are recognized as abraunegg or rclone option names, a specific migration hint is provided.

**Rationale** (Decision C1): Silent ignoring of unknown keys leads to configuration that appears to work but does not. Fatal error with suggestion is the safest approach.

---

## 3. Drive Sections

### 3.1 Drive Identification

Each drive is represented as a TOML section with the canonical drive identifier as the section name. The canonical identifier format is `type:email[:site:library]`:

```toml
["personal:toni@outlook.com"]
["business:alice@contoso.com"]
["sharepoint:alice@contoso.com:marketing:Documents"]
```

Drive sections are auto-created by `login` and `drive add`. They can also be manually added. See [accounts.md §2](accounts.md) for the canonical identifier format.

### 3.2 Per-Drive Fields

These fields appear inside drive sections:

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `sync_dir` | String | Yes | — | Local directory to sync (tilde-expanded). Must be unique across drives. |
| `enabled` | Boolean | No | `true` | `false` = paused (`drive remove` sets this) |
| `alias` | String | No | — | Short name for `--drive` (e.g., `"work"`) |
| `remote_path` | String | No | `"/"` | Remote subfolder to sync |
| `drive_id` | String | No | auto | Explicit drive ID (auto-detected for personal/business) |
| `application_id` | String | No | (built-in) | Custom Azure application ID |
| `azure_ad_endpoint` | String | No | `""` | National cloud endpoint: `USL4`, `USL5`, `DE`, `CN` |
| `azure_tenant_id` | String | No | `""` | Azure AD tenant GUID or domain |

### 3.3 Per-Drive Overrides

Drive sections can override individual global settings. Only the following settings are overridable per-drive:

| Setting | Description |
|---------|-------------|
| `skip_dotfiles` | Per-drive override of global `skip_dotfiles` |
| `skip_dirs` | Per-drive override of global `skip_dirs` |
| `skip_files` | Per-drive override of global `skip_files` |
| `poll_interval` | Per-drive override of global `poll_interval` |
| `sync_paths` | Per-drive selective sync paths |

Per-drive overrides **replace** the global value entirely — they do not merge. If a drive section specifies `skip_dirs = ["vendor"]`, the global `skip_dirs` list is NOT merged in. This is a deliberate design choice: merge semantics for arrays are confusing and error-prone. Complete replacement is predictable.

**Example:**

```toml
# Global defaults
skip_dirs = ["node_modules", ".git"]
poll_interval = "5m"

# Work drive overrides
["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
skip_dirs = ["node_modules", ".git", "vendor"]   # replaces global list
poll_interval = "10m"                              # overrides global interval
```

### 3.4 Token and State Isolation

Each drive has its own state DB file. Token files are per-account (not per-drive) — SharePoint drives share the business account's token.

| File | Naming | Example |
|------|--------|---------|
| Token | `token_<type>_<email>.json` | `token_personal_toni@outlook.com.json` |
| State DB | `state_<canonical_id_underscored>.db` | `state_personal_toni@outlook.com.db` |

The `:` separator in canonical IDs is replaced with `_` in filenames. See [accounts.md §3](accounts.md) for the complete file layout.

---

## 4. Global Settings Catalog

All global settings are flat top-level TOML keys. They are organized here by functional area for documentation purposes, but in the config file they are all peers at the top level.

### 4.1 Logging Settings

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `log_level` | String | `"info"` | Log file verbosity: `debug`, `info`, `warn`, `error` |
| `log_file` | String | (platform default) | Log file path override. Empty = automatic path. |
| `log_format` | String | `"auto"` | Log format: `text`, `json`, `auto` (auto = text if interactive TTY, json otherwise) |
| `log_retention_days` | Integer | `30` | Days to keep log files before automatic cleanup. Minimum: 1. |

**Always-on file logging**: A log file is always written, regardless of console verbosity. Console output is controlled by CLI flags (`--quiet`, `--verbose`, `--debug`); the log file is controlled by `log_level` in config. These are independent channels. See [accounts.md §5](accounts.md).

**Automatic log file location** (when `log_file` is empty):

| Platform | Log File |
|----------|----------|
| Linux | `~/.local/share/onedrive-go/onedrive-go.log` |
| macOS | `~/Library/Application Support/onedrive-go/onedrive-go.log` |

**Token scrubbing**: Bearer tokens and pre-authenticated URLs are NEVER written to log files, even at debug level. Headers are replaced with `[REDACTED]`. Pre-authenticated download/upload URLs are truncated.

### 4.2 Filtering Settings

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `skip_dotfiles` | Boolean | `false` | Skip files/dirs starting with `.` |
| `skip_symlinks` | Boolean | `false` | Skip symbolic links (default: follow them). Broken and circular symlinks are always skipped. |
| `max_file_size` | String | `"0"` | Skip files larger than this size. `"0"` = no limit. Format: `"50MB"`, `"1GB"`. |
| `skip_files` | Array of strings | `[]` | File name patterns to exclude. Case-insensitive. Supports `*` and `?` wildcards. |
| `skip_dirs` | Array of strings | `[]` | Directory name patterns to exclude. Case-insensitive. Bare name matches anywhere; leading `/` anchors to sync root. |
| `ignore_marker` | String | `".odignore"` | Per-directory ignore marker file name |

### 4.3 Transfer Settings

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `parallel_downloads` | Integer | `8` | Concurrent download workers. Range: 1-16. |
| `parallel_uploads` | Integer | `8` | Concurrent upload workers. Range: 1-16. |
| `parallel_checkers` | Integer | `8` | Concurrent hash computation workers. Range: 1-16. |
| `chunk_size` | String | `"10MB"` | Upload chunk size for resumable sessions. Must be 320 KiB multiple. Range: 10-60 MB. |
| `bandwidth_limit` | String | `"0"` | Global bandwidth limit. `"0"` = unlimited. Format: `"5MB/s"`. |
| `bandwidth_schedule` | Array of tables | `[]` | Time-of-day bandwidth schedule. Overrides `bandwidth_limit` when active. |
| `transfer_order` | String | `"default"` | Transfer queue order: `default` (FIFO), `size_asc`, `size_desc`, `name_asc`, `name_desc`. |

### 4.4 Safety Settings

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `big_delete_threshold` | Integer | `1000` | Abort if deleting more than N items. |
| `big_delete_percentage` | Integer | `50` | Abort if deleting more than N% of total items. |
| `big_delete_min_items` | Integer | `10` | Skip big-delete check if total items < N. |
| `min_free_space` | String | `"1GB"` | Minimum free disk space. Downloads skipped if below. Format: human-readable size. |
| `use_recycle_bin` | Boolean | `true` | Remote: use OneDrive recycle bin (not permanent delete). |
| `use_local_trash` | Boolean | `true` | Local: use OS trash for remote-triggered deletes. |
| `disable_download_validation` | Boolean | `false` | Skip QuickXorHash verification on downloads. Last-resort escape hatch only. |
| `disable_upload_validation` | Boolean | `false` | Skip hash verification on uploads. Last-resort escape hatch only. |
| `sync_dir_permissions` | String | `"0700"` | POSIX permissions for created directories. Octal string. |
| `sync_file_permissions` | String | `"0600"` | POSIX permissions for created files. Octal string. |

### 4.5 Sync Behavior Settings

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `poll_interval` | Duration | `"5m"` | Remote change polling interval for `--watch`. Minimum: 5m. |
| `fullscan_frequency` | Integer | `12` | Full scan every N poll intervals. `0` = disabled. Minimum non-zero: 2. |
| `websocket` | Boolean | `true` | Near-real-time remote change detection via WebSocket. |
| `conflict_strategy` | String | `"keep_both"` | Conflict resolution strategy. Currently only `keep_both`. |
| `conflict_reminder_interval` | Duration | `"1h"` | Nag interval for unresolved conflicts in `--watch`. `"0"` disables. |
| `dry_run` | Boolean | `false` | Preview operations without executing. Equivalent to `--dry-run`. |
| `verify_interval` | Duration | `"0"` | Periodic full-tree hash verification. `"0"` disables. |
| `shutdown_timeout` | Duration | `"30s"` | Max time to wait for in-flight transfers on shutdown. |

### 4.6 Network Settings

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `connect_timeout` | Duration | `"10s"` | TCP connection timeout. |
| `data_timeout` | Duration | `"60s"` | Timeout for receiving data on active connection. |
| `user_agent` | String | `""` | Custom User-Agent. Empty = ISV default: `"ISV\|tonimelisma\|onedrive-go/vX.Y.Z"`. |
| `force_http_11` | Boolean | `false` | Force HTTP/1.1 instead of HTTP/2. |

### 4.7 Duration Format

Duration values use Go's duration string format:

| Format | Meaning |
|--------|---------|
| `"5m"` | 5 minutes |
| `"1h"` | 1 hour |
| `"30s"` | 30 seconds |
| `"2h30m"` | 2 hours 30 minutes |
| `"168h"` | 1 week |
| `"0"` | Disabled / no duration |

### 4.8 Size Format

Size values use human-readable format:

| Format | Meaning |
|--------|---------|
| `"0"` | No limit / unlimited |
| `"50MB"` | 50 megabytes (1000^2) |
| `"1GB"` | 1 gigabyte (1000^3) |
| `"500KiB"` | 500 kibibytes (1024) |
| `"1GiB"` | 1 gibibyte (1024^3) |

Bare numbers are interpreted as bytes. Valid suffixes: `KB`, `KiB`, `MB`, `MiB`, `GB`, `GiB`, `TB`, `TiB`.

### 4.9 Bandwidth Limit Format

Bandwidth limits require a `/s` suffix:

| Format | Meaning |
|--------|---------|
| `"0"` | Unlimited |
| `"100KB/s"` | 100 kilobytes per second |
| `"5MB/s"` | 5 megabytes per second |

Applied globally across all workers via a shared token bucket.

---

## 5. Per-Drive Settings

### 5.1 Override Mechanics

Per-drive settings override global settings for that drive only. A setting specified in a drive section **completely replaces** the global value — no merging.

```toml
# Global
skip_dirs = ["node_modules", ".git"]

# This drive gets ONLY ["vendor"] — not ["node_modules", ".git", "vendor"]
["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
skip_dirs = ["vendor"]
```

### 5.2 Resolution Algorithm

```
function ResolveSetting(driveID, settingName):
    if drive section has settingName:
        return drive section value
    else:
        return global value (or built-in default if global absent)
```

### 5.3 Overridable Settings

Only these global settings can be overridden per-drive:

| Setting | Rationale |
|---------|-----------|
| `skip_dotfiles` | Different projects have different dotfile needs |
| `skip_dirs` | Different codebases need different exclusions |
| `skip_files` | Different content types need different exclusions |
| `poll_interval` | Some drives are more latency-sensitive |
| `sync_paths` | Selective sync is inherently per-drive |

All other global settings (transfers, safety, logging, network) apply uniformly. This keeps the configuration predictable — you don't need to check every drive section to understand how transfers or safety work.

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
|    allowlist         |  are considered. Everything else excluded.
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

**Key property**: Each layer can only EXCLUDE more. No layer can include back an item excluded by a previous layer.

### 6.2 Pattern Matching Semantics

#### skip_files

- Patterns matched against the **basename** of each file
- Case-insensitive
- `*` matches zero or more characters, `?` matches one character
- Applied to files only, not directories
- Applied in both upload and download directions

#### skip_dirs

- **Default mode** (no leading `/`): matches each path component individually. `temp` matches `temp` at any depth.
- **Anchored mode** (leading `/`): matches full path relative to sync root. `/Documents/temp` matches only that directory.
- Case-insensitive, same wildcards as skip_files
- Applied to directories only. When excluded, all descendants are automatically excluded.

#### max_file_size

- Valid suffixes: `KB`, `KiB`, `MB`, `MiB`, `GB`, `GiB`, `TB`, `TiB`
- Bare number = bytes
- Applied to files only, both directions

### 6.3 sync_paths (Selective Sync)

`sync_paths` provides an inclusion allowlist. When set in a drive section, ONLY the listed paths and their children are synced:

```toml
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
sync_paths = ["/Documents", "/Photos/Camera Roll"]
```

- Paths must start with `/` (relative to sync root)
- A path includes all descendants
- Parent directories are traversed but NOT synced themselves
- Empty array = sync everything (default)
- Evaluated as the FIRST filter layer, before config patterns and .odignore

### 6.4 .odignore Marker Files

Drop an `.odignore` file in any directory to control filtering. Uses **full gitignore syntax**:

| Feature | Example | Description |
|---------|---------|-------------|
| Basic glob | `*.log` | Exclude .log files |
| Directory-only | `build/` | Exclude `build` directories |
| Negation | `!important.log` | Re-include a file |
| Double-star | `**/temp` | Match at any depth |
| Anchored | `/dist` | Match at this level only |
| Comment | `# Artifacts` | Ignored |

**Scope**: applies to the containing directory and all descendants. Does NOT affect parent directories. Applied in both directions.

### 6.5 Name Validation

OneDrive enforces naming restrictions that differ from POSIX. These are always applied and cannot be disabled:

| Rule | Details |
|------|---------|
| **Disallowed names** | `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9` |
| **Disallowed patterns** | Names starting with `~$`; names containing `_vti_`; `forms` at root level |
| **Invalid characters** | `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `\|` |
| **Invalid formatting** | Leading/trailing whitespace, trailing dot |
| **Path length** | 400 characters maximum |
| **Component length** | 255 bytes per component |

Items failing name validation are skipped with a warning log entry.

### 6.6 Stale Files Handling

When filter rules change, files that were previously synced but are now excluded become "stale." The sync engine detects this by comparing a hash of the current filter config against the stored snapshot.

**Behavior**:
1. Stale files stop syncing (excluded by new filters)
2. Stale files are NOT auto-deleted (safety invariant)
3. Each stale file is recorded in a ledger with path, reason, and size
4. User is nagged at the broadest level (directory, not individual files)
5. User explicitly disposes via `stale delete` or `stale keep`

**Rationale** (Decision C3): The user added a filter to stop syncing files, not to delete them. Auto-deletion would be surprising. Explicit disposition gives control.

---

## 7. Transfer Options

### 7.1 Worker Pool Design

Three independent worker pools:

| Pool | Default | Max | Purpose |
|------|---------|-----|---------|
| Downloads | 8 | 16 | File downloads |
| Uploads | 8 | 16 | File uploads |
| Checkers | 8 | 16 | Local hash computation |

The default of 8 aligns with Microsoft Graph API guidance (5-10 concurrent requests). Values above 16 are rejected.

### 7.2 Chunk Size Validation

Upload chunks must be multiples of 320 KiB. Valid values in the 10-60 MB range:

```
10MB, 15MB, 20MB, 25MB, 30MB, 35MB, 40MB, 45MB, 50MB, 55MB, 60MB
```

Invalid values are rejected with an error listing valid options.

### 7.3 Bandwidth Schedule

Time-of-day bandwidth limits, using local system time:

```toml
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },
    { time = "18:00", limit = "50MB/s" },
    { time = "23:00", limit = "0" },
]
```

- Times in 24-hour format, local system time
- Schedule wraps around midnight
- `"0"` = unlimited
- If `bandwidth_schedule` is non-empty, it overrides `bandwidth_limit`
- Evaluated every minute

---

## 8. Safety Options

### 8.1 Big-Delete Protection

Uses **OR logic**: if EITHER absolute count OR percentage threshold is exceeded, sync aborts.

```
Big delete triggered if:
    (totalItems >= big_delete_min_items) AND
    (deleteCount > big_delete_threshold OR deletePercentage > big_delete_percentage)
```

The `big_delete_min_items` guard prevents false positives on small drives.

### 8.2 Permissions

Specified as octal strings to avoid TOML integer parsing ambiguity:

```toml
sync_dir_permissions = "0700"    # drwx------
sync_file_permissions = "0600"   # -rw-------
```

Applied to files/directories **created** by sync. Existing files are not modified.

### 8.3 Validation Bypass

`disable_download_validation` and `disable_upload_validation` are **last-resort escape hatches only**. The primary mechanism for handling SharePoint enrichment is per-side hash baselines. These options exist for edge cases not covered by baselines (AIP-protected files, unrecognized transforms). When enabled:

```
WARN: Download validation disabled. Data integrity cannot be guaranteed.
```

---

## 9. Logging Options

### 9.1 Two Independent Channels

| Channel | Controlled by | Default |
|---------|--------------|---------|
| **Console** (stderr) | CLI flags (`--quiet`, `--verbose`, `--debug`) | Operational summaries |
| **Log file** | `log_level` in config | `info` level |

These are independent. `--quiet` suppresses console output but does not affect the log file.

### 9.2 Structured Log Fields

Every log entry includes structured fields:

| Field | When | Example |
|-------|------|---------|
| `drive` | During sync | `"personal:toni@outlook.com"` |
| `path` | Item operations | `"/Documents/report.pdf"` |
| `op` | Transfer/delete | `"download"`, `"upload"` |
| `duration` | After operation | `"2.3s"` |
| `size` | File operations | `2457600` |
| `error` | Warnings/errors | `"connection timeout"` |

---

## 10. Network Options

### 10.1 User-Agent Format

The default User-Agent follows Microsoft's ISV requirements:

```
ISV|tonimelisma|onedrive-go/v0.1.0
```

Format: `ISV|<publisher>|<application>/<version>`. Helps Microsoft distinguish ISV traffic from unauthorized usage.

---

## 11. CLI Flag Reference

### 11.1 Global Flags

| Flag | Type | Description |
|------|------|-------------|
| `--account <email>` | String | Select account by email (auth commands: `login`, `logout`) |
| `--drive <id\|alias>` | String (repeatable) | Select drive by canonical ID, alias, or partial match |
| `--config <path>` | String | Override config file path |
| `--json` | Boolean | Machine-readable JSON output |
| `--verbose` / `-v` | Boolean | Show individual file operations |
| `--debug` | Boolean | Show HTTP requests, internal state, config resolution |
| `--quiet` / `-q` | Boolean | Errors only |
| `--dry-run` | Boolean | Preview operations without executing |

### 11.2 Sync Flags

| Flag | Description |
|------|-------------|
| `--watch` | Continuous sync — stay running, re-sync on `poll_interval` |
| `--download-only` | Download remote changes only |
| `--upload-only` | Upload local changes only |
| `--force` | Proceed past big-delete protection |

### 11.3 Login Flags

| Flag | Description |
|------|-------------|
| `--browser` | Use authorization code flow (opens browser, localhost callback) |

### 11.4 Flag-to-Config Mapping

| CLI Flag | Config Key | Override Behavior |
|----------|-----------|-------------------|
| `--dry-run` | `dry_run` | CLI replaces config |
| `--force` | (CLI-only) | No config equivalent |
| `--watch` | (CLI-only) | No config equivalent |
| `--download-only` | (CLI-only) | No config equivalent |
| `--upload-only` | (CLI-only) | No config equivalent |
| `--verbose` / `-v` | (CLI-only) | Overrides log_level for stderr |
| `--quiet` / `-q` | (CLI-only) | Suppresses stderr |
| `--debug` | (CLI-only) | Shows HTTP requests, config resolution |
| `--json` | (CLI-only) | Changes output format |
| `--browser` | (CLI-only) | Selects auth code flow |

### 11.5 Config-Only Options

These have no CLI flag equivalent:

| Config Key | Reason |
|-----------|--------|
| `application_id` | Rarely changed, per-drive |
| `azure_ad_endpoint` | Enterprise-only, per-drive |
| `azure_tenant_id` | Enterprise-only, per-drive |
| `drive_id` | Set by login/drive add |
| `remote_path` | Rarely changed |
| `ignore_marker` | Rarely changed |
| `bandwidth_schedule` | Complex structure |
| `transfer_order` | Rarely changed |
| `sync_dir_permissions` | Rarely changed |
| `sync_file_permissions` | Rarely changed |
| `disable_download_validation` | Dangerous, requires deliberate edit |
| `disable_upload_validation` | Dangerous, requires deliberate edit |
| `connect_timeout` | Rarely changed |
| `data_timeout` | Rarely changed |
| `user_agent` | Rarely changed |
| `force_http_11` | Rarely changed |
| `log_file` | Rarely changed |
| `log_retention_days` | Rarely changed |
| `websocket` | Rarely changed |
| `verify_interval` | Rarely changed |
| `shutdown_timeout` | Rarely changed |

---

## 12. Setup Command

### 12.1 Overview

`onedrive-go setup` is the interactive guided configuration command. It is menu-driven and covers all configuration tasks. Everything `setup` does can also be done by editing `config.toml` directly.

### 12.2 Capabilities

- View current drives and settings
- Change sync directories
- Configure exclusions (skip_dirs, skip_files, skip_dotfiles)
- Set sync interval (`poll_interval`)
- Set log level
- Configure per-drive overrides
- Set aliases

### 12.3 Migration During Setup

During `setup` (or `migrate`), the tool checks for existing sync tool configurations:

**abraunegg/onedrive detection**:
- Check for `~/.config/onedrive/config`
- Check for `~/.config/onedrive/sync_list`
- Check for running `onedrive` process

**rclone detection**:
- Check for `~/.config/rclone/rclone.conf` with `type = onedrive` remotes
- Check for running `rclone` process with OneDrive remote

If detected, offer to import settings. Warn about running instances to avoid conflicts.

---

## 13. Config Validation

### 13.1 Per-Field Validation

| Option | Validation | Warning |
|--------|-----------|---------|
| `sync_dir` | Absolute path after tilde expansion, unique across drives | - |
| `remote_path` | Starts with `/` | - |
| `drive_id` | Non-empty for SharePoint drives | - |
| `application_id` | Non-empty when set | - |
| `azure_ad_endpoint` | `""`, `USL4`, `USL5`, `DE`, `CN` | - |
| `azure_tenant_id` | Required with `azure_ad_endpoint` | - |
| `skip_files` | Valid glob patterns | - |
| `skip_dirs` | Valid glob patterns, not `.*` | - |
| `max_file_size` | Valid size, >= 0 | - |
| `sync_paths` | Each starts with `/` | - |
| `parallel_downloads` | 1-16 | - |
| `parallel_uploads` | 1-16 | - |
| `parallel_checkers` | 1-16 | - |
| `chunk_size` | 10-60 MB, 320 KiB multiple | - |
| `bandwidth_limit` | `"0"` or valid rate | - |
| `big_delete_threshold` | >= 1 | - |
| `big_delete_percentage` | 1-100 | - |
| `big_delete_min_items` | >= 1 | - |
| `min_free_space` | Valid size, >= 0 | - |
| `disable_download_validation` | Boolean | Warning if true |
| `disable_upload_validation` | Boolean | Warning if true |
| `sync_dir_permissions` | Valid octal (3-4 digits) | - |
| `sync_file_permissions` | Valid octal (3-4 digits) | - |
| `poll_interval` | >= 5m | - |
| `fullscan_frequency` | 0 or >= 2 | - |
| `conflict_strategy` | `keep_both` | - |
| `conflict_reminder_interval` | >= 0 | - |
| `verify_interval` | >= 0 | - |
| `shutdown_timeout` | >= 5s | - |
| `log_level` | `debug`, `info`, `warn`, `error` | - |
| `log_file` | Valid path when non-empty | - |
| `log_format` | `text`, `json`, `auto` | - |
| `log_retention_days` | >= 1 | - |
| `connect_timeout` | >= 1s | - |
| `data_timeout` | >= 5s | - |
| `force_http_11` | Boolean | - |

### 13.2 Cross-Field Validation

| Rule | Error Message |
|------|--------------|
| `azure_ad_endpoint` set but `azure_tenant_id` empty | `azure_ad_endpoint requires azure_tenant_id to be set` |
| `drive_id` empty for SharePoint drive | `SharePoint drives require drive_id` |
| `disable_download_validation = true` | Warning: `Download validation disabled. Data integrity cannot be guaranteed.` |
| `disable_upload_validation = true` | Warning: `Upload validation disabled. Post-upload corruption cannot be detected.` |
| Multiple drives with same `sync_dir` | `Drives "X" and "Y" have the same sync_dir — this will cause conflicts` |
| `bandwidth_schedule` not chronological | Warning: `bandwidth_schedule entries should be in chronological order for clarity` |
| `big_delete_min_items` > `big_delete_threshold` | Warning: `big_delete_min_items exceeds big_delete_threshold — big-delete protection will never trigger` |

### 13.3 Shadow Validation for Filters

At startup, the filter engine validates that `sync_paths` entries are not shadowed by `skip_dirs` or `skip_files` patterns:

```
if sync_paths entry matches skip_dirs or skip_files:
    error: "sync_paths entry %q shadowed by %s pattern — it will never sync"
```

### 13.4 Startup Validation Sequence

```
1. Load config file (TOML parse)
     - Reject malformed TOML with parse error + line number
2. Check for unknown keys
     - Fatal error with Levenshtein-based suggestion
3. Drive section validation
     - Required fields present (sync_dir)
     - No duplicate sync_dirs
4. Type validation
     - Correct TOML types, valid enums, ranges within bounds
5. Cross-field validation
     - Dependency checks (azure_ad_endpoint requires azure_tenant_id)
6. Filter shadow validation
     - sync_paths not shadowed by skip patterns
7. Warnings (non-fatal)
     - Validation bypass options enabled
     - Unusual configurations
8. Per-drive resolution
     - Resolve per-drive overrides
     - Apply defaults for missing settings
```

---

## 14. Hot Reload

### 14.1 Overview

`sync --watch` re-reads config on each sync cycle. Changes take effect on the next cycle without restart. For immediate effect, SIGHUP triggers config re-read.

### 14.2 Hot-Reloadable Options

| Category | Options | Effect |
|----------|---------|--------|
| **Filter** | `skip_files`, `skip_dirs`, `skip_dotfiles`, `skip_symlinks`, `max_file_size`, `sync_paths`, `ignore_marker` | Filter engine re-initialized, stale file detection triggered |
| **Transfers** | `bandwidth_limit`, `bandwidth_schedule`, `transfer_order` | Bandwidth scheduler updated |
| **Sync** | `poll_interval`, `fullscan_frequency`, `conflict_reminder_interval`, `websocket` | Timers updated |
| **Logging** | `log_level`, `log_format` | Log output updated immediately |
| **Safety** | `big_delete_threshold`, `big_delete_percentage`, `big_delete_min_items`, `min_free_space` | Updated for next cycle |

### 14.3 Non-Reloadable Options (Require Restart)

| Category | Options | Reason |
|----------|---------|--------|
| **Drive** | `sync_dir`, `drive_id`, `remote_path` | Requires re-initializing watcher and delta token |
| **Drive** | `application_id`, `azure_ad_endpoint`, `azure_tenant_id` | Requires re-authentication |
| **Transfers** | `parallel_downloads`, `parallel_uploads`, `parallel_checkers`, `chunk_size` | Worker pools initialized at startup |
| **Safety** | `sync_dir_permissions`, `sync_file_permissions` | Applied at file creation time |
| **Network** | `connect_timeout`, `data_timeout`, `force_http_11`, `user_agent` | HTTP client initialized at startup |
| **Logging** | `log_file`, `log_retention_days` | File handle management |

When a non-reloadable option changes:

```
WARN: Config option "sync_dir" changed but requires restart to take effect.
```

### 14.4 Drive Section Changes

New drive sections added to config are picked up on the next sync cycle. Drives with `enabled = false` are skipped. Removed sections stop syncing. This is how `login`, `drive add`, and `drive remove` work with a running `sync --watch` — they modify config, and the service picks it up. No restart needed.

---

## Appendix A: Complete Options Reference Table

Every configuration option in a single reference table.

| Option | Scope | Type | Default | CLI Flag | Hot-Reload | Description |
|--------|-------|------|---------|----------|:----------:|-------------|
| `sync_dir` | Drive | String | — | - | No | Local sync directory |
| `enabled` | Drive | Boolean | `true` | - | Yes | Drive enabled for sync |
| `alias` | Drive | String | `""` | - | Yes | Short name for `--drive` |
| `remote_path` | Drive | String | `"/"` | - | No | Remote path to sync |
| `drive_id` | Drive | String | auto | - | No | Drive ID (required for SharePoint) |
| `application_id` | Drive | String | (built-in) | - | No | Azure app ID |
| `azure_ad_endpoint` | Drive | String | `""` | - | No | National cloud endpoint |
| `azure_tenant_id` | Drive | String | `""` | - | No | Azure AD tenant |
| `log_level` | Global | String | `"info"` | `--debug` | Yes | Log file verbosity |
| `log_file` | Global | String | `""` | - | No | Log file path |
| `log_format` | Global | String | `"auto"` | - | Yes | Log format |
| `log_retention_days` | Global | Integer | `30` | - | No | Log file retention |
| `skip_dotfiles` | Global/Drive | Boolean | `false` | - | Yes | Skip dotfiles |
| `skip_symlinks` | Global | Boolean | `false` | - | Yes | Skip symlinks |
| `max_file_size` | Global | String | `"0"` | - | Yes | Max file size |
| `skip_files` | Global/Drive | Array | `[]` | - | Yes | File exclusion patterns |
| `skip_dirs` | Global/Drive | Array | `[]` | - | Yes | Dir exclusion patterns |
| `ignore_marker` | Global | String | `".odignore"` | - | Yes | Ignore marker name |
| `sync_paths` | Drive | Array | `[]` | - | Yes | Selective sync paths |
| `parallel_downloads` | Global | Integer | `8` | - | No | Download workers |
| `parallel_uploads` | Global | Integer | `8` | - | No | Upload workers |
| `parallel_checkers` | Global | Integer | `8` | - | No | Hash check workers |
| `chunk_size` | Global | String | `"10MB"` | - | No | Upload chunk size |
| `bandwidth_limit` | Global | String | `"0"` | - | Yes | Bandwidth limit |
| `bandwidth_schedule` | Global | Array | `[]` | - | Yes | Time-of-day bandwidth |
| `transfer_order` | Global | String | `"default"` | - | Yes | Transfer queue order |
| `big_delete_threshold` | Global | Integer | `1000` | - | Yes | Big-delete count |
| `big_delete_percentage` | Global | Integer | `50` | - | Yes | Big-delete percentage |
| `big_delete_min_items` | Global | Integer | `10` | - | Yes | Big-delete min items |
| `min_free_space` | Global | String | `"1GB"` | - | Yes | Min free disk space |
| `use_recycle_bin` | Global | Boolean | `true` | - | Yes | Remote recycle bin |
| `use_local_trash` | Global | Boolean | `true` | - | Yes | Local OS trash |
| `disable_download_validation` | Global | Boolean | `false` | - | Yes | Skip download hash |
| `disable_upload_validation` | Global | Boolean | `false` | - | Yes | Skip upload hash |
| `sync_dir_permissions` | Global | String | `"0700"` | - | No | Dir permissions |
| `sync_file_permissions` | Global | String | `"0600"` | - | No | File permissions |
| `poll_interval` | Global/Drive | Duration | `"5m"` | - | Yes | Polling interval |
| `fullscan_frequency` | Global | Integer | `12` | - | Yes | Full scan frequency |
| `websocket` | Global | Boolean | `true` | - | Yes | WebSocket support |
| `conflict_strategy` | Global | String | `"keep_both"` | - | Yes | Conflict resolution |
| `conflict_reminder_interval` | Global | Duration | `"1h"` | - | Yes | Conflict nag interval |
| `dry_run` | Global | Boolean | `false` | `--dry-run` | Yes | Dry-run mode |
| `verify_interval` | Global | Duration | `"0"` | - | No | Periodic verify |
| `shutdown_timeout` | Global | Duration | `"30s"` | - | No | Shutdown timeout |
| `connect_timeout` | Global | Duration | `"10s"` | - | No | TCP timeout |
| `data_timeout` | Global | Duration | `"60s"` | - | No | Data timeout |
| `user_agent` | Global | String | `""` | - | No | User-Agent header |
| `force_http_11` | Global | Boolean | `false` | - | No | Force HTTP/1.1 |

---

## Appendix B: Migration Mapping Tables

### B.1 abraunegg/onedrive to onedrive-go

#### Adopted (with mapping)

| abraunegg Option | onedrive-go Option | Transformation |
|-----------------|-------------------|----------------|
| `sync_dir` | drive section `sync_dir` | Direct |
| `skip_file` | `skip_files` | Pipe-delimited string to TOML array |
| `skip_dir` | `skip_dirs` | Pipe-delimited string to TOML array |
| `skip_dotfiles` | `skip_dotfiles` | Direct |
| `skip_symlinks` | `skip_symlinks` | Direct |
| `skip_size` | `max_file_size` | `50` (MB) to `"50MB"` |
| `threads` | `parallel_downloads` + `parallel_uploads` + `parallel_checkers` | One value to three |
| `rate_limit` | `bandwidth_limit` | Bytes/s to `"NMB/s"` |
| `file_fragment_size` | `chunk_size` | `10` (MB) to `"10MB"` |
| `transfer_order` | `transfer_order` | `size_dsc` to `size_desc` |
| `monitor_interval` | `poll_interval` | `300` (seconds) to `"5m"` |
| `monitor_fullscan_frequency` | `fullscan_frequency` | Direct |
| `classify_as_big_delete` | `big_delete_threshold` | Direct |
| `space_reservation` | `min_free_space` | `50` (MB) to `"50MB"` |
| `use_recycle_bin` | `use_recycle_bin` | Direct |
| `sync_dir_permissions` | `sync_dir_permissions` | `700` (int) to `"0700"` |
| `sync_file_permissions` | `sync_file_permissions` | `600` (int) to `"0600"` |
| `disable_download_validation` | `disable_download_validation` | Direct |
| `disable_upload_validation` | `disable_upload_validation` | Direct |
| `dry_run` | `dry_run` | Direct |
| `connect_timeout` | `connect_timeout` | `10` (seconds) to `"10s"` |
| `data_timeout` | `data_timeout` | `60` (seconds) to `"60s"` |
| `force_http_11` | `force_http_11` | Direct |
| `user_agent` | `user_agent` | Direct (update ISV format) |
| `application_id` | drive `application_id` | Direct |
| `azure_ad_endpoint` | drive `azure_ad_endpoint` | Direct |
| `azure_tenant_id` | drive `azure_tenant_id` | Direct |
| `drive_id` | drive `drive_id` | Direct |
| `disable_websocket_support` | `websocket` | Inverted |
| `sync_list` file | drive `sync_paths` | File to TOML array |
| `enable_logging` + `log_dir` | `log_file` | Combined (always-on) |

#### CLI-Only Mappings

| abraunegg Option | onedrive-go Equivalent | Notes |
|-----------------|----------------------|-------|
| `download_only` | `sync --download-only` | CLI flag |
| `upload_only` | `sync --upload-only` | CLI flag |
| `no_remote_delete` | (not supported separately) | Use `--upload-only` |
| `cleanup_local_files` | (not supported separately) | Use `--download-only` |
| `resync` | (automatic) | Delta token management handles this |
| `--confdir` | `--drive` | Multi-account via drives |
| `--sync` / `--monitor` | `sync` / `sync --watch` | Commands |
| `--verbose` | `-v` / `--debug` | Same concept |
| `use_device_auth` | (default) | Device code is the default auth method |

#### Explicitly Rejected

| abraunegg Option | Reason |
|-----------------|--------|
| `local_first` | Three-way merge handles conflicts; no "source of truth" concept |
| `bypass_data_preservation` | Unsafe: allows overwriting files without backup |
| `remove_source_files` | Dangerous: deletes files after upload |
| `remove_source_folders` | Dangerous: depends on `remove_source_files` |
| `permanent_delete` | Unsafe: bypasses recycle bin |
| `check_nomount` | Use systemd mount dependencies |
| `check_nosync` | Use `.odignore` instead |
| `create_new_file_version` | SharePoint enrichment handled differently |
| `force_session_upload` | Always use session upload for files > 4MB |
| `read_only_auth_scope` | Not supported at MVP |
| `use_intune_sso` | Not supported |
| `sync_root_files` | sync_paths handles this |
| `sync_business_shared_items` | Post-MVP |
| `webhook_*` | Built-in WebSocket replaces HTTP webhooks |
| `display_*` / `debug_*` | Use `--verbose`, `--debug`, `--json` |
| `notify_*` | Desktop notifications not in scope |
| `write_xattr_data` | Not supported |
| `inotify_delay` | 2-second debounce handles this |
| `ip_protocol_version` | Go handles dual-stack automatically |
| `dns_timeout` | Go handles DNS automatically |
| `operation_timeout` | Use `data_timeout` |
| `max_curl_idle` | Go HTTP manages connection pooling |
| `recycle_bin_path` | OS trash auto-detected |

### B.2 rclone to onedrive-go

| rclone Config/Flag | onedrive-go Option | Notes |
|-------------------|-------------------|-------|
| Remote name | Drive `alias` | Name becomes alias |
| `type = onedrive` | Validation only | Confirms OneDrive remote |
| `drive_id` | Drive `drive_id` | Direct |
| `drive_type = personal` | Auto-detected | Drive type from canonical ID |
| `drive_type = business` | Auto-detected | Drive type from canonical ID |
| `drive_type = documentLibrary` | Auto-detected | Drive type from canonical ID |
| `region = global` | `azure_ad_endpoint = ""` | Empty = global |
| `region = us` | `azure_ad_endpoint = "USL4"` | Value mapping |
| `region = de` | `azure_ad_endpoint = "DE"` | Value mapping |
| `region = cn` | `azure_ad_endpoint = "CN"` | Value mapping |
| `token` | (not migrated) | Must re-authenticate |
| `--transfers N` | `parallel_downloads` + `parallel_uploads` | Split |
| `--checkers N` | `parallel_checkers` | Direct |
| `--bwlimit RATE` | `bandwidth_limit` | Format mapping |
| `--bwlimit "08:00,5M 18:00,50M"` | `bandwidth_schedule` | Complex mapping |
| `--exclude PATTERN` | `skip_files` / `skip_dirs` | Pattern analysis |
| `--max-size SIZE` | `max_file_size` | Direct |
| `--dry-run` | `--dry-run` | Direct |

---

## Appendix C: Decision Log

| # | Decision | Rationale |
|---|----------|-----------|
| C1 | Unknown config keys are fatal errors with Levenshtein suggestion | Silent ignoring leads to hidden misconfigurations. Fatal error with suggestion is safest. |
| C2 | Per-drive filter overrides completely replace (not merge with) global values | Merge semantics for arrays are confusing. Complete replacement is unambiguous. |
| C3 | Stale files are never auto-deleted | User added filter to stop syncing, not to delete. Explicit disposition gives control. |
| C4 | TOML arrays for skip patterns instead of pipe-delimited strings | Pipe-delimited is bespoke format. TOML arrays are standard and well-tooled. |
| C5 | Two environment variables only (CONFIG, DRIVE) | Env vars for deployment path overrides only. For Docker, mount a config file. |
| C6 | Bandwidth schedule uses local system time | Adding timezone config adds complexity. Local time is what users expect. |
| C7 | .odignore uses full gitignore syntax via library | Gitignore syntax is well-known. Library avoids reimplementing negation/double-star/anchoring. |
| C8 | Setup command with auto-detection of abraunegg/rclone | Lowering migration barrier is critical for adoption. One-click conversion. |
| C9 | Single TOML file for all drives | One file to manage, back up, version-control. TOML sections namespace drives. |
| C10 | Duration values as strings ("5m") not integers | Self-documenting. `300` is ambiguous; `"5m"` is not. |
| C11 | Human-readable sizes ("10MB") not raw bytes | `10485760` is not human-readable. `"10MB"` is. |
| C12 | Permissions as octal strings ("0700") not integers | TOML integers are decimal. `700` != octal 0700. String with `0` prefix is clear. |
| C13 | skip_dirs default mode matches anywhere; `/` prefix for anchored | More intuitive than strict/non-strict modes. Matches gitignore conventions. |
| C14 | No check_nosync option | `.odignore` with `*` achieves same effect, more flexible. |
| C15 | No webhook configuration | WebSocket (client-initiated) replaces HTTP webhooks. No config needed. |
| C16 | Worker pool sizes not hot-reloadable | Draining/recreating pools during transfers is error-prone. Restart is simpler. |
| C17 | `sync --watch` syncs all enabled drives by default | Matches single-service deployment pattern. `--drive` for per-drive. |
| C18 | Flat config format — no sub-sections | Not enough settings to justify sections. Simpler to read, write, and manipulate. |
| C19 | Text-level config manipulation preserving comments | TOML libraries strip comments on round-trip. Line-based edits preserve everything. |
| C20 | Config auto-created by login, not a separate init command | One command to start (login), not two (init + login). Commented defaults for discovery. |
| C21 | No `config show` command | Users read config file directly. `status` shows runtime state. `--debug` shows resolution. |
| C22 | `--account` for auth commands, `--drive` for everything else | Two flags for two concepts. Clear, no ambiguity. |
| C23 | No `--sync-dir` CLI flag | All drives get sensible defaults. Change via config or `setup`. |
