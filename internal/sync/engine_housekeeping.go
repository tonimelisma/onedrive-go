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
//
// The transfer-artifact cleanup walks everything but the protected roots, and
// removes every owned partial it finds with no age check. It therefore runs
// only when the protected-root list was refreshed for this pass: a stale list
// could let the parent delete a child mount's in-flight partials.
func (flow *engineFlow) postSyncHousekeeping(ctx context.Context) {
	if flow.engine.refreshProtectedRootsForHousekeeping(ctx) {
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
	}
	flow.engine.purgeExpiredTrash(true)
}

// refreshProtectedRootsForHousekeeping refreshes the protected-root list and
// reports whether housekeeping may walk the tree. A failure is one Warn per
// streak (R-6.6.30), Debug on repeats, and the streak ends when a refresh
// succeeds; the cleanup it guards is skipped for the pass, not run stale.
func (e *Engine) refreshProtectedRootsForHousekeeping(ctx context.Context) bool {
	err := e.refreshProtectedRootsFromStore(ctx)
	if err == nil {
		if e.protectedRootRefreshFailing.Swap(false) {
			e.logger.Info("shared-folder list refreshed again; transfer cleanup resumes")
		}

		return true
	}

	if e.protectedRootRefreshFailing.Swap(true) {
		e.logger.Debug("refresh protected roots before housekeeping failed again; skipping transfer cleanup",
			slog.Any("error", err))
	} else {
		e.logger.Warn("Could not refresh the shared-folder list; skipping transfer-leftover cleanup until it can be read",
			slog.Any("error", err))
	}

	return false
}

// trashPurgeInterval bounds how often a long-running watch session scans the
// trash for expired entries.
const trashPurgeInterval = time.Hour

// purgeExpiredTrash removes trash entries older than the retention period.
// It is best effort: a failure leaves entries for the next attempt. A purge
// that keeps failing is reported once per streak at Warn, naming the trash
// directory, because the trash then grows without bound and nothing else
// says so; repeats are Debug and recovery ends the streak (R-6.6.30).
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
	switch {
	case err == nil:
		if e.trashPurgeFailing.Swap(false) {
			e.logger.Info("expired files can be removed from the sync trash again",
				slog.String("trash", e.trash.Dir()))
		}
	case e.trashPurgeFailing.Swap(true):
		e.logger.Debug("could not purge expired trash entries (still failing)",
			slog.String("trash", e.trash.Dir()), slog.Any("error", err))
	default:
		e.logger.Warn("Could not remove expired files from the sync trash; it will keep growing until this is fixed",
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
