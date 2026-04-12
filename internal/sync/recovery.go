// Package sync owns startup crash-recovery decisions that combine
// durable sync-store state with current sync-tree filesystem truth.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ResetInProgressStates performs startup crash recovery using the sync tree to
// decide whether deleting rows completed before the previous process died. The
// store owns only durable state transitions and sync_failures persistence.
func ResetInProgressStates(
	ctx context.Context,
	store crashRecoveryStore,
	tree *synctree.Root,
	delayFn func(int) time.Duration,
	logger *slog.Logger,
) error {
	if err := store.ResetDownloadingStates(ctx, delayFn); err != nil {
		return fmt.Errorf("reset downloading states: %w", err)
	}

	candidates, err := store.ListDeletingCandidates(ctx)
	if err != nil {
		return fmt.Errorf("list deleting candidates: %w", err)
	}

	var (
		deleted []syncstore.RecoveryCandidate
		pending []syncstore.RecoveryCandidate
	)

	for _, candidate := range candidates {
		relPath := strings.TrimPrefix(candidate.Path, "/")
		_, statErr := tree.Stat(relPath)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			deleted = append(deleted, candidate)
		case statErr != nil:
			logger.Warn("crash recovery delete stat failed; retrying delete",
				slog.String("path", candidate.Path),
				slog.String("error", statErr.Error()),
			)
			pending = append(pending, candidate)
		default:
			pending = append(pending, candidate)
		}
	}

	if err := store.FinalizeDeletingStates(ctx, deleted, pending, delayFn); err != nil {
		return fmt.Errorf("finalize deleting states: %w", err)
	}

	return nil
}
