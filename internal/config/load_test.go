package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Validates: R-4.1.1
func TestLoad_ValidFullConfig(t *testing.T) {
	tomlContent := `
skip_files = ["*.tmp", "*.swp"]
skip_dirs = ["node_modules", ".git"]
skip_dotfiles = true
skip_symlinks = true
sync_paths = ["/Documents", "/Photos"]
ignore_marker = ".syncignore"

transfer_workers = 16
check_workers = 8
chunk_size = "20MiB"
bandwidth_limit = "5MB/s"
transfer_order = "size_asc"

big_delete_threshold = 500
min_free_space = "2GB"
use_local_trash = false
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
	assert.Equal(t, []string{"/Documents", "/Photos"}, cfg.SyncPaths)
	assert.Equal(t, ".syncignore", cfg.IgnoreMarker)

	assert.Equal(t, 16, cfg.TransferWorkers)
	assert.Equal(t, 8, cfg.CheckWorkers)
	assert.Equal(t, "20MiB", cfg.ChunkSize)
	assert.Equal(t, "5MB/s", cfg.BandwidthLimit)
	assert.Equal(t, "size_asc", cfg.TransferOrder)

	assert.Equal(t, 500, cfg.BigDeleteThreshold)
	assert.Equal(t, "2GB", cfg.MinFreeSpace)
	assert.False(t, cfg.UseLocalTrash)
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

// Validates: R-4.2.1
func TestLoad_MinimalConfig_UsesDefaults(t *testing.T) {
	path := writeTestConfig(t, "")
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.Equal(t, 8, cfg.TransferWorkers)
	assert.Equal(t, 4, cfg.CheckWorkers)
	assert.Equal(t, "10MiB", cfg.ChunkSize)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "5m", cfg.PollInterval)
}

func TestLoad_TransferWorkers_Default(t *testing.T) {
	path := writeTestConfig(t, "")
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 8, cfg.TransferWorkers)
}

func TestLoad_CheckWorkers_Default(t *testing.T) {
	path := writeTestConfig(t, "")
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.CheckWorkers)
}

func TestLoad_DeprecatedKeys_Warning(t *testing.T) {
	// Using old parallel_downloads key should load successfully (key is
	// "known" to avoid unknown-key error) but the value is ignored —
	// transfer_workers uses its default value. The deprecation warning
	// is produced by WarnDeprecatedKeys, tested separately.
	path := writeTestConfig(t, `parallel_downloads = 4`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	// The old key's value is NOT mapped to any struct field (field removed).
	// transfer_workers retains its default value.
	assert.Equal(t, 8, cfg.TransferWorkers)
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
	path := writeTestConfig(t, `transfer_workers = 0`)
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
	assert.Equal(t, 8, cfg.TransferWorkers)
}

func TestLoad_ReadManagedFileError(t *testing.T) {
	_, err := loadWithIO("/tmp/config.toml", testLogger(t), configIO{
		readManagedFile: func(path string) ([]byte, error) {
			return nil, errors.New("boom")
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
	assert.Contains(t, err.Error(), "boom")
}

func TestLoadOrDefault_StatError(t *testing.T) {
	_, err := loadOrDefaultWithIO("/tmp/config.toml", testLogger(t), configIO{
		statManagedPath: func(path string) (os.FileInfo, error) {
			return nil, errors.New("boom")
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stating config file")
	assert.Contains(t, err.Error(), "boom")
}

func TestLoadLenient_ReadManagedFileError(t *testing.T) {
	_, _, err := loadLenientWithIO("/tmp/config.toml", testLogger(t), configIO{
		readManagedFile: func(path string) ([]byte, error) {
			return nil, errors.New("boom")
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
	assert.Contains(t, err.Error(), "boom")
}

func TestLoadOrDefaultLenient_StatError(t *testing.T) {
	_, _, err := loadOrDefaultLenientWithIO("/tmp/config.toml", testLogger(t), configIO{
		statManagedPath: func(path string) (os.FileInfo, error) {
			return nil, errors.New("boom")
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stating config file")
	assert.Contains(t, err.Error(), "boom")
}

func TestLoad_PartialConfig_UsesDefaults(t *testing.T) {
	path := writeTestConfig(t, `log_level = "warn"`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.Equal(t, "warn", cfg.LogLevel)
	assert.Equal(t, 8, cfg.TransferWorkers)
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

// Validates: R-4.1.1
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

// Validates: R-4.1.1, R-3.4.1
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
	resolved, rawCfg, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
	assert.Contains(t, resolved.SyncDir, "OneDrive")
	assert.NotNil(t, rawCfg, "raw config should be returned alongside resolved drive")
	assert.Len(t, rawCfg.Drives, 1)
}

// Validates: R-4.8.5, R-4.9.2, R-4.9.3
func TestResolveDrive_EmptyDriveSection_UsesResolvedSyncDirDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
`)
	resolved, rawCfg, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)
	require.NotNil(t, rawCfg)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
	assert.Equal(t, filepath.Join(home, "OneDrive"), resolved.SyncDir)
	assert.NoError(t, ValidateResolvedForSync(resolved))
}

