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
	RemoteRootItemID       string // configured remote root item for standalone mount-root drives; empty = drive root
	RemoteRootDeltaCapable bool   // true when folder delta is supported for the configured remote root

	TransfersConfig
	SafetyConfig
	SyncConfig
	LoggingConfig
	DriveFilterConfig
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
	var (
		canonicalID driveid.CanonicalID
		drive       Drive
		err         error
	)

	switch {
	case len(cfg.Drives) == 0:
		err = matchNoDrives(logger)
	case selector == "":
		canonicalID, drive, err = matchSingleDrive(cfg, logger)
	default:
		canonicalID, drive, err = matchBySelector(cfg, selector, logger)
	}

	return canonicalID, drive, markInvalidConfig(err)
}

// matchNoDrives handles drive matching when no drives are configured.
// Always returns an error with context-aware guidance based on whether
// tokens exist on disk (suggests "drive add") or not (suggests "login").
func matchNoDrives(logger *slog.Logger) error {
	// Check for tokens on disk to provide a more helpful error message. When
	// the data directory cannot be read, that is the problem to report:
	// "no accounts configured" would send the user to sign in again.
	tokens, err := DiscoverTokens(logger)
	if err != nil {
		return fmt.Errorf("checking for saved logins: %w", err)
	}
	if len(tokens) > 0 {
		return fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	return fmt.Errorf("no accounts configured — run 'onedrive-go login' to get started")
}

// matchSingleDrive auto-selects when exactly one drive is configured.
func matchSingleDrive(cfg *Config, logger *slog.Logger) (driveid.CanonicalID, Drive, error) {
	if len(cfg.Drives) == 1 {
		for id := range cfg.Drives {
			logger.Debug("auto-selected single drive", "canonical_id", id.String())

			return id, cfg.Drives[id], nil
		}
	}

	return driveid.CanonicalID{}, Drive{}, fmt.Errorf("several drives are configured; choose one with --drive (%s)",
		strings.Join(configuredDriveNames(cfg), ", "))
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

	return driveid.CanonicalID{}, Drive{}, fmt.Errorf("no drive matching %q; configured drives: %s",
		selector, strings.Join(configuredDriveNames(cfg), ", "))
}

// configuredDriveNames lists drives for selector errors, display name first
// when there is one, sorted so the message is stable.
func configuredDriveNames(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Drives))
	for cid := range cfg.Drives {
		name := cid.String()
		if display := cfg.Drives[cid].DisplayName; display != "" {
			name = display + " (" + name + ")"
		}
		names = append(names, name)
	}
	slices.Sort(names)

	return names
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
		DriveFilterConfig: DriveFilterConfig{
			IgnoredDirs:      slices.Clone(drive.IgnoredDirs),
			IncludedDirs:     slices.Clone(drive.IncludedDirs),
			IgnoredPaths:     slices.Clone(drive.IgnoredPaths),
			IgnoreDotfiles:   drive.IgnoreDotfiles,
			IgnoreJunkFiles:  cloneBoolPointer(drive.IgnoreJunkFiles),
			FollowSymlinks:   drive.FollowSymlinks,
			CrossFilesystems: drive.CrossFilesystems,
		},
	}

	if canonicalID.IsShared() {
		resolved.RemoteRootItemID = canonicalID.SourceItemID()
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

	if drive, found := catalog.DriveByCanonicalID(canonicalID); found && drive.RemoteDriveID != "" {
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
		resolved.RemoteRootDeltaCapable = remoteRootDeltaCapable(canonicalID, catalog)
	}

	// Compute runtime default sync_dir when the drive has none configured.
	resolved.SyncDir = EffectiveSyncDir(cfg, canonicalID, resolved.SyncDir, logger)

	// Auto-derive display_name when the user hasn't configured one explicitly.
	if resolved.DisplayName == "" {
		resolved.DisplayName = DefaultDisplayName(canonicalID)
		logger.Debug("using default display_name",
			"display_name", resolved.DisplayName,
			"canonical_id", canonicalID.String(),
		)
	}

	return resolved
}

// remoteRootDeltaCapable reports whether a shared drive's mount root can use
// folder delta, from the owner its catalog record names. A drive with no
// record (catalog nil when it could not be read) or no owner defaults to
// capable, and the runtime's fallback proves otherwise.
func remoteRootDeltaCapable(canonicalID driveid.CanonicalID, catalog *Catalog) bool {
	drive, found := catalog.DriveByCanonicalID(canonicalID)
	if !found {
		return true
	}

	ownerCID, hasOwner := drive.Owner()
	if !hasOwner {
		return true
	}

	return RemoteRootDeltaCapableForTokenOwner(ownerCID)
}

