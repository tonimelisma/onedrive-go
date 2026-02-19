package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
[filter]
skip_files = ["*.tmp", "*.swp"]
skip_dirs = ["node_modules", ".git"]
skip_dotfiles = true
skip_symlinks = true
max_file_size = "1GB"
sync_paths = ["/Documents", "/Photos"]
ignore_marker = ".syncignore"

[transfers]
parallel_downloads = 4
parallel_uploads = 4
parallel_checkers = 4
chunk_size = "20MiB"
bandwidth_limit = "5MB/s"
transfer_order = "size_asc"

[safety]
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
tombstone_retention_days = 60

[sync]
poll_interval = "10m"
fullscan_frequency = 6
websocket = false
conflict_strategy = "keep_both"
conflict_reminder_interval = "2h"
dry_run = true
verify_interval = "168h"
shutdown_timeout = "60s"

[logging]
log_level = "debug"
log_file = "/tmp/onedrive-go.log"
log_format = "json"
log_retention_days = 7

[network]
connect_timeout = "30s"
data_timeout = "120s"
user_agent = "ISV|test|test/v0.1.0"
force_http_11 = true
`

	path := writeTestConfig(t, tomlContent)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, []string{"*.tmp", "*.swp"}, cfg.Filter.SkipFiles)
	assert.Equal(t, []string{"node_modules", ".git"}, cfg.Filter.SkipDirs)
	assert.True(t, cfg.Filter.SkipDotfiles)
	assert.True(t, cfg.Filter.SkipSymlinks)
	assert.Equal(t, "1GB", cfg.Filter.MaxFileSize)
	assert.Equal(t, []string{"/Documents", "/Photos"}, cfg.Filter.SyncPaths)
	assert.Equal(t, ".syncignore", cfg.Filter.IgnoreMarker)

	assert.Equal(t, 4, cfg.Transfers.ParallelDownloads)
	assert.Equal(t, 4, cfg.Transfers.ParallelUploads)
	assert.Equal(t, 4, cfg.Transfers.ParallelCheckers)
	assert.Equal(t, "20MiB", cfg.Transfers.ChunkSize)
	assert.Equal(t, "5MB/s", cfg.Transfers.BandwidthLimit)
	assert.Equal(t, "size_asc", cfg.Transfers.TransferOrder)

	assert.Equal(t, 500, cfg.Safety.BigDeleteThreshold)
	assert.Equal(t, 25, cfg.Safety.BigDeletePercentage)
	assert.Equal(t, 5, cfg.Safety.BigDeleteMinItems)
	assert.Equal(t, "2GB", cfg.Safety.MinFreeSpace)
	assert.False(t, cfg.Safety.UseRecycleBin)
	assert.False(t, cfg.Safety.UseLocalTrash)
	assert.True(t, cfg.Safety.DisableDownloadValidation)
	assert.True(t, cfg.Safety.DisableUploadValidation)
	assert.Equal(t, "0755", cfg.Safety.SyncDirPermissions)
	assert.Equal(t, "0644", cfg.Safety.SyncFilePermissions)
	assert.Equal(t, 60, cfg.Safety.TombstoneRetentionDays)

	assert.Equal(t, "10m", cfg.Sync.PollInterval)
	assert.Equal(t, 6, cfg.Sync.FullscanFrequency)
	assert.False(t, cfg.Sync.Websocket)
	assert.Equal(t, "keep_both", cfg.Sync.ConflictStrategy)
	assert.Equal(t, "2h", cfg.Sync.ConflictReminderInterval)
	assert.True(t, cfg.Sync.DryRun)
	assert.Equal(t, "168h", cfg.Sync.VerifyInterval)
	assert.Equal(t, "60s", cfg.Sync.ShutdownTimeout)

	assert.Equal(t, "debug", cfg.Logging.LogLevel)
	assert.Equal(t, "/tmp/onedrive-go.log", cfg.Logging.LogFile)
	assert.Equal(t, "json", cfg.Logging.LogFormat)
	assert.Equal(t, 7, cfg.Logging.LogRetentionDays)

	assert.Equal(t, "30s", cfg.Network.ConnectTimeout)
	assert.Equal(t, "120s", cfg.Network.DataTimeout)
	assert.Equal(t, "ISV|test|test/v0.1.0", cfg.Network.UserAgent)
	assert.True(t, cfg.Network.ForceHTTP11)
}

func TestLoad_MinimalConfig_UsesDefaults(t *testing.T) {
	path := writeTestConfig(t, "")
	cfg, err := Load(path)
	require.NoError(t, err)

	// Should match all defaults
	assert.Equal(t, 8, cfg.Transfers.ParallelDownloads)
	assert.Equal(t, "10MiB", cfg.Transfers.ChunkSize)
	assert.Equal(t, "info", cfg.Logging.LogLevel)
	assert.Equal(t, "5m", cfg.Sync.PollInterval)
}

func TestLoad_MalformedTOML(t *testing.T) {
	path := writeTestConfig(t, `[filter
not valid toml`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing config file")
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml")
	require.Error(t, err)
}

func TestLoad_UnknownKey_TopLevel(t *testing.T) {
	path := writeTestConfig(t, `
unknown_section = "value"
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestLoad_UnknownKey_InSection(t *testing.T) {
	//nolint:misspell // intentional typo to test unknown key detection
	path := writeTestConfig(t, "[transfers]\nparralel_downloads = 4\n")
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.Contains(t, err.Error(), "parallel_downloads")
}

func TestLoad_UnknownKey_TypoInFilter(t *testing.T) {
	path := writeTestConfig(t, `
[filter]
skip_file = ["*.tmp"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_files")
}

func TestLoad_UnknownKey_NoSuggestion(t *testing.T) {
	path := writeTestConfig(t, `
[filter]
completely_unrelated_key = true
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.NotContains(t, err.Error(), "did you mean")
}

func TestLoad_ValidationError(t *testing.T) {
	path := writeTestConfig(t, `
[transfers]
parallel_downloads = 0
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")
}

func TestLoadOrDefault_FileExists(t *testing.T) {
	path := writeTestConfig(t, `
[logging]
log_level = "debug"
`)
	cfg, err := LoadOrDefault(path)
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.Logging.LogLevel)
}

func TestLoadOrDefault_FileNotFound(t *testing.T) {
	cfg, err := LoadOrDefault("/nonexistent/path/config.toml")
	require.NoError(t, err)
	// Should return defaults
	assert.Equal(t, "info", cfg.Logging.LogLevel)
	assert.Equal(t, 8, cfg.Transfers.ParallelDownloads)
}

func TestLoad_PartialConfig_UsesDefaults(t *testing.T) {
	path := writeTestConfig(t, `
[logging]
log_level = "warn"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	// Explicitly set
	assert.Equal(t, "warn", cfg.Logging.LogLevel)
	// Defaults for everything else
	assert.Equal(t, 8, cfg.Transfers.ParallelDownloads)
	assert.Equal(t, "5m", cfg.Sync.PollInterval)
	assert.Equal(t, ".odignore", cfg.Filter.IgnoreMarker)
}

func TestLoad_BandwidthSchedule(t *testing.T) {
	path := writeTestConfig(t, `
[transfers]
bandwidth_schedule = [
    { time = "08:00", limit = "5MB/s" },
    { time = "18:00", limit = "50MB/s" },
    { time = "23:00", limit = "0" },
]
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Transfers.BandwidthSchedule, 3)
	assert.Equal(t, "08:00", cfg.Transfers.BandwidthSchedule[0].Time)
	assert.Equal(t, "5MB/s", cfg.Transfers.BandwidthSchedule[0].Limit)
	assert.Equal(t, "18:00", cfg.Transfers.BandwidthSchedule[1].Time)
	assert.Equal(t, "23:00", cfg.Transfers.BandwidthSchedule[2].Time)
}
