# Configuration

GOVERNS: internal/config/account_owner.go, internal/config/catalog.go, internal/config/catalog_lifecycle.go, internal/config/config.go, internal/config/decoder.go, internal/config/defaults.go, internal/config/discovery.go, internal/config/display_name.go, internal/config/drive.go, internal/config/email_reconcile.go, internal/config/env.go, internal/config/failure_class.go, internal/config/holder.go, internal/config/load.go, internal/config/managed_io.go, internal/config/mount_state_path.go, internal/config/paths.go, internal/config/resolved_validator.go, internal/config/resolver.go, internal/config/size.go, internal/config/token_resolution.go, internal/config/toml_lines.go, internal/config/unknown.go, internal/config/validate.go, internal/config/validate_drive.go, internal/config/validated_state.go, internal/config/validator.go, internal/config/write.go

Implements: R-3.7 [verified], R-4.1 [verified], R-4.2 [verified], R-4.3 [verified], R-4.4 [verified], R-4.8.1 [verified], R-4.8.2 [verified], R-4.8.3 [verified], R-4.8.4 [verified], R-4.8.5 [verified], R-4.8.6 [verified], R-4.9.1 [verified], R-4.9.2 [verified], R-4.9.3 [verified], R-4.9.4 [verified], R-6.3.4 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Overview

TOML configuration with flat global settings and per-drive sections. Drive sections are identified by `:` in the section name (e.g., `["personal:user@example.com"]`).

## Ownership Contract

- Owns: Config-file schema, catalog schema, durable lifecycle mutation rules for config/catalog-managed inventory, child state DB path derivation, override resolution, path discovery, drive-section resolution, token-path resolution, and validation policy.
- Does Not Own: OAuth exchange, Graph request behavior, sync runtime orchestration, or durable sync-store contents.
- Source of Truth: The loaded `Config` snapshot plus derived `ResolvedDrive` values at the config/CLI selection boundary. `Holder` is the single in-process source of truth for reloadable config state; sync orchestration receives `multisync.StandaloneMountConfig` values compiled at the CLI edge, and runtime session/engine construction consume mount-owned identity compiled above their boundaries.
- Allowed Side Effects: Reading and writing config and managed inventory/state files through `fsroot`, plus statting arbitrary user-selected local paths through `localpath`.
- Mutable Runtime Owner: `config.Holder` owns the current `*Config` pointer behind `Holder.mu`. The package starts no background goroutines, owns no long-lived channels, and uses no timers.
- Error Boundary: `config` translates parse/load/validation outcomes into either fatal errors or warnings before callers act, following [error-model.md](error-model.md).

## Verified By

| Behavior | Evidence |
| --- | --- |
| Drive resolution applies pause semantics consistently, including expired timed pauses. | `TestResolveDrives_ExcludesPausedByDefault`, `TestResolveDrives_IncludePausedWhenRequested`, `TestClearExpiredPauses_ClearsExpired` |
| `buildResolvedDrive` owns defaulting and per-drive override materialization for sync callers. | `TestBuildResolvedDrive_GlobalDefaults`, `TestBuildResolvedDrive_NoPerDriveOverridesBeyondDriveFields`, `TestBuildResolvedDrive_TimedPauseExpired` |
| Standalone shared-folder drives always preserve the canonical remote root item even when the backing drive ID comes from the catalog. | `TestBuildResolvedDrive_SharedCanonicalSetsRootItem`, `TestBuildResolvedDrive_SharedCatalogDrivePreservesRootItem` |
| Mount-root delta capability is resolved in config from shared-drive ownership facts before sync engine construction. | `TestBuildResolvedDrive_SharedBusinessOwnerDisablesFolderDelta`, `TestBuildResolvedDrive_SharedUnknownOwnerDefaultsFolderDeltaCapable`, `TestStandaloneMountSelectionFromResolvedDrives_PreservesMountBoundaryFields` |
| Managed shortcut children keep stable child mount IDs and retained state DB paths without becoming synthetic configured drives. Parent-owned alias lifecycle state lives in the parent sync store, while multisync owns child-artifact purge using config path/catalog primitives only. | `TestMountStatePath_UsesManagedMountPrefix`, `TestRunOnce_ParentCleanupRequestPurgesShortcutChildStateArtifacts`, `TestPurgeShortcutChildArtifacts_IgnoresExplicitMountID`, `internal/multisync/shortcut_topology_test.go`, `internal/sync/shortcut_root_state_test.go` |
| Token-owner resolution stays config-owned for shared and business-derived drives. | `TestDriveTokenPath_Shared_WithCatalogDrive`, `TestTokenAccountCID_Shared`, `TestTokenAccountCID_SharePoint` |
| Control-socket path derivation keeps the socket under the data dir when possible, falls back to a stable hashed runtime dir when necessary, and fails explicitly when neither path can satisfy the Unix socket length budget. | `TestControlSocketPath_UsesDataDirWhenShortEnough`, `TestControlSocketPath_UsesShortRuntimePathWhenDataDirIsTooLong`, `TestControlSocketPath_ReturnsErrorWhenFallbackStillExceedsLimit` |
| Child mount state DB path derivation is stable, collision-resistant, and bounded by common basename limits. | `TestMountStatePath_UsesManagedMountPrefix`, `TestMountStatePath_EncodesManagedMountIDWithoutCollisions`, `TestMountStatePath_LongIDUsesBoundedFilename` |

