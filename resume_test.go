package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

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
	err := config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive")
	require.NoError(t, err)

	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2026-03-01T00:00:00Z"))

	// Verify keys exist before clearing.
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "paused = true")
	assert.Contains(t, string(data), "paused_until")

	// Clear the keys.
	require.NoError(t, clearPausedKeys(cfgPath, cid))

	// Verify keys are removed.
	data, err = os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "paused")
	assert.NotContains(t, string(data), "paused_until")
}

func TestClearPausedKeys_IdempotentWhenNoKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")

	err := config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive")
	require.NoError(t, err)

	// Clearing on an unpaused drive should succeed (keys don't exist).
	require.NoError(t, clearPausedKeys(cfgPath, cid))
}
