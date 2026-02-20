# Account & Drive System Design

## Core Concepts

**Accounts** are what you authenticate with (Microsoft accounts). You `login` and `logout` of accounts. An account is identified by its email address.

**Drives** are what you sync. Each drive has a canonical identifier derived from real data — no arbitrary names. Types:

- **Personal OneDrive** — one per personal Microsoft account
- **OneDrive for Business** — one per business/work account
- **SharePoint document library** — many per business account (same token)

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
| 5b | **`--drive` value** | CLI flag (drives) | `personal`, `work`, `marketing` |
| 6 | **Sync directory** | On disk, in config | `~/OneDrive - Contoso` |
| 7 | **Alias** | Per-drive config, CLI | `alias = "work"` |
| 8 | **Display name** | Messages, logs, errors | `toni@outlook.com (personal)` |

---

## 2. Canonical Drive Identifier

### Format

```
personal:<email>
business:<email>
sharepoint:<email>:<site>:<library>
```

### Examples

```
personal:toni@outlook.com
business:alice@contoso.com
sharepoint:alice@contoso.com:marketing:Documents
sharepoint:alice@contoso.com:hr:Policies
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

### Drive type auto-detection

Personal vs business is auto-detected. No `--personal` or `--business` flags needed:

| Auth endpoint | `driveType` | Drive type |
|---|---|---|
| Consumer endpoint | `personal` | `personal` |
| Work/school endpoint | `business` | `business` |
| Work/school endpoint | `documentLibrary` | `sharepoint` |

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

Filenames are always derived FROM the canonical ID in the config file. We never parse filenames to discover drives — config is the source of truth.

### SharePoint token sharing

SharePoint drives share the OAuth token with the business account (same user, same session, same scopes). Only the state DB is per-drive. `token_business_alice@contoso.com.json` serves both her OneDrive for Business and all her SharePoint drives.

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
skip_dotfiles = true
skip_dirs = ["node_modules", ".git"]
skip_files = ["*.tmp", "~*"]
poll_interval = "5m"

# ── Drives (any section with ":" is a drive) ──

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
alias = "home"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
alias = "work"
skip_dirs = ["node_modules", ".git", "vendor"]

["sharepoint:alice@contoso.com:marketing:Documents"]
sync_dir = "~/Contoso/Marketing - Documents"
enabled = false
```

Quotes around section names are required by TOML because `@` and `:` are not valid bare key characters. This is unavoidable with email-based identifiers.

### Global settings

| Key | Type | Default | Description |
|---|---|---|---|
| `log_level` | string | `"info"` | Log file verbosity: `debug`, `info`, `warn`, `error` |
| `log_file` | string | (platform default) | Log file path override |
| `skip_dotfiles` | bool | `false` | Skip files/dirs starting with `.` |
| `skip_dirs` | string[] | `[]` | Directory names to skip everywhere |
| `skip_files` | string[] | `[]` | File name patterns to skip everywhere |
| `poll_interval` | string | `"5m"` | Check interval for `sync --watch` |

### Per-drive settings

| Key | Type | Required | Default | Description |
|---|---|---|---|---|
| `sync_dir` | string | Yes | — | Where to sync. Must be unique across drives. |
| `enabled` | bool | No | `true` | `false` = paused (drive remove sets this) |
| `alias` | string | No | — | Short name for `--drive` (e.g., `"work"`) |
| `remote_path` | string | No | `"/"` | Remote subfolder to sync |
| `drive_id` | string | No | auto | Explicit drive ID (auto-detected for personal/business) |
| `skip_dotfiles` | bool | No | global | Per-drive override |
| `skip_dirs` | string[] | No | global | Per-drive override |
| `skip_files` | string[] | No | global | Per-drive override |
| `poll_interval` | string | No | global | Per-drive override |

Per-drive settings override globals when specified.

### Auto-creation and modification

Config is auto-created by `login` and modified by `drive add`/`drive remove`:

- `login` -> creates config with drive section (if new account)
- `drive add` -> appends drive section
- `drive remove` -> sets `enabled = false`
- `drive remove --purge` -> removes section + state DB
- `logout` -> no config change (only deletes token)
- `logout --purge` -> removes sections + state DBs for all affected drives
- `sync --watch` re-reads config each sync cycle; changes take effect on next cycle

