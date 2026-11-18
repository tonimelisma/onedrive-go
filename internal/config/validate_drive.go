package config

import (
	"fmt"
	"maps"
	"os"
	slashpath "path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// validateDrives checks all drive-level constraints on the configured values:
// per-drive setting validity, sync_dir uniqueness and overlap, and a sync_dir
// that meets a folder the client keeps for itself. Canonical ID format is
// already validated at parse time (driveSectionDecoder).
func validateDrives(cfg *Config) []error {
	if len(cfg.Drives) == 0 {
		return nil // no drives is valid (user hasn't logged in yet)
	}

	var errs []error

	syncDirs := make(map[string]string, len(cfg.Drives))

	for id := range cfg.Drives {
		drive := cfg.Drives[id]
		errs = append(errs, validateSingleDrive(id, &drive, syncDirs)...)
	}

	errs = append(errs, checkSyncDirOverlap(syncDirs)...)
	errs = append(errs, checkSyncDirsMeetNoManagedFolder(syncDirs)...)

	return errs
}

// validateSingleDrive validates one drive's fields and checks sync_dir uniqueness.
// The canonical ID format is guaranteed valid by parse-time validation.
// Empty sync_dir is valid — runtime defaults are computed in buildResolvedDrive().
// This supports minimal drive sections during initial setup.
func validateSingleDrive(id driveid.CanonicalID, drive *Drive, syncDirs map[string]string) []error {
	errs := checkDriveSyncDirUniqueness(id.String(), drive, syncDirs)
	errs = append(errs, validateDriveFilterConfig(id.String(), drive.DriveFilterConfig)...)
	errs = append(errs, validatePausedUntil(id.String(), drive.PausedUntil)...)

	return errs
}

// validatePausedUntil rejects a paused_until that is not a time. IsPaused
// deliberately treats such a value as an indefinite pause so a corrupt file
// never resumes a drive by surprise; this makes that state visible instead
// of silent.
func validatePausedUntil(id string, pausedUntil *string) []error {
	if pausedUntil == nil {
		return nil
	}

	if _, err := time.Parse(time.RFC3339, *pausedUntil); err != nil {
		return []error{fmt.Errorf("drive %s: paused_until %q is not a time like 2026-10-24T15:00:00Z; "+
			"the drive stays paused until you run 'onedrive-go resume' or fix the value", id, *pausedUntil)}
	}

	return nil
}

func validateDriveFilterConfig(id string, filter DriveFilterConfig) []error {
	var errs []error

	errs = append(errs, validateDriveFilterDirList(id, "ignored_dirs", filter.IgnoredDirs)...)
	errs = append(errs, validateDriveFilterDirList(id, "included_dirs", filter.IncludedDirs)...)
	errs = append(errs, validateDriveIgnoredPaths(id, filter.IgnoredPaths)...)

	return errs
}

func validateDriveFilterDirList(id, key string, entries []string) []error {
	var errs []error

	for _, entry := range entries {
		normalized := normalizeFilterPath(entry)
		switch {
		case normalized == "":
			errs = append(errs, fmt.Errorf("drive %q %s contains an empty path", id, key))
		case slashpath.IsAbs(normalized):
			errs = append(errs, fmt.Errorf("drive %q %s path %q must be root-relative", id, key, entry))
		case normalized == ".":
			errs = append(errs, fmt.Errorf("drive %q %s path %q cannot target the sync root", id, key, entry))
		case hasParentPathComponent(normalized):
			errs = append(errs, fmt.Errorf("drive %q %s path %q cannot contain '..'", id, key, entry))
		case strings.ContainsAny(normalized, "*?["):
			errs = append(errs, fmt.Errorf("drive %q %s path %q must be an exact root-relative directory path", id, key, entry))
		case slashpath.Clean(normalized) != normalized:
			errs = append(errs, fmt.Errorf("drive %q %s path %q must be normalized", id, key, entry))
		}
	}

	return errs
}

func validateDriveIgnoredPaths(id string, entries []string) []error {
	var errs []error

	for _, entry := range entries {
		normalized := normalizeFilterPath(entry)
		switch {
		case normalized == "":
			errs = append(errs, fmt.Errorf("drive %q ignored_paths contains an empty pattern", id))
		case slashpath.IsAbs(normalized):
			errs = append(errs, fmt.Errorf("drive %q ignored_paths pattern %q must be root-relative", id, entry))
		case normalized == ".":
			errs = append(errs, fmt.Errorf("drive %q ignored_paths pattern %q cannot target the sync root", id, entry))
		case hasParentPathComponent(normalized):
			errs = append(errs, fmt.Errorf("drive %q ignored_paths pattern %q cannot contain '..'", id, entry))
		case slashpath.Clean(normalized) != normalized:
			errs = append(errs, fmt.Errorf("drive %q ignored_paths pattern %q must be normalized", id, entry))
		default:
			if _, err := slashpath.Match(normalized, normalized); err != nil {
				errs = append(errs, fmt.Errorf("drive %q ignored_paths pattern %q is invalid: %w", id, entry, err))
			}
		}
	}

	return errs
}

func normalizeFilterPath(path string) string {
	return filepath.ToSlash(path)
}

// parentPathComponent is the path component that names a folder's parent.
const parentPathComponent = ".."

func hasParentPathComponent(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == parentPathComponent {
			return true
		}
	}

	return false
}

