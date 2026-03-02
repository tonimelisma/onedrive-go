package config

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig_AllFieldsPopulated(t *testing.T) {
	cfg := DefaultConfig()
	require.NotNil(t, cfg)

	// Filter defaults (using promoted field access)
	assert.Equal(t, ".odignore", cfg.IgnoreMarker)
	assert.Equal(t, "50GB", cfg.MaxFileSize)
	assert.False(t, cfg.SkipDotfiles)
	assert.False(t, cfg.SkipSymlinks)
	assert.Empty(t, cfg.SkipFiles)
	assert.Empty(t, cfg.SkipDirs)
	assert.Empty(t, cfg.SyncPaths)

	// Transfers defaults
	assert.Equal(t, 8, cfg.TransferWorkers)
	assert.Equal(t, 4, cfg.CheckWorkers)
	assert.Equal(t, "10MiB", cfg.ChunkSize)
	assert.Equal(t, "0", cfg.BandwidthLimit)
	assert.Equal(t, "default", cfg.TransferOrder)
	assert.Empty(t, cfg.BandwidthSchedule)

	// Safety defaults
	assert.Equal(t, 1000, cfg.BigDeleteThreshold)
	assert.Equal(t, 50, cfg.BigDeletePercentage)
	assert.Equal(t, 10, cfg.BigDeleteMinItems)
	assert.Equal(t, "1GB", cfg.MinFreeSpace)
	assert.True(t, cfg.UseRecycleBin)
	assert.Equal(t, runtime.GOOS == "darwin", cfg.UseLocalTrash) // platform-specific default
	assert.False(t, cfg.DisableDownloadValidation)
	assert.False(t, cfg.DisableUploadValidation)
	assert.Equal(t, "0700", cfg.SyncDirPermissions)
	assert.Equal(t, "0600", cfg.SyncFilePermissions)

	// Sync defaults
	assert.Equal(t, "5m", cfg.PollInterval)
	assert.Equal(t, 12, cfg.FullscanFrequency)
	assert.False(t, cfg.Websocket) // unimplemented — defaults to false
	assert.Equal(t, "keep_both", cfg.ConflictStrategy)
	assert.Equal(t, "1h", cfg.ConflictReminderInterval)
	assert.False(t, cfg.DryRun)
	assert.Equal(t, "0", cfg.VerifyInterval)
	assert.Equal(t, "30s", cfg.ShutdownTimeout)

	// Logging defaults
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "", cfg.LogFile)
	assert.Equal(t, "auto", cfg.LogFormat)
	assert.Equal(t, 30, cfg.LogRetentionDays)

	// Network defaults
	assert.Equal(t, "10s", cfg.ConnectTimeout)
	assert.Equal(t, "60s", cfg.DataTimeout)
	assert.Equal(t, "", cfg.UserAgent)
	assert.False(t, cfg.ForceHTTP11)

	// Drives map initialized
	require.NotNil(t, cfg.Drives)
	assert.Empty(t, cfg.Drives)
}

func TestDefaultConfig_PassesValidation(t *testing.T) {
	cfg := DefaultConfig()
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestConfig_EmbeddedStructPromotion(t *testing.T) {
	// Verify that embedded struct fields are accessible directly on Config.
	cfg := DefaultConfig()

	// These should compile and work because of struct embedding.
	assert.Equal(t, false, cfg.SkipDotfiles)
	assert.Equal(t, 8, cfg.TransferWorkers)
	assert.Equal(t, 1000, cfg.BigDeleteThreshold)
	assert.Equal(t, "5m", cfg.PollInterval)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "10s", cfg.ConnectTimeout)
}
