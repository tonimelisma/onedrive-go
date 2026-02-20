package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Default remote path when none is specified.
const defaultRemotePath = "/"

// driveTypeParts is the minimum number of colon-separated parts in a
// canonical drive ID (type:email).
const driveTypeParts = 2

// ResolvedDrive contains drive fields plus effective config sections after
// merging global defaults with per-drive overrides and CLI/env flags. This
// is the final product consumed by the CLI and sync engine.
type ResolvedDrive struct {
	CanonicalID string
	Alias       string
	Enabled     bool
	SyncDir     string // absolute path after tilde expansion
	RemotePath  string
	DriveID     string

	FilterConfig
	TransfersConfig
	SafetyConfig
	SyncConfig
	LoggingConfig
	NetworkConfig
}

// matchDrive selects a drive from the config by selector string. The matching
// precedence is: exact canonical ID > alias > partial canonical ID substring.
// If selector is empty, auto-selects when exactly one drive is configured.
func matchDrive(cfg *Config, selector string, logger *slog.Logger) (string, Drive, error) {
	if len(cfg.Drives) == 0 {
		// If the selector looks like a canonical ID (contains ":"), allow
		// zero-config usage. This supports CLI-only workflows where --drive
		// provides a canonical ID and no config file exists.
		if strings.Contains(selector, ":") {
			logger.Debug("zero-config mode: using selector as canonical ID", "selector", selector)

			return selector, Drive{}, nil
		}

		return "", Drive{}, fmt.Errorf("no drives configured — run 'onedrive-go login' to get started")
	}

	if selector == "" {
		return matchSingleDrive(cfg, logger)
	}

	return matchBySelector(cfg, selector, logger)
}

// matchSingleDrive auto-selects when exactly one drive is configured.
func matchSingleDrive(cfg *Config, logger *slog.Logger) (string, Drive, error) {
	if len(cfg.Drives) == 1 {
		for id := range cfg.Drives {
			logger.Debug("auto-selected single drive", "canonical_id", id)

			return id, cfg.Drives[id], nil
		}
	}

	return "", Drive{}, fmt.Errorf("multiple drives configured — specify with --drive")
}

// matchBySelector finds a drive by exact ID, alias, or partial substring match.
func matchBySelector(cfg *Config, selector string, logger *slog.Logger) (string, Drive, error) {
	// Exact canonical ID match
	if d, ok := cfg.Drives[selector]; ok {
		logger.Debug("drive matched by exact canonical ID", "canonical_id", selector)

		return selector, d, nil
	}

	// Alias match
	for id := range cfg.Drives {
		if cfg.Drives[id].Alias == selector {
			logger.Debug("drive matched by alias", "alias", selector, "canonical_id", id)

			return id, cfg.Drives[id], nil
		}
	}

	return matchPartial(cfg, selector, logger)
}

// matchPartial finds drives whose canonical ID contains the selector as a substring.
func matchPartial(cfg *Config, selector string, logger *slog.Logger) (string, Drive, error) {
	var matches []string

	for id := range cfg.Drives {
		if strings.Contains(id, selector) {
			matches = append(matches, id)
		}
	}

	if len(matches) == 1 {
		logger.Debug("drive matched by partial substring", "selector", selector, "canonical_id", matches[0])

		return matches[0], cfg.Drives[matches[0]], nil
	}

	if len(matches) > 1 {
		sort.Strings(matches)

		return "", Drive{}, fmt.Errorf("ambiguous drive selector %q matches: %s",
			selector, strings.Join(matches, ", "))
	}

	return "", Drive{}, fmt.Errorf("no drive matching %q", selector)
}

