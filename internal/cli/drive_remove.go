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
	stored, err := config.LoadCatalog()
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
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
		prunePurgedDriveCatalogEntry(stored, cid)
		return saveCatalog(stored)
	}

	logger.Info("removing drive", "drive", cid.String(), "purge", purge)
	if purge {
		if err := purgeDrive(cc.Output(), cc.CfgPath, cid, logger); err != nil {
			return err
		}
		prunePurgedDriveCatalogEntry(stored, cid)
		return saveCatalog(stored)
	}

	if err := removeDrive(cc.Output(), cc.CfgPath, cid, cfg.Drives[cid].SyncDir, logger); err != nil {
		return err
	}
	return saveCatalog(stored)
}

func prunePurgedDriveCatalogEntry(stored *config.Catalog, cid driveid.CanonicalID) {
	if stored == nil || cid.IsZero() {
		return
	}

	drive, found := stored.DriveByCanonicalID(cid)
	if !found {
		return
	}

	drive.RetainedStatePresent = false
	accountCID, accountErr := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
	if drive.PrimaryForAccount || (accountErr == nil && accountOwnsPrimaryDrive(stored, accountCID, cid)) {
		stored.UpsertDrive(&drive)
		return
	}

	stored.DeleteDrive(cid)
}

func accountOwnsPrimaryDrive(stored *config.Catalog, accountCID, driveCID driveid.CanonicalID) bool {
	if stored == nil || accountCID.IsZero() || driveCID.IsZero() {
		return false
	}

	account, found := stored.AccountByCanonicalID(accountCID)
	if !found {
		return false
	}

	return account.PrimaryDriveCanonical == driveCID.String()
}
