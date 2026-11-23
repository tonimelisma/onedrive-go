package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	emailReconcileTestUser123       = "user-123"
	emailReconcileTestBusinessUser  = "Business User"
	emailReconcileTestDriveBusiness = "drive-business"
	emailReconcileTestDrivePersonal = "drive-personal"
)

// stateDBMoverForTest stands in for the sync package's mover, which config
// cannot import: it records the change through begin when the move is a new
// one, renames each database file, as that mover does once it holds the
// stores' locks, and records what it was asked to move.
func stateDBMoverForTest(t *testing.T, moved *[]StateDBMove) StateDBMover {
	t.Helper()

	return func(moves []StateDBMove, begin func() error) error {
		if begin != nil {
			if err := begin(); err != nil {
				return err
			}
		}
		for _, move := range moves {
			if err := os.Rename(move.From, move.To); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err //nolint:wrapcheck // test double reports the rename as it is
			}
			*moved = append(*moved, move)
		}
		return nil
	}
}

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

	seedCatalogAccount(t, fixture.oldBusiness, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = emailReconcileTestBusinessUser
		account.PrimaryDriveID = emailReconcileTestDriveBusiness
		account.PrimaryDriveCanonical = fixture.oldBusiness.String()
	})
	seedCatalogAccount(t, fixture.personal, func(account *CatalogAccount) {
		account.UserID = "other-user"
		account.DisplayName = "Personal User"
		account.PrimaryDriveID = emailReconcileTestDrivePersonal
		account.PrimaryDriveCanonical = fixture.personal.String()
	})

	writeManagedFixture(t, DriveTokenPath(fixture.oldBusiness), []byte(`{"token":"old-business"}`))
	writeManagedFixture(t, DriveTokenPath(fixture.personal), []byte(`{"token":"personal"}`))
	writeManagedFixture(t, DriveStatePath(fixture.oldBusiness), []byte("business-state"))
	writeManagedFixture(t, DriveStatePath(fixture.oldSharePoint), []byte("sharepoint-state"))
	writeManagedFixture(t, DriveStatePath(fixture.oldShared), []byte("shared-state"))
	writeManagedFixture(t, DriveStatePath(fixture.personal), []byte("personal-state"))

	seedCatalogDrive(t, fixture.oldBusiness, func(drive *CatalogDrive) {
		drive.RemoteDriveID = emailReconcileTestDriveBusiness
	})
	seedCatalogDrive(t, fixture.oldSharePoint, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "drive-sharepoint"
		drive.SiteID = "site-123"
	})
	seedCatalogDrive(t, fixture.oldShared, func(drive *CatalogDrive) {
		drive.OwnerAccountCanonical = fixture.oldBusiness.String()
		drive.SharedOwnerName = "Alice"
		drive.SharedOwnerEmail = "alice@example.com"
	})

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
	_, oldBusinessStateErr := os.Stat(DriveStatePath(fixture.oldBusiness))
	require.ErrorIs(t, oldBusinessStateErr, os.ErrNotExist)
	_, oldSharePointStateErr := os.Stat(DriveStatePath(fixture.oldSharePoint))
	require.ErrorIs(t, oldSharePointStateErr, os.ErrNotExist)
	_, oldSharedStateErr := os.Stat(DriveStatePath(fixture.oldShared))
	require.ErrorIs(t, oldSharedStateErr, os.ErrNotExist)

	assert.FileExists(t, DriveTokenPath(fixture.newBusiness))
	assert.FileExists(t, DriveStatePath(fixture.newBusiness))
	assert.FileExists(t, DriveStatePath(fixture.newSharePoint))
	assert.FileExists(t, DriveStatePath(fixture.newShared))
	assert.FileExists(t, DriveStatePath(fixture.personal))
	assert.FileExists(t, DriveTokenPath(fixture.personal))

	sharedIdentity, found := loadCatalogDrive(t, fixture.newShared)
	require.True(t, found)
	assert.Equal(t, fixture.newBusiness.String(), sharedIdentity.OwnerAccountCanonical)

	// The reconcile rewrote the config: it must exist, not read as the defaults.
	require.FileExists(t, fixture.configPath)
	cfg, err := LoadOrDefault(fixture.configPath, logger)
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

	var moved []StateDBMove
	result, err := ReconcileAccountEmail(t.Context(),
		fixture.configPath,
		fixture.newBusiness,
		emailReconcileTestUser123,
		"renamed@example.com",
		stateDBMoverForTest(t, &moved),
		logger,
	)
	require.NoError(t, err)

	assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
	assert.ElementsMatch(t, []StateDBMove{
		{From: DriveStatePath(fixture.oldBusiness), To: DriveStatePath(fixture.newBusiness)},
		{From: DriveStatePath(fixture.oldSharePoint), To: DriveStatePath(fixture.newSharePoint)},
		{From: DriveStatePath(fixture.oldShared), To: DriveStatePath(fixture.newShared)},
	}, moved, "every state DB is moved by the mover, which locks it, never renamed here")
}

