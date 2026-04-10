package config

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig_AllFieldsPopulated(t *testing.T) {
	cfg := DefaultConfig()
	require.NotNil(t, cfg)

	// Filter defaults (using promoted field access)
	assert.Equal(t, ".odignore", cfg.IgnoreMarker)
	assert.False(t, cfg.SkipDotfiles)
	assert.False(t, cfg.SkipSymlinks)
	assert.Empty(t, cfg.SkipFiles)
	assert.Empty(t, cfg.SkipDirs)
	assert.Empty(t, cfg.SyncPaths)

	// Transfers defaults
	assert.Equal(t, 8, cfg.TransferWorkers)
	assert.Equal(t, 4, cfg.CheckWorkers)

	// Safety defaults
	assert.Equal(t, 1000, cfg.DeleteSafetyThreshold)
	assert.Equal(t, "1GB", cfg.MinFreeSpace)
	assert.Equal(t, runtime.GOOS == "darwin", cfg.UseLocalTrash) // platform-specific default

	// Sync defaults
	assert.Equal(t, "5m", cfg.PollInterval)
	assert.False(t, cfg.Websocket)
	assert.False(t, cfg.DryRun)
	assert.Equal(t, "5m", cfg.SafetyScanInterval)

	// Logging defaults
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Empty(t, cfg.LogFile)
	assert.Equal(t, "auto", cfg.LogFormat)
	assert.Equal(t, 30, cfg.LogRetentionDays)

	// Drives map initialized
	require.NotNil(t, cfg.Drives)
	assert.Empty(t, cfg.Drives)
}

func TestDefaultConfig_PassesValidation(t *testing.T) {
	cfg := DefaultConfig()
	err := Validate(cfg)
	assert.NoError(t, err)
}

// --- IsPaused ---

// Validates: R-2.6.1
func TestDrive_IsPaused(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	future := now.Add(1 * time.Hour).Format(time.RFC3339)
	past := now.Add(-1 * time.Hour).Format(time.RFC3339)
	garbage := "not-a-timestamp"

	boolTrue := true
	boolFalse := false

	tests := []struct {
		name   string
		drive  Drive
		expect bool
	}{
		{
			name:   "nil Paused → not paused",
			drive:  Drive{},
			expect: false,
		},
		{
			name:   "explicit false → not paused",
			drive:  Drive{Paused: &boolFalse},
			expect: false,
		},
		{
			name:   "indefinite pause (no PausedUntil) → paused",
			drive:  Drive{Paused: &boolTrue},
			expect: true,
		},
		{
			name:   "timed pause still active (future) → paused",
			drive:  Drive{Paused: &boolTrue, PausedUntil: &future},
			expect: true,
		},
		{
			name:   "timed pause expired (past) → not paused",
			drive:  Drive{Paused: &boolTrue, PausedUntil: &past},
			expect: false,
		},
		{
			name:   "unparseable PausedUntil → indefinite (safe default)",
			drive:  Drive{Paused: &boolTrue, PausedUntil: &garbage},
			expect: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expect, tc.drive.IsPaused(now))
		})
	}
}

func TestConfig_EmbeddedStructPromotion(t *testing.T) {
	// Verify that embedded struct fields are accessible directly on Config.
	cfg := DefaultConfig()

	// These should compile and work because of struct embedding.
	assert.False(t, cfg.SkipDotfiles)
	assert.Equal(t, 8, cfg.TransferWorkers)
	assert.Equal(t, 1000, cfg.DeleteSafetyThreshold)
	assert.Equal(t, "5m", cfg.PollInterval)
	assert.Equal(t, "info", cfg.LogLevel)
}
