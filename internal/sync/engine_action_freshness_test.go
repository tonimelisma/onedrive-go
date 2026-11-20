package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// Validates: R-2.8.9, R-6.6.17
func TestEngineAdmissionFreshness_RemoteMismatchRetiresWithoutDispatchOrDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.perfCollector = perf.NewCollector(nil)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "download.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     12,
		ETag:     "etag-changed",
	}}, "delta-1", remoteDriveID))

	flow.initializeRuntimeState(&runtimePlan{})
	flow.sched.graph = NewDepGraph(eng.logger)

	root := flow.sched.graph.Add(&Action{
		Type:    ActionDownload,
		Path:    "download.txt",
		ItemID:  "remote-1",
		DriveID: remoteDriveID,
		View: &PathView{
			Path: "download.txt",
			Remote: &RemoteState{
				DriveID:  remoteDriveID,
				ItemID:   "remote-1",
				ItemType: ItemTypeFile,
				Hash:     "planned",
				Size:     12,
				ETag:     "etag-planned",
			},
		},
	}, 1, nil)
	require.NotNil(t, root)

	child := flow.sched.graph.Add(&Action{
		Type: ActionUpload,
		Path: "dependent.txt",
		View: &PathView{Path: "dependent.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := flow.admitReady(ctx, []*TrackedAction{root})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Equal(t, 0, flow.sched.graph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().SupersededEngineAdmissionCount)
}
