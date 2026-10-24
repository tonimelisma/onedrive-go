package sync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// Validates: R-6.6.18
func TestEmitActivity_ClassifiesOutcomesForDisplay(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	failure := errors.New("boom")
	eng.emitActivity(&actionCompletion{Path: "a.txt", ActionType: ActionDownload, Size: 5, SizeKnown: true}, errclass.ClassSuccess)
	eng.emitActivity(&actionCompletion{Path: "b.txt", ActionType: ActionUpload, Err: failure}, errclass.ClassRetryableTransient)
	eng.emitActivity(&actionCompletion{Path: "c.txt", ActionType: ActionRemoteDelete, Err: failure}, errclass.ClassActionable)
	eng.emitActivity(&actionCompletion{Path: "d.txt", ActionType: ActionUpload}, errclass.ClassShutdown)
	eng.emitActivity(&actionCompletion{Path: "e.txt", ActionType: ActionUpload}, errclass.ClassSuperseded)
	eng.emitActivity(&actionCompletion{Path: "f.txt", ActionType: ActionBaselineUpdate}, errclass.ClassSuccess)

	require.Len(t, got, 3, "shutdown, superseded and bookkeeping completions are not user activity")

	assert.Equal(t, ActivityDownload, got[0].Kind)
	assert.True(t, got[0].Success)
	assert.True(t, got[0].SizeKnown)
	assert.Equal(t, int64(5), got[0].Size)

	assert.Equal(t, ActivityUpload, got[1].Kind)
	assert.False(t, got[1].Success)
	assert.True(t, got[1].WillRetry)
	require.ErrorIs(t, got[1].Err, failure)

	assert.Equal(t, ActivityRemoteDelete, got[2].Kind)
	assert.False(t, got[2].WillRetry)
	assert.ErrorIs(t, got[2].Err, failure)
}

// Validates: R-6.6.18
func TestRunOnce_ActivityHookNamesEachCompletedFile(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := newRunOnceBidirectionalMock(driveID)
	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local.txt", "local-content")

	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	byPath := map[string]Activity{}
	for _, a := range got {
		byPath[a.Path] = a
	}
	require.Contains(t, byPath, "remote.txt")
	require.Contains(t, byPath, "local.txt")
	assert.Equal(t, ActivityDownload, byPath["remote.txt"].Kind)
	assert.True(t, byPath["remote.txt"].Success)
	assert.Equal(t, ActivityUpload, byPath["local.txt"].Kind)
	assert.True(t, byPath["local.txt"].Success)
	assert.True(t, byPath["local.txt"].SizeKnown)
	assert.Equal(t, int64(len("local-content")), byPath["local.txt"].Size)
}

// Validates: R-6.6.18
func TestRunOnce_DryRunActivityListsPlannedActions(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := newRunOnceBidirectionalMock(driveID)
	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local.txt", "local-content")

	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	for _, a := range got {
		assert.True(t, a.Planned, "dry-run activity must be marked planned: %+v", a)
	}
}
