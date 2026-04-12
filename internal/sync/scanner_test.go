package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// DetectCaseCollisions — pure function tests (R-2.12.1)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1
func TestDetectCaseCollisions_TwoWay(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "dir/File.txt", Name: "File.txt", Type: synctypes.ChangeCreate},
		{Path: "dir/file.txt", Name: "file.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
	assert.Empty(t, clean, "both colliders should be removed from clean events")
	require.Len(t, collisions, 2, "both colliders should be returned as SkippedItems")

	for _, si := range collisions {
		assert.Equal(t, synctypes.IssueCaseCollision, si.Reason)
		assert.NotEmpty(t, si.Detail, "detail should name the colliding file")
	}
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_ThreeWay(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "File.txt", Name: "File.txt", Type: synctypes.ChangeCreate},
		{Path: "file.txt", Name: "file.txt", Type: synctypes.ChangeCreate},
		{Path: "FILE.txt", Name: "FILE.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
	assert.Empty(t, clean)
	assert.Len(t, collisions, 3, "all three colliders should be flagged")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DifferentDirs(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "dir1/File.txt", Name: "File.txt", Type: synctypes.ChangeCreate},
		{Path: "dir2/file.txt", Name: "file.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
	assert.Len(t, clean, 2, "files in different dirs should not collide")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_Empty(t *testing.T) {
	t.Parallel()

	clean, collisions := DetectCaseCollisions(nil, nil)
	assert.Empty(t, clean)
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_NoCollisions(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "a.txt", Name: "a.txt", Type: synctypes.ChangeCreate},
		{Path: "b.txt", Name: "b.txt", Type: synctypes.ChangeCreate},
		{Path: "c.txt", Name: "c.txt", Type: synctypes.ChangeCreate},
		{Path: "dir/d.txt", Name: "d.txt", Type: synctypes.ChangeCreate},
		{Path: "dir/e.txt", Name: "e.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
	assert.Len(t, clean, 5, "no collisions — all events returned clean")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DetailContainsCollidingName(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "docs/Report.md", Name: "Report.md", Type: synctypes.ChangeCreate},
		{Path: "docs/report.md", Name: "report.md", Type: synctypes.ChangeCreate},
	}

	_, collisions := DetectCaseCollisions(events, nil)
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

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Both colliders should be in Skipped, not in Events.
	var collisionPaths []string
	for _, si := range result.Skipped {
		if si.Reason == synctypes.IssueCaseCollision {
			collisionPaths = append(collisionPaths, si.Path)
		}
	}
	assert.Len(t, collisionPaths, 2, "both case-colliding files should be skipped")

	// No ChangeEvent should reference either colliding path.
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
	baseline := baselineWith(
		&BaselineEntry{Path: "Doc.pdf", DriveID: driveid.New("d"), ItemID: "id1"},
		&BaselineEntry{Path: "doc.pdf", DriveID: driveid.New("d"), ItemID: "id2"},
	)

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Colliders should NOT generate ChangeDelete events — they stay in the
	// observed map to prevent false deletions.
	for _, ev := range result.Events {
		if ev.Type == synctypes.ChangeDelete {
			assert.NotEqual(t, "Doc.pdf", ev.Path, "collider should not generate false delete")
			assert.NotEqual(t, "doc.pdf", ev.Path, "collider should not generate false delete")
		}
	}
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

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// No collision — the FS itself prevents the scenario from arising.
	for _, si := range result.Skipped {
		assert.NotEqual(t, synctypes.IssueCaseCollision, si.Reason,
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
	baseline := newBaselineForTest([]*BaselineEntry{
		{Path: "File.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []ChangeEvent{
		{Path: "file.txt", Name: "file.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, baseline)
	assert.Empty(t, clean, "new file should be removed — collides with baseline")
	require.Len(t, collisions, 1)
	assert.Equal(t, "file.txt", collisions[0].Path)
	assert.Equal(t, synctypes.IssueCaseCollision, collisions[0].Reason)
	assert.Contains(t, collisions[0].Detail, "File.txt", "detail should name the synced file")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineExactMatch_NoCollision(t *testing.T) {
	t.Parallel()

	// Baseline has "File.txt" synced. Event for "File.txt" (same casing) is NOT a collision.
	baseline := newBaselineForTest([]*BaselineEntry{
		{Path: "File.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []ChangeEvent{
		{Path: "File.txt", Name: "File.txt", Type: synctypes.ChangeModify},
	}

	clean, collisions := DetectCaseCollisions(events, baseline)
	assert.Len(t, clean, 1, "exact match should not be a collision")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineAndEventCollision(t *testing.T) {
	t.Parallel()

	// Baseline has "FILE.txt" synced. Two new events: "File.txt" and "file.txt".
	// All three should be treated as colliders, but only the events are flagged
	// (baseline entry is already synced and produces no event).
	baseline := newBaselineForTest([]*BaselineEntry{
		{Path: "FILE.txt", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []ChangeEvent{
		{Path: "File.txt", Name: "File.txt", Type: synctypes.ChangeCreate},
		{Path: "file.txt", Name: "file.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, baseline)
	assert.Empty(t, clean, "both events should be flagged")
	assert.Len(t, collisions, 2, "both events collide with each other and baseline")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_BaselineInSubdir(t *testing.T) {
	t.Parallel()

	// Baseline has "docs/Report.md". Event for "docs/report.md" — collision.
	baseline := newBaselineForTest([]*BaselineEntry{
		{Path: "docs/Report.md", DriveID: driveid.New("d"), ItemID: "id1"},
	})

	events := []ChangeEvent{
		{Path: "docs/report.md", Name: "report.md", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, baseline)
	assert.Empty(t, clean)
	require.Len(t, collisions, 1)
	assert.Contains(t, collisions[0].Detail, "Report.md")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_NilBaseline_NoChange(t *testing.T) {
	t.Parallel()

	// nil baseline (backwards compat) — should behave exactly like before.
	events := []ChangeEvent{
		{Path: "unique.txt", Name: "unique.txt", Type: synctypes.ChangeCreate},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
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
	events := []ChangeEvent{
		{Path: "Docs", Name: "Docs", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFolder},
		{Path: "docs", Name: "docs", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFile},
		{Path: "Docs/readme.txt", Name: "readme.txt", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFile},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
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
	events := []ChangeEvent{
		{Path: "Docs", Name: "Docs", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFolder},
		{Path: "docs", Name: "docs", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFile},
		{Path: "Docs/sub", Name: "sub", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFolder},
		{Path: "Docs/sub/file.txt", Name: "file.txt", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFile},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
	assert.Empty(t, clean, "all events should be suppressed")
	assert.Len(t, collisions, 4, "parent + child dir + nested file should all be skipped")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DirectoryNoCollision_ChildrenPass(t *testing.T) {
	t.Parallel()

	// No collision on the directory — children pass through normally.
	events := []ChangeEvent{
		{Path: "Docs", Name: "Docs", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFolder},
		{Path: "Docs/readme.txt", Name: "readme.txt", Type: synctypes.ChangeCreate, ItemType: synctypes.ItemTypeFile},
	}

	clean, collisions := DetectCaseCollisions(events, nil)
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

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Both files should appear in Skipped with IssueCaseCollision.
	var collisions []SkippedItem
	for _, si := range result.Skipped {
		if si.Reason == synctypes.IssueCaseCollision {
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

	events := []ChangeEvent{
		{Path: "Docs", ItemType: synctypes.ItemTypeFolder},
		{Path: "docs", ItemType: synctypes.ItemTypeFile},
		// These are children of "Docs/" AND collide with each other.
		{Path: "Docs/readme.txt", ItemType: synctypes.ItemTypeFile},
		{Path: "Docs/README.txt", ItemType: synctypes.ItemTypeFile},
	}

	_, collisions := DetectCaseCollisions(events, nil)

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
