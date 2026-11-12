package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// An email change would rename a drive's state DB; while a sync still has it
// open, nothing is renamed and the command says to stop that sync and run
// again. Renaming it under the engine split the store from its log.
//
// Validates: R-3.7.2, R-6.3.9
func TestReconcileGraphUser_RefusesWhileASyncUsesTheState(t *testing.T) {
	setTestDriveHome(t)

	const userID = "user-123"
	oldCID := driveid.MustCanonicalID("business:user@example.com")
	newCID := driveid.MustCanonicalID("business:renamed@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_user@example.com.json")
	seedCatalogAccount(t, oldCID, func(account *config.CatalogAccount) {
		account.UserID = userID
	})
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, oldCID, t.TempDir()))
	statePath := config.DriveStatePath(oldCID)
	require.NoError(t, os.WriteFile(statePath, []byte(purgeTestStateContent), 0o600))
	holdDriveState(t, statePath)

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	_, err := cc.reconcileGraphUser(t.Context(), newCID, &graph.User{ID: userID, Email: newCID.Email()})

	require.ErrorIs(t, err, syncengine.ErrSyncStoreInUse)
	assert.Contains(t, err.Error(), "stop that sync, then run the command again")
	assert.FileExists(t, statePath, "the state DB keeps its name")
	assert.FileExists(t, config.DriveTokenPath(oldCID), "so does the token")
	assert.NotContains(t, out.String(), "Account email changed")
}