func TestResolveDrive_NoDrives_Error(t *testing.T) {
	// Override HOME so token discovery finds nothing on disk.
	t.Setenv("HOME", t.TempDir())

	path := writeTestConfig(t, `log_level = "debug"`)
	_, _, err := ResolveDrive(
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
	_, _, err := ResolveDrive(
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
	resolved, _, err := ResolveDrive(
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
	resolved, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: path, Drive: "home"},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
}

// Validates: R-4.3
func TestResolveDrive_CLIDriveOverridesEnv(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
display_name = "work"
`)
	resolved, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: path, Drive: "home"},
		CLIOverrides{Drive: "work"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), resolved.CanonicalID)
}

// Validates: R-4.3
func TestResolveDrive_CLIConfigPathOverridesEnv(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)
	resolved, _, err := ResolveDrive(
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
	resolved, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{DryRun: &dryRun},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.True(t, resolved.DryRun)
}

func TestResolveDrive_InvalidConfigFile(t *testing.T) {
	path := writeTestConfig(t, `[invalid toml`)
	_, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{},
		testLogger(t),
	)
	require.Error(t, err)
}

func TestResolveDrive_NoConfigFile(t *testing.T) {
	// Override HOME so token discovery finds nothing on disk.
	t.Setenv("HOME", t.TempDir())

	_, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: "/nonexistent/config.toml"},
		CLIOverrides{},
		testLogger(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts")
}

// Validates: R-4.3
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
	resolved, _, err := ResolveDrive(
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

// Validates: R-4.3
func TestResolveDrive_GlobalSettingsUsedWhenNoDriveOverride(t *testing.T) {
	path := writeTestConfig(t, `
skip_dotfiles = true
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)
	resolved, _, err := ResolveDrive(
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

// --- ResolveDrives ---

func TestResolveDrives_AllDrives(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	resolved, err := ResolveDrives(cfg, nil, false, testLogger(t))
	require.NoError(t, err)
	assert.Len(t, resolved, 2)
}

func TestResolveDrives_FilterBySelector(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	resolved, err := ResolveDrives(cfg, []string{"personal"}, false, testLogger(t))
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "personal:toni@outlook.com", resolved[0].CanonicalID.String())
}

func TestResolveDrives_ExcludesPausedByDefault(t *testing.T) {
	paused := true
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{
		SyncDir: "~/Work",
		Paused:  &paused,
	}

	resolved, err := ResolveDrives(cfg, nil, false, testLogger(t))
	require.NoError(t, err)
	assert.Len(t, resolved, 1)
	assert.Equal(t, "personal:toni@outlook.com", resolved[0].CanonicalID.String())
}

func TestResolveDrives_IncludePausedWhenRequested(t *testing.T) {
	paused := true
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{
		SyncDir: "~/Work",
		Paused:  &paused,
	}

	resolved, err := ResolveDrives(cfg, nil, true, testLogger(t))
	require.NoError(t, err)
	assert.Len(t, resolved, 2)
}

func TestResolveDrives_SortedByCanonicalID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:zack@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	resolved, err := ResolveDrives(cfg, nil, false, testLogger(t))
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	assert.Equal(t, "business:alice@contoso.com", resolved[0].CanonicalID.String())
	assert.Equal(t, "personal:zack@outlook.com", resolved[1].CanonicalID.String())
}

func TestResolveDrives_ZeroDrives(t *testing.T) {
	cfg := DefaultConfig()

	resolved, err := ResolveDrives(cfg, nil, false, testLogger(t))
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

// --- ResolveConfigPath tests ---

// Validates: R-4.1.3
func TestResolveConfigPath_DefaultWhenEmpty(t *testing.T) {
	result := ResolveConfigPath(EnvOverrides{}, CLIOverrides{}, testLogger(t))
	assert.Equal(t, DefaultConfigPath(), result)
}

// Validates: R-4.1.3
func TestResolveConfigPath_EnvOverridesDefault(t *testing.T) {
	result := ResolveConfigPath(
		EnvOverrides{ConfigPath: "/env/config.toml"},
		CLIOverrides{},
		testLogger(t),
	)
	assert.Equal(t, "/env/config.toml", result)
}

// Validates: R-4.1.3
func TestResolveConfigPath_CLIOverridesEnv(t *testing.T) {
	result := ResolveConfigPath(
		EnvOverrides{ConfigPath: "/env/config.toml"},
		CLIOverrides{ConfigPath: "/cli/config.toml"},
		testLogger(t),
	)
	assert.Equal(t, "/cli/config.toml", result)
}

// Validates: R-4.1.3
// --- Lenient loading tests ---

// Validates: R-4.8.4
func TestLoadLenient_UnknownKeys_ReturnsWarnings(t *testing.T) {
	path := writeTestConfig(t, `
unknown_global_key = "value"
transfer_workers = 8
`)
	cfg, warnings, err := LoadLenient(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "unknown config key")
	assert.Contains(t, warnings[0].Message, "unknown_global_key")
}

// Validates: R-4.8.4
func TestLoadLenient_InvalidValues_ReturnsWarnings(t *testing.T) {
	path := writeTestConfig(t, `transfer_workers = 0`)
	cfg, warnings, err := LoadLenient(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotEmpty(t, warnings)

	found := false
	for _, w := range warnings {
		if strings.Contains(w.Message, "transfer_workers") {
			found = true

			break
		}
	}

	assert.True(t, found, "expected warning about transfer_workers")
}

// Validates: R-4.8.4
func TestLoadLenient_ValidConfig_NoWarnings(t *testing.T) {
	path := writeTestConfig(t, `log_level = "debug"`)
	cfg, warnings, err := LoadLenient(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, warnings)
	assert.Equal(t, "debug", cfg.LogLevel)
}

// Validates: R-4.8.4
func TestLoadLenient_TOMLSyntaxError_Fatal(t *testing.T) {
	path := writeTestConfig(t, `[invalid toml`)
	_, _, err := LoadLenient(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing config file")
}

// Validates: R-4.8.4
func TestLoadOrDefaultLenient_MissingFile_ReturnsDefaults(t *testing.T) {
	cfg, warnings, err := LoadOrDefaultLenient("/nonexistent/path/config.toml", testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, warnings)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 8, cfg.TransferWorkers)
}

// Validates: R-4.8.4
func TestLoadLenient_UnknownDriveKeys_ReturnsWarnings(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
unknown_drive_key = "value"
`)
	cfg, warnings, err := LoadLenient(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "unknown key")
	assert.Contains(t, warnings[0].Message, "unknown_drive_key")
}

// Validates: R-4.8.4
func TestLoadLenient_MixedWarnings(t *testing.T) {
	// Both unknown keys and validation errors should be collected.
	path := writeTestConfig(t, `
unknown_global_key = "value"
transfer_workers = 0
`)
	cfg, warnings, err := LoadLenient(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	// Should have at least 2 warnings: one for unknown key, one for invalid value.
	assert.GreaterOrEqual(t, len(warnings), 2)
}

// Validates: R-4.8.4
func TestLoadLenient_FileNotFound_Fatal(t *testing.T) {
	_, _, err := LoadLenient("/nonexistent/path/config.toml", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
}

// Validates: R-4.8.4
func TestLoadLenient_DriveParseError_Warning(t *testing.T) {
	// A drive section with a type mismatch becomes a warning in lenient mode.
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused = "not-a-bool"
`)
	cfg, warnings, err := LoadLenient(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	// The drive should not be in cfg.Drives (mapToDrive failed).
	assert.Empty(t, cfg.Drives)
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0].Message, "personal:toni@outlook.com")
}

func TestResolveConfigPath_CLIOverridesDefault(t *testing.T) {
	result := ResolveConfigPath(
		EnvOverrides{},
		CLIOverrides{ConfigPath: "/cli/config.toml"},
		testLogger(t),
	)
	assert.Equal(t, "/cli/config.toml", result)
}

func TestLogWarnings_EmitsWarnPerWarning(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	warnings := []ConfigWarning{
		{Message: "unknown key foo"},
		{Message: "invalid value for bar"},
	}

	LogWarnings(warnings, logger)

	assert.Len(t, loggedAttrValues(t, &logBuf, "msg"), 2)
}

func TestLogWarnings_EmptySlice_NoLogs(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	LogWarnings(nil, logger)

	assert.Empty(t, strings.TrimSpace(logBuf.String()))
}

// --- ClearExpiredPauses ---

// Validates: R-2.6.1
func TestClearExpiredPauses_ClearsExpired(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-1 * time.Hour).Format(time.RFC3339)

	cid := driveid.MustCanonicalID("personal:expired@example.com")
	cfgPath := writeTestConfig(t, `
["personal:expired@example.com"]
sync_dir = "~/OneDrive"
paused = true
paused_until = "`+expired+`"
`)
	cfg, err := Load(cfgPath, testLogger(t))
	require.NoError(t, err)

	ClearExpiredPauses(cfgPath, cfg, now, testLogger(t))

	// In-memory config should have paused fields cleared.
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused, "paused should be nil after clearing expired pause")
	assert.Nil(t, d.PausedUntil, "paused_until should be nil after clearing expired pause")

	// On-disk config should also have the keys removed.
	reloaded, err := Load(cfgPath, testLogger(t))
	require.NoError(t, err)
	rd := reloaded.Drives[cid]
	assert.Nil(t, rd.Paused, "paused should be removed from config file")
	assert.Nil(t, rd.PausedUntil, "paused_until should be removed from config file")
}

