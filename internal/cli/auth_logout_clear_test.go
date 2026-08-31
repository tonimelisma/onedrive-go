package cli

import (
	"bytes"
	"log/slog"
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
// re-login start clean instead of inheriting a stale "needs auth" state.
func TestExecutePlainLogout_ClearsPersistedAuthRequirement(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	accountCID := driveid.MustCanonicalID("personal:logout-user@example.com")
	email := accountCID.Email()

	require.NoError(t, config.UpdateCatalogForDataDir(config.DefaultDataDir(),
		func(catalog *config.Catalog) error {
			account := config.CatalogAccount{CanonicalID: accountCID.String(), Email: email, DriveType: driveid.DriveTypePersonal}
			catalog.UpsertAccount(&account)

			return nil
		}))
	require.NoError(t, config.MarkAccountAuthRequired(
		config.DefaultDataDir(), email, authstate.ReasonSyncAuthRejected))

	required, err := config.LoadHasPersistedAccountAuthRequirement(config.DefaultDataDir(), email)
	require.NoError(t, err)
	require.True(t, required, "fixture must start with a persisted auth requirement")

	var out bytes.Buffer

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, executePlainLogout(cfgPath, &out, email, nil, slog.New(slog.DiscardHandler)))

	required, err = config.LoadHasPersistedAccountAuthRequirement(config.DefaultDataDir(), email)
	require.NoError(t, err)
	assert.False(t, required, "logout must clear the persisted auth requirement")
}
