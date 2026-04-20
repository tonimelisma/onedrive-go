package sync

import (
	"context"
	"fmt"
	"testing"

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
func TestProcessCommittedPrimaryWatchBatch_CommitsObservedRowsAndCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	finalEvents, err := rt.processCommittedPrimaryWatchBatch(ctx, bl, []ChangeEvent{
		testRemoteCreateEvent("primary-watch.txt", "item-primary", eng.driveID.String()),
	}, "cursor-primary")
	require.NoError(t, err)
	require.Len(t, finalEvents, 1)
	assert.Equal(t, "primary-watch.txt", finalEvents[0].Path)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "primary-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-primary", remoteRow.ItemID)
	assert.Equal(t, "cursor-primary", readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.1.2
func TestProcessCommittedSharedRootWatchBatch_CommitsObservedRowsAndPendingCursor(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	pendingCursor := "cursor-shared"

	finalEvents, committed := rt.processCommittedSharedRootWatchBatch(ctx, bl, &remoteFetchResult{
		events: []ChangeEvent{
			testRemoteCreateEvent("shared-watch.txt", "item-shared", eng.driveID.String()),
		},
		pending: &pendingPrimaryCursorCommit{
			driveID: eng.driveID.String(),
			rootID:  "shared-root",
			token:   pendingCursor,
		},
	})
	require.True(t, committed)
	require.Len(t, finalEvents, 1)
	assert.Equal(t, "shared-watch.txt", finalEvents[0].Path)

	remoteRow, found, err := eng.baseline.GetRemoteStateByPath(ctx, "shared-watch.txt", eng.driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "item-shared", remoteRow.ItemID)
	assert.Equal(t, pendingCursor, readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.1.2
func TestProcessCommittedSharedRootWatchBatch_CursorCommitFailureReturnsNotCommitted(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	saveObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String(), "existing-cursor")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	finalEvents, committed := rt.processCommittedSharedRootWatchBatch(ctx, bl, &remoteFetchResult{
		pending: &pendingPrimaryCursorCommit{
			driveID: mustParseDriveID("2").String(),
			rootID:  "shared-root",
			token:   "mismatched-cursor",
		},
	})
	assert.False(t, committed)
	assert.Nil(t, finalEvents)
	assert.Equal(t, "existing-cursor", readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String()))
}

// Validates: R-2.10.4
func TestProcessCommittedSharedRootWatchBatch_ReconcilesRemoteReadDeniedFindings(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	finalEvents, committed := rt.processCommittedSharedRootWatchBatch(ctx, bl, &remoteFetchResult{
		findings: rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden),
	})
	require.True(t, committed)
	assert.Empty(t, finalEvents)

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
func TestProcessCommittedPrimaryWatchBatch_ClearsRemoteReadDeniedFindingsOnHealthyPoll(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	batch := rootRemoteReadDeniedObservationFindingsBatch(eng.driveID, graph.ErrForbidden)
	require.NoError(t, eng.baseline.ReconcileObservationFindings(ctx, &batch, eng.nowFunc()))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	finalEvents, err := rt.processCommittedPrimaryWatchBatch(ctx, bl, []ChangeEvent{
		testRemoteCreateEvent("primary-watch.txt", "item-primary", eng.driveID.String()),
	}, "cursor-primary")
	require.NoError(t, err)
	require.Len(t, finalEvents, 1)

	issues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)

	scopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, scopes)
}

// Validates: R-2.1.2
func TestProcessCommittedPrimaryWatchBatch_CanceledContextReturnsCommitError(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	finalEvents, err := rt.processCommittedPrimaryWatchBatch(ctx, bl, []ChangeEvent{
		testRemoteCreateEvent("primary-canceled.txt", "item-canceled", eng.driveID.String()),
	}, "cursor-canceled")
	require.Error(t, err)
	assert.Nil(t, finalEvents)
	assert.ErrorIs(t, err, context.Canceled)
}

// Validates: R-2.1.2
func TestLogCommittedSharedRootBatchFailure_DoesNotPanic(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	assert.NotPanics(t, func() {
		rt.logCommittedSharedRootBatchFailure("commit observations", assert.AnError, 2)
		rt.logCommittedSharedRootBatchFailure("commit observations", assert.AnError, 0)
	})
}
