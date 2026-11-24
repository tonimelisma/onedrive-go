package syncobserve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Scans of a synced file and a second entry beside it whose name differs only
// in Unicode form. Both map to one stored path (R-2.13.1), so the second entry
// is a local name collision (R-2.13.2): neither file is chosen.

// syncedTwinScanRoot writes the synced file, scans it once and returns the
// sync root, the baseline that records the scanned row as synced, and the row.
func syncedTwinScanRoot(t *testing.T) (string, syncdomain.BaselineView, syncdomain.LocalStateRow) {
	t.Helper()
	syncfixture.SkipUnlessBothUnicodeFormsCoexist(t)

	syncRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, syncfixture.TwinSyncedName), []byte(syncfixture.TwinSyncedContent), 0o600))

	first, err := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)
	require.Len(t, first.Rows, 1)
	row := first.Rows[0]
	require.Equal(t, syncfixture.TwinSyncedName, row.Path)

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: row.Path, DriveID: driveid.New(syncfixture.EngineTestDriveID), ItemID: "synced-file", ItemType: syncdomain.ItemTypeFile,
		LocalHash: row.Hash, RemoteHash: row.Hash, LocalSize: row.Size, LocalSizeKnown: true,
		RemoteSize: row.Size, RemoteSizeKnown: true, LocalMtime: row.Mtime, LocalCtime: row.Ctime,
		LocalDevice: row.LocalDevice, LocalInode: row.LocalInode, LocalBirthTime: row.LocalBirthTime,
		LocalHasIdentity: row.LocalHasIdentity,
	})

	return syncRoot, baseline, row
}

// Validates: R-2.13.2
func TestFullScan_UnicodeFormTwinOfSyncedFileIsACollision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		twinContent   string
		syncedEdit    string
		wantRowSynced bool
	}{
		{name: "twin with other content", twinContent: "twin content, longer\n", wantRowSynced: true},
		{name: "twin with equal content", twinContent: syncfixture.TwinSyncedContent, wantRowSynced: true},
		{name: "both changed", twinContent: "twin content, longer\n", syncedEdit: "edited synced file\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			syncRoot, baseline, syncedRow := syncedTwinScanRoot(t)
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, syncfixture.TwinSecondName), []byte(tt.twinContent), 0o600))
			if tt.syncedEdit != "" {
				require.NoError(t, os.WriteFile(filepath.Join(syncRoot, syncfixture.TwinSyncedName), []byte(tt.syncedEdit), 0o600))
			}

			result, err := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)}).
				FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
			require.NoError(t, err)

			require.Len(t, result.Rows, 1)
			row := result.Rows[0]
			assert.Equal(t, syncedRow.LocalInode, row.LocalInode, "the row keeps describing the synced file")
			if tt.wantRowSynced {
				assert.Equal(t, syncedRow.Hash, row.Hash)
				assert.Equal(t, syncedRow.Size, row.Size)
			} else {
				assert.Equal(t, int64(len(tt.syncedEdit)), row.Size, "the synced file's own edit is its row")
			}

			require.Len(t, result.Skipped, 1)
			assert.Equal(t, syncfixture.TwinSyncedName, result.Skipped[0].Path)
			assert.Equal(t, syncdomain.IssueCaseCollision, result.Skipped[0].Reason)
			assert.Equal(t, sameUnicodeFormCollisionDetail, result.Skipped[0].Detail)
		})
	}
}

// Validates: R-2.13.2
func TestFullScan_RemovedUnicodeFormTwinClearsTheCollision(t *testing.T) {
	t.Parallel()

	syncRoot, baseline, syncedRow := syncedTwinScanRoot(t)
	twinPath := filepath.Join(syncRoot, syncfixture.TwinSecondName)
	require.NoError(t, os.WriteFile(twinPath, []byte("twin content, longer\n"), 0o600))
	tree := syncfixture.MustOpenSyncTree(t, syncRoot)
	obs := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)})

	withTwin, err := obs.FullScan(t.Context(), tree)
	require.NoError(t, err)
	require.Len(t, withTwin.Skipped, 1)

	require.NoError(t, os.Remove(twinPath))
	result, err := obs.FullScan(t.Context(), tree)
	require.NoError(t, err)

	assert.Empty(t, result.Skipped)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, syncedRow.Hash, result.Rows[0].Hash)
	assert.Equal(t, syncedRow.LocalInode, result.Rows[0].LocalInode)
}

// Validates: R-2.13.2
//
// A folder beside a synced file whose name differs only in Unicode form is a
// twin too: nothing below it gets a row, so no pass plans an upload into a
// path that is a file.
func TestFullScan_UnicodeFormFolderTwinOfSyncedFileIsACollision(t *testing.T) {
	t.Parallel()

	syncRoot, baseline, syncedRow := syncedTwinScanRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, syncfixture.TwinSecondName), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, syncfixture.TwinSecondName, "child.txt"), []byte("child\n"), 0o600))

	result, err := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	require.Len(t, result.Rows, 1, "the folder twin's child has no row")
	assert.Equal(t, syncdomain.ItemTypeFile, result.Rows[0].ItemType)
	assert.Equal(t, syncedRow.Hash, result.Rows[0].Hash)
	require.Len(t, result.Skipped, 1)
	assert.Equal(t, syncfixture.TwinSyncedName, result.Skipped[0].Path)
	assert.Equal(t, syncdomain.IssueCaseCollision, result.Skipped[0].Reason)
}

// Validates: R-2.13.2
func TestFullScan_UnicodeFormFileTwinOfSyncedFolderIsACollision(t *testing.T) {
	t.Parallel()
	syncfixture.SkipUnlessBothUnicodeFormsCoexist(t)

	syncRoot := t.TempDir()
	syncedFolder := filepath.Join(syncRoot, syncfixture.NfcCafe)
	require.NoError(t, os.Mkdir(syncedFolder, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(syncedFolder, "kept.txt"), []byte("kept\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, syncfixture.NfdCafe), []byte("twin\n"), 0o600))
	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: syncfixture.NfcCafe, DriveID: driveid.New(syncfixture.EngineTestDriveID), ItemID: "synced-folder", ItemType: syncdomain.ItemTypeFolder,
	})

	result, err := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	rows := map[string]syncdomain.ItemType{}
	for i := range result.Rows {
		rows[result.Rows[i].Path] = result.Rows[i].ItemType
	}
	assert.Equal(t, map[string]syncdomain.ItemType{syncfixture.NfcCafe: syncdomain.ItemTypeFolder, syncfixture.NfcCafe + "/kept.txt": syncdomain.ItemTypeFile}, rows,
		"the synced folder keeps its row and its content; the file twin has none")
	require.Len(t, result.Skipped, 1)
	assert.Equal(t, syncfixture.NfcCafe, result.Skipped[0].Path)
	assert.Equal(t, syncdomain.IssueCaseCollision, result.Skipped[0].Reason)
}
