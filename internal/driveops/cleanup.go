package driveops

import (
	"log/slog"
	"time"
)

// CleanTransferArtifacts runs non-critical post-sync housekeeping:
//   - Reports stale .partial files (warn-only, does not delete)
//   - Cleans expired upload session files from the session store
//
// Errors are logged but not propagated — housekeeping should never
// fail a sync cycle.
func CleanTransferArtifacts(
	syncRoot string, sessionStore *SessionStore,
	stalePartialAge, staleSessionAge time.Duration, logger *slog.Logger,
) {
	ReportStalePartials(syncRoot, stalePartialAge, logger)

	if sessionStore != nil {
		if n, err := sessionStore.CleanStale(staleSessionAge); err != nil {
			logger.Warn("stale session cleanup failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.Info("cleaned stale upload sessions", slog.Int("count", n))
		}
	}
}
