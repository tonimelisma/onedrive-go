package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// removeStateDBSidecars deletes the sidecars of the store at dbPath, never
// its database (config.StateDBFamily lists the database last).
func removeStateDBSidecars(dbPath string) error {
	family := config.StateDBFamily(dbPath)
	for _, sidecar := range family[:len(family)-1] {
		if err := localpath.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", sidecar, err)
		}
	}

	return nil
}

// moveStateDBFamily renames every file of the store at from that exists to
// the same file of the store at to, the sidecars first and the database last
// (config.StateDBFamily). It never deletes or replaces a file at to: one
// already there was moved there by an earlier run of this move that was cut
// short (the caller clears a stranger's sidecars before a move goes on
// record), and a moved log holds commits its database does not have yet. A
// file at both names is refused, both kept. The caller holds both stores'
// locks and has checked that no database is at to.
func moveStateDBFamily(from, to string) error {
	targets := config.StateDBFamily(to)
	for i, source := range config.StateDBFamily(from) {
		sourceExists, err := stateDBFileExists(source)
		if err != nil {
			return err
		}
		if !sourceExists {
			continue
		}
		targetExists, err := stateDBFileExists(targets[i])
		if err != nil {
			return err
		}
		if targetExists {
			return fmt.Errorf("move state DB file %s: %s already exists, moved there by this account email change; "+
				"%s was created after the account's email changed: remove %s and run the command again",
				source, targets[i], source, source)
		}
		if renameErr := localpath.Rename(source, targets[i]); renameErr != nil {
			return fmt.Errorf("move state DB file %s: %w", source, renameErr)
		}
	}

	return nil
}

func stateDBFileExists(path string) (bool, error) {
	_, err := localpath.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check state DB file %s: %w", path, err)
	}
}

// sqliteHeader is the magic string every SQLite database file starts with.
const sqliteHeader = "SQLite format 3\x00"

// foldStateDBLog folds the write-ahead log of the store at dbPath into its
// database with a truncating checkpoint, so that the store is one file and
// moving it is one rename that no interruption can part. It opens the store
// without applying the schema, so a store of any generation folds. A file
// that is not a SQLite database is not opened: SQLite would take up a log
// beside it, and could delete it, before it read the header. The caller
// holds the store's lock, logs the outcome and decides what a failure means;
// the store's own open and close records would only misdescribe a fold.
func foldStateDBLog(ctx context.Context, dbPath string) error {
	isSQLite, err := hasSQLiteHeader(dbPath)
	if err != nil {
		return err
	}
	if !isSQLite {
		return fmt.Errorf("fold state DB log: %s is not a SQLite database", dbPath)
	}

	store, err := openSyncStore(ctx, dbPath, slog.New(slog.DiscardHandler), false)
	if err != nil {
		return fmt.Errorf("fold state DB log: %w", err)
	}
	checkpointErr := store.Checkpoint(ctx)
	closeErr := store.Close(ctx)
	if foldErr := errors.Join(checkpointErr, closeErr); foldErr != nil {
		return fmt.Errorf("fold state DB log of %s: %w", dbPath, foldErr)
	}

	return nil
}

func hasSQLiteHeader(dbPath string) (_ bool, err error) {
	file, err := localpath.OpenRegular(dbPath)
	if err != nil {
		return false, fmt.Errorf("open state DB %s: %w", dbPath, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close state DB %s: %w", dbPath, closeErr)
		}
	}()

	header := make([]byte, len(sqliteHeader))
	if _, readErr := io.ReadFull(file, header); readErr != nil {
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, fmt.Errorf("read state DB %s: %w", dbPath, readErr)
	}

	return string(header) == sqliteHeader, nil
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
