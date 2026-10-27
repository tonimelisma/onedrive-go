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
		logger.Info("resetting sync state DB",
			slog.String("db_path", dbPath),
		)
	}

	if err := removeStateDBFiles(dbPath); err != nil {
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

// removeStateDBFiles deletes the SQLite DB family rooted at dbPath.
func removeStateDBFiles(dbPath string) error {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", candidate, err)
		}
	}

	return nil
}

// RebindSyncRoot forgets the sync-folder fingerprint recorded in one drive's
// state DB, keeping everything else. The next engine start binds the folder it
// finds at the sync path. It is the explicit answer to a refused start after
// the user moved, restored, or re-pointed the sync folder on purpose; the
// delete-safety hold still guards whatever that folder turns out to lack.
func RebindSyncRoot(ctx context.Context, dbPath string, logger *slog.Logger) (err error) {
	if logger != nil {
		logger.Info("clearing recorded sync folder identity",
			slog.String("db_path", dbPath),
		)
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("open sync store: %w", err)
	}
	defer func() {
		if closeErr := store.Close(context.WithoutCancel(ctx)); closeErr != nil && err == nil {
			err = fmt.Errorf("close sync store: %w", closeErr)
		}
	}()

	if err := store.ClearSyncRootIdentity(ctx); err != nil {
		return fmt.Errorf("clear sync folder identity: %w", err)
	}

	return nil
}
