package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.6.25
//
// A store that has not completed a sync (no delta cursor, empty baseline, as
// right after `drive reset-sync-state`) has zero drift because it knows
// nothing, not because everything is up to date. Status read it as "Up to
// date" on 2026-11-02.
func TestReadDriveStatusSnapshot_AFreshStoreIsNeverSynced(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	snapshot, err := store.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.True(t, snapshot.NeverSynced)
	assert.Zero(t, snapshot.BaselineFileCount)
}
