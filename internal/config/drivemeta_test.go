package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDriveMetadataPath_Personal(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drives", "personal_alice@outlook.com.json"), path)
}

func TestDriveMetadataPath_Business(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:bob@contoso.com")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drives", "business_bob@contoso.com.json"), path)
}

func TestDriveMetadataPath_SharePoint(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:marketing:Docs")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drives", "sharepoint_bob@contoso.com_marketing_Docs.json"), path)
}

func TestDriveMetadataPath_Shared(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drives", "shared_alice@outlook.com_b!abc123_01DEFGH.json"), path)
}

func TestDriveMetadataPath_ZeroID(t *testing.T) {
	path := DriveMetadataPath(driveid.CanonicalID{})
	assert.Empty(t, path)
}

func TestSaveAndLoadDriveMetadata_Personal(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	meta := &DriveMetadata{
		DriveID:  "abc123",
		CachedAt: "2026-03-06T10:00:00Z",
	}

	err := SaveDriveMetadata(cid, meta)
	require.NoError(t, err)

	loaded, err := LoadDriveMetadata(cid)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "abc123", loaded.DriveID)
	assert.Equal(t, "2026-03-06T10:00:00Z", loaded.CachedAt)
}

func TestSaveAndLoadDriveMetadata_SharePoint(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:marketing:Docs")

	meta := &DriveMetadata{
		DriveID:  "sp789",
		SiteID:   "site123",
		CachedAt: "2026-03-06T10:00:00Z",
	}

	require.NoError(t, SaveDriveMetadata(cid, meta))

	loaded, err := LoadDriveMetadata(cid)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "sp789", loaded.DriveID)
	assert.Equal(t, "site123", loaded.SiteID)
}

func TestSaveAndLoadDriveMetadata_Shared(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	meta := &DriveMetadata{
		AccountCanonicalID: "personal:alice@outlook.com",
		OwnerName:          "Bob Jones",
		OwnerEmail:         "bob@contoso.com",
		CachedAt:           "2026-03-06T10:00:00Z",
	}

	require.NoError(t, SaveDriveMetadata(cid, meta))

	loaded, err := LoadDriveMetadata(cid)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "personal:alice@outlook.com", loaded.AccountCanonicalID)
	assert.Equal(t, "Bob Jones", loaded.OwnerName)
	assert.Equal(t, "bob@contoso.com", loaded.OwnerEmail)
}

func TestLoadDriveMetadata_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	meta, err := LoadDriveMetadata(cid)
	assert.NoError(t, err)
	assert.Nil(t, meta)
}

func TestSaveDriveMetadata_Overwrites(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{DriveID: "old"}))
	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{DriveID: "new"}))

	loaded, err := LoadDriveMetadata(cid)
	require.NoError(t, err)
	assert.Equal(t, "new", loaded.DriveID)
}

func TestSaveDriveMetadata_CreatesDirectory(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{DriveID: "d123"}))

	path := DriveMetadataPath(cid)
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr)
}