// RemoteRootDeltaCapableForTokenOwner reports whether a mount-root
// mount owned by the given token account can use folder delta. Personal owner
// accounts can; business/SharePoint owners still require recursive fallback.
func RemoteRootDeltaCapableForTokenOwner(ownerCID driveid.CanonicalID) bool {
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
// Pass a zero CanonicalID to include all drives (no exclusion). An unreadable
// catalog only costs those base names their organization name.
func CollectOtherSyncDirs(cfg *Config, excludeID driveid.CanonicalID, logger *slog.Logger) []string {
	catalog, err := LoadCatalog()
	if err != nil {
		logger.Debug("could not load catalog accounts for sync dirs", "error", err)
		catalog = nil
	}

	return otherSyncDirs(cfg, catalog, excludeID)
}

// otherSyncDirs is CollectOtherSyncDirs over a catalog already in hand (nil
// for none).
func otherSyncDirs(cfg *Config, catalog *Catalog, excludeID driveid.CanonicalID) []string {
	var dirs []string

	for id := range cfg.Drives {
		if id == excludeID {
			continue
		}

		dir := cfg.Drives[id].SyncDir
		if dir == "" {
			orgName, _ := accountNames(catalog, id)
			dir = baseSyncDir(id, orgName, cfg.Drives[id].DisplayName)
		}

		if dir != "" {
			dirs = append(dirs, dir)
		}
	}

	return dirs
}

// EffectiveSyncDir returns the directory a drive will actually sync into:
// configuredSyncDir when the user set one, otherwise the computed runtime
// default. A configured value comes back as written, so a caller that opens
// the folder expands a leading "~" (ExpandTilde); the default is expanded
// already.
//
// Read surfaces must use this rather than the raw configured value. A drive
// that relies on the default has an empty sync_dir in config, so reporting the
// raw field tells the user their files go nowhere -- `status` did exactly that
// until it was caught by TestE2E_RoundTrip/status_json.
func EffectiveSyncDir(
	cfg *Config,
	canonicalID driveid.CanonicalID,
	configuredSyncDir string,
	logger *slog.Logger,
) string {
	if configuredSyncDir != "" {
		return configuredSyncDir
	}

	orgName, displayName := ResolveAccountNames(canonicalID, logger)
	otherDirs := CollectOtherSyncDirs(cfg, canonicalID, logger)
	syncDir := expandTilde(DefaultSyncDir(canonicalID, orgName, displayName, otherDirs))

	logger.Debug("using default sync_dir",
		"sync_dir", syncDir,
		"canonical_id", canonicalID.String(),
		"org_name", orgName,
	)

	return syncDir
}

// ResolveAccountNames returns org_name and display_name for a drive's parent
// account using the catalog account record. Returns empty strings if the
// account record is unavailable.
func ResolveAccountNames(cid driveid.CanonicalID, logger *slog.Logger) (orgName, displayName string) {
	if accountCIDForDrive(cid).IsZero() {
		return "", ""
	}

	catalog, err := LoadCatalog()
	if err != nil {
		logger.Debug("could not load catalog account for names",
			"canonical_id", cid.String(), "error", err)

		return "", ""
	}

	return accountNames(catalog, cid)
}

// accountNames is ResolveAccountNames over a catalog already in hand (nil for
// none).
func accountNames(catalog *Catalog, cid driveid.CanonicalID) (orgName, displayName string) {
	account, found := catalog.AccountByCanonicalID(accountCIDForDrive(cid))
	if !found {
		return "", ""
	}

	return account.OrgName, account.DisplayName
}

// ExpandTilde replaces a leading "~/" with the user's home directory. If
// os.UserHomeDir() fails, the path is returned unexpanded so downstream
// validation can report the invalid sync_dir clearly.
func ExpandTilde(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, path[2:])
}

func expandTilde(path string) string {
	return ExpandTilde(path)
}

