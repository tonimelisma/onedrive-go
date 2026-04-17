package e2e

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type driveIdentityFetcher func(context.Context, string) (*config.DriveIdentity, error)

func ensureTestDriveIdentity(
	ctx context.Context,
	driveIDs []string,
	fetch driveIdentityFetcher,
) error {
	for _, driveID := range driveIDs {
		if err := ensureTestDriveIdentityForDrive(ctx, driveID, fetch); err != nil {
			return err
		}
	}

	return nil
}

func ensureTestDriveIdentityForDrive(
	ctx context.Context,
	driveID string,
	fetch driveIdentityFetcher,
) error {
	cid, err := driveid.NewCanonicalID(driveID)
	if err != nil {
		return fmt.Errorf("parse test drive canonical ID %q: %w", driveID, err)
	}

	_, found, lookupErr := config.LookupDriveIdentity(cid)
	if lookupErr != nil {
		return fmt.Errorf("lookup drive identity for %s: %w", driveID, lookupErr)
	}
	if found {
		return nil
	}

	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		if fetch == nil {
			return fmt.Errorf("recover drive identity for %s: nil fetcher", driveID)
		}

		identity, fetchErr := fetch(ctx, driveID)
		if fetchErr != nil {
			return fmt.Errorf("recover drive identity for %s: %w", driveID, fetchErr)
		}
		if identity == nil || identity.DriveID == "" {
			return fmt.Errorf("recover drive identity for %s: empty drive ID", driveID)
		}

		if saveErr := config.SaveDriveIdentity(cid, identity); saveErr != nil {
			return fmt.Errorf("save drive identity for %s: %w", driveID, saveErr)
		}

		return nil

	case cid.IsSharePoint():
		return fmt.Errorf(
			"missing drive identity for SharePoint test drive %s; re-run scripts/bootstrap-test-credentials.sh",
			driveID,
		)

	case cid.IsShared():
		return fmt.Errorf(
			"missing drive identity for shared-root test drive %s; re-run scripts/bootstrap-test-credentials.sh",
			driveID,
		)

	default:
		return fmt.Errorf("missing drive identity for unsupported test drive %s", driveID)
	}
}
