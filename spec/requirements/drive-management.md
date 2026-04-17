# R-3 Drive Management

Authentication, account management, drive types, and shared drive support.

## R-3.1 Authentication [verified]

- R-3.1.1: When the user runs `login`, the system shall authenticate via device code flow (default). [verified]
- R-3.1.2: When `--browser` is passed, the system shall use PKCE authorization code flow with localhost callback. [verified]
- R-3.1.3: When the user runs `logout`, the system shall remove the authentication token and configured drive sections while preserving retained state databases and the managed catalog inventory for that account. [verified]
- R-3.1.4: When `--purge` is passed with logout, the system shall also delete retained state databases and remove the purged account and owned drives from the managed catalog. `--purge` shall work after a prior non-purge logout. [verified]
- R-3.1.5: When the user runs `status`, the system shall display all known accounts, including accounts with zero configured drives. Each account card shall include account identity when known, account-level authentication state, and configured-drive status rows. The auth-required set may come from catalog-known accounts with no saved login, invalid saved-login files on disk, or a persisted catalog auth-requirement reason. If authenticated live discovery degrades after `/me` succeeds but `/me/drives` still exhausts its transient retry budget, the command shall keep the authenticated account visible and report the degraded discovery on that account card rather than collapsing it into auth-required state. [verified]
- R-3.1.6: When `--json` is passed to `status`, the system shall output structured JSON account cards including account auth state, any live identity fields (`user_id`, `live_drives`) recovered by authenticated discovery, any degraded discovery fields (`degraded_reason`, `degraded_action`), configured-drive rows, and the overall summary. [verified]
- R-3.1.7: When `logout` runs without `--account`, account auto-selection shall use the durable offline account catalog rather than only configured drives. Plain `logout` shall auto-select only when exactly one known account has a usable saved login. When multiple accounts have usable saved logins, `--account` is required. When no known account has a usable saved login, plain `logout` shall not auto-select an account. [verified]
- R-3.1.8: Degraded discovery is a command-local overlay, not a durable account state. It shall not be persisted to config, catalog inventory, or state DBs, and it shall clear automatically on a later successful discovery command. [verified]

## R-3.2 Drive Types [verified]

The system shall support all four drive types:

- R-3.2.1: OneDrive Personal (consumer Microsoft accounts). [verified]
- R-3.2.2: OneDrive Business (Microsoft 365 / Azure AD work accounts). [verified]
- R-3.2.3: SharePoint Document Libraries (via `drive add`). [verified]
- R-3.2.4: Shared Folders (folders shared by other users, synced as separate drives). [verified]

## R-3.3 Drive Management Commands [implemented]

- R-3.3.1: When the user runs `login`, the system shall auto-add the account's
  primary drive to the configuration: `personal:<email>` for consumer Microsoft
  accounts, `business:<email>` for work/school accounts. Re-login shall refresh
  the token and metadata without duplicating the drive section. [verified]
- R-3.3.2: When the user runs `drive list`, the system shall show two drive sections plus account-level warnings when needed. Section 1 ("Configured drives") shall list every drive in config with display name, sync directory (blank if not set), operational state (ready or paused), and auth status. Section 2 ("Available drives") shall list all not-yet-added drives discovered from authenticated accounts — personal and business drives, SharePoint document libraries (business accounts only, capped at 10 sites), and shared folders (all account types). Drives already in config shall not appear in the available section. If account discovery cannot proceed because authentication is required, the command shall surface that as an account-level warning instead of silently omitting the account. If `/me/drives` is temporarily unavailable after exhausting its transient retry budget, the command shall keep the account visible under `accounts_degraded`, fall back to `/me/drive` for the primary personal/business drive when possible, and continue independent shared-folder and SharePoint discovery. [verified]
- R-3.3.3: Available drives in `drive list` that retain a state database from a
  previous configuration shall be marked as such, indicating they can be
  re-added without a full re-sync or purged with `drive remove --purge`.
  [verified]
- R-3.3.4: When `--all` is passed to `drive list`, the system shall remove the
  SharePoint site discovery cap and show all discoverable drives. [verified]
- R-3.3.5: When the user runs `drive add <canonical-id>`, the system shall add
  the specified drive to the configuration, provided a valid token exists for
  the account. This works for any drive type: personal, business, SharePoint,
  or shared — including re-adding a default drive previously removed via
  `drive remove`. If the drive is already configured, the system shall report
  it as such without creating a duplicate. [verified]
- R-3.3.6: When a search term without `:` is passed to `drive add`, the system
  shall match against shared folder names using case-insensitive substring
  search across the authenticated shared-discovery account set, honoring
  `--account` when present. A single match auto-adds the drive. Multiple
  matches display a numbered list with canonical IDs. Zero matches return an
  error that still suggests `drive list`, while also surfacing any
  auth-required or degraded accounts that prevented shared discovery from being
  complete. [verified]
