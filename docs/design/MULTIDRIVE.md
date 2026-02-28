# Multi-Account / Multi-Drive Architecture

> **Status**: All decision points resolved. Ready for implementation.

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
    Enabled     *bool    `toml:"enabled,omitempty"`
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
  Jane's Photos                ~/OneDrive-Shared/Jane's Photos    ready

Available drives (not configured):
  me@contoso.com
  Marketing / Documents
  Bob's Project Files          (shared by bob@contoso.com)
  Grandma's Recipes            (shared by grandma@outlook.com)

Run 'onedrive-go drive add <name>' to add a drive.
```

- Shows display_name for all drives
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
| No flags | All enabled drives |
| `--account alice@contoso.com` | All enabled drives under that account |
| `--drive "Jane's Photos"` | Just that one drive |
| `--drive work --drive personal` | Those specific drives |

`--download-only` and `--upload-only` are `sync`-command flags that affect the current invocation for whatever drives are in scope. They are not persisted in config. No per-drive sync mode setting exists.

```bash
onedrive-go sync --download-only                              # all enabled drives, download-only
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