Transfer validation behavior is not user-disableable. The config surface intentionally has no `disable_download_validation` or `disable_upload_validation` escape hatches; transfer correctness policy lives in the transfer and observation layers, not in mutable config toggles.

## Override Chain

Four-layer resolution: defaults → config file → environment → CLI flags. CLI flags replace (never merge) config values.

Only two environment variables: `ONEDRIVE_GO_CONFIG` (config path override) and `ONEDRIVE_GO_DRIVE` (drive selection). All other configuration uses the config file or CLI flags.

## File Locations

Platform-specific paths following XDG (Linux) and Application Support (macOS) conventions. Config and data may share the same directory on macOS.

`ControlSocketPath()` normally places the Unix control socket under
`DefaultDataDir()`. Unix socket path length is platform-bounded, so when the
full data-dir socket path is too long the config layer derives a stable,
hash-named runtime directory under `os.TempDir()` and places only the socket
there. Durable state, tokens, logs, and catalog/state artifacts remain under the normal
config/data/cache roots.

`ControlSocketPath()` returns `(path, error)`, not an empty-string sentinel.
The config layer revalidates the hashed runtime fallback against the same Unix
socket soft limit as the primary path. If neither path fits, it returns an
explicit error describing both candidates. Callers then decide policy at their
own boundary: sync-owner startup treats the error as fatal because the socket
is the single-owner lock, while some CLI mutation/reload flows can intentionally
degrade when the derivation proves no daemon can exist.

`config` uses two filesystem boundaries, not ad hoc `os.*` calls:

- `fsroot` for repo-managed state selected by the config layer: config files, catalog, tokens, and state DB path discovery
- `localpath` for arbitrary local paths supplied by the user or derived from config semantics: `sync_dir` existence/type validation

Config entrypoints still accept full path strings, so `managed_io.go` establishes the managed root per call via `fsroot.OpenPath` or `fsroot.Open`. This keeps the path trust boundary explicit without broad `gosec` carve-outs.

## Persisted Facts And Lifecycle

`config.toml` persists drive sections only. It never contains account sections.
Durable account, drive, and sync-state facts are split across managed files:

- `config.toml`: configured drive sections keyed by canonical drive ID
- `catalog.json`: durable account inventory, drive inventory, profile cache, ownership, and account-level auth requirement
- `token_*.json`: saved login owned by the account
- `state_*.db`: retained configured-standalone-drive sync state
- `state_mount_*.db`: retained managed-child-mount sync state

`config` owns both the persistence mechanics and the durable lifecycle rules
that coordinate config/catalog/token path artifacts. CLI commands may still own
Graph/OAuth flows, token deletion, and user interaction, but they do not
hand-edit `CatalogAccount` or `CatalogDrive` records directly for product
behavior. Automatic shortcut lifecycle is not a config-owned durable fact: the
parent engine stores parent shortcut-root lifecycle in its sync store and
multisync compiles child runners from ephemeral parent-declared runner-action
publications.

The config file itself is durable even when it has zero drive sections. Removing
the final configured drive or logging out the final configured account leaves
`config.toml` in place with only the global template/comments that remain after
text-level edits.

### Derived Account States

CLI account surfaces derive one lifecycle state from the durable facts above:

