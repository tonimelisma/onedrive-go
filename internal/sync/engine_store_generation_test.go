package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// stampStoreGeneration rewrites the generation marker of the closed store at
// dbPath, the way a store written by another onedrive-go version reads.
func stampStoreGeneration(t *testing.T, dbPath string, generation int) {
	t.Helper()

	db, err := sql.Open("sqlite", syncstore.SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE store_metadata SET schema_generation = ?`, generation)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// A store an older onedrive-go wrote is rebuilt at startup rather than
// refused: state is a cache of local and remote truth. The rebuilt store has
// no baseline, so a file deleted here while the old version's state was in
// place is downloaded again, never deleted on OneDrive.
//
// Validates: R-2.5.9
func TestRunOnce_StoreFromAnOlderGenerationIsRebuiltAndDeletesNothing(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	closeRootSafetyEngine(t, eng)

	deleted := filepath.Join(paths.syncRoot, "Docs", "a.txt")
	require.NoError(t, os.Remove(deleted))
	stampStoreGeneration(t, paths.dbPath, 1)

	eng, err = openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err, "an older store is rebuilt, not refused")
	baseline, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	assert.Zero(t, baseline.Len(), "the rebuilt store starts with no baseline")

	runScenarioPass(t, eng)
	closeRootSafetyEngine(t, eng)

	assert.Empty(t, remote.deletes, "a rebuilt store must not delete anything online")
	assert.FileExists(t, deleted, "without a baseline the remote file is downloaded again")
}

// A store a newer onedrive-go wrote is refused with its own reason and left
// exactly as it is: rebuilding it would throw away the newer version's state
// every time an older binary started.
//
// Validates: R-2.5.5, R-2.5.9
func TestNewEngine_StoreFromANewerGenerationIsRefusedUntouched(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	closeRootSafetyEngine(t, eng)

	const newerGeneration = 1_000_000
	stampStoreGeneration(t, paths.dbPath, newerGeneration)
	before, err := os.ReadFile(paths.dbPath)
	require.NoError(t, err)

	_, err = openRootSafetyEngine(t, paths, remote.client(t))
	var refusal *syncstore.StateStoreIncompatibleError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, syncstore.StateStoreIncompatibleReasonNewerVersion, refusal.Reason)
	assert.Contains(t, refusal.Error(), "newer version of onedrive-go")

	after, err := os.ReadFile(paths.dbPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a newer store is never rewritten")
}
