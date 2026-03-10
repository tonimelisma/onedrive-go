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

func TestAccountFilePath_Personal(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	path := AccountFilePath(cid)
	assert.Equal(t, filepath.Join(dataDir, "account_personal_alice@outlook.com.json"), path)
}

func TestAccountFilePath_Business(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:bob@contoso.com")

	path := AccountFilePath(cid)
	assert.Equal(t, filepath.Join(dataDir, "account_business_bob@contoso.com.json"), path)
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

	// Data dir already exists — SaveAccountProfile should write directly.
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

// Validates: R-3.1.5
func TestDiscoverAccountProfilesIn_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ids := discoverAccountProfilesIn(dir, slog.Default())
	assert.Nil(t, ids)
}

// Validates: R-3.1.5
func TestDiscoverAccountProfilesIn_OneProfile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "account_personal_alice@outlook.com.json"),
		[]byte(`{"profile":{"user_id":"u1"}}`), 0o600,
	))

	ids := discoverAccountProfilesIn(dir, slog.Default())
	require.Len(t, ids, 1)
	assert.Equal(t, "personal:alice@outlook.com", ids[0].String())
}

// Validates: R-3.1.5
func TestDiscoverAccountProfilesIn_MultipleProfiles(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"account_personal_charlie@outlook.com.json",
		"account_business_alice@contoso.com.json",
		"account_personal_bob@outlook.com.json",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600))
	}

	ids := discoverAccountProfilesIn(dir, slog.Default())
	require.Len(t, ids, 3)
	// Should be sorted alphabetically by canonical ID string.
	assert.Equal(t, "business:alice@contoso.com", ids[0].String())
	assert.Equal(t, "personal:bob@outlook.com", ids[1].String())
	assert.Equal(t, "personal:charlie@outlook.com", ids[2].String())
}

// Validates: R-3.1.5
func TestDiscoverAccountProfilesIn_IgnoresNonProfileFiles(t *testing.T) {
	dir := t.TempDir()

	// Create account profile, token file, state DB, drive metadata — only profile should match.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account_personal_alice@outlook.com.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token_personal_alice@outlook.com.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state_personal_alice@outlook.com.db"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "drive_personal_alice@outlook.com.json"), []byte(`{}`), 0o600))

	ids := discoverAccountProfilesIn(dir, slog.Default())
	require.Len(t, ids, 1)
	assert.Equal(t, "personal:alice@outlook.com", ids[0].String())
}

// Validates: R-3.1.5
func TestDiscoverAccountProfilesIn_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()

	// Malformed: no underscore between type and email, or missing email.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account_.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account_personal_.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account_unknowntype_alice@outlook.com.json"), []byte(`{}`), 0o600))
	// Valid one for contrast:
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account_personal_alice@outlook.com.json"), []byte(`{}`), 0o600))

	ids := discoverAccountProfilesIn(dir, slog.Default())
	require.Len(t, ids, 1)
	assert.Equal(t, "personal:alice@outlook.com", ids[0].String())
}

func TestAccountFilePath_NoSubdirectory(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	path := AccountFilePath(cid)
	// File should be directly in dataDir, not in an accounts/ subdirectory.
	assert.Equal(t, dataDir, filepath.Dir(path))
	assert.NotContains(t, path, "accounts/")
}
