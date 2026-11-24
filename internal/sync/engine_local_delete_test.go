package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// watchDeleteRemainingRows stores rows as local truth, runs the delete the
// watch observer reports for rel, applies the batch it sends the way the
// watch loop does, and returns the rows left.
func watchDeleteRemainingRows(t *testing.T, baseline syncdomain.BaselineView, rows []syncdomain.LocalStateRow, rel string) []syncdomain.LocalStateRow {
	t.Helper()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)
	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), rows))

	batches := make(chan syncobserve.LocalObservation, 1)
	obs := syncobserve.NewLocalObserver(&syncobserve.LocalObserverInputs{
		Baseline: baseline,
		Logger:   synctest.TestLogger(t),
		Batches:  batches,
	})
	obs.PendingTimers = make(map[string]syncdomain.SyncTimer)
	obs.HashRequests = make(chan syncobserve.HashRequest, syncobserve.HashRequestBufSize)

	syncRoot := t.TempDir()
	obs.HandleDelete(t.Context(), syncfixture.NewRecordingFsWatcher(), syncfixture.MustOpenSyncTree(t, syncRoot),
		filepath.Join(syncRoot, rel), rel, filepath.Base(rel))

	var batch syncobserve.LocalObservation
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

	remaining := watchDeleteRemainingRows(t, syncfixture.EmptyBaseline(), []syncdomain.LocalStateRow{
		{Path: "new", ItemType: syncdomain.ItemTypeFolder},
		{Path: "new/a.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "new/nested", ItemType: syncdomain.ItemTypeFolder},
		{Path: "new/nested/b.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "new-sibling.txt", ItemType: syncdomain.ItemTypeFile},
	}, "new")

	assert.Equal(t, []syncdomain.LocalStateRow{{Path: "new-sibling.txt", ItemType: syncdomain.ItemTypeFile}}, remaining)
}

// Validates: R-2.8.8
func TestHandleDelete_FileDeleteKeepsSiblingsThatShareItsNamePrefix(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "report", DriveID: driveid.New("d"), ItemID: "item1", ItemType: syncdomain.ItemTypeFile,
	})
	remaining := watchDeleteRemainingRows(t, baseline, []syncdomain.LocalStateRow{
		{Path: "report", ItemType: syncdomain.ItemTypeFile},
		{Path: "report.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "report-old", ItemType: syncdomain.ItemTypeFolder},
		{Path: "report-old/a.txt", ItemType: syncdomain.ItemTypeFile},
	}, "report")

	assert.Equal(t, []syncdomain.LocalStateRow{
		{Path: "report-old", ItemType: syncdomain.ItemTypeFolder},
		{Path: "report-old/a.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "report.txt", ItemType: syncdomain.ItemTypeFile},
	}, remaining)
}
