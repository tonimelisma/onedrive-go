package sync

import (
	"context"
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

	synced := "hello"
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

	batches := make(chan localObservationBatch, 1)
	obs.SetLocalObservationBatchChannel(batches)
	obs.TrySendFullLocalSnapshot(ctx, result)
	snapshot := receiveBatch(t, batches, "safety scan snapshot")
	require.True(t, snapshot.fullSnapshot)
	require.NoError(t, rt.handleWatchLocalObservationBatch(ctx, &snapshot))

	// Before the fix the snapshot carried a dirty hint, the debounce flushed
	// it, and the ready replan retired the admitted upload for a plan that
	// could only come out identical.
	if flushed := rt.resources.dirtyBuf.FlushImmediate(); flushed != nil {
		require.NoError(t, rt.handleWatchReplanReady(ctx, pipe, *flushed))
	}

	assert.False(t, rt.hasPendingReplan(), "a rescan that changed no committed truth schedules no replan")
	dispatchCh, next := rt.dispatchChannelForOutbox()
	require.NotNil(t, next, "the admitted upload is still dispatchable")
	assert.NotNil(t, dispatchCh)
	assert.Equal(t, "edited.txt", next.Action.Path)
	assert.Equal(t, 0, eng.collector().Snapshot().SupersededPendingReplanRetirementCount)
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
	ctx := t.Context()

	bad := skippedItem{Path: "docs/bad:name.txt", Reason: IssueInvalidFilename, Detail: "name contains ':'"}

	_, err := rt.handleWatchSkippedSignal(ctx, []skippedItem{bad}, true)
	require.NoError(t, err)
	assert.NotNil(t, rt.resources.dirtyBuf.FlushImmediate(), "a new finding replans")

	_, err = rt.handleWatchSkippedSignal(ctx, []skippedItem{bad}, true)
	require.NoError(t, err)
	assert.Nil(t, rt.resources.dirtyBuf.FlushImmediate(), "the same finding re-reported by the next scan does not")

	_, err = rt.handleWatchSkippedSignal(ctx, nil, true)
	require.NoError(t, err)
	assert.NotNil(t, rt.resources.dirtyBuf.FlushImmediate(), "a finding that cleared replans")
}
