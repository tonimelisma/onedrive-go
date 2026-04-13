package sync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

func TestProcessCommittedPrimaryWatchBatch_CommitsObservationsAndToken(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)

	rt := testWatchRuntime(t, eng)
	rt.setScopeSnapshot(syncscope.Snapshot{}, 7)

	driveID := eng.driveID
	events, err := rt.processCommittedPrimaryWatchBatch(
		t.Context(),
		emptyBaseline(),
		[]ChangeEvent{{
			Type:     ChangeCreate,
			Source:   SourceRemote,
			DriveID:  driveID,
			ItemID:   "item-1",
			ParentID: "root",
			Path:     "docs/report.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-1",
			Size:     42,
		}},
		"token-1",
	)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, "docs/report.txt", events[0].Path)

	row := readRemoteStateRow(t, eng.baseline.DB(), "item-1")
	require.NotNil(t, row)
	assert.Equal(t, "docs/report.txt", row.Path)
	assert.False(t, row.IsFiltered)
	assert.Equal(t, int64(0), row.FilterGeneration)
	assert.Equal(t, "token-1", readDeltaToken(t, eng.baseline.DB(), driveID.String()))
}

// Validates: R-2.4.5
func TestProcessCommittedPrimaryWatchBatch_PersistsFilteredRowsWithoutEmittingThem(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)

	snapshot, err := syncscope.NewSnapshot(syncscope.Config{
		SyncPaths: []string{"Allowed"},
	}, nil)
	require.NoError(t, err)

	rt := testWatchRuntime(t, eng)
	rt.setScopeSnapshot(snapshot, 11)

	driveID := eng.driveID
	events, err := rt.processCommittedPrimaryWatchBatch(
		t.Context(),
		emptyBaseline(),
		[]ChangeEvent{{
			Type:     ChangeCreate,
			Source:   SourceRemote,
			DriveID:  driveID,
			ItemID:   "filtered-item",
			ParentID: "root",
			Path:     "Blocked/file.txt",
			ItemType: ItemTypeFile,
		}},
		"token-filtered",
	)
	require.NoError(t, err)
	assert.Empty(t, events, "out-of-scope remote rows should persist without reaching the planner")

	row := readRemoteStateRow(t, eng.baseline.DB(), "filtered-item")
	require.NotNil(t, row)
	assert.Equal(t, "Blocked/file.txt", row.Path)
	assert.True(t, row.IsFiltered)
	assert.Equal(t, RemoteFilterPathScope, row.FilterReason)
	assert.Equal(t, int64(11), row.FilterGeneration)
	assert.Equal(t, "token-filtered", readDeltaToken(t, eng.baseline.DB(), driveID.String()))
}

// Validates: R-2.10.16
func TestProcessCommittedPrimaryWatchBatch_RunsShortcutFollowUpAfterCommit(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  remoteDriveID.String(),
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedDocs",
		Observation:  ObservationEnumerate,
		DiscoveredAt: 1000,
	}))

	rawEngine := &Engine{
		baseline: mgr,
		driveID:  driveid.New(engineTestDriveID),
		logger:   testLogger(t),
		recursiveLister: &mockRecursiveLister{
			items: []graph.Item{{
				ID:       "shared-file",
				Name:     "note.txt",
				ParentID: "remote-item-1",
				DriveID:  remoteDriveID,
			}},
		},
	}
	eng := newFlowBackedTestEngine(rawEngine)
	setupWatchEngine(t, eng)

	rt := testWatchRuntime(t, eng)
	rt.setScopeSnapshot(syncscope.Snapshot{}, 3)

	events, err := rt.processCommittedPrimaryWatchBatch(
		ctx,
		emptyBaseline(),
		[]ChangeEvent{{
			Type:          ChangeShortcut,
			Source:        SourceRemote,
			DriveID:       rawEngine.driveID,
			ItemID:        "sc-1",
			Path:          "SharedDocs",
			ItemType:      ItemTypeFolder,
			RemoteDriveID: remoteDriveID.String(),
			RemoteItemID:  "remote-item-1",
		}},
		"token-shortcut",
	)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, "SharedDocs/note.txt", events[0].Path)
	assert.Equal(t, "token-shortcut", readDeltaToken(t, mgr.DB(), rawEngine.driveID.String()))
}

func TestRunWatchStep_FatalObserverErrorStopsWatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)

	rt := testWatchRuntime(t, eng)
	errs := make(chan error, 1)
	errs <- newFatalObserverError(errors.New("commit primary watch observations: boom"))

	done, err := rt.runWatchStep(t.Context(), &watchPipeline{
		runtime:   rt,
		errs:      errs,
		activeObs: 1,
	})
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "commit primary watch observations: boom")
}
