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
		DBPath:                    mount.statePath,
		SyncRoot:                  mount.syncRoot,
		DataDir:                   config.DefaultDataDir(),
		DriveID:                   mount.remoteDriveID,
		DriveType:                 mount.canonicalID.DriveType(),
		AccountEmail:              mount.accountEmail,
		RootItemID:                mount.remoteRootItemID,
		RootedSubtreeDeltaCapable: mount.rootedSubtreeDeltaCapable,
		EnableWebsocket:           mount.enableWebsocket,
		LocalFilter: syncengine.LocalFilterConfig{
			SkipDirs: append([]string(nil), mount.localSkipDirs...),
		},
		LocalRules: syncengine.LocalObservationRules{
			RejectSharePointRootForms: mount.canonicalID.IsSharePoint(),
		},
		TransferWorkers: mount.transferWorkers,
		CheckWorkers:    mount.checkWorkers,
		MinFreeSpace:    mount.minFreeSpace,
	}, nil
}
