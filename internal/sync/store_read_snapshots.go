package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// ReadDriveStatusSnapshot opens a read-only inspector for one per-drive status
// projection and closes it before returning so callers do not own inspector
// lifecycle.
func ReadDriveStatusSnapshot(
	ctx context.Context,
	dbPath string,
	history bool,
	logger *slog.Logger,
) (DriveStatusSnapshot, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) (DriveStatusSnapshot, error) {
		return inspector.ReadDriveStatusSnapshot(ctx, history)
	})
}

func readWithInspector[T any](
	dbPath string,
	logger *slog.Logger,
	read func(*storeInspector) (T, error),
) (result T, err error) {
	inspector, err := openStoreInspector(dbPath, logger)
	if err != nil {
		return result, fmt.Errorf("open sync store inspector: %w", err)
	}

	defer func() {
		if closeErr := inspector.Close(); closeErr != nil {
			result, err = finalizeInspectorRead(dbPath, logger, result, err, closeErr)
		}
	}()

	return read(inspector)
}

func finalizeInspectorRead[T any](
	dbPath string,
	logger *slog.Logger,
	result T,
	readErr error,
	closeErr error,
) (T, error) {
	if closeErr == nil {
		return result, readErr
	}

	wrappedCloseErr := fmt.Errorf("close sync store inspector %s: %w", dbPath, closeErr)
	if readErr == nil {
		if logger != nil {
			logger.Warn("close sync store inspector after successful read",
				slog.String("path", dbPath),
				slog.Any("error", wrappedCloseErr),
			)
		}

		return result, nil
	}

	return result, errors.Join(readErr, wrappedCloseErr)
}
