# Configuration

GOVERNS: internal/config/account.go, internal/config/config.go, internal/config/defaults.go, internal/config/display_name.go, internal/config/drive.go, internal/config/drivemeta.go, internal/config/env.go, internal/config/holder.go, internal/config/load.go, internal/config/paths.go, internal/config/size.go, internal/config/token_resolution.go, internal/config/toml_lines.go, internal/config/unknown.go, internal/config/validate.go, internal/config/validate_drive.go, internal/config/write.go

Implements: R-4.1 [implemented], R-4.2 [implemented], R-4.3 [implemented], R-4.4 [implemented]

## Overview

TOML configuration with flat global settings and per-drive sections. Drive sections are identified by `:` in the section name (e.g., `["personal:user@example.com"]`).

## Override Chain

Four-layer resolution: defaults → config file → environment → CLI flags. CLI flags replace (never merge) config values.

Only two environment variables: `ONEDRIVE_GO_CONFIG` (config path override) and `ONEDRIVE_GO_DRIVE` (drive selection). All other configuration uses the config file or CLI flags.

## File Locations

Platform-specific paths following XDG (Linux) and Application Support (macOS) conventions. Config and data may share the same directory on macOS.

## Config File Manipulation

The config file is read with a TOML parser (`BurntSushi/toml`) but written with line-based text edits (`toml_lines.go`). This preserves all comments — both the initial defaults template and user additions. No TOML round-trip serialization.

## Drive Sections

Implements: R-3.4.1 [implemented], R-3.4.3 [implemented], R-6.2.9 [implemented]

Each drive section contains per-drive settings (sync_dir, filters, paused state). Drive resolution (`ResolveDrive`) matches by exact canonical ID → exact display_name (case-insensitive) → substring. Ambiguous matches produce an error with suggestions. `ResolveDrive()` returns both `*ResolvedDrive` and `*Config` — the raw config is needed by shared drive token resolution.

Default sync directories are computed deterministically from the canonical ID + cached metadata using a two-level collision scheme: base name → + display_name → + email. `DefaultSyncDir()` is the single entry point.

## Token Resolution

`TokenCanonicalID()` determines which OAuth token file a drive uses. Shared and SharePoint drives share their account's primary token. Resolution scans configured drives to determine account type — this is business logic in `config`, not identity logic in `driveid`.

## Validation

Unknown config keys are fatal errors (`unknown.go`). Per-drive validation checks sync_dir, filter patterns, size parsing, and drive-specific constraints. Global validation checks log level, transfer workers, and safety thresholds. `checkSyncDirOverlap()` prevents overlapping sync directories using `filepath.Clean` + `strings.HasPrefix` with separator suffix. Called at both config load and Orchestrator start.

## Config Holder

`config.Holder` wraps `*Config` + immutable config path behind an `RWMutex`. Both `SessionProvider` and `OrchestratorConfig` share the same `*Holder` instance. On SIGHUP reload, one `holder.Update(newCfg)` call atomically updates config for all consumers.

## Auto-Creation

`login` creates the config file from a template string with all global settings as commented-out defaults. Drive sections are appended. Subsequent `login` calls add new drive sections without disturbing existing content. `EnsureDriveInConfig` calls `ResolveAccountNames()` (reads account profile only, no token file access). Shared drives bypass `EnsureDriveInConfig` and write config directly via `AppendDriveSection` + `SetDriveKey`.

When no config file exists, `DiscoverTokens()` scans the data dir for `token_*.json` files and extracts canonical IDs from filenames. One token → auto-select. Multiple → prompt with list.

### Rationale

- **Flat structure** (no `[filter]` sub-sections): simpler to read/write/manipulate. Drive sections are the only nesting.
- **Text-level writes**: TOML libraries strip comments on round-trip. Users care about their comments.
- **Per-drive filters only** (no global filter defaults): different drives have fundamentally different content. Global defaults create confusing inheritance.
- **Fatal on unknown keys**: prevents silent misconfiguration from typos.
