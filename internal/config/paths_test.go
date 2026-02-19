package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testHome = "/home/testuser"

func TestDefaultConfigDir_NonEmpty(t *testing.T) {
	dir := DefaultConfigDir()
	assert.NotEmpty(t, dir)
	assert.True(t, strings.Contains(dir, appName))
}

func TestDefaultDataDir_NonEmpty(t *testing.T) {
	dir := DefaultDataDir()
	assert.NotEmpty(t, dir)
	assert.True(t, strings.Contains(dir, appName))
}

func TestDefaultCacheDir_NonEmpty(t *testing.T) {
	dir := DefaultCacheDir()
	assert.NotEmpty(t, dir)
	assert.True(t, strings.Contains(dir, appName))
}

func TestDefaultConfigPath_EndsWithConfigToml(t *testing.T) {
	path := DefaultConfigPath()
	assert.NotEmpty(t, path)
	assert.True(t, strings.HasSuffix(path, "config.toml"))
}

func TestDefaultConfigDir_MacOS(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	dir := DefaultConfigDir()
	assert.Contains(t, dir, "Library/Application Support")
}

func TestDefaultDataDir_MacOS(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	dir := DefaultDataDir()
	assert.Contains(t, dir, "Library/Application Support")
}

func TestDefaultCacheDir_MacOS(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	dir := DefaultCacheDir()
	assert.Contains(t, dir, "Library/Caches")
}

func TestLinuxConfigDir_XDGOverride(t *testing.T) {
	xdgDir := "/custom/config"

	t.Setenv("XDG_CONFIG_HOME", xdgDir)
	result := linuxConfigDir(testHome)
	assert.Equal(t, filepath.Join(xdgDir, appName), result)
}

func TestLinuxConfigDir_DefaultFallback(t *testing.T) {
	// Unset XDG_CONFIG_HOME to test fallback
	t.Setenv("XDG_CONFIG_HOME", "")
	os.Unsetenv("XDG_CONFIG_HOME")
	result := linuxConfigDir(testHome)
	assert.Equal(t, filepath.Join(testHome, ".config", appName), result)
}

func TestLinuxDataDir_XDGOverride(t *testing.T) {
	xdgDir := "/custom/data"

	t.Setenv("XDG_DATA_HOME", xdgDir)
	result := linuxDataDir(testHome)
	assert.Equal(t, filepath.Join(xdgDir, appName), result)
}

func TestLinuxDataDir_DefaultFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	os.Unsetenv("XDG_DATA_HOME")
	result := linuxDataDir(testHome)
	assert.Equal(t, filepath.Join(testHome, ".local", "share", appName), result)
}

func TestLinuxCacheDir_XDGOverride(t *testing.T) {
	xdgDir := "/custom/cache"

	t.Setenv("XDG_CACHE_HOME", xdgDir)
	result := linuxCacheDir(testHome)
	assert.Equal(t, filepath.Join(xdgDir, appName), result)
}

func TestLinuxCacheDir_DefaultFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	os.Unsetenv("XDG_CACHE_HOME")
	result := linuxCacheDir(testHome)
	assert.Equal(t, filepath.Join(testHome, ".cache", appName), result)
}
