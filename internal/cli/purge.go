package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// removeDriveDataFiles deletes the state database and drive metadata files for
// a drive. Best-effort: attempts both removals and returns a joined error if
// either fails (ignoring "not found" — idempotent). This is the shared
// primitive used by purgeSingleDrive (auth.go) and purgeOrphanedDriveState
// (drive.go) to avoid duplicated file-removal logic.
//
// Returns the number of files actually removed (0–2) so callers can report
// what happened.
func removeDriveDataFiles(cid driveid.CanonicalID, logger *slog.Logger) (int, error) {
	removed := 0

	var errs []error

	// Remove state database.
	statePath := config.DriveStatePath(cid)
	if statePath != "" {
		if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove state database", "path", statePath, "error", err)
			errs = append(errs, fmt.Errorf("removing state database %s: %w", statePath, err))
		} else if err == nil {
			logger.Info("removed state database", "path", statePath)
			removed++
		}
	}

	// Remove drive metadata file.
	metaPath := config.DriveMetadataPath(cid)
	if metaPath != "" {
		if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove drive metadata", "path", metaPath, "error", err)
			errs = append(errs, fmt.Errorf("removing drive metadata %s: %w", metaPath, err))
		} else if err == nil {
			logger.Info("removed drive metadata", "path", metaPath)
			removed++
		}
	}

	return removed, errors.Join(errs...)
}
