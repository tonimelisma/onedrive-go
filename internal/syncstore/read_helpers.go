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

// ReadIssuesSnapshot opens a read-only inspector for one issues projection and
// closes it before returning so callers do not own inspector lifecycle.
func ReadIssuesSnapshot(
	ctx context.Context,
	dbPath string,
	history bool,
	logger *slog.Logger,
) (IssuesSnapshot, error) {
	return readWithInspector(dbPath, logger, func(inspector *Inspector) (IssuesSnapshot, error) {
		return inspector.ReadIssuesSnapshot(ctx, history)
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

// ListConflictsAtPath returns either unresolved or full conflict history for
// one read-only store path and closes the inspector before returning.
func ListConflictsAtPath(
	ctx context.Context,
	dbPath string,
	history bool,
	logger *slog.Logger,
) ([]synctypes.ConflictRecord, error) {
	return readWithInspector(dbPath, logger, func(inspector *Inspector) ([]synctypes.ConflictRecord, error) {
		if history {
			return inspector.ListAllConflicts(ctx)
		}

		return inspector.ListConflicts(ctx)
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
