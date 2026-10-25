package sync

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchUpsertsScopedRows(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.perfCollector = perf.NewCollector(nil)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []localStateRow{{
		Path:     "stale.txt",
		ItemType: ItemTypeFile,
		Hash:     "old",
		Size:     1,
	}}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		rows: []localStateRow{
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
	assert.Equal(t, []localStateRow{
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
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	snapshot := eng.collector().Snapshot()
	assert.Equal(t, 1, snapshot.LocalObservationScopedCommitCount)
	assert.Equal(t, 2, snapshot.LocalObservationScopedUpsertCount)
}

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchDeletesExactPath(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.perfCollector = perf.NewCollector(nil)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []localStateRow{
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
	assert.Equal(t, []localStateRow{{Path: "kept.txt", ItemType: ItemTypeFile}}, rows)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	snapshot := eng.collector().Snapshot()
	assert.Equal(t, 1, snapshot.LocalObservationScopedCommitCount)
	assert.Equal(t, 1, snapshot.LocalObservationExactDeleteCount)
}

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchDeletesDirectoryPrefix(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.perfCollector = perf.NewCollector(nil)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []localStateRow{
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
	assert.Equal(t, []localStateRow{{Path: "dir-sibling.txt", ItemType: ItemTypeFile}}, rows)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	snapshot := eng.collector().Snapshot()
	assert.Equal(t, 1, snapshot.LocalObservationScopedCommitCount)
	assert.Equal(t, 1, snapshot.LocalObservationPrefixDeleteCount)
}

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchFullSnapshotReplacesLocalTruth(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.perfCollector = perf.NewCollector(nil)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.MarkLocalTruthSuspect(t.Context(), localTruthRecoveryDroppedEvents))
	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []localStateRow{
		{Path: "stale.txt", ItemType: ItemTypeFile},
	}))
	require.NoError(t, eng.baseline.MarkLocalTruthSuspect(t.Context(), localTruthRecoveryDroppedEvents))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		fullSnapshot: true,
		rows: []localStateRow{
			{Path: "fresh.txt", ItemType: ItemTypeFile, Hash: "fresh"},
		},
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []localStateRow{{Path: "fresh.txt", ItemType: ItemTypeFile, Hash: "fresh"}}, rows)

	state := readObservationStateForTest(t, eng.baseline, t.Context())
	assert.True(t, state.LocalTruthComplete)
	assert.Empty(t, state.LocalTruthRecoveryReason)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().LocalObservationFullSnapshotReplacementCount)
}

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchMarksLocalTruthSuspect(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.perfCollector = perf.NewCollector(nil)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []localStateRow{
		{Path: "known.txt", ItemType: ItemTypeFile},
	}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &localObservationBatch{
		markSuspect:    true,
		recoveryReason: localTruthRecoveryDroppedEvents,
	})
	require.NoError(t, err)

	state := readObservationStateForTest(t, eng.baseline, t.Context())
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, localTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().LocalObservationSuspectDroppedEventsCount)
}

// Validates: R-2.8.8
func TestWatchRuntime_MaintenanceMarksLocalTruthSuspectAfterDroppedObservation(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	rt.resources.localObs = newLocalObserver(bl, eng.logger, eng.checkWorkers)
	rt.resources.localObs.droppedEvents.Add(1)
	rt.resources.localObs.droppedRetries.Add(1)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []localStateRow{
		{Path: "known.txt", ItemType: ItemTypeFile},
	}))

	rt.handleMaintenanceTick(t.Context())

	state := readObservationStateForTest(t, eng.baseline, t.Context())
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, localTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)
	assert.Zero(t, rt.resources.localObs.DroppedEvents())
	assert.Zero(t, rt.resources.localObs.DroppedRetries())
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
}

// Validates: R-6.6.30
// Regression: a dropped-event rescan that keeps failing for the same reason
// warned on every maintenance tick. It now warns once, repeats at Debug, and
// warns again only after the failure changed or a rescan was scheduled.
func TestWatchRuntime_DroppedObservationRescanFailureWarnsOnce(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)
	var logBuf bytes.Buffer
	rt.deps.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	rt.resources.localObs = newLocalObserver(bl, eng.logger, eng.checkWorkers)
	rt.resources.localObs.droppedEvents.Add(1)

	// A canceled context makes marking local truth suspect fail the same way
	// on every tick.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for range 3 {
		rt.recoverDroppedLocalObservation(canceled)
	}

	const message = "Could not schedule a rescan of the sync folder after missed changes"
	warnLines := func() int {
		count := 0
		for _, line := range strings.Split(logBuf.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, message) {
				count++
			}
		}
		return count
	}
	assert.Equal(t, 1, warnLines(), "an unchanged failure warns once")
	assert.Equal(t, 3, strings.Count(logBuf.String(), message), "repeats stay visible at Debug")
	assert.Equal(t, int64(1), rt.resources.localObs.DroppedEvents(), "the dropped count is kept for the retry")

	rt.recoverDroppedLocalObservation(t.Context())
	assert.Zero(t, rt.resources.localObs.DroppedEvents(), "the retry schedules the rescan")

	rt.resources.localObs.droppedEvents.Add(1)
	rt.recoverDroppedLocalObservation(canceled)
	assert.Equal(t, 2, warnLines(), "a failure after a successful rescan is news again")
}

// Validates: R-6.6.30
// The initial pass's periodic wait line repeats only when its counts moved;
// an unchanged tick goes to Debug.
func TestWatchRuntime_BootstrapWaitLogsOnlyWhenCountsChange(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	var logBuf bytes.Buffer
	rt.deps.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	for range 3 {
		rt.logBootstrapWait(t.Context())
	}
	assert.Equal(t, 1, strings.Count(logBuf.String(), "Waiting for the remaining changes to finish"))

	rt.sched.runningCount++
	rt.logBootstrapWait(t.Context())
	assert.Equal(t, 2, strings.Count(logBuf.String(), "Waiting for the remaining changes to finish"))
}
