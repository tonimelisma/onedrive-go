package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigDir_NonEmpty(t *testing.T) {
	dir := DefaultConfigDir()
	assert.NotEmpty(t, dir)
	assert.Contains(t, dir, appName)
}

func TestDefaultDataDir_NonEmpty(t *testing.T) {
	dir := DefaultDataDir()
	assert.NotEmpty(t, dir)
	assert.Contains(t, dir, appName)
}

func TestDefaultCacheDir_NonEmpty(t *testing.T) {
	dir := DefaultCacheDir()
	assert.NotEmpty(t, dir)
	assert.Contains(t, dir, appName)
}

func TestDefaultConfigPath_EndsWithConfigToml(t *testing.T) {
	path := DefaultConfigPath()
	assert.NotEmpty(t, path)
	assert.True(t, strings.HasSuffix(path, "config.toml"))
}

// Validates: R-4.1.2
func TestDefaultConfigDir_MacOS(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	// Unset XDG to test platform fallback.
	t.Setenv("XDG_CONFIG_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_CONFIG_HOME"))

	dir := DefaultConfigDir()
	assert.Contains(t, dir, "Library/Application Support")
}

// Validates: R-4.1.2
func TestDefaultDataDir_MacOS(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	t.Setenv("XDG_DATA_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_DATA_HOME"))

	dir := DefaultDataDir()
	assert.Contains(t, dir, "Library/Application Support")
}

// Validates: R-4.1.2
func TestDefaultCacheDir_MacOS(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	t.Setenv("XDG_CACHE_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_CACHE_HOME"))

	dir := DefaultCacheDir()
	assert.Contains(t, dir, "Library/Caches")
}

// XDG override tests: work on ALL platforms (the whole point of the change).

// Validates: R-4.1.2
func TestDefaultConfigDir_XDGOverride(t *testing.T) {
	xdgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	result := DefaultConfigDir()
	assert.Equal(t, filepath.Join(xdgDir, appName), result)
}

func TestDefaultConfigDir_XDGFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_CONFIG_HOME"))

	result := DefaultConfigDir()
	assert.NotEmpty(t, result)
	assert.Contains(t, result, appName)
}

// Validates: R-4.1.2
func TestDefaultDataDir_XDGOverride(t *testing.T) {
	xdgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgDir)

	result := DefaultDataDir()
	assert.Equal(t, filepath.Join(xdgDir, appName), result)
}

func TestDefaultDataDir_XDGFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_DATA_HOME"))

	result := DefaultDataDir()
	assert.NotEmpty(t, result)
	assert.Contains(t, result, appName)
}

func TestControlSocketPath_UsesDataDirWhenShortEnough(t *testing.T) {
	xdgDir := filepath.Join(os.TempDir(), "odgo-short")
	t.Setenv("XDG_DATA_HOME", xdgDir)

	expected := filepath.Join(xdgDir, appName, "control.sock")
	require.LessOrEqual(t, len(expected), unixSocketPathSoftLimit)
	assert.Equal(t, expected, ControlSocketPath())
}

func TestControlSocketPath_UsesShortRuntimePathWhenDataDirIsTooLong(t *testing.T) {
	longDir := filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8))
	t.Setenv("XDG_DATA_HOME", longDir)

	path := ControlSocketPath()
	assert.NotEqual(t, filepath.Join(longDir, appName, "control.sock"), path)
	assert.LessOrEqual(t, len(path), unixSocketPathSoftLimit)
	assert.Contains(t, path, "odgo-")
	assert.True(t, strings.HasSuffix(path, "control.sock"))
	assert.Equal(t, path, ControlSocketPath(), "hashed socket path should be stable within a data dir")
}

// Validates: R-4.1.2
func TestDefaultCacheDir_XDGOverride(t *testing.T) {
	xdgDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdgDir)

	result := DefaultCacheDir()
	assert.Equal(t, filepath.Join(xdgDir, appName), result)
}

func TestDefaultCacheDir_XDGFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_CACHE_HOME"))

	result := DefaultCacheDir()
	assert.NotEmpty(t, result)
	assert.Contains(t, result, appName)
}

func TestAssertDevSafe_PanicsWithoutXDG(t *testing.T) {
	// t.Setenv("") sets to empty string (not unset); AssertDevSafe treats "" as unset.
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	assert.Panics(t, func() {
		AssertDevSafe()
	})
}

func TestAssertDevSafe_PassesWithXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/test-data")

	assert.NotPanics(t, func() {
		AssertDevSafe()
	})
}
