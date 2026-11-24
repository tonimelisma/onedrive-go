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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchUpsertsScopedRows(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.PerfCollector = perf.NewCollector(nil)
	})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []syncdomain.LocalStateRow{{
		Path:     "stale.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old",
		Size:     1,
	}}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &syncobserve.LocalObservation{
		Rows: []syncdomain.LocalStateRow{
			{
				Path:             "stale.txt",
				ItemType:         syncdomain.ItemTypeFile,
				Hash:             "new",
				Size:             2,
				Mtime:            22,
				LocalDevice:      33,
				LocalInode:       44,
				LocalHasIdentity: true,
			},
			{
				Path:     "fresh.txt",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "fresh",
				Size:     3,
			},
		},
		Dirty: true,
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.LocalStateRow{
		{
			Path:     "fresh.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "fresh",
			Size:     3,
		},
		{
			Path:             "stale.txt",
			ItemType:         syncdomain.ItemTypeFile,
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
func TestWatchRuntime_LocalObservationBatchDeletesDirectoryPrefix(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.PerfCollector = perf.NewCollector(nil)
	})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []syncdomain.LocalStateRow{
		{Path: "dir", ItemType: syncdomain.ItemTypeFolder},
		{Path: "dir/file.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "dir/nested", ItemType: syncdomain.ItemTypeFolder},
		{Path: "dir/nested/deep.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "dir-sibling.txt", ItemType: syncdomain.ItemTypeFile},
	}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &syncobserve.LocalObservation{
		DeletedPrefixes: []string{"dir"},
		Dirty:           true,
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.LocalStateRow{{Path: "dir-sibling.txt", ItemType: syncdomain.ItemTypeFile}}, rows)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	snapshot := eng.collector().Snapshot()
	assert.Equal(t, 1, snapshot.LocalObservationScopedCommitCount)
	assert.Equal(t, 1, snapshot.LocalObservationPrefixDeleteCount)
}

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchFullSnapshotReplacesLocalTruth(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.PerfCollector = perf.NewCollector(nil)
	})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.MarkLocalTruthSuspect(t.Context(), syncdomain.LocalTruthRecoveryDroppedEvents))
	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []syncdomain.LocalStateRow{
		{Path: "stale.txt", ItemType: syncdomain.ItemTypeFile},
	}))
	require.NoError(t, eng.baseline.MarkLocalTruthSuspect(t.Context(), syncdomain.LocalTruthRecoveryDroppedEvents))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &syncobserve.LocalObservation{
		Kind: syncobserve.LocalSnapshot,
		Rows: []syncdomain.LocalStateRow{
			{Path: "fresh.txt", ItemType: syncdomain.ItemTypeFile, Hash: "fresh"},
		},
	})
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.LocalStateRow{{Path: "fresh.txt", ItemType: syncdomain.ItemTypeFile, Hash: "fresh"}}, rows)

	state := syncstoretest.ReadObservationStateForTest(t, eng.baseline, t.Context())
	assert.True(t, state.LocalTruthComplete)
	assert.Empty(t, state.LocalTruthRecoveryReason)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().LocalObservationFullSnapshotReplacementCount)
}

// Validates: R-2.8.8, R-6.6.17
func TestWatchRuntime_LocalObservationBatchMarksLocalTruthSuspect(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.PerfCollector = perf.NewCollector(nil)
	})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []syncdomain.LocalStateRow{
		{Path: "known.txt", ItemType: syncdomain.ItemTypeFile},
	}))

	err := rt.handleWatchLocalObservationBatch(t.Context(), &syncobserve.LocalObservation{
		Kind:           syncobserve.LocalTruthSuspect,
		RecoveryReason: syncdomain.LocalTruthRecoveryDroppedEvents,
	})
	require.NoError(t, err)

	state := syncstoretest.ReadObservationStateForTest(t, eng.baseline, t.Context())
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, syncdomain.LocalTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().LocalObservationSuspectDroppedEventsCount)
}

// Validates: R-2.8.8
func TestWatchRuntime_MaintenanceMarksLocalTruthSuspectAfterDroppedObservation(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	rt.resources.localObs = syncobserve.NewLocalObserver(&syncobserve.LocalObserverInputs{
		Baseline:     bl,
		Logger:       eng.logger,
		CheckWorkers: eng.checkWorkers,
	})
	rt.resources.localObs.RestoreDropped(1, 1)

	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []syncdomain.LocalStateRow{
		{Path: "known.txt", ItemType: syncdomain.ItemTypeFile},
	}))

	rt.handleMaintenanceTick(t.Context())

	state := syncstoretest.ReadObservationStateForTest(t, eng.baseline, t.Context())
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, syncdomain.LocalTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)
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

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)
	var logBuf bytes.Buffer
	rt.deps.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	rt.resources.localObs = syncobserve.NewLocalObserver(&syncobserve.LocalObserverInputs{
		Baseline:     bl,
		Logger:       eng.logger,
		CheckWorkers: eng.checkWorkers,
	})
	rt.resources.localObs.RestoreDropped(1, 0)

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

	rt.resources.localObs.RestoreDropped(1, 0)
	rt.recoverDroppedLocalObservation(canceled)
	assert.Equal(t, 2, warnLines(), "a failure after a successful rescan is news again")
}

// Validates: R-6.6.30
// The initial pass's periodic wait line repeats only when its counts moved;
// an unchanged tick goes to Debug.
func TestWatchRuntime_BootstrapWaitLogsOnlyWhenCountsChange(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	var logBuf bytes.Buffer
	rt.deps.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	for range 3 {
		rt.logBootstrapWait(t.Context())
	}
	assert.Equal(t, 1, strings.Count(logBuf.String(), "Waiting for the remaining changes to finish"))

	rt.sched.Running(&syncdomain.TrackedAction{ID: 1 << 40, Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "running.txt"}})
	rt.logBootstrapWait(t.Context())
	assert.Equal(t, 2, strings.Count(logBuf.String(), "Waiting for the remaining changes to finish"))
}
