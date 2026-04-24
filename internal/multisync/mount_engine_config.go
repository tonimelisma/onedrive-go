package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// engineMountConfigForMount derives the sync-owned engine constructor input from
// the control plane's runtime mount spec. Mount-owned fields stay authoritative
// here, including token-owner identity, sync tunables, and local child
// projection exclusions.
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
		LocalFilter: syncengine.LocalFilterConfig{
			SkipDirs: append([]string(nil), mount.localSkipDirs...),
		},
		LocalRules: syncengine.LocalObservationRules{
			RejectSharePointRootForms: mount.rejectSharePointRootForms,
		},
		TransferWorkers: mount.transferWorkers,
		CheckWorkers:    mount.checkWorkers,
		MinFreeSpace:    mount.minFreeSpace,
	}, nil
}
