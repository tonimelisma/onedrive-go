// Package config implements TOML configuration loading, validation, and
// platform-specific path resolution for onedrive-go. It supports a four-layer
// override chain (defaults -> config file -> environment -> CLI flags) with
// per-profile section-level overrides that completely replace global sections.
package config

// Config is the top-level configuration structure parsed from a TOML file.
// It contains named profiles and global configuration sections. When a profile
// defines its own section (e.g. [profile.work.filter]), that section completely
// replaces the global one — there is no merging of individual fields.
type Config struct {
	Profiles  map[string]Profile `toml:"profile"`
	Filter    FilterConfig       `toml:"filter"`
	Transfers TransfersConfig    `toml:"transfers"`
	Safety    SafetyConfig       `toml:"safety"`
	Sync      SyncConfig         `toml:"sync"`
	Logging   LoggingConfig      `toml:"logging"`
	Network   NetworkConfig      `toml:"network"`
}

// FilterConfig controls which files and directories are included in sync.
// These patterns are matched against the relative path within the sync directory.
type FilterConfig struct {
	SkipFiles    []string `toml:"skip_files"`
	SkipDirs     []string `toml:"skip_dirs"`
	SkipDotfiles bool     `toml:"skip_dotfiles"`
	SkipSymlinks bool     `toml:"skip_symlinks"`
	MaxFileSize  string   `toml:"max_file_size"`
	SyncPaths    []string `toml:"sync_paths"`
	IgnoreMarker string   `toml:"ignore_marker"`
}

// TransfersConfig controls parallel workers, chunk sizes, and bandwidth limits.
// The chunk_size must be a multiple of 320 KiB per the OneDrive upload API spec.
type TransfersConfig struct {
	ParallelDownloads int                      `toml:"parallel_downloads"`
	ParallelUploads   int                      `toml:"parallel_uploads"`
	ParallelCheckers  int                      `toml:"parallel_checkers"`
	ChunkSize         string                   `toml:"chunk_size"`
	BandwidthLimit    string                   `toml:"bandwidth_limit"`
	BandwidthSchedule []BandwidthScheduleEntry `toml:"bandwidth_schedule"`
	TransferOrder     string                   `toml:"transfer_order"`
}

// BandwidthScheduleEntry defines a time-of-day bandwidth limit.
// Entries must be sorted chronologically.
type BandwidthScheduleEntry struct {
	Time  string `toml:"time"`
	Limit string `toml:"limit"`
}

// SafetyConfig controls protective defaults and thresholds that prevent
// accidental data loss during sync operations.
type SafetyConfig struct {
	BigDeleteThreshold        int    `toml:"big_delete_threshold"`
	BigDeletePercentage       int    `toml:"big_delete_percentage"`
	BigDeleteMinItems         int    `toml:"big_delete_min_items"`
	MinFreeSpace              string `toml:"min_free_space"`
	UseRecycleBin             bool   `toml:"use_recycle_bin"`
	UseLocalTrash             bool   `toml:"use_local_trash"`
	DisableDownloadValidation bool   `toml:"disable_download_validation"`
	DisableUploadValidation   bool   `toml:"disable_upload_validation"`
	SyncDirPermissions        string `toml:"sync_dir_permissions"`
	SyncFilePermissions       string `toml:"sync_file_permissions"`
	TombstoneRetentionDays    int    `toml:"tombstone_retention_days"`
}

// SyncConfig controls sync engine behavior: polling intervals, conflict
// resolution strategy, and graceful shutdown timing.
type SyncConfig struct {
	PollInterval             string `toml:"poll_interval"`
	FullscanFrequency        int    `toml:"fullscan_frequency"`
	Websocket                bool   `toml:"websocket"`
	ConflictStrategy         string `toml:"conflict_strategy"`
	ConflictReminderInterval string `toml:"conflict_reminder_interval"`
	DryRun                   bool   `toml:"dry_run"`
	VerifyInterval           string `toml:"verify_interval"`
	ShutdownTimeout          string `toml:"shutdown_timeout"`
}

// LoggingConfig controls log output behavior: level, format, and rotation.
type LoggingConfig struct {
	LogLevel         string `toml:"log_level"`
	LogFile          string `toml:"log_file"`
	LogFormat        string `toml:"log_format"`
	LogRetentionDays int    `toml:"log_retention_days"`
}

// NetworkConfig controls HTTP client behavior: timeouts, user agent, and
// protocol version. force_http_11 is useful behind corporate proxies that
// don't support HTTP/2.
type NetworkConfig struct {
	ConnectTimeout string `toml:"connect_timeout"`
	DataTimeout    string `toml:"data_timeout"`
	UserAgent      string `toml:"user_agent"`
	ForceHTTP11    bool   `toml:"force_http_11"`
}

// CLIOverrides holds values from CLI flags that override config file and
// environment settings. Pointer fields distinguish "not specified" (nil)
// from "explicitly set to zero value" — this matters because --dry-run=false
// is different from not passing --dry-run at all.
type CLIOverrides struct {
	ConfigPath string  // --config flag (empty = use default)
	Profile    string  // --profile flag (empty = use default)
	SyncDir    *string // --sync-dir flag
	DryRun     *bool   // --dry-run flag
}