Manual editing always supported. The tool reads config fresh on each run (or each sync cycle in watch mode).

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

- **systemd**: captures stdout/stderr to journal automatically. Query with `journalctl -u onedrive-go`.
- **launchd**: redirect stdout/stderr in the plist file.

Default console output is fine for services. The log file provides persistent history. No special flags needed. Ship example `.service` and `.plist` files.

---

## 6. CLI Commands

### Complete command list

```
Authentication:
  login                     Sign in + auto-add primary drive
  logout [--purge]          Sign out (--purge: also delete state DBs + config sections)
  whoami                    Show authenticated accounts

Drive management:
  drive add                 Add a SharePoint library or resume a paused drive
  drive remove [--purge]    Pause a drive (--purge: delete state DB + config section)

Sync:
  sync [--watch]            Sync drives (once, or continuously with --watch)
  status                    Show all accounts, drives, and sync state

File operations:
  ls                        List remote files
  get                       Download files
  put                       Upload files
  rm                        Delete remote files/folders
  mkdir                     Create remote directory
  stat                      Show file/folder info

Configuration:
  setup                     Interactive guided configuration (menu-driven)
```

### What each command affects

| Command | Token | State DB | Config | Sync dir on disk |
|---|---|---|---|---|
| `login` | Created | — | Section created | — |
| `logout` | **Deleted** | Kept | Kept | Untouched |
| `logout --purge` | **Deleted** | **Deleted** | **Sections removed** | Untouched |
| `drive add` | — (reuses existing) | — | Section created | — |
| `drive remove` | Kept | Kept | `enabled = false` | Untouched |
| `drive remove --purge` | Kept (unless orphaned) | **Deleted** | **Section removed** | Untouched |
| `sync` | May refresh | Updated | — | Files synced |
| `setup` | — | — | Modified | — |

**Sync directories on disk are NEVER deleted by any command.** They contain the user's files. All remove/logout/purge commands explicitly state: "Sync directory kept — delete manually if desired."

### Global CLI flags

| Flag | Used on | Type | Description |
|---|---|---|---|
| `--account` | `login`, `logout` | string | Select account by email or partial email |
| `--drive` | Everything else | string (repeatable for sync/status) | Select drive by canonical ID, alias, or partial match |
| `--config` | All | string | Config file path override |
| `--verbose` / `-v` | All | bool | Show individual file operations |
| `--debug` | All | bool | Show HTTP requests, internal state |
| `--quiet` / `-q` | All | bool | Errors only |
| `--json` | All | bool | Machine-readable output |
| `--dry-run` | All | bool | Show what would happen |

---

## 7. `--account` and `--drive` Flags

Two flags for two concepts:

- **`--account`** — identifies a Microsoft account by email. Used on auth commands (`login`, `logout`).
- **`--drive`** — identifies a specific drive by canonical ID, alias, or partial match. Used on everything else.

### `--account` (auth commands)

Matches against account emails:

```bash
onedrive-go login --account alice@contoso.com     # re-authenticate specific account
onedrive-go logout --account alice@contoso.com    # remove token, affects all drives under this email
onedrive-go logout --account alice                # partial email match (if unambiguous)
```

`logout --account alice@contoso.com` affects ALL drives using that account's token (business + SharePoint). No confirmation prompt — just do it and report.

### `--drive` (drive commands)

Fuzzy matches against canonical drive IDs and aliases. Resolution order:

1. **Exact canonical match**: `personal:toni@outlook.com` -> direct hit
2. **Alias match**: `work` -> resolves via `alias = "work"` in config
3. **Prefix match on type**: `personal` -> matches `personal:toni@outlook.com` (if only one personal drive)
4. **Email match**: `toni@outlook.com` -> matches if only one drive has this email
5. **Partial match**: `marketing` -> matches if only one drive contains `marketing`

Shortest unique match wins. If ambiguous -> error with suggestions showing shortest unique identifiers.

### Discoverability

Users learn about partial matching through:

**After login:**
```
Drive added: personal:toni@outlook.com -> ~/OneDrive
Use with: --drive personal  or  --drive toni@outlook.com
```

**In error messages (ambiguous match):**
```
Error: "alice" matches multiple drives:
  business:alice@contoso.com
  sharepoint:alice@contoso.com:marketing:Documents

Try:
  --drive business       (OneDrive for Business)
  --drive marketing      (Marketing — Documents)
  --drive work           (alias for business:alice@contoso.com)
```

