package config

import (
	"cmp"
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

// setConfigTestHome points HOME at a temp dir and clears the two XDG
// overrides the client reads, so every default path derives from that home
// whatever the shell running the tests exports (scripts/dev-env.sh sets both).
// HOME alone leaves a test reading and writing the directories those
// variables name.
func setConfigTestHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	return home
}

func testConfigLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
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

	_, _, err := LoadValidatedState(cfgPath, testConfigLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is in config.toml")
	assert.Contains(t, err.Error(), "not in onedrive-go's drive inventory")
	assert.Contains(t, err.Error(), "run 'onedrive-go drive add personal:user@example.com'",
		"drive add completes a section the inventory lacks, for every kind of drive")
}

// Validates: R-3.1.5
//
// Of several configured drives the inventory lacks, the refusal names the
// first in canonical ID order, the same one on every run.
func TestLoadValidatedState_NamesTheFirstConfiguredDriveMissingFromTheCatalog(t *testing.T) {
	setConfigTestHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	for _, name := range []string{"e", "c", "a", "d", "b"} {
		require.NoError(t, AppendDriveSection(cfgPath, driveid.MustCanonicalID("personal:"+name+"@example.com"), "~/"+name))
	}

	_, _, err := LoadValidatedState(cfgPath, testConfigLogger())
	require.ErrorContains(t, err, "drive personal:a@example.com is in config.toml")
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

	_, _, err := LoadValidatedState(cfgPath, testConfigLogger())
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

	_, _, err := LoadValidatedState(filepath.Join(t.TempDir(), "missing-config.toml"), testConfigLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary drive")
	assert.Contains(t, err.Error(), "owned by")
}

// Validates: R-4.8.3, R-4.8.4
//
// The lenient load behind status, drive list and shared warns once about a
// drive whose sync folder holds the data folder, whether the folder is
// configured or the drive's default, and the warning about a default is the
// refusal sync gives that drive. A work or school drive's default is named
// after the organization the catalog records for its account, and a default
// another drive's folder already holds steps on to the account's display
// name. A default beside the client's own folders warns nothing. Without a home folder no
// default can be computed: even where the data folder is the same relative
// path below "~/OneDrive", it warns nothing and fails nothing (sync refuses
// such a drive for a sync_dir that is not absolute).
func TestLoadValidatedState_WarnsOnceAboutASyncDirHoldingTheDataFolder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		drive       string // the canonical ID; managedFolderTestDrive when empty
		orgName     string // the catalog's organization for the drive's account
		displayName string // the catalog's display name for the drive's account
		section     string // the drive's keys in config.toml
		// otherHoldsOneDrive adds a second drive whose section sets
		// sync_dir to "~/OneDrive".
		otherHoldsOneDrive bool
		// syncFolder is the drive's sync folder below the home folder
		// ("OneDrive" when empty); the data folder lies inside it unless
		// dataBeside or noHome sets it.
		syncFolder string
		dataBeside bool
		noHome     bool
		warns      bool
	}{
		{name: "default sync_dir", warns: true},
		{
			name: "default sync_dir named after the organization", drive: "business:a@example.com",
			orgName: "Contoso", syncFolder: "OneDrive - Contoso", warns: true,
		},
		{
			name: "default sync_dir stepped past the folder another drive holds", displayName: "Alice",
			otherHoldsOneDrive: true, syncFolder: "OneDrive - Alice", warns: true,
		},
		{name: "configured sync_dir", section: "sync_dir = \"~/OneDrive\"\n", warns: true},
		{name: "default sync_dir beside the data folder", dataBeside: true},
		{name: "default sync_dir without a home folder", noHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setConfigTestHome(t)
			drive := cmp.Or(tc.drive, managedFolderTestDrive)
			syncFolder := filepath.Join(home, cmp.Or(tc.syncFolder, "OneDrive"))
			dataHome := filepath.Join(syncFolder, "data")
			switch {
			case tc.dataBeside:
				dataHome = filepath.Join(home, "data")
			case tc.noHome:
				// The data folder as a path relative to the working folder,
				// the way an unexpanded default is.
				t.Chdir(t.TempDir())
				dataHome = filepath.Join("~", "OneDrive", "data")
			}
			t.Setenv("XDG_DATA_HOME", dataHome)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			cid := driveid.MustCanonicalID(drive)
			seedCatalogAccount(t, cid, func(account *CatalogAccount) {
				account.OrgName = tc.orgName
				account.DisplayName = tc.displayName
			})
			seedCatalogDrive(t, cid, nil)
			content := "[\"" + drive + "\"]\n" + tc.section
			if tc.otherHoldsOneDrive {
				other := driveid.MustCanonicalID("personal:b@example.com")
				seedCatalogAccount(t, other, nil)
				seedCatalogDrive(t, other, nil)
				content += "\n[\"" + other.String() + "\"]\nsync_dir = \"~/OneDrive\"\n"
			}
			cfgPath := writeTestConfig(t, content)
			if tc.noHome {
				t.Setenv("HOME", "")
			}

			_, warnings, err := LoadValidatedState(cfgPath, testConfigLogger())
			require.NoError(t, err)
			if !tc.warns {
				assert.Empty(t, warnings)
				return
			}
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0].Message, `drive "`+drive+`" sync_dir `+
				canonicalSyncDirPath(syncFolder)+refusalSyncDirHolds+canonicalSyncDirPath(DefaultDataDir()))
			if tc.section != "" {
				return
			}
			resolved, _, err := ResolveDrive(EnvOverrides{ConfigPath: cfgPath}, CLIOverrides{Drive: drive}, testConfigLogger())
			require.NoError(t, err)
			require.EqualError(t, ValidateResolvedForSync(resolved), warnings[0].Message)
		})
	}
}

