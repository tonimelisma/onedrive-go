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
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
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
	StateDir    string // override for state DB directory (empty = platform default)
	RemotePath  string
	DriveID     driveid.ID

	FilterConfig
	TransfersConfig
	SafetyConfig
	SyncConfig
	LoggingConfig
	NetworkConfig
}

// StatePath returns the state DB file path for this drive. When StateDir is
// set, the DB is placed inside that directory instead of the platform default
// data directory. This allows E2E tests to use per-test temp dirs for isolation.
func (rd *ResolvedDrive) StatePath() string {
	if rd.StateDir != "" {
		sanitized := strings.ReplaceAll(rd.CanonicalID.String(), ":", "_")

		return filepath.Join(rd.StateDir, "state_"+sanitized+".db")
	}

	return DriveStatePath(rd.CanonicalID)
}

// MatchDrive selects a drive from the config by selector string. The matching
// precedence is: exact canonical ID > alias > partial canonical ID substring.
// If selector is empty, auto-selects when exactly one drive is configured.
//
// When no drives are configured, provides smart error messages: checks for
// existing tokens on disk and suggests "drive add" or "login" accordingly.
func MatchDrive(cfg *Config, selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	if len(cfg.Drives) == 0 {
		return matchNoDrives(selector, logger)
	}

	if selector == "" {
		return matchSingleDrive(cfg, logger)
	}

	return matchBySelector(cfg, selector, logger)
}

// matchNoDrives handles drive matching when no drives are configured.
// Provides context-aware error messages based on whether tokens exist on disk.
func matchNoDrives(selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	// If the selector looks like a canonical ID, allow zero-config usage
	// for CLI workflows where --drive provides a canonical ID directly.
	if strings.Contains(selector, ":") {
		logger.Debug("zero-config mode: using selector as canonical ID", "selector", selector)

		cid, err := driveid.NewCanonicalID(selector)
		if err != nil {
			return driveid.CanonicalID{}, Drive{}, fmt.Errorf("invalid drive selector: %w", err)
		}

		return cid, Drive{}, nil
	}

	// Check for tokens on disk to provide a more helpful error message.
	tokens := DiscoverTokens(logger)
	if len(tokens) > 0 {
		return driveid.CanonicalID{}, Drive{},
			fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	return driveid.CanonicalID{}, Drive{},
		fmt.Errorf("no accounts configured — run 'onedrive-go login' to get started")
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
		StateDir:        expandTilde(drive.StateDir),
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

	// Compute runtime default sync_dir when the drive has none configured.
	// Reads org_name from the token file metadata for accurate business
	// drive naming (e.g., "~/OneDrive - Contoso" instead of "~/OneDrive - Business").
	if resolved.SyncDir == "" {
		orgName, displayName := ReadTokenMetaForSyncDir(canonicalID, logger)
		otherDirs := CollectOtherSyncDirs(cfg, canonicalID, logger)
		resolved.SyncDir = expandTilde(DefaultSyncDir(canonicalID, orgName, displayName, otherDirs))
		logger.Debug("using default sync_dir",
			"sync_dir", resolved.SyncDir,
			"canonical_id", canonicalID.String(),
			"org_name", orgName,
		)
	}

	applyDriveOverrides(resolved, drive, logger)

	return resolved
}

// ReadTokenMetaForSyncDir reads org_name and display_name from the token file's
// cached metadata. Returns empty strings if the token file is missing or
// doesn't contain metadata. Uses tokenfile.ReadMeta (leaf package) to avoid
// an import cycle with graph.
func ReadTokenMetaForSyncDir(cid driveid.CanonicalID, logger *slog.Logger) (orgName, displayName string) {
	tokenPath := DriveTokenPath(cid.TokenCanonicalID())
	if tokenPath == "" {
		return "", ""
	}

	meta, err := tokenfile.ReadMeta(tokenPath)
	if err != nil {
		logger.Debug("could not read token meta for sync_dir computation",
			"canonical_id", cid.String(), "error", err)

		return "", ""
	}

	return meta["org_name"], meta["display_name"]
}

// CollectOtherSyncDirs collects sync_dir values from all drives in the config
// except the specified one. For drives without explicit sync_dir, computes
// the base name (without collision cascade) so all potential collisions are detected.
// Pass a zero CanonicalID to include all drives (no exclusion).
func CollectOtherSyncDirs(cfg *Config, excludeID driveid.CanonicalID, logger *slog.Logger) []string {
	var dirs []string

	for id := range cfg.Drives {
		if id == excludeID {
			continue
		}

		dir := cfg.Drives[id].SyncDir
		if dir == "" {
			// Compute base name for this drive (without collision cascade).
			orgName, _ := ReadTokenMetaForSyncDir(id, logger)
			dir = BaseSyncDir(id, orgName)
		}

		if dir != "" {
			dirs = append(dirs, dir)
		}
	}

	return dirs
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
// If os.UserHomeDir() fails, the path is returned unexpanded and a debug
// log is emitted. This is safe because ValidateResolved() catches invalid
// sync_dir paths downstream and will report a clear error to the user.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Debug("expandTilde: could not determine home directory", "error", err)

		return path
	}

	return filepath.Join(home, path[2:])
}

// DiscoverTokens lists token files in the default data directory and returns
// canonical drive IDs extracted from filenames. Token files follow the naming
// convention: token_{type}_{email}.json (e.g., token_personal_user@example.com.json).
// Used for smart error messages and drive list.
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

// DriveStatePathWithOverride returns the state DB path for a drive. When
// stateDir is non-empty, the DB is placed there (with tilde expansion)
// instead of the platform default (B-193).
func DriveStatePathWithOverride(canonicalID driveid.CanonicalID, stateDir string) string {
	if stateDir != "" {
		expanded := expandTilde(stateDir)
		sanitized := strings.ReplaceAll(canonicalID.String(), ":", "_")

		return filepath.Join(expanded, "state_"+sanitized+".db")
	}

	return DriveStatePath(canonicalID)
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
