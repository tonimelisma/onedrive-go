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
	return finalizePendingMountRemovalsWithCleanupMode(mountIDs, mounts, logger, false)
}

func finalizePendingMountRemovalsAfterFinalDrain(mountIDs []string, mounts []*mountSpec, logger *slog.Logger) (bool, error) {
	return finalizePendingMountRemovalsWithCleanupMode(mountIDs, mounts, logger, true)
}

func finalizePendingMountRemovalsWithCleanupMode(
	mountIDs []string,
	mounts []*mountSpec,
	logger *slog.Logger,
	removeProjectionAfterDrain bool,
) (bool, error) {
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
			recordFinalized, err := finalizePendingMountRemovalRecord(
				inventory,
				mountID,
				&record,
				parent,
				logger,
				removeProjectionAfterDrain,
			)
			if err != nil {
				return err
			}
			finalized = finalized || recordFinalized
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("updating mount inventory after child mount removal: %w", err)
	}

	return finalized, nil
}

func finalizePendingMountRemovalRecord(
	inventory *config.MountInventory,
	mountID string,
	record *config.MountRecord,
	parent *mountSpec,
	logger *slog.Logger,
	removeProjectionAfterDrain bool,
) (bool, error) {
	cleaned, reason := cleanupPendingProjectionRoot(parent, record, removeProjectionAfterDrain)
	if !cleaned {
		plan, err := planPendingRemovalCleanup(record, false, reason)
		if err != nil {
			return false, err
		}
		inventory.Mounts[mountID] = plan.Record
		return false, nil
	}

	plan, err := planPendingRemovalCleanup(record, true, "")
	if err != nil {
		return false, err
	}
	if err := syncengine.RemoveStateDBFiles(config.MountStatePath(mountID)); err != nil {
		blocked, blockedErr := planPendingRemovalCleanup(record, false, config.MountStateReasonRemovedProjectionUnavailable)
		if blockedErr != nil {
			return false, blockedErr
		}
		inventory.Mounts[mountID] = blocked.Record
		if logger != nil {
			logger.Warn("purging removed child mount state",
				"mount_id", mountID,
				"error", err.Error(),
			)
		}
		return false, nil
	}
	if plan.RemoveRecord {
		delete(inventory.Mounts, mountID)
	}
	return true, nil
}

func cleanupPendingProjectionRoot(
	parent *mountSpec,
	record *config.MountRecord,
	removeProjectionAfterDrain bool,
) (bool, config.MountStateReason) {
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
	if removeProjectionAfterDrain {
		if removeErr := root.RemoveTreeNoFollow(rel); removeErr != nil {
			return false, config.MountStateReasonRemovedProjectionUnavailable
		}
		return true, ""
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
