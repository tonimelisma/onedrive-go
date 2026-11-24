package syncstore

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Forgetting one recorded mount point removes that record alone and keeps
// every other one and the rest of the state; forgetting a path that is not
// recorded changes nothing and says so.
//
// Validates: R-6.2.18
func TestForgetMountBoundary_ForgetsOneRecordAndNothingElse(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	logger := syncfixture.NewTestLogger(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	for _, mount := range []string{"nas", "photos"} {
		_, insertErr := store.db.ExecContext(ctx, `INSERT INTO mount_boundaries (path) VALUES (?)`, mount)
		require.NoError(t, insertErr)
	}
	require.NoError(t, store.Close(ctx))

	recorded, err := ReadMountBoundaries(ctx, dbPath, logger)
	require.NoError(t, err)
	assert.Equal(t, []string{"nas", "photos"}, recorded)

	forgotten, err := ForgetMountBoundary(ctx, dbPath, "nas", logger)
	require.NoError(t, err)
	assert.True(t, forgotten)

	recorded, err = ReadMountBoundaries(ctx, dbPath, logger)
	require.NoError(t, err)
	assert.Equal(t, []string{"photos"}, recorded, "only the named record is forgotten")

	forgotten, err = ForgetMountBoundary(ctx, dbPath, "nas", logger)
	require.NoError(t, err)
	assert.False(t, forgotten, "a path no longer recorded has nothing to forget")
}