**In `--help` output:**
```
--drive string     Select a drive. Matches shortest unique prefix:
                     --drive personal
                     --drive toni@outlook.com
                     --drive work  (alias)
```

### Auto-selection (when `--drive` is omitted)

| Active drives | Behavior |
|---|---|
| 0 | Error: "No drives configured. Run: onedrive-go login" |
| 1 | Auto-select the only drive |
| 2+ single-target (ls, get, etc.) | Error with list of drives and shortest identifiers |
| 2+ multi-target (sync, status) | All enabled drives |

### Repeatable for multi-target commands

```bash
onedrive-go sync                                     # all enabled drives
onedrive-go sync --drive personal                    # just one
onedrive-go sync --drive personal --drive work       # two of three
onedrive-go status --drive work                      # status for one drive
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

`login` is **not interactive** (beyond the device code flow which requires visiting a URL). No prompts. Assumes defaults. Tells the user what was done.

### Personal account

```
$ onedrive-go login
To sign in, visit https://microsoft.com/devicelogin and enter code: ABCD-EFGH

Signed in as toni@outlook.com (personal account).
Drive added: personal:toni@outlook.com -> ~/OneDrive
Use with: --drive personal  or  --drive toni@outlook.com

Run 'onedrive-go sync' to sync once, or 'sync --watch' for continuous sync.
Run 'onedrive-go setup' to change settings.
```

### Business account (auto-adds OneDrive for Business)

```
$ onedrive-go login
To sign in, visit https://microsoft.com/devicelogin and enter code: WXYZ-1234

Signed in as alice@contoso.com (Contoso Ltd).
Drive added: business:alice@contoso.com -> ~/OneDrive - Contoso
Use with: --drive business  or  --drive alice  or  --drive work (alias)

You also have access to SharePoint libraries.
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
Drive added: personal:toni@outlook.com -> ~/OneDrive - Personal
  (~/OneDrive already used by business:alice@contoso.com)

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

Adds a SharePoint library or resumes a paused drive. Does NOT offer new account sign-in — that's what `login` is for.

```
$ onedrive-go drive add
Paused drives:
  1. business:alice@contoso.com (~/OneDrive - Contoso) — resume

SharePoint libraries (using alice@contoso.com):
  2. Marketing — Documents
  3. HR — Policies
  4. Engineering — Wiki

Select (number): 2
Drive added: sharepoint:alice@contoso.com:marketing:Documents
  -> ~/Contoso/Marketing - Documents

To add a new Microsoft account, use 'onedrive-go login'.
```

`drive add` is the one command that's interactive (SharePoint site/library selection). Everything else assumes defaults.

For non-interactive use (CI, scripts):
```bash
onedrive-go drive add --site marketing --library Documents --sync-dir ~/Marketing
```

### `drive remove`

Pauses a drive. Sets `enabled = false` in config. Everything preserved.

```
$ onedrive-go drive remove --drive work
Drive paused: business:alice@contoso.com
  Token: kept
  State database: kept
  Config section: kept (enabled = false)
  Sync directory (~/OneDrive - Contoso): untouched — your files remain on disk

Resume with: onedrive-go drive add
Delete everything: onedrive-go drive remove --drive work --purge
```

Works for any drive type — personal, business, or SharePoint.

### `drive remove --purge`

Permanently removes the drive's state DB and config section. Token is kept if other drives still use it.

```
$ onedrive-go drive remove --drive sharepoint:alice:marketing --purge
Drive removed: sharepoint:alice@contoso.com:marketing:Documents
  Token: kept (still used by business:alice@contoso.com)
  State database: deleted
  Config section: removed
  Sync directory (~/Contoso/Marketing - Documents): untouched — delete manually if desired
```

If no other drives share the token:

```
$ onedrive-go drive remove --drive personal --purge
Drive removed: personal:toni@outlook.com
  Token: deleted (no other drives use it)
  State database: deleted
  Config section: removed
  Sync directory (~/OneDrive): untouched — delete manually if desired
```

---

## 11. Logout

Removes the authentication token for an account. All drives using that token are affected — no confirmation prompt, just report what happened.

### `logout`

