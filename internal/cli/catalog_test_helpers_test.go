package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func seedCatalogAccount(
	t *testing.T,
	cid driveid.CanonicalID,
	mutate func(*config.CatalogAccount),
) {
	t.Helper()

	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		account := config.CatalogAccount{
			CanonicalID: cid.String(),
			Email:       cid.Email(),
			DriveType:   cid.DriveType(),
		}
		if existing, found := catalog.AccountByCanonicalID(cid); found {
			account = existing
		}
		if mutate != nil {
			mutate(&account)
		}
		catalog.UpsertAccount(&account)
		return nil
	}))
}

func seedCatalogDrive(
	t *testing.T,
	cid driveid.CanonicalID,
	mutate func(*config.CatalogDrive),
) {
	t.Helper()

	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		drive := config.CatalogDrive{
			CanonicalID: cid.String(),
			DriveType:   cid.DriveType(),
		}
		if existing, found := catalog.DriveByCanonicalID(cid); found {
			drive = existing
		}
		if mutate != nil {
			mutate(&drive)
		}
		if drive.OwnerAccountCanonical == "" {
			switch {
			case cid.IsPersonal(), cid.IsBusiness():
				drive.OwnerAccountCanonical = cid.String()
			case cid.IsSharePoint():
				if ownerCID, err := driveid.Construct(driveid.DriveTypeBusiness, cid.Email()); err == nil {
					drive.OwnerAccountCanonical = ownerCID.String()
				}
			}
		}
		catalog.UpsertDrive(&drive)
		return nil
	}))
}

func loadCatalogAccount(t *testing.T, cid driveid.CanonicalID) (config.CatalogAccount, bool) {
	t.Helper()

	catalog, err := config.LoadCatalog()
	require.NoError(t, err)

	account, found := catalog.AccountByCanonicalID(cid)
	return account, found
}

func loadCatalogDrive(t *testing.T, cid driveid.CanonicalID) (config.CatalogDrive, bool) {
	t.Helper()

	catalog, err := config.LoadCatalog()
	require.NoError(t, err)

	drive, found := catalog.DriveByCanonicalID(cid)
	return drive, found
}
