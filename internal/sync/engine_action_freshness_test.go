package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.8.9, R-6.6.17
func TestEngineAdmissionFreshness_RemoteMismatchRetiresWithoutDispatchOrDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.perfCollector = perf.NewCollector(nil)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	remoteDriveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "download.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "changed",
		Size:     12,
		ETag:     "etag-changed",
	}}, "delta-1", remoteDriveID))

	flow.initializeRuntimeState(&runtimePlan{})
	flow.sched.graph = syncexec.NewDepGraph(eng.logger)

	root := flow.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "download.txt",
		ItemID:  "remote-1",
		DriveID: remoteDriveID,
		View: &syncdomain.PathView{
			Path: "download.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  remoteDriveID,
				ItemID:   "remote-1",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "planned",
				Size:     12,
				ETag:     "etag-planned",
			},
		},
	}, 1, nil)
	require.NotNil(t, root)

	child := flow.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "dependent.txt",
		View: &syncdomain.PathView{Path: "dependent.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := flow.admitReady(ctx, []*syncdomain.TrackedAction{root})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Equal(t, 0, flow.sched.graph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().SupersededEngineAdmissionCount)
}
