package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.8
func TestLocalObservationBatchForEvent_FileCreateUpsertsExactRow(t *testing.T) {
	t.Parallel()

	batch := localObservationBatchForEvent(&ChangeEvent{
		Path:             "new.txt",
		ItemType:         ItemTypeFile,
		Hash:             "hash",
		Size:             12,
		Mtime:            34,
		LocalDevice:      56,
		LocalInode:       78,
		LocalHasIdentity: true,
	})

	assert.True(t, batch.Dirty)
	assert.Equal(t, []LocalStateRow{{
		Path:             "new.txt",
		ItemType:         ItemTypeFile,
		Hash:             "hash",
		Size:             12,
		Mtime:            34,
		LocalDevice:      56,
		LocalInode:       78,
		LocalHasIdentity: true,
	}}, batch.Rows)
	assert.Empty(t, batch.DeletedPaths)
	assert.Empty(t, batch.DeletedPrefixes)
}

// Validates: R-2.8.8
func TestLocalObservationBatchForEvent_FileDeleteRemovesSubtree(t *testing.T) {
	t.Parallel()

	// A removal carries no kind: whatever the path was, its rows go with
	// every row below it, so a file and a folder are removed the same way.
	batch := localObservationBatchForEvent(&ChangeEvent{
		Path:      "gone",
		IsDeleted: true,
	})

	assert.True(t, batch.Dirty)
	assert.Empty(t, batch.Rows)
	assert.Empty(t, batch.DeletedPaths)
	assert.Equal(t, []string{"gone"}, batch.DeletedPrefixes,
		"a removed path can still have rows below it")
}

// Validates: R-2.8.8
func TestLocalObserver_RunSafetyScanEmitsFullLocalSnapshotBatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "fresh.txt"), []byte("fresh"), 0o600))

	baseline := NewBaselineForTest(nil)
	obs := NewLocalObserver(baseline, testLogger(t), 1)
	batches := make(chan LocalObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)

	require.True(t, obs.runSafetyScan(t.Context(), mustOpenSyncTree(t, root)))

	// The scan sends before it returns: a missing batch fails here instead of
	// hanging the test.
	var batch LocalObservation
	select {
	case batch = <-batches:
	default:
		require.Fail(t, "the safety scan sent no local snapshot batch")
	}
	assert.Equal(t, LocalSnapshot, batch.Kind)
	require.Len(t, batch.Rows, 1)
	assert.Equal(t, "fresh.txt", batch.Rows[0].Path)
	assert.Equal(t, ItemTypeFile, batch.Rows[0].ItemType)
	// The runtime replans when the snapshot changes committed truth, not on
	// the scan's baseline-relative event count (R-2.8.21).
	assert.False(t, batch.Dirty)
}

// forwardedSnapshot takes the snapshot a full scan sent, failing when it
// sent none. The scan sends before it returns, so the snapshot is already
// buffered.
func forwardedSnapshot(t *testing.T, batches <-chan LocalObservation) *LocalObservation {
	t.Helper()

	select {
	case batch := <-batches:
		require.Equal(t, LocalSnapshot, batch.Kind)
		return &batch
	default:
		require.Fail(t, "the full scan sent no snapshot")
		return &LocalObservation{}
	}
}

// A safety scan is a full scan, so its snapshot carries its finding set even
// when the set is empty: an empty set is what clears a finding whose cause
// went away. Forwarding only a non-empty set kept the last watch-mode finding
// (here an interrupted removal's leftover the user deleted) until the next
// restart.
//
// Validates: R-2.8.21, R-6.4.9
func TestLocalObserver_RunSafetyScanForwardsAnEmptyFindingSet(t *testing.T) {
	t.Parallel()

	const leftoverName = ".onedrive-go.0123456789abcdef.detach"
	root := t.TempDir()
	leftover := filepath.Join(root, leftoverName)
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	obs := NewLocalObserver(NewBaselineForTest(nil), testLogger(t), 1)
	batches := make(chan LocalObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)
	tree := mustOpenSyncTree(t, root)

	require.True(t, obs.runSafetyScan(t.Context(), tree))
	skipped := forwardedSnapshot(t, batches).Findings
	require.Len(t, skipped, 1)
	assert.Equal(t, leftoverName, skipped[0].Path)
	assert.Equal(t, IssueLocalInterruptedRemoval, skipped[0].Reason)

	require.NoError(t, os.Remove(leftover))
	require.True(t, obs.runSafetyScan(t.Context(), tree))
	assert.Empty(t, forwardedSnapshot(t, batches).Findings, "the scan found nothing to skip, and says so")
}

// Validates: R-2.8.8, R-2.8.22
func TestLocalObserver_AFullScanSendsItsFindingsWithItsRows(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(NewBaselineForTest(nil), testLogger(t), 1)
	batches := make(chan LocalObservation, 1)
	obs.SetLocalObservationBatchChannel(batches)
	scan := ScanResult{
		Rows:          []LocalStateRow{{Path: "disk", ItemType: ItemTypeFolder}},
		Skipped:       []SkippedItem{{Path: "bad:name.txt", Reason: IssueInvalidFilename}},
		CrossedMounts: []string{"disk"},
	}

	obs.TrySendFullLocalSnapshot(t.Context(), scan)
	snapshot := forwardedSnapshot(t, batches)
	assert.Equal(t, scan.Rows, snapshot.Rows)
	assert.Equal(t, scan.Skipped, snapshot.Findings)
	assert.Equal(t, scan.CrossedMounts, snapshot.CrossedMounts)

	batches <- LocalObservation{}
	obs.TrySendFullLocalSnapshot(t.Context(), scan)
	assert.Len(t, obs.recoveryScanRequests, 1, "a snapshot the full channel dropped asks for a recovery scan")
	assert.Equal(t, int64(1), obs.ResetDroppedEvents())
}
