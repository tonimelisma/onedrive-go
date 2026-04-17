package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestSaveAndLoadDriveIdentity_Personal(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	identity := &DriveIdentity{
		DriveID:  "abc123",
		CachedAt: "2026-03-06T10:00:00Z",
	}

	err := SaveDriveIdentity(cid, identity)
	require.NoError(t, err)

	loaded, found, err := LookupDriveIdentity(cid)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, loaded)
	assert.Equal(t, "abc123", loaded.DriveID)
	assert.Equal(t, "2026-03-06T10:00:00Z", loaded.CachedAt)
}

func TestSaveAndLoadDriveIdentity_SharePoint(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:marketing:Docs")

	identity := &DriveIdentity{
		DriveID:  "sp789",
		SiteID:   "site123",
		CachedAt: "2026-03-06T10:00:00Z",
	}

	require.NoError(t, SaveDriveIdentity(cid, identity))

	loaded, found, err := LookupDriveIdentity(cid)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, loaded)
	assert.Equal(t, "sp789", loaded.DriveID)
	assert.Equal(t, "site123", loaded.SiteID)
}

func TestSaveAndLoadDriveIdentity_Shared(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	identity := &DriveIdentity{
		AccountCanonicalID: "personal:alice@outlook.com",
		OwnerName:          "Bob Jones",
		OwnerEmail:         "bob@contoso.com",
		CachedAt:           "2026-03-06T10:00:00Z",
	}

	require.NoError(t, SaveDriveIdentity(cid, identity))

	loaded, found, err := LookupDriveIdentity(cid)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, loaded)
	assert.Equal(t, "personal:alice@outlook.com", loaded.AccountCanonicalID)
	assert.Equal(t, "Bob Jones", loaded.OwnerName)
	assert.Equal(t, "bob@contoso.com", loaded.OwnerEmail)
}

func TestLookupDriveIdentity_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	identity, found, err := LookupDriveIdentity(cid)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, identity)
}

func TestSaveDriveIdentity_Overwrites(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	require.NoError(t, SaveDriveIdentity(cid, &DriveIdentity{DriveID: "old"}))
	require.NoError(t, SaveDriveIdentity(cid, &DriveIdentity{DriveID: "new"}))

	loaded, found, err := LookupDriveIdentity(cid)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new", loaded.DriveID)
}

func TestSaveDriveIdentity_CreatesDirectory(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	require.NoError(t, SaveDriveIdentity(cid, &DriveIdentity{DriveID: "d123"}))

	path := CatalogPath()
	_, statErr := os.Stat(path)
	require.NoError(t, statErr)

	loaded, found, err := LookupDriveIdentity(cid)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "d123", loaded.DriveID)
}
