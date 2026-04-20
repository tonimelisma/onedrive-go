package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	catalogLifecycleUserID      = "user-123"
	catalogLifecycleDisplayName = "Toni"
	catalogLifecycleOrgName     = "Contoso"
	catalogLifecyclePrimaryID   = "drv-primary"
)

// Validates: R-3.1.5
func TestRecordLogin_UpsertsAccountAndPrimaryDrive(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("business:user@example.com")
	seedCatalogAccount(t, accountCID, func(account *CatalogAccount) {
		account.AuthRequirementReason = authstate.ReasonSyncAuthRejected
	})

	require.NoError(t, RecordLogin(
		DefaultDataDir(),
		accountCID,
		catalogLifecycleUserID,
		catalogLifecycleDisplayName,
		catalogLifecycleOrgName,
		driveid.New(catalogLifecyclePrimaryID),
	))

	account, found := loadCatalogAccount(t, accountCID)
	require.True(t, found)
	assert.Equal(t, catalogLifecycleUserID, account.UserID)
	assert.Equal(t, catalogLifecycleDisplayName, account.DisplayName)
	assert.Equal(t, catalogLifecycleOrgName, account.OrgName)
	assert.Equal(t, driveid.New(catalogLifecyclePrimaryID).String(), account.PrimaryDriveID)
	assert.Equal(t, accountCID.String(), account.PrimaryDriveCanonical)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, account.AuthRequirementReason)

	drive, found := loadCatalogDrive(t, accountCID)
	require.True(t, found)
	assert.Equal(t, accountCID.String(), drive.OwnerAccountCanonical)
	assert.Equal(t, driveid.DriveTypeBusiness, drive.DriveType)
	assert.True(t, drive.PrimaryForAccount)
	assert.Equal(t, driveid.New(catalogLifecyclePrimaryID).String(), drive.RemoteDriveID)
	assert.NotEmpty(t, drive.DisplayName)
	assert.NotEmpty(t, drive.CachedAt)
}

// Validates: R-3.1.5
func TestRegisterDrive_UpsertsOwnedDrive(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("business:user@example.com")
	driveCID := driveid.MustCanonicalID("sharepoint:user@example.com:site-123:Docs")
	seedCatalogAccount(t, accountCID, nil)

	require.NoError(t, RegisterDrive(DefaultDataDir(), driveCID, "Project Docs"))

	drive, found := loadCatalogDrive(t, driveCID)
	require.True(t, found)
	assert.Equal(t, accountCID.String(), drive.OwnerAccountCanonical)
	assert.Equal(t, driveid.DriveTypeSharePoint, drive.DriveType)
	assert.Equal(t, "Project Docs", drive.DisplayName)
	assert.False(t, drive.PrimaryForAccount)
}

// Validates: R-3.1.5
func TestRegisterSharedDrive_UpsertsSharedOwnership(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("personal:owner@example.com")
	sharedCID := driveid.MustCanonicalID("shared:owner@example.com:drv123:item456")
	seedCatalogAccount(t, accountCID, nil)

	require.NoError(t, RegisterSharedDrive(DefaultDataDir(), sharedCID, accountCID, "Shared Folder"))

	drive, found := loadCatalogDrive(t, sharedCID)
	require.True(t, found)
	assert.Equal(t, accountCID.String(), drive.OwnerAccountCanonical)
	assert.Equal(t, driveid.DriveTypeShared, drive.DriveType)
	assert.Equal(t, "Shared Folder", drive.DisplayName)
}

