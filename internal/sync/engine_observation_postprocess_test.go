package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func testRemoteCreateEvent(path string, itemID string, driveID string) ChangeEvent {
	return ChangeEvent{
		Source:   SourceRemote,
		Type:     ChangeCreate,
		Path:     path,
		ItemID:   itemID,
		ParentID: "root",
		DriveID:  mustParseDriveID(driveID),
		ItemType: ItemTypeFile,
		Hash:     fmt.Sprintf("hash-%s", itemID),
		Size:     12,
		Mtime:    123,
		ETag:     fmt.Sprintf("etag-%s", itemID),
	}
}

func mustParseDriveID(raw string) driveid.ID {
	id := driveid.New(raw)
	if id.IsZero() {
		panic("test drive ID must be non-zero")
	}
	return id
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_PrimaryWatchCommitsObservedRowsAndCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	batch := buildPrimaryWatchBatch(eng.Engine, []ChangeEvent{
		testRemoteCreateEvent("primary-watch.txt", "item-primary", eng.driveID.String()),
	}, "cursor-primary")
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "primary-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-primary", remoteRow.ItemID)
	assert.Equal(t, "cursor-primary", readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_SharedRootWatchCommitsObservedRowsAndPendingCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	pendingCursor := "cursor-shared"
	batch := buildSharedRootWatchBatch(eng.Engine, &remoteFetchResult{
		events: []ChangeEvent{
			testRemoteCreateEvent("shared-watch.txt", "item-shared", eng.driveID.String()),
		},
		pending: &pendingPrimaryCursorCommit{
			driveID: eng.driveID.String(),
			rootID:  "shared-root",
			token:   pendingCursor,
		},
	})
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "shared-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-shared", remoteRow.ItemID)
	assert.Equal(t, pendingCursor, readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_SharedRootCursorCommitFailureLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	saveObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String(), "existing-cursor")

	batch := buildSharedRootWatchBatch(eng.Engine, &remoteFetchResult{
		pending: &pendingPrimaryCursorCommit{
			driveID: mustParseDriveID("2").String(),
			rootID:  "shared-root",
			token:   "mismatched-cursor",
		},
	})
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.Error(t, err)
	assert.True(t, isFatalObserverError(err))
	assert.Equal(t, "existing-cursor", readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_SharedRootReconcilesRemoteReadDeniedFindings(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	batch := buildSharedRootWatchBatch(eng.Engine, &remoteFetchResult{
		findings: rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden),
	})
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "/", issues[0].Path)
	assert.Equal(t, IssueRemoteReadDenied, issues[0].IssueType)
	assert.Equal(t, SKPermRemoteRead(""), issues[0].ScopeKey)

	scopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, scopes, "shared-root remote read-denied findings should not create block scope rows")
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_PrimaryWatchClearsRemoteReadDeniedFindingsOnHealthyPoll(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	findings := rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden)
	require.NoError(t, eng.baseline.ReconcileObservationFindings(ctx, &findings, eng.nowFunc()))

	batch := buildPrimaryWatchBatch(eng.Engine, []ChangeEvent{
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

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	batch := buildPrimaryWatchBatch(eng.Engine, []ChangeEvent{
		testRemoteCreateEvent("primary-canceled.txt", "item-canceled", eng.driveID.String()),
	}, "cursor-canceled")
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// Validates: R-2.1.2, R-2.10.4
func TestHandleRemoteObservationBatch_PrimaryWatchReconcileFailureIsFatal(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	batch := buildPrimaryWatchBatch(eng.Engine, nil, "")
	batch.findings = rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden)

	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.Error(t, err)
	assert.True(t, isFatalObserverError(err))
}

// Validates: R-2.1.2, R-2.10.4
func TestHandleWatchSkippedChannel_ReconcileFailureReturnsError(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	done, err := rt.handleWatchSkippedChannel(ctx, &watchPipeline{}, []SkippedItem{{
		Path:   "blocked.txt",
		Reason: IssueInvalidFilename,
		Detail: "invalid",
	}}, true)
	require.Error(t, err)
	assert.False(t, done)
}

// Validates: R-2.1.2
func TestHandleRemoteObservationBatch_FullRefreshApplyFailureMarksDirtyForRetry(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	batch := buildFullRemoteRefreshBatch(eng.Engine, remoteFetchResult{
		findings: rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden),
	})
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	dirty := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, dirty)
	assert.True(t, dirty.FullRefresh)
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_DoesNotReloadActiveScopesAfterObservationReconcile(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "blocked.txt",
		DriveID:  eng.driveID,
		ItemID:   "blocked-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	serviceScope := &ActiveScope{
		Key:           SKService(),
		BlockedAt:     eng.nowFunc(),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}
	rt.upsertActiveScope(serviceScope)

	batch := buildPrimaryWatchBatch(eng.Engine, nil, "")
	batch.findings = rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden)
	require.NoError(t, rt.handleRemoteObservationBatch(ctx, &batch))

	activeScopes := rt.snapshotActiveScopes()
	require.Len(t, activeScopes, 1)
	assert.Equal(t, SKService(), activeScopes[0].Key)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	prepared, err := rt.prepareSteadyStateCurrentPlan(ctx, bl, SyncBidirectional)
	require.NoError(t, err)
	assert.Empty(t, prepared.Plan.Actions, "read-denied observation findings should suppress planning without reloading active scopes")
}