// checkDriveSyncDirUniqueness ensures no two drives share the same expanded sync_dir.
// Resolves symlinks so that two paths pointing to the same directory are detected (B-287).
// Falls back to lexical comparison if the path doesn't exist yet.
func checkDriveSyncDirUniqueness(id string, drive *Drive, seen map[string]string) []error {
	if drive.SyncDir == "" {
		return nil
	}

	expanded := canonicalSyncDirPath(drive.SyncDir)

	if other, exists := seen[expanded]; exists {
		return []error{fmt.Errorf(
			"drives %q and %q have the same sync_dir %q", other, id, drive.SyncDir)}
	}

	seen[expanded] = id

	return nil
}

func canonicalSyncDirPath(syncDir string) string {
	expanded := filepath.Clean(expandTilde(syncDir))
	if !filepath.IsAbs(expanded) {
		return expanded
	}

	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		return filepath.Clean(resolved)
	}

	return canonicalPathThroughExistingParent(expanded)
}

func canonicalPathThroughExistingParent(path string) string {
	probe := path
	missingParts := make([]string, 0)
	for {
		parent := filepath.Dir(probe)
		if parent == probe {
			return path
		}

		missingParts = append([]string{filepath.Base(probe)}, missingParts...)
		if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
			parts := append([]string{resolvedParent}, missingParts...)
			return filepath.Clean(filepath.Join(parts...))
		}

		probe = parent
	}
}

// checkSyncDirOverlap detects ancestor/descendant relationships between sync
// directories. Two drives whose sync_dirs overlap (one is a parent of the other)
// would cause file conflicts and duplicate syncing. The syncDirs map contains
// expanded paths -> canonical IDs, populated by checkDriveSyncDirUniqueness.
func checkSyncDirOverlap(syncDirs map[string]string) []error {
	// Collect all expanded paths for pairwise comparison.
	// O(n²) is fine — users typically have <10 drives.
	type entry struct {
		path string
		id   string
	}

	// Paths are already Clean'd by EvalSymlinks in checkDriveSyncDirUniqueness.
	// Sorted, so the same config reports its overlaps in the same order.
	entries := make([]entry, 0, len(syncDirs))
	for _, path := range slices.Sorted(maps.Keys(syncDirs)) {
		entries = append(entries, entry{path: path, id: syncDirs[path]})
	}

	var errs []error

	for i := range entries {
		for j := i + 1; j < len(entries); j++ {
			if isAncestorOrDescendant(entries[i].path, entries[j].path) {
				errs = append(errs, fmt.Errorf(
					"sync_dir overlap: drives %q and %q have nested directories (%s, %s)",
					entries[i].id, entries[j].id, entries[i].path, entries[j].path))
			}
		}
	}

	return errs
}

// isAncestorOrDescendant returns true if a is an ancestor of b or b is an
// ancestor of a.
func isAncestorOrDescendant(a, b string) bool {
	return pathContains(a, b) || pathContains(b, a)
}

// pathContains reports whether path is dir or lies below it, comparing whole
// components: "/OneDrive" does not contain "/OneDriveBackup", and "/"
// contains every absolute path.
func pathContains(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)

	return err == nil && rel != parentPathComponent && !strings.HasPrefix(rel, parentPathComponent+string(filepath.Separator))
}

// managedFolder is a folder onedrive-go keeps for itself, which no sync
// folder may be, hold or lie inside.
type managedFolder struct {
	path string
	// holds says what the folder is, for the refusal.
	holds string
}

// managedFolders returns the data and configuration folders, resolved as
// sync_dirs are. Where both are one folder (macOS), it is named once.
func managedFolders() []managedFolder {
	dataDir, configDir := DefaultDataDir(), DefaultConfigDir()
	if dataDir != "" {
		dataDir = canonicalSyncDirPath(dataDir)
	}
	if configDir != "" {
		configDir = canonicalSyncDirPath(configDir)
	}
	if dataDir != "" && dataDir == configDir {
		return []managedFolder{{
			path:  dataDir,
			holds: "the folder onedrive-go keeps its own data and configuration in (sign-ins, sync state, trash, config.toml)",
		}}
	}

	var folders []managedFolder
	if dataDir != "" {
		folders = append(folders, managedFolder{
			path:  dataDir,
			holds: "the folder onedrive-go keeps its own data in (sign-ins, sync state, trash)",
		})
	}
	if configDir != "" {
		folders = append(folders, managedFolder{
			path:  configDir,
			holds: "the folder onedrive-go keeps its configuration in",
		})
	}

	return folders
}

// checkSyncDirsMeetNoManagedFolder runs checkSyncDirMeetsNoManagedFolder on
// every configured sync_dir. The syncDirs map holds resolved paths, as
// checkDriveSyncDirUniqueness fills it.
func checkSyncDirsMeetNoManagedFolder(syncDirs map[string]string) []error {
	var errs []error

	for _, syncDir := range slices.Sorted(maps.Keys(syncDirs)) {
		errs = append(errs, checkSyncDirMeetsNoManagedFolder(syncDirs[syncDir], syncDir)...)
	}

	return errs
}

