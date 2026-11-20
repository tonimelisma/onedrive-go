package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// watchDeleteRemainingRows stores rows as local truth, runs the delete the
// watch observer reports for rel, applies the batch it sends the way the
// watch loop does, and returns the rows left.
func watchDeleteRemainingRows(t *testing.T, baseline *Baseline, rows []LocalStateRow, rel string) []LocalStateRow {
	t.Helper()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = NewDirtyBuffer(eng.logger)
	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), rows))

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.PendingTimers = make(map[string]SyncTimer)
	obs.HashRequests = make(chan HashRequest, HashRequestBufSize)
	batches := make(chan LocalObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)

	syncRoot := t.TempDir()
	obs.HandleDelete(t.Context(), newRecordingFsWatcher(), mustOpenSyncTree(t, syncRoot),
		filepath.Join(syncRoot, rel), rel, filepath.Base(rel))

	var batch LocalObservation
	select {
	case batch = <-batches:
	default:
		require.Fail(t, "the delete sent no batch")
	}
	require.NoError(t, rt.handleWatchLocalObservationBatch(t.Context(), &batch))

	remaining, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)

	return remaining
}

// Validates: R-2.8.8
//
// A new folder whose watch could not be added still had its children's rows
// stored by the scan of the new folder. Its delete removes them with it, even
// though neither the baseline nor the watch set says the path was a folder.
func TestHandleDelete_UnwatchedNewDirectoryRemovesSubtreeRows(t *testing.T) {
	t.Parallel()

	remaining := watchDeleteRemainingRows(t, emptyBaseline(), []LocalStateRow{
		{Path: "new", ItemType: ItemTypeFolder},
		{Path: "new/a.txt", ItemType: ItemTypeFile},
		{Path: "new/nested", ItemType: ItemTypeFolder},
		{Path: "new/nested/b.txt", ItemType: ItemTypeFile},
		{Path: "new-sibling.txt", ItemType: ItemTypeFile},
	}, "new")

	assert.Equal(t, []LocalStateRow{{Path: "new-sibling.txt", ItemType: ItemTypeFile}}, remaining)
}

// Validates: R-2.8.8
func TestHandleDelete_FileDeleteKeepsSiblingsThatShareItsNamePrefix(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(&BaselineEntry{
		Path: "report", DriveID: driveid.New("d"), ItemID: "item1", ItemType: ItemTypeFile,
	})
	remaining := watchDeleteRemainingRows(t, baseline, []LocalStateRow{
		{Path: "report", ItemType: ItemTypeFile},
		{Path: "report.txt", ItemType: ItemTypeFile},
		{Path: "report-old", ItemType: ItemTypeFolder},
		{Path: "report-old/a.txt", ItemType: ItemTypeFile},
	}, "report")

	assert.Equal(t, []LocalStateRow{
		{Path: "report-old", ItemType: ItemTypeFolder},
		{Path: "report-old/a.txt", ItemType: ItemTypeFile},
		{Path: "report.txt", ItemType: ItemTypeFile},
	}, remaining)
}
