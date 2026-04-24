package driveops

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// TransferArtifactCleanupOptions controls local transfer artifact cleanup.
// SkipDirs entries are rooted slash paths relative to the sync root and are
// left untouched during partial-file cleanup.
type TransferArtifactCleanupOptions struct {
	SkipDirs []string
}

// CleanTransferArtifacts runs non-critical post-sync housekeeping:
//   - Deletes orphaned .partial files (always garbage after sync completes)
//   - Cleans expired upload session files from the session store
//
// Errors are logged but not propagated — housekeeping should never
// fail a sync run.
func CleanTransferArtifacts(tree *synctree.Root, sessionStore *SessionStore, logger *slog.Logger) {
	CleanTransferArtifactsWithOptions(tree, sessionStore, logger, TransferArtifactCleanupOptions{})
}

// CleanTransferArtifactsWithOptions runs post-sync housekeeping with local
// subtree exclusions.
func CleanTransferArtifactsWithOptions(
	tree *synctree.Root,
	sessionStore *SessionStore,
	logger *slog.Logger,
	opts TransferArtifactCleanupOptions,
) {
	if n, err := CleanStalePartialsWithOptions(tree, logger, StalePartialCleanupOptions(opts)); err != nil {
		logger.Warn("partial file cleanup failed", slog.String("error", err.Error()))
	} else if n > 0 {
		logger.Info("cleaned stale partial files", slog.Int("count", n))
	}

	if sessionStore != nil {
		if n, err := sessionStore.CleanStale(StaleSessionAge); err != nil {
			logger.Warn("stale session cleanup failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.Info("cleaned stale upload sessions", slog.Int("count", n))
		}
	}
}
