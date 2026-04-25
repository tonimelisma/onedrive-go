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

const shortcutTopologyTestNamespaceID = "personal:owner@example.com"

// Validates: R-2.4.3, R-2.4.8
func TestShortcutTopologyBatch_ShouldApplyCompleteEvenWithoutFacts(t *testing.T) {
	t.Parallel()

	assert.True(t, ShortcutTopologyBatch{
		Kind: ShortcutTopologyObservationComplete,
	}.ShouldApply())
	assert.True(t, ShortcutTopologyBatch{
		Kind: ShortcutTopologyObservationIncremental,
		Deletes: []ShortcutBindingDelete{{
			BindingItemID: "binding-1",
		}},
	}.ShouldApply())
	assert.False(t, ShortcutTopologyBatch{
		Kind: ShortcutTopologyObservationIncremental,
	}.ShouldApply())
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutTopologyBatch_ForwardsEmptyCompleteBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID
	var got []ShortcutTopologyBatch
	eng.shortcutTopologyHandler = func(_ context.Context, batch ShortcutTopologyBatch) error {
		got = append(got, batch)
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutTopologyBatch(t.Context(), &remoteObservationBatch{
		shortcutTopology: ShortcutTopologyBatch{
			Kind: ShortcutTopologyObservationComplete,
		},
	})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, shortcutTopologyTestNamespaceID, got[0].NamespaceID)
	assert.Equal(t, ShortcutTopologyObservationComplete, got[0].Kind)
	assert.False(t, got[0].HasFacts())
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutTopologyBatch_SkipsEmptyIncrementalBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutTopologyHandler = func(_ context.Context, _ ShortcutTopologyBatch) error {
		require.FailNow(t, "empty incremental topology batch should not be applied")
		return nil
	}

	err := testEngineFlow(t, eng).applyShortcutTopologyBatch(t.Context(), &remoteObservationBatch{
		shortcutTopology: ShortcutTopologyBatch{
			Kind: ShortcutTopologyObservationIncremental,
		},
	})
	require.NoError(t, err)
}

// Validates: R-2.4.3, R-2.4.8
func TestRefreshShortcutTopology_ForwardsEmptyCompleteBatchWithoutCommittingCursor(t *testing.T) {
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
	eng.shortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID
	var got []ShortcutTopologyBatch
	eng.shortcutTopologyHandler = func(_ context.Context, batch ShortcutTopologyBatch) error {
		got = append(got, batch)
		return nil
	}

	err := eng.RefreshShortcutTopology(t.Context())
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, ShortcutTopologyObservationComplete, got[0].Kind)
	assert.False(t, got[0].HasFacts())
	assert.Empty(t, readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}

// Validates: R-2.4.3, R-2.4.8
func TestRefreshShortcutTopology_ApplyFailureDoesNotCommitCursor(t *testing.T) {
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
	applyErr := errors.New("persist topology")
	eng.shortcutTopologyHandler = func(_ context.Context, _ ShortcutTopologyBatch) error {
		return applyErr
	}

	err := eng.RefreshShortcutTopology(t.Context())
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}
