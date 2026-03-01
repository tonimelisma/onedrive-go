# Account & Drive System Design

## Core Concepts

**Accounts** are what you authenticate with (Microsoft accounts). You `login` and `logout` of accounts. An account is identified by its email address.

**Drives** are what you sync. Each drive has a canonical identifier derived from real data — no arbitrary names. Types:

- **Personal OneDrive** — one per personal Microsoft account
- **OneDrive for Business** — one per business/work account
- **SharePoint document library** — many per business account (same token)
- **Shared folders** — folders shared by other users, synced as separate drives

A single business login grants access to the OneDrive for Business drive AND all SharePoint document libraries the user can access. No separate SharePoint authentication.

---

## 1. What Needs Naming

| # | Thing | Where it appears | Example |
|---|---|---|---|
| 1 | **Canonical drive ID** | Internal key, config section, display | `personal:toni@outlook.com` |
| 2 | **Token file** | Data directory | `token_personal_toni@outlook.com.json` |
| 3 | **State DB file** | Data directory | `state_personal_toni@outlook.com.db` |
| 4 | **Config section** | `config.toml` | `["personal:toni@outlook.com"]` |
| 5 | **`--account` value** | CLI flag (auth) | `alice@contoso.com` |
| 5b | **`--drive` value** | CLI flag (drives) | `"me@outlook.com"`, `"Jane's Photos"`, `"marketing"` |
| 6 | **Sync directory** | On disk, in config | `~/OneDrive - Contoso` |
| 7 | **Display name** | Per-drive config, CLI, logs, errors | `display_name = "Jane's Photos"` |

---

## 2. Canonical Drive Identifier

### Format

```
personal:<email>
business:<email>
sharepoint:<email>:<site>:<library>
shared:<email>:<sourceDriveID>:<sourceItemID>
```

### Examples

```
personal:toni@outlook.com
business:alice@contoso.com
sharepoint:alice@contoso.com:marketing:Documents
sharepoint:alice@contoso.com:hr:Policies
shared:me@outlook.com:b!TG9yZW0:01ABCDEF
```

### Rules

- **Constructed from real data.** After login, we know the drive type and email from the Graph API. For SharePoint, site and library names come from the Sites API.
- **`:` is the separator** in display/config form. Replaced with `_` in filenames (see section 3).
- **No "default" or synthetic names.** Every drive has a real identity from the moment it's added.
- **Email comes from `mail` field** with `userPrincipalName` as fallback. For guest/B2B users, `mail` contains their real email (e.g., `alice@partner.com`), not the ugly UPN (`alice_partner.com#EXT#@contoso.com`). Users never see the UPN unless `mail` is empty.

### Where the data comes from

| Component | Source | API |
|---|---|---|
| Drive type | Auth endpoint + drive type | `GET /me/drive` -> `driveType` |
| Email | User identity | `GET /me` -> `mail` (fallback: `userPrincipalName`) |
| User GUID | Stable identifier | `GET /me` -> `id` (stored for email change detection) |
| Site name | SharePoint site URL slug | `GET /sites/{id}` -> `name` |
| Library name | Document library display name | `GET /sites/{id}/drives` -> drive `name` |
| Source drive ID | Shared-with-me or shortcut | `sharedWithMe` API -> `remoteItem.driveId` |
| Source item ID | Shared-with-me or shortcut | `sharedWithMe` API -> `remoteItem.id` |

### Drive type auto-detection

Personal vs business is auto-detected. No `--personal` or `--business` flags needed:

| Auth endpoint | `driveType` | Drive type |
|---|---|---|
| Consumer endpoint | `personal` | `personal` |
| Work/school endpoint | `business` | `business` |
| Work/school endpoint | `documentLibrary` | `sharepoint` |
| Consumer OR Work/school endpoint | (shared-with-me API) | `shared` |

For shared drives, the canonical ID is opaque to humans — users interact via display names.

---

## 3. File Layout

All data files in one flat directory. No nested folders. The `:` separator from canonical IDs is replaced with `_` in filenames.

```
~/.local/share/onedrive-go/
  config.toml
  onedrive-go.log
  token_personal_toni@outlook.com.json
  token_business_alice@contoso.com.json
  state_personal_toni@outlook.com.db
  state_business_alice@contoso.com.db
  state_sharepoint_alice@contoso.com_marketing_Documents.db
  state_sharepoint_alice@contoso.com_hr_Policies.db
```

### Filename mapping

