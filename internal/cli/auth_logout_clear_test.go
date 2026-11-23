package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.7.31
//
// Plain logout used to clear the persisted auth requirement through its own
// copy of the clearing logic. It now goes through the single owner, so this
// pins the behavior that copy provided: after logout the account keeps its
// catalog record but carries no auth requirement, which is what lets a later
// re-login start clean instead of inheriting a stale "needs auth" state. The
// account is the one whose token the logout deleted: another account at the
// same address (a personal and a work or school account can share one) keeps
// its own requirement, whichever of the two sorts first.
func TestExecutePlainLogout_ClearsPersistedAuthRequirement(t *testing.T) {
	personal := driveid.MustCanonicalID("personal:logout-user@example.com")
	business := driveid.MustCanonicalID("business:logout-user@example.com")

	for _, pair := range [][2]driveid.CanonicalID{{personal, business}, {business, personal}} {
		loggedOut, other := pair[0], pair[1]
		t.Run(loggedOut.DriveType(), func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			for _, account := range []driveid.CanonicalID{loggedOut, other} {
				seedCatalogAccount(t, account, nil)
				require.NoError(t, config.MarkAccountAuthRequired(t.Context(),
					config.DefaultDataDir(), account, authstate.ReasonSyncAuthRejected))
				require.True(t, hasPersistedAccountAuthRequirement(t, account),
					"fixture must start with a persisted auth requirement")
			}

			var out bytes.Buffer

			require.NoError(t, executePlainLogout(t.Context(), &out, loggedOut, slog.New(slog.DiscardHandler)))

			assert.False(t, hasPersistedAccountAuthRequirement(t, loggedOut), "logout must clear the persisted auth requirement")
			assert.True(t, hasPersistedAccountAuthRequirement(t, other), "of the account it logged out only")
		})
	}
}

// A personal and a work or school account at one address, both signed in and
// both needing a sign-in, with a drive configured for one of them: logout
// --account with that account's canonical ID deletes that account's token and
// ends that account's requirement. The other account keeps its token and its
// requirement, whichever of the two sorts first.
//
// Validates: R-3.1.3, R-2.10.47, R-3.5.2
func TestLogout_EndsTheSignInRequirementOfTheAccountWhoseTokenItDeleted(t *testing.T) {
	personal := driveid.MustCanonicalID("personal:shared-address@example.com")
	business := driveid.MustCanonicalID("business:shared-address@example.com")

	for _, pair := range [][2]driveid.CanonicalID{{personal, business}, {business, personal}} {
		configured, other := pair[0], pair[1]
		t.Run(configured.DriveType(), func(t *testing.T) {
			setTestDriveHome(t)
			cfgPath := filepath.Join(t.TempDir(), "config.toml")
			syncDir := filepath.Join(t.TempDir(), "sync")
			require.NoError(t, os.MkdirAll(syncDir, 0o700))
			require.NoError(t, config.AppendDriveSection(t.Context(), cfgPath, configured, syncDir))
			require.NoError(t, config.SetDriveKey(t.Context(), cfgPath, configured, "sync_dir", syncDir))
			for _, account := range []driveid.CanonicalID{personal, business} {
				seedCatalogAccount(t, account, nil)
				writeTestTokenFile(t, config.DefaultDataDir(),
					"token_"+account.DriveType()+"_"+account.Email()+".json")
				seedAuthScope(t, account)
				require.True(t, hasPersistedAccountAuthRequirement(t, account),
					"fixture must start with a persisted auth requirement")
			}

			var out bytes.Buffer
			cc := newCommandContext(&out, cfgPath)
			cc.Flags.Account = configured.String()
			require.NoError(t, runLogoutWithContext(t.Context(), cc, false))

			assert.NoFileExists(t, config.DriveTokenPath(configured), "logout deletes the configured account's token")
			assert.FileExists(t, config.DriveTokenPath(other), "the other account keeps its token")
			assert.False(t, hasPersistedAccountAuthRequirement(t, configured),
				"logout ends the requirement of the account whose token it deleted")
			assert.True(t, hasPersistedAccountAuthRequirement(t, other), "and of that account only")
		})
	}
}

