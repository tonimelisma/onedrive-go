package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func assertPreparedCurrentPlanEqual(t *testing.T, expected *PreparedCurrentPlan, actual *PreparedCurrentPlan) {
	t.Helper()

	require.NotNil(t, expected)
	require.NotNil(t, actual)
	assert.Equal(t, expected.PendingCursorCommit, actual.PendingCursorCommit)
	assert.Equal(t, expected.Report, actual.Report)
	assert.Equal(t, expected.Plan, actual.Plan)
	assert.Equal(t, expected.RetryRows, actual.RetryRows)
	assert.Equal(t, expected.BlockScopes, actual.BlockScopes)
}

// Validates: R-2.10.5
func TestPrepareBootstrapCurrentPlan_MatchesOneShotLivePrepare(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         10,
					QuickXorHash: "hash-1",
				},
			}, "token-prepare"), nil
		},
	}

	oneShotEng, _ := newTestEngine(t, mock)
	watchEng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	oneShotBaseline, err := oneShotEng.baseline.Load(ctx)
	require.NoError(t, err)
	fullReconcile, err := oneShotEng.shouldRunFullRemoteRefresh(ctx, false)
	require.NoError(t, err)
	oneShotPrepared, err := newOneShotRunner(oneShotEng.Engine).prepareLiveCurrentPlan(ctx, oneShotBaseline, SyncDownloadOnly, RunOptions{
		FullReconcile: fullReconcile,
	})
	require.NoError(t, err)

	setupWatchEngine(t, watchEng)
	watchBaseline, err := watchEng.baseline.Load(ctx)
	require.NoError(t, err)
	watchPrepared, err := testWatchRuntime(t, watchEng).prepareBootstrapCurrentPlan(ctx, watchBaseline, SyncDownloadOnly)
	require.NoError(t, err)

	assertPreparedCurrentPlanEqual(t, oneShotPrepared, watchPrepared)
}

// Validates: R-2.10.5
func TestProcessDirtyBatch_PrunesStaleDurableRuntimeStateLikeBootstrapPrepare(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-bootstrap"), nil
		},
	}

	bootstrapEng, _ := newTestEngine(t, mock)
	dirtyEng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	seedStaleRetryAndBlockScope := func(t *testing.T, eng *testEngine) {
		t.Helper()

		_, err := eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
			Path:          "stale.txt",
			ActionType:    ActionUpload,
			ConditionType: IssueServiceOutage,
			LastError:     "stale retry row",
		}, func(int) time.Duration { return time.Minute })
		require.NoError(t, err)
		require.NoError(t, eng.baseline.UpsertBlockScope(ctx, &BlockScope{
			Key:           SKService(),
			BlockedAt:     eng.nowFunc(),
			TrialInterval: time.Minute,
			NextTrialAt:   eng.nowFunc().Add(time.Minute),
		}))
	}

	seedStaleRetryAndBlockScope(t, bootstrapEng)
	seedStaleRetryAndBlockScope(t, dirtyEng)

	setupWatchEngine(t, bootstrapEng)
	bootstrapBaseline, err := bootstrapEng.baseline.Load(ctx)
	require.NoError(t, err)
	bootstrapPrepared, err := testWatchRuntime(t, bootstrapEng).prepareBootstrapCurrentPlan(ctx, bootstrapBaseline, SyncBidirectional)
	require.NoError(t, err)
	assert.Empty(t, bootstrapPrepared.RetryRows)
	assert.Empty(t, bootstrapPrepared.BlockScopes)

	setupWatchEngine(t, dirtyEng)
	dirtyBaseline, err := dirtyEng.baseline.Load(ctx)
	require.NoError(t, err)
	dispatch := testWatchRuntime(t, dirtyEng).processDirtyBatch(ctx, DirtyBatch{
		Paths: []string{"stale.txt"},
	}, dirtyBaseline, SyncBidirectional)
	assert.Nil(t, dispatch)

	dirtyRetryRows, err := dirtyEng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, dirtyRetryRows)

	dirtyBlockScopes, err := dirtyEng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, dirtyBlockScopes)
}
