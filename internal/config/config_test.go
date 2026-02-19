package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig_AllFieldsPopulated(t *testing.T) {
	cfg := DefaultConfig()
	require.NotNil(t, cfg)

	// Filter defaults
	assert.Equal(t, ".odignore", cfg.Filter.IgnoreMarker)
	assert.Equal(t, "50GB", cfg.Filter.MaxFileSize)
	assert.False(t, cfg.Filter.SkipDotfiles)
	assert.False(t, cfg.Filter.SkipSymlinks)
	assert.Empty(t, cfg.Filter.SkipFiles)
	assert.Empty(t, cfg.Filter.SkipDirs)
	assert.Empty(t, cfg.Filter.SyncPaths)

	// Transfers defaults
	assert.Equal(t, 8, cfg.Transfers.ParallelDownloads)
	assert.Equal(t, 8, cfg.Transfers.ParallelUploads)
	assert.Equal(t, 8, cfg.Transfers.ParallelCheckers)
	assert.Equal(t, "10MiB", cfg.Transfers.ChunkSize)
	assert.Equal(t, "0", cfg.Transfers.BandwidthLimit)
	assert.Equal(t, "default", cfg.Transfers.TransferOrder)
	assert.Empty(t, cfg.Transfers.BandwidthSchedule)

	// Safety defaults
	assert.Equal(t, 1000, cfg.Safety.BigDeleteThreshold)
	assert.Equal(t, 50, cfg.Safety.BigDeletePercentage)
	assert.Equal(t, 10, cfg.Safety.BigDeleteMinItems)
	assert.Equal(t, "1GB", cfg.Safety.MinFreeSpace)
	assert.True(t, cfg.Safety.UseRecycleBin)
	assert.True(t, cfg.Safety.UseLocalTrash)
	assert.False(t, cfg.Safety.DisableDownloadValidation)
	assert.False(t, cfg.Safety.DisableUploadValidation)
	assert.Equal(t, "0700", cfg.Safety.SyncDirPermissions)
	assert.Equal(t, "0600", cfg.Safety.SyncFilePermissions)
	assert.Equal(t, 30, cfg.Safety.TombstoneRetentionDays)

	// Sync defaults
	assert.Equal(t, "5m", cfg.Sync.PollInterval)
	assert.Equal(t, 12, cfg.Sync.FullscanFrequency)
	assert.True(t, cfg.Sync.Websocket)
	assert.Equal(t, "keep_both", cfg.Sync.ConflictStrategy)
	assert.Equal(t, "1h", cfg.Sync.ConflictReminderInterval)
	assert.False(t, cfg.Sync.DryRun)
	assert.Equal(t, "0", cfg.Sync.VerifyInterval)
	assert.Equal(t, "30s", cfg.Sync.ShutdownTimeout)

	// Logging defaults
	assert.Equal(t, "info", cfg.Logging.LogLevel)
	assert.Equal(t, "", cfg.Logging.LogFile)
	assert.Equal(t, "auto", cfg.Logging.LogFormat)
	assert.Equal(t, 30, cfg.Logging.LogRetentionDays)

	// Network defaults
	assert.Equal(t, "10s", cfg.Network.ConnectTimeout)
	assert.Equal(t, "60s", cfg.Network.DataTimeout)
	assert.Equal(t, "", cfg.Network.UserAgent)
	assert.False(t, cfg.Network.ForceHTTP11)
}

func TestDefaultConfig_PassesValidation(t *testing.T) {
	cfg := DefaultConfig()
	err := Validate(cfg)
	assert.NoError(t, err)
}