| State | Durable facts |
| --- | --- |
| `logged_in_with_configured_drives` | usable saved login + at least one configured drive |
| `logged_in_without_configured_drives` | usable saved login + zero configured drives |
| `auth_required_missing_login` | known account metadata/state remains but no usable saved login exists |
| `auth_required_invalid_saved_login` | token file exists but the saved login is invalid or unreadable |
| `auth_required_sync_rejected` | usable saved login plus a persisted catalog auth requirement |
| `absent` | no durable account facts remain |

### Derived Drive Inventory States

Drive surfaces derive inventory state per canonical drive ID. Authentication
remains account-scoped; drive inventory is separate from whether its owning
account currently has a usable saved login.

| State | Durable facts |
| --- | --- |
| `configured` | drive section exists in `config.toml` |
| `retained_state` | no drive section, but a retained state DB still exists |
| `known_catalog_only` | the catalog knows the drive, but no config section or retained state DB exists |
| `absent` | no durable drive fact exists |

### Transition Summary

| Operation | Durable mutation |
| --- | --- |
| `login` | write saved login, upsert account/primary-drive catalog records, append primary drive section |
| `drive add` | register the drive in the catalog and append one drive section |
| `drive remove` | delete one drive section only |
| `drive remove --purge` | delete one drive section plus drive-owned retained state; prune the catalog drive record when no longer needed |
| `logout` | delete saved login and all configured drive sections for the selected account |
| `logout --purge` | `logout` durable mutations plus delete retained state DBs and purge the selected account/drives from the catalog, using best-effort recovery when validated state cannot load |
| external token deletion | removes saved login only; catalog/state may still keep the account known |
| invalid/unreadable token file | keeps the saved-login file path but moves the derived account state to `auth_required_invalid_saved_login` |
| persisted catalog auth requirement creation | keeps other durable facts intact but moves the derived account state to `auth_required_sync_rejected` |
| successful authenticated proof | clears the persisted catalog auth requirement and returns the derived account state to the matching logged-in state |

Degraded discovery is intentionally outside this lifecycle model. It is a
command-local discovery overlay, not a persisted account or drive state.

Shared-drive add is inventory-first. The catalog admits the drive before the
config section is appended, and later config-write failures must roll that
catalog admission back so config/catalog ownership never diverges across a
partial write.

`logout --purge` is intentionally recovery-oriented. When validated config and
catalog state cannot be loaded together, callers may still fall back to the
best recoverable config, token, and catalog inputs and perform durable cleanup
against those facts instead of hard-failing before any purge happens.

## Internal Organization

`config` stays one package, but the responsibilities are separated internally so each layer has one job:

- `load.go`: public config-loading entrypoints and the `configLoader` coordinator
- `decoder.go`: second-pass drive-section decoding for strict and lenient loads
- `failure_class.go`: shared-domain classification for config load outcomes
- `resolver.go`: config-path selection plus single-selection and multi-selection resolution
- `validator.go`: whole-config validation orchestration
- `resolved_validator.go`: post-override resolved-drive validation against the local filesystem
- `validate.go` and `validate_drive.go`: field- and drive-specific validation rules

The public API stays stable (`Load*`, `Resolve*`, `Validate*`), but those entrypoints delegate into these internal collaborators instead of re-mixing decode, resolution, and validation logic in one file.

## Field Reference

This table is the authoritative config-package view of the current schema.
`Command class` names the current consumer boundary at a coarse level:
`sync`, `sync --watch`, `transfer commands`, `all CLI`, or `display`.

### Global Fields

| Key | Type | Default / effective default | Valid range / shape | Command class | Notes |
| --- | --- | --- | --- | --- | --- |
| `transfer_workers` | `int` | `8` | `4..64` | `sync`, `transfer commands` | Shared transfer worker-pool size. |
| `check_workers` | `int` | `4` | `1..16` | `sync` | Parallel local hashing worker count. |
| `min_free_space` | `string` | `1GB` | parseable size string; `0` disables | `sync`, `get`, shared download commands | Disk reservation floor for downloads. |
| `poll_interval` | `string` | `5m` | duration `>= 30s` | `sync --watch` | Remote observation fallback poll cadence. |
| `websocket` | `bool` | `false` | boolean | `sync --watch` | Enables Socket.IO remote wakeups where supported. |
| `dry_run` | `bool` | `false` | boolean | `sync` | Config-owned default for dry-run sync. CLI flag may override. |
| `log_level` | `string` | `info` | `debug`, `info`, `warn`, `error` | `all CLI` | File-log verbosity. |
| `log_file` | `string` | `""` (platform default location) | path string | `all CLI` | Empty means standard managed log path. |
| `log_format` | `string` | `auto` | `auto`, `json`, `text` | `all CLI` | File-log format. |
| `log_retention_days` | `int` | `30` | `>= 1` | `all CLI` | Log rotation retention window. |