// sharedAddressLogoutFixture configures the primary drive of a personal and
// of a work or school account at one address, each signed in, and returns the
// config path.
func sharedAddressLogoutFixture(t *testing.T, personal, business driveid.CanonicalID) string {
	t.Helper()

	setTestDriveHome(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	for _, account := range []driveid.CanonicalID{personal, business} {
		syncDir := filepath.Join(t.TempDir(), account.DriveType())
		require.NoError(t, os.MkdirAll(syncDir, 0o700))
		require.NoError(t, config.AppendDriveSection(t.Context(), cfgPath, account, syncDir))
		seedCatalogAccount(t, account, nil)
		seedCatalogDrive(t, account, func(drive *config.CatalogDrive) {
			drive.OwnerAccountCanonical = account.String()
			drive.PrimaryForAccount = true
		})
		writeTestTokenFile(t, config.DefaultDataDir(), "token_"+account.DriveType()+"_"+account.Email()+".json")
	}

	return cfgPath
}

// An email two accounts share does not name an account: logout refuses it,
// names the canonical IDs that do, and deletes nothing.
//
// Validates: R-3.5.2
func TestLogout_RefusesAnEmailTwoAccountsShare(t *testing.T) {
	personal := driveid.MustCanonicalID("personal:two-accounts@example.com")
	business := driveid.MustCanonicalID("business:two-accounts@example.com")
	cfgPath := sharedAddressLogoutFixture(t, personal, business)

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Account = personal.Email()
	err := runLogoutWithContext(t.Context(), cc, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--account "+personal.String())
	assert.Contains(t, err.Error(), "--account "+business.String())
	assert.FileExists(t, config.DriveTokenPath(personal), "nothing is deleted")
	assert.FileExists(t, config.DriveTokenPath(business), "nothing is deleted")
	cfg, loadErr := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, loadErr)
	assert.Contains(t, cfg.Drives, personal)
	assert.Contains(t, cfg.Drives, business)
}

// Logout with a canonical ID removes that account's token and drive sections
// and leaves the other account at the same address as it was, whichever of
// the two is named.
//
// Validates: R-3.5.2, R-3.1.3
func TestLogout_ACanonicalIDSelectorRemovesOnlyThatAccount(t *testing.T) {
	personal := driveid.MustCanonicalID("personal:two-accounts@example.com")
	business := driveid.MustCanonicalID("business:two-accounts@example.com")

	for _, pair := range [][2]driveid.CanonicalID{{personal, business}, {business, personal}} {
		named, other := pair[0], pair[1]
		t.Run(named.DriveType(), func(t *testing.T) {
			cfgPath := sharedAddressLogoutFixture(t, personal, business)

			var out bytes.Buffer
			cc := newCommandContext(&out, cfgPath)
			cc.Flags.Account = named.String()
			require.NoError(t, runLogoutWithContext(t.Context(), cc, false))

			assert.NoFileExists(t, config.DriveTokenPath(named), "the named account's token is deleted")
			assert.FileExists(t, config.DriveTokenPath(other), "the other account keeps its token")
			cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			assert.NotContains(t, cfg.Drives, named, "the named account's drive section is removed")
			assert.Contains(t, cfg.Drives, other, "the other account's drive section stays")
		})
	}
}

// A logout whose catalog cannot be read falls back to its recovery load, and
// the config's warning (an unknown key) is logged once, not once for the
// failed validated load and again for the recovery load of the same file.
//
// Validates: R-3.1.3
func TestLogout_AnUnreadableCatalogWarnsOfTheConfigOnce(t *testing.T) {
	setTestDriveHome(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("made_up_logout_key = true\n"), 0o600))
	catalogPath := config.CatalogPathForDataDir(config.DefaultDataDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(catalogPath), 0o700))
	require.NoError(t, os.WriteFile(catalogPath, []byte("{not a catalog"), 0o600))

	var logs bytes.Buffer
	cc := newCommandContext(&bytes.Buffer{}, cfgPath)
	cc.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	//nolint:errcheck // with no account signed in the logout's outcome is not under test, only its warnings
	runLogoutWithContext(t.Context(), cc, false)

	assert.Contains(t, logs.String(), "logout recovery could not load catalog")
	assert.Equal(t, 1, strings.Count(logs.String(), "made_up_logout_key"), logs.String())
}
