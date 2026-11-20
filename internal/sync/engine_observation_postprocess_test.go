package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func testRemoteCreateEvent(path string, itemID string, driveID string) syncdomain.RemoteItem {
	return syncdomain.RemoteItem{ObservedItem: syncdomain.ObservedItem{
		Path:     path,
		ItemID:   itemID,
		ParentID: "root",
		DriveID:  mustParseDriveID(driveID),
		ItemType: syncdomain.ItemTypeFile,
		Hash:     fmt.Sprintf("hash-%s", itemID),
		Size:     12,
		Mtime:    123,
		ETag:     fmt.Sprintf("etag-%s", itemID),
	}}
}

func testMountRootWatchBatch(
	engine *Engine,
	mode syncdomain.RemoteObservationMode,
	events []syncdomain.RemoteItem,
	cursorToken string,
	findings syncdomain.ObservationFindingsBatch,
) syncobserve.RemoteObservationBatch {
	batch := buildRemoteObservationBatch(
		engine,
		mode,
		events,
		cursorToken,
		false,
		findings,
	)
	batch.Source = syncobserve.RemoteObservationBatchMountRoot
	batch.ApplyAck = make(chan error, 1)

	return batch
}

func mustParseDriveID(raw string) driveid.ID {
	id := driveid.New(raw)
	if id.IsZero() {
		panic("test drive ID must be non-zero")
	}
	return id
}

// Validates: R-2.4.3, R-2.4.8
func TestHandleRemoteObservationBatch_EmptyCompleteTopologyApplyFailureDoesNotCommitCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	applyErr := errors.New("persist topology")
	eng.shortcutChildWorkSink = func(_ context.Context, _ syncshortcut.ShortcutChildWorkSnapshot) error {
		return applyErr
	}

	batch := buildPrimaryWatchBatch(eng.Engine, nil, nil, "cursor-empty-complete")
	batch.CursorToken = "cursor-empty-complete"
	batch.ShortcutTopology = syncshortcut.ShortcutTopologyBatch{
		NamespaceID: "personal:owner@example.com",
		Kind:        syncshortcut.ShortcutTopologyObservationComplete,
	}
	err := rt.handleRemoteObservationBatch(t.Context(), &batch)
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, syncstoretest.ReadObservationCursorForTest(t, eng.baseline, t.Context(), eng.driveID.String()))
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_PrimaryWatchCommitsObservedRowsAndCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	batch := buildPrimaryWatchBatch(eng.Engine, nil, []syncdomain.RemoteItem{
		testRemoteCreateEvent("primary-watch.txt", "item-primary", eng.driveID.String()),
	}, "cursor-primary")
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "primary-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-primary", remoteRow.ItemID)
	assert.Equal(t, "cursor-primary", syncstoretest.ReadObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_FilteredPrimaryWatchDoesNotMarkDirty(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	eng.contentFilter = syncdomain.ContentFilterConfig{IgnoredDirs: []string{"hidden"}}
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)
	ctx := t.Context()

	batch := buildPrimaryWatchBatch(eng.Engine, nil, []syncdomain.RemoteItem{
		testRemoteCreateEvent("hidden/remote.txt", "item-hidden", eng.driveID.String()),
	}, "cursor-hidden")
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "hidden/remote.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-hidden", remoteRow.ItemID)
	assert.Equal(t, "cursor-hidden", syncstoretest.ReadObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
	assert.Nil(t, rt.resources.dirtyBuf.FlushImmediate())
}

