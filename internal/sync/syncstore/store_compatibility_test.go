package syncstore

import (
	"errors"
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
	assert.ErrorContains(t, err, "rebuild sync store from an older version")
	assert.False(t, IsStateStoreIncompatible(err), "a failed delete is not a store a reset would fix")
	assert.FileExists(t, dbPath, "a failed rebuild keeps the older store")

	store, err = OpenEngineSyncStore(t.Context(), dbPath, syncfixture.NewTestLogger(t), nil, Hooks{})
	require.NoError(t, err, "the next start rebuilds the store")
	metadata, err := readStoreCompatibilityMetadata(t.Context(), store.db)
	require.NoError(t, err)
	assert.Equal(t, currentSyncStoreGeneration, metadata.SchemaGeneration)
	require.NoError(t, store.Close(t.Context()))
}
