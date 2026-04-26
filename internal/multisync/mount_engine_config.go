package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// engineMountConfigForMount derives the sync-owned engine constructor input from
// the control plane's runtime mount spec. Mount-owned fields stay authoritative
// here, including token-owner identity and sync tunables. Parent shortcut-root
// reservations are rebuilt by parent engines from their own sync stores, not
// synthesized by multisync.
func engineMountConfigForMount(mount *mountSpec) (*syncengine.EngineMountConfig, error) {
	if mount == nil {
		return nil, fmt.Errorf("multisync: mount is required")
	}

	return &syncengine.EngineMountConfig{
		DBPath:                 mount.statePath,
		SyncRoot:               mount.syncRoot,
		DataDir:                config.DefaultDataDir(),
		DriveID:                mount.remoteDriveID,
		DriveType:              mount.driveType,
		AccountEmail:           mount.accountEmail,
		RemoteRootItemID:       mount.remoteRootItemID,
		RemoteRootDeltaCapable: mount.remoteRootDeltaCapable,
		EnableWebsocket:        mount.enableWebsocket,
		LocalFilter:            syncengine.LocalFilterConfig{},
		LocalRules: syncengine.LocalObservationRules{
			RejectSharePointRootForms: mount.rejectSharePointRootForms,
		},
		ShortcutTopologyNamespaceID: mount.mountID.String(),
		ShortcutTopologyHandler:     mount.shortcutTopologyHandler,
		TransferWorkers:             mount.transferWorkers,
		CheckWorkers:                mount.checkWorkers,
		MinFreeSpace:                mount.minFreeSpace,
	}, nil
}