### Per-Drive Fields

| Key | Type | Default / effective default | Valid range / shape | Command class | Notes |
| --- | --- | --- | --- | --- | --- |
| `sync_dir` | `string` | computed by `DefaultSyncDir()` when omitted | absolute or `~/...`; must be existing directory or creatable for `sync` | `sync` | File-operation commands require only the section, not this field. |
| `paused` | `bool?` | unset / inherited as not paused | boolean when present | `sync`, `pause`, `resume`, `status`, `drive list` | `Drive.IsPaused()` is the single source of truth. |
| `paused_until` | `string?` | empty | RFC3339 timestamp when present | `sync`, `pause`, `resume`, `status`, `drive list` | Timed pause expiry owned by config resolution. |
| `display_name` | `string` | derived by `DefaultDisplayName()` when omitted | string | `display`, selector matching | Human-facing label for status and drive selection. |
| `owner` | `string` | empty | string | `display` | Shared-drive owner label. |

## Config File Manipulation

The config file is read with a TOML parser (`BurntSushi/toml`) but written with line-based text edits (`toml_lines.go`). This preserves all comments — both the initial defaults template and user additions. No TOML round-trip serialization.

The first-login template is generated from the same default constants the
runtime uses, so commented-out defaults stay aligned with the real schema.

All authoritative config/account/drive-metadata writes go through
`fsroot.Root.AtomicWrite`. The config package does not leave temp-file and
rename choreography to callers.

Section-header rewrites are also config-owned. `RenameDriveSections()` edits
only the quoted section name, leaving comments, key order, and section body
text intact. Email reconciliation uses this instead of re-encoding TOML.

## Drive Sections

Implements: R-3.4.1 [verified], R-3.4.3 [verified]

Each drive section contains per-drive settings (`sync_dir`, paused state, and
display metadata). When a drive section omits `sync_dir`,
`buildResolvedDrive` computes the deterministic default local path for that
canonical ID before command-specific validation runs. Drive resolution
(`ResolveDrive`) matches by exact canonical ID → exact display_name
(case-insensitive) → substring. Ambiguous matches produce an error with
suggestions. `ResolveDrive()` returns both `*ResolvedDrive` and `*Config` —
the raw config is needed by shared drive token resolution.

For `shared:email:sourceDriveID:sourceItemID` drives, `buildResolvedDrive`
always preserves `RemoteRootItemID` from the canonical ID even when `DriveID` is
resolved from a catalog drive record. Catalog-backed shared drives therefore
keep the configured mount-root observation boundary instead of silently
falling back to whole-drive observation.

`buildResolvedDrive` also resolves `ResolvedDrive.RemoteRootDeltaCapable`.
That boolean is derived once from token-owner identity facts through the shared
helper `RemoteRootDeltaCapableForTokenOwner(...)`:

- personal owner account -> folder delta capable
- business/sharepoint owner account -> enumerate only
- unknown or malformed owner metadata -> default to capable and let runtime
  fallback prove otherwise

The sync engine consumes that resolved boolean directly for configured
standalone drives, while managed child mounts derive the same capability from
their explicit token-owner identity. Runtime startup does not reload catalog
metadata just to rediscover mount-root delta support.

`websocket` is a live watch-mode control. When `websocket = true`, watch mode
fetches a OneDrive Socket.IO endpoint and establishes an outbound websocket
wake source. The wake source does not replace delta: it only interrupts the
normal wait so the remote observer runs delta sooner. `poll_interval` remains
the fallback poll cadence even when websocket is enabled.

### Pause State

`Drive.IsPaused(now time.Time) bool` is the single source of truth for whether a drive is currently paused. All callers (CLI commands, the multi-drive control plane, drive resolution) use this method instead of checking `Paused`/`PausedUntil` fields directly. Logic: nil/false → not paused; true without expiry → indefinite; true with future expiry → active; true with past expiry → expired (not paused); true with unparseable expiry → indefinite (safe default).

`ResolvedDrive.Paused` is expiry-aware: `buildResolvedDrive` calls `IsPaused(time.Now())`, so `ResolveDrives(includePaused=false)` correctly includes drives with expired timed pauses.

