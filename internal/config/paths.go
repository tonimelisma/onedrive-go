package config

import (
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

// PIDFilePath returns the path to the daemon PID file. The PID file is used
// to prevent multiple sync --watch daemons and to deliver SIGHUP for config
// reload. Located in the data directory alongside state DBs and tokens.
func PIDFilePath() string {
	dir := DefaultDataDir()
	if dir == "" {
		return ""
	}

	return filepath.Join(dir, "daemon.pid")
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
