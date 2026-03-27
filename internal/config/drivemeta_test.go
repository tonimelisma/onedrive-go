package config

import (
	"log/slog"
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
	assert.Equal(t, filepath.Join(dataDir, "drive_personal_alice@outlook.com.json"), path)
}

func TestDriveMetadataPath_Business(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:bob@contoso.com")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drive_business_bob@contoso.com.json"), path)
}

func TestDriveMetadataPath_SharePoint(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:marketing:Docs")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drive_sharepoint_bob@contoso.com_marketing_Docs.json"), path)
}

func TestDriveMetadataPath_Shared(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	path := DriveMetadataPath(cid)
	assert.Equal(t, filepath.Join(dataDir, "drive_shared_alice@outlook.com_b!abc123_01DEFGH.json"), path)
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

	loaded, found, err := LookupDriveMetadata(cid)
	require.NoError(t, err)
	require.True(t, found)
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

	loaded, found, err := LookupDriveMetadata(cid)
	require.NoError(t, err)
	require.True(t, found)
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

	loaded, found, err := LookupDriveMetadata(cid)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, loaded)
	assert.Equal(t, "personal:alice@outlook.com", loaded.AccountCanonicalID)
	assert.Equal(t, "Bob Jones", loaded.OwnerName)
	assert.Equal(t, "bob@contoso.com", loaded.OwnerEmail)
}

func TestLoadDriveMetadata_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	meta, found, err := LookupDriveMetadata(cid)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, meta)
}

func TestSaveDriveMetadata_Overwrites(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{DriveID: "old"}))
	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{DriveID: "new"}))

	loaded, found, err := LookupDriveMetadata(cid)
	require.NoError(t, err)
	require.True(t, found)
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

// Validates: R-3.1.4
func TestDiscoverDriveMetadataForEmailIn_MatchesEmail(t *testing.T) {
	dir := t.TempDir()

	// Drive metadata for alice — personal and SharePoint.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "drive_personal_alice@outlook.com.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "drive_sharepoint_alice@outlook.com_marketing_Docs.json"), []byte(`{}`), 0o600))
	// Different account — should NOT match.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "drive_personal_bob@outlook.com.json"), []byte(`{}`), 0o600))
	// Not a drive metadata file — should NOT match.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account_personal_alice@outlook.com.json"), []byte(`{}`), 0o600))

	paths := discoverDriveMetadataForEmailIn(dir, "alice@outlook.com", slog.Default())
	require.Len(t, paths, 2)
	assert.Contains(t, paths, filepath.Join(dir, "drive_personal_alice@outlook.com.json"))
	assert.Contains(t, paths, filepath.Join(dir, "drive_sharepoint_alice@outlook.com_marketing_Docs.json"))
}

func TestDriveMetadataPath_NoSubdirectory(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	path := DriveMetadataPath(cid)
	// File should be directly in dataDir, not in a drives/ subdirectory.
	assert.Equal(t, dataDir, filepath.Dir(path))
	assert.NotContains(t, path, "drives/")
}
