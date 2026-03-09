# R-3 Drive Management

Authentication, account management, drive types, and shared drive support.

## R-3.1 Authentication [implemented]

- R-3.1.1: When the user runs `login`, the system shall authenticate via device code flow (default). [implemented]
- R-3.1.2: When `--browser` is passed, the system shall use PKCE authorization code flow with localhost callback. [implemented]
- R-3.1.3: When the user runs `logout`, the system shall remove authentication state. [implemented]
- R-3.1.4: When `--purge` is passed with logout, the system shall also delete state DBs and config sections. [implemented]
- R-3.1.5: When the user runs `whoami`, the system shall display authenticated accounts. [implemented]

## R-3.2 Drive Types [verified]

The system shall support all four drive types:

- R-3.2.1: OneDrive Personal (consumer Microsoft accounts). [verified]
- R-3.2.2: OneDrive Business (Microsoft 365 / Azure AD work accounts). [verified]
- R-3.2.3: SharePoint Document Libraries (via `drive add`). [verified]
- R-3.2.4: Shared Folders (folders shared by other users, synced as separate drives). [verified]

## R-3.3 Drive Management Commands [verified]

- R-3.3.1: When the user runs `drive list`, the system shall show all configured drives with status. [verified]
- R-3.3.2: When the user runs `drive add`, the system shall add a SharePoint library or shared folder. [verified]
- R-3.3.3: When the user runs `drive remove`, the system shall remove the drive config section. [verified]
- R-3.3.4: When the user runs `drive search`, the system shall search SharePoint sites by name. [verified]

## R-3.4 Multi-Account [implemented]

- R-3.4.1: A single config file shall hold multiple drive sections. [implemented]
- R-3.4.2: A single `sync --watch` process shall sync all non-paused drives simultaneously. [implemented]
- R-3.4.3: SharePoint drives shall share the business account's OAuth token. [implemented]

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
