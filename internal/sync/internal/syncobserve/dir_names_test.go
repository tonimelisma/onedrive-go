package syncobserve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.12.3
// Every pair OneDrive treats as one name is a rival, every pair it keeps
// apart is not, and a spelling is never its own rival.
func TestDirNames_ProbeCorpus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		disk, name string
		rival      bool
		sameForm   bool
	}{
		{disk: "File.txt", name: "file.txt", rival: true},
		{disk: "STRASSE.txt", name: "straße.txt", rival: true},
		{disk: "a😀.txt", name: "a.txt", rival: true},
		{disk: norm.NFD.String("café.txt"), name: "café.txt"},
		{disk: "file.txt", name: "file.txt"},
		{disk: "other.txt", name: "file.txt"},
		{disk: "\u212a.txt", name: "K.txt"}, // Kelvin sign: a distinct name on OneDrive
		{disk: "\u00b5.txt", name: "\u039c.txt"},
	} {
		d := make(DirNames)
		d.Add(tc.disk)
		others, sameForm := d.rivals(tc.name)
		assert.Equal(t, tc.rival, len(others) > 0, "%q against %q on disk", tc.name, tc.disk)
		assert.Equal(t, tc.sameForm, sameForm, "%q against %q on disk", tc.name, tc.disk)
	}

	// The two Unicode forms of one name on disk together are twins of the
	// stored name, and adding a spelling twice records it once.
	twins := make(DirNames)
	twins.Add("café.txt")
	twins.Add(norm.NFD.String("café.txt"))
	twins.Add("café.txt")
	others, sameForm := twins.rivals("café.txt")
	assert.Empty(t, others)
	assert.True(t, sameForm)
}

// Validates: R-2.12.3
// The full scan is the one local collision detector: the names a directory
// holds on disk give one verdict per name, whichever entries changed. Watch
// mode detects nothing itself; a collider's event wakes the replan whose scan
// judges it.
func TestLocalCollisions_ScanJudgesTheNamesOnDisk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	disk := []string{"Report.txt", "report.txt", "notes.txt", "STRASSE.md", "straße.md", "solo.md"}
	for _, name := range disk {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	if len(entries) != len(disk) {
		t.Skip("case-insensitive filesystem: colliding names fold into one file")
	}

	result, err := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, dir))
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"Report.txt", "report.txt", "STRASSE.md", "straße.md"}, collisionPaths(&result))
	events := make([]string, 0, len(result.Events))
	for i := range result.Events {
		events = append(events, result.Events[i].Path)
	}
	assert.ElementsMatch(t, []string{"notes.txt", "solo.md"}, events)
}

// syncedScanRoot writes files, scans them once, and returns the sync root
// and the baseline that records every scanned row as synced.
func syncedScanRoot(t *testing.T, files map[string]string) (string, syncdomain.BaselineView) {
	t.Helper()

	syncRoot := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(syncRoot, name), []byte(content), 0o600))
	}
	first, err := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	entries := make([]*syncdomain.BaselineEntry, 0, len(first.Rows))
	for i := range first.Rows {
		row := first.Rows[i]
		entries = append(entries, &syncdomain.BaselineEntry{
			Path: row.Path, DriveID: driveid.New(syncfixture.EngineTestDriveID), ItemID: "id-" + row.Path, ItemType: row.ItemType,
			LocalHash: row.Hash, RemoteHash: row.Hash, LocalSize: row.Size, LocalSizeKnown: true,
			RemoteSize: row.Size, RemoteSizeKnown: true, LocalMtime: row.Mtime, LocalCtime: row.Ctime,
			LocalDevice: row.LocalDevice, LocalInode: row.LocalInode, LocalBirthTime: row.LocalBirthTime,
			LocalHasIdentity: row.LocalHasIdentity,
		})
	}

	return syncRoot, syncfixture.BaselineWith(entries...)
}

// writeCollider writes name beside an existing file, skipping the test on a
// filesystem that folds the two names into one.
func writeCollider(t *testing.T, syncRoot, existing, name string) {
	t.Helper()

	path := filepath.Join(syncRoot, name)
	require.NoError(t, os.WriteFile(path, []byte("collider"), 0o600))
	a, errA := os.Lstat(filepath.Join(syncRoot, existing))
	b, errB := os.Lstat(path)
	if errA != nil || errB != nil || os.SameFile(a, b) {
		t.Skip("case-insensitive filesystem: the two names are one file")
	}
}

func collisionPaths(result *ScanResult) []string {
	var paths []string
	for i := range result.Skipped {
		if result.Skipped[i].Reason == syncdomain.IssueCaseCollision {
			paths = append(paths, result.Skipped[i].Path)
		}
	}

	return paths
}

// Validates: R-2.12.2
// A synced file the user changed and a new collider beside it both wait:
// uploading either would decide which of the two OneDrive keeps.
func TestFullScan_ModifiedSyncedSiblingOfNewColliderBlocksBoth(t *testing.T) {
	t.Parallel()

	syncRoot, baseline := syncedScanRoot(t, map[string]string{"Report.txt": "synced"})
	writeCollider(t, syncRoot, "Report.txt", "report.txt")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "Report.txt"), []byte("edited since the sync"), 0o600))

	result, err := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"Report.txt", "report.txt"}, collisionPaths(&result))
	assert.Empty(t, result.Events, "neither collider is uploaded")
}

// Validates: R-2.12.2
// An unchanged synced file keeps syncing when a new collider appears beside
// it: only the new name waits.
func TestFullScan_UnchangedSyncedSiblingKeepsSyncing(t *testing.T) {
	t.Parallel()

	syncRoot, baseline := syncedScanRoot(t, map[string]string{"Report.txt": "synced"})
	writeCollider(t, syncRoot, "Report.txt", "report.txt")

	result, err := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)}).
		FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	assert.Equal(t, []string{"report.txt"}, collisionPaths(&result))
	require.Len(t, result.Skipped, 1)
	assert.Contains(t, result.Skipped[0].Detail, "conflicts with synced file Report.txt")
	assert.NotNil(t, findRow(result.Rows, "Report.txt"), "the synced file keeps its row")
}
