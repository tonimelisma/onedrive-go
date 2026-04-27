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

const shortcutNamespaceTestID = "personal:owner@example.com"

// Validates: R-2.4.3, R-2.4.8
func TestShortcutObservationBatch_ShouldApplyCompleteEvenWithoutFacts(t *testing.T) {
	t.Parallel()

	assert.True(t, shortcutTopologyBatch{
		Kind: shortcutTopologyObservationComplete,
	}.shouldApply())
	assert.True(t, shortcutTopologyBatch{
		Kind: shortcutTopologyObservationIncremental,
		Deletes: []shortcutBindingDelete{{
			BindingItemID: "binding-1",
		}},
	}.shouldApply())
	assert.False(t, shortcutTopologyBatch{
		Kind: shortcutTopologyObservationIncremental,
	}.shouldApply())
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_ForwardsEmptyCompleteBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	var got []ShortcutChildRunnerPublication
	eng.shortcutChildRunnerSink = func(_ context.Context, publication ShortcutChildRunnerPublication) error {
		got = append(got, publication)
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &remoteObservationBatch{
		shortcutTopology: shortcutTopologyBatch{
			Kind: shortcutTopologyObservationComplete,
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	assert.Equal(t, shortcutNamespaceTestID, got[len(got)-1].NamespaceID)
	assert.Empty(t, got[len(got)-1].Children)
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	eng.shortcutChildRunnerSink = func(ctx context.Context, publication ShortcutChildRunnerPublication) error {
		roots, err := eng.baseline.ListShortcutRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, 1)
		assert.Equal(t, "binding-1", roots[0].BindingItemID)
		assert.Equal(t, ShortcutRootStateActive, roots[0].State)
		require.Len(t, publication.Children, 1)
		assert.Equal(t, "binding-1", publication.Children[0].BindingItemID)
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &remoteObservationBatch{
		shortcutTopology: shortcutTopologyBatch{
			Kind: shortcutTopologyObservationIncremental,
			Upserts: []shortcutBindingUpsert{{
				BindingItemID:     "binding-1",
				RelativeLocalPath: "Shared/Docs",
				LocalAlias:        "Docs",
				RemoteDriveID:     "drive-1",
				RemoteItemID:      "target-1",
				RemoteIsFolder:    true,
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
	eng.shortcutChildRunnerSink = func(_ context.Context, _ ShortcutChildRunnerPublication) error {
		require.FailNow(t, "empty incremental topology batch should not be applied")
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutObservationBatch(t.Context(), &remoteObservationBatch{
		shortcutTopology: shortcutTopologyBatch{
			Kind: shortcutTopologyObservationIncremental,
		},
	})
	require.NoError(t, err)
}

// Validates: R-2.4.3, R-2.4.8
func TestRunOncePublishesEmptyCompleteChildRunnerPublicationBeforeCommittingCursor(t *testing.T) {
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
	var got []ShortcutChildRunnerPublication
	eng.shortcutChildRunnerSink = func(_ context.Context, publication ShortcutChildRunnerPublication) error {
		got = append(got, publication)
		return nil
	}

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	assert.Equal(t, shortcutNamespaceTestID, got[len(got)-1].NamespaceID)
	assert.Empty(t, got[len(got)-1].Children)
	assert.Equal(t, "cursor-empty-complete", readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}

// Validates: R-2.4.3, R-2.4.8
func TestRunOnceChildRunnerPublicationPublishFailureDoesNotCommitCursor(t *testing.T) {
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
	eng.shortcutChildRunnerSink = func(_ context.Context, _ ShortcutChildRunnerPublication) error {
		return applyErr
	}

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}