```
$ onedrive-go logout --account alice@contoso.com
Token removed for alice@contoso.com.
Affected drives (can no longer sync):
  business:alice@contoso.com (~/OneDrive - Contoso)
  sharepoint:alice@contoso.com:marketing:Documents (~/Contoso/Marketing - Documents)

State databases and config kept. Run 'onedrive-go login' to re-authenticate.
Sync directories untouched — your files remain on disk.
```

### `logout --purge`

```
$ onedrive-go logout --account alice@contoso.com --purge
Token removed for alice@contoso.com.
Drives removed:
  business:alice@contoso.com — state DB deleted, config section removed
  sharepoint:alice@contoso.com:marketing:Documents — state DB deleted, config section removed

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
  personal:toni@outlook.com        ~/OneDrive                      synced 2m ago

Account: alice@contoso.com (Contoso Ltd)
  Token: valid (expires in 45 min)
  business:alice@contoso.com       ~/OneDrive - Contoso            syncing 42/100
  sharepoint:...:marketing:Docs    ~/Contoso/Marketing - Documents paused
  sharepoint:...:hr:Policies       ~/Contoso/HR - Policies         removed (config only)
```

Each account is a section showing token status. Drives listed underneath showing sync dir and state. Paused drives shown as "paused". Removed-but-config-remains drives shown as "removed (config only)."

With `--drive` for detail:
```
$ onedrive-go status --drive work
business:alice@contoso.com
  Sync dir:      ~/OneDrive - Contoso
  Status:        syncing (42/100 files)
  Last sync:     in progress
  Files:         1,234
  Size:          4.2 GB
  Alias:         work
  Token:         valid (expires in 45 min)
```

With `--json` for scripting:
```bash
onedrive-go status --json | jq '.drives[].status'
```

---

## 13. Multi-Drive Sync

```bash
onedrive-go sync                         # all enabled drives, once
onedrive-go sync --watch                 # continuous (daemon-like)
onedrive-go sync --drive personal        # just one drive
onedrive-go sync --drive home --drive work       # two of three
```

### Runtime behavior

- Each drive syncs in its own goroutine (single process)
- Each drive locks its own state DB — no contention between drives
- SharePoint drives share the business token (thread-safe token source)
- Failure in one drive doesn't block others
- Progress output shows per-drive status

### `sync --watch` (continuous mode)

Runs in the foreground forever. Suitable for systemd/launchd service management.

- Syncs all enabled drives on each cycle (configurable `poll_interval`)
- **Re-reads config on each cycle.** If a drive is added/removed/paused while running, changes take effect on the next cycle.
- Token expiration -> log error, skip that drive, continue others
- No interactive prompts — all output goes to stderr + log file
- Commands that modify config (`login`, `drive add/remove`) output: "If sync is running, changes take effect on the next sync cycle."

### No `--daemon` flag

Modern Linux doesn't need daemon mode (fork, detach, setsid). systemd handles process lifecycle — backgrounding, restart on failure, logging. Same with launchd on macOS. `sync --watch` just runs in the foreground. We ship example service files:

- `contrib/systemd/onedrive-go.service`
- `contrib/launchd/com.onedrive-go.plist`

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
- Set aliases

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
$ onedrive-go drive remove --drive work  -> enabled=false, everything preserved
$ onedrive-go sync                         -> only syncs personal
$ onedrive-go drive add                    -> shows work as resumable -> resume
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

### I: CI / scripted usage

```bash
onedrive-go login --sync-dir ~/OneDrive     # non-interactive
onedrive-go ls /                            # auto-selects only drive
onedrive-go sync                            # syncs
```

### J: Email changes

```
# User's email changed from old@company.com to new@company.com
$ onedrive-go sync
Email changed for user abc123: old@company.com -> new@company.com
Renamed token and state files. Config updated. No re-sync needed.
Syncing...
```

---

## 17. Edge Cases & Error Messages

### Ambiguous `--drive`

```
Error: "alice" matches multiple drives:
  business:alice@contoso.com (~/OneDrive - Contoso)
  sharepoint:alice@contoso.com:marketing:Documents (~/Contoso/Marketing - Documents)

Try a more specific match:
  --drive business       (OneDrive for Business)
  --drive marketing      (Marketing — Documents)
  --drive work           (alias for business:alice@contoso.com)
```

