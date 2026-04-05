package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

func writeManagedFixture(t *testing.T, path string, data []byte) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

type emailReconcileFixture struct {
	configPath    string
	oldBusiness   driveid.CanonicalID
	newBusiness   driveid.CanonicalID
	oldSharePoint driveid.CanonicalID
	newSharePoint driveid.CanonicalID
	oldShared     driveid.CanonicalID
	newShared     driveid.CanonicalID
	personal      driveid.CanonicalID
}

func seedEmailReconcileFixture(t *testing.T) emailReconcileFixture {
	t.Helper()

	fixture := emailReconcileFixture{
		configPath:    filepath.Join(t.TempDir(), "config.toml"),
		oldBusiness:   driveid.MustCanonicalID("business:user@example.com"),
		newBusiness:   driveid.MustCanonicalID("business:renamed@example.com"),
		oldSharePoint: driveid.MustCanonicalID("sharepoint:user@example.com:team:Docs"),
		newSharePoint: driveid.MustCanonicalID("sharepoint:renamed@example.com:team:Docs"),
		oldShared:     driveid.MustCanonicalID("shared:user@example.com:drv123:item456"),
		newShared:     driveid.MustCanonicalID("shared:renamed@example.com:drv123:item456"),
		personal:      driveid.MustCanonicalID("personal:user@example.com"),
	}

	require.NoError(t, SaveAccountProfile(fixture.oldBusiness, &AccountProfile{
		UserID:         "user-123",
		DisplayName:    "Business User",
		PrimaryDriveID: "drive-business",
	}))
	require.NoError(t, SaveAccountProfile(fixture.personal, &AccountProfile{
		UserID:         "other-user",
		DisplayName:    "Personal User",
		PrimaryDriveID: "drive-personal",
	}))

	writeManagedFixture(t, DriveTokenPath(fixture.oldBusiness), []byte(`{"token":"old-business"}`))
	writeManagedFixture(t, DriveTokenPath(fixture.personal), []byte(`{"token":"personal"}`))
	writeManagedFixture(t, DriveStatePath(fixture.oldBusiness), []byte("business-state"))
	writeManagedFixture(t, DriveStatePath(fixture.oldSharePoint), []byte("sharepoint-state"))
	writeManagedFixture(t, DriveStatePath(fixture.oldShared), []byte("shared-state"))
	writeManagedFixture(t, DriveStatePath(fixture.personal), []byte("personal-state"))

	require.NoError(t, SaveDriveMetadata(fixture.oldBusiness, &DriveMetadata{DriveID: "drive-business"}))
	require.NoError(t, SaveDriveMetadata(fixture.oldSharePoint, &DriveMetadata{
		DriveID: "drive-sharepoint",
		SiteID:  "site-123",
	}))
	require.NoError(t, SaveDriveMetadata(fixture.oldShared, &DriveMetadata{
		AccountCanonicalID: fixture.oldBusiness.String(),
		OwnerName:          "Alice",
		OwnerEmail:         "alice@example.com",
	}))

	writeConfigFixture(t, fixture.configPath, []byte(`# config
["business:user@example.com"]
# keep this comment
sync_dir = "~/Business"
display_name = "Work"

["sharepoint:user@example.com:team:Docs"]
sync_dir = "~/Team"

["shared:user@example.com:drv123:item456"]
sync_dir = "~/Shared"
owner = "Alice's Shared"

["personal:user@example.com"]
sync_dir = "~/Personal"
`))

	return fixture
}

func assertEmailReconcileFixtureRenamed(
	t *testing.T,
	logger *slog.Logger,
	fixture *emailReconcileFixture,
	result *EmailReconcileResult,
) {
	t.Helper()

	require.True(t, result.Changed())
	assert.Contains(t, result.AccountRenames, CanonicalIDRename{From: fixture.oldBusiness, To: fixture.newBusiness})
	assert.Contains(t, result.DriveRenames, CanonicalIDRename{From: fixture.oldBusiness, To: fixture.newBusiness})
	assert.Contains(t, result.DriveRenames, CanonicalIDRename{From: fixture.oldSharePoint, To: fixture.newSharePoint})
	assert.Contains(t, result.DriveRenames, CanonicalIDRename{From: fixture.oldShared, To: fixture.newShared})

	_, oldTokenErr := os.Stat(DriveTokenPath(fixture.oldBusiness))
	require.ErrorIs(t, oldTokenErr, os.ErrNotExist)
	_, oldAccountErr := os.Stat(AccountFilePath(fixture.oldBusiness))
	require.ErrorIs(t, oldAccountErr, os.ErrNotExist)
	_, oldBusinessStateErr := os.Stat(DriveStatePath(fixture.oldBusiness))
	require.ErrorIs(t, oldBusinessStateErr, os.ErrNotExist)
	_, oldSharePointStateErr := os.Stat(DriveStatePath(fixture.oldSharePoint))
	require.ErrorIs(t, oldSharePointStateErr, os.ErrNotExist)
	_, oldSharedStateErr := os.Stat(DriveStatePath(fixture.oldShared))
	require.ErrorIs(t, oldSharedStateErr, os.ErrNotExist)

	assert.FileExists(t, DriveTokenPath(fixture.newBusiness))
	assert.FileExists(t, AccountFilePath(fixture.newBusiness))
	assert.FileExists(t, DriveStatePath(fixture.newBusiness))
	assert.FileExists(t, DriveStatePath(fixture.newSharePoint))
	assert.FileExists(t, DriveStatePath(fixture.newShared))
	assert.FileExists(t, DriveStatePath(fixture.personal))
	assert.FileExists(t, DriveTokenPath(fixture.personal))

	sharedMeta, found, err := LookupDriveMetadata(fixture.newShared)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, fixture.newBusiness.String(), sharedMeta.AccountCanonicalID)

	cfg, err := Load(fixture.configPath, logger)
	require.NoError(t, err)
	assert.Contains(t, cfg.Drives, fixture.newBusiness)
	assert.Contains(t, cfg.Drives, fixture.newSharePoint)
	assert.Contains(t, cfg.Drives, fixture.newShared)
	assert.Contains(t, cfg.Drives, fixture.personal)
	assert.NotContains(t, cfg.Drives, fixture.oldBusiness)
	assert.NotContains(t, cfg.Drives, fixture.oldSharePoint)
	assert.NotContains(t, cfg.Drives, fixture.oldShared)

	raw, err := localpath.ReadFile(fixture.configPath)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, `["business:renamed@example.com"]`)
	assert.Contains(t, content, "# keep this comment")
	assert.Contains(t, content, `display_name = "Work"`)
	assert.NotContains(t, content, `["business:user@example.com"]`)
}

