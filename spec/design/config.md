# Configuration

GOVERNS: internal/config/account.go, internal/config/config.go, internal/config/defaults.go, internal/config/discovery.go, internal/config/display_name.go, internal/config/drive.go, internal/config/drivemeta.go, internal/config/env.go, internal/config/holder.go, internal/config/load.go, internal/config/paths.go, internal/config/size.go, internal/config/token_resolution.go, internal/config/toml_lines.go, internal/config/unknown.go, internal/config/validate.go, internal/config/validate_drive.go, internal/config/write.go

Implements: R-4.1 [verified], R-4.2 [verified], R-4.3 [verified], R-4.4 [verified], R-4.8.1 [verified], R-4.8.2 [verified], R-4.8.3 [verified], R-4.8.4 [verified], R-4.8.5 [verified], R-4.8.6 [verified], R-4.9.2 [verified], R-4.9.3 [verified]

## Overview

TOML configuration with flat global settings and per-drive sections. Drive sections are identified by `:` in the section name (e.g., `["personal:user@example.com"]`).

## Override Chain

Four-layer resolution: defaults → config file → environment → CLI flags. CLI flags replace (never merge) config values.

Only two environment variables: `ONEDRIVE_GO_CONFIG` (config path override) and `ONEDRIVE_GO_DRIVE` (drive selection). All other configuration uses the config file or CLI flags.

## File Locations

Platform-specific paths following XDG (Linux) and Application Support (macOS) conventions. Config and data may share the same directory on macOS.

Managed config/account/drive-metadata reads use root-based trusted-path opens once the CLI/config layer has selected the file path. This keeps the path trust boundary explicit without broad `gosec` carve-outs.

## Config File Manipulation

The config file is read with a TOML parser (`BurntSushi/toml`) but written with line-based text edits (`toml_lines.go`). This preserves all comments — both the initial defaults template and user additions. No TOML round-trip serialization.

## Drive Sections

Implements: R-3.4.1 [verified], R-3.4.3 [verified], R-6.2.9 [verified]

Each drive section contains per-drive settings (sync_dir, filters, paused state). Drive resolution (`ResolveDrive`) matches by exact canonical ID → exact display_name (case-insensitive) → substring. Ambiguous matches produce an error with suggestions. `ResolveDrive()` returns both `*ResolvedDrive` and `*Config` — the raw config is needed by shared drive token resolution.

### Pause State

`Drive.IsPaused(now time.Time) bool` is the single source of truth for whether a drive is currently paused. All callers (CLI commands, the multi-drive control plane, drive resolution) use this method instead of checking `Paused`/`PausedUntil` fields directly. Logic: nil/false → not paused; true without expiry → indefinite; true with future expiry → active; true with past expiry → expired (not paused); true with unparseable expiry → indefinite (safe default).

`ResolvedDrive.Paused` is expiry-aware: `buildResolvedDrive` calls `IsPaused(time.Now())`, so `ResolveDrives(includePaused=false)` correctly includes drives with expired timed pauses.

`ClearExpiredPauses(cfgPath, cfg, now, logger)` is config-level housekeeping: removes stale `paused`/`paused_until` keys from config for drives whose timed pause has expired. Updates both in-memory config and on-disk file. Called by the control-plane reload path before resolving drives.

Default sync directories are computed deterministically from the canonical ID + cached metadata using a two-level collision scheme: base name → + display_name → + email. `DefaultSyncDir()` is the single entry point.

## Token Resolution

`TokenCanonicalID()` determines which OAuth token file a drive uses. Shared and SharePoint drives share their account's primary token. Resolution scans configured drives to determine account type — this is business logic in `config`, not identity logic in `driveid`.

## Optional Metadata Lookups

Cached account and drive metadata are optional state, not exceptional control flow. `LookupAccountProfile(cid)` and `LookupDriveMetadata(cid)` both return `(*T, found bool, error)`:

- `found=false, err=nil`: no cached metadata applies to that canonical ID (missing file, wrong drive type, shared drive, or metadata intentionally absent)
- `found=true, err=nil`: valid cached metadata loaded
- `err!=nil`: malformed JSON, I/O failure, or invariant violation

Callers branch on `found` instead of overloading `(nil, nil)` as both “missing” and “success”.

## Validation

Implements: R-4.8.1 [verified], R-4.8.2 [verified], R-4.8.3 [verified]

Unknown config keys are fatal errors (`unknown.go`). Per-drive validation checks sync_dir, filter patterns, size parsing, and drive-specific constraints. Global validation checks log level, transfer workers, and safety thresholds. `checkSyncDirOverlap()` prevents overlapping sync directories using `filepath.Clean` + `strings.HasPrefix` with separator suffix. Called at both config load and control-plane startup.

### Validation Tiers [verified]

Implements: R-4.8.4 [verified]

Two loading paths: strict (`Load`/`LoadOrDefault`) for data commands, lenient (`LoadLenient`/`LoadOrDefaultLenient`) for informational commands.

**Strict path** (default): unknown keys and validation errors are fatal. Used by `sync`, file operations, `drive add`, `drive remove`.

**Lenient path**: TOML syntax errors remain fatal (can't produce Config). Unknown keys, validation errors, and drive section issues (type mismatches, unknown drive keys) are collected as `ConfigWarning` values. Malformed drive sections are skipped — other drives remain usable. Used by `drive list`, `status`, `whoami`.

Internal refactoring supports both paths cleanly: `collectUnknownGlobalKeyErrors`, `collectDriveUnknownKeyErrors`, and `collectValidationErrors` return `[]error` slices. The strict wrappers (`checkUnknownKeys`, `Validate`) join them into a single error. The lenient path converts them to warnings.

**Sync-specific validation** (`ValidateResolvedForSync`): enforces sync_dir is set, absolute, and not a regular file. Called only by the `sync` command — file operations don't require sync_dir.

## Config Holder

`config.Holder` wraps `*Config` + immutable config path behind an `RWMutex`. Both `SessionProvider` and `multisync.OrchestratorConfig` share the same `*Holder` instance. On SIGHUP reload, one `holder.Update(newCfg)` call atomically updates config for all consumers.

## Auto-Creation

`login` creates the config file from a template string with all global settings as commented-out defaults. Drive sections are appended. Subsequent `login` calls add new drive sections without disturbing existing content. `EnsureDriveInConfig` calls `ResolveAccountNames()` (reads account profile only, no token file access). Shared drives bypass `EnsureDriveInConfig` and write config directly via `AppendDriveSection` + `SetDriveKey`.

When no config file exists, `DiscoverTokens()` scans the data dir for `token_*.json` files and extracts canonical IDs from filenames. One token → auto-select. Multiple → prompt with list.

### Rationale

- **Flat structure** (no `[filter]` sub-sections): simpler to read/write/manipulate. Drive sections are the only nesting.
- **Text-level writes**: TOML libraries strip comments on round-trip. Users care about their comments.
- **Per-drive filters only** (no global filter defaults): different drives have fundamentally different content. Global defaults create confusing inheritance.
- **Fatal on unknown keys**: prevents silent misconfiguration from typos.