`ClearExpiredPauses(cfgPath, cfg, now, logger)` is config-level housekeeping: removes stale `paused`/`paused_until` keys from config for drives whose timed pause has expired. Updates both in-memory config and on-disk file. Called by the control-plane reload path before resolving drives.

Default sync directories are computed deterministically from the canonical ID + cached metadata using a two-level collision scheme: base name → + display_name → + email. `DefaultSyncDir()` is the single entry point.

## Token Resolution

`DriveTokenPath()` determines which OAuth token file a drive uses. Shared and SharePoint drives share their account's primary token. Resolution uses catalog ownership for shared drives and canonical account type rules for the other drive types — this is business logic in `config`, not identity logic in `driveid`.

`TokenAccountCanonicalID()` is the authoritative token-owner lookup for
runtime callers that need the owning account identity itself instead of only
the token file path. SharePoint resolves to the business account with the same
email. Shared drives resolve through catalog ownership, not by scanning config
or legacy metadata files.

## Email Reconciliation

Implements: R-3.7.1 [verified], R-3.7.2 [verified]

`config.ReconcileAccountEmail()` owns durable email-change mutation. Given the
current authenticated account type, stable Graph user GUID, and current Graph
email, it:

- finds stored personal/business catalog accounts with the same `UserID`
- computes exact old → new canonical-ID rewrites
- renames owned managed files (`token_*`, `state_*`)
- rewrites catalog account IDs and owned drive ownership/canonical IDs
- rewrites config section headers in place through `RenameDriveSections()`

Ownership stays in `config` because the package already owns canonical-ID to
path mapping, token-owner resolution, and text-level config mutation.

The reconciliation planner is conservative about authority:

- personal/business ownership comes from the canonical-ID type
- SharePoint ownership comes from token-owner resolution to the business account
- shared-drive ownership comes from the managed catalog, not by guessing

Before any file mutation, the planner validates that every target path is free.
Existing target paths are treated as collisions rather than being overwritten,
so a partial or conflicting rename fails loudly instead of silently merging two
authorities.

### Catalog JSON Policy

`catalog.json` is the durable authority for accounts and drives, so decode is
strict:

- unknown JSON fields are rejected
- `schema_version` is required
- only the exact supported version is accepted
- the decoder must reach EOF after the first top-level object, so trailing JSON
  or garbage is rejected
- every save is a full atomic rewrite of the file

Runtime callers do not silently accept future or partially shaped catalog
files. A malformed or unsupported catalog is treated as an actionable local
state error that must be repaired explicitly.

`MountStatePath(mountID)` gives each managed child mount a stable retained-state
DB path independent of configured drive canonical IDs. Configured standalone
drives still use `DriveStatePath(...)`; managed child mounts now own their own
durable store namespace instead of pretending to be synthetic configured drives.
The filename uses a fixed-length SHA-256 digest of the managed mount ID encoded
with raw URL base64 under the `state_mount_*.db` prefix. That keeps every
managed child state filename below common filesystem basename limits while
preserving collision-resistant separation for IDs that differ only by path or
punctuation characters.

Automatic child mount IDs are stable across placeholder rename or move because
they are derived from `(NamespaceID, BindingItemID)`, not from the local
projection path or the content-root identity. A shortcut can therefore move
inside the parent namespace without losing its retained child mount state DB.
Parent engines own the authoritative parent protected-path state in
`shortcut_roots`; config only derives the child state DB path from the stable
mount ID.

When multisync releases a managed shortcut child, config provides only the
stable path and catalog mutation primitives. The control plane owns deleting
child-owned artifacts: the `state_mount_*.db` SQLite file family, upload
sessions tagged with the child mount scope, and any accidental catalog drive
record keyed by the automatic child mount ID. This purge is guarded by the
child-mount ID shape (`parent|binding:<id>`) so an explicit user-configured
shared-drive catalog entry or parent drive state is not removed by shortcut
lifecycle cleanup.

## Optional Catalog Fields

Catalog-backed account and drive records may omit cached presentation fields
such as display name, org name, remote drive ID, or shared-owner identity.
Missing optional catalog fields are normal input, not exceptional control flow.

Callers branch on the presence of the specific catalog field they need instead
of treating absent optional state as a load failure.

## Validation

Implements: R-4.8.1 [verified], R-4.8.2 [verified], R-4.8.3 [verified]

