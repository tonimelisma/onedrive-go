package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// engineMountConfigForMount derives the sync-owned engine constructor input from
// the control plane's runtime mount spec. Mount-owned fields stay authoritative
// here; unresolved sync tunables continue to flow from the config-backed
// resolved drive until later increments promote them into mount specs.
func engineMountConfigForMount(mount *mountSpec) (*syncengine.EngineMountConfig, error) {
	if mount == nil {
		return nil, fmt.Errorf("multisync: mount is required")
	}
	if mount.resolved == nil {
		return nil, fmt.Errorf("multisync: resolved drive is required for mount %s", mount.mountID)
	}

	minFree, err := config.ParseSize(mount.resolved.MinFreeSpace)
	if err != nil {
		return nil, fmt.Errorf("invalid min_free_space %q: %w", mount.resolved.MinFreeSpace, err)
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
		LocalFilter:               syncengine.LocalFilterConfig{},
		LocalRules: syncengine.LocalObservationRules{
			RejectSharePointRootForms: mount.canonicalID.IsSharePoint(),
		},
		TransferWorkers: mount.resolved.TransferWorkers,
		CheckWorkers:    mount.resolved.CheckWorkers,
		MinFreeSpace:    minFree,
	}, nil
}