// Validates: R-2.6.1
func TestClearExpiredPauses_KeepsActive(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	future := now.Add(1 * time.Hour).Format(time.RFC3339)

	cid := driveid.MustCanonicalID("personal:active@example.com")
	cfgPath := writeTestConfig(t, `
["personal:active@example.com"]
sync_dir = "~/OneDrive"
paused = true
paused_until = "`+future+`"
`)
	cfg, err := Load(cfgPath, testLogger(t))
	require.NoError(t, err)

	ClearExpiredPauses(cfgPath, cfg, now, testLogger(t))

	// Active timed pause should be preserved.
	d := cfg.Drives[cid]
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused)
	require.NotNil(t, d.PausedUntil)
	assert.Equal(t, future, *d.PausedUntil)
}

// Validates: R-2.6.1
func TestClearExpiredPauses_KeepsIndefinite(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	cid := driveid.MustCanonicalID("personal:indefinite@example.com")
	cfgPath := writeTestConfig(t, `
["personal:indefinite@example.com"]
sync_dir = "~/OneDrive"
paused = true
`)
	cfg, err := Load(cfgPath, testLogger(t))
	require.NoError(t, err)

	ClearExpiredPauses(cfgPath, cfg, now, testLogger(t))

	// Indefinite pause should be preserved.
	d := cfg.Drives[cid]
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused)
}

