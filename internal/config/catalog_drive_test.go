package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	catalogDriveTestCachedAt      = "2026-03-06T10:00:00Z"
	catalogDriveTestOwnerPersonal = "personal:alice@outlook.com"
	catalogDriveTestOwnerEmail    = "bob@contoso.com"
)

func TestCatalogDriveFields_RoundTripPersonal(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "abc123"
		drive.CachedAt = catalogDriveTestCachedAt
	})

	loaded, found := loadCatalogDrive(t, cid)
	require.True(t, found)
	assert.Equal(t, "abc123", loaded.RemoteDriveID)
	assert.Equal(t, catalogDriveTestCachedAt, loaded.CachedAt)
}

func TestCatalogDriveFields_RoundTripSharePoint(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:marketing:Docs")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "sp789"
		drive.SiteID = "site123"
	})

	loaded, found := loadCatalogDrive(t, cid)
	require.True(t, found)
	assert.Equal(t, "sp789", loaded.RemoteDriveID)
	assert.Equal(t, "site123", loaded.SiteID)
	assert.Equal(t, "business:bob@contoso.com", loaded.OwnerAccountCanonical)
}

func TestCatalogDriveFields_RoundTripShared(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = catalogDriveTestOwnerPersonal
		drive.SharedOwnerName = "Bob Jones"
		drive.SharedOwnerEmail = catalogDriveTestOwnerEmail
		drive.CachedAt = catalogDriveTestCachedAt
	})

	loaded, found := loadCatalogDrive(t, cid)
	require.True(t, found)
	assert.Equal(t, catalogDriveTestOwnerPersonal, loaded.OwnerAccountCanonical)
	assert.Equal(t, "Bob Jones", loaded.SharedOwnerName)
	assert.Equal(t, catalogDriveTestOwnerEmail, loaded.SharedOwnerEmail)
}

func TestCatalogDrive_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	drive, found := loadCatalogDrive(t, cid)
	assert.False(t, found)
	assert.Empty(t, drive)
}

func TestCatalogDrive_Overwrites(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "old"
	})
	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "new"
	})

	loaded, found := loadCatalogDrive(t, cid)
	require.True(t, found)
	assert.Equal(t, "new", loaded.RemoteDriveID)
}

func TestCatalogDrive_CreatesDirectory(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "d123"
	})

	_, statErr := os.Stat(CatalogPath())
	require.NoError(t, statErr)

	loaded, found := loadCatalogDrive(t, cid)
	require.True(t, found)
	assert.Equal(t, "d123", loaded.RemoteDriveID)
}
