package syncobserve

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// DetectCaseCollisions — pure function tests (R-2.12.1)
// ---------------------------------------------------------------------------

// namesOnDisk is the disk a pure DetectCaseCollisions test describes: every
// event's entry, and every baseline entry the scan observed (all of them when
// observed is nil). A second event at one stored path is that name's other
// Unicode form, spelled decomposed on disk.
func namesOnDisk(events []syncdomain.LocalEntry, baseline syncdomain.BaselineView, observed map[string]bool) ScanNames {
	names := make(ScanNames)
	seen := make(map[string]bool, len(events))
	for i := range events {
		disk := filepath.Base(events[i].Path)
		if seen[events[i].Path] {
			disk = norm.NFD.String(disk)
		}
		seen[events[i].Path] = true
		names.Add(events[i].Path, disk)
	}
	if baseline != nil {
		baseline.ForEachPath(func(path string, _ syncdomain.BaselineEntry) {
			if observed == nil || observed[path] {
				names.Add(path, filepath.Base(path))
			}
		})
	}

	return names
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_TwoWay(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "dir/File.txt"},
		{Path: "dir/file.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Empty(t, clean, "both colliders should be removed from clean events")
	require.Len(t, collisions, 2, "both colliders should be returned as SkippedItems")

	for _, si := range collisions {
		assert.Equal(t, syncdomain.IssueCaseCollision, si.Reason)
		assert.NotEmpty(t, si.Detail, "detail should name the colliding file")
	}
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_ThreeWay(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "File.txt"},
		{Path: "file.txt"},
		{Path: "FILE.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Empty(t, clean)
	assert.Len(t, collisions, 3, "all three colliders should be flagged")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DifferentDirs(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "dir1/File.txt"},
		{Path: "dir2/file.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Len(t, clean, 2, "files in different dirs should not collide")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_Empty(t *testing.T) {
	t.Parallel()

	clean, collisions := DetectCaseCollisions(nil, namesOnDisk(nil, nil, nil))
	assert.Empty(t, clean)
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_NoCollisions(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "a.txt"},
		{Path: "b.txt"},
		{Path: "c.txt"},
		{Path: "dir/d.txt"},
		{Path: "dir/e.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Len(t, clean, 5, "no collisions — all events returned clean")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DetailContainsCollidingName(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "docs/Report.md"},
		{Path: "docs/report.md"},
	}

	_, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	require.Len(t, collisions, 2)

	// Each collision's Detail should mention the other collider's name.
	for _, si := range collisions {
		if si.Path == "docs/Report.md" {
			assert.Contains(t, si.Detail, "report.md")
		} else {
			assert.Contains(t, si.Detail, "Report.md")
		}
	}
}

// ---------------------------------------------------------------------------
// FullScan integration tests (R-2.12.1, R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1, R-2.12.2
func TestFullScan_CaseCollision_InSkippedNotEvents(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()

	// Create two files that differ only in case. On case-sensitive FS (Linux),
	// both files coexist. On macOS (case-insensitive), only one is created.
	f1 := filepath.Join(syncRoot, "File.txt")
	f2 := filepath.Join(syncRoot, "file.txt")

	require.NoError(t, os.WriteFile(f1, []byte("content1"), 0o600))

	if err := os.WriteFile(f2, []byte("content2"), 0o600); err != nil {
		t.Skip("case-insensitive filesystem — cannot create case-colliding files")
	}

	// Verify both files actually exist with distinct inodes (case-sensitive FS).
	info1, err1 := os.Lstat(f1)
	info2, err2 := os.Lstat(f2)
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem — files are the same inode")
	}

	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	})

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Both colliders should be in Skipped, not in Events.
	var collisionPaths []string
	for _, si := range result.Skipped {
		if si.Reason == syncdomain.IssueCaseCollision {
			collisionPaths = append(collisionPaths, si.Path)
		}
	}
	assert.Len(t, collisionPaths, 2, "both case-colliding files should be skipped")

	// No LocalEntry should reference either colliding path.
	for _, ev := range result.Events {
		assert.NotEqual(t, "File.txt", ev.Path, "collider should not appear in events")
		assert.NotEqual(t, "file.txt", ev.Path, "collider should not appear in events")
	}
}

