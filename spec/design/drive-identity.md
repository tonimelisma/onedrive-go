# Drive Identity

GOVERNS: internal/driveid/canonical.go, internal/driveid/id.go, internal/driveid/itemkey.go, drive.go, purge.go

Implements: R-3.2 [verified], R-3.5 [verified], R-3.3.12 [verified], R-6.7.2 [verified], R-3.6.4 [verified], R-3.6.5 [planned], R-6.10.6 [verified]

## Core Concepts

**Accounts** are what you authenticate with (Microsoft accounts). **Drives** are what you sync. Each drive has a canonical identifier derived from real data — no arbitrary names.

## Ownership Contract

- Owns: Canonical drive-identity formats, parsing/formatting, display-name rules, and CLI drive matching semantics.
- Does Not Own: Config scanning, token resolution, Graph discovery, or sync runtime policy.
- Source of Truth: Canonical IDs derived from authenticated account and drive metadata; display names are config-owned user-facing labels layered on top of those identities.
- Allowed Side Effects: The leaf `driveid` package is pure. CLI drive commands governed here perform config mutations through the config layer, not through identity types.
- Mutable Runtime Owner: None. Identity values are pure value types and CLI-local state is invocation-scoped.
- Error Boundary: Parsing and matching errors stop at the CLI/config boundary; identity types do not translate transport or filesystem failures.

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

## Shared Item Selectors

The CLI now also uses `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>`
as a user-facing selector for shared files and shared folders discovered by the
`shared` command. That wire format intentionally matches the canonical shared
drive ID shape, but it is not the same concept:

- `driveid.CanonicalID` still means "configured drive identity"
- `internal/sharedref.Ref` means "one shared item target for one recipient account"

Shared folders may flow from selector form into `drive add`, which then
normalizes them into configured shared drives. Shared files never become drives
and are rejected by `drive add`.

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

Implements: R-3.3.2 [verified], R-3.3.3 [verified], R-3.3.4 [verified], R-3.3.5 [verified], R-3.3.6 [verified], R-3.3.7 [verified], R-3.3.8 [verified], R-3.3.9 [verified], R-3.6.1 [verified], R-3.6.2 [verified], R-3.6.3 [verified]

`drive list`, `drive add`, `drive remove`, `drive search`. Drive add creates a config section with auto-generated display_name and sync_dir. Drive list annotates available drives with state DB presence (R-3.3.3), supports `--all` to remove the SharePoint site cap (R-3.3.4). Drive remove `--purge` works on unconfigured drives with orphaned state (R-3.3.8).

## Design Constraints

- DriveID is cached in token metadata at login — NEVER stored in config. `discoverAccount()` returns the primary drive ID, saved via `tokenfile.LoadAndMergeMeta()`. Both the multi-drive control plane and the single-drive Engine reject zero DriveID.
- SharePoint document libraries have their OWN drive_id, different from the business account's primary drive. Drive metadata files (`drive_*.json`) store the correct per-drive ID.

### Rationale

- **`driveid` is a leaf package** (stdlib only): parsing, construction, formatting. No business logic, no config imports.
- **Token resolution in `config`**, not `driveid`: determining which token file a shared drive uses requires scanning all configured drives — that's business logic.
- **Display names replace aliases**: one field, one purpose. Email is already unique and human-readable for personal/business.
- **Per-drive filter scoping** (DP-8): different drives have fundamentally different content. Global defaults create confusing inheritance.
