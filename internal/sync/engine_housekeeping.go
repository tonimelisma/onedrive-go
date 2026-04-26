package sync

import "github.com/tonimelisma/onedrive-go/internal/driveops"

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping() {
	driveops.CleanTransferArtifactsWithOptions(
		flow.engine.syncTree,
		flow.engine.sessionStore,
		flow.engine.logger,
		driveops.TransferArtifactCleanupOptions{
			SkipDirs: transferArtifactCleanupSkipDirs(flow.engine.localFilter),
		},
	)
}

func transferArtifactCleanupSkipDirs(filter LocalFilterConfig) []string {
	skipDirs := append([]string(nil), filter.SkipDirs...)
	for i := range filter.ManagedRoots {
		if filter.ManagedRoots[i].Path == "" {
			continue
		}
		skipDirs = append(skipDirs, filter.ManagedRoots[i].Path)
	}
	return skipDirs
}
