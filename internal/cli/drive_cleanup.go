package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// removeDrive deletes the config section for the drive, preserving token,
// state database, and sync directory.
func removeDrive(w io.Writer, cfgPath string, driveID driveid.CanonicalID, syncDir string, logger *slog.Logger) error {
	if err := config.DeleteDriveSection(cfgPath, driveID); err != nil {
		return fmt.Errorf("removing drive: %w", err)
	}

	logger.Info("removed drive config section", "drive", driveID.String())

	idStr := driveID.String()
	if err := writef(w, "Removed drive %s from config.\n", idStr); err != nil {
		return err
	}
	if err := writef(w, "Token and state database kept for %s.\n", idStr); err != nil {
		return err
	}
	if err := writef(w, "Sync directory untouched: %s\n", syncDir); err != nil {
		return err
	}

	return writeln(w, "Run 'onedrive-go drive add "+idStr+"' to re-add.")
}

// purgeDrive deletes the config section and state database for a drive.
// The token is NOT deleted here — it may be shared with other drives (SharePoint).
func purgeDrive(w io.Writer, cfgPath string, driveID driveid.CanonicalID, logger *slog.Logger) error {
	if err := purgeSingleDrive(cfgPath, driveID, logger); err != nil {
		return err
	}

	if err := writef(w, "Purged config and state for %s.\n", driveID.String()); err != nil {
		return err
	}

	return writeln(w, "Sync directory untouched — delete manually if desired.")
}

// purgeOrphanedDriveState removes state DB and drive metadata files for a
// drive that is no longer in config. Unlike purgeSingleDrive (which also
// removes the drive's config section), this only deletes drive-owned data files
// left behind from a previous `drive remove` without --purge.
func purgeOrphanedDriveState(w io.Writer, cid driveid.CanonicalID, logger *slog.Logger) error {
	removed, err := removeDriveDataFiles(cid, logger)
	if err != nil {
		return err
	}

	if removed == 0 {
		return writef(w, "No orphaned state found for %s.\n", cid.String())
	}

	return writef(w, "Purged %d orphaned data file(s) for %s.\n", removed, cid.String())
}

// purgeSingleDrive removes only drive-owned state for one drive: the state
// database, drive metadata, and config section. Account profiles are
// account-owned catalog snapshot state and must survive `drive remove --purge` so
// the remaining logged-in account still has offline identity metadata. Token
// deletion is handled separately since tokens may be shared (SharePoint).
func purgeSingleDrive(cfgPath string, canonicalID driveid.CanonicalID, logger *slog.Logger) error {
	// Remove state DB and drive metadata (best-effort, errors logged).
	if _, err := removeDriveDataFiles(canonicalID, logger); err != nil {
		logger.Warn("errors removing drive data files", "drive", canonicalID.String(), "error", err)
	}

	if err := config.DeleteDriveSection(cfgPath, canonicalID); err != nil {
		return fmt.Errorf("deleting drive section: %w", err)
	}

	return nil
}

// purgeAccountDrives removes drive config sections and state databases for
// all affected drives. Token deletion is already handled before this call.
func purgeAccountDrives(w io.Writer, cfgPath string, affected []driveid.CanonicalID, logger *slog.Logger) error {
	if err := writeln(w); err != nil {
		return err
	}

	var errs []error

	for _, cid := range affected {
		if err := purgeSingleDrive(cfgPath, cid, logger); err != nil {
			logger.Warn("failed to purge drive", "drive", cid.String(), "error", err)
			errs = append(errs, fmt.Errorf("purging drive %s: %w", cid.String(), err))
		} else {
			if writeErr := writef(w, "Purged config and state for %s.\n", cid.String()); writeErr != nil {
				errs = append(errs, writeErr)
			}
		}
	}

	return errors.Join(errs...)
}

// removeAccountDriveConfigs deletes config sections for all affected drives
// without removing state databases. Used by regular logout (without --purge).
func removeAccountDriveConfigs(cfgPath string, affected []driveid.CanonicalID, logger *slog.Logger) error {
	var errs []error

	for _, cid := range affected {
		if err := config.DeleteDriveSection(cfgPath, cid); err != nil {
			logger.Warn("failed to remove drive config section", "drive", cid.String(), "error", err)
			errs = append(errs, fmt.Errorf("removing drive %s: %w", cid.String(), err))
		} else {
			logger.Info("removed drive config section", "drive", cid.String())
		}
	}

	return errors.Join(errs...)
}