// Validates: R-2.12.1, R-2.12.2
func TestFullScan_CaseCollision_NoFalseDeletion(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()

	f1 := filepath.Join(syncRoot, "Doc.pdf")
	f2 := filepath.Join(syncRoot, "doc.pdf")

	require.NoError(t, os.WriteFile(f1, []byte("pdf1"), 0o600))

	if err := os.WriteFile(f2, []byte("pdf2"), 0o600); err != nil {
		t.Skip("case-insensitive filesystem")
	}

	info1, err1 := os.Lstat(f1)
	info2, err2 := os.Lstat(f2)
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem")
	}

	// Pre-populate baseline with both paths so deletion detection would
	// fire if they were removed from the observed map.
	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{Path: "Doc.pdf", DriveID: driveid.New("d"), ItemID: "id1"},
		&syncdomain.BaselineEntry{Path: "doc.pdf", DriveID: driveid.New("d"), ItemID: "id2"},
	)

	obs := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)})

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Colliders are not deletes: both keep their rows, and the collision
	// finding blocks them instead.
	assert.NotNil(t, findRow(result.Rows, "Doc.pdf"), "collider should not become a false delete")
	assert.NotNil(t, findRow(result.Rows, "doc.pdf"), "collider should not become a false delete")
}

// Validates: R-2.12.1
func TestFullScan_CaseCollision_SyncedItemReplacedBySymlinkStillHoldsItsName(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()

	// The synced Foo.txt is now an unfollowed symbolic link, and a new
	// foo.txt sits beside it. OneDrive still holds Foo.txt, so foo.txt must be
	// held back as a collision rather than uploaded into it.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "target"), []byte("t"), 0o600))
	require.NoError(t, os.Symlink("target", filepath.Join(syncRoot, "Foo.txt")))
	if err := os.WriteFile(filepath.Join(syncRoot, "foo.txt"), []byte("new"), 0o600); err != nil {
		t.Skip("case-insensitive filesystem")
	}
	info1, err1 := os.Lstat(filepath.Join(syncRoot, "Foo.txt"))
	info2, err2 := os.Lstat(filepath.Join(syncRoot, "foo.txt"))
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem")
	}

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{Path: "Foo.txt", DriveID: driveid.New("d"), ItemID: "id1", ItemType: syncdomain.ItemTypeFile},
	)
	obs := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)})

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	collided := false
	for _, si := range result.Skipped {
		if si.Reason == syncdomain.IssueCaseCollision && si.Path == "foo.txt" {
			collided = true
		}
	}
	assert.True(t, collided, "foo.txt collides with the synced Foo.txt the symlink replaced")
	for _, ev := range result.Events {
		assert.NotEqual(t, "foo.txt", ev.Path, "the collider must not reach the plan")
	}
}

// Validates: R-2.1.3
func TestFullScan_StoresFilesystemIdentityForFilesAndDirectories(t *testing.T) {
	t.Parallel()

	const linuxGOOS = "linux"
	switch runtime.GOOS {
	case syncfixture.DarwinGOOS, linuxGOOS:
	default:
		t.Skip("filesystem identity is only expected on Unix-like supported platforms")
	}

	syncRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "Docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "Docs", "report.txt"), []byte("hello"), 0o600))

	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	})
	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	rowsByPath := make(map[string]syncdomain.LocalStateRow, len(result.Rows))
	for i := range result.Rows {
		rowsByPath[result.Rows[i].Path] = result.Rows[i]
	}

	require.Contains(t, rowsByPath, "Docs")
	require.Contains(t, rowsByPath, "Docs/report.txt")
	assert.True(t, rowsByPath["Docs"].LocalHasIdentity)
	assert.NotZero(t, rowsByPath["Docs"].LocalInode)
	assert.True(t, rowsByPath["Docs/report.txt"].LocalHasIdentity)
	assert.NotZero(t, rowsByPath["Docs/report.txt"].LocalInode)
}