| Canonical ID | Token file | State DB |
|---|---|---|
| `personal:<email>` | `token_personal_<email>.json` | `state_personal_<email>.db` |
| `business:<email>` | `token_business_<email>.json` | `state_business_<email>.db` |
| `sharepoint:<email>:<site>:<lib>` | (shares `token_business_<email>.json`) | `state_sharepoint_<email>_<site>_<lib>.db` |
| `shared:<email>:<did>:<iid>` | (shares primary drive's token) | `state_shared_<email>_<did>_<iid>.db` |

Filenames are always derived FROM the canonical ID in the config file. We never parse filenames to discover drives — config is the source of truth.

Example layout with a shared drive:

```
~/.local/share/onedrive-go/
  config.toml
  token_personal_me@outlook.com.json
  state_personal_me@outlook.com.db
  state_shared_me@outlook.com_b!TG9yZW0_01ABCDEF.db
```

### Token file format

Token files contain the OAuth token AND cached API metadata in a single JSON file:

```json
{
  "token": {
    "access_token": "...",
    "refresh_token": "...",
    "token_type": "Bearer",
    "expiry": "2026-02-27T..."
  },
  "meta": {
    "user_id": "abc123-def456",
    "display_name": "Alice Smith",
    "org_name": "Contoso Ltd",
    "cached_at": "2026-02-27T10:00:00Z"
  }
}
```

Metadata fields:
- `user_id` — Azure AD user GUID (stable identifier)
- `display_name` — user's display name (for friendly status output and sync_dir naming)
- `org_name` — organization name (for business sync_dir computation: `~/OneDrive - {org_name}`)
- `cached_at` — timestamp of last metadata refresh

Every login (including re-login) refreshes both the token AND all cached metadata. Old bare `oauth2.Token` files (without the `token`/`meta` wrapper) are rejected — re-login is required.

### SharePoint token sharing

SharePoint drives share the OAuth token with the business account (same user, same session, same scopes). Only the state DB is per-drive. `token_business_alice@contoso.com.json` serves both her OneDrive for Business and all her SharePoint drives.

Shared drives share the OAuth token with their primary drive (personal or business). Token resolution is handled by `config.TokenCanonicalID()`, which finds the account type (personal or business) for the email and returns the corresponding token's canonical ID.

### Platform paths

| Platform | Config | Data + logs |
|---|---|---|
| Linux | `~/.config/onedrive-go/config.toml` | `~/.local/share/onedrive-go/` |
| macOS | `~/Library/Application Support/onedrive-go/config.toml` | `~/Library/Application Support/onedrive-go/` |

Config and data may share the same directory on macOS. On Linux, XDG conventions separate them.

---

## 4. Config File

### Layout

Flat global settings at the top. Drive sections identified by `:` in the section name (TOML requires quotes around section names containing `:` and `@` — this is a TOML spec requirement, not our choice).

```toml
# ── Global settings ──
log_level = "info"
poll_interval = "5m"
# Note: filter settings (skip_dotfiles, skip_dirs, skip_files, etc.) are
# per-drive only — configure them inside each drive section below. (DP-8)

# ── Drives (any section with ":" is a drive) ──

["personal:toni@outlook.com"]
display_name = "toni@outlook.com"
sync_dir = "~/OneDrive"
skip_dotfiles = true
skip_dirs = ["node_modules", ".git"]
skip_files = ["*.tmp", "~*"]

["business:alice@contoso.com"]
display_name = "alice@contoso.com"
sync_dir = "~/OneDrive - Contoso"
skip_dirs = ["node_modules", ".git", "vendor"]

["sharepoint:alice@contoso.com:marketing:Documents"]
display_name = "Marketing / Documents"
sync_dir = "~/Contoso/Marketing - Documents"
paused = true

["shared:me@outlook.com:b!TG9yZW0:01ABCDEF"]
display_name = "Jane's Photos"
owner = "jane@outlook.com"
sync_dir = "~/OneDrive-Shared/Jane's Photos"
```

Quotes around section names are required by TOML because `@` and `:` are not valid bare key characters. This is unavoidable with email-based identifiers.

### Global settings

| Key | Type | Default | Description |
|---|---|---|---|
| `log_level` | string | `"info"` | Log file verbosity: `debug`, `info`, `warn`, `error` |
| `log_file` | string | (platform default) | Log file path override |
| `poll_interval` | string | `"5m"` | Check interval for `sync --watch` |

> **Note**: Filter settings (`skip_dotfiles`, `skip_dirs`, `skip_files`, etc.) are per-drive only — there are no global filter defaults. See [MULTIDRIVE.md §10](MULTIDRIVE.md#10-filter-scoping) (DP-8).

### Per-drive settings

| Key | Type | Required | Default | Description |
|---|---|---|---|---|
| `sync_dir` | string | No | (computed) | Where to sync. Must be unique across drives. Auto-computed from canonical ID + token metadata if omitted. |
| `paused` | bool | No | `false` | `true` = paused (pause command sets this; replaces old `enabled` field) |
| `paused_until` | string | No | — | RFC3339 timestamp for timed pause expiry (e.g., `2026-02-28T18:00:00Z`) |
| `display_name` | string | No | (auto-derived) | Human-facing name. Auto-generated at drive add time, user-editable. |
| `owner` | string | No | — | Owner's email (shared drives only) |
| `sync_vault` | bool | No | `false` | Include Personal Vault items in sync (dangerous — vault auto-lock can cause local deletes) |
| `remote_path` | string | No | `"/"` | Remote subfolder to sync |
| `drive_id` | string | No | auto | Explicit drive ID (auto-detected for personal/business) |
| `skip_dotfiles` | bool | No | `false` | Skip files/dirs starting with `.` (per-drive native, DP-8) |
| `skip_dirs` | string[] | No | `[]` | Directory names to skip (per-drive native) |
| `skip_files` | string[] | No | `[]` | File name patterns to skip (per-drive native) |
| `poll_interval` | string | No | global | Per-drive override of global poll_interval |

Filter settings are per-drive native (no global defaults to inherit from). Non-filter per-drive settings override globals when specified.

> **Note:** The Go `Drive` struct definition and display_name derivation rules are specified in [MULTIDRIVE.md §2](MULTIDRIVE.md#2-configuration--naming).

### Auto-creation and modification

Config is auto-created by `login` and modified by `drive add`/`drive remove`/`pause`/`resume`:

- `login` -> creates config with drive section (if new account)
- `drive add` -> appends fresh drive section (state DB provides fast re-sync if it exists)
- `drive remove` -> deletes section from config (state DB kept for fast re-add)
- `drive remove --purge` -> removes section + state DB
- `pause` -> sets `paused = true` (+ optional `paused_until`) in config
- `resume` -> removes `paused` / `paused_until` from config
- `logout` -> deletes token + config sections for all account drives (state DBs kept)
- `logout --purge` -> deletes token + config sections + state DBs for all affected drives
- `sync --watch` watches config.toml via fsnotify; changes take effect within milliseconds

Manual editing always supported. The tool reads config fresh on each run. In watch mode, fsnotify provides immediate pickup.

See [MULTIDRIVE.md §11.10](MULTIDRIVE.md#1110-drive-lifecycle) for the full drive lifecycle specification including pause/resume and timed pause expiry.

### Config file management: text-level manipulation

TOML libraries strip comments on round-trip (parse → modify → serialize). To preserve all comments — both the initial defaults and any the user adds — the app **never round-trips the config through a TOML serializer**.

- **Read path**: normal TOML parsing (parser ignores comments, that's fine)
- **Write path**: surgical line-based text edits — never serialize the whole document

This is how `sshd_config`, `nginx.conf`, and most Unix tools handle commented config files.

#### Initial creation (first `login`)

On first login, the app writes a complete config from a template string constant baked into the code. All global settings are present as commented-out defaults, so users can discover every option without reading docs:

```toml
# onedrive-go configuration
# Docs: https://github.com/tonimelisma/onedrive-go

# ── Global settings ──
# Uncomment and modify to override defaults.

# Log file verbosity: debug, info, warn, error
# log_level = "info"

# Log file path (default: platform standard location)
# log_file = ""

# Check interval for sync --watch
# poll_interval = "5m"

# ── Drives ──
# Added automatically by 'login' and 'drive add'.
# Filter settings (skip_dotfiles, skip_dirs, skip_files, etc.) are
# per-drive only — configure them inside each drive section below.
# Each section name is the canonical drive identifier.

["personal:toni@outlook.com"]
display_name = "toni@outlook.com"
sync_dir = "~/OneDrive"
```

#### Write operations

Each modification is a small, testable function that operates on the file as lines of text:

| Operation | When | How |
|-----------|------|-----|
| Append drive section | `login`, `drive add` | Append new `["type:email"]` block at end of file |
| Delete section | `drive remove` | Find section header, delete lines through next header or EOF |
| Delete section | `drive remove --purge` | Find section header, delete lines through next header or EOF |
| Set `paused = true` | `pause` | Find section header, find or insert `paused` key |
| Remove `paused` | `resume` | Find section header, delete `paused` and `paused_until` lines |
| Rename section header | Email change detection | Find-and-replace one `["old"]` → `["new"]` line |

User comments survive every operation because no line is touched unless it's the specific target of the edit.

---

## 5. Logging

### Two independent channels

| Channel | What controls it | Default |
|---|---|---|
| **Console** (stderr) | CLI flags | Operational summaries |
| **Log file** | `log_level` in config | `info` level, platform-default path |

### Console verbosity (CLI flags)

| Flag | Output | Use case |
|---|---|---|
| `--quiet` / `-q` | Errors only | Cron jobs, scripts |
| (default) | Summaries: "Synced 42 files" | Normal interactive use |
| `--verbose` / `-v` | Individual file operations | Watching sync progress |
| `--debug` | HTTP requests, tokens, internals | Troubleshooting |

### Log file

Always writes to platform-default location:
- Linux: `~/.local/share/onedrive-go/onedrive-go.log`
- macOS: `~/Library/Application Support/onedrive-go/onedrive-go.log`

Overridable via `log_file` in config. Level controlled by `log_level` in config. Console flags and log file level are independent.

### Service mode (systemd / launchd)

`sync --watch` runs in the foreground. No `--daemon` flag, no fork/detach — modern service managers handle process lifecycle:

- **systemd**: captures stdout/stderr to journal automatically. Query with `journalctl --user -u onedrive-go`.
- **launchd**: stdout/stderr configured in the plist file.

Default console output is fine for services. The log file provides persistent history. No special flags needed. Run `onedrive-go service install` to generate the service file (see section 13).

---

## 6. CLI Commands

### Complete command list

```
Authentication:
  login [--browser]         Sign in + auto-add primary drive (device code by default)
  logout [--purge]          Sign out (--purge: also delete state DBs + config sections)
  whoami                    Show authenticated accounts

Drive management:
  drive list                Show configured drives + available drives from network (including shared-with-me folders)
  drive add <name>          Add a new drive by display name or canonical ID
  drive remove [--purge]    Remove a drive (deletes config section; --purge: also delete state DB)
  drive search <term>       Search SharePoint sites by name (shared-with-me folders discoverable via drive list)
  pause [--drive X] [dur]   Pause sync for a drive or all drives (optional duration, e.g., "2h")
  resume [--drive X]        Resume a paused drive or all drives

Sync:
  sync                      One-shot bidirectional sync, exits when done
  sync --watch              Continuous sync — stays running, re-syncs on interval
  sync --download-only      One-way: download remote changes only
  sync --upload-only        One-way: upload local changes only
  status                    Show all accounts, drives, and sync state
  conflicts                 List unresolved conflicts with details
  resolve <id|path>         Resolve a conflict
  verify                    Re-hash local files, compare to DB and remote

File operations:
  ls [path]                 List remote files
  get <remote> [local]      Download file
  put <local> [remote]      Upload file
  rm <path>                 Delete remote file/folder
  mkdir <path>              Create remote directory
  stat <path>               Show file/folder metadata

Service:
  service install           Generate and install systemd/launchd service file (does NOT enable)
  service uninstall         Remove the installed service file
  service status            Show whether service is installed, enabled, running

Configuration:
  setup                     Interactive guided configuration (menu-driven)

Migration:
  migrate                   Auto-detect and migrate from abraunegg or rclone
  migrate --from abraunegg  Explicitly migrate from abraunegg/onedrive
  migrate --from rclone     Explicitly migrate from rclone OneDrive remote
```

### What each command affects

| Command | Token | State DB | Config | Sync dir on disk |
|---|---|---|---|---|
| `login` | Created | — | Section created | — |
| `logout` | **Deleted** | Kept | **Sections deleted** | Untouched |
| `logout --purge` | **Deleted** | **Deleted** | **Sections deleted** | Untouched |
| `drive add` | — (reuses existing) | — | Section created | — |
| `drive remove` | Kept | Kept | **Section deleted** | Untouched |
| `drive remove --purge` | Kept (unless orphaned) | **Deleted** | **Section deleted** | Untouched |
| `pause` | — | — | `paused = true` set | — |
| `resume` | — | — | `paused` removed | — |
| `sync` | May refresh | Updated | — | Files synced |
| `status` | — | Read | Read | — |
| `conflicts` | — | Read | — | — |
| `resolve` | May refresh | Updated | — | May modify files |
| `verify` | May refresh | Read | — | — |
| `service install` | — | — | — | — (writes service file) |
| `service uninstall` | — | — | — | — (removes service file) |
| `setup` | — | — | Modified | — |
| `migrate` | — | Created | Created | — |

**Sync directories on disk are NEVER deleted by any command.** They contain the user's files. All remove/logout/purge commands explicitly state: "Sync directory kept — delete manually if desired."

### Global CLI flags

| Flag | Used on | Type | Description |
|---|---|---|---|
| `--account` | `login`, `logout` | string | Select account by email or partial email |
| `--drive` | Everything else | string (repeatable for sync/status) | Select drive by canonical ID, display name, or partial match |
| `--config` | All | string | Config file path override |
| `--verbose` / `-v` | All | bool | Show individual file operations |
| `--debug` | All | bool | Show HTTP requests, internal state |
| `--quiet` / `-q` | All | bool | Errors only |
| `--json` | All | bool | Machine-readable output |
| `--dry-run` | `sync`, file ops | bool | Show what would happen |

### Sync-specific flags

| Flag | Description |
|---|---|
| `--watch` | Continuous sync — stay running, re-sync on `poll_interval` |
| `--download-only` | One-way: download remote changes only |
| `--upload-only` | One-way: upload local changes only |

### Login-specific flags

| Flag | Description |
|---|---|
| `--browser` | Use authorization code flow (opens browser, localhost callback). Better UX on desktop. |

---

## 7. `--account` and `--drive` Flags

Two flags for two concepts:

- **`--account`** — identifies a Microsoft account by email. Used on auth commands (`login`, `logout`).
- **`--drive`** — identifies a specific drive by canonical ID, display name, or partial match. Used on everything else.

### `--account` (auth commands)

Matches against account emails:

```bash
onedrive-go login --account alice@contoso.com     # re-authenticate specific account
onedrive-go logout --account alice@contoso.com    # remove token, affects all drives under this email
onedrive-go logout --account alice                # partial email match (if unambiguous)
```

`logout --account alice@contoso.com` affects ALL drives using that account's token (business + SharePoint). No confirmation prompt — just do it and report.

### `--drive` (drive commands)

Matches against canonical drive IDs and display names. Resolution order:

1. **Exact canonical ID**: `--drive "personal:me@outlook.com"` -> direct hit
2. **Exact display_name** (case-insensitive): `--drive "Jane's Photos"` -> matches display_name exactly
3. **Substring match** on canonical ID, display_name, or owner: `--drive jane`, `--drive personal`, `--drive photos`

```bash
onedrive-go sync --drive "personal:me@outlook.com"   # exact canonical
onedrive-go sync --drive personal                     # substring on canonical
onedrive-go sync --drive "Jane's Photos"              # exact display_name
onedrive-go sync --drive jane                         # substring on display_name/owner
```

If ambiguous -> error with suggestions showing display names and canonical IDs.

### Discoverability

Users learn about matching through:

**After login:**
```
Drive added: me@outlook.com -> ~/OneDrive
Use with: --drive "me@outlook.com"  or  --drive personal
```

**In error messages (ambiguous match):**
```
Error: "alice" matches multiple drives:
  me@contoso.com (~/OneDrive - Contoso)
  Marketing / Documents (~/Contoso/Marketing - Documents)

Try a more specific match:
  --drive "me@contoso.com"        (OneDrive for Business)
  --drive "Marketing / Documents" (SharePoint library)
```

**In `--help` output:**
```
--drive string     Select a drive. Matches by canonical ID, display name, or substring:
                     --drive "me@outlook.com"
                     --drive personal
                     --drive "Jane's Photos"
```

### Auto-selection (when `--drive` is omitted)

Paused state is a **sync concept only**. File operations (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`) work on all drives in config including paused ones — the token is still valid, the drive still exists. Removed drives are not visible to any command.

**File operations** (single-target):

| State | Behavior |
|---|---|
| No accounts (no tokens) | Error: "No accounts. Run: onedrive-go login" |
| 1 drive (paused or not) | Auto-select |
| 2+ drives | Error with list of all drives and shortest identifiers |

**Sync** (multi-target):

| State | Behavior |
|---|---|
| No accounts | Error: "No accounts. Run: onedrive-go login" |
| Accounts exist, all drives paused | "All drives paused. Run: onedrive-go resume" |
| Some not paused | Sync non-paused drives only |

**Status**: shows all drives in config (ready, paused, token expired). Removed drives are not shown.

### Repeatable for multi-target commands

```bash
onedrive-go sync                                                    # all non-paused drives
onedrive-go sync --drive personal                                   # just one (substring)
onedrive-go sync --drive "me@outlook.com" --drive "me@contoso.com" # two of three
onedrive-go status --drive "me@contoso.com"                        # status for one drive
```

---

## 8. Sync Directory Naming

Following Microsoft's OneDrive client conventions:

### Defaults

| Drive type | Default sync directory | Source |
|---|---|---|
| Personal OneDrive | `~/OneDrive` | Fixed (Microsoft uses `~/OneDrive - Personal` but we simplify for the 90% single-account case) |
| OneDrive for Business | `~/OneDrive - {OrgName}` | `GET /me/organization` -> `displayName` |
| SharePoint library | `~/{OrgName}/{SiteName} - {LibraryName}` | Microsoft convention: org as parent dir, site-library as subfolder |
| Shared folder | `~/OneDrive-Shared/{display_name}` | Derived from shared drive's display_name (e.g., `~/OneDrive-Shared/Jane's Photos`) |

### Microsoft's actual convention

Microsoft's official OneDrive desktop client uses:
- Personal: `~/OneDrive - Personal` (always, even if only account)
- Business: `~/OneDrive - {OrgName}` (e.g., `~/OneDrive - Contoso`)
- SharePoint: `~/{OrgName}/{SiteName} - {LibraryName}` (e.g., `~/Contoso/Marketing - Documents`)

SharePoint content lives in a **sibling directory** to the OneDrive folder, under the org name as parent. It is NOT prefixed with "OneDrive."

### Our defaults

We follow Microsoft's convention with one simplification: personal OneDrive defaults to `~/OneDrive` (not `~/OneDrive - Personal`) because 90% of users only have one account.

If a user has both personal and business, they get:
```
~/OneDrive/                              <- personal
~/OneDrive - Contoso/                    <- business
~/Contoso/Marketing - Documents/         <- SharePoint
```

### Rules

- **Login auto-picks the default.** No interactive prompt for sync dir. Output tells the user what was chosen and how to change it.
- **Never rename existing directories.**
- **Sync dirs must be unique.** Two drives cannot share a directory.
- **OrgName sanitization.** Strip characters unsafe in paths (`:`, `/`, `\`, null). Keep spaces, dashes, periods.
- **Collision handling.** If `~/OneDrive` is already taken by another drive, fall back to `~/OneDrive - Personal`.

### Changing sync directory

Manual process (Unix way):
1. Rename directory on disk: `mv ~/OneDrive ~/PersonalCloud`
2. Update config: `sync_dir = "~/PersonalCloud"` (or use `setup`)
3. Run sync — works because state DB stores paths relative to sync root

If config is updated without moving files -> new empty dir -> big-delete safety check fires. Correct behavior.

---

## 9. Login Flow

`login` is **interactive** — it blocks while the user authenticates in a browser. No config prompts though — assumes defaults for sync dir and settings, tells the user what was done.

### Auth methods

| Method | Flag | How it works | Best for |
|--------|------|-------------|----------|
| **Device code** (default) | (none) | Prints code + URL, user visits URL in any browser, CLI polls until complete | Headless, SSH, CI bootstrap, containers |
| **Auth code + localhost** | `--browser` | Opens browser, starts `http://127.0.0.1:<port>/callback` listener, Microsoft redirects back | Desktop users (fewer steps) |

Both methods block until auth completes or times out. If killed (ctrl-C, crash), no state is corrupted — the device code expires, or the localhost listener stops. Run login again.

### Machine-readable login (`--json`)

For GUIs, scripts, or other tools that need to drive the login flow programmatically, `--json` outputs newline-delimited JSON events on stdout:

```json
{"event": "device_code", "url": "https://microsoft.com/devicelogin", "code": "ABCD-EFGH", "expires_in": 900}
{"event": "login_complete", "email": "toni@outlook.com", "drive_type": "personal", "drive_id": "personal:toni@outlook.com", "sync_dir": "~/OneDrive"}
```

Or with `--browser --json`:

```json
{"event": "auth_url", "url": "https://login.microsoftonline.com/..."}
{"event": "login_complete", "email": "toni@outlook.com", "drive_type": "personal", "drive_id": "personal:toni@outlook.com", "sync_dir": "~/OneDrive"}
```

A GUI reads the first event, presents the code or opens the URL in a webview, then waits for the completion event. The CLI is the API — no RPC needed for login.

### Personal account

```
$ onedrive-go login
To sign in, visit https://microsoft.com/devicelogin and enter code: ABCD-EFGH

Signed in as toni@outlook.com (personal account).
Drive added: toni@outlook.com
  Display name: toni@outlook.com
  Sync folder:  ~/OneDrive
  Use with:     --drive "toni@outlook.com"

Run 'onedrive-go sync' to sync once, or 'sync --watch' for continuous sync.
Run 'onedrive-go setup' to change settings.
```

### Business account (auto-adds OneDrive for Business)

```
$ onedrive-go login
To sign in, visit https://microsoft.com/devicelogin and enter code: WXYZ-1234

Signed in as alice@contoso.com (Contoso Ltd).
Drive added: alice@contoso.com
  Display name: alice@contoso.com
  Sync folder:  ~/OneDrive - Contoso
  Use with:     --drive "alice@contoso.com"

You also have access to SharePoint libraries and shared folders.
Run 'onedrive-go drive add' to add them.

Run 'onedrive-go sync' to sync, or 'onedrive-go setup' to change settings.
```

Business OneDrive is **always auto-added**, even if the user only wants SharePoint. They can `drive remove` it later. This matches Microsoft's behavior.

SharePoint is mentioned as a tease — use `drive add` to add libraries.

### Second login (another account)

```
$ onedrive-go login
To sign in, visit https://microsoft.com/devicelogin and enter code: QRST-5678

Signed in as toni@outlook.com (personal account).
Drive added: personal:toni@outlook.com
  Display name: toni@outlook.com
  Sync folder:  ~/OneDrive - Personal
  Use with:     --drive "toni@outlook.com"
  Note: ~/OneDrive already used by business:alice@contoso.com

You now have 2 drives. Run 'onedrive-go status' to see all.
```

Since `~/OneDrive` is taken, falls back to `~/OneDrive - Personal`. No prompt — just picks the best default and tells the user.

### Re-login (token refresh)

```
$ onedrive-go login --account alice@contoso.com
To sign in, visit https://microsoft.com/devicelogin and enter code: MNOP-9012

Token refreshed for alice@contoso.com.
```

### What if user ctrl-C during login?

The device code flow is inherently two-phase: (1) request code, (2) poll for completion. If ctrl-C during polling, no token is saved, no config is modified. Clean state. If ctrl-C after auth but before config write... config write is atomic (write temp file, rename). Worst case: token saved but config not updated. Next login fixes it.

---

## 10. Drive Add / Remove

### `drive add`

Adds a SharePoint library or a shared folder. Does NOT offer new account sign-in — that's what `login` is for.

```
$ onedrive-go drive add
SharePoint libraries (using alice@contoso.com):
  1. Marketing / Documents
  2. HR / Policies

Shared with me:
  3. Jane's Photos       (shared by jane@outlook.com, read-only)
  4. Bob's Project Files  (shared by bob@contoso.com, read-write)
  ... and 3 more shared folders

Select (number): 3
Drive added: Jane's Photos (shared by jane@outlook.com, read-only)
  -> ~/OneDrive-Shared/Jane's Photos

To add a new Microsoft account, use 'onedrive-go login'.
```

`drive add` is the one command that's interactive (SharePoint site/library selection). Everything else assumes defaults.

To temporarily stop syncing a drive without removing it, use `pause` instead.

For non-interactive use (scripts):
```bash
onedrive-go drive add --site marketing --library Documents
```

### `drive remove`

Removes a drive from config. Config section is deleted. Token, state DB, and sync directory are untouched. State DB is preserved so re-adding the drive resumes delta sync from where it left off (no full re-sync needed).

```
$ onedrive-go drive remove --drive "Jane's Photos"
Drive removed: Jane's Photos (shared by jane@outlook.com)
  Token: kept
  State database: kept (fast re-add)
  Config section: deleted
  Sync directory (~/OneDrive-Shared/Jane's Photos): untouched — your files remain on disk

Re-add with: onedrive-go drive add "Jane's Photos"
Delete everything: onedrive-go drive remove --drive "Jane's Photos" --purge
```

Works for any drive type — personal, business, or SharePoint. To temporarily stop sync without removing, use `pause` instead.

### `drive remove --purge`

Permanently removes the drive's state DB and config section. Token is kept if other drives still use it.

```
$ onedrive-go drive remove --drive sharepoint:alice:marketing --purge
Drive removed: sharepoint:alice@contoso.com:marketing:Documents
  Token: kept (still used by business:alice@contoso.com)
  State database: deleted
  Config section: deleted
  Sync directory (~/Contoso/Marketing - Documents): untouched — delete manually if desired
```

If no other drives share the token:

```
$ onedrive-go drive remove --drive personal --purge
Drive removed: personal:toni@outlook.com
  Token: deleted (no other drives use it)
  State database: deleted
  Config section: deleted
  Sync directory (~/OneDrive): untouched — delete manually if desired
```

### Duplicate-source detection (DP-9)

When `drive add` adds a shared folder that is already synced as a shortcut inside the user's primary drive (or vice versa), a warning is displayed:

```
$ onedrive-go drive add "Jane's Photos"
Warning: "Jane's Photos" is also synced as a shortcut in your drive
  (at ~/OneDrive/Jane's Photos). Both will sync independently — this
  wastes bandwidth and disk. Consider removing one.

Drive added: Jane's Photos (shared by jane@outlook.com, read-only)
  -> ~/OneDrive-Shared/Jane's Photos
```

The warning is informational — the add proceeds. The same warning appears at config validation time if the duplicate is detected. See [MULTIDRIVE.md §4](MULTIDRIVE.md#duplicate-source-detection-dp-9).

---

## 11. Logout

Removes the authentication token for an account. All drives using that token are affected — no confirmation prompt, just report what happened.

### `logout`

Deletes the token file and removes all account's drive sections from config. State DBs are preserved so re-login creates fresh config sections and delta sync resumes.

```
$ onedrive-go logout --account alice@contoso.com
Token removed for alice@contoso.com.
Drive config sections removed:
  business:alice@contoso.com (~/OneDrive - Contoso)
  sharepoint:alice@contoso.com:marketing:Documents (~/Contoso/Marketing - Documents)

State databases kept. Run 'onedrive-go login' to re-authenticate.
Sync directories untouched — your files remain on disk.
```

### `logout --purge`

```
$ onedrive-go logout --account alice@contoso.com --purge
Token removed for alice@contoso.com.
Drives permanently removed:
  business:alice@contoso.com — state DB deleted, config section deleted
  sharepoint:alice@contoso.com:marketing:Documents — state DB deleted, config section deleted

Sync directories untouched — delete manually if desired:
  ~/OneDrive - Contoso
  ~/Contoso/Marketing - Documents
```

---

## 12. Status

Shows all accounts and drives in a hierarchy:

```
$ onedrive-go status
Account: toni@outlook.com (personal)
  Token: valid
  me@outlook.com               ~/OneDrive                      synced 2m ago

Account: alice@contoso.com (Contoso Ltd)
  Token: valid (expires in 45 min)
  me@contoso.com               ~/OneDrive - Contoso            syncing 42/100
  Marketing / Documents        ~/Contoso/Marketing - Documents paused
  Jane's Photos                ~/OneDrive-Shared/Jane's Photos synced 5m ago
```

Each account is a section showing token status. Drives listed underneath showing display name, sync dir, and state. Paused drives shown as "paused". Removed-but-config-remains drives shown as "removed (config only)."

With `--drive` for detail:
```
$ onedrive-go status --drive "me@contoso.com"
me@contoso.com (business:alice@contoso.com)
  Sync dir:      ~/OneDrive - Contoso
  Status:        syncing (42/100 files)
  Last sync:     in progress
  Files:         1,234
  Size:          4.2 GB
  Display name:  me@contoso.com
  Token:         valid (expires in 45 min)
```

With `--json` for scripting:
```bash
onedrive-go status --json | jq '.drives[].status'
```

---

## 13. Multi-Drive Sync

```bash
onedrive-go sync                                            # all non-paused drives, once
onedrive-go sync --watch                                    # continuous (daemon mode)
onedrive-go sync --drive personal                          # just one drive (substring match)
onedrive-go sync --drive "me@outlook.com" --drive business  # two of three
```

### Runtime behavior

- Each drive syncs in its own goroutine via `DriveRunner` (single process, per-drive isolation)
- Each drive locks its own state DB — no contention between drives
- SharePoint drives share the business token via `graph.Client` pooling (thread-safe)
- Failure in one drive doesn't block others (panic recovery, exponential backoff)
- Progress output shows per-drive status
- Worker budget: global cap (default 16) split proportionally by baseline file count

### `sync --watch` (daemon mode)

Runs in the foreground forever. Suitable for systemd/launchd service management.

- Starts the `Orchestrator`, which manages `DriveRunner` instances for all non-paused drives
- **Watches config.toml via fsnotify.** Changes take effect within milliseconds — no waiting for poll cycle.
- **Idles gracefully with no drives.** Can be installed as a service before any login — just waits. When a drive is added via `login` or `drive add`, fsnotify picks it up immediately. No restart needed.
- Token expiration → log error, enter backoff for that drive, continue others
- No interactive prompts — all output goes to stderr + log file
- Commands that modify config (`login`, `drive add/remove`, `pause`, `resume`) trigger immediate fsnotify reload

### Config-as-IPC (Phase 7.0 control mechanism)

All control flows through the config file. CLI commands write to `config.toml`; the daemon watches via fsnotify and reacts:

- `pause` → writes `paused = true` → daemon stops drive within milliseconds
- `resume` → removes `paused` → daemon starts drive within milliseconds
- `drive add` → adds section → daemon starts drive
- `drive remove` → removes section from config → daemon stops drive
- `login` → adds section → daemon starts drive
- `logout` → removes sections from config → daemon stops drives

No RPC socket is needed for Phase 7.0. All CLI commands are standalone — they read/write config, state DBs, and tokens directly. The `status` command works with or without the daemon by reading config + token files + state DBs.

### Future: RPC socket (Phase 12.6)

RPC can be added for live status data (in-flight action counts, real-time progress, SSE events) that can't be read from config + state DBs. This is additive — config-as-IPC remains the control mechanism. Login is always done via the CLI, not via RPC — login requires user interaction (device code or browser).

### No `--daemon` flag

Modern Linux doesn't need daemon mode (fork, detach, setsid). systemd handles process lifecycle — backgrounding, restart on failure, logging. Same with launchd on macOS. `sync --watch` just runs in the foreground.

### Service management

```
onedrive-go service install       # writes service file, does NOT enable
onedrive-go service uninstall     # removes service file
onedrive-go service status        # installed? enabled? running?
```

`service install` generates the appropriate service file for the platform:
- Linux: `~/.config/systemd/user/onedrive-go.service`
- macOS: `~/Library/LaunchAgents/com.onedrive-go.plist`

After install, it prints the native commands to enable/disable — no wrapping:

```
Service file created: ~/.config/systemd/user/onedrive-go.service

To enable and start:
  systemctl --user enable --now onedrive-go

To stop and disable:
  systemctl --user disable --now onedrive-go

To check logs:
  journalctl --user -u onedrive-go
```

**Never auto-enables.** The user explicitly chooses when to start the service. Uninstall removes the file — if the service was enabled, it tells the user to disable first.

### Typical setup flow

```
$ onedrive-go service install                          # write service file
$ systemctl --user enable --now onedrive-go            # start the service (idles, no drives yet)
$ onedrive-go login                                    # add account + drive
  -> next sync cycle picks it up automatically
```

The service can be installed once and forgotten. Login, drive add/remove, and config changes are all picked up on the next cycle.

---

## 14. Setup (Interactive Configuration)

`onedrive-go setup` is the one interactive command for configuration. Menu-driven, guided. Works for first-time setup and reconfiguration.

Covers:
- View current drives and settings
- Change sync directories
- Configure exclusions (skip_dirs, skip_files, skip_dotfiles)
- Set sync interval
- Set log level
- Configure per-drive overrides
- Edit display names

Everything `setup` does can also be done by editing `config.toml` directly. `setup` is convenience for users who prefer guided configuration. Power users edit the file.

---

## 15. Email Change Detection

### The problem

Drive IDs use email (`personal:toni@outlook.com`). If a user's email changes (corporate rename, personal alias switch), the canonical ID changes, breaking the link to token and state DB files.

### Solution: stable user GUID

Microsoft's Graph API returns a stable `id` field (object GUID) on `/me` that **never changes**, even when email or UPN changes. This is true for both personal and business accounts.

### Implementation

1. On login, call `GET /me`, store `user.id` (GUID) alongside the token.
2. On each startup / sync cycle, call `GET /me` and compare:
   - **Same GUID, same email**: normal operation.
   - **Same GUID, different email**: email changed. Auto-rename token file, state DB file, and config section. No re-sync needed — state DB is intact.
   - **Different GUID**: different user authenticated. Error: "This token belongs to a different user."

### What gets renamed

When email changes from `old@example.com` to `new@example.com`:
- `token_personal_old@example.com.json` -> `token_personal_new@example.com.json`
- `state_personal_old@example.com.db` -> `state_personal_new@example.com.db`
- Config section `["personal:old@example.com"]` -> `["personal:new@example.com"]`
- Sync directory: untouched (just files on disk, name doesn't depend on email)

Output:
```
Email changed for user abc123: old@example.com -> new@example.com
Renamed token and state files. Config updated. No re-sync needed.
```

---

## 16. User Journeys

### A: Single personal account (90% of users)

```
$ onedrive-go login       -> auto-adds personal:toni@outlook.com -> ~/OneDrive
$ onedrive-go sync        -> done forever
```

One command to set up, one to sync. Never thinks about drives or config.

### B: Add business account later

```
$ onedrive-go login       -> business:alice@contoso.com -> ~/OneDrive - Contoso
$ onedrive-go sync        -> syncs both drives
```

First drive (~/OneDrive) undisturbed.

### C: Business + SharePoint

```
$ onedrive-go login       -> business:alice@contoso.com -> ~/OneDrive - Contoso
                              (mentions SharePoint availability)
$ onedrive-go drive add   -> pick Marketing library -> ~/Contoso/Marketing - Documents
$ onedrive-go sync        -> syncs both (shared token)
```

### D: Same email, personal + business

```
$ onedrive-go login       -> personal:toni@company.com -> ~/OneDrive
$ onedrive-go login       -> business:toni@company.com -> ~/OneDrive - CompanyName
$ onedrive-go ls /        -> error: "ambiguous, try --drive personal or --drive business"
$ onedrive-go sync        -> syncs both
```

### E: Pause and resume a drive

```
$ onedrive-go pause --drive work           -> paused = true in config, drive still visible in status
$ onedrive-go sync                         -> only syncs personal
$ onedrive-go resume --drive work          -> paused removed from config
$ onedrive-go sync                         -> syncs both, picks up where it left off
```

### E2: Remove and re-add a drive

```
$ onedrive-go drive remove --drive work    -> config section deleted (state DB kept)
$ onedrive-go sync                         -> only syncs personal
$ onedrive-go drive add work               -> fresh config section, resumes from last delta token
$ onedrive-go sync                         -> syncs both, picks up where it left off
```

### F: Change sync directory

```
$ mv ~/OneDrive ~/PersonalCloud
$ vim ~/.local/share/onedrive-go/config.toml   # change sync_dir
$ onedrive-go sync                             # works (relative paths in state DB)
```

Or use `onedrive-go setup` to change it interactively.

### G: Full account removal

```
$ onedrive-go logout --account alice@contoso.com --purge
  -> token + state DBs + config sections deleted for all alice's drives
  -> sync dirs on disk kept (user deletes manually)
```

### H: Configure exclusions

```
$ vim ~/.local/share/onedrive-go/config.toml
  # add: skip_dirs = ["node_modules", ".git"]
$ onedrive-go sync   # respects new exclusions
```

Or use `onedrive-go setup`.

### I: CI usage

CI doesn't login — a human bootstraps the token once, uploads it to secret storage (e.g., Azure Key Vault), and CI downloads it to disk before running. The app just finds the token file and uses it.

```bash
# CI workflow downloads pre-provisioned token to disk, then:
onedrive-go ls /                            # auto-selects only drive
onedrive-go sync                            # syncs
```

### J: Service-first setup

```
$ onedrive-go service install
$ systemctl --user enable --now onedrive-go     # service starts, idles (no drives)
$ onedrive-go login                             # adds drive to config
  -> next sync cycle, service picks it up and starts syncing
```

### K: Email changes

```
# User's email changed from old@company.com to new@company.com
$ onedrive-go sync
Email changed for user abc123: old@company.com -> new@company.com
Renamed token and state files. Config updated. No re-sync needed.
Syncing...
```

### L: Sync a shared folder

```
$ onedrive-go drive list
...
Available shared folders:
  Jane's Photos       (shared by jane@outlook.com, read-only)
  Bob's Project Files (shared by bob@contoso.com, read-write)

$ onedrive-go drive add "Jane's Photos"
Drive added: Jane's Photos (shared by jane@outlook.com, read-only)
  -> ~/OneDrive-Shared/Jane's Photos

$ onedrive-go sync  # syncs all non-paused drives including shared
```

---

## 17. Edge Cases & Error Messages

### Ambiguous `--drive`

```
Error: "alice" matches multiple drives:
  me@contoso.com (~/OneDrive - Contoso)
  Marketing / Documents (~/Contoso/Marketing - Documents)

Try a more specific match:
  --drive "me@contoso.com"        (OneDrive for Business)
  --drive "Marketing / Documents" (SharePoint library)
```

### Unknown `--drive`

```
Error: no drive matching "xyz"

Configured drives:
  me@outlook.com     (--drive personal, --drive "me@outlook.com")
  me@contoso.com     (--drive business, --drive "me@contoso.com")
```

### No accounts

```
Error: no accounts. Run 'onedrive-go login' to get started.
```

### All drives paused

```
All drives paused. Run 'onedrive-go resume' to resume syncing.
```

### Multiple drives, single-target command, no `--drive`

```
Error: multiple drives configured. Specify which:
  --drive "me@outlook.com"   (toni@outlook.com, ~/OneDrive)
  --drive "me@contoso.com"   (alice@contoso.com, ~/OneDrive - Contoso)
  --drive "Jane's Photos"    (shared by jane@outlook.com, ~/OneDrive-Shared/Jane's Photos)
```

### Sync dir collision at login

```
Signed in as toni@outlook.com (personal account).
Drive added: personal:toni@outlook.com
  Display name: toni@outlook.com
  Sync folder:  ~/OneDrive - Personal
  Use with:     --drive "toni@outlook.com"
  Note: ~/OneDrive already used by business:alice@contoso.com
```

No prompt. Auto-picks alternative and tells the user.

### Token expired in daemon mode

```
[ERROR] business:alice@contoso.com: token expired.
  Run: onedrive-go login --account alice
  Continuing to sync other drives.
```

### Logout affects multiple drives

```
Token removed for alice@contoso.com.
Drive config sections removed:
  business:alice@contoso.com
  sharepoint:alice@contoso.com:marketing:Documents
  sharepoint:alice@contoso.com:hr:Policies
State databases kept. Sync directories untouched.
Run 'onedrive-go login' to re-authenticate.
```

No confirmation prompt. Just does it and reports.

---

## 18. Decisions Log

| # | Decision | Rationale |
|---|---|---|
| 1 | Real identity as drive ID | No migration, no rename detection, no "default" concept. |
| 2 | `type:email[:site:library]` with `:` separator | Clean, parseable, unambiguous. |
| 3 | Flat file layout, `_` replaces `:` | Simpler than nested directories. One directory for all data. |
| 4 | TOML sections need quotes | Spec requirement — `@` and `:` not valid bare keys. Unavoidable. |
| 5 | `--account` for auth, `--drive` for drives | Two flags for two concepts. Auth commands match by email. Drive commands match by canonical ID, display name, or substring. |
| 6 | Shortest unique partial matching | `--drive personal` works. Error messages show shortest options. |
| 7 | Display names auto-derived for all drives | Personal/business use email. SharePoint uses "site / lib". Shared uses "{FirstName}'s {FolderName}". User-editable. |
| 8 | SharePoint shares business token | Same OAuth session. Token per-user, state DB per-drive. |
| 9 | Microsoft convention for sync dirs | `~/OneDrive`, `~/OneDrive - Org`, `~/Org/Site - Library`. |
| 10 | Login is interactive (blocks for auth) | Device code by default, `--browser` for auth code flow. Assumes config defaults. No config prompts. |
| 11 | Business OneDrive always auto-added | Even if user only wants SharePoint. `drive remove` to remove, `pause` to pause. |
| 12 | `drive add` doesn't offer new sign-ins | Tells user to use `login`. Only shows SharePoint + available drives. |
| 13 | `drive remove` = delete config section | Config section deleted. State DB kept for fast re-add. Token, sync dir untouched. |
| 13b | `pause`/`resume` = temporary sync stop | Sets `paused = true` in config. Drive stays in config, visible in status. Supports timed pause (`pause 2h`). |
| 14 | `drive remove --purge` = permanent | Deletes state DB + config section. Token if orphaned. Sync dir untouched. |
| 15 | No interactive prompts except `setup` and `drive add` | No y/n questions. Assume defaults. Tell user what was done. |
| 16 | Flat config, no sub-sections | Not enough settings. Everything top-level or per-drive. |
| 17 | `setup` for interactive config | One guided wizard. No `config set` / `exclude add` CLI sprawl. |
| 18 | Separate console from log file | Flags control console. Config controls file. Independent. |
| 19 | Log file always active | Platform-default path. Captures info-level by default. |
| 20 | No `--daemon` flag | `sync --watch` runs in foreground. systemd/launchd handle lifecycle. |
| 21 | Config watched via fsnotify | Changes take effect within milliseconds. No restart needed. CLI commands write to config, daemon reacts. |
| 22 | `status` shows account/drive hierarchy | Accounts with token status, drives indented with sync state. |
| 23 | Stable user GUID for email change detection | Auto-rename files on email change. No re-sync needed. |
| 24 | Guest/B2B uses real email from `mail` field | Users never see ugly UPN. Fallback only if `mail` is empty. |
| 25 | Sync dirs never deleted by any command | User's files. Always state explicitly in output. |
| 26 | Text-level config manipulation | TOML libraries strip comments on round-trip. Read with parser, write with line-based text edits. Preserves all comments (ours and user's). |
| 27 | Commented-out defaults on first start | Config bootstrapped with all global settings as comments. Users discover options without reading docs. Only written once — never regenerated. |
| 28 | Device code as default auth | Works everywhere (headless, SSH, containers). `--browser` for desktop convenience. Both block. |
| 29 | `--json` on login for GUI/scripting | Outputs newline-delimited JSON events. GUI reads events, presents code/URL in own UI. CLI is the API. |
| 30 | Login always via CLI, never RPC | Login requires user interaction. RPC (Phase 12.6) only for live status data. Control flows through config-as-IPC. |
| 31 | `sync --watch` idles with no drives | Can be installed as service before any login. Picks up new drives via fsnotify. No restart needed. |
| 32 | File ops work on paused drives | Paused state is a sync concept only. `ls`, `get`, etc. still work — token is valid, drive exists. Removed drives are invisible. |
| 33b | Config-as-IPC via fsnotify | CLI commands write to config.toml; daemon watches via fsnotify and reacts within milliseconds. No RPC socket needed for Phase 7.0. |
| 33c | Simple drive lifecycle | `drive remove` deletes config section (state DB kept for fast re-add). `drive add` creates fresh section. `logout` deletes all account config sections. State DBs provide fast re-sync. |
| 33 | `service install` never auto-enables | Writes service file only. Prints native commands to enable. User explicitly chooses. |
| 34 | No `--sync-dir` flag | All drives get sensible defaults from Microsoft conventions. Change via config file or `setup`. |
| 35 | No `config show` command | Users read config file directly. `status` shows runtime state. `--debug` shows config resolution. |
| 36 | `127.0.0.1` only for auth callback | `--browser` binds to localhost only. Never `0.0.0.0`. Standard OAuth security practice. |
| 37 | Personal Vault excluded by default | Lock/unlock cycle creates unsolvable data-loss risk. Detect via `specialFolder.name == "vault"`. Config escape hatch `sync_vault = true`. |
| 38 | Share revocation deletes local copies | Consistent with "remote deleted → local deleted" behavior. Post-release: add config option for alternative behavior. |
| 39 | Read-only content auto-detected via 403 | Summarized errors (not per-file). Treat as error, not warning. No proactive permission checking. |
| 40 | Shared-with-me synced as separate configured drives | Clean isolation. Added/removed via `drive add`/`drive remove`. No modification to user's OneDrive structure. |
| 41 | Accounts stay implicit | No `[account]` config sections. No identified use case for account-level config. |
| 42 | Shared canonical ID: `shared:email:sourceDriveID:sourceItemID` | Only `(driveID, itemID)` is guaranteed globally unique and stable across renames/moves. Display names solve readability. |
| 43 | Individual shared files deferred to post-release | No delta tracking for individual files. Focus on folder/drive sync story first. |

---

## 19. Open Questions

| # | Question | Notes |
|---|---|---|
| 1 | OrgName sanitization | Strip `:`, `/`, `\`, null. Keep spaces, dashes, periods. Max length? |
| 2 | SharePoint site discovery UX | `GET /sites?search=*` is search, not list-all. Large tenants have thousands of sites. Need search + pagination in `drive add`. |
| 3 | `setup` wizard UX | Menu structure, navigation. Design when implementing. |
| 4 | Personal `mail` field often empty | For personal accounts, `mail` is often null. UPN is ugly. Need to extract actual email from the UPN or from token claims. Current code handles with fallback. |
| 6 | RPC protocol | Unix socket path, message format (JSON-RPC? gRPC? plain JSON?), auth model for the socket. Design when implementing `sync --watch`. |
| 7 | Localhost callback port | Fixed port (e.g., 53682 like OneDrive) or dynamic (random available port)? Dynamic is more robust but requires registering `http://localhost` redirect URI without port in Azure AD. |
| 8 | Shared folder rename detection | If the source owner renames the shared folder, should we auto-update the local directory name? |