// Validates: R-2.4.2
func TestRemoteChangeHasPlannerVisibleEffect_TransitionMatrix(t *testing.T) {
	t.Parallel()

	filter := syncdomain.ContentFilterConfig{IgnoredDirs: []string{"hidden"}}
	tests := []struct {
		name string
		path string
		old  string
		want bool
	}{
		{
			name: "hidden to hidden does not wake",
			path: "hidden/new.txt",
			old:  "hidden/old.txt",
			want: false,
		},
		{
			name: "visible to hidden wakes",
			path: "hidden/new.txt",
			old:  "visible/old.txt",
			want: true,
		},
		{
			name: "hidden to visible wakes",
			path: "visible/new.txt",
			old:  "hidden/old.txt",
			want: true,
		},
		{
			name: "visible to visible wakes",
			path: "visible/new.txt",
			old:  "visible/old.txt",
			want: true,
		},
		{
			name: "hidden create does not wake",
			path: "hidden/create.txt",
			want: false,
		},
		{
			name: "visible delete wakes",
			path: "visible/delete.txt",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			change := syncobserve.RemoteChange{Path: tt.path, OldPath: tt.old, ItemType: syncdomain.ItemTypeFile}
			assert.Equal(t, tt.want, remoteChangeHasPlannerVisibleEffect(filter, &change))
		})
	}
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_MountRootWatchCommitsObservedRowsAndPendingCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	pendingCursor := "cursor-shared"
	batch := testMountRootWatchBatch(
		eng.Engine,
		syncdomain.RemoteObservationModeDelta,
		[]syncdomain.RemoteItem{
			testRemoteCreateEvent("shared-watch.txt", "item-shared", eng.driveID.String()),
		},
		pendingCursor,
		syncdomain.NewRemoteObservationFindingsBatch(),
	)
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "shared-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-shared", remoteRow.ItemID)
	assert.Equal(t, pendingCursor, syncstoretest.ReadObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.8.3
func TestHandleRemoteObservationBatch_MountRootEnumerateClampRearmsRefreshTimerImmediately(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	clock := syncfixture.NewManualClock(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)

	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, eng.driveID, "cursor-shared"))
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(
		ctx,
		eng.driveID,
		clock.Now(),
		syncdomain.RemoteObservationModeDelta,
	))
	require.NoError(t, rt.armFullRefreshTimer(ctx))

	initialDueAt := clock.Now().Add(syncdomain.FullRemoteRefreshInterval)
	enumerateDueAt := clock.Now().Add(syncstore.RemoteRefreshEnumerateInterval)
	assert.True(t, clock.HasPendingTimerAt(initialDueAt))
	assert.False(t, clock.HasPendingTimerAt(enumerateDueAt))

	batch := testMountRootWatchBatch(
		eng.Engine,
		syncdomain.RemoteObservationModeEnumerate,
		nil,
		"",
		syncdomain.NewRemoteObservationFindingsBatch(),
	)
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	state, err := eng.baseline.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, enumerateDueAt.UnixNano(), state.NextFullRemoteRefreshAt)
	assert.False(t, clock.HasPendingTimerAt(initialDueAt))
	assert.True(t, clock.HasPendingTimerAt(enumerateDueAt))
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_MountRootReconcilesRemoteReadDeniedFindings(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	batch := testMountRootWatchBatch(
		eng.Engine,
		syncdomain.RemoteObservationModeEnumerate,
		nil,
		"",
		rootReadFailureObservationFindingsBatch(eng.driveID, syncdomain.IssueRemoteReadDenied),
	)
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "/", issues[0].Path)
	assert.Equal(t, syncdomain.IssueRemoteReadDenied, issues[0].IssueType)
	assert.Equal(t, syncdomain.SKPermRemoteRead(""), issues[0].ScopeKey)

	scopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, scopes, "mount-root remote read-denied findings should not create block scope rows")
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_PrimaryWatchClearsRemoteReadDeniedFindingsOnHealthyPoll(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	findings := rootReadFailureObservationFindingsBatch(eng.driveID, syncdomain.IssueRemoteReadDenied)
	require.NoError(t, eng.baseline.ReconcileObservationFindings(ctx, &findings))

	batch := buildPrimaryWatchBatch(eng.Engine, nil, []syncdomain.RemoteItem{
		testRemoteCreateEvent("primary-watch.txt", "item-primary", eng.driveID.String()),
	}, "cursor-primary")
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)

	scopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, scopes)
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_PrimaryWatchCanceledContextReturnsCommitError(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	batch := buildPrimaryWatchBatch(eng.Engine, nil, []syncdomain.RemoteItem{
		testRemoteCreateEvent("primary-canceled.txt", "item-canceled", eng.driveID.String()),
	}, "cursor-canceled")
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// Validates: R-2.1.2, R-2.10.4
func TestHandleRemoteObservationBatch_PrimaryWatchReconcileFailureIsFatal(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	batch := buildPrimaryWatchBatch(eng.Engine, nil, nil, "")
	batch.Findings = rootReadFailureObservationFindingsBatch(eng.driveID, syncdomain.IssueRemoteReadDenied)

	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.Error(t, err)
	assert.True(t, isFatalObserverError(err))
}

// Validates: R-2.1.2, R-2.10.4
func TestCommitLocalScan_ReturnsErrorOnFailure(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	_, err := rt.commitLocalScan(ctx, nil, []syncdomain.SkippedItem{{
		Path:   "blocked.txt",
		Reason: syncdomain.IssueInvalidFilename,
		Detail: "invalid",
	}}, nil)
	require.Error(t, err)
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_FullRefreshApplyFailureMarksDirtyForRetry(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	batch := buildRemoteObservationBatch(
		eng.Engine,
		syncdomain.RemoteObservationModeDelta,
		nil,
		"",
		true,
		rootReadFailureObservationFindingsBatch(eng.driveID, syncdomain.IssueRemoteReadDenied),
	)
	batch.Source = syncobserve.RemoteObservationBatchFullRefresh
	batch.ArmFullRefreshTimer = true
	batch.MarkFullRefreshIfIdle = true
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	dirty := rt.resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, dirty)
	assert.True(t, dirty.FullRefresh)
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_DoesNotReloadActiveScopesAfterObservationReconcile(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "blocked.txt",
		DriveID:  eng.driveID,
		ItemID:   "blocked-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	serviceScope := &syncdomain.ActiveScope{
		Key:           syncdomain.SKService(),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}
	rt.scopes.upsertActiveScope(serviceScope)

	batch := buildPrimaryWatchBatch(eng.Engine, nil, nil, "")
	batch.Findings = rootReadFailureObservationFindingsBatch(eng.driveID, syncdomain.IssueRemoteReadDenied)
	require.NoError(t, rt.handleRemoteObservationBatch(ctx, &batch))

	activeScopes := rt.scopes.snapshotActiveScopes()
	require.Len(t, activeScopes, 1)
	assert.Equal(t, syncdomain.SKService(), activeScopes[0].Key)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	runtime, err := rt.runSteadyStateCurrentPlan(ctx, bl, syncdomain.SyncBidirectional)
	require.NoError(t, err)
	assert.Empty(t, runtime.Plan.Actions, "read-denied observation findings should suppress planning without reloading active scopes")
}

// Validates: R-2.17.2, R-2.15.3
//
// A watch-mode delta that deletes a synced folder makes the full refresh due
// within FullRefreshAfterFolderDeleteDelay, since delta never reports an item moved into the folder in the
// same window. The tombstone echo of a folder this client deleted (no longer
// in the baseline) does not.
func TestHandleRemoteObservationBatch_SyncedFolderTombstonePullsFullRefreshForward(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		inBase   bool
		wantPull bool
	}{
		{name: "synced folder", inBase: true, wantPull: true},
		{name: "own delete echo", inBase: false, wantPull: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
			setupWatchEngine(t, eng)
			rt := testWatchRuntime(t, eng)
			ctx := t.Context()
			now := eng.nowFunc()
			require.NoError(t, eng.baseline.MarkFullRemoteRefresh(ctx, eng.driveID, now, syncdomain.RemoteObservationModeDelta))

			bl := syncdomain.NewBaselineForTest(nil)
			if tc.inBase {
				bl.Put(&syncdomain.BaselineEntry{Path: "bin", DriveID: eng.driveID, ItemID: "item-bin", ParentID: "root", ItemType: syncdomain.ItemTypeFolder})
			}
			batch := buildPrimaryWatchBatch(eng.Engine, bl, []syncdomain.RemoteItem{{ObservedItem: syncdomain.ObservedItem{
				Path:      "bin",
				ItemID:    "item-bin",
				ParentID:  "root",
				DriveID:   eng.driveID,
				ItemType:  syncdomain.ItemTypeFile,
				IsDeleted: true,
			}}}, "cursor-after-delete")
			require.NoError(t, rt.handleRemoteObservationBatch(ctx, &batch))

			state, err := eng.baseline.ReadObservationState(ctx)
			require.NoError(t, err)
			if tc.wantPull {
				assert.Less(t, state.NextFullRemoteRefreshAt, now.Add(time.Hour).UnixNano(), "the full refresh is due soon, not in 24 h")
				assert.GreaterOrEqual(t, state.NextFullRemoteRefreshAt, now.Add(syncdomain.FullRefreshAfterFolderDeleteDelay).UnixNano(),
					"but deferred, so a burst of folder deletions shares one enumeration")
				return
			}
			assert.Equal(t, now.Add(syncdomain.FullRemoteRefreshInterval).UnixNano(), state.NextFullRemoteRefreshAt)
		})
	}
}
