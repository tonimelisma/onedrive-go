# R-4 Configuration

TOML configuration, service integration, and interactive setup.

## R-4.1 Config Format [verified]

- R-4.1.1: The system shall use TOML format with drive sections keyed by canonical ID. [verified]
- R-4.1.2: The system shall support XDG paths (Linux: `~/.config/onedrive-go/`, macOS: `~/Library/Application Support/onedrive-go/`). [verified]
- R-4.1.3: The system shall support `--config` flag and `ONEDRIVE_GO_CONFIG` env var overrides. [verified]
- R-4.1.4: `config.toml` shall contain drive sections only. Account saved-login state, account profiles, drive metadata, and retained sync state shall be persisted outside the config file. [verified]

## R-4.2 Config Auto-Creation [verified]

- R-4.2.1: When `login` creates the first drive, the system shall auto-create `config.toml` with all global settings as commented-out defaults. [verified]
- R-4.2.2: Config modifications by CLI commands shall use line-based text edits to preserve comments. [verified]
- R-4.2.3: Removing the final configured drive or logging out the final configured account shall leave `config.toml` in place with zero drive sections. [verified]

## R-4.3 Config Override Chain [verified]

The system shall resolve settings with a four-layer override chain: defaults → config file → environment → CLI flags.

## R-4.4 Hot Reload [verified]

- R-4.4.1: When running `sync --watch`, the system shall reload config on control-socket reload request. [verified]
- R-4.4.2: Drives added, removed, or paused while running shall take effect immediately. [verified]

## R-4.5 Interactive Setup [future]

- R-4.5.1: When the user runs `setup`, the system shall provide menu-driven configuration. [future]

## R-4.6 Service Integration [future]

- R-4.6.1: When the user runs `service install`, the system shall generate a systemd unit (Linux) or launchd plist (macOS). [future]
- R-4.6.2: `service install` shall NOT auto-enable the service. [future]
- R-4.6.3: `service install` shall be idempotent, regenerating the definition if already present. [future]
- R-4.6.4: When the user runs `service enable`, the system shall enable auto-start at boot. [future]
- R-4.6.5: When the user runs `service disable`, the system shall disable auto-start at boot. [future]
- R-4.6.6: When the user runs `service uninstall`, the system shall stop, disable, and remove the service definition. [future]
- R-4.6.7: When the user runs `service status`, the system shall report whether the service is installed, enabled, and running. [future]

## R-4.7 Logging [verified]

- R-4.7.1: The system shall support dual-channel logging: console (stderr, controlled by `--verbose`/`--quiet`/`--debug`) and file (controlled by `log_level`/`log_file`). [verified]
- R-4.7.2: The log file shall use structured JSON format. [verified]
- R-4.7.3: The system shall support configurable log retention (`log_retention_days`). [verified]

## R-4.8 Config Validation [verified]

- R-4.8.1: The system shall reject unknown config keys with "did you mean?"
  suggestions. [verified]
- R-4.8.2: The system shall validate value types, ranges, and formats at config
  load time: numeric ranges (e.g. transfer_workers 4–64), duration minimums
  (e.g. poll_interval >= 30s), enum fields (e.g. log_level, log_format),
  and size formats (e.g. min_free_space). [verified]
- R-4.8.3: The system shall prevent overlapping or duplicate sync directories
  across drives. [verified]
- R-4.8.4: Informational commands (`drive list`, `status`, `whoami`) shall
  tolerate config validation errors, parsing what they can and reporting
  warnings instead of failing. [verified]
- R-4.8.5: File operation commands (`ls`, `get`, `put`, `rm`, `mkdir`, `mv`,
  `cp`, `stat`) shall require only a selectable drive section and a valid
  token. They shall NOT require `sync_dir` or any sync-related settings.
  [verified]
- R-4.8.6: The `sync` command shall require `sync_dir` to be set, absolute,
  and either an existing directory or creatable. All sync-related settings
  (filters, safety thresholds, timing) shall be fully validated before sync
  starts. [verified]

## R-4.9 Config Schema [verified]

- R-4.9.1: All global settings shall be optional with documented defaults. No
  global settings are required for any command to function. [verified]
- R-4.9.2: For file operation commands, a drive section header
  (`["type:email"]`) must exist in config. No fields inside the section are
  required. [verified]
- R-4.9.3: For the `sync` command, each resolved drive must have an effective
  `sync_dir` before sync starts. An explicit per-drive `sync_dir` is optional:
  when omitted, configuration resolution shall derive the deterministic default
  local path for that drive before sync-specific validation runs. All other
  per-drive fields (filters, paused state, display metadata) are optional with
  documented defaults. [verified]
- R-4.9.4: The config design doc shall contain a complete field reference:
  every global and per-drive field, its type, default value, valid range, and
  which command class uses it. [verified]
