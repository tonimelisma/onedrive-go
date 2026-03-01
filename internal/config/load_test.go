package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// testLogger returns a debug-level logger that writes to t.Log, ensuring all
// config debug output appears in test output for CI visibility.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	err := os.WriteFile(path, []byte(content), 0o600)
	require.NoError(t, err)

	return path
}

func TestLoad_ValidFullConfig(t *testing.T) {
	tomlContent := `
skip_files = ["*.tmp", "*.swp"]
skip_dirs = ["node_modules", ".git"]
skip_dotfiles = true
skip_symlinks = true
max_file_size = "1GB"
sync_paths = ["/Documents", "/Photos"]
ignore_marker = ".syncignore"

parallel_downloads = 4
parallel_uploads = 4
parallel_checkers = 4
chunk_size = "20MiB"
bandwidth_limit = "5MB/s"
transfer_order = "size_asc"

big_delete_threshold = 500
big_delete_percentage = 25
big_delete_min_items = 5
min_free_space = "2GB"
use_recycle_bin = false
use_local_trash = false
disable_download_validation = true
disable_upload_validation = true
sync_dir_permissions = "0755"
sync_file_permissions = "0644"

poll_interval = "10m"
fullscan_frequency = 6
websocket = false
conflict_strategy = "keep_both"
conflict_reminder_interval = "2h"
dry_run = true
verify_interval = "168h"
shutdown_timeout = "60s"

log_level = "debug"
log_file = "/tmp/onedrive-go.log"
log_format = "json"
log_retention_days = 7

connect_timeout = "30s"
data_timeout = "120s"
user_agent = "ISV|test|test/v0.1.0"
force_http_11 = true
`

	path := writeTestConfig(t, tomlContent)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.Equal(t, []string{"*.tmp", "*.swp"}, cfg.SkipFiles)
	assert.Equal(t, []string{"node_modules", ".git"}, cfg.SkipDirs)
	assert.True(t, cfg.SkipDotfiles)
	assert.True(t, cfg.SkipSymlinks)
	assert.Equal(t, "1GB", cfg.MaxFileSize)
	assert.Equal(t, []string{"/Documents", "/Photos"}, cfg.SyncPaths)
	assert.Equal(t, ".syncignore", cfg.IgnoreMarker)

	assert.Equal(t, 4, cfg.ParallelDownloads)
	assert.Equal(t, 4, cfg.ParallelUploads)
	assert.Equal(t, 4, cfg.ParallelCheckers)
	assert.Equal(t, "20MiB", cfg.ChunkSize)
	assert.Equal(t, "5MB/s", cfg.BandwidthLimit)
	assert.Equal(t, "size_asc", cfg.TransferOrder)

	assert.Equal(t, 500, cfg.BigDeleteThreshold)
	assert.Equal(t, 25, cfg.BigDeletePercentage)
	assert.Equal(t, 5, cfg.BigDeleteMinItems)
	assert.Equal(t, "2GB", cfg.MinFreeSpace)
	assert.False(t, cfg.UseRecycleBin)
	assert.False(t, cfg.UseLocalTrash)
	assert.True(t, cfg.DisableDownloadValidation)
	assert.True(t, cfg.DisableUploadValidation)
	assert.Equal(t, "0755", cfg.SyncDirPermissions)
	assert.Equal(t, "0644", cfg.SyncFilePermissions)

	assert.Equal(t, "10m", cfg.PollInterval)
	assert.Equal(t, 6, cfg.FullscanFrequency)
	assert.False(t, cfg.Websocket)
	assert.Equal(t, "keep_both", cfg.ConflictStrategy)
	assert.Equal(t, "2h", cfg.ConflictReminderInterval)
	assert.True(t, cfg.DryRun)
	assert.Equal(t, "168h", cfg.VerifyInterval)
	assert.Equal(t, "60s", cfg.ShutdownTimeout)

	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "/tmp/onedrive-go.log", cfg.LogFile)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, 7, cfg.LogRetentionDays)

	assert.Equal(t, "30s", cfg.ConnectTimeout)
	assert.Equal(t, "120s", cfg.DataTimeout)
	assert.Equal(t, "ISV|test|test/v0.1.0", cfg.UserAgent)
	assert.True(t, cfg.ForceHTTP11)
}

