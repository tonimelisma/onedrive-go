# Reference Implementation: Configuration Inventory

This document catalogs every configuration option in the reference OneDrive sync client for Linux, organized by functional area. It describes what each option controls, its type, default value, and importance for our Go reimplementation.

---

## Table of Contents

1. [Config File Format](#config-file-format)
2. [Config File Locations](#config-file-locations)
3. [Command-Line Overrides](#command-line-overrides)
4. [Environment Variable Support](#environment-variable-support)
5. [Multi-Config / Profile Support](#multi-config--profile-support)
6. [Sync List File](#sync-list-file)
7. [Configuration Options by Category](#configuration-options-by-category)
   - [Authentication & Authorization](#1-authentication--authorization)
   - [Sync Behavior](#2-sync-behavior)
   - [Filtering](#3-filtering)
   - [Performance](#4-performance)
   - [Monitoring](#5-monitoring)
   - [Logging & Diagnostics](#6-logging--diagnostics)
   - [Paths & Directories](#7-paths--directories)
   - [Network & HTTP](#8-network--http)
   - [Data Integrity & Safety](#9-data-integrity--safety)
   - [GUI & Desktop Integration](#10-gui--desktop-integration)
   - [Webhooks](#11-webhooks)
   - [Advanced / Enterprise](#12-advanced--enterprise)
   - [Developer / Debug Only](#13-developer--debug-only)
8. [CLI-Only Options (Not in Config File)](#cli-only-options-not-in-config-file)
9. [Deprecated Options](#deprecated-options)

---

## Config File Format

The reference implementation uses a simple, custom key-value format. Each configuration line follows the pattern:

```
key = "value"
```

Specifics:
- **Regex for parsing:** `^(\w+)\s*=\s*"(.*)"\s*$`
- All values are enclosed in double quotes, regardless of type (bool, int, string).
- Boolean values must be the literal strings `"true"` or `"false"`.
- Lines beginning with `#` or `;` are treated as comments and ignored.
- Empty lines are ignored.
- The file has no section headers (unlike INI format) -- it is completely flat.
- Unknown keys cause a fatal error and abort config loading.
- Malformed lines (not matching the regex) cause a fatal error.
- Certain keys (specifically `skip_file` and `skip_dir`) can appear **multiple times**; their values are merged with pipe (`|`) delimiters and deduplicated.

This is **not** TOML, YAML, JSON, or INI. It is a bespoke format.

---

## Config File Locations

The reference implementation searches for configuration files in the following order of priority:

1. **User config file:** `~/.config/onedrive/config`
   - The primary location. If this file exists, it is used.
   - The base directory (`~/.config`) is derived from the `XDG_CONFIG_HOME` environment variable if set, otherwise defaults to `$HOME/.config`.

2. **System config file:** `/etc/onedrive/config`
   - Used as a fallback only when the user config file does not exist and no `--confdir` was specified.
   - When the system config is used, the system config directory is used for config data (including the hash file), but user state data (database, delta link, tokens) still resides under the user directory.

3. **Custom config directory:** Specified via the `--confdir` CLI option.
   - Completely overrides both the user and system config lookup.
   - Must be specified on every invocation; it is not remembered.
   - Example: `onedrive --confdir '~/.config/onedrive-business/'`

If no config file is found anywhere, the application runs with built-in defaults.

### Files stored in the config directory

The config directory (whether user, system, or custom) also stores or references these state and metadata files:
- `config` -- the configuration file itself
- `refresh_token` -- OAuth2 refresh token
- `intune_account` -- Intune SSO account details
- `delta_link` -- saved delta query token for incremental sync
- `items.sqlite3` -- the local item database (SQLite)
- `items-dryrun.sqlite3` -- a separate database used during dry-run operations
- `session_upload` -- resumable upload session state
- `resume_download` -- download resume state
- `sync_list` -- selective sync include list (see below)
- `.config.hash` -- hash of the config file (to detect changes requiring resync)
- `.config.backup` -- backup copy of the config file
- `.sync_list.hash` -- hash of the sync_list file (to detect changes requiring resync)

---

## Command-Line Overrides

Many config file options can also be set or overridden via command-line flags. When both a config file value and a CLI flag are present, the CLI flag takes precedence and **replaces** the config file value (it does not merge with it).

The following table summarizes which config-file options have CLI equivalents:

| Config Key | CLI Flag |
|---|---|
| `check_nomount` | `--check-for-nomount` |
| `check_nosync` | `--check-for-nosync` |
| `classify_as_big_delete` | `--classify-as-big-delete N` |
| `cleanup_local_files` | `--cleanup-local-files` |
| `debug_https` | `--debug-https` |
| `disable_notifications` | `--disable-notifications` |
| `disable_download_validation` | `--disable-download-validation` |
| `disable_upload_validation` | `--disable-upload-validation` |
| `display_running_config` | `--display-running-config` |
| `download_only` | `--download-only` |
| `dry_run` | `--dry-run` |
| `enable_logging` | `--enable-logging` |
| `file_fragment_size` | `--file-fragment-size N` |
| `force_http_11` | `--force-http-11` |
| `local_first` | `--local-first` |
| `log_dir` | `--log-dir PATH` |
| `monitor_interval` | `--monitor-interval N` |
| `monitor_fullscan_frequency` | `--monitor-fullscan-frequency N` |
| `monitor_log_frequency` | `--monitor-log-frequency N` |
| `no_remote_delete` | `--no-remote-delete` |
| `remove_source_files` | `--remove-source-files` |
| `remove_source_folders` | `--remove-source-folders` |
| `resync` | `--resync` |
| `resync_auth` | `--resync-auth` |
| `skip_dir` | `--skip-dir PATTERN` |
| `skip_dir_strict_match` | `--skip-dir-strict-match` |
| `skip_dotfiles` | `--skip-dot-files` |
| `skip_file` | `--skip-file PATTERN` |
| `skip_size` | `--skip-size N` |
| `skip_symlinks` | `--skip-symlinks` |
| `space_reservation` | `--space-reservation N` |
| `sync_dir` | `--syncdir PATH` |
| `sync_root_files` | `--sync-root-files` |
| `threads` | `--threads N` |
| `upload_only` | `--upload-only` |

Options that are config-file-only (no CLI equivalent): `application_id`, `azure_ad_endpoint`, `azure_tenant_id`, `bypass_data_preservation`, `create_new_file_version`, `data_timeout`, `connect_timeout`, `delay_inotify_processing`, `disable_permission_set`, `disable_version_check`, `disable_websocket_support`, `display_manager_integration`, `display_transfer_metrics`, `dns_timeout`, `drive_id`, `force_session_upload`, `inotify_delay`, `ip_protocol_version`, `max_curl_idle`, `notify_file_actions`, `operation_timeout`, `permanent_delete`, `rate_limit`, `read_only_auth_scope`, `recycle_bin_path`, `sync_business_shared_items`, `sync_dir_permissions`, `sync_file_permissions`, `transfer_order`, `use_device_auth`, `use_intune_sso`, `use_recycle_bin`, `user_agent`, `webhook_*`, `write_xattr_data`.

---

## Environment Variable Support

The reference implementation does **not** directly read configuration options from environment variables at the application level. However, it does rely on several environment variables for path resolution and runtime context:

- **`HOME`** -- Used to resolve `~` in paths. If `HOME` is not set (e.g., running as a systemd service), falls back to reading `SHELL` and `USER`, and ultimately defaults to `/root`.
- **`XDG_CONFIG_HOME`** -- If set, determines the base directory for user configuration (instead of `$HOME/.config`).
- **`XDG_RUNTIME_DIR`**, **`DBUS_SESSION_BUS_ADDRESS`** -- Checked for GUI notification support.
- **`XDG_SESSION_TYPE`**, **`WAYLAND_DISPLAY`**, **`DISPLAY`** -- Checked for display/desktop environment detection.
- **`XDG_CURRENT_DESKTOP`**, **`XDG_SESSION_DESKTOP`**, **`DESKTOP_SESSION`**, **`GDMSESSION`**, **`KDE_FULL_SESSION`** -- Checked for desktop environment integration (adding OneDrive to file manager sidebar).

For Docker/container deployments, environment variables like `ONEDRIVE_SINGLE_DIRECTORY` are typically handled by an `entrypoint.sh` script that translates them into config file entries or CLI flags before launching the application. The application itself detects container environments by checking for the presence of `/entrypoint.sh`.

---

## Multi-Config / Profile Support

The reference implementation supports running multiple independent instances by using the `--confdir` CLI option to point each instance at a different configuration directory. Each instance then has its own:

- Config file
- OAuth2 refresh token
- Item database (SQLite)
- Delta link state
- Sync list

This is the primary mechanism for syncing multiple OneDrive accounts (e.g., personal and business) on the same machine. Each instance runs as a separate process, typically managed via separate systemd service units.

There is no built-in "profile" abstraction -- it is simply the convention of using separate config directories.

---

## Sync List File

The `sync_list` file is a separate configuration file (not a key in the main config) that lives alongside the `config` file (i.e., in the same config directory). It contains a line-by-line list of paths to include in synchronization. When present and non-empty, only paths matching the entries in `sync_list` are synced -- everything else is excluded.

- The file is located at: `<config_dir>/sync_list`
- Changes to this file require a `--resync` to take effect.
- The companion option `sync_root_files` controls whether files in the root of `sync_dir` are also synced when `sync_list` is active.
- A hash of the sync_list file (`.sync_list.hash`) is maintained to detect changes.

---

## Configuration Options by Category

### Importance Levels

- **Critical** -- Must be implemented for a minimally viable sync client.
- **Important** -- Needed for a production-quality deployment.
- **Nice-to-have** -- Advanced, niche, or enterprise-specific features.

---

### 1. Authentication & Authorization

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `application_id` | String | `d50ca740-c83f-4d1b-b616-12c519384f0c` | No | The Microsoft Azure application (client) ID used for OAuth2 authentication. Users may register their own application in Azure Portal and substitute their own ID. An empty value in the config file is rejected; the default is restored. | Critical |
| `azure_ad_endpoint` | String | (empty) | No | Selects a national cloud deployment endpoint for authentication. Valid values: `USL4` (US Gov), `USL5` (US Gov DOD), `DE` (Germany), `CN` (China/21Vianet). When empty, the global Azure AD endpoint is used. Must be paired with `azure_tenant_id`. | Important |
| `azure_tenant_id` | String | (empty) | No | Locks the client to a specific Azure AD tenant rather than using the `common` multiplexer. Can be a GUID directory ID or a fully qualified tenant domain name. Required when `azure_ad_endpoint` is configured. | Important |
| `read_only_auth_scope` | Bool | `false` | No | When enabled, the client requests only read-only OAuth2 scopes. The application cannot upload or modify data. Existing consent must be revoked separately for this to take full effect. | Nice-to-have |
| `use_device_auth` | Bool | `false` | No | Enables the OAuth2 Device Authorization Flow (device code grant). The user is shown a code and URL to authenticate from a separate browser-enabled device. Intended for headless systems. Only works with Entra ID (work/school) accounts, not personal Microsoft accounts. | Important |
| `use_intune_sso` | Bool | `false` | No | Enables authentication via Microsoft Intune Single Sign-On through the Identity Device Broker over D-Bus. For Intune-enrolled Linux systems. | Nice-to-have |

---

### 2. Sync Behavior

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `download_only` | Bool | `false` | `--download-only` | Restricts the client to only downloading data from OneDrive. No local changes are uploaded. In this mode, local files deleted online are **not** cleaned up by default (making local data an archive). Use `cleanup_local_files` to enable cleanup. | Critical |
| `upload_only` | Bool | `false` | `--upload-only` | Restricts the client to only uploading local data to OneDrive. No remote changes are downloaded. By default, local deletions are replicated online; use `no_remote_delete` to prevent this. | Critical |
| `local_first` | Bool | `false` | `--local-first` | Changes the "source of truth" for sync. By default, the remote (OneDrive) state is authoritative. When enabled, local data takes precedence during conflict resolution. | Important |
| `no_remote_delete` | Bool | `false` | `--no-remote-delete` | Prevents the client from deleting files on OneDrive when they are deleted locally. Can only be used with `upload_only`. | Important |
| `resync` | Bool | `false` | `--resync` | Discards the locally saved sync state (delta token and database) and performs a full re-enumeration from OneDrive. Required after changing certain configuration items: `drive_id`, `sync_dir`, `skip_file`, `skip_dir`, `skip_dotfiles`, `skip_symlinks`, `sync_business_shared_items`, or the `sync_list` file. Increases API activity and may trigger HTTP 429 throttling. | Critical |
| `resync_auth` | Bool | `false` | `--resync-auth` | Automatically acknowledges the interactive "proceed with resync?" confirmation prompt. Useful in automated or containerized environments. | Important |
| `dry_run` | Bool | `false` | `--dry-run` | Simulates sync operations without making any actual changes (no downloads, uploads, moves, deletes, or folder creation). Uses a separate dry-run database. | Important |
| `cleanup_local_files` | Bool | `false` | `--cleanup-local-files` | When used with `download_only`, enables deletion of local files that have been removed online. Cannot be combined with other modes. | Important |
| `remove_source_files` | Bool | `false` | `--remove-source-files` | Deletes the local file after it has been successfully uploaded to OneDrive. Can only be used with `upload_only`. | Nice-to-have |
| `remove_source_folders` | Bool | `false` | `--remove-source-folders` | Removes local directory structures after all files within them have been successfully uploaded. Can only be used with `upload_only` and `remove_source_files`. Directories are only removed if empty. | Nice-to-have |
| `bypass_data_preservation` | Bool | `false` | No | Disables the default behavior of renaming local files to preserve data during conflicts. When enabled, local files are overwritten with the online version without creating a backup copy. Risk of local data loss. | Important |
| `sync_root_files` | Bool | `false` | `--sync-root-files` | When using a `sync_list`, also sync files located directly in the root of `sync_dir`. Without this, only paths explicitly listed in `sync_list` are synced. | Important |
| `create_new_file_version` | Bool | `false` | No | Controls behavior when SharePoint modifies uploaded files (PDF, MS Office, HTML) post-upload. Default behavior: re-download the modified file. When enabled: create a new online version instead. The new version counts against quota. | Nice-to-have |
| `force_session_upload` | Bool | `false` | No | Forces the use of upload sessions (rather than simple PUT) for all file uploads. Upload sessions include the local file timestamp, which prevents editors (vim, emacs, LibreOffice) from falsely detecting file modification after upload. Should be enabled alongside `delay_inotify_processing`. | Nice-to-have |
| `transfer_order` | String | `default` | No | Controls the order in which files are transferred. Valid values: `default` (FIFO order as received), `size_asc` (smallest first), `size_dsc` (largest first), `name_asc` (alphabetical A-Z), `name_dsc` (alphabetical Z-A). | Nice-to-have |
| `check_nomount` | Bool | `false` | `--check-for-nomount` | Checks for a `.nosync` file in the mount point of `sync_dir` before starting. If the file is found (indicating the disk is not mounted), sync is aborted. Protects against syncing to an unmounted path and triggering mass deletions online. | Nice-to-have |
| `check_nosync` | Bool | `false` | `--check-for-nosync` | Checks each local directory for a `.nosync` file. If found, that directory is excluded from sync. Only applies to local directories -- does not check online. | Nice-to-have |
| `permanent_delete` | Bool | `false` | No | When deleting items online, permanently deletes them instead of sending to the OneDrive Recycle Bin. Permanently deleted items cannot be restored. Only supported on Business and SharePoint accounts; not supported on Personal or US Government accounts. | Nice-to-have |
| `use_recycle_bin` | Bool | `false` | No | When files are deleted online, moves the local copies to a recycle bin directory instead of deleting them outright. Allows manual review before permanent local deletion. | Nice-to-have |
| `recycle_bin_path` | String | `~/.local/share/Trash/` | No | Path to the local recycle bin directory used when `use_recycle_bin` is enabled. Default follows GNOME/KDE convention with `files/` and `info/` subdirectories. | Nice-to-have |

---

### 3. Filtering

All filtering options are considered "Client Side Filtering Rules." Changes to any of these require a `--resync`.

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `skip_file` | String | `~*\|.~*\|*.tmp\|*.swp\|*.partial` | `--skip-file` | Pipe-delimited list of file name patterns to exclude from sync. Patterns are case-insensitive and support `*` and `?` wildcards. Files can be specified by wildcard (e.g., `*.txt`), by exact name (e.g., `filename.ext`), or by full path relative to `sync_dir` (e.g., `/path/to/file.ext`). Can appear multiple times in the config file; values are merged. CLI value replaces config file values entirely. If the user overrides the default, a warning is emitted if the default patterns (for temp/transient files) are missing. | Critical |
| `skip_dir` | String | (empty) | `--skip-dir` | Pipe-delimited list of directory name patterns to exclude from sync. Patterns are case-insensitive with `*` and `?` wildcard support. Can be a bare directory name (matches anywhere in the tree) or a full path relative to `sync_dir` (starting with `/`). Can appear multiple times in the config; values are merged. CLI value replaces config file values. | Critical |
| `skip_dir_strict_match` | Bool | `false` | `--skip-dir-strict-match` | When enabled, `skip_dir` patterns must be a full path match rather than matching anywhere in the directory tree. | Important |
| `skip_dotfiles` | Bool | `false` | `--skip-dot-files` | Excludes all files and directories whose names begin with `.` from sync operations. | Important |
| `skip_symlinks` | Bool | `false` | `--skip-symlinks` | Excludes all symbolic links from sync. OneDrive has no concept of symbolic links; attempting to upload one produces an API error. All uploaded content must be actual files or directories. | Important |
| `skip_size` | Int | `0` | `--skip-size N` | Skips files larger than the specified size in megabytes. A value of 0 means no size limit. | Important |

---

### 4. Performance

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `threads` | Int | `8` | `--threads N` | Number of worker threads for parallel upload and download operations. Maximum is 16. Only affects file transfer; all API metadata operations (listings, delta queries) are single-threaded. The default of 8 aligns with Microsoft Graph API guidance recommending 5-10 concurrent requests. Values exceeding available CPU cores trigger a warning. | Critical |
| `rate_limit` | Int | `0` | No | Bandwidth limit per thread in bytes per second. A value of 0 means unlimited. Tested values range from 131072 (128 KB/s, minimum to prevent timeouts) to 104857600 (100 MB/s). | Important |
| `file_fragment_size` | Int | `10` | `--file-fragment-size N` | Size (in MB) of each fragment when performing resumable upload sessions for large files. Minimum 10, maximum 60. Must be an exact multiple of 320 KiB (valid values: 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60). | Important |
| `space_reservation` | Int | `50` (MB) | `--space-reservation N` | Amount of local disk space (in MB) to keep reserved, preventing the client from filling the disk entirely. Stored internally as bytes (value * 2^20). Minimum effective value is 1 MB. | Important |

---

### 5. Monitoring

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `monitor_interval` | Int | `300` | `--monitor-interval N` | Number of seconds between sync cycles in monitor mode. Enforced minimum of 300 seconds (5 minutes). Each cycle checks for online changes, performs integrity checks on local data, and scans `sync_dir` for new uploads. | Critical |
| `monitor_fullscan_frequency` | Int | `12` | `--monitor-fullscan-frequency N` | Number of monitor intervals between full disk scans. By default, a full scan occurs every `300 * 12 = 3600` seconds (1 hour). Setting to 0 disables full scans entirely. Minimum non-zero value is 12. | Important |
| `monitor_log_frequency` | Int | `12` | `--monitor-log-frequency N` | Controls suppression of repetitive log output in monitor mode. After the initial sync, routine "Starting a sync / Sync complete" messages are suppressed until this many intervals have passed. Has no effect when `--verbose` is used. | Nice-to-have |
| `delay_inotify_processing` | Bool | `false` | No | When enabled, delays processing of filesystem inotify events. Designed to handle editors like Obsidian that perform atomic saves on every keystroke, generating rapid sequences of file delete/create events. Must be paired with `force_session_upload`. | Nice-to-have |
| `inotify_delay` | Int | `5` | No | Number of seconds to delay inotify event processing. Only used when `delay_inotify_processing` is enabled. Minimum 5, maximum 15. | Nice-to-have |
| `disable_websocket_support` | Bool | `false` | No | Disables the built-in WebSocket support (RFC 6455) that provides near-real-time notifications of online changes from the Microsoft Graph API. When disabled, the client relies solely on polling at `monitor_interval`. | Important |

---

### 6. Logging & Diagnostics

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `enable_logging` | Bool | `false` | `--enable-logging` | Enables writing application activity to a log file. By default, logs go to `/var/log/onedrive/` (configurable via `log_dir`). Additional system configuration may be needed for write access to the default log directory. | Important |
| `log_dir` | String | `/var/log/onedrive` | `--log-dir PATH` | Custom directory for log file output. Only used when `enable_logging` is true. An empty value in the config file is rejected; the default is restored. | Important |
| `debug_https` | Bool | `false` | `--debug-https` | Enables verbose libcurl output for HTTPS operations. Outputs the full HTTP request/response including headers. WARNING: This exposes the `Authorization: bearer` token in the output. | Nice-to-have |
| `display_running_config` | Bool | `false` | `--display-running-config` | Outputs the complete running configuration at application startup. Useful in containerized environments to capture the effective config in log files. Also enables display of developer-only options. | Nice-to-have |
| `display_transfer_metrics` | Bool | `false` | No | Displays file transfer metrics (file path, size, duration, speed in Mbps) after each file transfer completes. | Nice-to-have |
| `disable_version_check` | Bool | `false` | No | Disables the automatic check against the GitHub API for newer application versions and grace period warnings for running outdated versions. | Nice-to-have |
| `write_xattr_data` | Bool | `false` | No | Writes extended attributes (`xattr`) on downloaded files recording the `createdBy` and `lastModifiedBy` information from the OneDrive API. Attribute names: `user.onedrive.createdBy` and `user.onedrive.lastModifiedBy`. | Nice-to-have |

Verbosity is controlled via CLI:
- `--verbose` or `-v` once = verbose output
- `--verbose --verbose` or `-vv` = debug-level output

---

### 7. Paths & Directories

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `sync_dir` | String | `~/OneDrive` | `--syncdir PATH` | The local directory where OneDrive data is synchronized. Changes to this value require `--resync`. When changed via `--syncdir`, the new value is persisted to the config file. An empty value is a fatal error. | Critical |
| `sync_dir_permissions` | Int (octal) | `700` | No | POSIX permission mode applied to directories created during sync. Value is interpreted as octal (e.g., 700 = `drwx------`). | Important |
| `sync_file_permissions` | Int (octal) | `600` | No | POSIX permission mode applied to files created during sync. Value is interpreted as octal (e.g., 600 = `-rw-------`). | Important |
| `disable_permission_set` | Bool | `false` | No | When enabled, the application does not explicitly set permissions on files and directories. Instead, file system permission inheritance is used. Useful for file systems that do not support POSIX permissions. | Nice-to-have |

The config directory path itself is not a config-file option but is determined by:
- Default: `~/.config/onedrive`
- Override: `--confdir` CLI option
- Fallback: `$XDG_CONFIG_HOME/onedrive`

---

### 8. Network & HTTP

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `connect_timeout` | Int | `10` | No | TCP connection timeout in seconds for HTTPS connections (maps to `CURLOPT_CONNECTTIMEOUT`). | Important |
| `data_timeout` | Int | `60` | No | Timeout in seconds for when no data is received on an active HTTPS connection before the connection is timed out. | Important |
| `dns_timeout` | Int | `60` | No | libcurl DNS cache timeout in seconds. This is speculative caching (not based on DNS TTL). Not recommended to change unless strictly necessary. | Nice-to-have |
| `operation_timeout` | Int | `0` | No | Maximum total time in seconds for any complete network operation (DNS + connect + TLS + transfer). Maps to `CURLOPT_TIMEOUT`. A value of 0 means no limit. Setting a non-zero value can prematurely abort large file transfers on slow connections. Strongly recommended to leave at 0. | Nice-to-have |
| `ip_protocol_version` | Int | `0` | No | Controls IP protocol version: `0` = IPv4 + IPv6, `1` = IPv4 only, `2` = IPv6 only. Useful when dual-stack causes resolution/routing issues. Values above 2 are rejected; the default is restored. | Nice-to-have |
| `force_http_11` | Bool | `false` | `--force-http-11` | Forces libcurl to use HTTP/1.1 instead of the default (typically HTTP/2). | Nice-to-have |
| `max_curl_idle` | Int | `120` | No | Number of seconds a cURL connection handle can be idle before it is destroyed. Some upstream network devices forcibly close idle TCP connections. Not recommended to change without thorough network testing. | Nice-to-have |
| `user_agent` | String | `ISV\|abraunegg\|OneDrive Client for Linux/vX.Y.Z` | No | The User-Agent header sent with all Microsoft Graph API requests. Conforms to Microsoft ISV traffic decoration requirements. Changing this may affect how Microsoft classifies and throttles the client's traffic. | Important |

---

### 9. Data Integrity & Safety

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `classify_as_big_delete` | Int | `1000` | `--classify-as-big-delete N` | Threshold (number of child items) for detecting accidental mass deletions. If a local directory removal would delete more items than this threshold from OneDrive, sync is blocked until `--force` is used. Designed to prevent accidental data loss from misconfiguration. | Critical |
| `disable_download_validation` | Bool | `false` | `--disable-download-validation` | Disables integrity checking (size/hash comparison) of downloaded files. Sometimes necessary for SharePoint, where reported file sizes can differ from actual downloaded bytes. Also needed for Azure Information Protection (AIP) files. Disabling this risks silent data corruption. | Important |
| `disable_upload_validation` | Bool | `false` | `--disable-upload-validation` | Disables integrity checking of uploaded files. SharePoint may modify files (PDF, Office, HTML) post-upload, breaking integrity checks. If disabled, the client cannot determine if post-upload modification occurred, and defaults to assuming the integrity check failed. | Important |

---

### 10. GUI & Desktop Integration

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `disable_notifications` | Bool | `false` | `--disable-notifications` | Disables GUI desktop notifications (e.g., via D-Bus / notification daemon). | Nice-to-have |
| `notify_file_actions` | Bool | `false` | No | When enabled, sends GUI notifications for successful individual file actions (upload, download, delete). Requires compilation with notification support (`--enable-notifications` build flag). | Nice-to-have |
| `display_manager_integration` | Bool | `false` | No | Integrates the `sync_dir` with the desktop file manager (Nautilus, Dolphin, etc.), adding it as a sidebar bookmark and setting a custom OneDrive folder icon. | Nice-to-have |

---

### 11. Webhooks

Webhooks allow the client (in monitor mode) to receive near-real-time push notifications from Microsoft OneDrive about remote changes, instead of relying solely on polling.

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `webhook_enabled` | Bool | `false` | No | Enables the webhook subscription feature. Only operates in monitor mode. The client listens for incoming HTTP notifications at the configured host/port. | Nice-to-have |
| `webhook_public_url` | String | (empty) | No | The internet-accessible HTTPS URL that Microsoft will send subscription notifications to. Must use a valid (non-self-signed) HTTPS certificate. Required when `webhook_enabled` is true. Example: `https://example.com/webhooks/onedrive` | Nice-to-have |
| `webhook_listening_host` | String | `0.0.0.0` | No | The local IP address the webhook listener binds to. | Nice-to-have |
| `webhook_listening_port` | Int | `8888` | No | The local TCP port the webhook listener binds to. | Nice-to-have |
| `webhook_expiration_interval` | Int | `600` | No | Number of seconds before the Microsoft webhook subscription expires. | Nice-to-have |
| `webhook_renewal_interval` | Int | `300` | No | Number of seconds between subscription renewal attempts. | Nice-to-have |
| `webhook_retry_interval` | Int | `60` | No | Number of seconds to wait before retrying a failed subscription creation or renewal. | Nice-to-have |

---

### 12. Advanced / Enterprise

| Name | Type | Default | CLI | Description | Importance |
|------|------|---------|-----|-------------|------------|
| `sync_business_shared_items` | Bool | `false` | No | Enables syncing of OneDrive Business / Office 365 Shared Folders that have been added as shortcuts to "My Files." This is a client-side filtering rule and requires `--resync` after enabling. Not backward-compatible with v2.4.x -- all instances must be on v2.5.x or later. | Important |

Related CLI-only options for shared item operations:
- `--list-shared-items` -- lists available shared items
- `--sync-shared-files` -- syncs shared files (requires `sync_business_shared_items` enabled)
- `--get-sharepoint-drive-id QUERY` -- queries and returns the drive ID for a SharePoint document library

The reference implementation creates a default directory called `Files Shared With Me` for synced shared files.

---

### 13. Developer / Debug Only

These options are intended for development and debugging and are not documented in user-facing materials. They are config-file options only.

| Name | Type | Default | Description |
|------|------|---------|-------------|
| `display_memory` | Bool | `false` | Displays application memory usage. Useful for diagnosing memory issues or running Valgrind. |
| `monitor_max_loop` | Int | `0` | Forces monitor mode to exit after N sync loops. Useful for automated testing and memory analysis. 0 = infinite (normal behavior). |
| `display_sync_options` | Bool | `false` | Displays what parameters are being passed into the internal `performSync()` function without enabling full verbose debug logging. |
| `force_children_scan` | Bool | `false` | Forces the client to use `/children` API calls instead of `/delta` for change detection. Simulates behavior required for national cloud deployments. |
| `display_processing_time` | Bool | `false` | Adds function execution timing to console output for performance profiling. |

---

## CLI-Only Options (Not in Config File)

These options are only available as command-line flags and cannot be set in the config file. They are typically operational commands rather than persistent configuration.

| CLI Flag | Description | Importance |
|----------|-------------|------------|
| `--confdir PATH` | Sets the configuration directory for this invocation. Must be specified every time. Enables multi-account support. | Critical |
| `--sync` / `-s` | Performs a one-time (standalone) sync operation. | Critical |
| `--monitor` / `-m` | Runs in continuous monitor mode, performing ongoing sync cycles. | Critical |
| `--verbose` / `-v` | Increases output verbosity. Can be repeated (`-vv`) for debug-level detail. | Important |
| `--version` | Prints the application version and exits. | Important |
| `--display-config` | Displays the effective (merged) configuration and exits without syncing. | Important |
| `--display-sync-status` | Displays the sync status of the configured `sync_dir` and exits. Can be combined with `--single-directory`. | Nice-to-have |
| `--display-quota` | Displays the storage quota status for the configured drive. | Nice-to-have |
| `--auth-files authUrl:responseUrl` | Non-interactive authentication via file exchange. The client writes the auth URL to the first file and reads the response URI from the second. | Important |
| `--auth-response URI` | Non-interactive authentication by directly providing the OAuth2 response URI. | Important |
| `--logout` | Removes the client's authentication state (refresh token). | Important |
| `--reauth` | Re-authenticates the client with OneDrive. | Important |
| `--print-access-token` | Prints the current OAuth2 access token. Security-sensitive. | Nice-to-have |
| `--force` | Allows a sync to proceed when a "big delete" has been detected. | Important |
| `--force-sync` | Syncs a specific directory using default filtering rules, overriding user skip_dir/skip_file settings. Requires `--sync --single-directory`. Interactive risk acceptance prompt. | Nice-to-have |
| `--single-directory PATH` | Restricts sync to a specific subdirectory within `sync_dir`. Path is relative to `sync_dir`. | Important |
| `--download-file PATH` | Downloads a single file by its online path, without performing a full sync. | Nice-to-have |
| `--create-directory PATH` | Creates a directory on OneDrive without syncing. Path is relative to `sync_dir`. | Nice-to-have |
| `--remove-directory PATH` | Removes a directory on OneDrive without syncing. Path is relative to `sync_dir`. | Nice-to-have |
| `--source-directory PATH` | Specifies the source path for an online move/rename operation (used with `--destination-directory`). | Nice-to-have |
| `--destination-directory PATH` | Specifies the destination path for an online move/rename operation (used with `--source-directory`). | Nice-to-have |
| `--create-share-link PATH` | Creates a shareable link for an existing file on OneDrive. Default is read-only. | Nice-to-have |
| `--with-editing-perms` | Used with `--create-share-link` to create a read-write link instead of read-only. Must come after the file path. | Nice-to-have |
| `--share-password PASSWORD` | Sets a password requirement on a shared link. Used with `--create-share-link`. | Nice-to-have |
| `--get-file-link PATH` | Returns the WebURL for a synced local file. Path is relative to `sync_dir`. | Nice-to-have |
| `--modified-by PATH` | Returns the last-modified-by details for a local file. Path is relative to `sync_dir`. | Nice-to-have |
| `--get-sharepoint-drive-id QUERY` | Queries OneDrive API for a SharePoint library's drive ID. Use `'*'` to list all. | Nice-to-have |
| `--list-shared-items` | Lists all OneDrive Business shared items available to the account. | Nice-to-have |
| `--sync-shared-files` | Syncs OneDrive Business shared files to local filesystem. Requires `sync_business_shared_items` to be enabled. | Nice-to-have |

---

## Deprecated Options

These options are recognized but ignored or rejected:

| Name | Type | Replacement | Notes |
|------|------|-------------|-------|
| `force_http_2` | Bool (config) / `--force-http-2` (CLI) | Removed | HTTP/2 is now used by default where the API supports it. The option is silently ignored with a deprecation message. |
| `min_notify_changes` | Int (config) / `--min-notify-changes N` (CLI) | Removed | Was used to throttle desktop notifications. Silently ignored with a deprecation message after the application was rewritten. |
| `sync_business_shared_folders` | Config only | `sync_business_shared_items` | The shared folder sync mechanism was completely redesigned. Presence of this key is a fatal error requiring the user to update their configuration and local setup. |
| `--synchronize` | CLI only | `--sync` / `-s` | Deprecated alias. Still accepted but emits a deprecation warning. |
| `--get-O365-drive-id` | CLI only | `--get-sharepoint-drive-id` | Deprecated alias. Still accepted. |

---

## Summary of Critical Options for MVP

The following options represent the minimum set needed for a functional sync client:

1. **`application_id`** -- OAuth2 client identity
2. **`sync_dir`** -- where to sync
3. **`skip_file`** / **`skip_dir`** -- basic filtering
4. **`download_only`** / **`upload_only`** -- sync direction control
5. **`resync`** -- state reset mechanism
6. **`classify_as_big_delete`** -- safety guard against mass deletion
7. **`threads`** -- parallel transfer performance
8. **`monitor_interval`** -- polling frequency in monitor mode
9. **`--confdir`** -- multi-account support
10. **`--sync`** / **`--monitor`** -- operational mode selection

The config file format itself (custom key=value with double-quoted values, comment support, multi-value merging for skip rules) must also be implemented for compatibility.
