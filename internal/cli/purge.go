package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// removeDriveDataFiles deletes the retained per-drive state DB for one drive.
// The catalog owns drive inventory; the state DB is the only drive-owned file
// artifact that survives `drive remove` and is purged here. Best-effort:
// ignores "not found" so repeated cleanup stays idempotent.
//
// Returns the number of files actually removed (0–1) so callers can report
// what happened.
func removeDriveDataFiles(cid driveid.CanonicalID, logger *slog.Logger) (int, error) {
	removed := 0

	var errs []error

	// Remove state database.
	statePath := config.DriveStatePath(cid)
	if statePath != "" {
		removedPath, err := removeManagedPath(statePath)
		if err != nil {
			logger.Warn("failed to remove state database", "path", statePath, "error", err)
			errs = append(errs, fmt.Errorf("removing state database %s: %w", statePath, err))
		} else if removedPath {
			logger.Info("removed state database", "path", statePath)
			removed++
		}
	}

	return removed, errors.Join(errs...)
}