// The catalog stores a default drive name derived from the drive's ID, which
// holds the account's address. That name follows the address; a name Graph or
// the user supplied stays.
//
// Validates: R-3.7.2
func TestReconcileAccountEmail_CatalogDefaultDriveNameFollowsTheAddress(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)
	seedCatalogDrive(t, fixture.oldBusiness, func(drive *CatalogDrive) {
		drive.DisplayName = DefaultDisplayName(fixture.oldBusiness)
	})
	seedCatalogDrive(t, fixture.oldShared, func(drive *CatalogDrive) {
		drive.DisplayName = DefaultDisplayName(fixture.oldShared)
	})
	seedCatalogDrive(t, fixture.oldSharePoint, func(drive *CatalogDrive) {
		drive.DisplayName = "Team documents"
	})

	var moved []StateDBMove
	_, err := ReconcileAccountEmail(t.Context(),
		fixture.configPath,
		fixture.newBusiness,
		emailReconcileTestUser123,
		"renamed@example.com",
		stateDBMoverForTest(t, &moved),
		logger,
	)
	require.NoError(t, err)

	business, found := loadCatalogDrive(t, fixture.newBusiness)
	require.True(t, found)
	assert.Equal(t, "renamed@example.com", business.DisplayName)

	shared, found := loadCatalogDrive(t, fixture.newShared)
	require.True(t, found)
	assert.Equal(t, "Shared from renamed@example.com", shared.DisplayName)

	sharePoint, found := loadCatalogDrive(t, fixture.newSharePoint)
	require.True(t, found)
	assert.Equal(t, "Team documents", sharePoint.DisplayName)
}

// A state DB an engine still has open refuses the mover, and the email
// change then renames nothing at all: the state DBs move first, before any
// token, config section or catalog record, and the mover records the change
// only once it holds every store, so a refusal leaves the old names
// consistent and no journal for the next try.
//
// Validates: R-3.7.2, R-3.7.3, R-6.3.9
func TestReconcileAccountEmail_AStateDBInUseChangesNothing(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)
	inUse := errors.New("another onedrive-go sync is already using this sync state")

	_, err := ReconcileAccountEmail(t.Context(), fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		"renamed@example.com", func([]StateDBMove, func() error) error { return inUse }, logger)
	require.ErrorIs(t, err, inUse)

	assert.NoFileExists(t, emailChangeJournalPath(DefaultDataDir()), "a change that renamed nothing leaves no journal")
	assert.FileExists(t, DriveTokenPath(fixture.oldBusiness), "the token keeps its name")
	assert.NoFileExists(t, DriveTokenPath(fixture.newBusiness))
	assert.FileExists(t, DriveStatePath(fixture.oldBusiness), "the state DB keeps its name")
	cfg := loadWrittenConfig(t, fixture.configPath)
	assert.Contains(t, cfg.Drives, fixture.oldBusiness, "the config keeps the old section")
	assert.NotContains(t, cfg.Drives, fixture.newBusiness)
	_, found := loadCatalogAccount(t, fixture.oldBusiness)
	assert.True(t, found, "the catalog keeps the old account")
}