### Unknown `--drive`

```
Error: no drive matching "xyz"

Configured drives:
  personal:toni@outlook.com     (--drive personal, --drive home)
  business:alice@contoso.com    (--drive business, --drive work)
```

### No drives configured

```
Error: no drives configured. Run 'onedrive-go login' to get started.
```

### Multiple drives, single-target command, no `--drive`

```
Error: multiple drives configured. Specify which:
  --drive personal     (toni@outlook.com, ~/OneDrive)
  --drive business     (alice@contoso.com, ~/OneDrive - Contoso)
  --drive work         (alias for business)
```

### Sync dir collision at login

```
Signed in as toni@outlook.com (personal account).
Drive added: personal:toni@outlook.com -> ~/OneDrive - Personal
  (~/OneDrive already used by business:alice@contoso.com)
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
Affected drives (can no longer sync):
  business:alice@contoso.com
  sharepoint:alice@contoso.com:marketing:Documents
  sharepoint:alice@contoso.com:hr:Policies
State databases and config kept. Sync directories untouched.
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
| 5 | `--account` for auth, `--drive` for drives | Two flags for two concepts. Auth commands match by email. Drive commands match by canonical ID / alias / partial. |
| 6 | Shortest unique partial matching | `--drive personal` works. Error messages show shortest options. |
| 7 | Aliases are per-drive convenience | Optional. Set in config. Resolved during fuzzy matching. |
| 8 | SharePoint shares business token | Same OAuth session. Token per-user, state DB per-drive. |
| 9 | Microsoft convention for sync dirs | `~/OneDrive`, `~/OneDrive - Org`, `~/Org/Site - Library`. |
| 10 | Login is non-interactive | No prompts. Assume defaults. Tell user what happened. Suggest `setup`. |
| 11 | Business OneDrive always auto-added | Even if user only wants SharePoint. `drive remove` to pause. |
| 12 | `drive add` doesn't offer new sign-ins | Tells user to use `login`. Only shows SharePoint + paused drives. |
| 13 | `drive remove` = pause (`enabled=false`) | Config, state DB, token preserved. `drive add` resumes. |
| 14 | `drive remove --purge` = permanent | Deletes state DB + config section. Token if orphaned. Sync dir untouched. |
| 15 | No interactive prompts except `setup` and `drive add` | No y/n questions. Assume defaults. Tell user what was done. |
| 16 | Flat config, no sub-sections | Not enough settings. Everything top-level or per-drive. |
| 17 | `setup` for interactive config | One guided wizard. No `config set` / `exclude add` CLI sprawl. |
| 18 | Separate console from log file | Flags control console. Config controls file. Independent. |
| 19 | Log file always active | Platform-default path. Captures info-level by default. |
| 20 | No `--daemon` flag | `sync --watch` runs in foreground. systemd/launchd handle lifecycle. |
| 21 | Config re-read each sync cycle | Changes take effect on next cycle. No restart needed. |
| 22 | `status` shows account/drive hierarchy | Accounts with token status, drives indented with sync state. |
| 23 | Stable user GUID for email change detection | Auto-rename files on email change. No re-sync needed. |
| 24 | Guest/B2B uses real email from `mail` field | Users never see ugly UPN. Fallback only if `mail` is empty. |
| 25 | Sync dirs never deleted by any command | User's files. Always state explicitly in output. |

---

## 19. Open Questions

| # | Question | Notes |
|---|---|---|
| 1 | OrgName sanitization | Strip `:`, `/`, `\`, null. Keep spaces, dashes, periods. Max length? |
| 2 | SharePoint site discovery UX | `GET /sites?search=*` is search, not list-all. Large tenants have thousands of sites. Need search + pagination in `drive add`. |
| 3 | `setup` wizard UX | Menu structure, navigation. Design when implementing. |
| 4 | Personal `mail` field often empty | For personal accounts, `mail` is often null. UPN is ugly. Need to extract actual email from the UPN or from token claims. Current code handles with fallback. |
| 5 | Config file location | Should config live with data files (one directory) or separate (XDG: `~/.config` vs `~/.local/share`)? |
| 6 | Service file examples | Ship `contrib/systemd/onedrive-go.service` and `contrib/launchd/com.onedrive-go.plist`. Design when implementing. |
