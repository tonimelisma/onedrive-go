package syncstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ReadStatusSnapshot opens a read-only inspector for one status projection and
// closes it before returning so callers do not own inspector lifecycle.
func ReadStatusSnapshot(ctx context.Context, dbPath string, logger *slog.Logger) (StatusSnapshot, error) {
	return readWithInspector(dbPath, logger, func(inspector *Inspector) (StatusSnapshot, error) {
		return inspector.ReadStatusSnapshot(ctx), nil
	})
}

// ReadDriveStatusSnapshot opens a read-only inspector for one per-drive status
// projection and closes it before returning so callers do not own inspector
// lifecycle.
func ReadDriveStatusSnapshot(
	ctx context.Context,
	dbPath string,
	history bool,
	logger *slog.Logger,
) (DriveStatusSnapshot, error) {
	return readWithInspector(dbPath, logger, func(inspector *Inspector) (DriveStatusSnapshot, error) {
		return inspector.ReadDriveStatusSnapshot(ctx, history)
	})
}

// ReadDurableIntentCounts opens a read-only inspector for one durable-intent
// count projection and closes it before returning.
func ReadDurableIntentCounts(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
) (DurableIntentCounts, error) {
	return readWithInspector(dbPath, logger, func(inspector *Inspector) (DurableIntentCounts, error) {
		return inspector.ReadDurableIntentCounts(ctx)
	})
}

// HasScopeBlockAtPath answers one scope-block existence query without forcing
// callers to manage inspector open/close.
func HasScopeBlockAtPath(
	ctx context.Context,
	dbPath string,
	key synctypes.ScopeKey,
	logger *slog.Logger,
) (bool, error) {
	return readWithInspector(dbPath, logger, func(inspector *Inspector) (bool, error) {
		return inspector.HasScopeBlock(ctx, key)
	})
}

func readWithInspector[T any](
	dbPath string,
	logger *slog.Logger,
	read func(*Inspector) (T, error),
) (result T, err error) {
	inspector, err := OpenInspector(dbPath, logger)
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
