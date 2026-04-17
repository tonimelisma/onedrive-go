package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

var errUnexpectedFetch = errors.New("unexpected fetch")

// Validates: R-6
func TestEnsureCatalogDriveRecords_WritesMissingPersonalDriveRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	fetchCalls := 0
	err := ensureCatalogDriveRecords(
		t.Context(),
		[]string{"personal:user@example.com"},
		func(context.Context, string) (*config.CatalogDrive, error) {
			fetchCalls++
			return &config.CatalogDrive{RemoteDriveID: "drive-123"}, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, fetchCalls)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	catalog, err := config.LoadCatalog()
	require.NoError(t, err)
	identity, found := catalog.DriveByCanonicalID(cid)
	require.True(t, found)
	assert.Equal(t, "drive-123", identity.RemoteDriveID)
}

// Validates: R-6
func TestEnsureCatalogDriveRecords_SkipsExistingRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("business:user@example.com")
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.UpsertDrive(&config.CatalogDrive{
			CanonicalID:           cid.String(),
			OwnerAccountCanonical: cid.String(),
			DriveType:             cid.DriveType(),
			RemoteDriveID:         "existing-drive",
		})
		return nil
	}))

	err := ensureCatalogDriveRecords(
		t.Context(),
		[]string{cid.String()},
		func(context.Context, string) (*config.CatalogDrive, error) {
			t.Fatal("fetcher should not be called when catalog drive record already exists")
			return nil, errUnexpectedFetch
		},
	)
	require.NoError(t, err)

	catalog, err := config.LoadCatalog()
	require.NoError(t, err)
	identity, found := catalog.DriveByCanonicalID(cid)
	require.True(t, found)
	assert.Equal(t, "existing-drive", identity.RemoteDriveID)
}

// Validates: R-6
func TestEnsureCatalogDriveRecords_RejectsSharePointWithoutRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	err := ensureCatalogDriveRecords(
		t.Context(),
		[]string{"sharepoint:user@example.com:site:library"},
		func(context.Context, string) (*config.CatalogDrive, error) {
			t.Fatal("fetcher should not be called for SharePoint catalog-drive bootstrap")
			return nil, errUnexpectedFetch
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing catalog drive record for SharePoint test drive")
}