// checkDefaultSyncDirsMeetNoManagedFolder runs checkSyncDirMeetsNoManagedFolder
// on the default folder (defaultSyncDir, from catalog) of every drive whose
// section sets no sync_dir; checkSyncDirsMeetNoManagedFolder checks the
// configured ones, so each drive is checked once. A default that is not an
// absolute path, as when the home folder is unknown, could not be computed and
// is not checked.
func checkDefaultSyncDirsMeetNoManagedFolder(cfg *Config, catalog *Catalog) []error {
	var errs []error

	for _, cid := range sortedDriveIDs(cfg) {
		if cfg.Drives[cid].SyncDir != "" {
			continue
		}

		syncDir := defaultSyncDir(cfg, catalog, cid)
		if !filepath.IsAbs(syncDir) {
			continue
		}

		errs = append(errs, checkSyncDirMeetsNoManagedFolder(cid.String(), canonicalSyncDirPath(syncDir))...)
	}

	return errs
}

// sortedDriveIDs returns the config's drive IDs in canonical ID order, so a
// check over several drives reports them in the same order on every run.
func sortedDriveIDs(cfg *Config) []driveid.CanonicalID {
	return slices.SortedFunc(maps.Keys(cfg.Drives), func(a, b driveid.CanonicalID) int {
		return strings.Compare(a.String(), b.String())
	})
}

// checkSyncDirMeetsNoManagedFolder refuses syncDir, the resolved path
// (canonicalSyncDirPath) of driveID's sync folder, when it and a folder
// onedrive-go manages for itself, resolved the same way, are the same folder
// or either holds the other (R-4.8.3). A sync folder holding one would upload
// the client's state databases, sign-ins and trash, the files sync moves to
// the trash would come back as new files, and a trashed file could not be
// restored, since a restore moves a file into the sync folder from outside
// it. A sync folder inside one is synced where the client writes its own
// files, and inside the data folder it holds the trash's files or lies beside
// them. Each refusal names both paths, which holds which, and what to change.
func checkSyncDirMeetsNoManagedFolder(driveID, syncDir string) []error {
	var errs []error

	// outsideIt is the change for a sync folder that is a managed folder or
	// lies inside one.
	const outsideIt = "outside it"

	for _, folder := range managedFolders() {
		var relation, change string
		switch nestingOf(syncDir, folder.path) {
		case foldersApart:
			continue
		case foldersSame:
			relation, change = "is", outsideIt
		case firstFolderHoldsSecond:
			relation, change = "contains", "that does not contain it"
		case secondFolderHoldsFirst:
			relation, change = "lies inside", outsideIt
		}

		errs = append(errs, fmt.Errorf("drive %q sync_dir %s %s %s, %s; set sync_dir to a folder %s",
			driveID, syncDir, relation, folder.path, folder.holds, change))
	}

	return errs
}

// folderNesting is how two folders lie: apart, as one folder, or one holding
// the other.
type folderNesting int

const (
	foldersApart folderNesting = iota
	foldersSame
	firstFolderHoldsSecond
	secondFolderHoldsFirst
)

// nestingOf decides how the folders first and second lie, both resolved
// paths. It decides by text, component by component (pathContains), and,
// where both are absolute paths, also by what the folders are: a folder that
// exists is compared (os.SameFile) with every folder above the other path
// that exists, and with the other when both exist, so a path that names a
// folder in another spelling, as on a volume that ignores letter case, is
// still found, whether or not the other path exists yet. A folder that does
// not exist yet, or cannot be read, is compared by text alone.
func nestingOf(first, second string) folderNesting {
	firstInfo, firstErr := localpath.Stat(first)
	secondInfo, secondErr := localpath.Stat(second)
	// Each identity is compared only between absolute paths: a relative one
	// would be read against the working folder.
	bothAbsolute := filepath.IsAbs(first) && filepath.IsAbs(second)
	firstHasIdentity := bothAbsolute && firstErr == nil
	secondHasIdentity := bothAbsolute && secondErr == nil

	switch {
	case first == second || firstHasIdentity && secondHasIdentity && os.SameFile(firstInfo, secondInfo):
		return foldersSame
	case pathContains(first, second) || firstHasIdentity && hasFolderAbove(second, firstInfo):
		return firstFolderHoldsSecond
	case pathContains(second, first) || secondHasIdentity && hasFolderAbove(first, secondInfo):
		return secondFolderHoldsFirst
	default:
		return foldersApart
	}
}

// hasFolderAbove reports whether folder is one of the folders above path, an
// absolute path, found by taking its last component off, one at a time, up
// to the filesystem's root.
func hasFolderAbove(path string, folder os.FileInfo) bool {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if info, err := localpath.Stat(dir); err == nil && os.SameFile(info, folder) {
			return true
		}
		if filepath.Dir(dir) == dir {
			return false
		}
	}
}
