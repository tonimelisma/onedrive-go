package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

func testRootedSubtreeWatchBatch(
	engine *Engine,
	mode remoteObservationMode,
	events []ChangeEvent,
	cursorToken string,
	findings ObservationFindingsBatch,
) remoteObservationBatch {
	batch := buildRemoteObservationBatch(
		engine,
		mode,
		events,
		cursorToken,
		false,
		findings,
	)
	batch.source = remoteObservationBatchRootedSubtree
	batch.applyAck = make(chan error, 1)

	return batch
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
func TestHandleRemoteObservationBatch_RootedSubtreeWatchCommitsObservedRowsAndPendingCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	pendingCursor := "cursor-shared"
	batch := testRootedSubtreeWatchBatch(
		eng.Engine,
		remoteObservationModeDelta,
		[]ChangeEvent{
			testRemoteCreateEvent("shared-watch.txt", "item-shared", eng.driveID.String()),
		},
		pendingCursor,
		newRemoteObservationFindingsBatch(),
	)
	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.NoError(t, err)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "shared-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-shared", remoteRow.ItemID)
	assert.Equal(t, pendingCursor, readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.8.3
func TestHandleRemoteObservationBatch_RootedSubtreeEnumerateClampRearmsRefreshTimerImmediately(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	clock := newManualClock(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC))
	installManualClock(eng.Engine, clock)

	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, eng.driveID, "cursor-shared"))
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(
		ctx,
		eng.driveID,
		clock.Now(),
		remoteObservationModeDelta,
	))
	require.NoError(t, rt.armFullRefreshTimer(ctx))

	initialDueAt := clock.Now().Add(fullRemoteRefreshInterval)
	enumerateDueAt := clock.Now().Add(remoteRefreshEnumerateInterval)
	assert.True(t, clock.HasPendingTimerAt(initialDueAt))
	assert.False(t, clock.HasPendingTimerAt(enumerateDueAt))

	batch := testRootedSubtreeWatchBatch(
		eng.Engine,
		remoteObservationModeEnumerate,
		nil,
		"",
		newRemoteObservationFindingsBatch(),
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
func TestHandleRemoteObservationBatch_RootedSubtreeReconcilesRemoteReadDeniedFindings(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	batch := testRootedSubtreeWatchBatch(
		eng.Engine,
		remoteObservationModeEnumerate,
		nil,
		"",
		rootRemoteReadDeniedObservationFindingsBatch(eng.driveID),
	)
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
	assert.Empty(t, scopes, "rooted-subtree remote read-denied findings should not create block scope rows")
}

// Validates: R-2.10.4
func TestHandleRemoteObservationBatch_PrimaryWatchClearsRemoteReadDeniedFindingsOnHealthyPoll(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	findings := rootRemoteReadDeniedObservationFindingsBatch(eng.driveID)
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
	batch.findings = rootRemoteReadDeniedObservationFindingsBatch(eng.driveID)

	err := rt.handleRemoteObservationBatch(ctx, &batch)
	require.Error(t, err)
	assert.True(t, isFatalObserverError(err))
}

// Validates: R-2.1.2, R-2.10.4
func TestReconcileSkippedObservationFindings_ReturnsErrorOnFailure(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	require.NoError(t, eng.baseline.Close(ctx))

	err := rt.reconcileSkippedObservationFindings(ctx, []SkippedItem{{
		Path:   "blocked.txt",
		Reason: IssueInvalidFilename,
		Detail: "invalid",
	}})
	require.Error(t, err)
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

	batch := buildRemoteObservationBatch(
		eng.Engine,
		remoteObservationModeDelta,
		nil,
		"",
		true,
		rootRemoteReadDeniedObservationFindingsBatch(eng.driveID),
	)
	batch.source = remoteObservationBatchFullRefresh
	batch.armFullRefreshTimer = true
	batch.markFullRefreshIfIdle = true
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
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}
	rt.upsertActiveScope(serviceScope)

	batch := buildPrimaryWatchBatch(eng.Engine, nil, "")
	batch.findings = rootRemoteReadDeniedObservationFindingsBatch(eng.driveID)
	require.NoError(t, rt.handleRemoteObservationBatch(ctx, &batch))

	activeScopes := rt.snapshotActiveScopes()
	require.Len(t, activeScopes, 1)
	assert.Equal(t, SKService(), activeScopes[0].Key)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	runtime, err := rt.runSteadyStateCurrentPlan(ctx, bl, SyncBidirectional)
	require.NoError(t, err)
	assert.Empty(t, runtime.Plan.Actions, "read-denied observation findings should suppress planning without reloading active scopes")
}
