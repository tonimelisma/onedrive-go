package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func runDriveRemoveWithContext(ctx context.Context, cc *CLIContext, purge bool) error {
	logger := cc.Logger

	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive to remove)")
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, inConfig, err := resolveExactDriveSelector(cfg, driveSelector)
	if err != nil {
		return err
	}
	if !inConfig && !purge {
		return fmt.Errorf("drive %q not found in config — use --purge to clean up leftover state", driveSelector)
	}

	if !inConfig && purge {
		logger.Info("purging orphaned drive state", "drive", cid.String())
		if err := purgeOrphanedDriveState(ctx, cc.resultWriter(), cid, logger); err != nil {
			return err
		}
		if err := config.PruneDriveAfterPurge(config.DefaultDataDir(), cid); err != nil {
			return fmt.Errorf("pruning catalog drive after purge: %w", err)
		}
		return reportDriveRemove(cc, driveRemoveJSONOutput{Drive: cid.String(), Purged: true})
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)
	if purge {
		// The catalog keeps the drive's record until its state is gone: a
		// purge that kept the store is finished by the orphaned-state path
		// above, which prunes it then.
		if err := purgeConfiguredDrive(ctx, cc, cid, driveSyncDir(cfg, cid, logger)); err != nil {
			return err
		}
		if err := config.PruneDriveAfterPurge(config.DefaultDataDir(), cid); err != nil {
			return fmt.Errorf("pruning catalog drive after purge: %w", err)
		}
		return reportDriveRemove(cc, driveRemoveJSONOutput{Drive: cid.String(), RemovedFromConfig: true, Purged: true})
	}

	// The folder named is the one the drive synced: the configured sync_dir
	// is empty for a drive on its default folder.
	if err := removeDrive(cc.resultWriter(), cc.CfgPath, cid, driveSyncDir(cfg, cid, logger), logger); err != nil {
		return err
	}
	notifyDaemonIfRunning(ctx, cc)
	return reportDriveRemove(cc, driveRemoveJSONOutput{Drive: cid.String(), RemovedFromConfig: true})
}

// driveRemoveJSONOutput is `drive remove --json`. removed_from_config is
// false when --purge only cleaned up state left behind by an earlier remove;
// purged says the state database was deleted too.
type driveRemoveJSONOutput struct {
	Drive             string `json:"drive"`
	RemovedFromConfig bool   `json:"removed_from_config"`
	Purged            bool   `json:"purged"`
}

func reportDriveRemove(cc *CLIContext, result driveRemoveJSONOutput) error {
	if !cc.Flags.JSON {
		return nil
	}

	return writeJSONDocument(cc.Output(), result)
}
