package cli

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

const (
	loginRollbackOldDriveID     = "old-drive"
	loginRollbackOldDisplayName = "Old User"
)

// Validates: R-3.3.1, R-3.3.5
func TestMaterializeDriveSyncDirRejectsRelativePathWithoutCreatingDirectory(t *testing.T) {
	cwd := t.TempDir()
	previousCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(cwd))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(previousCWD))
	})

	err = materializeDriveSyncDir("relative-sync")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")

	_, statErr := os.Stat(filepath.Join(cwd, "relative-sync"))
	assert.True(t, os.IsNotExist(statErr))
}

// Validates: R-3.3.1
func TestRollbackLoginSideEffectsRemovesNewLoginArtifacts(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:new-login@example.com")
	tokenPath := config.DriveTokenPath(cid)
	snapshot, err := captureLoginRollbackSnapshot(cid, tokenPath, cfgPath)
	require.NoError(t, err)

	writeAccessTokenFile(t, cid, "new-token")
	require.NoError(t, config.RecordLogin(
		config.DefaultDataDir(),
		cid,
		"user-new",
		"New User",
		"",
		driveid.New("new-drive"),
	))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, filepath.Join(t.TempDir(), "sync")))

	require.NoError(t, rollbackLoginSideEffects(cfgPath, cid, &snapshot))

	_, tokenErr := tokenfile.Load(tokenPath)
	require.ErrorIs(t, tokenErr, tokenfile.ErrNotFound)
	_, found := loadCatalogAccount(t, cid)
	assert.False(t, found)
	_, found = loadCatalogDrive(t, cid)
	assert.False(t, found)

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	_, found = cfg.Drives[cid]
	assert.False(t, found)
}

// Validates: R-3.3.1
func TestRollbackLoginSideEffectsRestoresExistingLoginArtifacts(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("business:existing@contoso.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, filepath.Join(t.TempDir(), "sync")))
	writeAccessTokenFile(t, cid, "old-token")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = "old-user"
		account.DisplayName = loginRollbackOldDisplayName
		account.OrgName = "Old Org"
		account.PrimaryDriveID = loginRollbackOldDriveID
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.DisplayName = "Old Drive"
		drive.RemoteDriveID = loginRollbackOldDriveID
		drive.PrimaryForAccount = true
	})

	tokenPath := config.DriveTokenPath(cid)
	snapshot, err := captureLoginRollbackSnapshot(cid, tokenPath, cfgPath)
	require.NoError(t, err)

	writeAccessTokenFile(t, cid, "new-token")
	require.NoError(t, config.RecordLogin(
		config.DefaultDataDir(),
		cid,
		"new-user",
		"New User",
		"New Org",
		driveid.New("new-drive"),
	))

	require.NoError(t, rollbackLoginSideEffects(cfgPath, cid, &snapshot))

	token, err := tokenfile.Load(tokenPath)
	require.NoError(t, err)
	assert.Equal(t, "old-token", token.AccessToken)

	account, found := loadCatalogAccount(t, cid)
	require.True(t, found)
	assert.Equal(t, "old-user", account.UserID)
	assert.Equal(t, loginRollbackOldDisplayName, account.DisplayName)
	assert.Equal(t, "Old Org", account.OrgName)
	assert.Equal(t, loginRollbackOldDriveID, account.PrimaryDriveID)

	drive, found := loadCatalogDrive(t, cid)
	require.True(t, found)
	assert.Equal(t, "Old Drive", drive.DisplayName)
	assert.Equal(t, loginRollbackOldDriveID, drive.RemoteDriveID)
	assert.True(t, drive.PrimaryForAccount)
}

// Validates: R-3.3.1
func TestRollbackLoginSideEffectsRemovesBackfilledSyncDir(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:backfill@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, filepath.Join(t.TempDir(), "initial")))
	require.NoError(t, config.DeleteDriveKey(cfgPath, cid, "sync_dir"))

	tokenPath := config.DriveTokenPath(cid)
	snapshot, err := captureLoginRollbackSnapshot(cid, tokenPath, cfgPath)
	require.NoError(t, err)

	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", filepath.Join(t.TempDir(), "backfilled")))

	require.NoError(t, rollbackLoginSideEffects(cfgPath, cid, &snapshot))

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "sync_dir")
	assert.Contains(t, string(data), cid.String())
}
