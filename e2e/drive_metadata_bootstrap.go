package e2e

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type driveMetadataFetcher func(context.Context, string) (*config.DriveMetadata, error)

func ensureTestDriveMetadata(
	ctx context.Context,
	driveIDs []string,
	fetch driveMetadataFetcher,
) error {
	for _, driveID := range driveIDs {
		if err := ensureTestDriveMetadataForDrive(ctx, driveID, fetch); err != nil {
			return err
		}
	}

	return nil
}

func ensureTestDriveMetadataForDrive(
	ctx context.Context,
	driveID string,
	fetch driveMetadataFetcher,
) error {
	cid, err := driveid.NewCanonicalID(driveID)
	if err != nil {
		return fmt.Errorf("parse test drive canonical ID %q: %w", driveID, err)
	}

	_, found, lookupErr := config.LookupDriveMetadata(cid)
	if lookupErr != nil {
		return fmt.Errorf("lookup drive metadata for %s: %w", driveID, lookupErr)
	}
	if found {
		return nil
	}

	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		if fetch == nil {
			return fmt.Errorf("recover drive metadata for %s: nil fetcher", driveID)
		}

		meta, fetchErr := fetch(ctx, driveID)
		if fetchErr != nil {
			return fmt.Errorf("recover drive metadata for %s: %w", driveID, fetchErr)
		}
		if meta == nil || meta.DriveID == "" {
			return fmt.Errorf("recover drive metadata for %s: empty drive ID", driveID)
		}

		if saveErr := config.SaveDriveMetadata(cid, meta); saveErr != nil {
			return fmt.Errorf("save drive metadata for %s: %w", driveID, saveErr)
		}

		return nil

	case cid.IsSharePoint():
		return fmt.Errorf(
			"missing drive metadata for SharePoint test drive %s; re-run scripts/bootstrap-test-credentials.sh",
			driveID,
		)

	case cid.IsShared():
		return fmt.Errorf(
			"missing drive metadata for shared-root test drive %s; re-run scripts/bootstrap-test-credentials.sh",
			driveID,
		)

	default:
		return fmt.Errorf("missing drive metadata for unsupported test drive %s", driveID)
	}
}
