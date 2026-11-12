package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ResetStateDB deletes one per-mount state DB file family and recreates a
// fresh canonical store in place. The caller holds the store's offline-change
// lock (LockDriveForOfflineChange).
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
	if err := store.Close(ctx); err != nil {
		return fmt.Errorf("close fresh sync store: %w", err)
	}

	return nil
}

// removeStateDBFiles deletes the SQLite DB family rooted at dbPath
// (config.StateDBFamily), the database last.
func removeStateDBFiles(dbPath string) error {
	for _, candidate := range config.StateDBFamily(dbPath) {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", candidate, err)
		}
	}

	return nil
}

// moveStateDBFamily renames every file of the store at from to the same file
// of the store at to, the database last (config.StateDBFamily). A sidecar the
// source does not have is removed at the target instead: with no database
// there it belongs to nothing, and it would be replayed into the moved one.
// The caller holds both stores' locks and has checked that no database is
// at to.
func moveStateDBFamily(from, to string) error {
	targets := config.StateDBFamily(to)
	for i, source := range config.StateDBFamily(from) {
		err := localpath.Rename(source, targets[i])
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move state DB file %s: %w", source, err)
		}
		if removeErr := localpath.Remove(targets[i]); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove leftover state DB file %s: %w", targets[i], removeErr)
		}
	}

	return nil
}

// RebindSyncRoot forgets the sync-folder fingerprint recorded in one drive's
// state DB, keeping everything else. The next engine start binds the folder it
// finds at the sync path. It is the explicit answer to a refused start after
// the user moved, restored, or re-pointed the sync folder on purpose; the
// delete-safety hold still guards whatever that folder turns out to lack. The
// caller holds the store's offline-change lock (LockDriveForOfflineChange).
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
		if closeErr := store.Close(ctx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close sync store: %w", closeErr))
		}
	}()

	if err := store.ClearSyncRootIdentity(ctx); err != nil {
		return fmt.Errorf("clear sync folder identity: %w", err)
	}

	return nil
}