// Validates: R-2.6.1
func TestClearExpiredPauses_NoPausedDrives(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	cfgPath := writeTestConfig(t, `
["personal:active@example.com"]
sync_dir = "~/OneDrive"
`)
	cfg, err := Load(cfgPath, testLogger(t))
	require.NoError(t, err)

	// Should be a no-op, no errors.
	ClearExpiredPauses(cfgPath, cfg, now, testLogger(t))

	// Drive should still be present and unmodified.
	assert.Len(t, cfg.Drives, 1)
}

// TestDecodeDriveSections_StrictAndLenient_IdenticalOutput verifies that valid
// configs produce the same cfg.Drives from both the strict and lenient paths.
// This is a regression guard for the unified decodeDriveSectionsInternal.
func TestDecodeDriveSections_StrictAndLenient_IdenticalOutput(t *testing.T) {
	data := []byte(`
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
skip_dirs = ["vendor"]
`)

	strictCfg := DefaultConfig()
	err := decodeDriveSections(data, strictCfg)
	require.NoError(t, err)

	lenientCfg := DefaultConfig()
	warnings := decodeDriveSectionsLenient(data, lenientCfg)
	assert.Empty(t, warnings, "valid config should produce no warnings in lenient mode")

	// Both should produce identical drive maps.
	require.Len(t, lenientCfg.Drives, len(strictCfg.Drives))

	for cid, strictDrive := range strictCfg.Drives {
		lenientDrive, exists := lenientCfg.Drives[cid]
		require.True(t, exists, "lenient should have drive %s", cid.String())
		assert.Equal(t, strictDrive, lenientDrive, "drives should match for %s", cid.String())
	}
}
