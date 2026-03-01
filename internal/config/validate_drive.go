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
// This supports zero-config mode and minimal drive sections.
func validateSingleDrive(id driveid.CanonicalID, drive *Drive, syncDirs map[string]string) []error {
	var errs []error

	idStr := id.String()

	if drive.PollInterval != "" {
		if err := validateDuration("poll_interval", drive.PollInterval, minPollInterval); err != nil {
			errs = append(errs, fmt.Errorf("drive %q: %w", idStr, err))
		}
	}

	errs = append(errs, checkDriveSyncDirUniqueness(idStr, drive, syncDirs)...)

	return errs
}

// checkDriveSyncDirUniqueness ensures no two drives share the same expanded sync_dir.
func checkDriveSyncDirUniqueness(id string, drive *Drive, seen map[string]string) []error {
	if drive.SyncDir == "" {
		return nil
	}

	expanded := expandTilde(drive.SyncDir)

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
	type entry struct {
		path string
		id   string
	}

	entries := make([]entry, 0, len(syncDirs))
	for path, id := range syncDirs {
		entries = append(entries, entry{path: filepath.Clean(path), id: id})
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