// Sync sets aside only a regular file while removing it, so only a regular
// file under that hidden name is reported as an interrupted removal. A folder
// or a FIFO with such a name is the user's: it is excluded like every entry
// with that name (neither synced nor walked), and it is not reported. Before,
// either was reported as a set-aside file.
//
// Validates: R-6.4.9
func TestFullScan_OnlyARegularFileIsReportedAsAnInterruptedRemoval(t *testing.T) {
	t.Parallel()

	const (
		leftoverName = ".onedrive-go.0123456789abcdef.detach"
		folderName   = ".onedrive-go.1123456789abcdef.detach"
		fifoName     = ".onedrive-go.2123456789abcdef.detach"
	)
	syncRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "visible.txt"), []byte("synced\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, leftoverName), []byte("half-removed\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, folderName), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, folderName, "inside.txt"), []byte("user\n"), 0o600))
	require.NoError(t, syscall.Mkfifo(filepath.Join(syncRoot, fifoName), 0o600))

	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	})
	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	var reported []string
	for i := range result.Skipped {
		if result.Skipped[i].Reason == syncdomain.IssueLocalInterruptedRemoval {
			reported = append(reported, result.Skipped[i].Path)
		}
	}
	assert.Equal(t, []string{leftoverName}, reported)
	paths := make([]string, 0, len(result.Rows))
	for i := range result.Rows {
		paths = append(paths, result.Rows[i].Path)
	}
	assert.Equal(t, []string{"visible.txt"}, paths,
		"every entry with that name stays excluded, the folder's contents included")
}

// ---------------------------------------------------------------------------
// Platform-aware case collision tests (R-2.12.1)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1
// On case-insensitive FS (macOS), creating File.txt
// and file.txt results in a single file. No collision can occur at the FS
// level because the OS prevents it. FullScan should not report any collision.
func TestDetectCaseCollisions_CaseInsensitiveFS(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("Linux uses case-sensitive FS by default")
	}

	syncRoot := t.TempDir()

	// Write File.txt — on case-insensitive FS, a subsequent write to
	// file.txt overwrites this (same inode), so only one file exists.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "File.txt"), []byte("content"), 0o600))

	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	})

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// No collision — the FS itself prevents the scenario from arising.
	for _, si := range result.Skipped {
		assert.NotEqual(t, syncdomain.IssueCaseCollision, si.Reason,
			"case-insensitive FS should not report collisions for a single file")
	}

	// The file should appear normally in events.
	assert.Len(t, result.Events, 1, "single file should produce one event")
}

// ---------------------------------------------------------------------------
// DetectCaseCollisions — baseline cross-check tests (R-2.12.1)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineCrossCheck(t *testing.T) {
	t.Parallel()

	// Baseline has "File.txt" synced. A new event for "file.txt" arrives.
	// The new file should collide with the baseline entry.
	baseline := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "File.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []syncdomain.LocalEntry{
		{Path: "file.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, baseline, nil))
	assert.Empty(t, clean, "new file should be removed — collides with baseline")
	require.Len(t, collisions, 1)
	assert.Equal(t, "file.txt", collisions[0].Path)
	assert.Equal(t, syncdomain.IssueCaseCollision, collisions[0].Reason)
	assert.Contains(t, collisions[0].Detail, "File.txt", "detail should name the synced file")
}

// Validates: R-2.12.3
func TestDetectCaseCollisions_FullCaseFoldingCollides(t *testing.T) {
	t.Parallel()

	// Manual test: STRASSE.txt uploaded over straße.txt because ToLower
	// does not fold ß to ss, while OneDrive treats them as one name.
	events := []syncdomain.LocalEntry{
		{Path: "stra\u00dfe.txt"},
		{Path: "STRASSE.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Empty(t, clean)
	require.Len(t, collisions, 2)
}

// Validates: R-2.12.3
func TestDetectCaseCollisions_EmojiOnlyDifferenceCollidesAndNamesBothFiles(t *testing.T) {
	t.Parallel()

	// Manual test: a.txt retried "graph: conflict" forever against a synced
	// a😀.txt, because OneDrive ignores the emoji when comparing names.
	baseline := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "a\U0001F600.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})
	events := []syncdomain.LocalEntry{{Path: "a.txt"}}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, baseline, nil))
	assert.Empty(t, clean)
	require.Len(t, collisions, 1)
	assert.Equal(t, "a.txt", collisions[0].Path)
	assert.Contains(t, collisions[0].Detail, `"a.txt"`)
	assert.Contains(t, collisions[0].Detail, "\"a\U0001F600.txt\"")
	assert.Contains(t, collisions[0].Detail, "emoji")
}

