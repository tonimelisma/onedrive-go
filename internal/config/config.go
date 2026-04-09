// Package config implements TOML configuration loading, validation, and
// platform-specific path resolution for onedrive-go. It supports a four-layer
// override chain (defaults -> config file -> environment -> CLI flags) with
// per-drive overrides that selectively replace global values.
//
// The config file uses flat TOML keys for global settings and quoted section
// names containing ":" for drive configuration (e.g. ["personal:user@example.com"]).
package config

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Config is the top-level configuration structure. Global settings live in
// embedded sub-structs, which BurntSushi/toml promotes to flat TOML keys.
// Drive sections use quoted headers containing ":" and are parsed in a
// separate decode pass. Map keys are validated CanonicalIDs — invalid keys
// are rejected at parse time.
type Config struct {
	FilterConfig
	TransfersConfig
	SafetyConfig
	SyncConfig
	LoggingConfig
	Drives map[driveid.CanonicalID]Drive `toml:"-"` // parsed via two-pass decode, keyed by canonical ID
}

// FilterConfig controls which files and directories are included in sync.
// These patterns are matched against the relative path within the sync directory.
type FilterConfig struct {
	SkipFiles    []string `toml:"skip_files"`
	SkipDirs     []string `toml:"skip_dirs"`
	SkipDotfiles bool     `toml:"skip_dotfiles"`
	SkipSymlinks bool     `toml:"skip_symlinks"`

	// SyncPaths narrows sync to selected absolute drive-root paths. Scope is
	// enforced bidirectionally by local observation, remote observation, and
	// persisted sync-state filtering.
	SyncPaths []string `toml:"sync_paths"`

	// IgnoreMarker excludes any directory containing this filename from sync.
	// The marker is a presence-only check; contents are ignored and the marker
	// file itself is never synced.
	IgnoreMarker string `toml:"ignore_marker"`
}

// TransfersConfig controls live worker counts. Upload fragment sizing is a
// fixed graph-boundary constant rather than user configuration, so the config
// surface owns only concurrency here.
//
// transfer_workers controls the number of concurrent upload/download goroutines.
// check_workers controls the number of concurrent file hashing goroutines.
type TransfersConfig struct {
	TransferWorkers int `toml:"transfer_workers"`
	CheckWorkers    int `toml:"check_workers"`
}

// SafetyConfig controls protective defaults and thresholds that prevent
// accidental data loss during sync operations.
type SafetyConfig struct {
	BigDeleteThreshold int    `toml:"big_delete_threshold"`
	MinFreeSpace       string `toml:"min_free_space"`
	UseLocalTrash      bool   `toml:"use_local_trash"`
}

// SyncConfig controls live sync behavior: remote observation, periodic safety
// scans, and one-shot dry-run execution.
type SyncConfig struct {
	PollInterval       string `toml:"poll_interval"`
	Websocket          bool   `toml:"websocket"`
	DryRun             bool   `toml:"dry_run"`
	SafetyScanInterval string `toml:"safety_scan_interval"`
}

// LoggingConfig controls log output behavior: level, format, and rotation.
type LoggingConfig struct {
	LogLevel         string `toml:"log_level"`
	LogFile          string `toml:"log_file"`
	LogFormat        string `toml:"log_format"`
	LogRetentionDays int    `toml:"log_retention_days"`
}

// Drive represents a single synced drive in the config file.
// Drive sections are keyed by canonical IDs like "personal:user@example.com".
// Per-drive fields override global settings when set (pointer fields distinguish
// "not specified" from "set to zero value").
type Drive struct {
	SyncDir      string   `toml:"sync_dir"`
	Paused       *bool    `toml:"paused,omitempty"`
	PausedUntil  *string  `toml:"paused_until,omitempty"`
	DisplayName  string   `toml:"display_name,omitempty"`
	Owner        string   `toml:"owner,omitempty"` // drive owner name; for shared drives: "{Owner}'s {FolderName}"
	SkipDotfiles *bool    `toml:"skip_dotfiles,omitempty"`
	SkipDirs     []string `toml:"skip_dirs,omitempty"`
	SkipFiles    []string `toml:"skip_files,omitempty"`
	SyncPaths    []string `toml:"sync_paths,omitempty"`
	IgnoreMarker string   `toml:"ignore_marker,omitempty"`
}

// IsPaused returns whether this drive is currently paused. This is the single
// source of truth for pause state — all callers should use this instead of
// checking Paused/PausedUntil fields directly.
//
// Logic:
//   - Paused nil or false → not paused
//   - Paused true, no PausedUntil → indefinitely paused
//   - Paused true, PausedUntil in the future → timed pause still active
//   - Paused true, PausedUntil in the past → timed pause expired → not paused
//   - Paused true, PausedUntil unparseable → treated as indefinite (safe default)
func (d *Drive) IsPaused(now time.Time) bool {
	if d.Paused == nil || !*d.Paused {
		return false
	}

	// No expiry timestamp → indefinite pause.
	if d.PausedUntil == nil {
		return true
	}

	until, err := time.Parse(time.RFC3339, *d.PausedUntil)
	if err != nil {
		// Unparseable timestamp → treat as indefinite to avoid accidentally
		// resuming a drive with a corrupt config value.
		return true
	}

	return now.Before(until)
}

// CLIOverrides holds values from CLI flags that override config file and
// environment settings. Pointer fields distinguish "not specified" (nil)
// from "explicitly set to zero value" — this matters because --dry-run=false
// is different from not passing --dry-run at all.
type CLIOverrides struct {
	ConfigPath string // --config flag (empty = use default)
	Account    string // --account flag (auth commands)
	Drive      string // --drive flag (drive selection)
	DryRun     *bool  // --dry-run flag
}