// buildResolvedDrive creates a ResolvedDrive by starting with global config
// values and applying per-drive overrides for fields that the drive specifies.
func buildResolvedDrive(cfg *Config, canonicalID string, drive *Drive, logger *slog.Logger) *ResolvedDrive {
	resolved := &ResolvedDrive{
		CanonicalID:     canonicalID,
		Alias:           drive.Alias,
		Enabled:         drive.Enabled == nil || *drive.Enabled, // default true
		SyncDir:         expandTilde(drive.SyncDir),
		RemotePath:      drive.RemotePath,
		DriveID:         drive.DriveID,
		FilterConfig:    cfg.FilterConfig,
		TransfersConfig: cfg.TransfersConfig,
		SafetyConfig:    cfg.SafetyConfig,
		SyncConfig:      cfg.SyncConfig,
		LoggingConfig:   cfg.LoggingConfig,
		NetworkConfig:   cfg.NetworkConfig,
	}

	if resolved.RemotePath == "" {
		resolved.RemotePath = defaultRemotePath
	}

	applyDriveOverrides(resolved, drive, logger)

	return resolved
}

// applyDriveOverrides selectively replaces global config values with per-drive
// values for fields that the drive explicitly sets.
func applyDriveOverrides(resolved *ResolvedDrive, drive *Drive, logger *slog.Logger) {
	if drive.SkipDotfiles != nil {
		resolved.SkipDotfiles = *drive.SkipDotfiles
		logger.Debug("per-drive override applied", "field", "skip_dotfiles", "value", *drive.SkipDotfiles)
	}

	if drive.SkipDirs != nil {
		resolved.SkipDirs = drive.SkipDirs
		logger.Debug("per-drive override applied", "field", "skip_dirs", "count", len(drive.SkipDirs))
	}

	if drive.SkipFiles != nil {
		resolved.SkipFiles = drive.SkipFiles
		logger.Debug("per-drive override applied", "field", "skip_files", "count", len(drive.SkipFiles))
	}

	if drive.PollInterval != "" {
		resolved.PollInterval = drive.PollInterval
		logger.Debug("per-drive override applied", "field", "poll_interval", "value", drive.PollInterval)
	}
}

// expandTilde replaces a leading "~/" with the user's home directory.
// If os.UserHomeDir() fails, the path is returned unexpanded. This is safe
// because ValidateResolved() catches invalid sync_dir paths downstream and
// will report a clear error to the user.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, path[2:])
}

// DriveTokenPath returns the token file path for a canonical drive ID.
// SharePoint drives share the business account's token since they use the
// same OAuth session. For example:
//
//	"personal:toni@outlook.com" -> "{dataDir}/token_personal_toni@outlook.com.json"
//	"sharepoint:alice@contoso.com:marketing:Docs" -> "{dataDir}/token_business_alice@contoso.com.json"
func DriveTokenPath(canonicalID string) string {
	dataDir := DefaultDataDir()
	if dataDir == "" {
		return ""
	}

	parts := strings.SplitN(canonicalID, ":", driveTypeParts+1)
	if len(parts) < driveTypeParts {
		return ""
	}

	driveType := parts[0]
	email := parts[1]

	// SharePoint drives share the business token (same user, same OAuth session).
	if driveType == "sharepoint" {
		driveType = "business"
	}

	sanitized := driveType + "_" + email

	return filepath.Join(dataDir, "token_"+sanitized+".json")
}

// DriveStatePath returns the state DB path for a canonical drive ID.
// Each drive gets its own state database. The ":" separator in canonical
// IDs is replaced with "_" for filesystem safety.
//
//	"personal:toni@outlook.com" -> "{dataDir}/state_personal_toni@outlook.com.db"
//	"sharepoint:alice@contoso.com:marketing:Docs" -> "{dataDir}/state_sharepoint_alice@contoso.com_marketing_Docs.db"
func DriveStatePath(canonicalID string) string {
	dataDir := DefaultDataDir()
	if dataDir == "" {
		return ""
	}

	// Validate canonical ID format — must be non-empty and contain ":"
	// to match DriveTokenPath's validation behavior.
	if canonicalID == "" || !strings.Contains(canonicalID, ":") {
		return ""
	}

	sanitized := strings.ReplaceAll(canonicalID, ":", "_")

	return filepath.Join(dataDir, "state_"+sanitized+".db")
}
