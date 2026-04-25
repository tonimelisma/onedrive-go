package multisync

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func recordShortcutLifecyclePlan(
	id mountID,
	logger *slog.Logger,
	warnMessage string,
	planFn func(*config.MountRecord) (shortcutLifecyclePlan, error),
) bool {
	updateErr := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[id.String()]
		if !found {
			return nil
		}
		plan, err := planFn(&record)
		if err != nil {
			return err
		}
		inventory.Mounts[plan.Record.MountID] = plan.Record
		return nil
	})
	if updateErr != nil && logger != nil {
		logger.Warn(warnMessage,
			slog.String("mount_id", id.String()),
			slog.String("error", updateErr.Error()),
		)
		return false
	}

	return true
}
