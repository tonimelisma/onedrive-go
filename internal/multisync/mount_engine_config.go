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

	cfg := &syncengine.EngineMountConfig{
		MountID:                mount.id().String(),
		DBPath:                 mount.statePath(),
		SyncRoot:               mount.syncRoot(),
		DataDir:                dataDir,
		DriveID:                mount.remoteDriveID(),
		DriveType:              mount.parentDriveType(),
		AccountEmail:           mount.accountEmail(),
		RemoteRootItemID:       mount.remoteRootItemID(),
		RemoteRootDeltaCapable: mount.remoteRootDeltaCapable(),
		EnableWebsocket:        mount.enableWebsocket(),
		LocalRules: syncengine.LocalObservationRules{
			RejectSharePointRootForms: mount.rejectSharePointRootForms(),
		},
		ShortcutNamespaceID:   mount.id().String(),
		ShortcutChildWorkSink: mount.shortcutChildWorkSink(),
		TransferWorkers:       mount.transferWorkers(),
		CheckWorkers:          mount.checkWorkers(),
		MinFreeSpace:          mount.minFreeSpace(),
	}
	if mount.projectionKind() == MountProjectionChild {
		if err := syncengine.ApplyShortcutChildRunCommandToEngineMountConfig(
			mount.shortcutChildRunCommand(),
			cfg,
		); err != nil {
			return nil, fmt.Errorf("multisync: apply parent-declared child engine input: %w", err)
		}
	}
	return cfg, nil
}
