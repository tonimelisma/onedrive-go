package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.33
func TestRetryFailureRow_MapsRetryStateToHeldFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	assert.Nil(t, retryFailureRow(nil, driveID))

	row := retryFailureRow(&RetryStateRow{
		Path:       "held.txt",
		OldPath:    "old-held.txt",
		ActionType: ActionRemoteDelete,
		ScopeKey:   SKService(),
	}, driveID)
	require.NotNil(t, row)
	assert.Equal(t, "held.txt", row.Path)
	assert.Equal(t, driveID, row.DriveID)
	assert.Equal(t, DirectionDelete, row.Direction)
	assert.Equal(t, ActionRemoteDelete, row.ActionType)
	assert.Equal(t, FailureRoleHeld, row.Role)
	assert.Equal(t, CategoryTransient, row.Category)
	assert.Equal(t, IssueServiceOutage, row.IssueType)
	assert.Equal(t, SKService(), row.ScopeKey)
}

// Validates: R-2.10.33
func TestRetryFailureRowForStore_UsesEngineDriveFallback(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	row, err := flow.retryFailureRowForStore(t.Context(), &RetryStateRow{
		Path:       "held.txt",
		ActionType: ActionUpload,
		ScopeKey:   SKPermRemote("shared"),
	})
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, eng.driveID, row.DriveID)
	assert.Equal(t, DirectionUpload, row.Direction)
	assert.Equal(t, IssueSharedFolderBlocked, row.IssueType)
}

// Validates: R-2.10.33
func TestGetSyncFailureByPath_ReturnsHeldFailureRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "held.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueSharedFolderBlocked,
		ErrMsg:     "held for retry",
		ActionType: ActionUpload,
		ItemID:     "item-1",
		ScopeKey:   SKPermRemote("shared"),
	}, nil))

	row, found, err := store.GetSyncFailureByPath(ctx, "held.txt", driveID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, "held.txt", row.Path)
	assert.Equal(t, driveID, row.DriveID)
	assert.Equal(t, ActionUpload, row.ActionType)
	assert.Equal(t, FailureRoleHeld, row.Role)
	assert.Equal(t, CategoryTransient, row.Category)
	assert.Equal(t, "item-1", row.ItemID)
	assert.Equal(t, SKPermRemote("shared"), row.ScopeKey)
}

// Validates: R-2.10.33
func TestGetSyncFailureByPath_ReturnsNotFoundForMissingPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)

	row, found, err := store.GetSyncFailureByPath(t.Context(), "missing.txt", driveID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, row)
}

// Validates: R-2.10.33
func TestClearStaleRetrySweepRow_ResolvedRetryDeletesRetryAndFailure(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	row := &RetryStateRow{
		Path:         "stale-download.txt",
		ActionType:   ActionDownload,
		ScopeKey:     SKService(),
		AttemptCount: 1,
		FirstSeenAt:  10,
		LastSeenAt:   20,
	}
	work := RetryWorkKey{
		Path:       row.Path,
		ActionType: row.ActionType,
	}

	require.NoError(t, eng.baseline.UpsertRetryState(ctx, row))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       row.Path,
		DriveID:    eng.driveID,
		Direction:  DirectionDownload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "held for retry",
		ActionType: row.ActionType,
		ScopeKey:   row.ScopeKey,
	}, nil))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.clearStaleRetrySweepRow(ctx, bl, row, work)

	retryRows, err := eng.baseline.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, retryRows)

	failure, found, err := eng.baseline.GetSyncFailureByPath(ctx, row.Path, eng.driveID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, failure)
}

// Validates: R-2.10.33
func TestClearStaleRetrySweepRow_SkippedRetryPreservesActionableFailure(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.localRules = LocalObservationRules{RejectSharePointRootForms: true}
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "forms"), 0o750))

	row := &RetryStateRow{
		Path:         "forms",
		ActionType:   ActionUpload,
		ScopeKey:     SKPermDir("forms"),
		AttemptCount: 1,
		FirstSeenAt:  30,
		LastSeenAt:   40,
	}
	work := RetryWorkKey{
		Path:       row.Path,
		ActionType: row.ActionType,
	}

	require.NoError(t, eng.baseline.UpsertRetryState(ctx, row))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       row.Path,
		DriveID:    eng.driveID,
		Direction:  DirectionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueLocalPermissionDenied,
		ErrMsg:     "held for retry",
		ActionType: row.ActionType,
		ScopeKey:   row.ScopeKey,
	}, nil))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.clearStaleRetrySweepRow(ctx, bl, row, work)

	retryRows, err := eng.baseline.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, retryRows)

	failures, err := eng.baseline.ListActionableFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "forms", failures[0].Path)
	assert.Equal(t, IssueInvalidFilename, failures[0].IssueType)
	assert.Equal(t, CategoryActionable, failures[0].Category)
}