// Validates: R-2.12.3
func TestDetectCaseCollisions_KelvinSignAndMicroSignAreDistinctNames(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "\u212a.txt"},
		{Path: "K.txt"},
		{Path: "\u00b5.txt"},
		{Path: "\u039c.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Len(t, clean, 4)
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_CaseOnlyRenameIsNotACollision(t *testing.T) {
	t.Parallel()

	// mv File.txt file.txt: the baseline still names File.txt, but the scan
	// no longer finds it on disk, so nothing collides.
	baseline := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "File.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})
	events := []syncdomain.LocalEntry{
		{Path: "file.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, baseline, map[string]bool{"file.txt": true}))
	assert.Len(t, clean, 1)
	assert.Empty(t, collisions)

	clean, collisions = DetectCaseCollisions(events, namesOnDisk(events, baseline, map[string]bool{"file.txt": true, "File.txt": true}))
	assert.Empty(t, clean, "a synced variant still on disk is a real collision")
	require.Len(t, collisions, 1)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineExactMatch_NoCollision(t *testing.T) {
	t.Parallel()

	// Baseline has "File.txt" synced. Event for "File.txt" (same casing) is NOT a collision.
	baseline := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "File.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []syncdomain.LocalEntry{
		{Path: "File.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, baseline, nil))
	assert.Len(t, clean, 1, "exact match should not be a collision")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineAndEventCollision(t *testing.T) {
	t.Parallel()

	// Baseline has "FILE.txt" synced. Two new events: "File.txt" and "file.txt".
	// All three should be treated as colliders, but only the events are flagged
	// (baseline entry is already synced and produces no event).
	baseline := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "FILE.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []syncdomain.LocalEntry{
		{Path: "File.txt"},
		{Path: "file.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, baseline, nil))
	assert.Empty(t, clean, "both events should be flagged")
	assert.Len(t, collisions, 2, "both events collide with each other and baseline")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineInSubdir(t *testing.T) {
	t.Parallel()

	// Baseline has "docs/Report.md". Event for "docs/report.md" — collision.
	baseline := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "docs/Report.md", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []syncdomain.LocalEntry{
		{Path: "docs/report.md"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, baseline, nil))
	assert.Empty(t, clean)
	require.Len(t, collisions, 1)
	assert.Contains(t, collisions[0].Detail, "Report.md")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_NilBaseline_NoChange(t *testing.T) {
	t.Parallel()

	// nil baseline (backwards compat) — should behave exactly like before.
	events := []syncdomain.LocalEntry{
		{Path: "unique.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Len(t, clean, 1)
	assert.Empty(t, collisions)
}

// ---------------------------------------------------------------------------
// DetectCaseCollisions — directory child suppression tests (R-2.12.1)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1
func TestDetectCaseCollisions_DirectoryChildrenSuppressed(t *testing.T) {
	t.Parallel()

	// "Docs/" (folder) collides with "docs" (file). Children of "Docs/" must
	// also be suppressed — they can't be uploaded to a folder that won't exist.
	events := []syncdomain.LocalEntry{
		{Path: "Docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "docs", ItemType: syncdomain.ItemTypeFile},
		{Path: "Docs/readme.txt", ItemType: syncdomain.ItemTypeFile},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Empty(t, clean, "all events should be suppressed")
	assert.Len(t, collisions, 3, "parent collision + child should all be skipped")

	// Verify the child's detail mentions the parent directory collision.
	for _, si := range collisions {
		if si.Path == "Docs/readme.txt" {
			assert.Contains(t, si.Detail, "parent directory")
		}
	}
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DirectoryNestedChildrenSuppressed(t *testing.T) {
	t.Parallel()

	// Nested children under a colliding directory should also be suppressed.
	events := []syncdomain.LocalEntry{
		{Path: "Docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "docs", ItemType: syncdomain.ItemTypeFile},
		{Path: "Docs/sub", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Docs/sub/file.txt", ItemType: syncdomain.ItemTypeFile},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Empty(t, clean, "all events should be suppressed")
	assert.Len(t, collisions, 4, "parent + child dir + nested file should all be skipped")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DirectoryNoCollision_ChildrenPass(t *testing.T) {
	t.Parallel()

	// No collision on the directory — children pass through normally.
	events := []syncdomain.LocalEntry{
		{Path: "Docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Docs/readme.txt", ItemType: syncdomain.ItemTypeFile},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Len(t, clean, 2, "no collision — all events pass")
	assert.Empty(t, collisions)
}

// ---------------------------------------------------------------------------
// Platform-aware case collision tests (R-2.12.1)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1
// On case-sensitive FS (Linux), both File.txt and
// file.txt can coexist. DetectCaseCollisions removes both from the event
// set and places them in Skipped.
func TestDetectCaseCollisions_CaseSensitiveFS(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()

	f1 := filepath.Join(syncRoot, "File.txt")
	f2 := filepath.Join(syncRoot, "file.txt")

	require.NoError(t, os.WriteFile(f1, []byte("content1"), 0o600))

	if err := os.WriteFile(f2, []byte("content2"), 0o600); err != nil {
		t.Skip("case-insensitive filesystem — cannot create case-colliding files")
	}

	// Verify both files exist with distinct inodes.
	info1, err1 := os.Lstat(f1)
	info2, err2 := os.Lstat(f2)
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem — files are the same inode")
	}

	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
	})

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Both files should appear in Skipped with IssueCaseCollision.
	var collisions []syncdomain.SkippedItem
	for _, si := range result.Skipped {
		if si.Reason == syncdomain.IssueCaseCollision {
			collisions = append(collisions, si)
		}
	}

	assert.Len(t, collisions, 2,
		"both case-colliding files should be skipped on case-sensitive FS")

	// No events for the colliding files.
	for _, ev := range result.Events {
		assert.NotEqual(t, "File.txt", ev.Path)
		assert.NotEqual(t, "file.txt", ev.Path)
	}
}

// Validates: R-2.12.1
// Defensive test: a child of a colliding directory
// that also participates in a multi-event collision group should appear
// exactly once in SkippedItems (no duplicate from both the child pass
// and the group pass).
func TestDetectCaseCollisions_ChildInMultiGroup_NoDuplicate(t *testing.T) {
	t.Parallel()

	events := []syncdomain.LocalEntry{
		{Path: "Docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "docs", ItemType: syncdomain.ItemTypeFile},
		// These are children of "Docs/" AND collide with each other.
		{Path: "Docs/readme.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "Docs/README.txt", ItemType: syncdomain.ItemTypeFile},
	}

	_, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))

	// Count occurrences of each path in SkippedItems.
	pathCounts := make(map[string]int)
	for _, s := range collisions {
		pathCounts[s.Path]++
	}

	for path, count := range pathCounts {
		assert.Equal(t, 1, count, "path %q should appear exactly once in SkippedItems, got %d", path, count)
	}

	// All 4 events should be in SkippedItems.
	assert.Len(t, collisions, 4, "all 4 events should be skipped")
}

// Validates: R-2.12.3
func TestCollisionDetail_SaysTheNamesAreOneAndWhatToDo(t *testing.T) {
	t.Parallel()

	// Manual test: "conflicts with synced file straße.txt" said neither why
	// STRASSE.txt collides nor what to do about it.
	detail := collisionDetail("conflicts with synced file", "STRASSE.txt", []string{"straße.txt"})
	assert.Equal(t,
		`conflicts with synced file straße.txt: OneDrive treats "STRASSE.txt" and "straße.txt" as the same name; rename one of them`,
		detail)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_UnicodeFormTwinsReportOnceWithClearReason(t *testing.T) {
	t.Parallel()

	// "café" composed and decomposed both normalize to the same NFC path.
	events := []syncdomain.LocalEntry{
		{Path: "dir/café.txt"},
		{Path: "dir/café.txt"},
	}

	clean, collisions := DetectCaseCollisions(events, namesOnDisk(events, nil, nil))
	assert.Empty(t, clean)
	require.Len(t, collisions, 1, "one path, one report")
	assert.Equal(t, "dir/café.txt", collisions[0].Path)
	assert.Contains(t, collisions[0].Detail, "different Unicode form")
	assert.NotContains(t, collisions[0].Detail, "conflicts with café.txt")
}
