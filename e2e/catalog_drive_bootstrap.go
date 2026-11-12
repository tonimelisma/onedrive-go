package e2e

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/testutil"
)

type catalogDriveFetcher func(context.Context, string) (*config.CatalogDrive, error)

// ensureCatalogDriveRecords makes sure the catalog has a record of every test
// drive. A failure names the drive by role, drive-1 for the first: the
// message reaches the public CI log.
func ensureCatalogDriveRecords(
	ctx context.Context,
	driveIDs []string,
	fetch catalogDriveFetcher,
) error {
	roles := testutil.NewDriveRoles(driveIDs...)
	for _, driveID := range driveIDs {
		if err := ensureCatalogDriveRecordForDrive(ctx, driveID, fetch); err != nil {
			return fmt.Errorf("catalog drive record of test drive %s: %w", roles.Role(driveID), err)
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
		return fmt.Errorf("parse test drive canonical ID: %w", err)
	}

	catalog, loadErr := config.LoadCatalog()
	if loadErr != nil {
		return fmt.Errorf("load: %w", loadErr)
	}
	drive, found := catalog.DriveByCanonicalID(cid)
	if found && drive.RemoteDriveID != "" {
		return nil
	}

	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		if fetch == nil {
			return fmt.Errorf("recover: nil fetcher")
		}

		recoveredDrive, fetchErr := fetch(ctx, driveID)
		if fetchErr != nil {
			return fmt.Errorf("recover: %w", fetchErr)
		}
		if recoveredDrive == nil || recoveredDrive.RemoteDriveID == "" {
			return fmt.Errorf("recover: empty drive ID")
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
			return fmt.Errorf("save: %w", saveErr)
		}

		return nil

	case cid.IsSharePoint():
		return fmt.Errorf("missing catalog drive record for SharePoint test drive; re-run scripts/bootstrap-test-credentials.sh")

	case cid.IsShared():
		return fmt.Errorf("missing catalog drive record for shared-root test drive; re-run scripts/bootstrap-test-credentials.sh")

	default:
		return fmt.Errorf("missing catalog drive record for unsupported test drive")
	}
}
