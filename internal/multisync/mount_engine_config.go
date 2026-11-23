package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/localtrash"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// engineMountConfigForMount derives the sync-owned engine constructor input from
// the control plane's runtime mount spec. Mount-owned fields stay authoritative
// here, including token-owner identity and sync tunables. Parent shortcut-root
// reservations are rebuilt by parent engines from their own sync stores, not
// synthesized by multisync.
func engineMountConfigForMount(mount *mountSpec, dataDir string) (*syncengine.EngineMountConfig, error) {
	if mount == nil {
		return nil, fmt.Errorf("mount is required")
	}

	cfg := &syncengine.EngineMountConfig{
		MountID:                mount.id().String(),
		DBPath:                 mount.statePath(),
		TrashDir:               localtrash.DirForStatePath(mount.statePath()),
		SyncRoot:               mount.syncRoot(),
		DataDir:                dataDir,
		DriveID:                mount.remoteDriveID(),
		DriveType:              mount.parentDriveType(),
		AccountEmail:           mount.accountEmail(),
		AccountCanonicalID:     mount.tokenOwnerCanonical(),
		RemoteRootItemID:       mount.remoteRootItemID(),
		RemoteRootDeltaCapable: mount.remoteRootDeltaCapable(),
		EnableWebsocket:        mount.enableWebsocket(),
		LocalRules: syncdomain.LocalObservationRules{
			RejectSharePointRootForms: mount.rejectSharePointRootForms(),
			PersonalNameRules:         mount.personalNameRules(),
		},
		ShortcutNamespaceID:   mount.id().String(),
		ShortcutChildWorkSink: mount.shortcutChildWorkSink(),
		TransferWorkers:       mount.transferWorkers(),
		CheckWorkers:          mount.checkWorkers(),
		MinFreeSpace:          mount.minFreeSpace(),
		ContentFilter:         mount.contentFilter(),
	}
	if mount.projectionKind() == MountProjectionChild {
		trash, err := shortcutChildTrashDirs(dataDir, mount.id().String())
		if err != nil {
			return nil, err
		}
		cfg.TrashDir = trash.child
		if err := syncengine.ApplyShortcutChildRunCommandToEngineMountConfig(
			mount.shortcutChildRunCommand(),
			cfg,
		); err != nil {
			return nil, fmt.Errorf("apply shortcut settings: %w", err)
		}
	}
	return cfg, nil
}