// Validates: R-3.1.5, R-2.10.45
func TestApplyPlainLogout_ClearsAuthRequirementAndPreservesInventory(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("personal:user@example.com")
	seedCatalogAccount(t, accountCID, func(account *CatalogAccount) {
		account.UserID = catalogLifecycleUserID
		account.DisplayName = catalogLifecycleDisplayName
		account.AuthRequirementReason = authstate.ReasonSyncAuthRejected
	})
	seedCatalogDrive(t, accountCID, func(drive *CatalogDrive) {
		drive.PrimaryForAccount = true
		drive.RetainedStatePresent = true
	})

	require.NoError(t, ApplyPlainLogout(DefaultDataDir(), accountCID.Email()))

	account, found := loadCatalogAccount(t, accountCID)
	require.True(t, found)
	assert.Empty(t, account.AuthRequirementReason)

	drive, found := loadCatalogDrive(t, accountCID)
	require.True(t, found)
	assert.True(t, drive.PrimaryForAccount)
	assert.True(t, drive.RetainedStatePresent)
}

// Validates: R-3.1.5
func TestApplyPurgeLogout_RemovesAccountAndOwnedDrives(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("business:user@example.com")
	otherCID := driveid.MustCanonicalID("business:other@example.com")
	ownedDriveCID := driveid.MustCanonicalID("sharepoint:user@example.com:site-123:Docs")
	otherDriveCID := driveid.MustCanonicalID("sharepoint:other@example.com:site-456:Docs")

	seedCatalogAccount(t, accountCID, nil)
	seedCatalogAccount(t, otherCID, nil)
	seedCatalogDrive(t, accountCID, func(drive *CatalogDrive) {
		drive.PrimaryForAccount = true
	})
	seedCatalogDrive(t, ownedDriveCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = accountCID.String()
	})
	seedCatalogDrive(t, otherDriveCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = otherCID.String()
	})

	require.NoError(t, ApplyPurgeLogout(DefaultDataDir(), accountCID.Email()))

	_, found := loadCatalogAccount(t, accountCID)
	assert.False(t, found)
	_, found = loadCatalogDrive(t, accountCID)
	assert.False(t, found)
	_, found = loadCatalogDrive(t, ownedDriveCID)
	assert.False(t, found)

	otherAccount, found := loadCatalogAccount(t, otherCID)
	require.True(t, found)
	assert.Equal(t, otherCID.Email(), otherAccount.Email)
	otherDrive, found := loadCatalogDrive(t, otherDriveCID)
	require.True(t, found)
	assert.Equal(t, otherCID.String(), otherDrive.OwnerAccountCanonical)
}

// Validates: R-3.1.5
func TestPruneDriveAfterPurge_KeepsPrimaryDriveButDeletesNonPrimaryDrive(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("business:user@example.com")
	primaryCID := driveid.MustCanonicalID("business:user@example.com")
	secondaryCID := driveid.MustCanonicalID("sharepoint:user@example.com:site-123:Docs")

	seedCatalogAccount(t, accountCID, func(account *CatalogAccount) {
		account.PrimaryDriveCanonical = primaryCID.String()
	})
	seedCatalogDrive(t, primaryCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = accountCID.String()
		drive.PrimaryForAccount = true
		drive.RetainedStatePresent = true
	})
	seedCatalogDrive(t, secondaryCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = accountCID.String()
		drive.RetainedStatePresent = true
	})

	require.NoError(t, PruneDriveAfterPurge(DefaultDataDir(), primaryCID))
	require.NoError(t, PruneDriveAfterPurge(DefaultDataDir(), secondaryCID))

	primaryDrive, found := loadCatalogDrive(t, primaryCID)
	require.True(t, found)
	assert.False(t, primaryDrive.RetainedStatePresent)

	_, found = loadCatalogDrive(t, secondaryCID)
	assert.False(t, found)
}

func TestAuthenticatedAccountEmails_UsesEachCatalogAccountCanonicalID(t *testing.T) {
	setConfigTestHome(t)

	firstCID := driveid.MustCanonicalID("business:duplicate@example.com")
	secondCID := driveid.MustCanonicalID("personal:duplicate@example.com")
	seedCatalogAccount(t, firstCID, nil)
	seedCatalogAccount(t, secondCID, nil)

	tokenPath := DriveTokenPath(secondCID)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{}"), 0o600))

	emails, err := AuthenticatedAccountEmails(DefaultDataDir())
	require.NoError(t, err)
	assert.Equal(t, []string{"duplicate@example.com"}, emails)
}
