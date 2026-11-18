package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
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
				require.NoError(t, config.MarkAccountAuthRequired(
					config.DefaultDataDir(), account, authstate.ReasonSyncAuthRejected))
				require.True(t, hasPersistedAccountAuthRequirement(t, account),
					"fixture must start with a persisted auth requirement")
			}

			var out bytes.Buffer

			require.NoError(t, executePlainLogout(&out, loggedOut, slog.New(slog.DiscardHandler)))

			assert.False(t, hasPersistedAccountAuthRequirement(t, loggedOut), "logout must clear the persisted auth requirement")
			assert.True(t, hasPersistedAccountAuthRequirement(t, other), "of the account it logged out only")
		})
	}
}

// A personal and a work or school account at one address, both signed in and
// both needing a sign-in, with a drive configured for one of them: logout
// --account deletes that account's token and ends that account's requirement.
// The other account keeps its token and its requirement, whichever of the two
// sorts first.
//
// Validates: R-3.1.3, R-2.10.47
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
			require.NoError(t, config.AppendDriveSection(cfgPath, configured, syncDir))
			require.NoError(t, config.SetDriveKey(cfgPath, configured, "sync_dir", syncDir))
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
			cc.Flags.Account = configured.Email()
			require.NoError(t, runLogoutWithContext(t.Context(), cc, false))

			assert.NoFileExists(t, config.DriveTokenPath(configured), "logout deletes the configured account's token")
			assert.FileExists(t, config.DriveTokenPath(other), "the other account keeps its token")
			assert.False(t, hasPersistedAccountAuthRequirement(t, configured),
				"logout ends the requirement of the account whose token it deleted")
			assert.True(t, hasPersistedAccountAuthRequirement(t, other), "and of that account only")
		})
	}
}
