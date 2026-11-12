package cli

import (
	"context"
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

// purgeConfiguredDrive removes a configured drive's config section, tells a
// running watch, whose reload retires the drive's runner within the runner
// stop bound, and only then deletes the drive's state DB (deleteStateDB),
// which syncDir's engine had open. The token is NOT deleted here: it may be
// shared with other drives (SharePoint). A store an engine still has open is
// kept and the returned error says how to finish.
func purgeConfiguredDrive(ctx context.Context, cc *CLIContext, cid driveid.CanonicalID, syncDir string) error {
	if err := config.DeleteDriveSection(cc.CfgPath, cid); err != nil {
		return fmt.Errorf("removing drive: %w", err)
	}
	cc.Logger.Info("removed drive config section", "drive", cid.String())
	notifyDaemonIfRunning(ctx, cc)

	if _, err := deleteDriveState(ctx, cid, syncDir, cc.Logger); err != nil {
		return fmt.Errorf("removed %s from the config, but %w", cid.String(), err)
	}

	w := cc.resultWriter()
	if err := writef(w, "Purged config and state for %s.\n", cid.String()); err != nil {
		return err
	}

	return writeln(w, "Sync directory untouched — delete manually if desired.")
}

// purgeOrphanedDriveState removes the retained state DB of a drive that is no
// longer in the config, left behind by an earlier `drive remove` without
// --purge. Its folder is no longer known, so only the store is locked.
func purgeOrphanedDriveState(ctx context.Context, w io.Writer, cid driveid.CanonicalID, logger *slog.Logger) error {
	removed, err := deleteDriveState(ctx, cid, "", logger)
	if err != nil {
		return err
	}

	if !removed {
		return writef(w, "No orphaned state found for %s.\n", cid.String())
	}

	return writef(w, "Purged the orphaned sync state for %s.\n", cid.String())
}

// deleteDriveState deletes one drive's state DB (deleteStateDB). A kept store
// names `drive remove --purge` for the drive as the command that finishes,
// which works once the drive has left the config.
func deleteDriveState(ctx context.Context, cid driveid.CanonicalID, syncDir string, logger *slog.Logger) (bool, error) {
	statePath := config.DriveStatePath(cid)
	if statePath == "" {
		return false, nil
	}

	return deleteStateDB(ctx, statePath, syncDir, "onedrive-go drive remove --purge --drive "+cid.String(), logger)
}

// deleteAccountDriveStates deletes the state DBs of a logged-out account's
// drives, after their config sections were removed and a running watch was
// told (executeLogout). syncDirs holds each drive's folder as its engine
// locked it. A kept store is an error naming rerun; the other drives are
// purged anyway.
func deleteAccountDriveStates(
	ctx context.Context,
	w io.Writer,
	affected []driveid.CanonicalID,
	syncDirs map[driveid.CanonicalID]string,
	rerun string,
	logger *slog.Logger,
) error {
	if len(affected) == 0 {
		return nil
	}
	if err := writeln(w); err != nil {
		return err
	}

	var errs []error
	for _, cid := range affected {
		statePath := config.DriveStatePath(cid)
		if statePath == "" {
			continue
		}
		if _, err := deleteStateDB(ctx, statePath, syncDirs[cid], rerun, logger); err != nil {
			errs = append(errs, fmt.Errorf("removed %s from the config, but %w", cid.String(), err))
			continue
		}
		if err := writef(w, "Purged config and state for %s.\n", cid.String()); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// removeAccountDriveConfigs deletes config sections for all affected drives,
// collecting failures, and returns the drives whose section it removed. The
// state databases are left to the caller, which purges only those drives':
// a drive still in the config keeps its state.
func removeAccountDriveConfigs(
	cfgPath string,
	affected []driveid.CanonicalID,
	logger *slog.Logger,
) ([]driveid.CanonicalID, error) {
	removed := make([]driveid.CanonicalID, 0, len(affected))
	var errs []error

	for _, cid := range affected {
		if err := config.DeleteDriveSection(cfgPath, cid); err != nil {
			errs = append(errs, fmt.Errorf("removing drive %s: %w", cid.String(), err))
			continue
		}
		logger.Info("removed drive config section", "drive", cid.String())
		removed = append(removed, cid)
	}

	return removed, errors.Join(errs...)
}
