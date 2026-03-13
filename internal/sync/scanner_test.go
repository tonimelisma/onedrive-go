package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// detectCaseCollisions — pure function tests (R-2.12.1)
// ---------------------------------------------------------------------------

// Validates: R-2.12.1
func TestDetectCaseCollisions_TwoWay(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "dir/File.txt", Name: "File.txt", Type: ChangeCreate},
		{Path: "dir/file.txt", Name: "file.txt", Type: ChangeCreate},
	}

	clean, collisions := detectCaseCollisions(events)
	assert.Empty(t, clean, "both colliders should be removed from clean events")
	require.Len(t, collisions, 2, "both colliders should be returned as SkippedItems")

	for _, si := range collisions {
		assert.Equal(t, IssueCaseCollision, si.Reason)
		assert.NotEmpty(t, si.Detail, "detail should name the colliding file")
	}
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_ThreeWay(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "File.txt", Name: "File.txt", Type: ChangeCreate},
		{Path: "file.txt", Name: "file.txt", Type: ChangeCreate},
		{Path: "FILE.txt", Name: "FILE.txt", Type: ChangeCreate},
	}

	clean, collisions := detectCaseCollisions(events)
	assert.Empty(t, clean)
	assert.Len(t, collisions, 3, "all three colliders should be flagged")
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DifferentDirs(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "dir1/File.txt", Name: "File.txt", Type: ChangeCreate},
		{Path: "dir2/file.txt", Name: "file.txt", Type: ChangeCreate},
	}

	clean, collisions := detectCaseCollisions(events)
	assert.Len(t, clean, 2, "files in different dirs should not collide")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_Empty(t *testing.T) {
	t.Parallel()

	clean, collisions := detectCaseCollisions(nil)
	assert.Empty(t, clean)
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_NoCollisions(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "a.txt", Name: "a.txt", Type: ChangeCreate},
		{Path: "b.txt", Name: "b.txt", Type: ChangeCreate},
		{Path: "c.txt", Name: "c.txt", Type: ChangeCreate},
		{Path: "dir/d.txt", Name: "d.txt", Type: ChangeCreate},
		{Path: "dir/e.txt", Name: "e.txt", Type: ChangeCreate},
	}

	clean, collisions := detectCaseCollisions(events)
	assert.Len(t, clean, 5, "no collisions — all events returned clean")
	assert.Empty(t, collisions)
}

// Validates: R-2.12.1
func TestDetectCaseCollisions_DetailContainsCollidingName(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Path: "docs/Report.md", Name: "Report.md", Type: ChangeCreate},
		{Path: "docs/report.md", Name: "report.md", Type: ChangeCreate},
	}

	_, collisions := detectCaseCollisions(events)
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

	require.NoError(t, os.WriteFile(f1, []byte("content1"), 0o644))

	if err := os.WriteFile(f2, []byte("content2"), 0o644); err != nil {
		t.Skip("case-insensitive filesystem — cannot create case-colliding files")
	}

	// Verify both files actually exist with distinct inodes (case-sensitive FS).
	info1, err1 := os.Lstat(f1)
	info2, err2 := os.Lstat(f2)
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem — files are the same inode")
	}

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	result, err := obs.FullScan(t.Context(), syncRoot)
	require.NoError(t, err)

	// Both colliders should be in Skipped, not in Events.
	var collisionPaths []string
	for _, si := range result.Skipped {
		if si.Reason == IssueCaseCollision {
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

	require.NoError(t, os.WriteFile(f1, []byte("pdf1"), 0o644))

	if err := os.WriteFile(f2, []byte("pdf2"), 0o644); err != nil {
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

	obs := NewLocalObserver(baseline, testLogger(t), 0)

	result, err := obs.FullScan(t.Context(), syncRoot)
	require.NoError(t, err)

	// Colliders should NOT generate ChangeDelete events — they stay in the
	// observed map to prevent false deletions.
	for _, ev := range result.Events {
		if ev.Type == ChangeDelete {
			assert.NotEqual(t, "Doc.pdf", ev.Path, "collider should not generate false delete")
			assert.NotEqual(t, "doc.pdf", ev.Path, "collider should not generate false delete")
		}
	}
}
