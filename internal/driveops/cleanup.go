package driveops

import "log/slog"

// CleanTransferArtifacts runs non-critical post-sync housekeeping:
//   - Deletes orphaned .partial files (always garbage after sync completes)
//   - Cleans expired upload session files from the session store
//
// Errors are logged but not propagated — housekeeping should never
// fail a sync run.
func CleanTransferArtifacts(syncRoot string, sessionStore *SessionStore, logger *slog.Logger) {
	if n, err := CleanStalePartials(syncRoot, logger); err != nil {
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
