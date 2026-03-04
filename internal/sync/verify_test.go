package sync

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// hashBytes computes the QuickXorHash of raw bytes and returns the
// base64-encoded digest, matching the format stored in the baseline.
func hashBytes(t *testing.T, data []byte) string {
	t.Helper()

	h := quickxorhash.New()
	h.Write(data)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestVerifyBaseline_AllMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello verify"

	writeTestFile(t, dir, "docs/readme.md", content)
	writeTestFile(t, dir, "notes.txt", content)

	hash := hashBytes(t, []byte(content))
	bl := &Baseline{
		byPath: map[string]*BaselineEntry{
			"docs/readme.md": {
				Path: "docs/readme.md", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
			"notes.txt": {
				Path: "notes.txt", DriveID: driveid.New("d"), ItemID: "i2",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Verified)
	assert.Empty(t, report.Mismatches)
}

func TestVerifyBaseline_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Baseline references a file that doesn't exist on disk.
	bl := &Baseline{
		byPath: map[string]*BaselineEntry{
			"ghost.txt": {
				Path: "ghost.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "somehash", Size: 100,
			},
		},
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyMissing, report.Mismatches[0].Status)
}

func TestVerifyBaseline_HashMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "modified content"

	writeTestFile(t, dir, "changed.txt", content)

	// Baseline has a different hash than what's on disk.
	bl := &Baseline{
		byPath: map[string]*BaselineEntry{
			"changed.txt": {
				Path: "changed.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "wrong-hash", Size: int64(len(content)),
			},
		},
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyHashMismatch, report.Mismatches[0].Status)

	// Actual should be the real hash.
	actualHash := hashBytes(t, []byte(content))
	assert.Equal(t, actualHash, report.Mismatches[0].Actual)
}

func TestVerifyBaseline_EmptyBaseline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bl := &Baseline{
		byPath: make(map[string]*BaselineEntry),
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

	hash := hashBytes(t, []byte(content))
	bl := &Baseline{
		byPath: map[string]*BaselineEntry{
			"docs": {
				Path: "docs", DriveID: driveid.New("d"), ItemID: "folder1",
				ItemType: ItemTypeFolder,
			},
			"docs/file.txt": {
				Path: "docs/file.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
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
		byPath: map[string]*BaselineEntry{
			"size.txt": {
				Path: "size.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "somehash", Size: 99999,
			},
		},
	}

	ctx := t.Context()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifySizeMismatch, report.Mismatches[0].Status)
}
