# Multi-Account / Multi-Drive Architecture

> **Status**: All decision points resolved. Orchestrator architecture specified (§11).

---

## 1. What Microsoft Actually Offers

### Account Types

| Account Type | Auth Endpoint | What You Get |
|---|---|---|
| **Personal** (Outlook/Hotmail/Live) | Consumer OAuth | One personal OneDrive drive |
| **Work/School** (Microsoft 365) | Azure AD OAuth | One OneDrive for Business drive + access to SharePoint sites |

### Drive Types (things with their own drive ID and full delta support)

| Drive Type | `driveType` | Who owns it | How you access it |
|---|---|---|---|
| Personal OneDrive | `personal` | You (personal account) | `GET /me/drive` |
| OneDrive for Business | `business` | You (work account) | `GET /me/drive` |
| SharePoint document library | `documentLibrary` | A SharePoint site | `GET /sites/{siteId}/drives/{driveId}` |

### Shared Content (NOT their own drive — lives inside someone else's drive)

Three distinct mechanisms exist:

**A. Shortcuts ("Add shortcut to My files")**

- User adds a shortcut to someone else's folder (or a SharePoint folder) into their own drive
- The shortcut ITEM appears in YOUR drive's delta with a `remoteItem` facet
- The CHILDREN of the shortcut DO NOT appear in your drive's delta
- A separate delta call scoped to the shared folder is required: `GET /drives/{remoteItem.driveId}/items/{remoteItem.id}/delta`
- Each shortcut needs its own cached delta token
- Available for both Personal and Business accounts
- Shortcuts sit in the user's drive tree — the user can place them wherever they want (root, subfolders, nested)
- Downloads/uploads/deletes for items under shortcuts must target the SOURCE drive (`remoteItem.driveId`), not the user's own drive
- Your OAuth token has access to the shared content because the sharing permission grants it

**B. "Shared with me" items (NOT added as shortcuts)**

- Someone shares a file or folder with you, but you haven't added it to your drive
- Discoverable via `GET /me/drive/sharedWithMe`
- NOT in your drive's delta response at all
- BUT: you CAN get folder-scoped delta on the source drive: `GET /drives/{driveId}/items/{itemId}/delta` — same mechanism as shortcuts
- Available for both Personal and Business accounts
- Synced as separate configured drives (see §6)

**C. SharePoint document libraries**

- Separate drives entirely, each with their own drive ID
- Full delta support via `GET /drives/{driveId}/root/delta`
- Accessed via the Sites API
- Only available to Work/School accounts

### Key Facts (confirmed from Microsoft docs)

