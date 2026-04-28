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
func TestShortcutChildProcessSnapshotIncludesExplicitCleanupScope(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := shortcutChildProcessSnapshotFromRootsWithParentRoot(
		shortcutNamespaceTestID,
		parentRoot,
		[]ShortcutRootRecord{{
			NamespaceID:       shortcutNamespaceTestID,
			BindingItemID:     "binding-cleanup",
			RelativeLocalPath: "Shortcuts/Old",
			State:             ShortcutRootStateRemovedChildCleanupPending,
		}},
	)

	require.Empty(t, publication.RunCommands)
	require.Len(t, publication.Cleanups, 1)
	cleanup := publication.Cleanups[0]
	assert.Equal(t, "personal:owner@example.com|binding:binding-cleanup", cleanup.ChildMountID)
	assert.Equal(t, filepath.Join(parentRoot, "Shortcuts", "Old"), cleanup.LocalRoot)
	assert.False(t, cleanup.AckRef.IsZero())
}

func TestShortcutChildProcessSnapshotIncludesExplicitRunnerScope(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := shortcutChildProcessSnapshotFromRootsWithParentRoot(
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

	require.Len(t, publication.RunCommands, 1)
	child := publication.RunCommands[0]
	assert.Equal(t, "personal:owner@example.com|binding:binding-run", child.ChildMountID)
	assert.Equal(t, filepath.Join(parentRoot, "Shortcuts", "Run"), child.Engine.LocalRoot)
	assert.Equal(t, ShortcutChildRunModeNormal, child.Mode)
	assert.False(t, child.AckRef.IsZero())
}

// Validates: R-2.4.8
func TestShortcutChildProcessSnapshotEqualityIsSyncOwned(t *testing.T) {
	t.Parallel()

	identity := &ShortcutRootIdentity{Device: 1, Inode: 2}
	first := NormalizeShortcutChildProcessSnapshot(shortcutNamespaceTestID, ShortcutChildProcessSnapshot{
		RunCommands: []ShortcutChildRunCommand{{
			ChildMountID: "personal:owner@example.com|binding:binding-b",
			DisplayName:  "B",
			Engine: ShortcutChildEngineSpec{
				LocalRootIdentity: identity,
			},
			Mode:   ShortcutChildRunModeNormal,
			AckRef: NewShortcutChildAckRef("binding-b"),
		}, {
			ChildMountID: "personal:owner@example.com|binding:binding-a",
			DisplayName:  "A",
			Mode:         ShortcutChildRunModeFinalDrain,
			AckRef:       NewShortcutChildAckRef("binding-a"),
		}},
		Cleanups: []ShortcutChildCleanupCommand{{
			ChildMountID: "personal:owner@example.com|binding:cleanup-b",
			LocalRoot:    filepath.Join("parent", "B"),
			Reason:       ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       NewShortcutChildAckRef("cleanup-b"),
		}},
	})
	identity.Device = 99
	second := NormalizeShortcutChildProcessSnapshot(shortcutNamespaceTestID, ShortcutChildProcessSnapshot{
		NamespaceID: shortcutNamespaceTestID,
		RunCommands: []ShortcutChildRunCommand{{
			ChildMountID: "personal:owner@example.com|binding:binding-a",
			DisplayName:  "A",
			Mode:         ShortcutChildRunModeFinalDrain,
			AckRef:       NewShortcutChildAckRef("binding-a"),
		}, {
			ChildMountID: "personal:owner@example.com|binding:binding-b",
			DisplayName:  "B",
			Engine: ShortcutChildEngineSpec{
				LocalRootIdentity: &ShortcutRootIdentity{Device: 1, Inode: 2},
			},
			Mode:   ShortcutChildRunModeNormal,
			AckRef: NewShortcutChildAckRef("binding-b"),
		}},
		Cleanups: []ShortcutChildCleanupCommand{{
			ChildMountID: "personal:owner@example.com|binding:cleanup-b",
			LocalRoot:    filepath.Join("parent", "B"),
			Reason:       ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       NewShortcutChildAckRef("cleanup-b"),
		}},
	})

	assert.True(t, ShortcutChildProcessSnapshotsEqual(first, second))
	assert.Equal(t, uint64(1), first.RunCommands[1].Engine.LocalRootIdentity.Device)
}

// Validates: R-2.4.8
func TestShortcutChildAckHandleZeroReturnsExplicitErrors(t *testing.T) {
	t.Parallel()

	handle := ShortcutChildAckHandle{}

	_, err := handle.AcknowledgeChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		Ref: NewShortcutChildAckRef("binding-1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shortcut child final-drain ack requires live parent")

	_, err = handle.AcknowledgeChildArtifactsPurged(t.Context(), ShortcutChildArtifactCleanupAck{
		Ref: NewShortcutChildAckRef("binding-1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shortcut child artifact cleanup ack requires live parent")
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_ForwardsEmptyCompleteBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	var got []ShortcutChildProcessSnapshot
	eng.shortcutChildProcessSink = func(_ context.Context, publication ShortcutChildProcessSnapshot) error {
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
	assert.Empty(t, got[len(got)-1].RunCommands)
}

// Validates: R-2.4.3, R-2.4.8
func TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	eng.shortcutChildProcessSink = func(ctx context.Context, publication ShortcutChildProcessSnapshot) error {
		roots, err := eng.baseline.listShortcutRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, 1)
		assert.Equal(t, "binding-1", roots[0].BindingItemID)
		assert.Equal(t, ShortcutRootStateActive, roots[0].State)
		require.Len(t, publication.RunCommands, 1)
		assert.Equal(t, "personal:owner@example.com|binding:binding-1", publication.RunCommands[0].ChildMountID)
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
	eng.shortcutChildProcessSink = func(_ context.Context, _ ShortcutChildProcessSnapshot) error {
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
func TestRunOncePublishesEmptyCompleteChildChildProcessSnapshotBeforeCommittingCursor(t *testing.T) {
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
	var got []ShortcutChildProcessSnapshot
	eng.shortcutChildProcessSink = func(_ context.Context, publication ShortcutChildProcessSnapshot) error {
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
func TestRunOnceChildChildProcessSnapshotPublishFailureDoesNotCommitCursor(t *testing.T) {
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
	eng.shortcutChildProcessSink = func(_ context.Context, _ ShortcutChildProcessSnapshot) error {
		return applyErr
	}

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, readObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}
