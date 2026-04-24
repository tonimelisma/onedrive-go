package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// NOTE: DriveTokenPath is defined in token_resolution.go (not here).

// ResolvedDrive contains drive fields plus effective config sections after
// merging global defaults with per-drive overrides and CLI/env flags. This
// is the final product consumed by the CLI and sync engine.
type ResolvedDrive struct {
	CanonicalID            driveid.CanonicalID
	DisplayName            string
	Owner                  string // drive owner name; populated for shared drives
	Paused                 bool
	PausedUntil            string // RFC3339 timestamp; empty when not timed
	SyncDir                string // absolute path after tilde expansion
	DriveID                driveid.ID
	RootItemID             string // configured remote root item for shared-root drives; empty = drive root
	SharedRootDeltaCapable bool   // true when folder delta is supported for the configured shared root

	TransfersConfig
	SafetyConfig
	SyncConfig
	LoggingConfig
}

// StatePath returns the state DB file path for this drive.
func (rd *ResolvedDrive) StatePath() string {
	return DriveStatePath(rd.CanonicalID)
}

// MatchDrive selects a drive from the config by selector string. The matching
// precedence is: exact canonical ID > display_name > partial canonical ID substring.
// If selector is empty, auto-selects when exactly one drive is configured.
//
// When no drives are configured, provides smart error messages: checks for
// existing tokens on disk and suggests "drive add" or "login" accordingly.
func MatchDrive(cfg *Config, selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	if len(cfg.Drives) == 0 {
		return matchNoDrives(logger)
	}

	if selector == "" {
		return matchSingleDrive(cfg, logger)
	}

	return matchBySelector(cfg, selector, logger)
}

