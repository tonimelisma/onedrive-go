package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

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

	loaded, found, err := LookupAccountProfile(cid)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, loaded)
	assert.Equal(t, "u123", loaded.UserID)
	assert.Equal(t, "Alice Smith", loaded.DisplayName)
	assert.Empty(t, loaded.OrgName)
	assert.Equal(t, "abc123", loaded.PrimaryDriveID)
}

func TestLoadAccountProfile_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	profile, found, err := LookupAccountProfile(cid)
	require.NoError(t, err)
	assert.False(t, found)
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

	loaded, found, err := LookupAccountProfile(cid)
	require.NoError(t, err)
	require.True(t, found)
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

	loaded, found, err := LookupAccountProfile(cid)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new", loaded.UserID)
	assert.Equal(t, "New Name", loaded.DisplayName)
}

func TestSaveAccountProfile_CreatesDirectory(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	// Data dir already exists — SaveAccountProfile should write directly.
	err := SaveAccountProfile(cid, &AccountProfile{
		UserID: "u123", DisplayName: "Alice", PrimaryDriveID: "d123",
	})
	require.NoError(t, err)

	path := CatalogPath()
	_, statErr := os.Stat(path)
	require.NoError(t, statErr)

	profile, found, lookupErr := LookupAccountProfile(cid)
	require.NoError(t, lookupErr)
	require.True(t, found)
	assert.Equal(t, "Alice", profile.DisplayName)
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