// Validates: R-3.7.1, R-3.7.2
func TestReconcileAccountEmail_NoOpWhenEmailUnchanged(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)

	current := driveid.MustCanonicalID("business:user@example.com")
	seedCatalogAccount(t, current, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = emailReconcileTestBusinessUser
		account.PrimaryDriveID = emailReconcileTestDriveBusiness
		account.PrimaryDriveCanonical = current.String()
	})
	writeManagedFixture(t, DriveTokenPath(current), []byte(`{"token":"business"}`))

	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfigFixture(t, configPath, []byte(`["business:user@example.com"]
sync_dir = "~/Business"
`))

	var moved []StateDBMove
	result, err := ReconcileAccountEmail(t.Context(), configPath, current, emailReconcileTestUser123, "user@example.com",
		stateDBMoverForTest(t, &moved), logger)
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
	seedCatalogAccount(t, oldBusiness, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = emailReconcileTestBusinessUser
		account.PrimaryDriveID = emailReconcileTestDriveBusiness
		account.PrimaryDriveCanonical = oldBusiness.String()
	})
	writeManagedFixture(t, DriveTokenPath(oldBusiness), []byte(`{"token":"old-business"}`))

	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfigFixture(t, configPath, []byte(`["business:user@example.com"]
sync_dir = "~/Business"
`))

	var moved []StateDBMove
	mover := stateDBMoverForTest(t, &moved)
	first, err := ReconcileAccountEmail(t.Context(), configPath, newBusiness, emailReconcileTestUser123, "renamed@example.com", mover, logger)
	require.NoError(t, err)
	require.True(t, first.Changed())

	second, err := ReconcileAccountEmail(t.Context(), configPath, newBusiness, emailReconcileTestUser123, "renamed@example.com", mover, logger)
	require.NoError(t, err)
	assert.False(t, second.Changed())
}

// A token at the new name before the change starts was there first: the
// change is refused before it is put on record or renames anything, and the
// error names the file to remove.
//
// Validates: R-3.7.2, R-3.7.3
func TestReconcileAccountEmail_CollisionFailsWithoutMutation(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)

	oldBusiness := driveid.MustCanonicalID("business:user@example.com")
	newBusiness := driveid.MustCanonicalID("business:renamed@example.com")
	seedCatalogAccount(t, oldBusiness, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = emailReconcileTestBusinessUser
		account.PrimaryDriveID = emailReconcileTestDriveBusiness
		account.PrimaryDriveCanonical = oldBusiness.String()
	})
	writeManagedFixture(t, DriveTokenPath(oldBusiness), []byte(`{"token":"old"}`))
	writeManagedFixture(t, DriveTokenPath(newBusiness), []byte(`{"token":"new"}`))

	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfigFixture(t, configPath, []byte(`["business:user@example.com"]
sync_dir = "~/Business"
`))

	var moved []StateDBMove
	_, err := ReconcileAccountEmail(t.Context(), configPath, newBusiness, emailReconcileTestUser123, "renamed@example.com",
		stateDBMoverForTest(t, &moved), logger)
	require.ErrorContains(t, err, "target already exists: "+DriveTokenPath(newBusiness))
	require.ErrorContains(t, err, "remove "+DriveTokenPath(newBusiness))
	assert.NoFileExists(t, emailChangeJournalPath(DefaultDataDir()), "a refused change is never put on record")
	assert.FileExists(t, DriveTokenPath(oldBusiness))

	cfg := loadWrittenConfig(t, configPath)
	assert.Contains(t, cfg.Drives, oldBusiness)
	assert.NotContains(t, cfg.Drives, newBusiness)
}

// reconcileRenamedForTest runs the business account's change of the fixture
// from user@example.com to renamed@example.com.
func reconcileRenamedForTest(t *testing.T, configPath string, newBusiness driveid.CanonicalID) EmailReconcileResult {
	t.Helper()

	var moved []StateDBMove
	result, err := ReconcileAccountEmail(t.Context(), configPath, newBusiness, emailReconcileTestUser123,
		"renamed@example.com", stateDBMoverForTest(t, &moved), testLogger(t))
	require.NoError(t, err)
	require.True(t, result.Changed())

	return result
}