Source: [Accessing shared files and folders - OneDrive API](https://learn.microsoft.com/en-us/onedrive/developer/rest-api/concepts/using-sharing-links?view=odsp-graph-online)

> "When using delta in a drive with shared folders, the shared folder themselves will be returned as part of the response but the items contained within a shared folder will not be returned. A separate call to delta and separate cached delta token is required for each shared folder."

> "requesting the children collection for a remote item will result in an error from the server"

You CANNOT do `GET /me/drive/items/{shortcut-id}/children`. You MUST use: `GET /drives/{remoteItem.driveId}/items/{remoteItem.id}/children`.

Delta can be scoped to a folder: `GET /drives/{driveId}/items/{itemId}/delta` — this returns only changes within that subtree, not the entire source drive.

Shortcuts can be created via API: `POST /drive/root/children` with a `remoteItem` body containing `driveId` and `id`.

Shortcuts can be removed via API: `DELETE /drive/items/{local-item-id}`.

### Family Sharing

**Not an architectural concept.** Microsoft 365 Family gives each member their own independent OneDrive. There is no shared family drive. "Family sharing" is a UI convenience — typing "Family" in the share dialog auto-completes to family group members. At the API level, it produces the exact same sharing mechanism as sharing with any other person. No family-specific API endpoints or behaviors exist. For our purposes, family sharing is indistinguishable from regular sharing.

### Personal Vault

Personal Vault is a special protected folder within OneDrive Personal. It requires additional 2FA to unlock and auto-locks after 20 minutes of inactivity.

**The Graph API does not expose endpoints for accessing Personal Vault** (confirmed in our tier-1 research, issues-api-inconsistencies.md §9.2). However, vault items MAY appear in delta responses depending on lock state. This creates a dangerous edge case:

1. Vault unlocked → delta returns vault items → we download them → baseline entries created
2. Vault auto-locks → delta shows items as deleted (or omits them) → planner sees "in baseline but not in remote" → emits delete actions → **user's most sensitive files deleted locally**

Safety invariant S1 (never delete without synced base) does NOT protect against this — baseline entries exist from step 1. Big-delete protection S5 also likely won't trigger — vaults typically contain few files.

**Decision: Exclude by default.** Implement IMMEDIATELY as the next roadmap item. Detect via `specialFolder.name == "vault"`. Add to built-in exclusion list. Never include vault items in observation, planning, or baseline. Log at INFO when vault items are skipped. Config escape hatch: `sync_vault = true` with a warning about auto-lock behavior. Post-release: explore additional vault functionality beyond total exclusion.

---

## 2. Identity Architecture

### Canonical ID — machine identity

Four drive types, all using colon-separated format with max 4 parts:

```
personal:me@outlook.com
business:me@contoso.com
sharepoint:me@contoso.com:Marketing:Documents
shared:me@outlook.com:b!TG9yZW0:01ABCDEF
```

| Type | Format | Parts |
|---|---|---|
| `personal` | `personal:email` | 2 |
| `business` | `business:email` | 2 |
| `sharepoint` | `sharepoint:email:site:library` | 4 |
| `shared` | `shared:email:sourceDriveID:sourceItemID` | 4 |

For shared drives, the canonical ID uses server-assigned IDs (`sourceDriveID` + `sourceItemID`) because they are the only combination that is both **globally unique** and **stable** (doesn't change on rename/move). The user's email in position 2 identifies which account's OAuth token to use.

The `shared` canonical ID is opaque to humans. This is by design — users interact via display names (see below), never via the canonical ID directly.

### Display name — human identity

Every drive has a `display_name` field in config. It is:
- **Auto-generated** when the drive is first added
- **User-editable** — the user can change it in config at any time
- **Used everywhere the user sees a drive**: CLI output, `--drive` matching, error messages, log messages (info/warn/error level)

Default display name derivation at `drive add` time:

| Drive type | Default display_name | Uniqueness escalation |
|---|---|---|
| `personal:email` | email (e.g., `me@outlook.com`) | Already globally unique — email IS the display name |
| `business:email` | email (e.g., `me@contoso.com`) | Already globally unique — email IS the display name |
| `sharepoint:email:site:lib` | `"site / lib"` (e.g., `"Marketing / Documents"`) | If collision: `"site / lib (email)"` |
| `shared:email:did:iid` | `"{FirstName}'s {FolderName}"` (e.g., `"Jane's Photos"`) | Steps 1-3 below |

For shared drives, display name data comes from the `sharedWithMe` API response:
- Folder name: `name` field
- Owner full name: `shared.owner.user.displayName`
- Owner email: `shared.owner.user.email`
- First name: extracted from display name

Display name uniqueness at derivation time (only for shared drives):

| Step | Format | When used |
|---|---|---|
| 1 | `{FirstName}'s {FolderName}` | If unique among all drives |
| 2 | `{FullName}'s {FolderName}` | If step 1 collides |
| 3 | `{FullName}'s {FolderName} ({email})` | If step 2 collides |

The email IS the friendly name for personal and business drives — it's unique, human-readable, and immediately identifies the account. No generic "OneDrive", "Personal", or "My Drive" names are ever auto-generated.

One field, one purpose — `display_name` is the sole human-facing identifier for all drives.

### `driveid` package — pure identity

The `driveid` package handles ONLY identity: parsing, construction, formatting, accessors.

```go
const (
    DriveTypePersonal   = "personal"
    DriveTypeBusiness   = "business"
    DriveTypeSharePoint = "sharepoint"
    DriveTypeShared     = "shared"
)

type CanonicalID struct {
    driveType     string
    email         string
    site          string  // SharePoint only
    library       string  // SharePoint only
    sourceDriveID string  // Shared only
    sourceItemID  string  // Shared only
}
```

Methods: `String()`, `DriveType()`, `Email()`, `IsShared()`, `IsSharePoint()`, `Site()`, `Library()`, `SourceDriveID()`, `SourceItemID()`, `IsZero()`, `MarshalText()`, `UnmarshalText()`.

Constructors: `NewCanonicalID(s)`, `Construct(type, email)`, `ConstructSharePoint(email, site, lib)`, `ConstructShared(email, sourceDriveID, sourceItemID)`.

**`TokenCanonicalID()` is removed from this package.** Token resolution is business logic, not identity. It belongs in the `config` package.

### Token resolution in `config` package

```go
// TokenCanonicalID resolves which OAuth token a drive uses.
func TokenCanonicalID(cid driveid.CanonicalID, cfg *Config) (driveid.CanonicalID, error) {
    switch cid.DriveType() {
    case driveid.DriveTypePersonal, driveid.DriveTypeBusiness:
        return cid, nil
    case driveid.DriveTypeSharePoint:
        return driveid.Construct(driveid.DriveTypeBusiness, cid.Email())
    case driveid.DriveTypeShared:
        // Find the primary drive for this email to determine account type.
        for id := range cfg.Drives {
            if id.Email() == cid.Email() &&
                (id.DriveType() == driveid.DriveTypePersonal ||
                 id.DriveType() == driveid.DriveTypeBusiness) {
                return driveid.Construct(id.DriveType(), cid.Email())
            }
        }
        return driveid.CanonicalID{}, fmt.Errorf("no account found for %s", cid.Email())
    }
    return driveid.CanonicalID{}, fmt.Errorf("unknown drive type: %s", cid.DriveType())
}
```

This replaces the old `CanonicalID.TokenCanonicalID()` method. The two existing call sites (`drive.go:425` and `config/drive.go:212`) both have config available.

### Drive config struct

```go
type Drive struct {
    SyncDir     string   `toml:"sync_dir"`
    StateDir    string   `toml:"state_dir,omitempty"`
    Paused      *bool    `toml:"paused,omitempty"`
    PausedUntil *string  `toml:"paused_until,omitempty"` // RFC3339 timestamp for timed pause
    DisplayName string   `toml:"display_name,omitempty"`
    Owner       string   `toml:"owner,omitempty"`        // shared drives only: owner's email
    RemotePath  string   `toml:"remote_path,omitempty"`
    DriveID     string   `toml:"drive_id,omitempty"`
    // ... per-drive overrides ...
}
```

### Config file example

```toml
["personal:me@outlook.com"]
display_name = "me@outlook.com"
sync_dir = "~/OneDrive"

["sharepoint:me@contoso.com:Marketing:Documents"]
display_name = "Marketing / Documents"
sync_dir = "~/SharePoint/Marketing"

["shared:me@outlook.com:b!TG9yZW0:01ABCDEF"]
display_name = "Jane's Photos"
owner = "jane@outlook.com"
sync_dir = "~/OneDrive-Shared/Jane's Photos"
```

---

## 3. CLI UX

### `--drive` matching

Single matching function, one priority order:

1. **Exact canonical ID** — `--drive "personal:me@outlook.com"`
2. **Exact display_name** (case-insensitive) — `--drive "Jane's Photos"`
3. **Substring match** on canonical ID, display_name, or owner — `--drive jane`, `--drive personal`, `--drive photos`

```bash
onedrive-go sync --drive "personal:me@outlook.com"   # exact canonical
onedrive-go sync --drive personal                     # substring on canonical
onedrive-go sync --drive "Jane's Photos"              # exact display_name
onedrive-go sync --drive jane                         # substring on display_name/owner
```

Ambiguity: error with disambiguation guidance.

### `drive list`

```
Configured drives:
  me@outlook.com               ~/OneDrive                         ready
  Jane's Photos (read-only)    ~/OneDrive-Shared/Jane's Photos    ready

Available drives (not configured):
  me@contoso.com
  Marketing / Documents
  Bob's Project Files          (shared by bob@contoso.com, read-write)
  Grandma's Recipes            (shared by grandma@outlook.com, read-only)

Run 'onedrive-go drive add <name>' to add a drive.
```

- Shows display_name for all drives
- Shared content shows `(read-only)` or `(read-write)` from the permissions facet (DP-10)
- Permission annotations are informational — the sync engine still auto-detects via 403 (DP-3)
- Available shared drives capped at first 10. More: `... and N more shared drives`
- All drive types listed together (personal, business, SharePoint, shared) — no `--shared` flag
- `drive list --verbose` adds canonical IDs

### `drive add`

```bash
onedrive-go drive add jane              # substring matches "Jane's Photos"
onedrive-go drive add "Marketing / Doc" # substring matches SharePoint drive
onedrive-go drive add personal          # substring matches personal drive
```

Flow for shared drives:
1. Not a valid canonical ID → try display name resolution
2. Call `GET /me/drive/sharedWithMe` for each account token
3. Derive unique display names for all available shared folders
4. Substring match against derived display names
5. One match → construct canonical ID, auto-fill display_name/owner/sync_dir
6. Multiple matches → error with disambiguation
7. Output: `Added "Jane's Photos" (shared by jane@outlook.com) -> ~/OneDrive-Shared/Jane's Photos`

### `drive remove`

```bash
onedrive-go drive remove --drive "Jane's Photos"     # display name
onedrive-go drive remove --drive personal             # partial canonical
```

### Scope resolution for commands

`sync`, `status`, and `conflicts` all use the same scope resolution:

| Flags | Scope |
|---|---|
| No flags | All non-paused drives |
| `--account alice@contoso.com` | All non-paused drives under that account |
| `--drive "Jane's Photos"` | Just that one drive |
| `--drive work --drive personal` | Those specific drives |

`--download-only` and `--upload-only` are `sync`-command flags that affect the current invocation for whatever drives are in scope. They are not persisted in config. No per-drive sync mode setting exists.

```bash
onedrive-go sync --download-only                              # all non-paused drives, download-only
onedrive-go sync --download-only --drive personal             # one drive, download-only
onedrive-go sync --download-only --account alice@contoso.com  # all Alice's drives, download-only
onedrive-go sync --watch --upload-only                        # continuous, all drives, upload-only
```

### Status / error / log output

```
# status
Jane's Photos (shared by jane@outlook.com)
  Last sync: 2 minutes ago, 142 files

# errors (summarized, not per-file)
Error: 3 uploads failed for "Jane's Photos" (read-only share)

# info/warn logs — display name
INFO  sync started  drive="Jane's Photos"
WARN  permission denied  drive="Jane's Photos"  action=upload  count=3

# debug logs — canonical ID
DEBUG delta request  canonical_id="shared:me@outlook.com:b!TG9yZW0:01ABCDEF"
```

---

## 4. Sync Architecture for Shortcuts

### Per-drive sync: primary delta + N sub-deltas

For a user whose drive contains 3 shortcuts:

```
My Drive/
  Documents/           ← tracked by MY drive's delta
  Photos/              ← tracked by MY drive's delta
  Family Photos/       ← shortcut → Alice's drive (separate delta)
  Work/
    Project Files/     ← shortcut → SharePoint (separate delta)
  Recipes/             ← shortcut → Grandma's drive (separate delta)
```

The sync engine runs:

1. `GET /me/drive/root/delta` → my items + 3 shortcut items (with `remoteItem` facet)
2. `GET /drives/{alice-drive}/items/{family-photos-id}/delta` → Family Photos content
3. `GET /drives/{sharepoint-drive}/items/{project-files-id}/delta` → Project Files content
4. `GET /drives/{grandma-drive}/items/{recipes-id}/delta` → Recipes content

Four delta calls, four delta tokens, all for one configured drive. Content from calls 2-4 is mapped into the local directory tree at the shortcut's position.

### Delta token storage

The `delta_tokens` table needs to support multiple tokens per configured drive:

```sql
CREATE TABLE delta_tokens (
    drive_id    TEXT NOT NULL,     -- the configured drive's normalized ID
    scope_id    TEXT NOT NULL,     -- "" for primary, remoteItem.id for shortcuts
    scope_drive TEXT NOT NULL,     -- same as drive_id for primary, remoteItem.driveId for shortcuts
    token       TEXT NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (drive_id, scope_id)
);
```

### Baseline table

Items under shortcuts have the SOURCE drive's ID in `drive_id`, not the user's own drive ID. This is already correct — the baseline stores server-assigned identity, and API operations need source drive coordinates. No schema change needed, but the observer must populate `drive_id` from `remoteItem.driveId` for shortcut content.

### Observer changes

The Remote Observer needs to:

1. Run primary delta → collect ChangeEvents for own items
2. Detect shortcuts: items with `remoteItem` facet and `folder` type
3. For each shortcut: run a folder-scoped delta on the source drive using `remoteItem.driveId` and `remoteItem.id`
4. Map source-drive relative paths to local paths by prefixing with the shortcut's position in the user's tree
5. Emit ChangeEvents with source drive coordinates but local-tree-relative paths
6. Handle shortcut appearance (new share accepted) → trigger initial enumeration of the shared folder
7. Handle shortcut disappearance (share revoked or shortcut removed) → delete local copies

### Executor changes

The executor already takes `driveID` per action. Items under shortcuts will have the source drive's ID in their action. Downloads, uploads, and deletes will target the correct drive. The key change: the executor must use the user's OAuth token (which has permission to access the shared content) against a DIFFERENT drive ID than the configured drive. This works with the current `graph.Client` since the token is per-account, not per-drive.

### Shortcut lifecycle

| Event | How detected | What happens |
|---|---|---|
| New shortcut appears | Primary delta returns new item with `remoteItem` facet | Start tracking: run initial delta on source drive, create delta token entry |
| Shortcut moved | Primary delta shows shortcut at new path | Update local path prefix for all items under it. Rename local directory. |
| Shortcut removed | Primary delta shows shortcut as deleted | Delete local copies (consistent with "remote deleted → local deleted"). Post-release: add config option for alternative behavior (keep local). |
| Shortcut content changes | Source-drive scoped delta returns changes | Normal sync (download/upload/delete) targeting source drive |
| Source permissions change | 403 on source-drive operations | Summarized error (not per-file). "3 uploads failed for drive X (read-only share)". Treat as error, not warning. |

### Duplicate-source detection (DP-9)

When the same remote folder is synced through both a shortcut (inside the user's drive) and a standalone shared drive (via `drive add`), the folder is synced twice independently. This is wasteful (double bandwidth, double disk space) but not incorrect — each sync path produces consistent results independently.

Detection: at config validation time and during `drive add`, compare each shortcut's `(remoteItem.driveId, remoteItem.id)` against configured shared drives' `(sourceDriveID, sourceItemID)`. On match, emit a warning:

```
Warning: "Jane's Photos" is synced as both a shortcut in your drive
  (at ~/OneDrive/Jane's Photos) and as a standalone shared drive
  (at ~/OneDrive-Shared/Jane's Photos). This wastes bandwidth and disk.
  Consider removing one. To suppress this warning: duplicate_source_ok = true
```

This is a warning, not an error — the user may intentionally want both sync paths for different use cases (e.g., shortcut for online access, standalone drive for backup).

---

## 5. "Shared with me" Drives

**Decision: Sync as separate configured drives.** Added/removed via `drive add`/`drive remove`. Post-release roadmap item, but architecture designed now for future-proofing.

### How it works

Each shared-with-me folder the user wants to sync becomes a full drive entry in config with:
- Its own canonical ID: `shared:email:sourceDriveID:sourceItemID`
- Its own sync directory
- Its own state DB
- Its own delta token
- A display_name and owner for UX

Discovery: `drive list` calls `GET /me/drive/sharedWithMe` and shows available shared folders alongside personal/business/SharePoint drives.

Adding: `drive add "Jane's Photos"` resolves the friendly name via the `sharedWithMe` API, constructs the canonical ID, and adds the drive to config.

Delta tracking: `GET /drives/{sourceDriveID}/items/{sourceItemID}/delta` — same folder-scoped delta mechanism as shortcuts.

### Individual shared files (not folders)

Deferred to post-release. Single shared files can't be added as shortcuts and have no delta tracking (delta is folder-scoped). Users can download individual shared files via the `get` command but they won't be part of sync.

---

## 6. Accounts

**Accounts are implicit.** They exist because token files exist. No explicit `[account]` sections in config. Token sharing (SharePoint → business, shared → primary drive) is handled by the `config.TokenCanonicalID()` function, which scans configured drives to determine the account type.

---

## 7. Implementation Phases

| Phase | Description | Priority |
|---|---|---|
| **1. Personal Vault exclusion** | Detect and exclude vault items from sync. Must ship before sync. Small, critical. | IMMEDIATE |
| **2. Identity refactoring** | Add `DriveTypeShared` to `driveid`. Move `TokenCanonicalID()` from `driveid` to `config`. Add `display_name` to Drive struct. Update `--drive` matching. | Before multi-drive |
| **3. Shortcut detection** | Detect `remoteItem` facets in delta. Log "shared folder detected, content sync not yet supported" until shortcut content sync is implemented. | Phase 7 |
| **4. Shortcut content sync** | Per-shortcut delta, path mapping, cross-drive API operations. Delta token schema change. | Phase 7 |
| **5. Shortcut lifecycle** | Handle appearance, disappearance (delete local), moves, permission errors (summarized 403). | Phase 7 |
| **6. Shared-with-me discovery** | `sharedWithMe` API integration in `drive list`. Display name derivation. Capped at 10 in listing. | Post-release |
| **7. Shared-with-me sync** | Full drive infrastructure for shared-with-me folders. `drive add`/`remove` by friendly name. | Post-release |
| **8. Individual shared files** | If ever. Deferred. | Post-release |

---

## 8. Decision Log

All decision points have been resolved.

| DP | Decision | Rationale |
|---|---|---|
| **DP-1: Personal Vault** | Exclude by default. Implement immediately. Config escape hatch `sync_vault = true`. Post-release: explore additional vault functionality. | Lock/unlock cycle creates unsolvable data-loss risk. S1 doesn't protect. |
| **DP-2: Share revocation** | Delete local copies. Post-release: add config option for alternative behavior. | Consistent with "remote deleted → local deleted" behavior. |
| **DP-3: Read-only content** | Auto-detect via 403. Summarized errors (not per-file). Treat as error, not warning. | Simple, no proactive permission checking. |
| **DP-4: Shared-with-me** | Sync as separate configured drives. Post-release, but architecture designed now. Added/removed via `drive add`/`drive remove`. | Clean isolation, no modification to user's OneDrive structure. |
| **DP-5: Account entities** | Keep implicit. No `[account]` config sections. | No identified use case for account-level config. |
| **DP-6: Canonical ID format** | `shared:email:sourceDriveID:sourceItemID`. Opaque to users; display_name provides human identity. Token resolution via config lookup (not in driveid). | Only `(driveID, itemID)` is guaranteed globally unique + stable. Display names solve readability. |
| **DP-7: Individual shared files** | Deferred to post-release. | No delta tracking for individual files. Focus on folder/drive sync story first. |
| **DP-8: Filter scoping** | Per-drive only. No global filter defaults. | Different drives have fundamentally different content. Global defaults create confusing inheritance. |
| **DP-9: Duplicate-source detection** | Warn (not block) when same shared folder synced from multiple places. | No data corruption — just wasted bandwidth/disk. Warning gives visibility. |
| **DP-10: Permission display** | `drive list` shows `(read-only)` / `(read-write)` for shared content. | Proactive visibility reduces confusion from 403 errors during sync. |

---

## 9. Operational Constraints

### 9.1 Linux inotify Watch Limits

Linux inotify requires one watch per directory. The default kernel limit is 8192 watches (`/proc/sys/fs/inotify/max_user_watches`), though many distributions set it higher (e.g., 65536). Multi-drive sync multiplies the problem — each non-paused drive's directory tree consumes watches independently.

**Detection at watch startup**: Before starting inotify watches, the engine reads `/proc/sys/fs/inotify/max_user_watches` and estimates the total watch count from the baseline directory counts across all non-paused drives.

**Warning threshold**: If the estimated watch count exceeds 80% of the kernel limit, the engine logs a warning with sysctl instructions:

```
WARN  inotify watch limit may be insufficient
  estimated_watches=6800  limit=8192  drives=3
  Increase with: sudo sysctl fs.inotify.max_user_watches=524288
  Persist with: echo 'fs.inotify.max_user_watches=524288' | sudo tee -a /etc/sysctl.conf
```

**Per-drive fallback**: If a drive exhausts available watches (`ENOSPC` from `inotify_add_watch`), that drive falls back to periodic full scan at `poll_interval`. Other drives retain their inotify watches. The fallback is per-drive, not global — one drive running out of watches does not degrade the others.

**No per-drive watch budget**: There is no quota or reservation system for inotify watches. Watches are allocated first-come first-served as drives start up. Drives that exhaust the limit fall back individually.

**macOS**: FSEvents has no per-directory watch limit — this is a Linux-only concern. macOS uses a single event stream per sync root, regardless of directory count.

---

## 10. Filter Scoping

All filter settings (`skip_dirs`, `skip_files`, `skip_dotfiles`, `max_file_size`, `sync_paths`, `ignore_marker`) are **per-drive only**. There are no global filter defaults.

Each drive gets built-in defaults (empty lists, `false`) unless it specifies its own filter values in its config section. A drive with no filter settings syncs everything (subject to built-in exclusions like `.partial` files).

**Rationale** (DP-8): Different drives have fundamentally different content — a personal OneDrive may contain photos and documents, while a business drive has code repositories. Global filter defaults create confusing inheritance: a user adds `skip_dirs = ["node_modules"]` globally, then wonders why their personal drive skips a folder named "node_modules" containing photos. Per-drive-only scoping makes the behavior predictable and self-contained.

**What this means for the config file**:

```toml
# No global filter settings — these keys exist only inside drive sections.

["personal:me@outlook.com"]
sync_dir = "~/OneDrive"
# No filter settings → syncs everything (built-in exclusions still apply)

["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
skip_dirs = ["node_modules", ".git", "vendor"]
skip_dotfiles = true
max_file_size = "100MB"
```

Non-filter settings (`poll_interval`, `log_level`, transfer settings, safety settings) retain their global-with-per-drive-override semantics. Only filter settings are per-drive native.

---

## 11. Multi-Drive Orchestrator

> **STATUS: RESOLVED** — Architecture A (per-drive goroutine with isolated engines) selected after analysis of four candidate architectures. All 10 open questions answered. See §11.11 for alternatives considered.

### 11.1 Architecture: Per-Drive Goroutine (Isolated Engines)

Each drive gets its own goroutine running its own Engine. A thin Orchestrator starts, stops, and monitors them. The proven sync pipeline (Engine, WorkerPool, DepTracker, Executor) stays exactly as-is — zero changes.

```
Orchestrator
├── DriveRunner[0]  →  Engine[0]  →  DepTracker[0] + WorkerPool[0] (N₀ workers)
├── DriveRunner[1]  →  Engine[1]  →  DepTracker[1] + WorkerPool[1] (N₁ workers)
└── DriveRunner[2]  →  Engine[2]  →  DepTracker[2] + WorkerPool[2] (N₂ workers)
     each with own: observers, buffer, planner, tracker, baseline, worker pool
```

**Why this architecture**: The theoretical advantage of shared worker pools barely matters in practice. ~95% of users have 1 drive (orchestrator invisible). Drives are rarely simultaneously busy — steady state is idle (empty delta responses). When work happens, it's almost always one drive at a time. Proportional allocation solves the real problem (don't dedicate half the workers to a drive with 5 files). Error isolation is critical and free — a corrupted SQLite DB, expired token, or network error in one drive affects nothing else. Architecture C (hybrid with fair scheduler) remains available as a future optimization if simultaneous multi-drive bursts become common.

### 11.2 New Types

```go
// Orchestrator manages the lifecycle of multiple DriveRunners.
// Watches config.toml via fsnotify for immediate config pickup.
type Orchestrator struct {
    runners      map[driveid.CanonicalID]*DriveRunner
    clients      map[string]*graph.Client  // keyed by token file path
    globalCap    int                       // total worker budget (configurable, default 16)
    configPath   string                    // path to config.toml (watched via fsnotify)
    logger       *slog.Logger
    mu           sync.Mutex
}

// DriveRunner wraps an Engine with lifecycle management.
type DriveRunner struct {
    engine     *sync.Engine
    canonID    driveid.CanonicalID
    config     *config.ResolvedDrive
    workers    int                       // allocated workers for this drive
    cancel     context.CancelFunc
    err        error                     // last error (for status reporting)
    logger     *slog.Logger
}
```

### 11.3 Worker Budget Algorithm

```
globalCap = config.MaxWorkers (default: 16, minimum: 4)
minPerDrive = 4

For each drive in config that is not paused:
    weight[drive] = max(1, drive.baselineFileCount)

totalWeight = sum(weight[all active drives])

For each active drive:
    allocated = max(minPerDrive, globalCap * weight[drive] / totalWeight)

// If sum(allocated) > globalCap due to minPerDrive floors, scale down proportionally
// but never below minPerDrive.
```

A drive with 100K files and 7 shared shortcuts gets ~90% of workers. A drive with 5 files gets 4 workers. Shortcuts don't affect allocation because they're internal to the Engine — the shortcut content is part of the file count. Rebalanced on config reload (fsnotify) when the active drive set changes.

### 11.4 graph.Client Pooling

```go
func (o *Orchestrator) getOrCreateClient(tokenPath string) *graph.Client {
    if client, ok := o.clients[tokenPath]; ok {
        return client
    }
    client := graph.NewClient(baseURL, httpClient, tokenSource, logger, userAgent)
    o.clients[tokenPath] = client
    return client
}
```

Multiple drives on the same account (personal + SharePoint libraries) share one client. 429 backoff is automatically coordinated because `graph.Client.Do()` handles retry/backoff per-request, and all drives using the same client wait on the same backoff.

### 11.5 Context Tree

```
processCtx (SIGTERM/SIGINT)
├── orchestratorCtx
│   ├── fsnotify watcher (config.toml)
│   ├── driveCtx[0] (cancelable independently)
│   │   └── Engine[0]
│   │       ├── observers
│   │       └── WorkerPool[0]
│   ├── driveCtx[1]
│   │   └── Engine[1]
│   └── driveCtx[2]
│       └── Engine[2]
```

Canceling `driveCtx[1]` stops only that drive's Engine (graceful shutdown: drain workers, commit pending). Canceling `orchestratorCtx` stops all drives. Config changes (via fsnotify) cancel and recreate individual driveCtxs as needed.

### 11.6 Error Isolation

Each DriveRunner catches panics in its goroutine and records the error:

```go
func (dr *DriveRunner) run(ctx context.Context) {
    defer func() {
        if r := recover(); r != nil {
            dr.err = fmt.Errorf("drive panic: %v", r)
            dr.logger.Error("drive panicked", slog.Any("recover", r))
        }
    }()
    // ... engine.RunOnce() or engine.RunWatch()
}
```

On persistent error (3 consecutive failures), the DriveRunner enters a backoff state: retry with exponential delay (1m, 5m, 15m, 1h). Other drives continue unaffected.

### 11.7 Config Reload (fsnotify)

The daemon watches `config.toml` via fsnotify for immediate config pickup:

```
1. Re-read and validate config file
   - Valid → proceed to step 2
   - Invalid → log warning with parse error, keep old config. No disruption.
2. Diff against current running drives:
   - New drives in config: create DriveRunner, allocate workers, start
   - Drives removed from config: cancel driveCtx, wait for shutdown, remove runner
   - Drives with paused=true added/changed: stop DriveRunner (or don't start)
   - Drives with paused removed: start DriveRunner
   - Hot-reloadable settings changed (filters, display_name): apply in-place
   - Non-hot-reloadable settings changed (sync_dir): log warning, requires restart
   - Unchanged drives: keep running
3. Rebalance worker allocations across active (non-paused) drives
```

CLI commands like `pause`, `resume`, `drive add`, `drive remove` write directly to `config.toml`. The daemon picks up changes within milliseconds via fsnotify. No RPC socket is needed for these operations.

Non-hot-reloadable settings (require daemon restart): `sync_dir`, `max_workers`, network config. Drive presence and `paused` state are immediate.

### 11.8 Aggregate Reporting

`onedrive-go status` works with or without the daemon by reading config + state DBs directly:

```
Drives:
  me@outlook.com              ~/OneDrive           ready (142 files, last sync 2m ago)
  Jane's Photos (read-only)   ~/OneDrive-Shared    paused
  me@contoso.com              ~/OneDrive-Contoso   token expired

Summary: 1 ready, 1 paused, 1 error
```

Status is determined from config (is drive present? is it paused?) + token (does token file exist? is it expired?) + baseline DB (file count, last sync time). No daemon communication needed. Removed drives (in shadow files) are not shown.

### 11.9 Daemon Model (Config-as-IPC)

`sync --watch` is the daemon entry point. It starts the Orchestrator, which:

1. Reads config, creates `graph.Client` pool
2. Starts DriveRunners for all non-paused drives (or zero if none exist / all paused)
3. Starts fsnotify watcher on `config.toml`
4. Enters main loop: select on fsnotify events, SIGTERM/SIGINT

The orchestrator can run with zero active DriveRunners — valid state. The daemon stays up, watches config, and starts drives as they appear.

**No RPC socket for Phase 7.0.** All control flows through the config file:
- `pause` → writes `paused = true` to config → fsnotify → daemon stops drive
- `resume` → removes `paused` from config → fsnotify → daemon starts drive
- `drive add` → adds section to config → fsnotify → daemon starts drive
- `drive remove` → moves section to shadow → fsnotify → daemon stops drive

```
onedrive-go sync --watch
  │
  ├── Read config → resolve drives
  ├── Create graph.Client pool
  ├── Start DriveRunners for non-paused drives
  ├── Start fsnotify watcher on config.toml
  │
  ├── select {
  │     case fsnotify event: → reload()
  │     case SIGTERM/INT:    → graceful shutdown
  │     case paused_until expires: → clear paused fields, write config
  │   }
  │
  └── On shutdown: stop all DriveRunners, exit
```

**Future (Phase 12.6)**: RPC socket can be added for live status data (in-flight action counts, real-time progress, SSE events) that can't be read from config + state DBs. This is additive — config-as-IPC remains the control mechanism.

### 11.10 Drive Lifecycle: Shadow Files

Two concepts control a drive's lifecycle, each with its own storage location:

| Concept | Question it answers | Controlled by | Stored in |
|---------|---------------------|---------------|-----------|
| **Drive exists** | "Is this drive part of my active setup?" | `drive add` / `drive remove` | Presence in `config.toml` vs shadow file |
| **Drive paused** | "Should this drive sync right now?" | `pause` / `resume` | `paused = true` in config section |

#### Shadow File Storage

When `drive remove` removes a drive section from `config.toml`, it writes that section to a **shadow file** in the data directory:

```
~/Library/Application Support/onedrive-go/          (macOS)
  config.toml                                        ← active drives
  shadow_personal_user@example.com.toml              ← removed drive's config
  token_personal_user@example.com.json               ← token (untouched)
  state_personal_user@example.com.db                 ← state DB (untouched)
```

Shadow files use the same `{prefix}_{type}_{email}.toml` naming convention as tokens and state DBs. One shadow file per drive. Contains the full drive section (sync_dir, paused, display_name, filters — everything).

#### Command Behaviors

**`drive add <canonical-id>`**:
1. Shadow file exists → restore: read shadow, append section to config.toml, delete shadow. All settings (sync_dir, paused state, display_name, filters) restored.
2. No shadow → add fresh section with computed default sync_dir.

**`drive remove [--drive X]`**:
1. Read drive section from config.toml.
2. Write it to shadow file.
3. Delete section from config.toml.
4. Drive disappears from drive list, status, sync. State DB, token, sync directory untouched.

**`drive remove --purge [--drive X]`**:
1. Delete section from config.toml (if present).
2. Delete shadow file (if present).
3. Delete state DB.
4. Token untouched (may be shared across drives). Sync directory untouched (user's files).

**`pause [--drive X] [duration]`**:
1. Set `paused = true` in drive's config section.
2. If duration given, also set `paused_until = 2026-02-28T18:00:00Z`.
3. Without `--drive`, pause all drives in config.

**`resume [--drive X]`**:
1. Remove `paused` and `paused_until` from drive's config section.
2. Without `--drive`, resume all drives.

**`login`**:
1. Authenticate, save token.
2. Drive already in config.toml → "Token refreshed." Config untouched.
3. Drive not in config.toml → check for shadow file. Shadow exists → auto-restore (seamless logout+login round-trip). No shadow → add fresh drive section.

**`logout [--account X]`**:
1. Delete token file.
2. For every drive belonging to that account in config.toml: `drive remove` (move to shadow).
3. State DBs, sync directories untouched. Shadow files preserve everything.

**`logout --purge [--account X]`**:
1. Delete token file.
2. For every drive belonging to that account: `drive remove --purge` (delete config section, shadow, state DB).
3. Sync directories untouched.

#### Drive State Table

| In config? | paused? | Token? | Display | Syncs? | How user got here |
|------------|---------|--------|---------|--------|-------------------|
| Yes | No | Valid | ready | Yes | Normal state |
| Yes | Yes | Valid | paused | No | Ran `pause` |
| Yes | Yes (timed) | Valid | paused (1h32m left) | No | Ran `pause 2h` |
| Yes | No | Expired | token expired | No | Token needs refresh |
| Yes | No | Missing | no token | No | Token deleted externally |
| No (shadow) | — | — | (not shown) | No | Ran `drive remove` or `logout` |
| Gone | — | — | (not shown) | No | Ran `--purge` |

When paused, that's the dominant display state. Token issues become visible on resume.

#### Timed Pause Expiry

When `paused_until` is set, the daemon checks on each sync cycle. When the time passes, the daemon clears both `paused` and `paused_until` from config.toml. The fsnotify-triggered reload then starts the drive.

#### Config File Example

User has two drives, personal paused:

```toml
# onedrive-go configuration

["personal:user@example.com"]
sync_dir = "~/OneDrive"
paused = true

["shared:user@example.com:SharedFolder"]
sync_dir = "~/SharedFolder"
```

After `drive remove --drive personal:user@example.com`:

```toml
# onedrive-go configuration

["shared:user@example.com:SharedFolder"]
sync_dir = "~/SharedFolder"
```

And in the data directory: `shadow_personal_user@example.com.toml` ← contains sync_dir, paused = true.

After `drive add personal:user@example.com`: section restored from shadow (including `paused = true`), shadow file deleted.

#### Changes from Today's Implementation

| Area | Today | New |
|------|-------|-----|
| `Drive.Enabled` field | `*bool` in config struct | Replaced by `Paused *bool` + `PausedUntil *string` |
| `drive remove` | Sets `enabled = false` in config | Moves section to shadow file, removes from config |
| `drive add` (existing) | Sets `enabled = true` | Restores from shadow file |
| Display state "paused" | Means `enabled = false` | Means `paused = true` (correct semantics) |
| `logout` | Deletes token, keeps drives in config | Deletes token, moves drives to shadow |
| `login` (re-login) | Token refresh only | Token refresh + restore from shadow if shadow exists |
| Config reload | Per-cycle (up to 5 min delay) | fsnotify on config.toml (immediate, validated) |
| `pause`/`resume` commands | Not implemented | New commands, write `paused` to config |

### 11.11 CLI Command Categorization

All CLI commands are standalone — they read/write config, state DBs, and tokens directly. No daemon communication required for Phase 7.0.

**Config-modifying commands** (trigger daemon reload via fsnotify if daemon is running):

| Command | Config effect |
|---------|---------------|
| `pause [--drive X] [duration]` | Sets `paused = true` (+ `paused_until`) in config |
| `resume [--drive X]` | Removes `paused` / `paused_until` from config |
| `drive add` | Adds section to config (restores from shadow if available) |
| `drive remove` | Moves section to shadow, removes from config |
| `login` | Adds drive section (or restores from shadow) if new |
| `logout` | Moves drive sections to shadow |

**Read-only commands** (work identically with or without daemon):

| Command | Data source |
|---------|-------------|
| `status` | Config (drive presence, paused state) + token files (valid/expired/missing) + state DBs (file count, last sync time) |
| `drive list` | Config (active drives only — shadow drives invisible) |
| `whoami` | Token files |
| `conflicts`, `verify` | State DBs |

**Direct API commands** (standalone, short-lived graph.Client):

| Command | Notes |
|---------|-------|
| `ls`, `get`, `put`, `rm`, `mkdir`, `stat` | Direct file operations via Graph API |
| `sync` (one-shot) | Run one sync cycle per drive and exit |
| `drive search` | Search available drives via Graph API |

**Daemon entry point**:

| Command | Behavior |
|---------|----------|
| `sync --watch` | Start daemon. Fail with "already running" if PID lock exists. |

### 11.12 Concurrency Stages and Control Mechanisms

Each stage of the sync pipeline has exactly ONE bottleneck and exactly ONE control mechanism. No overlapping mechanisms.

| Stage | Bottleneck | Control Mechanism | Scope | Phase 7.0 | Future |
|-------|-----------|-------------------|-------|-----------|--------|
| Walk | Disk metadata I/O | None (sequential) | Per-drive | — | — |
| Hash | Disk read throughput | Global semaphore (`parallel_checkers`) | Global | Static (auto-detect) | Same semaphore, AIMD-sized |
| Execution | Network (latency + bandwidth) | Worker count per drive | Per-drive | Static proportional budget | AIMD per-account (replaces static) |
| DB commits | SQLite write | None (not a bottleneck) | Per-drive | — | — |
| Memory | Buffer size | Buffer backpressure | Per-drive | None needed | High/low water marks |

No overlapping mechanisms: hashing has ONE control (semaphore). Execution has ONE control (worker count). Buffer has ONE control (backpressure). They are in different pipeline stages and cannot conflict. Future AIMD is not a new, additional mechanism — it dynamically adjusts the same control knob (semaphore limit or worker count) that Phase 7.0 sets statically.

### 11.13 Resource Sharing (Non-Orchestrator Concerns)

**graph.Client pooling**: `map[tokenPath]*graph.Client` in Orchestrator. Created once per unique token path. Thread-safe (`graph.Client` is stateless per-request).

**429 rate limiting**: Per-account (per OAuth token), NOT global. Handled entirely by `graph.Client` — when a request gets 429 with `Retry-After`, the client sleeps for that duration before retrying. Drives sharing a token share a client, so they naturally wait on the same backoff. Throttled goroutines sleep (~8KB each, no CPU/disk/bandwidth). No orchestrator-level 429 concern.

**Bandwidth limiting**: Global token bucket on shared `http.RoundTripper` (per concurrent-execution.md §7). One physical network = one bandwidth limit, shared across ALL drives regardless of account.

**Checker pool**: Global `*semaphore.Weighted` shared across all `LocalObserver` instances. Limit auto-detected by storage type (SSD: 8, HDD: 2, unknown: 4) or configurable via `parallel_checkers`. Detect storage type at startup via `/sys/block/*/queue/rotational` on Linux, heuristics on macOS.

### 11.14 Answers to Open Questions

| # | Question | Answer |
|---|----------|--------|
| 1 | Orchestrator struct and lifecycle | `Orchestrator` manages `map[CanonicalID]*DriveRunner`. Each DriveRunner owns an Engine. `sync` (one-shot) runs all non-paused drives concurrently and exits. `sync --watch` starts the daemon: watches config.toml via fsnotify, starts/stops DriveRunners as drives are added/removed/paused. |
| 2 | ClientPool | `map[tokenPath]*graph.Client` in Orchestrator. Created once per token path. Shared by reference. Thread-safe. |
| 3 | Worker budget | Global cap (`max_workers`, default 16), proportional allocation by baseline file count, minimum 4 per drive. Rebalanced on config reload. |
| 4 | Error isolation | Per-drive goroutine with panic recovery. 3 consecutive failures → exponential backoff. Other drives unaffected. |
| 5 | Watch mode lifecycle | Daemon watches config.toml via fsnotify. CLI commands write to config → daemon picks up changes within milliseconds. Drive presence controlled by config vs shadow files. Paused state persisted in config. |
| 6 | Aggregate reporting | `status` reads config + token files + state DBs directly. Works with or without daemon. Shows: ready/paused/token expired. Removed drives (in shadow) are invisible. |
| 7 | Context tree | `processCtx → orchestratorCtx → driveCtx[i] → Engine[i]`. Independent cancellation per drive. |
| 8 | Bandwidth limiting | Global token bucket on shared `http.RoundTripper` across ALL drives. One physical network = one bandwidth limit. |
| 9 | Rate limit coordination | 429s are per-account (per OAuth token), NOT global. Handled entirely by `graph.Client`. Drives sharing a token share a client, which handles 429 backoff per-request. |
| 10 | Checker pool | Global `*semaphore.Weighted` shared across all `LocalObserver` instances. Limit auto-detected by storage type or configurable via `parallel_checkers`. |

### 11.15 Alternatives Considered

Three alternative architectures were evaluated and rejected:

**Architecture B: Shared Worker Pool (Centralized Execution)** — All drives share a single DepTracker and WorkerPool. Each drive has its own observers, buffer, and planner. Optimal worker utilization but poor error isolation (panic in any worker kills the shared pool), high complexity (Engine must be split into observation+planning vs execution), problematic DepTracker mutex contention, and starvation risk. ~800 LOC new code. Rejected for complexity and error isolation concerns.

**Architecture C: Hybrid (Per-Drive Observation + Shared Execution via Fair Scheduler)** — Each drive has its own DepTracker shard. A FairScheduler multiplexes ready actions from all shards onto a shared WorkerPool. Near-optimal worker utilization, good error isolation (per-drive tracker shards isolate dependency graphs), no DepTracker contention between drives. ~500 LOC new code. Not rejected — available as a future optimization if simultaneous multi-drive bursts become common and Architecture A is measured as the bottleneck.

**Architecture D: Actor Model** — Each component is an actor with its own goroutine and mailbox. Highest theoretical elegance (no shared mutable state, deadlock-free by construction) but unacceptably high complexity (~1000+ LOC, 15-20 message types), poor Go idiom fit, minimal existing code reuse. Rejected for ceremony-to-value ratio.

### Constraints

- Each drive has its own state DB (already the case)
- Each drive has its own delta token(s) (already the case)
- Drives sharing a token file MUST share a `graph.Client` (rate limit correctness)
- The orchestrator handles drives being added/removed at runtime (fsnotify on config.toml)
- Memory and CPU usage remain within PRD targets (< 100 MB for 100K files, < 1% CPU idle)

---

## References

- [Accessing shared files and folders - OneDrive API](https://learn.microsoft.com/en-us/onedrive/developer/rest-api/concepts/using-sharing-links?view=odsp-graph-online)
- [driveItem: delta - Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/driveitem-delta?view=graph-rest-1.0)
- [driveItem resource type](https://learn.microsoft.com/en-us/graph/api/resources/driveitem?view=graph-rest-1.0)
- [OneDrive family and group sharing](https://techcommunity.microsoft.com/blog/onedriveblog/onedrive-family-and-group-sharing-now-available/1816818)
- [Personal Vault](https://support.microsoft.com/en-us/office/protect-your-onedrive-files-in-personal-vault-6540ef37-e9bf-4121-a773-56f98dce78c4)
- accounts.md — Current account/drive design
- issues-api-inconsistencies.md §9.2 — Personal Vault API limitation
- issues-feature-requests.md §5.1 — Shared folder sync (#1 most discussed feature)
- ref-sync-algorithm.md — Reference implementation's shared folder handling
- api-analysis.md §5.5 — Shared folder differences between Personal/Business/SharePoint
