package sync

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// SyncedFileCount reports how many files the drive's sync state at dbPath
// records as synced.
func SyncedFileCount(ctx context.Context, dbPath string, logger *slog.Logger) (int, error) {
	if _, statErr := localpath.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		return 0, nil
	}
	snapshot, err := ReadDriveStatusSnapshot(ctx, dbPath, logger)
	if err != nil {
		return 0, err
	}
	return snapshot.BaselineFileCount, nil
}