// DiscoverTokens lists token files in the default data directory and returns
// canonical drive IDs extracted from filenames. Token files follow the naming
// convention: token_{type}_{email}.json (e.g., token_personal_user@example.com.json).
// Used for smart error messages and drive list. A missing data directory
// has no tokens; a data directory that cannot be listed is an error.
func DiscoverTokens(logger *slog.Logger) ([]driveid.CanonicalID, error) {
	return discoverTokensIn(DefaultDataDir(), logger)
}

// discoverTokensIn scans dir for token files and extracts canonical IDs.
// Files that don't match the token naming convention are silently skipped.
func discoverTokensIn(dir string, logger *slog.Logger) ([]driveid.CanonicalID, error) {
	return discoverCIDFiles(dir, logger)
}

// DriveStatePath returns the state DB path for a canonical drive ID.
// Each drive gets its own state database. The ":" separator in canonical
// IDs is replaced with "_" for filesystem safety.
//
//	"personal:toni@outlook.com" -> "{dataDir}/state_personal_toni@outlook.com.db"
//	"sharepoint:alice@contoso.com:marketing:Docs" -> "{dataDir}/state_sharepoint_alice@contoso.com_marketing_Docs.db"
func DriveStatePath(canonicalID driveid.CanonicalID) string {
	return DriveStatePathForDataDir(DefaultDataDir(), canonicalID)
}

// DriveStatePathForDataDir is DriveStatePath for the data directory dataDir,
// for a caller that runs onedrive-go with a data directory of its own.
func DriveStatePathForDataDir(dataDir string, canonicalID driveid.CanonicalID) string {
	if dataDir == "" || canonicalID.IsZero() {
		return ""
	}

	sanitized := strings.ReplaceAll(canonicalID.String(), ":", "_")

	return filepath.Join(dataDir, stateDBPrefix+sanitized+stateDBSuffix)
}

// stateDBPrefix and stateDBSuffix bracket every state DB's file name in the
// data directory (DriveStatePath, MountStatePathForDataDir).
const (
	stateDBPrefix = "state_"
	stateDBSuffix = ".db"
)

// stateDBSidecarSuffixes name the files SQLite keeps beside a state DB, which
// internal/sync opens in WAL mode: the write-ahead log, its shared-memory
// index, and the rollback journal a store has before it switches to WAL.
func stateDBSidecarSuffixes() []string {
	return []string{"-wal", "-shm", "-journal"}
}

// StateDBFamily is every file of the state DB at dbPath: its sidecars, then
// the database itself, last. Every delete or move of a store takes all of
// them, in this order. The database goes last so that a delete cut short
// leaves a store the rerun still finds and finishes. All of them go because
// SQLite pairs a write-ahead log with a database by file name alone: a log
// beside a database that did not write it is replayed into it. (A log left
// alone is deleted when a database is created at that name.) Config owns the
// list, with the state DB paths; internal/sync, internal/multisync and the
// CLI all delete or move by it.
func StateDBFamily(dbPath string) []string {
	sidecars := stateDBSidecarSuffixes()
	family := make([]string, 0, len(sidecars)+1)
	for _, suffix := range sidecars {
		family = append(family, dbPath+suffix)
	}

	return append(family, dbPath)
}

// stateDBOfFamilyFile is the file name of the state DB that the file named
// name belongs to: name itself, or name without its sidecar suffix.
func stateDBOfFamilyFile(name string) string {
	for _, suffix := range stateDBSidecarSuffixes() {
		if trimmed, isSidecar := strings.CutSuffix(name, suffix); isSidecar {
			return trimmed
		}
	}

	return name
}

// DiscoverStateDBsForEmail scans the data directory for the state DBs
// belonging to the given email address: every store with any file of its
// family on disk, returned once by its database path, whether or not the
// database file itself is there. A log or journal left without its database
// (by an earlier version's purge, or a database deleted by hand) is still
// retained state to purge. The email match
// uses an underscore boundary ("_email") to prevent substring collisions
// (e.g. "a@b.com" won't match "ba@b.com"). A data directory that cannot be
// listed is an error, not "no state databases".
func DiscoverStateDBsForEmail(email string, logger *slog.Logger) ([]string, error) {
	return discoverStateDBsForEmailIn(DefaultDataDir(), email, logger)
}

// discoverStateDBsForEmailIn scans dir for state DBs belonging to email.
func discoverStateDBsForEmailIn(dir, email string, logger *slog.Logger) ([]string, error) {
	return discoverStateDBsForEmailWithIO(dir, email, logger, defaultConfigIO())
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}

	clone := *value
	return &clone
}