Unknown config keys are fatal errors (`unknown.go`). Per-drive validation checks sync_dir, filter patterns, size parsing, and drive-specific constraints. Global validation checks log level, transfer workers, and safety thresholds. `checkSyncDirOverlap()` prevents overlapping sync directories using `filepath.Clean` + `strings.HasPrefix` with separator suffix. Called at both config load and control-plane startup.

`ValidateResolved` and `ValidateResolvedForSync` treat only `os.ErrNotExist` as an acceptable sync-dir stat result. If the local path is unreadable or the filesystem returns another error, validation fails instead of silently accepting a broken path.

### Validation Tiers [verified]

Implements: R-4.8.4 [verified]

Two loading paths: strict (`Load`/`LoadOrDefault`) for data commands, lenient (`LoadLenient`/`LoadOrDefaultLenient`) for informational commands.

**Strict path** (default): unknown keys and validation errors are fatal. Used by `sync`, file operations, `drive add`, `drive remove`.

**Lenient path**: TOML syntax errors remain fatal (can't produce Config). Unknown keys, validation errors, and drive section issues (type mismatches, unknown drive keys) are collected as `ConfigWarning` values. Malformed drive sections are skipped — other drives remain usable. Used by `drive list`, `status`, `shared`.

Internal refactoring supports both paths cleanly: `collectUnknownGlobalKeyErrors`, `collectDriveUnknownKeyErrors`, and `collectValidationErrors` return `[]error` slices. The strict wrappers (`checkUnknownKeys`, `Validate`) join them into a single error. The lenient path converts them to warnings.

Strict and lenient loading also share one loader/decoder pipeline: `configLoader` owns managed-file reads and base TOML decode, while `driveSectionDecoder` owns the second pass that extracts and validates drive tables. That keeps strict and lenient behavior aligned instead of maintaining two separate decode implementations.

`ClassifyLoadOutcome(err, warnings)` is the single config-owned translation
step into the shared failure model: fatal read/parse errors map to `fatal`,
successful loads with warnings map to `actionable`, and clean loads map to
`success`.

**Sync-specific validation** (`ValidateResolvedForSync`): enforces that the resolved `sync_dir` is set, absolute, and not a regular file. That validation happens after `buildResolvedDrive` has already applied either the explicit per-drive `sync_dir` or the deterministic runtime default for drives that omit it. Non-existent paths are allowed because sync creates them on first run; other stat failures are fatal. The CLI then materializes the validated directory and compiles selected `ResolvedDrive` values into `multisync.StandaloneMountConfig` before launching one-shot or watch sync. Called only by the `sync` command — file operations do not require an explicit `sync_dir` in config.

## Config Holder

`config.Holder` wraps `*Config` + immutable config path behind an `RWMutex`. Both `SessionRuntime` and `multisync.OrchestratorConfig` share the same `*Holder` instance. On control-socket reload, multisync reloads the config file, uses the CLI-supplied standalone-mount compiler, then one `holder.Update(newCfg)` call atomically updates config for all consumers.

## Runtime Ownership

`config` has one long-lived mutable runtime structure: `Holder`.

- `Holder.mu` guards exactly one field: the current `*Config` snapshot.
- `Holder.path` is immutable after construction and intentionally read without locking.
- Reload callers own goroutine lifetime outside the package. `config` exposes synchronous load/update operations only.
- The package owns no channels or timers. Control-socket request handling lives in the control plane; `config` only provides the atomic snapshot swap.

## Auto-Creation

`login` creates the config file from a template string with all global settings as commented-out defaults. Drive sections are appended. Subsequent `login` calls add new drive sections without disturbing existing content. `EnsureDriveInConfig` calls `ResolveAccountNames()` (reads catalog-backed account records only, no token file access). Shared drives bypass `EnsureDriveInConfig` and write config directly via `AppendDriveSection` + `SetDriveKey`.

Removing a drive or logging out an account deletes the relevant drive sections
through `DeleteDriveSection()`, but it does not delete `config.toml` itself.

When no config file exists, `DiscoverTokens()` scans the data dir for `token_*.json` files and extracts canonical IDs from filenames. One token → auto-select. Multiple → prompt with list.

### Rationale

- **Flat structure** (no `[filter]` sub-sections): simpler to read/write/manipulate. Drive sections are the only nesting.
- **Text-level writes**: TOML libraries strip comments on round-trip. Users care about their comments.
- **Global defaults with per-drive overrides**: shared defaults keep common policy centralized, while drive sections can still diverge when a specific library or account needs different exclusions.
- **Fatal on unknown keys**: prevents silent misconfiguration from typos.
