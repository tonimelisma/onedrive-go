package syncstore

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

	if err := RemoveStateDBFiles(dbPath); err != nil {
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

// RemoveStateDBFiles deletes the SQLite DB family rooted at dbPath
// (config.StateDBFamily), the database last. The wraps in this file keep
// their verbs and leave the path to the error under them: a localpath
// failure, and an *os.File's own read or close error, already names it.
func RemoveStateDBFiles(dbPath string) error {
	for _, candidate := range config.StateDBFamily(dbPath) {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file: %w", err)
		}
	}

	return nil
}

// RemoveStateDBSidecars deletes the sidecars of the store at dbPath, never
// its database (config.StateDBFamily lists the database last).
func RemoveStateDBSidecars(dbPath string) error {
	family := config.StateDBFamily(dbPath)
	for _, sidecar := range family[:len(family)-1] {
		if err := localpath.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file: %w", err)
		}
	}

	return nil
}

// MoveStateDBFamily renames every file of the store at from that exists to
// the same file of the store at to, the sidecars first and the database last
// (config.StateDBFamily). It never deletes or replaces a file at to: one
// already there was moved there by an earlier run of this move that was cut
// short (the caller clears a stranger's sidecars before a move goes on
// record), and a moved log holds commits its database does not have yet. A
// file at both names is refused, both kept. The caller holds both stores'
// locks and has checked that no database is at to.
func MoveStateDBFamily(from, to string) error {
	targets := config.StateDBFamily(to)
	for i, source := range config.StateDBFamily(from) {
		sourceExists, err := StateDBFileExists(source)
		if err != nil {
			return err
		}
		if !sourceExists {
			continue
		}
		targetExists, err := StateDBFileExists(targets[i])
		if err != nil {
			return err
		}
		if targetExists {
			return fmt.Errorf("move state DB file %s: %s already exists, moved there by this account email change; "+
				"%s was created after the account's email changed: remove %s and run the command again",
				source, targets[i], source, source)
		}
		if renameErr := localpath.Rename(source, targets[i]); renameErr != nil {
			return fmt.Errorf("move state DB file: %w", renameErr)
		}
	}

	return nil
}

func StateDBFileExists(path string) (bool, error) {
	_, err := localpath.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check state DB file: %w", err)
	}
}

// sqliteHeader is the magic string every SQLite database file starts with.
const sqliteHeader = "SQLite format 3\x00"

// FoldStateDBLog folds the write-ahead log of the store at dbPath into its
// database with a truncating checkpoint, so that the store is one file and
// moving it is one rename that no interruption can part. It opens the store
// without applying the schema, so a store of any generation folds. A file
// that is not a SQLite database is not opened: SQLite would take up a log
// beside it, and could delete it, before it read the header. The caller
// holds the store's lock, logs the outcome and decides what a failure means;
// the store's own open and close records would only misdescribe a fold.
func FoldStateDBLog(ctx context.Context, dbPath string) error {
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
		return false, fmt.Errorf("open state DB: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close state DB: %w", closeErr)
		}
	}()

	header := make([]byte, len(sqliteHeader))
	if _, readErr := io.ReadFull(file, header); readErr != nil {
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, fmt.Errorf("read state DB: %w", readErr)
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

// ForgetMountBoundary forgets that path, relative to the sync folder, was a
// crossed mount point, keeping everything else, and reports whether it was
// recorded. The next scan then reads the folder as an ordinary folder of the
// sync folder's own disk: its content is compared with the baseline like any
// other, so a disk's content restored into a plain folder is kept, not
// deleted and uploaded again (R-6.2.18). The caller holds the store's
// offline-change lock (LockDriveForOfflineChange) and has checked that the
// folder holds content on the sync folder's disk: forgetting the record of an
// empty folder would make a disk that is not mounted read as deletes.
func ForgetMountBoundary(ctx context.Context, dbPath string, path string, logger *slog.Logger) (_ bool, err error) {
	if logger != nil {
		logger.Info("forgetting recorded mount point",
			slog.String("db_path", dbPath),
			slog.String("path", path),
		)
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return false, fmt.Errorf("open sync store: %w", err)
	}
	defer func() {
		if closeErr := store.Close(ctx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close sync store: %w", closeErr))
		}
	}()

	result, err := store.db.ExecContext(ctx, `DELETE FROM mount_boundaries WHERE path = ?`, path)
	if err != nil {
		return false, fmt.Errorf("sync: forgetting mount boundary %s: %w", path, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sync: forgetting mount boundary %s: %w", path, err)
	}

	return removed > 0, nil
}

// ReadMountBoundaries reads, through a read-only inspector, the folders the
// drive's sync state at dbPath records as crossed mount points.
func ReadMountBoundaries(ctx context.Context, dbPath string, logger *slog.Logger) ([]string, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) ([]string, error) {
		return listMountBoundariesTx(ctx, inspector.db)
	})
}
