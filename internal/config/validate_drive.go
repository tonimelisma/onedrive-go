package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// validateDrives checks all drive-level constraints: required fields,
// per-drive setting validity, and sync_dir uniqueness. Canonical ID format
// is already validated at parse time (decodeDriveSections).
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

	return errs
}

// validateSingleDrive validates one drive's fields and checks sync_dir uniqueness.
// The canonical ID format is guaranteed valid by parse-time validation.
// Empty sync_dir is valid — runtime defaults are computed in buildResolvedDrive().
// This supports minimal drive sections during initial setup.
func validateSingleDrive(id driveid.CanonicalID, drive *Drive, syncDirs map[string]string) []error {
	var errs []error

	idStr := id.String()

	if drive.PollInterval != "" {
		if err := validateDuration("poll_interval", drive.PollInterval, minPollInterval); err != nil {
			errs = append(errs, fmt.Errorf("drive %q: %w", idStr, err))
		}
	}

	for _, p := range drive.SyncPaths {
		if !strings.HasPrefix(p, "/") {
			errs = append(errs, fmt.Errorf("drive %q: sync_paths: path %q must start with /", idStr, p))
		}
	}

	errs = append(errs, checkDriveSyncDirUniqueness(idStr, drive, syncDirs)...)

	return errs
}

// checkDriveSyncDirUniqueness ensures no two drives share the same expanded sync_dir.
// Resolves symlinks so that two paths pointing to the same directory are detected (B-287).
// Falls back to lexical comparison if the path doesn't exist yet.
func checkDriveSyncDirUniqueness(id string, drive *Drive, seen map[string]string) []error {
	if drive.SyncDir == "" {
		return nil
	}

	expanded := expandTilde(drive.SyncDir)

	// Resolve symlinks so two paths pointing to the same directory are caught.
	// If the path doesn't exist yet (valid — user may create it later), fall back to lexical.
	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		expanded = resolved
	}

	if other, exists := seen[expanded]; exists {
		return []error{fmt.Errorf(
			"drives %q and %q have the same sync_dir %q", other, id, drive.SyncDir)}
	}

	seen[expanded] = id

	return nil
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
	entries := make([]entry, 0, len(syncDirs))
	for path, id := range syncDirs {
		entries = append(entries, entry{path: path, id: id})
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
// ancestor of a. Uses filepath.Separator suffix to avoid false positives from
// path prefixes (e.g., "/OneDrive" vs "/OneDriveBackup").
func isAncestorOrDescendant(a, b string) bool {
	aSlash := a + string(filepath.Separator)
	bSlash := b + string(filepath.Separator)

	return strings.HasPrefix(bSlash, aSlash) || strings.HasPrefix(aSlash, bSlash)
}
