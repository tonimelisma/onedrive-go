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
		Source:           SourceLocal,
		Type:             ChangeCreate,
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
	assert.Equal(t, []LocalStateRow{{
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

	batch := localObservationBatchForEvent(&ChangeEvent{
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
	obs := NewLocalObserver(baseline, testLogger(t), 1)
	batches := make(chan localObservationBatch, 1)
	obs.SetLocalObservationBatchChannel(batches)

	obs.runSafetyScan(t.Context(), mustOpenSyncTree(t, root), nil)

	batch := <-batches
	assert.True(t, batch.fullSnapshot)
	require.Len(t, batch.rows, 1)
	assert.Equal(t, "fresh.txt", batch.rows[0].Path)
	assert.Equal(t, ItemTypeFile, batch.rows[0].ItemType)
	assert.True(t, batch.dirty)
}
