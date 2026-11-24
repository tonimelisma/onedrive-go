package syncstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// writeStoreAtGeneration creates a canonical store at dbPath whose marker
// names generation, as one another onedrive-go version wrote reads.
func writeStoreAtGeneration(t *testing.T, dbPath string, generation int) {
	t.Helper()

	store, err := NewSyncStore(t.Context(), dbPath, syncfixture.NewTestLogger(t))
	require.NoError(t, err)
	requireStoreTestExec(t, store, `UPDATE store_metadata SET schema_generation = ?`, generation)
	require.NoError(t, store.Close(t.Context()))
}

// A rebuild whose delete fails leaves the older store where it was and says
// why; the next start rebuilds it once the file family can be deleted.
//
// Validates: R-2.5.9
func TestOpenEngineSyncStore_RebuildThatCannotDeleteKeepsTheOlderStore(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	writeStoreAtGeneration(t, dbPath, currentSyncStoreGeneration-1)
	diskErr := errors.New("input/output error")
	failing := Hooks{RemoveStoreFiles: func(string) error { return diskErr }}

	store, err := OpenEngineSyncStore(t.Context(), dbPath, syncfixture.NewTestLogger(t), nil, failing)
	require.Error(t, err)
	require.Nil(t, store)
	require.ErrorIs(t, err, diskErr)
	require.ErrorContains(t, err, "rebuild sync store from an older version")
	assert.False(t, IsStateStoreIncompatible(err), "a failed delete is not a store a reset would fix")
	assert.FileExists(t, dbPath, "a failed rebuild keeps the older store")

	store, err = OpenEngineSyncStore(t.Context(), dbPath, syncfixture.NewTestLogger(t), nil, Hooks{})
	require.NoError(t, err, "the next start rebuilds the store")
	metadata, err := readStoreCompatibilityMetadata(t.Context(), store.db)
	require.NoError(t, err)
	assert.Equal(t, currentSyncStoreGeneration, metadata.SchemaGeneration)
	require.NoError(t, store.Close(t.Context()))
}

// The read-only inspector names a store another version wrote instead of
// failing on a column it does not know, and changes nothing: the engine
// rebuilds an older store, and a newer one is the newer version's to read.
//
// Validates: R-2.5.5, R-2.5.9
func TestReadDriveStatusSnapshot_NamesAStoreAnotherVersionWrote(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		generation int
		want       stateStoreIncompatibleReason
	}{
		{name: "older", generation: currentSyncStoreGeneration - 1, want: StateStoreIncompatibleReasonOlderVersion},
		{name: "newer", generation: currentSyncStoreGeneration + 1, want: StateStoreIncompatibleReasonNewerVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dbPath := filepath.Join(t.TempDir(), "state.db")
			writeStoreAtGeneration(t, dbPath, tc.generation)
			before, err := os.ReadFile(dbPath) //nolint:gosec // test-owned temp path
			require.NoError(t, err)

			_, err = ReadDriveStatusSnapshot(t.Context(), dbPath, syncfixture.NewTestLogger(t))
			var refusal *StateStoreIncompatibleError
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, tc.want, refusal.Reason)

			after, err := os.ReadFile(dbPath) //nolint:gosec // test-owned temp path
			require.NoError(t, err)
			assert.Equal(t, before, after, "an inspector never writes")
		})
	}
}

// The current store reads as before.
//
// Validates: R-2.5.5
func TestReadDriveStatusSnapshot_CurrentGenerationReads(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	writeStoreAtGeneration(t, dbPath, currentSyncStoreGeneration)

	_, err := ReadDriveStatusSnapshot(t.Context(), dbPath, syncfixture.NewTestLogger(t))
	require.NoError(t, err)
}
