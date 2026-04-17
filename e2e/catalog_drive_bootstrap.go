package e2e

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type catalogDriveFetcher func(context.Context, string) (*config.CatalogDrive, error)

func ensureCatalogDriveRecords(
	ctx context.Context,
	driveIDs []string,
	fetch catalogDriveFetcher,
) error {
	for _, driveID := range driveIDs {
		if err := ensureCatalogDriveRecordForDrive(ctx, driveID, fetch); err != nil {
			return err
		}
	}

	return nil
}

func ensureCatalogDriveRecordForDrive(
	ctx context.Context,
	driveID string,
	fetch catalogDriveFetcher,
) error {
	cid, err := driveid.NewCanonicalID(driveID)
	if err != nil {
		return fmt.Errorf("parse test drive canonical ID %q: %w", driveID, err)
	}

	catalog, loadErr := config.LoadCatalog()
	if loadErr != nil {
		return fmt.Errorf("load catalog drive record for %s: %w", driveID, loadErr)
	}
	drive, found := catalog.DriveByCanonicalID(cid)
	if found && drive.RemoteDriveID != "" {
		return nil
	}

	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		if fetch == nil {
			return fmt.Errorf("recover catalog drive record for %s: nil fetcher", driveID)
		}

		recoveredDrive, fetchErr := fetch(ctx, driveID)
		if fetchErr != nil {
			return fmt.Errorf("recover catalog drive record for %s: %w", driveID, fetchErr)
		}
		if recoveredDrive == nil || recoveredDrive.RemoteDriveID == "" {
			return fmt.Errorf("recover catalog drive record for %s: empty drive ID", driveID)
		}

		recoveredDrive.CanonicalID = cid.String()
		recoveredDrive.DriveType = cid.DriveType()
		if recoveredDrive.OwnerAccountCanonical == "" {
			recoveredDrive.OwnerAccountCanonical = cid.String()
		}
		if saveErr := config.UpdateCatalog(func(catalog *config.Catalog) error {
			catalog.UpsertDrive(recoveredDrive)
			return nil
		}); saveErr != nil {
			return fmt.Errorf("save catalog drive record for %s: %w", driveID, saveErr)
		}

		return nil

	case cid.IsSharePoint():
		return fmt.Errorf(
			"missing catalog drive record for SharePoint test drive %s; re-run scripts/bootstrap-test-credentials.sh",
			driveID,
		)

	case cid.IsShared():
		return fmt.Errorf(
			"missing catalog drive record for shared-root test drive %s; re-run scripts/bootstrap-test-credentials.sh",
			driveID,
		)

	default:
		return fmt.Errorf("missing catalog drive record for unsupported test drive %s", driveID)
	}
}
