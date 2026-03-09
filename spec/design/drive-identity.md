# Drive Identity

GOVERNS: internal/driveid/canonical.go, internal/driveid/id.go, internal/driveid/itemkey.go, drive.go

Implements: R-3.2 [implemented], R-3.5 [implemented], R-6.7.2 [implemented]

## Core Concepts

**Accounts** are what you authenticate with (Microsoft accounts). **Drives** are what you sync. Each drive has a canonical identifier derived from real data — no arbitrary names.

## Canonical Drive ID

Format: `type:email[:site:library]` or `shared:email:sourceDriveID:sourceItemID`.

Four drive types:
- `personal:email` — one per personal Microsoft account
- `business:email` — one per business/work account
- `sharepoint:email:site:library` — many per business account (same token)
- `shared:email:sourceDriveID:sourceItemID` — shared folders synced as separate drives

Rules:
- Constructed from real data (drive type from API, email from `mail` field with `userPrincipalName` fallback)
- `:` is the separator in display/config form, replaced with `_` in filenames
- Drive type auto-detected from auth endpoint + `driveType` field

## ID Type (`driveid.ID`)

Normalized API drive identifier. Lowercase, zero-padded to 16 chars (handles Personal driveId truncation where leading zero is dropped). Pure value type — no business logic.

## CanonicalID Type (`driveid.CanonicalID`)

Config-level identifier. Parsed from/to string form. Provides `DriveType()`, `Email()`, accessors. Pure identity — no config or business logic imports.

## ItemKey Type (`driveid.ItemKey`)

Composite `(DriveID, ItemID)` map key for cross-drive item lookup. Used by the sync engine for collision detection.

## Display Names

Every drive gets a `display_name` auto-generated at add time:
- Personal/business: email address
- SharePoint: "site / library"
- Shared: "{Name}'s {Folder}"

User-editable. Used in CLI output, `--drive` matching, error messages, and logs.

## Drive Matching (`--drive`)

Resolution order: exact canonical ID → exact display_name (case-insensitive) → substring on canonical ID, display_name, or owner. Ambiguous match → error with suggestions.

## CLI Drive Command (`drive.go`)

`drive list`, `drive add`, `drive remove`, `drive search`. Drive add creates a config section with auto-generated display_name and sync_dir.

## Design Constraints

- DriveID is cached in token metadata at login — NEVER stored in config. `discoverAccount()` returns the primary drive ID, saved via `tokenfile.LoadAndMergeMeta()`. Both Orchestrator and Engine reject zero DriveID.
- SharePoint document libraries have their OWN drive_id, different from the business account's primary drive. Drive metadata files (`drive_*.json`) store the correct per-drive ID.

### Rationale

- **`driveid` is a leaf package** (stdlib only): parsing, construction, formatting. No business logic, no config imports.
- **Token resolution in `config`**, not `driveid`: determining which token file a shared drive uses requires scanning all configured drives — that's business logic.
- **Display names replace aliases**: one field, one purpose. Email is already unique and human-readable for personal/business.
- **Per-drive filter scoping** (DP-8): different drives have fundamentally different content. Global defaults create confusing inheritance.
