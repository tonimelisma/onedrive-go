package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_ForwardsEmptyCompleteBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	var got []ShortcutChildWorkSnapshot
	eng.shortcutChildWorkSink = func(_ context.Context, publication ShortcutChildWorkSnapshot) error {
		got = append(got, publication)
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &RemoteObservationBatch{
		ShortcutTopology: ShortcutTopologyBatch{
			Kind: ShortcutTopologyObservationComplete,
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	assert.Equal(t, shortcutNamespaceTestID, got[len(got)-1].NamespaceID)
	assert.Empty(t, got[len(got)-1].RunCommands)
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	eng.shortcutChildWorkSink = func(ctx context.Context, publication ShortcutChildWorkSnapshot) error {
		roots, err := eng.baseline.ListShortcutRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, 1)
		assert.Equal(t, "binding-1", roots[0].BindingItemID)
		assert.Equal(t, ShortcutRootStateActive, roots[0].State)
		require.Len(t, publication.RunCommands, 1)
		assert.Equal(t, "personal:owner@example.com|binding:binding-1", publication.RunCommands[0].ChildMountID)
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &RemoteObservationBatch{
		ShortcutTopology: ShortcutTopologyBatch{
			Kind: ShortcutTopologyObservationIncremental,
			Upserts: []ShortcutBindingUpsert{{
				BindingItemID:     "binding-1",
				RelativeLocalPath: "Shared/Docs",
				LocalAlias:        "Docs",
				RemoteDriveID:     "drive-1",
				RemoteItemID:      "target-1",
				Complete:          true,
			}},
		},
	})
	require.NoError(t, err)
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_SkipsEmptyIncrementalBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutChildWorkSink = func(_ context.Context, _ ShortcutChildWorkSnapshot) error {
		require.FailNow(t, "empty incremental topology batch should not be applied")
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &RemoteObservationBatch{
		ShortcutTopology: ShortcutTopologyBatch{
			Kind: ShortcutTopologyObservationIncremental,
		},
	})
	require.NoError(t, err)
}

// Validates: R-2.4.3, R-2.4.8
func TestRunOncePublishesEmptyCompleteChildChildWorkSnapshotBeforeCommittingCursor(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "cursor-empty-complete"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	var got []ShortcutChildWorkSnapshot
	eng.shortcutChildWorkSink = func(_ context.Context, publication ShortcutChildWorkSnapshot) error {
		got = append(got, publication)
		return nil
	}

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	assert.Equal(t, shortcutNamespaceTestID, got[len(got)-1].NamespaceID)
	assert.Empty(t, got[len(got)-1].RunCommands)
	assert.Equal(t, "cursor-empty-complete", readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}

// Validates: R-2.4.3, R-2.4.8
func TestRunOnceChildChildWorkSnapshotPublishFailureDoesNotCommitCursor(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "cursor-empty-complete"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	applyErr := errors.New("persist topology")
	eng.shortcutChildWorkSink = func(_ context.Context, _ ShortcutChildWorkSnapshot) error {
		return applyErr
	}

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}
