package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	tokenResolutionTestOwnerPersonal = "personal:me@outlook.com"
	tokenResolutionTestOwnerNameBob  = "Bob"
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

func TestDriveTokenPath_Shared_WithCatalogDrive(t *testing.T) {
	setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	seedCatalogDrive(t, sharedCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = catalogDriveTestOwnerPersonal
	})

	path := DriveTokenPath(sharedCID)
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_personal_alice@outlook.com.json")
	assert.NotContains(t, path, "shared")
}

func TestDriveTokenPath_Shared_WithBusinessAccount(t *testing.T) {
	setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@contoso.com:b!TG9yZW0:01ABCDEF")

	seedCatalogDrive(t, sharedCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = "business:alice@contoso.com"
	})

	path := DriveTokenPath(sharedCID)
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_business_alice@contoso.com.json")
}

func TestDriveTokenPath_Shared_NoCatalogDrive(t *testing.T) {
	setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@outlook.com:drv123:item456")

	path := DriveTokenPath(sharedCID)
	assert.Empty(t, path, "shared drive without a catalog drive record can't resolve token")
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

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = tokenResolutionTestOwnerPersonal
	})

	got := tokenAccountCID(cid)
	assert.Equal(t, tokenResolutionTestOwnerPersonal, got.String())
}

func TestTokenAccountCanonicalID_Shared(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = tokenResolutionTestOwnerPersonal
	})

	got, err := TokenAccountCanonicalID(cid)
	require.NoError(t, err)
	assert.Equal(t, tokenResolutionTestOwnerPersonal, got.String())
}

func TestTokenAccountCID_Shared_NoCatalogDrive(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:nobody@example.com:b!TG9yZW0:01ABCDEF")

	got := tokenAccountCID(cid)
	assert.True(t, got.IsZero(), "should return zero CID when the catalog drive record is missing")
}

func TestResolveSharedTokenCID_ValidCatalogDrive(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = catalogDriveTestOwnerPersonal
		drive.SharedOwnerName = tokenResolutionTestOwnerNameBob
		drive.SharedOwnerEmail = "bob@contoso.com"
	})

	got, err := resolveSharedTokenCID(cid)
	require.NoError(t, err)
	assert.Equal(t, catalogDriveTestOwnerPersonal, got.String())
}

func TestResolveSharedTokenCID_MissingCatalogDrive(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	got, err := resolveSharedTokenCID(cid)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
}

func TestResolveSharedTokenCID_EmptyAccountCID(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.SharedOwnerName = tokenResolutionTestOwnerNameBob
		drive.SharedOwnerEmail = catalogDriveTestOwnerEmail
	})

	got, err := resolveSharedTokenCID(cid)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
}

func TestDriveTokenPath_Shared_EndToEnd(t *testing.T) {
	dataDir := setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:alice@outlook.com:b!abc123:01DEFGH")

	// Before catalog drive record: returns empty.
	assert.Empty(t, DriveTokenPath(sharedCID))

	seedCatalogDrive(t, sharedCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = "personal:alice@outlook.com"
	})

	// After catalog drive record: returns token path for the personal account.
	path := DriveTokenPath(sharedCID)
	assert.Equal(t, filepath.Join(dataDir, "token_personal_alice@outlook.com.json"), path)
}
