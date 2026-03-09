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
