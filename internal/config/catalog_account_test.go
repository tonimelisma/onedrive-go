package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	catalogAccountTestUser123     = "u123"
	catalogAccountTestDriveABC123 = "abc123"
	catalogAccountTestBobJones    = "Bob Jones"
	catalogAccountTestContosoLtd  = "Contoso Ltd"
	catalogAccountTestOld         = "old"
	catalogAccountTestNew         = "new"
	catalogAccountTestAlice       = "Alice"
	catalogAccountTestDriveD123   = "d123"
)

func TestCatalogAccountFields_RoundTrip(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = catalogAccountTestUser123
		account.DisplayName = "Alice Smith"
		account.OrgName = ""
		account.PrimaryDriveID = catalogAccountTestDriveABC123
	})

	loaded, found := loadCatalogAccount(t, cid)
	require.True(t, found)
	assert.Equal(t, catalogAccountTestUser123, loaded.UserID)
	assert.Equal(t, "Alice Smith", loaded.DisplayName)
	assert.Empty(t, loaded.OrgName)
	assert.Equal(t, catalogAccountTestDriveABC123, loaded.PrimaryDriveID)
}

func TestCatalogAccount_NotFound(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	account, found := loadCatalogAccount(t, cid)
	assert.False(t, found)
	assert.Empty(t, account)
}

func TestCatalogAccount_BusinessFields(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:bob@contoso.com")

	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = "u456"
		account.DisplayName = catalogAccountTestBobJones
		account.OrgName = catalogAccountTestContosoLtd
		account.PrimaryDriveID = "biz789"
	})

	loaded, found := loadCatalogAccount(t, cid)
	require.True(t, found)
	assert.Equal(t, catalogAccountTestContosoLtd, loaded.OrgName)
}

func TestCatalogAccount_Overwrites(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = catalogAccountTestOld
		account.DisplayName = "Old Name"
		account.PrimaryDriveID = "old-id"
	})
	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = catalogAccountTestNew
		account.DisplayName = "New Name"
		account.PrimaryDriveID = "new-id"
	})

	loaded, found := loadCatalogAccount(t, cid)
	require.True(t, found)
	assert.Equal(t, catalogAccountTestNew, loaded.UserID)
	assert.Equal(t, "New Name", loaded.DisplayName)
}

func TestCatalogAccount_CreatesDirectory(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:alice@outlook.com")

	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = catalogAccountTestUser123
		account.DisplayName = catalogAccountTestAlice
		account.PrimaryDriveID = catalogAccountTestDriveD123
	})

	_, statErr := os.Stat(CatalogPath())
	require.NoError(t, statErr)

	account, found := loadCatalogAccount(t, cid)
	require.True(t, found)
	assert.Equal(t, catalogAccountTestAlice, account.DisplayName)
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
