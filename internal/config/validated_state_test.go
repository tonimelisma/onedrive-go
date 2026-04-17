package config

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	validatedStateTestUser      = "User"
	validatedStateTestDriveUser = "drive-user"
)

func setConfigTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
}

func testConfigLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(ioDiscard{}, nil))
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}

// Validates: R-3.1.5
func TestLoadValidatedState_RejectsConfiguredDriveMissingCatalogEntry(t *testing.T) {
	setConfigTestHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = "Test User"
	})

	_, _, err := LoadValidatedState(cfgPath, true, testConfigLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured drive")
	assert.Contains(t, err.Error(), "has no catalog entry")
}

// Validates: R-3.1.5
func TestLoadValidatedState_RejectsDriveOwnerMissingCatalogAccount(t *testing.T) {
	setConfigTestHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("shared:user@example.com:drv123:item456")
	require.NoError(t, AppendDriveSection(cfgPath, cid, "~/Shared"))
	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = "business:owner@example.com"
	})

	_, _, err := LoadValidatedState(cfgPath, true, testConfigLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner")
	assert.Contains(t, err.Error(), "missing from the catalog")
}

// Validates: R-3.1.5
func TestLoadValidatedState_RejectsPrimaryDriveOwnedByDifferentAccount(t *testing.T) {
	setConfigTestHome(t)

	accountCID := driveid.MustCanonicalID("personal:user@example.com")
	otherCID := driveid.MustCanonicalID("personal:other@example.com")
	seedCatalogAccount(t, accountCID, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = validatedStateTestUser
		account.PrimaryDriveID = validatedStateTestDriveUser
	})
	seedCatalogAccount(t, otherCID, func(account *CatalogAccount) {
		account.UserID = "user-other"
		account.DisplayName = "Other"
	})
	require.NoError(t, UpdateCatalog(func(catalog *Catalog) error {
		account, found := catalog.AccountByCanonicalID(accountCID)
		require.True(t, found)
		account.PrimaryDriveCanonical = accountCID.String()
		catalog.UpsertAccount(&account)
		return nil
	}))
	seedCatalogDrive(t, accountCID, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = otherCID.String()
		drive.RemoteDriveID = validatedStateTestDriveUser
	})

	_, _, err := LoadValidatedState(filepath.Join(t.TempDir(), "missing-config.toml"), true, testConfigLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary drive")
	assert.Contains(t, err.Error(), "owned by")
}

// Validates: R-2.10.45, R-2.10.46
func TestMarkAndClearAccountAuthRequirement(t *testing.T) {
	setConfigTestHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = validatedStateTestUser
	})

	require.NoError(t, MarkAccountAuthRequired(DefaultDataDir(), cid.Email(), authstate.ReasonSyncAuthRejected))

	stored, err := LoadCatalog()
	require.NoError(t, err)
	account, found := stored.AccountByCanonicalID(cid)
	require.True(t, found)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, account.AuthRequirementReason)

	require.NoError(t, ClearAccountAuthRequirement(DefaultDataDir(), cid.Email(), AuthClearSourceCLIProof))

	stored, err = LoadCatalog()
	require.NoError(t, err)
	account, found = stored.AccountByCanonicalID(cid)
	require.True(t, found)
	assert.Empty(t, account.AuthRequirementReason)
}