- R-3.3.7: When the user runs `drive remove --drive <id>`, the system shall
  remove only the drive's config section. The account token is preserved (the
  account remains logged in), the state database is preserved (so a future
  `drive add` avoids a full re-sync), and the sync directory is preserved (the
  user's files remain on disk). After removal, the drive appears as "available"
  in `drive list` and can be re-added with `drive add`. Removing the final
  configured drive leaves `config.toml` in place with zero drive sections.
  [verified]
- R-3.3.8: When `--purge` is passed to `drive remove`, the system shall also
  delete the state database. This works both for configured drives (removing
  config section and state database) and for available drives that retain a
  state database from a previous removal (deleting only the state database).
  The account token and sync directory are always preserved; the catalog drive
  record is removed only when no longer needed as retained inventory.
  [verified]
- R-3.3.9: When the user runs `drive search <term>`, the system shall search SharePoint sites by name across all business accounts, returning up to 50 matching sites with their document libraries and canonical IDs. When `--account` is passed, the search is restricted to that business account. Business accounts that cannot be searched because authentication is required shall be reported as account-level auth-required results instead of being silently skipped. [verified]
- R-3.3.10: When `--json` is passed, `drive list` shall output structured JSON with `configured`, `available`, `accounts_requiring_auth`, and `accounts_degraded` arrays. Configured drive entries shall include auth status when authentication is required. [verified]
- R-3.3.11: When `--json` is passed, `drive search` shall output structured JSON with `results` and `accounts_requiring_auth` arrays. [verified]
- R-3.3.12: When the user runs `drive add shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>` and the selector resolves to a shared file instead of a shared folder, the system shall reject it with guidance to use direct file commands (`stat`, `get`, `put`) instead of configuring it as a drive. [verified]
- R-3.3.13: When the user runs `drive add <raw-share-url>` and the URL resolves to a shared folder, the system shall normalize that link to the canonical shared drive ID and configure the shared folder as a drive without depending on search or `sharedWithMe` discovery. Shared-file links shall continue to be rejected with the direct file-command guidance. [verified]

## R-3.4 Multi-Account [verified]

- R-3.4.1: A single config file shall hold multiple drive sections. [verified]
- R-3.4.2: A single `sync --watch` process shall sync all non-paused drives simultaneously. [verified]
- R-3.4.3: SharePoint drives shall share the business account's OAuth token. [verified]

## R-3.5 Drive Selection [verified]

- R-3.5.1: When `--drive` is passed, the system shall match by canonical ID, display name, or substring. [verified]
- R-3.5.2: When `--account` is passed on auth commands, the system shall select by email. [verified]
- R-3.5.3: `--drive` shall be repeatable for sync/status to select multiple drives. [verified]

## R-3.6 Shared Drive Discovery [verified]

- R-3.6.1: When the user runs `drive list`, the system shall show available shared folders. [verified]
- R-3.6.2: The system shall use non-deprecated search API for shared item discovery (SharedWithMe deprecated Nov 2026). [verified]
- R-3.6.3: When adding or listing a shared drive, the system shall derive a display name from the sharer's identity when available, and shall fall back to a deterministic remote-identity name when owner identity remains unavailable after enrichment. Shared-folder presentation shall explain that the owner identity is unavailable from Microsoft Graph and that retrying later may recover it. [verified]
- R-3.6.4: When discovering shared items via the Search API, the system shall enrich actionable results by making secondary `GET /drives/{driveId}/items/{itemId}` calls to retrieve fuller identity data. Actionable shared items shall remain visible even when enrichment still cannot recover owner identity. In that case the CLI shall surface an explicit item-level owner-identity status rather than relying on omitted fields or bare `unknown` placeholders. [verified]
- R-3.6.5: Shared-drive discovery shall use best-effort live search only. If live search fails for an otherwise authenticated account, the CLI shall surface that account under degraded discovery instead of silently omitting it. If live search returns `unauthorized`, the CLI shall surface that account under `accounts_requiring_auth` with the sync-auth-rejected reason rather than degrading it. If live search succeeds but Microsoft Graph still omits an external or cross-organization shared folder, the CLI shall explain the platform limitation clearly and point the user at discovery commands that show what the API actually exposed. [verified]
- R-3.6.6: When the user runs `shared`, the system shall list files and folders shared with the authenticated account set, using generated `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>` selectors as actionable targets because Graph discovery does not reliably expose the original inbound share URL. Accounts that cannot be queried because authentication is required, including live search returning `unauthorized`, shall be reported instead of being silently omitted. [verified]
- R-3.6.7: When `--json` is passed to `shared`, the system shall output structured JSON with `items`, `accounts_requiring_auth`, and `accounts_degraded`. Each item shall include the shared selector plus the discovered shared item metadata needed for follow-on commands, including an explicit owner-identity availability status for shared-owner enrichment results. [verified]

## R-3.7 Email Change Detection [verified]

- R-3.7.1: The system shall store a stable user GUID from the Graph API alongside the email. [verified]
- R-3.7.2: When a user's email changes, the system shall auto-rename token files, state DBs, and config sections. [verified]

## R-3.8 Migration [future]

- R-3.8.1: When the user runs `migrate`, the system shall auto-detect abraunegg/onedrive or rclone configs. [future]
- R-3.8.2: The system shall convert filter rules and sync settings to equivalent config. [future]
- R-3.8.3: The system shall NOT migrate auth tokens (different OAuth app ID; re-auth required). [future]
