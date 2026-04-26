package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping(ctx context.Context) {
	if err := flow.engine.refreshProtectedRootsFromStore(ctx); err != nil && flow.engine.logger != nil {
		flow.engine.logger.Warn("refresh protected roots before housekeeping",
			"error", err.Error(),
		)
	}
	driveops.CleanTransferArtifactsWithOptions(
		flow.engine.syncTree,
		flow.engine.sessionStore,
		flow.engine.logger,
		driveops.TransferArtifactCleanupOptions{
			SkipDirs: transferArtifactCleanupSkipDirs(flow.engine.localFilter, flow.engine.protectedRoots),
		},
	)
}

func transferArtifactCleanupSkipDirs(filter LocalFilterConfig, protectedRoots []ProtectedRoot) []string {
	skipDirs := append([]string(nil), filter.SkipDirs...)
	for i := range protectedRoots {
		if protectedRoots[i].Path == "" {
			continue
		}
		skipDirs = append(skipDirs, protectedRoots[i].Path)
	}
	return skipDirs
}