// matchNoDrives handles drive matching when no drives are configured.
// Always returns an error with context-aware guidance based on whether
// tokens exist on disk (suggests "drive add") or not (suggests "login").
func matchNoDrives(logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
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

// matchBySelector finds a drive by exact ID, display_name, or partial substring match.
func matchBySelector(cfg *Config, selector string, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	// Exact canonical ID match — try parsing the selector as a CanonicalID
	// and looking it up directly in the typed map.
	if selectorCID, err := driveid.NewCanonicalID(selector); err == nil {
		if d, ok := cfg.Drives[selectorCID]; ok {
			logger.Debug("drive matched by exact canonical ID", "canonical_id", selector)

			return selectorCID, d, nil
		}
	}

	// Display name match.
	for id := range cfg.Drives {
		if strings.EqualFold(cfg.Drives[id].DisplayName, selector) {
			logger.Debug("drive matched by display_name", "display_name", selector, "canonical_id", id.String())

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
	var pausedUntil string
	if drive.PausedUntil != nil {
		pausedUntil = *drive.PausedUntil
	}

	resolved := &ResolvedDrive{
		CanonicalID:     canonicalID,
		DisplayName:     drive.DisplayName,
		Owner:           drive.Owner,
		Paused:          drive.IsPaused(time.Now()),
		PausedUntil:     pausedUntil,
		SyncDir:         expandTilde(drive.SyncDir),
		TransfersConfig: cfg.TransfersConfig,
		SafetyConfig:    cfg.SafetyConfig,
		SyncConfig:      cfg.SyncConfig,
		LoggingConfig:   cfg.LoggingConfig,
	}

	if canonicalID.IsShared() {
		resolved.RootItemID = canonicalID.SourceItemID()
	}

	var catalog *Catalog
	// Two-source drive ID resolution: prefer the catalog-backed drive record
	// (per-drive, accurate for SharePoint libraries and shared drives), then
	// the explicit shared-drive canonical ID payload. Otherwise DriveID
	// stays zero.
	if loadedCatalog, err := LoadCatalog(); err != nil {
		logger.Debug("could not load catalog drive record", "canonical_id", canonicalID.String(), "error", err)
	} else {
		catalog = loadedCatalog
	}

	if drive, found := catalogDriveRecord(catalog, canonicalID); found && drive.RemoteDriveID != "" {
		resolved.DriveID = driveid.New(drive.RemoteDriveID)
		logger.Debug("resolved drive ID from catalog drive record",
			"drive_id", resolved.DriveID.String(),
			"canonical_id", canonicalID.String(),
		)
	} else if canonicalID.IsShared() {
		// Shared drives embed the remote drive ID in the canonical ID.
		resolved.DriveID = driveid.New(canonicalID.SourceDriveID())
		logger.Debug("resolved drive ID from canonical ID",
			"drive_id", resolved.DriveID.String(),
			"canonical_id", canonicalID.String(),
		)
	}

	if canonicalID.IsShared() {
		resolved.SharedRootDeltaCapable = sharedRootDeltaCapable(canonicalID, catalog, logger)
	}

	// Compute runtime default sync_dir when the drive has none configured.
	if resolved.SyncDir == "" {
		orgName, displayName := ResolveAccountNames(canonicalID, logger)
		otherDirs := CollectOtherSyncDirs(cfg, canonicalID, logger)
		resolved.SyncDir = expandTilde(DefaultSyncDir(canonicalID, orgName, displayName, otherDirs))
		logger.Debug("using default sync_dir",
			"sync_dir", resolved.SyncDir,
			"canonical_id", canonicalID.String(),
			"org_name", orgName,
		)
	}

	// Auto-derive display_name when the user hasn't configured one explicitly.
	if resolved.DisplayName == "" {
		resolved.DisplayName = DefaultDisplayName(canonicalID)
		logger.Debug("using default display_name",
			"display_name", resolved.DisplayName,
			"canonical_id", canonicalID.String(),
		)
	}

	applyDriveOverrides(resolved, drive, logger)

	return resolved
}

func catalogDriveRecord(catalog *Catalog, canonicalID driveid.CanonicalID) (CatalogDrive, bool) {
	if catalog == nil {
		return CatalogDrive{}, false
	}

	return catalog.DriveByCanonicalID(canonicalID)
}

func sharedRootDeltaCapable(
	canonicalID driveid.CanonicalID,
	catalog *Catalog,
	logger *slog.Logger,
) bool {
	drive, found := catalogDriveRecord(catalog, canonicalID)
	if !found || drive.OwnerAccountCanonical == "" {
		return true
	}

	ownerCID, err := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
	if err != nil {
		logger.Debug("could not parse shared-root owner account type",
			"canonical_id", canonicalID.String(),
			"owner_account_canonical", drive.OwnerAccountCanonical,
			"error", err,
		)
		return true
	}

	return RootedSubtreeDeltaCapableForTokenOwner(ownerCID)
}

// RootedSubtreeDeltaCapableForTokenOwner reports whether a rooted-subtree
// mount owned by the given token account can use folder delta. Personal owner
// accounts can; business/SharePoint owners still require recursive fallback.
func RootedSubtreeDeltaCapableForTokenOwner(ownerCID driveid.CanonicalID) bool {
	switch ownerCID.DriveType() {
	case driveid.DriveTypeBusiness, driveid.DriveTypeSharePoint:
		return false
	default:
		return true
	}
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
			// Use the catalog account record for org_name.
			var orgName string

			acctCID := accountCIDForDrive(id)
			if !acctCID.IsZero() {
				if catalog, err := LoadCatalog(); err != nil {
					logger.Debug("could not load catalog account for sync dir",
						"canonical_id", id.String(), "error", err)
				} else if account, found := catalog.AccountByCanonicalID(acctCID); found && account.OrgName != "" {
					orgName = account.OrgName
				}
			}

			dir = BaseSyncDir(id, orgName, cfg.Drives[id].DisplayName)
		}

		if dir != "" {
			dirs = append(dirs, dir)
		}
	}

	return dirs
}

// ResolveAccountNames returns org_name and display_name for a drive's parent
// account using the catalog account record. Returns empty strings if the
// account record is unavailable.
func ResolveAccountNames(cid driveid.CanonicalID, logger *slog.Logger) (orgName, displayName string) {
	acctCID := accountCIDForDrive(cid)
	if acctCID.IsZero() {
		return "", ""
	}

	catalog, err := LoadCatalog()
	if err != nil {
		logger.Debug("could not load catalog account for names",
			"canonical_id", cid.String(), "error", err)

		return "", ""
	}

	account, found := catalog.AccountByCanonicalID(acctCID)
	if !found {
		return "", ""
	}

	return account.OrgName, account.DisplayName
}

// applyDriveOverrides selectively replaces global config values with per-drive
// values for fields that the drive explicitly sets.
func applyDriveOverrides(resolved *ResolvedDrive, drive *Drive, logger *slog.Logger) {
	_ = resolved
	_ = drive
	_ = logger
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
	return discoverCIDFiles(dir, logger)
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

// DiscoverStateDBsForEmail scans the data directory for state database files
// belonging to the given email address. Returns full file paths. The email
// match uses an underscore boundary ("_email") to prevent substring collisions
// (e.g. "a@b.com" won't match "ba@b.com").
func DiscoverStateDBsForEmail(email string, logger *slog.Logger) []string {
	return discoverStateDBsForEmailIn(DefaultDataDir(), email, logger)
}

// discoverStateDBsForEmailIn scans dir for state DB files belonging to email.
func discoverStateDBsForEmailIn(dir, email string, logger *slog.Logger) []string {
	return discoverFilesForEmail(dir, "state_", ".db", email, logger)
}
