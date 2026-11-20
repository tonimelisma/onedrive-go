package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Live incident, 2026-11-01: a watch runtime whose steady-state replan took
// ten minutes had its 166-action plan retired by the first safety scan after
// it was installed, replanned for another ten minutes, had the 466-action
// plan retired by the next safety scan, and so on: nothing was ever
// dispatched while `status` said "Up to date". The safety scan had changed no
// local_state row; its snapshot batch carried `dirty` because the scan's
// events are counted against the baseline, which includes every change the
// plan already covered.
//
// Validates: R-2.8.21
func TestWatchRuntime_SafetyScanWithoutNewTruthKeepsAdmittedPlan(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	synced := "rescan keeps the synced content"
	syncedHash := hashContentQuickXor(t, synced)
	seedBaseline(t, eng.baseline, ctx, []actionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "edited.txt",
		DriveID:         driveID,
		ItemID:          "item-edited",
		ItemType:        ItemTypeFile,
		RemoteHash:      syncedHash,
		LocalHash:       syncedHash,
		LocalSize:       int64(len(synced)),
		LocalSizeKnown:  true,
		RemoteSize:      int64(len(synced)),
		RemoteSizeKnown: true,
	}}, "")
	// The local edit the plan is for.
	writeLocalFile(t, syncRoot, "edited.txt", "hello, edited")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)
	pipe := &watchPipeline{bl: bl, mode: SyncBidirectional}

	applied, err := rt.runSteadyStateReplan(ctx, pipe, dirtyBatch{})
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, rt.currentOutbox(), 1, "the edit is planned as one upload")
	require.Equal(t, ActionUpload, rt.currentOutbox()[0].Action.Type)
	require.Nil(t, rt.resources.dirtyBuf.FlushImmediate(), "installing the plan leaves no dirty signal behind")

	// The observer's safety scan of the unchanged tree, exactly as watch
	// mode produces it: a full scan against the baseline, handed over as a
	// full snapshot batch.
	obs := newLocalObserver(bl, eng.logger, 0)
	obs.SetFilterConfig(eng.contentFilter)
	obs.SetObservationRules(eng.localRules)
	obs.SetSyncRootIdentity(eng.syncRootIdentity)
	result, err := obs.FullScan(ctx, eng.syncTree)
	require.NoError(t, err)
	require.NotEmpty(t, result.Events, "against the baseline the edit is still an event")

	batches := make(chan localObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)
	obs.TrySendFullLocalSnapshot(ctx, result)
	snapshot := receiveWithin(t, batches, "safety scan snapshot")
	require.Equal(t, localSnapshot, snapshot.kind)
	require.NoError(t, rt.handleWatchLocalObservationBatch(ctx, &snapshot))

	// Before the fix the snapshot carried a dirty hint, the debounce flushed
	// it, and the ready replan retired the admitted upload for a plan that
	// could only come out identical.
	if flushed := rt.resources.dirtyBuf.FlushImmediate(); flushed != nil {
		require.NoError(t, rt.handleWatchReplanReady(ctx, pipe, *flushed))
	}

	assert.False(t, rt.hasPendingReplan(), "a rescan that changed no committed truth schedules no replan")
	dispatchCh, next := rt.dispatchChannelForOutbox()
	require.GreaterOrEqual(t, next, 0, "the admitted upload is still dispatchable")
	assert.NotNil(t, dispatchCh)
	assert.Equal(t, "edited.txt", rt.currentOutbox()[next].Action.Path)
	assert.Equal(t, 0, eng.collector().Snapshot().SupersededPendingReplanRetirementCount)
}

// applySnapshot hands a snapshot to the runtime as the watch loop does.
func applySnapshot(t *testing.T, rt *watchRuntime, snapshot *localObservation) {
	t.Helper()

	snapshot.kind = localSnapshot
	require.NoError(t, rt.handleWatchLocalObservationBatch(t.Context(), snapshot))
}

// A full scan's skipped findings are planner input, so a finding that appears
// or clears replans; the re-report every safety scan makes of a persistent
// finding does not.
//
// Validates: R-2.8.21
func TestWatchRuntime_SkippedFindingsReplanOnlyWhenTheyChange(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)
	applySnapshot(t, rt, &localObservation{})
	rt.resources.dirtyBuf.FlushImmediate()

	bad := skippedItem{Path: "docs/bad:name.txt", Reason: IssueInvalidFilename, Detail: "name contains ':'"}

	applySnapshot(t, rt, &localObservation{findings: []skippedItem{bad}})
	assert.NotNil(t, rt.resources.dirtyBuf.FlushImmediate(), "a new finding replans")

	applySnapshot(t, rt, &localObservation{findings: []skippedItem{bad}})
	assert.Nil(t, rt.resources.dirtyBuf.FlushImmediate(), "the same finding re-reported by the next scan does not")

	applySnapshot(t, rt, &localObservation{})
	assert.NotNil(t, rt.resources.dirtyBuf.FlushImmediate(), "a finding that cleared replans")
}

