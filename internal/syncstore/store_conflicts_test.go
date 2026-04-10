package syncstore

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func insertUnresolvedConflict(t *testing.T, store *SyncStore, id string) {
	t.Helper()

	_, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO conflicts
			(id, drive_id, path, conflict_type, detected_at, resolution)
		VALUES
			(?, ?, ?, 'edit_edit', 1, 'unresolved')`,
		id,
		testDriveID,
		id+".txt",
	)
	require.NoError(t, err)
}

// Validates: R-2.3.12
func TestSyncStore_RequestConflictResolutionSameStrategyIsIdempotent(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-idempotent")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-idempotent", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	result, err = store.RequestConflictResolution(t.Context(), "conflict-idempotent", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestAlreadyQueued, result.Status)
}

// Validates: R-2.3.12
func TestSyncStore_RequestConflictResolutionFirstWriterWinsConcurrently(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-concurrent")

	type requestResult struct {
		status ConflictRequestStatus
		err    error
	}
	results := make(chan requestResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, strategy := range []string{synctypes.ResolutionKeepLocal, synctypes.ResolutionKeepRemote} {
		wg.Add(1)
		go func(strategy string) {
			defer wg.Done()
			<-start
			result, err := store.RequestConflictResolution(t.Context(), "conflict-concurrent", strategy)
			results <- requestResult{status: result.Status, err: err}
		}(strategy)
	}
	close(start)
	wg.Wait()
	close(results)

	var statuses []ConflictRequestStatus
	for result := range results {
		require.NoError(t, result.err)
		statuses = append(statuses, result.status)
	}
	assert.ElementsMatch(t, []ConflictRequestStatus{
		ConflictRequestQueued,
		ConflictRequestDifferentStrategy,
	}, statuses)
}

// Validates: R-2.3.12
func TestSyncStore_RequestConflictResolutionRejectsResolvingAndResolved(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-resolving")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-resolving", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	_, ok, err := store.ClaimConflictResolution(t.Context(), "conflict-resolving")
	require.NoError(t, err)
	require.True(t, ok)

	result, err = store.RequestConflictResolution(t.Context(), "conflict-resolving", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestAlreadyResolving, result.Status)

	require.NoError(t, store.ResolveConflict(t.Context(), "conflict-resolving", synctypes.ResolutionKeepLocal))
	result, err = store.RequestConflictResolution(t.Context(), "conflict-resolving", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestAlreadyResolved, result.Status)
}

// Validates: R-2.3.12
func TestSyncStore_ResolveConflictClearsConflictRequestRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-clear-request")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-clear-request", synctypes.ResolutionKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	require.NoError(t, store.ResolveConflict(t.Context(), "conflict-clear-request", synctypes.ResolutionKeepBoth))

	_, err = store.GetConflictRequest(t.Context(), "conflict-clear-request")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