// drive add stores the default name derived from a shared folder's ID when
// it cannot resolve the shared name. That stored default holds the address,
// so it follows the address with the section's header.
//
// Validates: R-3.7.2
func TestReconcileAccountEmail_AConfigSectionsDefaultNameFollowsTheAddress(t *testing.T) {
	setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)
	writeConfigFixture(t, fixture.configPath, []byte(`["shared:user@example.com:drv123:item456"]
sync_dir = "~/Shared"
display_name = "Shared from user@example.com" # set by drive add
`))

	reconcileRenamedForTest(t, fixture.configPath, fixture.newBusiness)

	raw, err := localpath.ReadFile(fixture.configPath)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, `["shared:renamed@example.com:drv123:item456"]`)
	assert.Contains(t, content, `display_name = "Shared from renamed@example.com" # set by drive add`,
		"the stored default follows the address and keeps its comment")
	assert.NotContains(t, content, "user@example.com")
}

// A name the user typed stays, even one that contains the old address: only
// the exact default follows it.
//
// Validates: R-3.7.2
func TestReconcileAccountEmail_AConfigSectionsOwnNameStays(t *testing.T) {
	setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)
	writeConfigFixture(t, fixture.configPath, []byte(`["shared:user@example.com:drv123:item456"]
sync_dir = "~/Shared"
display_name = "Shared from user@example.com, the team copy"

["business:user@example.com"]
sync_dir = "~/Business"
display_name = "Work"
`))

	reconcileRenamedForTest(t, fixture.configPath, fixture.newBusiness)

	raw, err := localpath.ReadFile(fixture.configPath)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, `["shared:renamed@example.com:drv123:item456"]`)
	assert.Contains(t, content, `display_name = "Shared from user@example.com, the team copy"`)
	assert.Contains(t, content, `display_name = "Work"`)
}

// A shared folder's store the catalog has no record of is the account's when
// it was the only account at the old email: its name is the evidence, and it
// moves with the rest.
//
// Validates: R-3.7.2
func TestReconcileAccountEmail_MovesASharedStoreWithNoCatalogRecordWhenOneAccountHadTheEmail(t *testing.T) {
	setTestDataDir(t)

	oldBusiness := driveid.MustCanonicalID("business:user@example.com")
	newBusiness := driveid.MustCanonicalID("business:renamed@example.com")
	oldShared := driveid.MustCanonicalID("shared:user@example.com:drvNoRecord:itemNoRecord")
	newShared := driveid.MustCanonicalID("shared:renamed@example.com:drvNoRecord:itemNoRecord")
	seedCatalogAccount(t, oldBusiness, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.PrimaryDriveCanonical = oldBusiness.String()
	})
	writeManagedFixture(t, DriveTokenPath(oldBusiness), []byte(`{"token":"old-business"}`))
	writeManagedFixture(t, DriveStatePath(oldShared), []byte("shared-state"))

	result := reconcileRenamedForTest(t, filepath.Join(t.TempDir(), "config.toml"), newBusiness)

	assert.Empty(t, result.KeptStateDBs)
	assert.NoFileExists(t, DriveStatePath(oldShared))
	data, err := localpath.ReadFile(DriveStatePath(newShared))
	require.NoError(t, err, "the store moved to the new email")
	assert.Equal(t, "shared-state", string(data), "with its content")
}

// When a personal and a work or school account both had the old email and no
// catalog record says which of them a shared folder's store belongs to, the
// store stays under the old email and the result names it, so the user is
// told; it is never moved to the wrong account.
//
// Validates: R-3.7.2
func TestReconcileAccountEmail_ReportsASharedStoreItCannotAssign(t *testing.T) {
	setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)
	unassigned := driveid.MustCanonicalID("shared:user@example.com:drvNoRecord:itemNoRecord")
	writeManagedFixture(t, DriveStatePath(unassigned), []byte("unassigned-state"))

	result := reconcileRenamedForTest(t, fixture.configPath, fixture.newBusiness)

	assert.Equal(t, []string{DriveStatePath(unassigned)}, result.KeptStateDBs)
	assert.FileExists(t, DriveStatePath(unassigned), "the store stays under the old email")
	assert.FileExists(t, DriveStatePath(fixture.newShared), "a store with a catalog record still moves")
}
