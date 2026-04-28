package sync

import (
	"context"
	"errors"
	"path/filepath"
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

// Validates: R-2.4.8
func TestShortcutChildRunnerPublicationIncludesExplicitCleanupScope(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := shortcutChildRunnerPublicationFromRootsWithParentRoot(
		shortcutNamespaceTestID,
		parentRoot,
		[]ShortcutRootRecord{{
			NamespaceID:       shortcutNamespaceTestID,
			BindingItemID:     "binding-cleanup",
			RelativeLocalPath: "Shortcuts/Old",
			State:             ShortcutRootStateRemovedChildCleanupPending,
		}},
	)

	require.Empty(t, publication.RunnerWork.Children)
	require.Len(t, publication.CleanupWork.Requests, 1)
	cleanup := publication.CleanupWork.Requests[0]
	assert.Equal(t, "binding-cleanup", cleanup.BindingItemID)
	assert.Equal(t, "Shortcuts/Old", cleanup.RelativeLocalPath)
	assert.Equal(t, "personal:owner@example.com|binding:binding-cleanup", cleanup.ChildMountID)
	assert.Equal(t, filepath.Join(parentRoot, "Shortcuts", "Old"), cleanup.LocalRoot)
}

func TestShortcutChildRunnerPublicationIncludesExplicitRunnerScope(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := shortcutChildRunnerPublicationFromRootsWithParentRoot(
		shortcutNamespaceTestID,
		parentRoot,
		[]ShortcutRootRecord{{
			NamespaceID:       shortcutNamespaceTestID,
			BindingItemID:     "binding-run",
			RelativeLocalPath: "Shortcuts/Run",
			LocalAlias:        "Run",
			RemoteDriveID:     driveid.New("remote-drive"),
			RemoteItemID:      "remote-root",
			State:             ShortcutRootStateActive,
		}},
	)

	require.Len(t, publication.RunnerWork.Children, 1)
	child := publication.RunnerWork.Children[0]
	assert.Equal(t, "binding-run", child.BindingItemID)
	assert.Equal(t, "personal:owner@example.com|binding:binding-run", child.ChildMountID)
	assert.Equal(t, filepath.Join(parentRoot, "Shortcuts", "Run"), child.LocalRoot)
	assert.Equal(t, ShortcutChildActionRun, child.RunnerAction)
}

// Validates: R-2.4.8
func TestShortcutChildRunnerPublicationEqualityIsSyncOwned(t *testing.T) {
	t.Parallel()

	identity := &ShortcutRootIdentity{Device: 1, Inode: 2}
	first := NormalizeShortcutChildRunnerPublication(shortcutNamespaceTestID, ShortcutChildRunnerPublication{
		RunnerWork: ShortcutChildRunnerWork{
			Children: []ShortcutChildRunner{{
				BindingItemID:     "binding-b",
				RelativeLocalPath: "B",
				RunnerAction:      ShortcutChildActionRun,
				LocalRootIdentity: identity,
			}, {
				BindingItemID:     "binding-a",
				RelativeLocalPath: "A",
				RunnerAction:      ShortcutChildActionFinalDrain,
			}},
		},
		CleanupWork: ShortcutChildArtifactCleanupWork{
			Requests: []ShortcutChildArtifactCleanupRequest{{
				BindingItemID:     "cleanup-b",
				RelativeLocalPath: "B",
				ChildMountID:      "personal:owner@example.com|binding:cleanup-b",
				LocalRoot:         filepath.Join("parent", "B"),
				Reason:            ShortcutChildArtifactCleanupParentRemoved,
			}},
		},
	})
	identity.Device = 99
	second := NormalizeShortcutChildRunnerPublication(shortcutNamespaceTestID, ShortcutChildRunnerPublication{
		NamespaceID: shortcutNamespaceTestID,
		RunnerWork: ShortcutChildRunnerWork{
			Children: []ShortcutChildRunner{{
				BindingItemID:     "binding-a",
				RelativeLocalPath: "A",
				RunnerAction:      ShortcutChildActionFinalDrain,
			}, {
				BindingItemID:     "binding-b",
				RelativeLocalPath: "B",
				RunnerAction:      ShortcutChildActionRun,
				LocalRootIdentity: &ShortcutRootIdentity{Device: 1, Inode: 2},
			}},
		},
		CleanupWork: ShortcutChildArtifactCleanupWork{
			Requests: []ShortcutChildArtifactCleanupRequest{{
				BindingItemID:     "cleanup-b",
				RelativeLocalPath: "B",
				ChildMountID:      "personal:owner@example.com|binding:cleanup-b",
				LocalRoot:         filepath.Join("parent", "B"),
				Reason:            ShortcutChildArtifactCleanupParentRemoved,
			}},
		},
	})

	assert.True(t, ShortcutChildRunnerPublicationsEqual(first, second))
	assert.Equal(t, uint64(1), first.RunnerWork.Children[1].LocalRootIdentity.Device)
}

// Validates: R-2.4.8
func TestShortcutChildAckHandleZeroReturnsExplicitErrors(t *testing.T) {
	t.Parallel()

	handle := ShortcutChildAckHandle{}

	_, err := handle.AcknowledgeChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		BindingItemID: "binding-1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shortcut child final-drain ack requires live parent")

	_, err = handle.AcknowledgeChildArtifactsPurged(t.Context(), ShortcutChildArtifactCleanupAck{
		BindingItemID: "binding-1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shortcut child artifact cleanup ack requires live parent")
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
	assert.Empty(t, got[len(got)-1].RunnerWork.Children)
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	eng.shortcutChildRunnerSink = func(ctx context.Context, publication ShortcutChildRunnerPublication) error {
		roots, err := eng.baseline.listShortcutRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, 1)
		assert.Equal(t, "binding-1", roots[0].BindingItemID)
		assert.Equal(t, ShortcutRootStateActive, roots[0].State)
		require.Len(t, publication.RunnerWork.Children, 1)
		assert.Equal(t, "binding-1", publication.RunnerWork.Children[0].BindingItemID)
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
	assert.Empty(t, got[len(got)-1].RunnerWork.Children)
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
