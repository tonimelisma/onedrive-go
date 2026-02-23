package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Default remote path when none is specified.
const defaultRemotePath = "/"

// ResolvedDrive contains drive fields plus effective config sections after
// merging global defaults with per-drive overrides and CLI/env flags. This
// is the final product consumed by the CLI and sync engine.
type ResolvedDrive struct {
	CanonicalID driveid.CanonicalID
	Alias       string
	Enabled     bool
	SyncDir     string // absolute path after tilde expansion
	RemotePath  string
	DriveID     driveid.ID

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
// When no drives are configured and no selector is given, falls back to
// token discovery on disk (see DiscoverTokens).
func matchDrive(cfg *Config, selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	if len(cfg.Drives) == 0 {
		// If the selector looks like a canonical ID (contains ":"), allow
		// zero-config usage. This supports CLI-only workflows where --drive
		// provides a canonical ID and no config file exists.
		if strings.Contains(selector, ":") {
			logger.Debug("zero-config mode: using selector as canonical ID", "selector", selector)

			cid, err := driveid.NewCanonicalID(selector)
			if err != nil {
				return driveid.CanonicalID{}, Drive{}, fmt.Errorf("invalid drive selector: %w", err)
			}

			return cid, Drive{}, nil
		}

		// Non-canonical selector with no drives — can't match against anything.
		if selector != "" {
			return driveid.CanonicalID{}, Drive{}, fmt.Errorf("no drives configured — run 'onedrive-go login' to get started")
		}

		// No selector and no config — discover tokens on disk.
		return matchDiscoveredTokens(logger)
	}

	if selector == "" {
		return matchSingleDrive(cfg, logger)
	}

	return matchBySelector(cfg, selector, logger)
}

// matchSingleDrive auto-selects when exactly one drive is configured.
func matchSingleDrive(cfg *Config, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	if len(cfg.Drives) == 1 {
		for id := range cfg.Drives {
			logger.Debug("auto-selected single drive", "canonical_id", id.String())

			return id, cfg.Drives[id], nil
		}
	}

	return driveid.CanonicalID{}, Drive{}, fmt.Errorf("multiple drives configured — specify with --drive")
}

// matchBySelector finds a drive by exact ID, alias, or partial substring match.
func matchBySelector(cfg *Config, selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	// Exact canonical ID match — try parsing the selector as a CanonicalID
	// and looking it up directly in the typed map.
	if selectorCID, err := driveid.NewCanonicalID(selector); err == nil {
		if d, ok := cfg.Drives[selectorCID]; ok {
			logger.Debug("drive matched by exact canonical ID", "canonical_id", selector)

			return selectorCID, d, nil
		}
	}

	// Alias match
	for id := range cfg.Drives {
		if cfg.Drives[id].Alias == selector {
			logger.Debug("drive matched by alias", "alias", selector, "canonical_id", id.String())

			return id, cfg.Drives[id], nil
		}
	}

	return matchPartial(cfg, selector, logger)
}

// matchPartial finds drives whose canonical ID contains the selector as a substring.
func matchPartial(cfg *Config, selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	var matches []driveid.CanonicalID

	for id := range cfg.Drives {
		if strings.Contains(id.String(), selector) {
			matches = append(matches, id)
		}
	}

	if len(matches) == 1 {
		logger.Debug("drive matched by partial substring", "selector", selector, "canonical_id", matches[0].String())

		return matches[0], cfg.Drives[matches[0]], nil
	}

	if len(matches) > 1 {
		strs := make([]string, 0, len(matches))
		for _, m := range matches {
			strs = append(strs, m.String())
		}

		slices.Sort(strs)

		return driveid.CanonicalID{}, Drive{}, fmt.Errorf("ambiguous drive selector %q matches: %s",
			selector, strings.Join(strs, ", "))
	}

	return driveid.CanonicalID{}, Drive{}, fmt.Errorf("no drive matching %q", selector)
}

// buildResolvedDrive creates a ResolvedDrive by starting with global config
// values and applying per-drive overrides for fields that the drive specifies.
func buildResolvedDrive(cfg *Config, canonicalID driveid.CanonicalID, drive *Drive, logger *slog.Logger) *ResolvedDrive {
	resolved := &ResolvedDrive{
		CanonicalID:     canonicalID,
		Alias:           drive.Alias,
		Enabled:         drive.Enabled == nil || *drive.Enabled, // default true
		SyncDir:         expandTilde(drive.SyncDir),
		RemotePath:      drive.RemotePath,
		DriveID:         driveid.New(drive.DriveID),
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

// matchDiscoveredTokens auto-selects a drive from token files found on disk.
// This enables the zero-config experience: login → ls works without a config file.
func matchDiscoveredTokens(logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	tokens := DiscoverTokens(logger)

	switch len(tokens) {
	case 0:
		return driveid.CanonicalID{}, Drive{}, fmt.Errorf("no accounts — run 'onedrive-go login' to get started")
	case 1:
		logger.Debug("auto-selected single discovered token", "canonical_id", tokens[0].String())

		return tokens[0], Drive{}, nil
	default:
		strs := make([]string, 0, len(tokens))
		for _, t := range tokens {
			strs = append(strs, t.String())
		}

		return driveid.CanonicalID{}, Drive{}, fmt.Errorf(
			"multiple accounts found — specify with --drive:\n  %s",
			strings.Join(strs, "\n  "))
	}
}

// DiscoverTokens lists token files in the default data directory and returns
// canonical drive IDs extracted from filenames. Token files follow the naming
// convention: token_{type}_{email}.json (e.g., token_personal_user@example.com.json).
// This enables zero-config drive discovery when no config file exists.
func DiscoverTokens(logger *slog.Logger) []driveid.CanonicalID {
	return discoverTokensIn(DefaultDataDir(), logger)
}

// discoverTokensIn scans dir for token files and extracts canonical IDs.
// Files that don't match the token naming convention are silently skipped.
func discoverTokensIn(dir string, logger *slog.Logger) []driveid.CanonicalID {
	if dir == "" {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Debug("cannot read data directory for token discovery", "dir", dir, "error", err)

		return nil
	}

	var ids []driveid.CanonicalID

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		if !strings.HasPrefix(name, "token_") || !strings.HasSuffix(name, ".json") {
			continue
		}

		// Strip "token_" prefix and ".json" suffix, then split on first "_"
		// to recover {type}:{email}. Emails may contain underscores, so only
		// the first underscore separates type from email.
		inner := strings.TrimPrefix(name, "token_")
		inner = strings.TrimSuffix(inner, ".json")

		parts := strings.SplitN(inner, "_", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			logger.Debug("skipping malformed token filename", "name", name)

			continue
		}

		cid, err := driveid.Construct(parts[0], parts[1])
		if err != nil {
			logger.Debug("skipping token with invalid drive type", "name", name, "error", err)

			continue
		}

		ids = append(ids, cid)
	}

	slices.SortFunc(ids, func(a, b driveid.CanonicalID) int {
		return cmp.Compare(a.String(), b.String())
	})
	logger.Debug("token discovery complete", "dir", dir, "count", len(ids))

	return ids
}

// DriveTokenPath returns the token file path for a canonical drive ID.
// SharePoint drives share the business account's token since they use the
// same OAuth session. For example:
//
//	"personal:toni@outlook.com" -> "{dataDir}/token_personal_toni@outlook.com.json"
//	"sharepoint:alice@contoso.com:marketing:Docs" -> "{dataDir}/token_business_alice@contoso.com.json"
func DriveTokenPath(canonicalID driveid.CanonicalID) string {
	dataDir := DefaultDataDir()
	if dataDir == "" || canonicalID.IsZero() {
		return ""
	}

	// TokenCanonicalID() maps SharePoint → business (shared OAuth session).
	tokenCID := canonicalID.TokenCanonicalID()
	sanitized := tokenCID.DriveType() + "_" + tokenCID.Email()

	return filepath.Join(dataDir, "token_"+sanitized+".json")
}

// DriveStatePath returns the state DB path for a canonical drive ID.
// Each drive gets its own state database. The ":" separator in canonical
// IDs is replaced with "_" for filesystem safety.
//
//	"personal:toni@outlook.com" -> "{dataDir}/state_personal_toni@outlook.com.db"
//	"sharepoint:alice@contoso.com:marketing:Docs" -> "{dataDir}/state_sharepoint_alice@contoso.com_marketing_Docs.db"
func DriveStatePath(canonicalID driveid.CanonicalID) string {
	dataDir := DefaultDataDir()
	if dataDir == "" || canonicalID.IsZero() {
		return ""
	}

	sanitized := strings.ReplaceAll(canonicalID.String(), ":", "_")

	return filepath.Join(dataDir, "state_"+sanitized+".db")
}
