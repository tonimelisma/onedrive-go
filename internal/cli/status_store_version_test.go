package cli

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// stampTestStateDBGeneration writes a canonical state DB at dbPath whose
// generation marker says another onedrive-go version wrote it.
func stampTestStateDBGeneration(t *testing.T, dbPath string, generationOffset int) {
	t.Helper()

	createTestStateDB(t, dbPath)
	db, err := sql.Open(syncstore.SqliteDriverName, syncstore.SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(),
		`UPDATE store_metadata SET schema_generation = schema_generation + ?`, generationOffset)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// A state DB an older version wrote is not unreadable: the next sync rebuilds
// it, and status says so instead of asking the user to check permissions.
// status itself changes nothing.
//
// Validates: R-2.5.9, R-6.6.25
func TestStatusCommand_StoreFromAnOlderVersionSaysTheNextSyncRebuildsIt(t *testing.T) {
	cid, cfgPath := seedDamagedStatusDrive(t)
	statePath := config.DriveStatePath(cid)
	stampTestStateDBGeneration(t, statePath, -1)
	before, err := os.ReadFile(statePath) //nolint:gosec // test-owned temp path
	require.NoError(t, err)

	drive, syncState, logs := runStatusJSONForDrive(t, cfgPath, cid)

	assert.Equal(t, driveStateNotSynced, drive.State, "nothing is known until the rebuilt state syncs")
	assert.Equal(t, syncStateFromOlderVersionReason, drive.StateReason)
	assert.Contains(t, drive.RecoveryAction, "next sync rebuilds")
	require.NotNil(t, syncState)
	assert.Equal(t, storeVersionOlder, syncState.StoreVersion)
	assert.Empty(t, syncState.ReadError, "the store was read far enough to know which version wrote it")
	assert.NotContains(t, logs, "cannot read the sync state", "an expected store is not a Warn")

	after, err := os.ReadFile(statePath) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, before, after, "status never rebuilds a store")
}

// A state DB a newer version wrote is unavailable to this version, and the
// action names the newer version before the reset.
//
// Validates: R-2.5.9, R-6.6.25
func TestStatusCommand_StoreFromANewerVersionNamesTheNewerVersion(t *testing.T) {
	cid, cfgPath := seedDamagedStatusDrive(t)
	stampTestStateDBGeneration(t, config.DriveStatePath(cid), 1)

	drive, syncState, _ := runStatusJSONForDrive(t, cfgPath, cid)

	assert.Equal(t, driveStateUnavailable, drive.State)
	assert.Equal(t, syncStateFromNewerVersionReason, drive.StateReason)
	assert.Contains(t, drive.RecoveryAction, "newer onedrive-go")
	assert.Contains(t, drive.RecoveryAction, syncStateResetCommand(cid))
	require.NotNil(t, syncState)
	assert.Equal(t, storeVersionNewer, syncState.StoreVersion)
}
