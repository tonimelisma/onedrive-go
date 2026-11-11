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

	batch := localObservationBatchForEvent(&changeEvent{
		Source:           SourceLocal,
		Type:             changeCreate,
		Path:             "new.txt",
		ItemType:         ItemTypeFile,
		Hash:             "hash",
		Size:             12,
		Mtime:            34,
		LocalDevice:      56,
		LocalInode:       78,
		LocalHasIdentity: true,
	})

	assert.True(t, batch.dirty)
	assert.Equal(t, []localStateRow{{
		Path:             "new.txt",
		ItemType:         ItemTypeFile,
		Hash:             "hash",
		Size:             12,
		Mtime:            34,
		LocalDevice:      56,
		LocalInode:       78,
		LocalHasIdentity: true,
	}}, batch.rows)
	assert.Empty(t, batch.deletedPaths)
	assert.Empty(t, batch.deletedPrefixes)
}

// Validates: R-2.8.8
func TestLocalObservationBatchForEvent_DirectoryDeleteRemovesPrefix(t *testing.T) {
	t.Parallel()

	batch := localObservationBatchForEvent(&changeEvent{
		Source:    SourceLocal,
		Type:      ChangeDelete,
		Path:      "gone",
		ItemType:  ItemTypeFolder,
		IsDeleted: true,
	})

	assert.True(t, batch.dirty)
	assert.Empty(t, batch.rows)
	assert.Empty(t, batch.deletedPaths)
	assert.Equal(t, []string{"gone"}, batch.deletedPrefixes)
}

// Validates: R-2.8.8
func TestLocalObserver_RunSafetyScanEmitsFullLocalSnapshotBatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "fresh.txt"), []byte("fresh"), 0o600))

	baseline := NewBaselineForTest(nil)
	obs := newLocalObserver(baseline, testLogger(t), 1)
	batches := make(chan localObservationBatch, 1)
	obs.SetLocalObservationBatchChannel(batches)

	require.True(t, obs.runSafetyScan(t.Context(), mustOpenSyncTree(t, root), nil))

	// The scan sends before it returns: a missing batch fails here instead of
	// hanging the test.
	var batch localObservationBatch
	select {
	case batch = <-batches:
	default:
		require.Fail(t, "the safety scan sent no local snapshot batch")
	}
	assert.True(t, batch.fullSnapshot)
	require.Len(t, batch.rows, 1)
	assert.Equal(t, "fresh.txt", batch.rows[0].Path)
	assert.Equal(t, ItemTypeFile, batch.rows[0].ItemType)
	// The runtime replans when the snapshot changes committed truth, not on
	// the scan's baseline-relative event count (R-2.8.21).
	assert.False(t, batch.dirty)
}

// forwardedFindings takes the finding set a safety scan forwarded, failing
// when it forwarded none. The scan sends before it returns, so the set is
// already buffered.
func forwardedFindings(t *testing.T, findings <-chan []skippedItem) []skippedItem {
	t.Helper()

	select {
	case skipped := <-findings:
		return skipped
	default:
		require.Fail(t, "the safety scan forwarded no finding set")
		return nil
	}
}

// A safety scan is a full scan, so it forwards its finding set even when the
// set is empty: an empty set is what clears a finding whose cause went away.
// Forwarding only a non-empty set kept the last watch-mode finding (here an
// interrupted removal's leftover the user deleted) until the next restart.
//
// Validates: R-2.8.21, R-6.4.9
func TestLocalObserver_RunSafetyScanForwardsAnEmptyFindingSet(t *testing.T) {
	t.Parallel()

	const leftoverName = ".onedrive-go.0123456789abcdef.detach"
	root := t.TempDir()
	leftover := filepath.Join(root, leftoverName)
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	obs := newLocalObserver(NewBaselineForTest(nil), testLogger(t), 1)
	findings := make(chan []skippedItem, 1)
	obs.SetSkippedChannel(findings)
	tree := mustOpenSyncTree(t, root)

	require.True(t, obs.runSafetyScan(t.Context(), tree, nil))
	skipped := forwardedFindings(t, findings)
	require.Len(t, skipped, 1)
	assert.Equal(t, leftoverName, skipped[0].Path)
	assert.Equal(t, issueLocalInterruptedRemoval, skipped[0].Reason)

	require.NoError(t, os.Remove(leftover))
	require.True(t, obs.runSafetyScan(t.Context(), tree, nil))
	assert.Empty(t, forwardedFindings(t, findings), "the scan found nothing to skip, and says so")
}
