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
// On Linux, respects XDG_CONFIG_HOME (defaults to ~/.config/onedrive-go).
// On macOS, uses ~/Library/Application Support/onedrive-go.
func DefaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case platformLinux:
		return linuxConfigDir(home)
	case platformDarwin:
		return filepath.Join(home, "Library", "Application Support", appName)
	default:
		return filepath.Join(home, ".config", appName)
	}
}

// linuxConfigDir returns the XDG-compliant config directory for Linux.
func linuxConfigDir(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}

	return filepath.Join(home, ".config", appName)
}

// DefaultDataDir returns the platform-specific directory for application data
// (state databases, logs, tokens).
// On Linux, respects XDG_DATA_HOME (defaults to ~/.local/share/onedrive-go).
// On macOS, uses ~/Library/Application Support/onedrive-go.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case platformLinux:
		return linuxDataDir(home)
	case platformDarwin:
		return filepath.Join(home, "Library", "Application Support", appName)
	default:
		return filepath.Join(home, ".local", "share", appName)
	}
}

// linuxDataDir returns the XDG-compliant data directory for Linux.
func linuxDataDir(home string) string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}

	return filepath.Join(home, ".local", "share", appName)
}

// DefaultCacheDir returns the platform-specific directory for cache files.
// On Linux, respects XDG_CACHE_HOME (defaults to ~/.cache/onedrive-go).
// On macOS, uses ~/Library/Caches/onedrive-go.
func DefaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case platformLinux:
		return linuxCacheDir(home)
	case platformDarwin:
		return filepath.Join(home, "Library", "Caches", appName)
	default:
		return filepath.Join(home, ".cache", appName)
	}
}

// linuxCacheDir returns the XDG-compliant cache directory for Linux.
func linuxCacheDir(home string) string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}

	return filepath.Join(home, ".cache", appName)
}

// DefaultConfigPath returns the full path to the default config file.
func DefaultConfigPath() string {
	dir := DefaultConfigDir()
	if dir == "" {
		return ""
	}

	return filepath.Join(dir, configFileName)
}
