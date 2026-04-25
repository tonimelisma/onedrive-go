package multisync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func finalizePendingMountRemovals(mountIDs []string, mounts []*mountSpec, logger *slog.Logger) (bool, error) {
	if len(mountIDs) == 0 {
		return false, nil
	}

	finalized := false
	parentByID := make(map[string]*mountSpec, len(mounts))
	for i := range mounts {
		mount := mounts[i]
		if mount.projectionKind == MountProjectionStandalone {
			parentByID[mount.mountID.String()] = mount
		}
	}
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		for _, mountID := range mountIDs {
			record, found := inventory.Mounts[mountID]
			if !found || record.State != config.MountStatePendingRemoval {
				continue
			}
			parent := parentByID[record.NamespaceID]
			if parent == nil {
				continue
			}
			cleaned, reason := cleanupPendingProjectionRoot(parent, &record)
			if !cleaned {
				record.StateReason = reason
				inventory.Mounts[mountID] = record
				continue
			}
			if err := syncengine.RemoveStateDBFiles(config.MountStatePath(mountID)); err != nil {
				record.StateReason = config.MountStateReasonRemovedProjectionUnavailable
				inventory.Mounts[mountID] = record
				if logger != nil {
					logger.Warn("purging removed child mount state",
						"mount_id", mountID,
						"error", err.Error(),
					)
				}
				continue
			}
			delete(inventory.Mounts, mountID)
			finalized = true
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("updating mount inventory after child mount removal: %w", err)
	}

	return finalized, nil
}

func cleanupPendingProjectionRoot(parent *mountSpec, record *config.MountRecord) (bool, config.MountStateReason) {
	root, err := synctree.Open(parent.syncRoot)
	if err != nil {
		return false, config.MountStateReasonRemovedProjectionUnavailable
	}
	rel, err := childProjectionRelativePath(record.RelativeLocalPath)
	if err != nil {
		return false, config.MountStateReasonRemovedProjectionUnavailable
	}
	state, err := root.PathStateNoFollow(rel)
	if errors.Is(err, os.ErrNotExist) {
		return true, ""
	}
	if err != nil {
		return false, config.MountStateReasonRemovedProjectionUnavailable
	}
	if !state.Exists {
		return true, ""
	}
	if !state.IsDir {
		return false, config.MountStateReasonRemovedProjectionDirty
	}
	empty, err := root.DirEmptyNoFollow(rel)
	if err != nil {
		return false, config.MountStateReasonRemovedProjectionUnavailable
	}
	if !empty {
		return false, config.MountStateReasonRemovedProjectionDirty
	}
	if err := root.RemoveTreeNoFollow(rel); err != nil {
		return false, config.MountStateReasonRemovedProjectionUnavailable
	}
	return true, ""
}
