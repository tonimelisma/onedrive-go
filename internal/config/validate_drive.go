package config

import (
	"fmt"

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

	return errs
}

// validateSingleDrive validates one drive's fields and checks sync_dir uniqueness.
// The canonical ID format is guaranteed valid by parse-time validation.
func validateSingleDrive(id driveid.CanonicalID, drive *Drive, syncDirs map[string]string) []error {
	var errs []error

	idStr := id.String()

	if drive.SyncDir == "" {
		errs = append(errs, fmt.Errorf("drive %q: sync_dir is required", idStr))
	}

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
