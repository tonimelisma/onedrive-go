package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDriveTokenPath_Personal(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("personal:toni@outlook.com"))
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_personal_toni@outlook.com.json")
}

func TestDriveTokenPath_Business(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("business:alice@contoso.com"))
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_business_alice@contoso.com.json")
}

// Validates: R-3.4.3
func TestDriveTokenPath_SharePoint_SharesBusinessToken(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"))
	assert.NotEmpty(t, path)
	// SharePoint drives share the business token.
	assert.Contains(t, path, "token_business_alice@contoso.com.json")
	assert.NotContains(t, path, "sharepoint")
}

func TestDriveTokenPath_Shared_WithDriveMetadata(t *testing.T) {
	setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	// Register drive metadata with parent account.
	require.NoError(t, SaveDriveMetadata(sharedCID, &DriveMetadata{
		AccountCanonicalID: "personal:alice@outlook.com",
	}))

	path := DriveTokenPath(sharedCID)
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_personal_alice@outlook.com.json")
	assert.NotContains(t, path, "shared")
}

func TestDriveTokenPath_Shared_WithBusinessAccount(t *testing.T) {
	setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@contoso.com:b!TG9yZW0:01ABCDEF")

	require.NoError(t, SaveDriveMetadata(sharedCID, &DriveMetadata{
		AccountCanonicalID: "business:alice@contoso.com",
	}))

	path := DriveTokenPath(sharedCID)
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_business_alice@contoso.com.json")
}

func TestDriveTokenPath_Shared_NoDriveMetadata(t *testing.T) {
	setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@outlook.com:drv123:item456")

	path := DriveTokenPath(sharedCID)
	assert.Empty(t, path, "shared drive without metadata can't resolve token")
}

func TestDriveTokenPath_ZeroID(t *testing.T) {
	path := DriveTokenPath(driveid.CanonicalID{})
	assert.Empty(t, path)
}

func TestDriveTokenPath_PlatformSpecific(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("personal:toni@outlook.com"))

	switch runtime.GOOS {
	case platformDarwin:
		assert.Contains(t, path, "Library/Application Support")
	case platformLinux:
		assert.Contains(t, path, ".local/share")
	}
}

func TestTokenAccountCID_Personal(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:user@example.com")
	got := tokenAccountCID(cid)
	assert.Equal(t, "personal:user@example.com", got.String())
}

func TestTokenAccountCID_Business(t *testing.T) {
	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	got := tokenAccountCID(cid)
	assert.Equal(t, "business:alice@contoso.com", got.String())
}

// Validates: R-3.4.3
func TestTokenAccountCID_SharePoint(t *testing.T) {
	cid := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")
	got := tokenAccountCID(cid)
	assert.Equal(t, "business:alice@contoso.com", got.String())
}

func TestTokenAccountCID_Shared(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")

	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{
		AccountCanonicalID: "personal:me@outlook.com",
	}))

	got := tokenAccountCID(cid)
	assert.Equal(t, "personal:me@outlook.com", got.String())
}

func TestTokenAccountCID_Shared_NoMetadata(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:nobody@example.com:b!TG9yZW0:01ABCDEF")

	got := tokenAccountCID(cid)
	assert.True(t, got.IsZero(), "should return zero CID when metadata is missing")
}

func TestResolveSharedTokenCID_ValidMetadata(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{
		AccountCanonicalID: "personal:alice@outlook.com",
		OwnerName:          "Bob",
		OwnerEmail:         "bob@contoso.com",
	}))

	got := resolveSharedTokenCID(cid)
	assert.Equal(t, "personal:alice@outlook.com", got.String())
}

func TestResolveSharedTokenCID_MissingMetadata(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	got := resolveSharedTokenCID(cid)
	assert.True(t, got.IsZero())
}

func TestResolveSharedTokenCID_EmptyAccountCID(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	// Save metadata without AccountCanonicalID.
	require.NoError(t, SaveDriveMetadata(cid, &DriveMetadata{
		OwnerName:  "Bob",
		OwnerEmail: "bob@contoso.com",
	}))

	got := resolveSharedTokenCID(cid)
	assert.True(t, got.IsZero())
}

func TestDriveTokenPath_Shared_EndToEnd(t *testing.T) {
	dataDir := setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	// Before metadata: returns empty.
	assert.Empty(t, DriveTokenPath(sharedCID))

	// Register drive metadata.
	require.NoError(t, SaveDriveMetadata(sharedCID, &DriveMetadata{
		AccountCanonicalID: "personal:alice@outlook.com",
	}))

	// After metadata: returns token path for the personal account.
	path := DriveTokenPath(sharedCID)
	assert.Equal(t, filepath.Join(dataDir, "token_personal_alice@outlook.com.json"), path)
}
