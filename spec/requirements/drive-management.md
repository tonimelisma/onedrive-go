# R-3 Drive Management

Authentication, account management, drive types, and shared drive support.

## R-3.1 Authentication [verified]

- R-3.1.1: When the user runs `login`, the system shall authenticate via device code flow (default). [verified]
- R-3.1.2: When `--browser` is passed, the system shall use PKCE authorization code flow with localhost callback. [verified]
- R-3.1.3: When the user runs `logout`, the system shall remove the authentication token and config sections, preserving state databases, account profiles, and drive metadata. [verified]
- R-3.1.4: When `--purge` is passed with logout, the system shall also delete state databases, account profiles, and drive metadata. `--purge` shall work after a prior non-purge logout. [verified]
- R-3.1.5: When the user runs `whoami`, the system shall display authenticated accounts and logged-out accounts whose account profile files remain (not yet purged). [verified]

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
- R-3.3.2: When the user runs `drive list`, the system shall show two sections.
  Section 1 ("Configured drives"): every drive in config with display name,
  sync directory (blank if not set), and state (ready or paused). Section 2
  ("Available drives"): all not-yet-added drives discovered from authenticated
  accounts — personal and business drives, SharePoint document libraries
  (business accounts only, capped at 10 sites), and shared folders (all
  account types). Drives already in config shall not appear in the available
  section. [verified]
- R-3.3.3: Available drives in `drive list` that retain a state database from a
  previous configuration shall be marked as such, indicating they can be
  re-added without a full re-sync or purged with `drive remove --purge`.
  [implemented]
- R-3.3.4: When `--all` is passed to `drive list`, the system shall remove the
  SharePoint site discovery cap and show all discoverable drives. [implemented]
- R-3.3.5: When the user runs `drive add <canonical-id>`, the system shall add
  the specified drive to the configuration, provided a valid token exists for
  the account. This works for any drive type: personal, business, SharePoint,
  or shared — including re-adding a default drive previously removed via
  `drive remove`. If the drive is already configured, the system shall report
  it as such without creating a duplicate. [verified]
- R-3.3.6: When a search term without `:` is passed to `drive add`, the system
  shall match against shared folder names using case-insensitive substring
  search across all authenticated accounts. A single match auto-adds the drive.
  Multiple matches display a numbered list with canonical IDs. Zero matches
  return an error suggesting `drive list`. [verified]
- R-3.3.7: When the user runs `drive remove --drive <id>`, the system shall
  remove only the drive's config section. The account token is preserved (the
  account remains logged in), the state database is preserved (so a future
  `drive add` avoids a full re-sync), and the sync directory is preserved (the
  user's files remain on disk). After removal, the drive appears as "available"
  in `drive list` and can be re-added with `drive add`. [verified]
- R-3.3.8: When `--purge` is passed to `drive remove`, the system shall also
  delete the state database. This works both for configured drives (removing
  config section and state database) and for available drives that retain a
  state database from a previous removal (deleting only the state database).
  The account token and sync directory are always preserved. [implemented]
- R-3.3.9: When the user runs `drive search <term>`, the system shall search
  SharePoint sites by name across all business accounts, returning up to 50
  matching sites with their document libraries and canonical IDs. When
  `--account` is passed, the search is restricted to that business account.
  [verified]

## R-3.4 Multi-Account [verified]

- R-3.4.1: A single config file shall hold multiple drive sections. [verified]
- R-3.4.2: A single `sync --watch` process shall sync all non-paused drives simultaneously. [verified]
- R-3.4.3: SharePoint drives shall share the business account's OAuth token. [verified]

## R-3.5 Drive Selection [verified]

- R-3.5.1: When `--drive` is passed, the system shall match by canonical ID, display name, or substring. [verified]
- R-3.5.2: When `--account` is passed on auth commands, the system shall select by email. [verified]
- R-3.5.3: `--drive` shall be repeatable for sync/status to select multiple drives. [verified]

## R-3.6 Shared Drive Discovery [implemented]

- R-3.6.1: When the user runs `drive list`, the system shall show available shared folders. [verified]
- R-3.6.2: The system shall use non-deprecated search API for shared item discovery (SharedWithMe deprecated Nov 2026). [verified]
- R-3.6.3: When adding a shared drive, the system shall derive a display name from the sharer's identity. [verified]
- R-3.6.4: When discovering shared items via the Search API (which returns less identity data than the deprecated SharedWithMe endpoint), the system shall enrich results by making secondary `GET /drives/{driveId}/items/{itemId}` calls to retrieve full identity data including email. [planned]
- R-3.6.5: When a user attempts to access shared folders from outside their organization (invisible to Graph API), the system shall provide a clear error message explaining the platform limitation. [planned]

## R-3.7 Email Change Detection [future]

- R-3.7.1: The system shall store a stable user GUID from the Graph API alongside the email. [future]
- R-3.7.2: When a user's email changes, the system shall auto-rename token files, state DBs, and config sections. [future]

## R-3.8 Migration [future]

- R-3.8.1: When the user runs `migrate`, the system shall auto-detect abraunegg/onedrive or rclone configs. [future]
- R-3.8.2: The system shall convert filter rules and sync settings to equivalent config. [future]
- R-3.8.3: The system shall NOT migrate auth tokens (different OAuth app ID; re-auth required). [future]