// Validates: R-3.7.1, R-3.7.2
func TestReconcileAccountEmail_RenamesOwnedArtifacts(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)

	result, err := ReconcileAccountEmail(
		fixture.configPath,
		fixture.newBusiness,
		"user-123",
		"renamed@example.com",
		logger,
	)
	require.NoError(t, err)

	assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
}

// Validates: R-3.7.1, R-3.7.2
func TestReconcileAccountEmail_NoOpWhenEmailUnchanged(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)

	current := driveid.MustCanonicalID("business:user@example.com")
	require.NoError(t, SaveAccountProfile(current, &AccountProfile{
		UserID:         "user-123",
		DisplayName:    "Business User",
		PrimaryDriveID: "drive-business",
	}))
	writeManagedFixture(t, DriveTokenPath(current), []byte(`{"token":"business"}`))

	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfigFixture(t, configPath, []byte(`["business:user@example.com"]
sync_dir = "~/Business"
`))

	result, err := ReconcileAccountEmail(configPath, current, "user-123", "user@example.com", logger)
	require.NoError(t, err)
	assert.False(t, result.Changed())
	assert.FileExists(t, DriveTokenPath(current))
}

// Validates: R-3.7.1, R-3.7.2
func TestReconcileAccountEmail_IdempotentRerun(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)

	oldBusiness := driveid.MustCanonicalID("business:user@example.com")
	newBusiness := driveid.MustCanonicalID("business:renamed@example.com")
	require.NoError(t, SaveAccountProfile(oldBusiness, &AccountProfile{
		UserID:         "user-123",
		DisplayName:    "Business User",
		PrimaryDriveID: "drive-business",
	}))
	writeManagedFixture(t, DriveTokenPath(oldBusiness), []byte(`{"token":"old-business"}`))

	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfigFixture(t, configPath, []byte(`["business:user@example.com"]
sync_dir = "~/Business"
`))

	first, err := ReconcileAccountEmail(configPath, newBusiness, "user-123", "renamed@example.com", logger)
	require.NoError(t, err)
	require.True(t, first.Changed())

	second, err := ReconcileAccountEmail(configPath, newBusiness, "user-123", "renamed@example.com", logger)
	require.NoError(t, err)
	assert.False(t, second.Changed())
}

// Validates: R-3.7.2
func TestReconcileAccountEmail_CollisionFailsWithoutMutation(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)

	oldBusiness := driveid.MustCanonicalID("business:user@example.com")
	newBusiness := driveid.MustCanonicalID("business:renamed@example.com")
	require.NoError(t, SaveAccountProfile(oldBusiness, &AccountProfile{
		UserID:         "user-123",
		DisplayName:    "Business User",
		PrimaryDriveID: "drive-business",
	}))
	writeManagedFixture(t, DriveTokenPath(oldBusiness), []byte(`{"token":"old"}`))
	writeManagedFixture(t, DriveTokenPath(newBusiness), []byte(`{"token":"new"}`))

	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfigFixture(t, configPath, []byte(`["business:user@example.com"]
sync_dir = "~/Business"
`))

	_, err := ReconcileAccountEmail(configPath, newBusiness, "user-123", "renamed@example.com", logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target already exists")
	assert.FileExists(t, DriveTokenPath(oldBusiness))

	cfg, loadErr := Load(configPath, logger)
	require.NoError(t, loadErr)
	assert.Contains(t, cfg.Drives, oldBusiness)
	assert.NotContains(t, cfg.Drives, newBusiness)
}
