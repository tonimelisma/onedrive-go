package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_ForwardsEmptyCompleteBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	var got []syncshortcut.ShortcutChildWorkSnapshot
	eng.shortcutChildWorkSink = func(_ context.Context, publication syncshortcut.ShortcutChildWorkSnapshot) error {
		got = append(got, publication)
		return nil
	}
	refreshTestFlowInputs(eng)

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &syncobserve.RemoteObservationBatch{
		ShortcutTopology: syncshortcut.ShortcutTopologyBatch{
			Kind: syncshortcut.ShortcutTopologyObservationComplete,
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	assert.Equal(t, syncfixture.ShortcutNamespaceTestID, got[len(got)-1].NamespaceID)
	assert.Empty(t, got[len(got)-1].RunCommands)
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	eng.shortcutChildWorkSink = func(ctx context.Context, publication syncshortcut.ShortcutChildWorkSnapshot) error {
		roots, err := eng.baseline.ListShortcutRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, 1)
		assert.Equal(t, "binding-1", roots[0].BindingItemID)
		assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
		require.Len(t, publication.RunCommands, 1)
		assert.Equal(t, "personal:owner@example.com|binding:binding-1", publication.RunCommands[0].ChildMountID)
		return nil
	}
	refreshTestFlowInputs(eng)

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &syncobserve.RemoteObservationBatch{
		ShortcutTopology: syncshortcut.ShortcutTopologyBatch{
			Kind: syncshortcut.ShortcutTopologyObservationIncremental,
			Upserts: []syncshortcut.ShortcutBindingUpsert{{
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

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	eng.shortcutChildWorkSink = func(_ context.Context, _ syncshortcut.ShortcutChildWorkSnapshot) error {
		require.FailNow(t, "empty incremental topology batch should not be applied")
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &syncobserve.RemoteObservationBatch{
		ShortcutTopology: syncshortcut.ShortcutTopologyBatch{
			Kind: syncshortcut.ShortcutTopologyObservationIncremental,
		},
	})
	require.NoError(t, err)
}

// Validates: R-2.4.3, R-2.4.8
func TestRunOncePublishesEmptyCompleteChildChildWorkSnapshotBeforeCommittingCursor(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "cursor-empty-complete"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	var got []syncshortcut.ShortcutChildWorkSnapshot
	eng.shortcutChildWorkSink = func(_ context.Context, publication syncshortcut.ShortcutChildWorkSnapshot) error {
		got = append(got, publication)
		return nil
	}
	refreshTestFlowInputs(eng)

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	assert.Equal(t, syncfixture.ShortcutNamespaceTestID, got[len(got)-1].NamespaceID)
	assert.Empty(t, got[len(got)-1].RunCommands)
	assert.Equal(t, "cursor-empty-complete", syncstoretest.ReadObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}

// Validates: R-2.4.3, R-2.4.8
func TestRunOnceChildChildWorkSnapshotPublishFailureDoesNotCommitCursor(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "cursor-empty-complete"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	applyErr := errors.New("persist topology")
	eng.shortcutChildWorkSink = func(_ context.Context, _ syncshortcut.ShortcutChildWorkSnapshot) error {
		return applyErr
	}
	refreshTestFlowInputs(eng)

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, syncstoretest.ReadObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}
