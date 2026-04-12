package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	result, err := store.RequestConflictResolution(t.Context(), "conflict-idempotent", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	result, err = store.RequestConflictResolution(t.Context(), "conflict-idempotent", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestAlreadyQueued, result.Status)
}

// Validates: R-2.3.12
func TestSyncStore_RequestConflictResolutionQueuedRequestCanBeOverwrittenBeforeApplying(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-overwrite")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-overwrite", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	result, err = store.RequestConflictResolution(t.Context(), "conflict-overwrite", ResolutionKeepRemote)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	request, err := store.GetConflictRequest(t.Context(), "conflict-overwrite")
	require.NoError(t, err)
	assert.Equal(t, ResolutionKeepRemote, request.RequestedResolution)
	assert.Empty(t, request.LastError)
}

// Validates: R-2.3.12
func TestSyncStore_RequestConflictResolutionRejectsApplyingAndResolved(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-applying")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-applying", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	_, ok, err := store.ClaimConflictResolution(t.Context(), "conflict-applying")
	require.NoError(t, err)
	require.True(t, ok)

	result, err = store.RequestConflictResolution(t.Context(), "conflict-applying", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestAlreadyApplying, result.Status)

	require.NoError(t, store.ResolveConflict(t.Context(), "conflict-applying", ResolutionKeepLocal))
	result, err = store.RequestConflictResolution(t.Context(), "conflict-applying", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestAlreadyResolved, result.Status)
}

// Validates: R-2.3.12
func TestSyncStore_RequeueConflictResolutionWithErrorKeepsQueuedRequest(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-error")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-error", ResolutionKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	claimed, ok, err := store.ClaimConflictResolution(t.Context(), "conflict-error")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, claimed)

	require.NoError(t, store.MarkConflictResolutionFailed(t.Context(), "conflict-error", assert.AnError))

	request, err := store.GetConflictRequest(t.Context(), "conflict-error")
	require.NoError(t, err)
	assert.Equal(t, ConflictStateQueued, request.State)
	assert.Equal(t, assert.AnError.Error(), request.LastError)
	assert.Zero(t, request.ApplyingAt)
}

// Validates: R-2.3.12
func TestSyncStore_ResolveConflictClearsConflictRequestRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	insertUnresolvedConflict(t, store, "conflict-clear-request")

	result, err := store.RequestConflictResolution(t.Context(), "conflict-clear-request", ResolutionKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	require.NoError(t, store.ResolveConflict(t.Context(), "conflict-clear-request", ResolutionKeepBoth))

	_, err = store.GetConflictRequest(t.Context(), "conflict-clear-request")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