// Validates: R-2.10.45, R-2.10.46
func TestMarkAndClearAccountAuthRequirement(t *testing.T) {
	setConfigTestHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	seedCatalogAccount(t, cid, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.DisplayName = validatedStateTestUser
	})

	require.NoError(t, MarkAccountAuthRequired(DefaultDataDir(), cid, authstate.ReasonSyncAuthRejected))

	stored, err := LoadCatalog()
	require.NoError(t, err)
	account, found := stored.AccountByCanonicalID(cid)
	require.True(t, found)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, account.AuthRequirementReason)

	require.NoError(t, ClearAccountAuthRequirement(DefaultDataDir(), cid))

	stored, err = LoadCatalog()
	require.NoError(t, err)
	account, found = stored.AccountByCanonicalID(cid)
	require.True(t, found)
	assert.Empty(t, account.AuthRequirementReason)
}

// A personal and a work or school account can share an address. Each one's
// sign-in requirement is marked, read and cleared on its own record, whichever
// of the two sorts first, and never on the other's.
//
// Validates: R-2.10.45, R-2.10.46, R-2.10.47
func TestAccountAuthRequirement_TwoAccountsAtOneAddressAreMarkedReadAndClearedApart(t *testing.T) {
	business := driveid.MustCanonicalID("business:user@example.com")
	personal := driveid.MustCanonicalID("personal:user@example.com")

	for _, pair := range [][2]driveid.CanonicalID{{business, personal}, {personal, business}} {
		account, other := pair[0], pair[1]
		t.Run(account.DriveType(), func(t *testing.T) {
			setConfigTestHome(t)
			seedCatalogAccount(t, account, nil)
			seedCatalogAccount(t, other, nil)
			required := func(cid driveid.CanonicalID) bool {
				t.Helper()
				has, err := LoadHasPersistedAccountAuthRequirement(DefaultDataDir(), cid)
				require.NoError(t, err)
				return has
			}

			require.NoError(t, MarkAccountAuthRequired(DefaultDataDir(), account, authstate.ReasonSyncAuthRejected))
			assert.True(t, required(account), "the account whose sign-in was refused is marked")
			assert.False(t, required(other), "the other account at the address is not")

			require.NoError(t, MarkAccountAuthRequired(DefaultDataDir(), other, authstate.ReasonSyncAuthRejected))
			require.NoError(t, ClearAccountAuthRequirement(DefaultDataDir(), account))
			assert.False(t, required(account), "the clear ends the account's own requirement")
			assert.True(t, required(other), "and keeps the other account's")
		})
	}
}
