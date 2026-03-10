# R-4 Configuration

TOML configuration, service integration, and interactive setup.

## R-4.1 Config Format [verified]

- R-4.1.1: The system shall use TOML format with drive sections keyed by canonical ID. [verified]
- R-4.1.2: The system shall support XDG paths (Linux: `~/.config/onedrive-go/`, macOS: `~/Library/Application Support/onedrive-go/`). [verified]
- R-4.1.3: The system shall support `--config` flag and `ONEDRIVE_GO_CONFIG` env var overrides. [verified]

## R-4.2 Config Auto-Creation [verified]

- R-4.2.1: When `login` creates the first drive, the system shall auto-create `config.toml` with all global settings as commented-out defaults. [verified]
- R-4.2.2: Config modifications by CLI commands shall use line-based text edits to preserve comments. [verified]

## R-4.3 Config Override Chain [verified]

The system shall resolve settings with a four-layer override chain: defaults → config file → environment → CLI flags.

## R-4.4 Hot Reload [verified]

- R-4.4.1: When running `sync --watch`, the system shall reload config on SIGHUP. [verified]
- R-4.4.2: Drives added, removed, or paused while running shall take effect immediately. [verified]

## R-4.5 Interactive Setup [future]

- R-4.5.1: When the user runs `setup`, the system shall provide menu-driven configuration. [future]

## R-4.6 Service Integration [future]

- R-4.6.1: When the user runs `service install`, the system shall generate a systemd unit (Linux) or launchd plist (macOS). [future]
- R-4.6.2: `service install` shall NOT auto-enable the service. [future]

## R-4.7 Logging [verified]

- R-4.7.1: The system shall support dual-channel logging: console (stderr, controlled by `--verbose`/`--quiet`/`--debug`) and file (controlled by `log_level`/`log_file`). [verified]
- R-4.7.2: The log file shall use structured JSON format. [verified]
- R-4.7.3: The system shall support configurable log retention (`log_retention_days`). [verified]

## R-4.8 Config Validation [implemented]

- R-4.8.1: The system shall reject unknown config keys with "did you mean?"
  suggestions. [verified]
- R-4.8.2: The system shall validate value types, ranges, and formats at config
  load time: numeric ranges (e.g. transfer_workers 4–64), duration minimums
  (e.g. poll_interval >= 5m), enum fields (e.g. log_level, conflict_strategy),
  and size formats (e.g. chunk_size, min_free_space). [verified]
- R-4.8.3: The system shall prevent overlapping or duplicate sync directories
  across drives. [verified]
- R-4.8.4: Informational commands (`drive list`, `status`, `whoami`) shall
  tolerate config validation errors, parsing what they can and reporting
  warnings instead of failing. [implemented]
- R-4.8.5: File operation commands (`ls`, `get`, `put`, `rm`, `mkdir`, `mv`,
  `cp`, `stat`) shall require only a selectable drive section and a valid
  token. They shall NOT require `sync_dir` or any sync-related settings.
  [planned]
- R-4.8.6: The `sync` command shall require `sync_dir` to be set, absolute,
  and either an existing directory or creatable. All sync-related settings
  (filters, safety thresholds, timing) shall be fully validated before sync
  starts. [implemented]

## R-4.9 Config Schema [planned]

- R-4.9.1: All global settings shall be optional with documented defaults. No
  global settings are required for any command to function. [verified]
- R-4.9.2: For file operation commands, a drive section header
  (`["type:email"]`) must exist in config. No fields inside the section are
  required. [planned]
- R-4.9.3: For the `sync` command, each drive section must contain `sync_dir`
  set to a valid local path. This is the only required per-drive config field.
  All other per-drive fields (filters, poll_interval, paused) are optional with
  documented defaults. [planned]
- R-4.9.4: The config design doc shall contain a complete field reference:
  every global and per-drive field, its type, default value, valid range, and
  which command class uses it. [planned]
