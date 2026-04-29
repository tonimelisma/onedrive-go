package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.8
func TestWatchRuntime_LocalObservationBatchUpsertsScopedRows(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []LocalStateRow{{
		Path:     "stale.txt",
		ItemType: ItemTypeFile,
		Hash:     "old",
		Size:     1,
	}}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		rows: []LocalStateRow{
			{
				Path:             "stale.txt",
				ItemType:         ItemTypeFile,
				Hash:             "new",
				Size:             2,
				Mtime:            22,
				LocalDevice:      33,
				LocalInode:       44,
				LocalHasIdentity: true,
			},
			{
				Path:     "fresh.txt",
				ItemType: ItemTypeFile,
				Hash:     "fresh",
				Size:     3,
			},
		},
		dirty: true,
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []LocalStateRow{
		{
			Path:     "fresh.txt",
			ItemType: ItemTypeFile,
			Hash:     "fresh",
			Size:     3,
		},
		{
			Path:             "stale.txt",
			ItemType:         ItemTypeFile,
			Hash:             "new",
			Size:             2,
			Mtime:            22,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
	}, rows)
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.8
func TestWatchRuntime_LocalObservationBatchDeletesExactPath(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []LocalStateRow{
		{Path: "deleted.txt", ItemType: ItemTypeFile},
		{Path: "kept.txt", ItemType: ItemTypeFile},
	}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		deletedPaths: []string{"deleted.txt"},
		dirty:        true,
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []LocalStateRow{{Path: "kept.txt", ItemType: ItemTypeFile}}, rows)
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.8
func TestWatchRuntime_LocalObservationBatchDeletesDirectoryPrefix(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []LocalStateRow{
		{Path: "dir", ItemType: ItemTypeFolder},
		{Path: "dir/file.txt", ItemType: ItemTypeFile},
		{Path: "dir/nested", ItemType: ItemTypeFolder},
		{Path: "dir/nested/deep.txt", ItemType: ItemTypeFile},
		{Path: "dir-sibling.txt", ItemType: ItemTypeFile},
	}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		deletedPrefixes: []string{"dir"},
		dirty:           true,
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []LocalStateRow{{Path: "dir-sibling.txt", ItemType: ItemTypeFile}}, rows)
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.8
func TestWatchRuntime_LocalObservationBatchFullSnapshotReplacesLocalTruth(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.MarkLocalTruthSuspect(t.Context(), LocalTruthRecoveryDroppedEvents))
	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []LocalStateRow{
		{Path: "stale.txt", ItemType: ItemTypeFile},
	}))
	require.NoError(t, eng.baseline.MarkLocalTruthSuspect(t.Context(), LocalTruthRecoveryDroppedEvents))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		fullSnapshot: true,
		rows: []LocalStateRow{
			{Path: "fresh.txt", ItemType: ItemTypeFile, Hash: "fresh"},
		},
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []LocalStateRow{{Path: "fresh.txt", ItemType: ItemTypeFile, Hash: "fresh"}}, rows)

	state := readObservationStateForTest(t, eng.baseline, t.Context())
	assert.True(t, state.LocalTruthComplete)
	assert.Empty(t, state.LocalTruthRecoveryReason)
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.8
func TestWatchRuntime_LocalObservationBatchMarksLocalTruthSuspect(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []LocalStateRow{
		{Path: "known.txt", ItemType: ItemTypeFile},
	}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		markSuspect:    true,
		recoveryReason: LocalTruthRecoveryDroppedEvents,
	})
	require.NoError(t, err)

	state := readObservationStateForTest(t, eng.baseline, t.Context())
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, LocalTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.8
func TestWatchRuntime_MaintenanceMarksLocalTruthSuspectAfterDroppedObservation(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	rt.localObs = NewLocalObserver(bl, eng.logger, eng.checkWorkers)
	rt.localObs.droppedEvents.Add(1)
	rt.localObs.droppedRetries.Add(1)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []LocalStateRow{
		{Path: "known.txt", ItemType: ItemTypeFile},
	}))

	rt.handleMaintenanceTick(t.Context())

	state := readObservationStateForTest(t, eng.baseline, t.Context())
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, LocalTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)
	assert.Zero(t, rt.localObs.DroppedEvents())
	assert.Zero(t, rt.localObs.DroppedRetries())
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}
