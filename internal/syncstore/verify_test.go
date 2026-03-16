package syncstore

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// writeTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed. Returns the absolute path.
//
//nolint:unparam // return value used by callers in verify_test.go for path assertions
func writeTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755), "MkdirAll(%s)", filepath.Dir(fullPath))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644), "WriteFile(%s)", fullPath)

	return fullPath
}

// hashContent computes the QuickXorHash of a string, returning the
// base64-encoded digest. Matches the output of driveops.ComputeQuickXorHash
// for the same content written to a file.
func hashContent(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "hash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Validates: R-2.7
func TestVerifyBaseline_AllMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello verify"

	writeTestFile(t, dir, "docs/readme.md", content)
	writeTestFile(t, dir, "notes.txt", content)

	hash := hashContent(t, content)
	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"docs/readme.md": {
				Path: "docs/readme.md", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
			"notes.txt": {
				Path: "notes.txt", DriveID: driveid.New("d"), ItemID: "i2",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	ctx := t.Context()
	logger := newTestLogger(t)

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
	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"ghost.txt": {
				Path: "ghost.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "somehash", Size: 100,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	ctx := t.Context()
	logger := newTestLogger(t)

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
	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"changed.txt": {
				Path: "changed.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "wrong-hash", Size: int64(len(content)),
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	ctx := t.Context()
	logger := newTestLogger(t)

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
	bl := &synctypes.Baseline{
		ByPath:     make(map[string]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	ctx := t.Context()
	logger := newTestLogger(t)

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
	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"docs": {
				Path: "docs", DriveID: driveid.New("d"), ItemID: "folder1",
				ItemType: synctypes.ItemTypeFolder,
			},
			"docs/file.txt": {
				Path: "docs/file.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	ctx := t.Context()
	logger := newTestLogger(t)

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

	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"size.txt": {
				Path: "size.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "somehash", Size: 99999,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	ctx := t.Context()
	logger := newTestLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifySizeMismatch, report.Mismatches[0].Status)
}
