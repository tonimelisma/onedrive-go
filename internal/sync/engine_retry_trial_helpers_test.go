package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.33
func TestRetryWorkKeyHelpers_PreserveExactIdentity(t *testing.T) {
	t.Parallel()

	assert.Equal(t, RetryWorkKey{}, retryWorkKeyForAction(nil))
	assert.Equal(t, RetryWorkKey{}, retryWorkKeyForCompletion(nil))
	assert.Equal(t, RetryWorkKey{}, retryWorkKeyForRetryState(nil))

	action := &Action{Path: "held.txt", OldPath: "old-held.txt", Type: ActionRemoteDelete}
	assert.Equal(t, retryWorkKey("held.txt", "old-held.txt", ActionRemoteDelete), retryWorkKeyForAction(action))

	result := &ActionCompletion{Path: "held.txt", OldPath: "old-held.txt", ActionType: ActionRemoteDelete}
	assert.Equal(t, retryWorkKey("held.txt", "old-held.txt", ActionRemoteDelete), retryWorkKeyForCompletion(result))

	row := &RetryStateRow{Path: "held.txt", OldPath: "old-held.txt", ActionType: ActionRemoteDelete}
	assert.Equal(t, retryWorkKey("held.txt", "old-held.txt", ActionRemoteDelete), retryWorkKeyForRetryState(row))
}

// Validates: R-2.10.33
func TestRetryStateDriveID_UsesEngineDriveFallback(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	driveID, err := flow.retryStateDriveID(t.Context())
	require.NoError(t, err)
	assert.Equal(t, eng.driveID, driveID)
}

// Validates: R-2.10.33
func TestRetryStateDriveID_PrefersPersistedConfiguredDrive(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	configuredDriveID := driveid.New("persisted-drive")

	require.NoError(t, eng.baseline.CommitObservation(ctx, nil, "", configuredDriveID))

	driveID, err := flow.retryStateDriveID(ctx)
	require.NoError(t, err)
	assert.Equal(t, configuredDriveID, driveID)
}

// Validates: R-2.10.33
func TestSyncFailureByPathForTest_ReturnsHeldFailureRow(t *testing.T) {
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

	row, found := syncFailureByPathForTest(t, store, ctx, "held.txt")
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
func TestSyncFailureByPathForTest_ReturnsNotFoundForMissingPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	row, found := syncFailureByPathForTest(t, store, t.Context(), "missing.txt")
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
		Role:       FailureRoleItem,
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

	failure, found := syncFailureByPathForTest(t, eng.baseline, ctx, row.Path)
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

	failures := actionableSyncFailuresForTest(t, eng.baseline, ctx)
	require.Len(t, failures, 1)
	assert.Equal(t, "forms", failures[0].Path)
	assert.Equal(t, IssueInvalidFilename, failures[0].IssueType)
	assert.Equal(t, CategoryActionable, failures[0].Category)
}

// Validates: R-2.10.5
func TestClearStaleTrialRetryWork_PreservesScopeWhenBlockedRetriesRemain(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	scopeKey := SKService()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           scopeKey,
		IssueType:     IssueServiceOutage,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "stale.txt",
		DriveID:    eng.driveID,
		Direction:  DirectionDownload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "held stale retry",
		ActionType: ActionDownload,
		ScopeKey:   scopeKey,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "still-blocked.txt",
		DriveID:    eng.driveID,
		Direction:  DirectionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "held remaining retry",
		ActionType: ActionUpload,
		ScopeKey:   scopeKey,
	}, nil))

	rt.clearStaleTrialRetryWork(ctx, scopeKey, &RetryStateRow{
		Path:       "stale.txt",
		ActionType: ActionDownload,
		ScopeKey:   scopeKey,
	})

	retryRows, err := eng.baseline.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 1)
	assert.Equal(t, "still-blocked.txt", retryRows[0].Path)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "still-blocked.txt", failures[0].Path)

	block, ok := getTestScopeBlock(eng, scopeKey)
	require.True(t, ok)
	assert.Equal(t, 10*time.Second, block.TrialInterval)
	assert.WithinDuration(t, eng.nowFn().Add(10*time.Second), block.NextTrialAt, 2*time.Second)
}

// Validates: R-2.10.5
func TestClearStaleTrialRetryWork_DiscardsScopeWhenBlockedRetriesDisappear(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	scopeKey := SKService()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           scopeKey,
		IssueType:     IssueServiceOutage,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "stale.txt",
		DriveID:    eng.driveID,
		Direction:  DirectionDownload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "held stale retry",
		ActionType: ActionDownload,
		ScopeKey:   scopeKey,
	}, nil))

	rt.clearStaleTrialRetryWork(ctx, scopeKey, &RetryStateRow{
		Path:       "stale.txt",
		ActionType: ActionDownload,
		ScopeKey:   scopeKey,
	})

	retryRows, err := eng.baseline.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, retryRows)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
	assert.False(t, isTestScopeBlocked(eng, scopeKey))
}