func TestLoad_MinimalConfig_UsesDefaults(t *testing.T) {
	path := writeTestConfig(t, "")
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.Equal(t, 8, cfg.ParallelDownloads)
	assert.Equal(t, "10MiB", cfg.ChunkSize)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "5m", cfg.PollInterval)
}

func TestLoad_MalformedTOML(t *testing.T) {
	path := writeTestConfig(t, `[filter
not valid toml`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing config file")
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml", testLogger(t))
	require.Error(t, err)
}

func TestLoad_ValidationError(t *testing.T) {
	path := writeTestConfig(t, `parallel_downloads = 0`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")
}

func TestLoadOrDefault_FileExists(t *testing.T) {
	path := writeTestConfig(t, `log_level = "debug"`)
	cfg, err := LoadOrDefault(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestLoadOrDefault_FileNotFound(t *testing.T) {
	cfg, err := LoadOrDefault("/nonexistent/path/config.toml", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 8, cfg.ParallelDownloads)
}

func TestLoad_PartialConfig_UsesDefaults(t *testing.T) {
	path := writeTestConfig(t, `log_level = "warn"`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.Equal(t, "warn", cfg.LogLevel)
	assert.Equal(t, 8, cfg.ParallelDownloads)
	assert.Equal(t, "5m", cfg.PollInterval)
	assert.Equal(t, ".odignore", cfg.IgnoreMarker)
}

func TestLoad_BandwidthSchedule(t *testing.T) {
	path := writeTestConfig(t, `
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },
    { time = "18:00", limit = "50MB/s" },
    { time = "23:00", limit = "0" },
]
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.BandwidthSchedule, 3)
	assert.Equal(t, "08:00", cfg.BandwidthSchedule[0].Time)
	assert.Equal(t, "5MB/s", cfg.BandwidthSchedule[0].Limit)
	assert.Equal(t, "18:00", cfg.BandwidthSchedule[1].Time)
	assert.Equal(t, "23:00", cfg.BandwidthSchedule[2].Time)
}

// --- Two-pass decode: drive section tests ---

func TestLoad_SingleDriveSection(t *testing.T) {
	path := writeTestConfig(t, `
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)

	d := cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")]
	assert.Equal(t, "~/OneDrive", d.SyncDir)
	assert.Equal(t, "home", d.DisplayName)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestLoad_MultipleDriveSections(t *testing.T) {
	path := writeTestConfig(t, `
skip_dotfiles = true

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
display_name = "work"
skip_dirs = ["node_modules", ".git", "vendor"]
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 2)

	personal := cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")]
	assert.Equal(t, "~/OneDrive", personal.SyncDir)
	assert.Equal(t, "home", personal.DisplayName)

	business := cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")]
	assert.Equal(t, "~/OneDrive - Contoso", business.SyncDir)
	assert.Equal(t, "work", business.DisplayName)
	assert.Equal(t, []string{"node_modules", ".git", "vendor"}, business.SkipDirs)
}

func TestLoad_DriveWithAllFields(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"
paused = true
remote_path = "/Documents"
drive_id = "abc123"
skip_dotfiles = true
skip_dirs = ["vendor"]
skip_files = ["*.log"]
poll_interval = "10m"
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	d := cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")]
	assert.Equal(t, "~/OneDrive", d.SyncDir)
	assert.Equal(t, "home", d.DisplayName)
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused)
	assert.Equal(t, "/Documents", d.RemotePath)
	assert.Equal(t, "abc123", d.DriveID)
	require.NotNil(t, d.SkipDotfiles)
	assert.True(t, *d.SkipDotfiles)
	assert.Equal(t, []string{"vendor"}, d.SkipDirs)
	assert.Equal(t, []string{"*.log"}, d.SkipFiles)
	assert.Equal(t, "10m", d.PollInterval)
}

func TestLoad_SharePointDrive(t *testing.T) {
	path := writeTestConfig(t, `
["sharepoint:alice@contoso.com:marketing:Documents"]
sync_dir = "~/Contoso/Marketing"
paused = true
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)

	d := cfg.Drives[driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")]
	assert.Equal(t, "~/Contoso/Marketing", d.SyncDir)
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused)
}

// --- ResolveDrive tests ---

func TestResolveDrive_SingleDrive_AutoSelect(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
	assert.Contains(t, resolved.SyncDir, "OneDrive")
}

func TestResolveDrive_NoDrives_Error(t *testing.T) {
	// Override HOME so token discovery finds nothing on disk.
	t.Setenv("HOME", t.TempDir())

	path := writeTestConfig(t, `log_level = "debug"`)
	_, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts")
}

func TestResolveDrive_MultipleDrives_NoDriveFlag_Error(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"

["business:alice@contoso.com"]
sync_dir = "~/Work"
`)
	_, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple drives")
}

func TestResolveDrive_CLIDriveSelector(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
display_name = "work"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{Drive: "work"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), resolved.CanonicalID)
}

func TestResolveDrive_EnvDriveSelector(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
display_name = "work"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path, Drive: "home"},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
}

func TestResolveDrive_CLIDriveOverridesEnv(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
display_name = "work"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path, Drive: "home"},
		CLIOverrides{Drive: "work"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), resolved.CanonicalID)
}

func TestResolveDrive_CLIConfigPathOverridesEnv(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: "/wrong/path"},
		CLIOverrides{ConfigPath: path},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
}

func TestResolveDrive_CLIDryRunOverride(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)
	dryRun := true
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{DryRun: &dryRun},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.True(t, resolved.DryRun)
}

func TestResolveDrive_InvalidConfigFile(t *testing.T) {
	path := writeTestConfig(t, `[invalid toml`)
	_, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.Error(t, err)
}

func TestResolveDrive_NoConfigFile(t *testing.T) {
	// Override HOME so token discovery finds nothing on disk.
	t.Setenv("HOME", t.TempDir())

	_, err := ResolveDrive(
		EnvOverrides{ConfigPath: "/nonexistent/config.toml"},
		CLIOverrides{},
		testLogger(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts")
}

func TestResolveDrive_PerDriveOverridesApplied(t *testing.T) {
	path := writeTestConfig(t, `
skip_dotfiles = false
poll_interval = "5m"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
skip_dotfiles = true
skip_dirs = ["vendor"]
skip_files = ["*.log"]
poll_interval = "10m"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)

	assert.True(t, resolved.SkipDotfiles)
	assert.Equal(t, []string{"vendor"}, resolved.SkipDirs)
	assert.Equal(t, []string{"*.log"}, resolved.SkipFiles)
	assert.Equal(t, "10m", resolved.PollInterval)
}

func TestResolveDrive_GlobalSettingsUsedWhenNoDriveOverride(t *testing.T) {
	path := writeTestConfig(t, `
skip_dotfiles = true
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)

	assert.True(t, resolved.SkipDotfiles)
	assert.Equal(t, "debug", resolved.LogLevel)
}

// --- Edge case: drive section is not a table ---

func TestLoad_DriveSectionNotTable(t *testing.T) {
	// A drive section key containing ":" but with a scalar value instead of a table.
	path := writeTestConfig(t, `"personal:toni@outlook.com" = "not a table"`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a table")
}

// --- Edge case: unknown key with known parent in bandwidth_schedule ---

func TestLoad_DriveSection_TypeMismatch(t *testing.T) {
	// A drive section where "paused" is a string instead of a boolean should
	// trigger a type-coercion error in mapToDrive during the re-encode/decode cycle.
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused = "not-a-bool"
`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "personal:toni@outlook.com")
}

func TestLoad_BandwidthScheduleSubField_NotFlagged(t *testing.T) {
	// bandwidth_schedule entries have "time" and "limit" sub-fields.
	// These appear as undecoded keys but the parent is known, so they should be skipped.
	path := writeTestConfig(t, `
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },
]
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.BandwidthSchedule, 1)
}
