package multisync

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func purgeManagedMountStateDBs(logger *slog.Logger, mountIDs []string) error {
	var errs []error
	for _, mountID := range mountIDs {
		if mountID == "" {
			continue
		}
		if err := syncengine.RemoveStateDBFiles(config.MountStatePath(mountID)); err != nil {
			errs = append(errs, fmt.Errorf("purging removed child mount state %s: %w", mountID, err))
			continue
		}
		if logger != nil {
			logger.Info("purged removed child mount state",
				"mount_id", mountID,
			)
		}
	}

	return errors.Join(errs...)
}

func finalizePendingMountRemovals(mountIDs []string) (bool, error) {
	if len(mountIDs) == 0 {
		return false, nil
	}

	finalized := false
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		for _, mountID := range mountIDs {
			record, found := inventory.Mounts[mountID]
			if !found || record.State != config.MountStatePendingRemoval {
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
