package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCheckPausedState_NotPaused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))

	paused, pausedUntil := checkPausedState(cfgPath, cid, logger)
	assert.False(t, paused)
	assert.Empty(t, pausedUntil)
}

func TestCheckPausedState_Paused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	paused, pausedUntil := checkPausedState(cfgPath, cid, logger)
	assert.True(t, paused)
	assert.Empty(t, pausedUntil)
}

func TestCheckPausedState_TimedPause(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2099-01-01T00:00:00Z"))

	paused, pausedUntil := checkPausedState(cfgPath, cid, logger)
	assert.True(t, paused)
	assert.Equal(t, "2099-01-01T00:00:00Z", pausedUntil)
}

func TestCheckPausedState_MissingConfig(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	cid := driveid.MustCanonicalID("personal:test@example.com")

	// Non-existent config path — should default to not paused.
	paused, _ := checkPausedState("/nonexistent/config.toml", cid, logger)
	assert.False(t, paused)
}

func TestCheckPausedState_DriveNotInConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	otherCID := driveid.MustCanonicalID("personal:other@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))

	// Query for a drive that doesn't exist in config.
	paused, _ := checkPausedState(cfgPath, otherCID, logger)
	assert.False(t, paused)
}

func TestWaitForResume_SIGHUPUnblocks(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	cid := driveid.MustCanonicalID("personal:test@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")

	sighup := make(chan os.Signal, 1)
	ctx := context.Background()

	// Send SIGHUP asynchronously.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sighup <- os.Signal(nil) // Any value unblocks the channel.
	}()

	err := waitForResume(ctx, sighup, cfgPath, cid, "", logger)
	assert.NoError(t, err)
}

func TestWaitForResume_ContextCancellation(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	cid := driveid.MustCanonicalID("personal:test@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")

	sighup := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context asynchronously.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := waitForResume(ctx, sighup, cfgPath, cid, "", logger)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitForResume_ExpiredTimerReturnsImmediately(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2000-01-01T00:00:00Z"))

	sighup := make(chan os.Signal, 1)
	ctx := context.Background()

	// Already expired — should return immediately and clear config.
	start := time.Now()
	err := waitForResume(ctx, sighup, cfgPath, cid, "2000-01-01T00:00:00Z", logger)
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Less(t, elapsed, 1*time.Second)

	// Config should have paused keys cleared.
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused)
	assert.Nil(t, d.PausedUntil)
}

func TestWaitForResume_TimerExpiry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	sighup := make(chan os.Signal, 1)
	ctx := context.Background()

	// Set a very short timer (100ms in the future).
	until := time.Now().Add(100 * time.Millisecond).Format(time.RFC3339)

	err := waitForResume(ctx, sighup, cfgPath, cid, until, logger)
	assert.NoError(t, err)
}

func TestDaemonClearPausedKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2099-01-01T00:00:00Z"))

	daemonClearPausedKeys(cfgPath, cid, logger)

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused)
	assert.Nil(t, d.PausedUntil)
}
