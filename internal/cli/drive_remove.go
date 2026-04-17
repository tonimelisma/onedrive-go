package cli

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func runDriveRemoveWithContext(cc *CLIContext, purge bool) error {
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

	cid, cidErr := driveid.NewCanonicalID(driveSelector)
	if cidErr != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveSelector, cidErr)
	}

	_, inConfig := cfg.Drives[cid]
	if !inConfig && !purge {
		return fmt.Errorf("drive %q not found in config — use --purge to clean up leftover state", driveSelector)
	}

	if !inConfig && purge {
		logger.Info("purging orphaned drive state", "drive", cid.String())
		if err := purgeOrphanedDriveState(cc.Output(), cid, logger); err != nil {
			return err
		}
		if err := config.PruneDriveAfterPurge(config.DefaultDataDir(), cid); err != nil {
			return fmt.Errorf("pruning catalog drive after purge: %w", err)
		}
		return nil
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)
	if purge {
		if err := purgeDrive(cc.Output(), cc.CfgPath, cid, logger); err != nil {
			return err
		}
		if err := config.PruneDriveAfterPurge(config.DefaultDataDir(), cid); err != nil {
			return fmt.Errorf("pruning catalog drive after purge: %w", err)
		}
		return nil
	}

	if err := removeDrive(cc.Output(), cc.CfgPath, cid, cfg.Drives[cid].SyncDir, logger); err != nil {
		return err
	}
	return nil
}
