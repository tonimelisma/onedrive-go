package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
)

// Platform identifiers.
const (
	platformLinux  = "linux"
	platformDarwin = "darwin"
)

// Application directory name used across all platforms.
const appName = "onedrive-go"

// Config file name.
const configFileName = "config.toml"

// Unix socket paths are bounded by sockaddr_un.sun_path. macOS is the tighter
// platform in practice, so long isolated XDG roots use a short hash-based
// runtime path instead of failing bind(2) with EINVAL.
const unixSocketPathSoftLimit = 100

// DefaultConfigDir returns the platform-specific directory for config files.
// XDG_CONFIG_HOME is checked first on ALL platforms (enables test isolation).
// Fallbacks: Linux ~/.config, macOS ~/Library/Application Support, other ~/.config.
func DefaultConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case platformDarwin:
		return filepath.Join(home, "Library", "Application Support", appName)
	default:
		return filepath.Join(home, ".config", appName)
	}
}

// DefaultDataDir returns the platform-specific directory for application data
// (state databases, logs, tokens).
// XDG_DATA_HOME is checked first on ALL platforms (enables test isolation).
// Fallbacks: Linux ~/.local/share, macOS ~/Library/Application Support, other ~/.local/share.
func DefaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case platformDarwin:
		return filepath.Join(home, "Library", "Application Support", appName)
	default:
		return filepath.Join(home, ".local", "share", appName)
	}
}

// DefaultCacheDir returns the platform-specific directory for cache files.
// XDG_CACHE_HOME is checked first on ALL platforms (enables test isolation).
// Fallbacks: Linux ~/.cache, macOS ~/Library/Caches, other ~/.cache.
func DefaultCacheDir() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case platformDarwin:
		return filepath.Join(home, "Library", "Caches", appName)
	default:
		return filepath.Join(home, ".cache", appName)
	}
}

// AssertDevSafe panics if a dev build (version=="dev") is running without
// XDG isolation. Prevents accidental production data access during development.
// At least one of XDG_DATA_HOME, XDG_CONFIG_HOME, or XDG_CACHE_HOME must be set.
func AssertDevSafe() {
	if os.Getenv("XDG_DATA_HOME") != "" ||
		os.Getenv("XDG_CONFIG_HOME") != "" ||
		os.Getenv("XDG_CACHE_HOME") != "" {
		return
	}

	panic("DEV BUILD: set XDG_DATA_HOME, XDG_CONFIG_HOME, and XDG_CACHE_HOME " +
		"to avoid touching production data.\n" +
		"Example: source scripts/dev-env.sh && go run . <command>")
}

// UploadSessionDir returns the directory for persisted upload session files.
// These are JSON files containing pre-authenticated upload URLs, stored with
// 0700 directory permissions for security.
func UploadSessionDir() string {
	dir := DefaultDataDir()
	if dir == "" {
		return ""
	}

	return filepath.Join(dir, "upload-sessions")
}

// ControlSocketPath returns the Unix-domain control socket path. The socket is
// the single local IPC boundary for daemon status, reload, and durable user
// intent requests.
func ControlSocketPath() string {
	dir := DefaultDataDir()
	if dir == "" {
		return ""
	}

	return controlSocketPathForDataDir(dir)
}

func controlSocketPathForDataDir(dir string) string {
	candidate := filepath.Join(dir, "control.sock")
	if len(candidate) <= unixSocketPathSoftLimit {
		return candidate
	}

	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(os.TempDir(), "odgo-"+hex.EncodeToString(sum[:])[:16], "control.sock")
}

// DefaultConfigPath returns the full path to the default config file.
// This is used as the fallback when neither ONEDRIVE_GO_CONFIG nor
// --config is specified.
func DefaultConfigPath() string {
	dir := DefaultConfigDir()
	if dir == "" {
		return ""
	}

	return filepath.Join(dir, configFileName)
}
