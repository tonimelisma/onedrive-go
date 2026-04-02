package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Validates: R-2.6
func TestNewResumeCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newResumeCmd()
	assert.Equal(t, "resume", cmd.Use)
	assert.Equal(t, "true", cmd.Annotations[skipConfigAnnotation])
}

func TestClearPausedKeys_RemovesBothKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")

	// Create config with paused drive.
	err := config.AppendDriveSection(cfgPath, cid, "~/OneDrive")
	require.NoError(t, err)

	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2026-03-01T00:00:00Z"))

	// Verify keys exist before clearing.
	data, err := localpath.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "paused = true")
	assert.Contains(t, string(data), "paused_until")

	// Clear the keys.
	require.NoError(t, clearPausedKeys(cfgPath, cid))

	// Verify keys are removed.
	data, err = localpath.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "paused")
	assert.NotContains(t, string(data), "paused_until")
}

// Validates: R-2.6
func TestClearPausedKeys_ExpiredTimedPause_ClearedByResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:expired@example.com")

	// Create config with an expired timed pause (paused=true + past paused_until).
	err := config.AppendDriveSection(cfgPath, cid, "~/OneDrive")
	require.NoError(t, err)
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2000-01-01T00:00:00Z"))

	// Verify IsPaused returns false (expired), but raw keys are present.
	cfg, err := config.LoadOrDefault(cfgPath, slog.Default())
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.False(t, d.IsPaused(time.Now()), "expired timed pause should not be considered paused")
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused, "raw paused flag should still be true")

	// Run resumeSingleDrive — should clean up stale keys.
	var statusBuf strings.Builder
	cc := &CLIContext{
		CfgPath:      cfgPath,
		Logger:       slog.Default(),
		StatusWriter: &statusBuf,
	}

	require.NoError(t, resumeSingleDrive(cc, cfg, cid.String()))

	// Verify stale keys were removed from config file.
	data, err := localpath.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "paused")
	assert.NotContains(t, string(data), "paused_until")

	// Verify status message.
	assert.Contains(t, statusBuf.String(), "expired timed pause cleared")
}

func TestClearPausedKeys_IdempotentWhenNoKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")

	err := config.AppendDriveSection(cfgPath, cid, "~/OneDrive")
	require.NoError(t, err)

	// Clearing on an unpaused drive should succeed (keys don't exist).
	require.NoError(t, clearPausedKeys(cfgPath, cid))
}
