package config

import (
	"fmt"
	"strings"
)

// validDriveTypes enumerates accepted drive type prefixes in canonical IDs.
var validDriveTypes = map[string]bool{
	"personal":   true,
	"business":   true,
	"sharepoint": true,
}

// validateDrives checks all drive-level constraints: canonical ID format,
// required fields, per-drive setting validity, and sync_dir uniqueness.
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
func validateSingleDrive(id string, drive *Drive, syncDirs map[string]string) []error {
	var errs []error

	if err := validateCanonicalID(id); err != nil {
		errs = append(errs, err)

		return errs
	}

	if drive.SyncDir == "" {
		errs = append(errs, fmt.Errorf("drive %q: sync_dir is required", id))
	}

	if drive.PollInterval != "" {
		if err := validateDuration(drive.PollInterval, "poll_interval", minPollInterval); err != nil {
			errs = append(errs, fmt.Errorf("drive %q: %w", id, err))
		}
	}

	errs = append(errs, checkDriveSyncDirUniqueness(id, drive, syncDirs)...)

	return errs
}

// validateCanonicalID checks that a drive ID follows the "type:email" format
// with a valid drive type prefix.
func validateCanonicalID(id string) error {
	parts := strings.SplitN(id, ":", driveTypeParts)
	if len(parts) < driveTypeParts {
		return fmt.Errorf("drive ID %q: must contain ':' (format: type:email)", id)
	}

	driveType := parts[0]
	if !validDriveTypes[driveType] {
		return fmt.Errorf("drive ID %q: unknown type %q (must be personal, business, or sharepoint)", id, driveType)
	}

	if parts[1] == "" {
		return fmt.Errorf("drive ID %q: email part cannot be empty", id)
	}

	return nil
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
