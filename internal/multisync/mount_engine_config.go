package multisync

import (
	"fmt"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// engineMountConfigForMount derives the sync-owned engine constructor input from
// the control plane's runtime mount spec. Mount-owned fields stay authoritative
// here, including token-owner identity and sync tunables. Parent shortcut-root
// reservations are rebuilt by parent engines from their own sync stores, not
// synthesized by multisync.
func engineMountConfigForMount(mount *mountSpec, dataDir string) (*syncengine.EngineMountConfig, error) {
	if mount == nil {
		return nil, fmt.Errorf("multisync: mount is required")
	}

	return &syncengine.EngineMountConfig{
		MountID:                mount.mountID.String(),
		DBPath:                 mount.statePath,
		SyncRoot:               mount.syncRoot,
		DataDir:                dataDir,
		DriveID:                mount.remoteDriveID,
		DriveType:              mount.driveType,
		AccountEmail:           mount.accountEmail,
		RemoteRootItemID:       mount.remoteRootItemID,
		RemoteRootDeltaCapable: mount.remoteRootDeltaCapable,
		ExpectedSyncRootIdentity: cloneShortcutChildEngineSpec(syncengine.ShortcutChildEngineSpec{
			LocalRootIdentity: mount.expectedChildRootIdentity(),
		}).LocalRootIdentity,
		EnableWebsocket: mount.enableWebsocket,
		LocalRules: syncengine.LocalObservationRules{
			RejectSharePointRootForms: mount.rejectSharePointRootForms,
		},
		ShortcutNamespaceID:      mount.mountID.String(),
		ShortcutChildProcessSink: mount.parentChildProcessSink,
		TransferWorkers:          mount.transferWorkers,
		CheckWorkers:             mount.checkWorkers,
		MinFreeSpace:             mount.minFreeSpace,
	}, nil
}
