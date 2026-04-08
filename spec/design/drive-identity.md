# Drive Identity

GOVERNS: internal/driveid/canonical.go, internal/driveid/id.go, internal/driveid/itemkey.go, drive.go, purge.go

Implements: R-3.2 [verified], R-3.5 [verified], R-3.7 [verified], R-3.3.12 [verified], R-3.3.13 [verified], R-6.7.2 [verified], R-3.6.4 [verified], R-3.6.5 [verified], R-6.10.6 [verified]

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

Config-level identifier. Parsed from/to string form. Provides `DriveType()`, `Email()`, accessors, and `WithEmail()` for exact email substitution while preserving the type-specific identity payload (SharePoint site/library or shared source IDs). Pure identity — no config or business logic imports.

## Email Rewrite Contract

Email change detection is a config/CLI workflow, but `driveid.CanonicalID`
owns the canonical rewrite rule itself: replacing the account email must keep
the rest of the identity stable.

- `personal` / `business`: rewrite only `type:email`
- `sharepoint`: rewrite only the account email; keep `site` and `library`
- `shared`: rewrite only the account email; keep `sourceDriveID` and `sourceItemID`

That keeps email migration deterministic and prevents CLI/config code from
hand-assembling canonical-ID strings in multiple places.

## Shared Item Selectors

The CLI now also uses `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>`
as a user-facing selector for shared files and shared folders discovered by the
`shared` command. That wire format intentionally matches the canonical shared
drive ID shape, but it is not the same concept:

- `driveid.CanonicalID` still means "configured drive identity"
- `internal/sharedref.Ref` means "one shared item target for one recipient account"

Shared folders may flow into `drive add` from either selector form or the
original raw share URL. Both inputs are normalized immediately to the canonical
`shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>` drive ID before any
config mutation happens. Shared files never become drives and are rejected by
`drive add`.

## ItemKey Type (`driveid.ItemKey`)

Composite `(DriveID, ItemID)` map key for cross-drive item lookup. Used by the sync engine for collision detection.

## Display Names

Every drive gets a `display_name` auto-generated at add time:
- Personal/business: email address
- SharePoint: "site / library"
- Shared: "{Name}'s {Folder}"

User-editable. Used in CLI output, `--drive` matching, error messages, and logs.

## Drive Matching (`--drive`)

Resolution order: exact canonical ID → exact display_name (case-insensitive) → substring on canonical ID. Ambiguous match → error with suggestions.

## CLI Drive Command (`drive.go`)

Implements: R-3.3.2 [verified], R-3.3.3 [verified], R-3.3.4 [verified], R-3.3.5 [verified], R-3.3.6 [verified], R-3.3.7 [verified], R-3.3.8 [verified], R-3.3.9 [verified], R-3.3.12 [verified], R-3.3.13 [verified], R-3.6.1 [verified], R-3.6.2 [verified], R-3.6.3 [verified]

`drive list`, `drive add`, `drive remove`, `drive search`. Drive add creates a config section with auto-generated display_name and sync_dir. It accepts canonical drive IDs, shared selectors, raw shared-folder URLs, and shared-folder name search terms; raw URLs and selectors are normalized to canonical shared drive IDs before config writes. Drive list annotates available drives with state DB presence (R-3.3.3), supports `--all` to remove the SharePoint site cap (R-3.3.4). Drive remove `--purge` works on unconfigured drives with orphaned state (R-3.3.8) and only purges drive-owned state; account-owned token/profile files remain until logout.

Shared-folder discovery is best-effort live search via
`GET /me/drive/root/search(q='*')` to satisfy `R-3.6.2`. Search hits are only
actionable when they expose both `remoteDriveID` and `remoteItemID`; hits
without those remote identities are logged and skipped rather than converted
into unstable selectors. For actionable hits, the CLI enriches owner metadata
through `GET /drives/{driveId}/items/{itemId}`. If owner identity still
remains unavailable after enrichment, `drive list` keeps the folder visible
with a deterministic display name
`"{folderName-or-'shared item'} (shared {remoteDriveID}:{remoteItemID})"` and
`shared` emits the item with empty owner fields instead of dropping it.

Search failure for an authenticated account becomes caller-level degraded
discovery (`shared_discovery_unavailable`) rather than a fallback to the
deprecated API. If live search succeeds but Graph still omits an external or
cross-organization share, the CLI reports that as a platform limitation
instead of claiming the share is categorically undiscoverable.

Exact shared selectors do not rediscover themselves through search. `drive add
shared:<recipient>:<driveId>:<itemId>` resolves display naming from the
authoritative `GET /drives/{driveId}/items/{itemId}` response so shared-target
bootstrap stays independent of live discovery quality.

## Design Constraints

- `driveid` stays pure value logic. Runtime drive-ID resolution is config-owned:
  shared drives recover the source drive ID from the canonical shared selector,
  while personal/business/SharePoint drives use drive metadata managed by the
  config layer rather than token-file side metadata.
- SharePoint document libraries have their OWN drive_id, different from the business account's primary drive. Drive metadata files (`drive_*.json`) store the correct per-drive ID.

### Rationale

- **`driveid` is a leaf package** (stdlib only): parsing, construction, formatting. No business logic, no config imports.
- **Token resolution in `config`**, not `driveid`: determining which token file a shared drive uses requires scanning all configured drives — that's business logic.
- **Display names replace aliases**: one field, one purpose. Email is already unique and human-readable for personal/business.
- **Per-drive filter scoping** (DP-8): different drives have fundamentally different content. Global defaults create confusing inheritance.
