package sync

import "github.com/tonimelisma/onedrive-go/internal/driveops"

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping() {
	driveops.CleanTransferArtifacts(flow.engine.syncTree, flow.engine.sessionStore, flow.engine.logger)
}