// From the safety scan through the runtime to the dirty mark: the scan that
// no longer finds the last finding's cause clears it and replans.
//
// Validates: R-2.8.21, R-2.8.22
func TestWatchRuntime_ASnapshotWhoseLastFindingClearedMarksDirty(t *testing.T) {
	t.Parallel()

	const leftoverName = ".onedrive-go.0123456789abcdef.detach"
	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)
	obs := newLocalObserver(NewBaselineForTest(nil), eng.logger, 1)
	batches := make(chan localObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)
	tree := mustOpenSyncTree(t, syncRoot)
	leftover := filepath.Join(syncRoot, leftoverName)
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	require.True(t, obs.runSafetyScan(t.Context(), tree))
	applySnapshot(t, rt, forwardedSnapshot(t, batches))
	require.Equal(t, []string{leftoverName}, interruptedRemovalStatusPaths(t, eng.baseline))
	rt.resources.dirtyBuf.FlushImmediate()

	require.NoError(t, os.Remove(leftover))
	require.True(t, obs.runSafetyScan(t.Context(), tree))
	applySnapshot(t, rt, forwardedSnapshot(t, batches))

	assert.Empty(t, interruptedRemovalStatusPaths(t, eng.baseline))
	assert.NotNil(t, rt.resources.dirtyBuf.FlushImmediate(), "the cleared finding replans")
}

// While watching is unavailable, the periodic full scan stands in for both
// the watcher and the safety scan, so it hands the engine its snapshot, with
// its finding set, the way a safety scan does, the empty set included.
//
// Validates: R-2.8.22, R-6.4.9
func TestWatchRuntime_FallbackScanRecordsAndClearsFindings(t *testing.T) {
	t.Parallel()

	const leftoverName = ".onedrive-go.0123456789abcdef.detach"
	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	obs := newLocalObserver(NewBaselineForTest(nil), eng.logger, 1)
	batches := make(chan localObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)
	tree := mustOpenSyncTree(t, syncRoot)
	leftover := filepath.Join(syncRoot, leftoverName)
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))
	scanFailing := false

	require.True(t, rt.runFallbackScan(ctx, obs, tree, &scanFailing))
	applySnapshot(t, rt, forwardedSnapshot(t, batches))
	assert.Equal(t, []string{leftoverName}, interruptedRemovalStatusPaths(t, eng.baseline))

	require.NoError(t, os.Remove(leftover))
	require.True(t, rt.runFallbackScan(ctx, obs, tree, &scanFailing))
	applySnapshot(t, rt, forwardedSnapshot(t, batches))
	assert.Empty(t, interruptedRemovalStatusPaths(t, eng.baseline), "the scan that no longer finds it clears it")
}

// Validates: R-2.8.8, R-2.8.22
func TestWatchRuntime_ASnapshotCommitsRowsFindingsAndMountsTogether(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	snapshot := localObservation{
		rows:          []localStateRow{{Path: "disk", ItemType: ItemTypeFolder}},
		findings:      []skippedItem{{Path: "bad:name.txt", Reason: IssueInvalidFilename}},
		crossedMounts: []string{"disk"},
	}

	// A finding the local findings batch does not manage fails the commit
	// part way: none of the three is stored.
	failing := snapshot
	failing.kind = localSnapshot
	failing.findings = append([]skippedItem{{Path: "quota", Reason: IssueQuotaExceeded}}, snapshot.findings...)
	require.Error(t, rt.handleWatchLocalObservationBatch(ctx, &failing))
	assertLocalSnapshotStored(t, eng, nil, nil, nil)

	applySnapshot(t, rt, &snapshot)
	assertLocalSnapshotStored(t, eng, []string{"disk"}, []string{"bad:name.txt"}, []string{"disk"})
}

func assertLocalSnapshotStored(t *testing.T, eng *testEngine, rows, findings, mounts []string) {
	t.Helper()

	ctx := t.Context()
	stored, err := eng.baseline.ListLocalState(ctx)
	require.NoError(t, err)
	storedRows := make([]string, 0, len(stored))
	for i := range stored {
		storedRows = append(storedRows, stored[i].Path)
	}
	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	storedFindings := make([]string, 0, len(issues))
	for i := range issues {
		storedFindings = append(storedFindings, issues[i].Path)
	}
	storedMounts, err := eng.baseline.listMountBoundaries(ctx)
	require.NoError(t, err)

	assert.ElementsMatch(t, rows, storedRows, "rows")
	assert.ElementsMatch(t, findings, storedFindings, "findings")
	assert.ElementsMatch(t, mounts, storedMounts, "mounts")
}

// Validates: R-2.8.22
func TestWatchRuntime_ASnapshotRefreshesTheObserversMountFlags(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	obs := newLocalObserver(NewBaselineForTest(nil), eng.logger, 1)
	obs.SetMountBoundaryFlags(nil)
	rt.resources.localObs = obs

	applySnapshot(t, rt, &localObservation{
		rows:          []localStateRow{{Path: "disk", ItemType: ItemTypeFolder}},
		crossedMounts: []string{"disk"},
	})

	flags := obs.mountBoundaryFlags.Load()
	require.NotNil(t, flags)
	assert.Contains(t, *flags, "disk", "a disk mounted during the watch is flagged for the next scan")
}
