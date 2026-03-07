package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestAccountFilePath_Personal(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	path := AccountFilePath(cid)
	assert.Equal(t, filepath.Join(dataDir, "accounts", "personal_alice@outlook.com.json"), path)
}

func TestAccountFilePath_Business(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:bob@contoso.com")

	path := AccountFilePath(cid)
	assert.Equal(t, filepath.Join(dataDir, "accounts", "business_bob@contoso.com.json"), path)
}

func TestAccountFilePath_SharePointReturnsEmpty(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:site:lib")

	path := AccountFilePath(cid)
	assert.Empty(t, path, "SharePoint drives don't have their own account files")
}

func TestAccountFilePath_SharedReturnsEmpty(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:drv123:item456")

	path := AccountFilePath(cid)
	assert.Empty(t, path, "shared drives don't have their own account files")
}

func TestAccountFilePath_ZeroID(t *testing.T) {
	path := AccountFilePath(driveid.CanonicalID{})
	assert.Empty(t, path)
}

func TestSaveAndLoadAccountProfile(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	profile := &AccountProfile{
		UserID:         "u123",
		DisplayName:    "Alice Smith",
		OrgName:        "",
		PrimaryDriveID: "abc123",
	}

	err := SaveAccountProfile(cid, profile)
	require.NoError(t, err)

	loaded, err := LoadAccountProfile(cid)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "u123", loaded.UserID)
	assert.Equal(t, "Alice Smith", loaded.DisplayName)
	assert.Equal(t, "", loaded.OrgName)
	assert.Equal(t, "abc123", loaded.PrimaryDriveID)
}

func TestLoadAccountProfile_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	profile, err := LoadAccountProfile(cid)
	assert.NoError(t, err)
	assert.Nil(t, profile)
}

func TestSaveAccountProfile_Business(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:bob@contoso.com")

	profile := &AccountProfile{
		UserID:         "u456",
		DisplayName:    "Bob Jones",
		OrgName:        "Contoso Ltd",
		PrimaryDriveID: "biz789",
	}

	err := SaveAccountProfile(cid, profile)
	require.NoError(t, err)

	loaded, err := LoadAccountProfile(cid)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "Contoso Ltd", loaded.OrgName)
}

func TestSaveAccountProfile_Overwrites(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	require.NoError(t, SaveAccountProfile(cid, &AccountProfile{
		UserID: "old", DisplayName: "Old Name", PrimaryDriveID: "old-id",
	}))
	require.NoError(t, SaveAccountProfile(cid, &AccountProfile{
		UserID: "new", DisplayName: "New Name", PrimaryDriveID: "new-id",
	}))

	loaded, err := LoadAccountProfile(cid)
	require.NoError(t, err)
	assert.Equal(t, "new", loaded.UserID)
	assert.Equal(t, "New Name", loaded.DisplayName)
}

func TestSaveAccountProfile_CreatesDirectory(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	// accounts/ dir doesn't exist yet — SaveAccountProfile should create it.
	err := SaveAccountProfile(cid, &AccountProfile{
		UserID: "u123", DisplayName: "Alice", PrimaryDriveID: "d123",
	})
	require.NoError(t, err)

	path := AccountFilePath(cid)
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr)
}

func TestAccountCIDForDrive_PersonalReturnsSelf(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")
	got := accountCIDForDrive(cid)
	assert.Equal(t, cid, got)
}

func TestAccountCIDForDrive_BusinessReturnsSelf(t *testing.T) {
	cid := driveid.MustCanonicalID("business:bob@contoso.com")
	got := accountCIDForDrive(cid)
	assert.Equal(t, cid, got)
}

func TestAccountCIDForDrive_SharePointReturnsBusiness(t *testing.T) {
	cid := driveid.MustCanonicalID("sharepoint:bob@contoso.com:site:lib")
	got := accountCIDForDrive(cid)
	assert.Equal(t, "business:bob@contoso.com", got.String())
}
