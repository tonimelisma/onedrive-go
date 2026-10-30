package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping(ctx context.Context) {
	if err := flow.engine.refreshProtectedRootsFromStore(ctx); err != nil && flow.deps.logger != nil {
		flow.deps.logger.Debug("refresh protected roots before housekeeping",
			"error", err.Error(),
		)
	}
	driveops.CleanTransferArtifactsWithOptions(
		flow.engine.syncTree,
		flow.engine.sessionStore,
		flow.deps.logger,
		driveops.TransferArtifactCleanupOptions{
			SkipDirs:            transferArtifactCleanupSkipDirs(flow.engine.contentFilter, flow.engine.protectedRoots),
			IncludedDirs:        flow.engine.contentFilter.IncludedDirs,
			IncludeJunkPartials: flow.engine.contentFilter.IgnoreJunkFiles,
		},
	)
	flow.engine.purgeExpiredTrash(true)
}

// trashPurgeInterval bounds how often a long-running watch session scans the
// trash for expired entries.
const trashPurgeInterval = time.Hour

// purgeExpiredTrash removes trash entries older than the retention period.
// It is best effort: a failure leaves entries for the next attempt.
func (e *Engine) purgeExpiredTrash(force bool) {
	if e == nil || e.trash == nil {
		return
	}
	now := e.nowFunc()
	if last := e.trashPurgedAt.Load(); !force && last != 0 && now.Sub(time.Unix(0, last)) < trashPurgeInterval {
		return
	}
	e.trashPurgedAt.Store(now.UnixNano())

	removed, err := e.trash.Purge()
	if err != nil {
		e.logger.Debug("could not purge expired trash entries",
			slog.String("trash", e.trash.Dir()), slog.Any("error", err))
	}
	if removed > 0 {
		e.logger.Info("removed expired files from the sync trash",
			slog.Int("removed", removed), slog.String("trash", e.trash.Dir()))
	}
}

func transferArtifactCleanupSkipDirs(filter ContentFilterConfig, protectedRoots []ProtectedRoot) []string {
	skipDirs := append([]string(nil), filter.IgnoredDirs...)
	for i := range protectedRoots {
		if protectedRoots[i].Path == "" {
			continue
		}
		skipDirs = append(skipDirs, protectedRoots[i].Path)
	}
	return skipDirs
}
