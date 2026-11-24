package sync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.6.18
func TestEmitActivity_ClassifiesOutcomesForDisplay(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	failure := errors.New("boom")
	emit := func(r *syncdomain.ActionCompletion, class errclass.Class) {
		eng.emitActivity(r, &syncdomain.ResultDecision{Class: class}, true)
	}
	emit(&syncdomain.ActionCompletion{Path: "a.txt", ActionType: syncdomain.ActionDownload, Size: 5, SizeKnown: true}, errclass.ClassSuccess)
	emit(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Path: "b.txt", ActionType: syncdomain.ActionUpload, Err: failure}), errclass.ClassRetryableTransient)
	emit(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Path: "c.txt", ActionType: syncdomain.ActionRemoteDelete, Err: failure}), errclass.ClassActionable)
	emit(&syncdomain.ActionCompletion{Path: "d.txt", ActionType: syncdomain.ActionUpload}, errclass.ClassShutdown)
	emit(&syncdomain.ActionCompletion{Path: "e.txt", ActionType: syncdomain.ActionUpload}, errclass.ClassSuperseded)
	emit(&syncdomain.ActionCompletion{Path: "f.txt", ActionType: syncdomain.ActionBaselineUpdate}, errclass.ClassSuccess)

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

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := newRunOnceBidirectionalMock(driveID)
	eng, syncRoot := newTestEngine(t, mock)
	syncfixture.WriteLocalFile(t, syncRoot, "local.txt", "local-content")

	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
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

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := newRunOnceBidirectionalMock(driveID)
	eng, syncRoot := newTestEngine(t, mock)
	syncfixture.WriteLocalFile(t, syncRoot, "local.txt", "local-content")

	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)

	require.NotEmpty(t, got)
	for _, a := range got {
		assert.True(t, a.Planned, "dry-run activity must be marked planned: %+v", a)
	}
}

// A delete of a folder says so on its activity, done and planned, from the
// planned action's view, and a report counts the folder deletes of a plan
// apart, so the console's summary never calls a folder a file.
//
// Validates: R-6.6.18, R-6.6.19
func TestActivity_AFolderDeleteIsMarkedAsAFolder(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	var got []Activity
	eng.SetActivityHook(func(a Activity) { got = append(got, a) })

	folderView := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	fileView := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionLocalDelete, Path: "old", View: folderView},
		{Type: syncdomain.ActionLocalDelete, Path: "old/a.txt", View: fileView},
		{Type: syncdomain.ActionRemoteDelete, Path: "gone", View: folderView},
	}

	completion := syncexec.CompletionFromTrackedAction(&syncdomain.TrackedAction{Action: actions[0]}, nil, nil)
	assert.Equal(t, syncdomain.ItemTypeFolder, completion.ItemType)
	eng.emitActivity(&completion, &syncdomain.ResultDecision{Class: errclass.ClassSuccess}, false)
	eng.emitPlannedActivity(&syncdomain.ActionPlan{Actions: actions})

	require.Len(t, got, 4)
	assert.True(t, got[0].Folder, "the done delete of a folder")
	assert.True(t, got[1].Folder, "the planned delete of a folder")
	assert.False(t, got[2].Folder, "the planned delete of a file")
	assert.True(t, got[3].Folder, "the planned delete of a folder on OneDrive")

	local, remote := countFolderDeletes(actions)
	assert.Equal(t, 1, local)
	assert.Equal(t, 1, remote)
}
