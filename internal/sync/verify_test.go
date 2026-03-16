package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.7
func TestVerifyBaseline_AllMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello verify"

	writeTestFile(t, dir, "docs/readme.md", content)
	writeTestFile(t, dir, "notes.txt", content)

	hash := hashContent(t, content)
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"docs/readme.md": {
				Path: "docs/readme.md", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
			"notes.txt": {
				Path: "notes.txt", DriveID: driveid.New("d"), ItemID: "i2",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestVerifyBaseline_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Baseline references a file that doesn't exist on disk.
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"ghost.txt": {
				Path: "ghost.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "somehash", Size: 100,
			},
		},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyMissing, report.Mismatches[0].Status)
}

// Validates: R-2.7
func TestVerifyBaseline_HashMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "modified content"

	writeTestFile(t, dir, "changed.txt", content)

	// Baseline has a different hash than what's on disk.
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"changed.txt": {
				Path: "changed.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "wrong-hash", Size: int64(len(content)),
			},
		},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyHashMismatch, report.Mismatches[0].Status)

	// Actual should be the real hash.
	actualHash := hashContent(t, content)
	assert.Equal(t, actualHash, report.Mismatches[0].Actual)
}

func TestVerifyBaseline_EmptyBaseline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bl := &Baseline{
		ByPath:     make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	assert.Empty(t, report.Mismatches)
}

func TestVerifyBaseline_SkipsFolders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "file content"

	writeTestFile(t, dir, "docs/file.txt", content)

	hash := hashContent(t, content)
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"docs": {
				Path: "docs", DriveID: driveid.New("d"), ItemID: "folder1",
				ItemType: ItemTypeFolder,
			},
			"docs/file.txt": {
				Path: "docs/file.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	// Only the file should be verified, not the folder.
	assert.Equal(t, 1, report.Verified)
	assert.Empty(t, report.Mismatches)
}

func TestVerifyBaseline_SizeMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "short"

	writeTestFile(t, dir, "size.txt", content)

	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"size.txt": {
				Path: "size.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "somehash", Size: 99999,
			},
		},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifySizeMismatch, report.Mismatches[0].Status)
}
