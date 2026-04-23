package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ResetStateDB deletes one per-mount state DB file family and recreates a
// fresh canonical store in place.
func ResetStateDB(ctx context.Context, dbPath string, logger *slog.Logger) error {
	if logger != nil {
		logger.Warn("resetting sync state DB",
			slog.String("db_path", dbPath),
		)
	}

	if err := RemoveStateDBFiles(dbPath); err != nil {
		return err
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("create fresh sync store: %w", err)
	}
	if err := store.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close fresh sync store: %w", err)
	}

	return nil
}

// RemoveStateDBFiles deletes the SQLite DB family rooted at dbPath.
func RemoveStateDBFiles(dbPath string) error {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", candidate, err)
		}
	}

	return nil
}
