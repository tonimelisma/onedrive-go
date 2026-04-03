package syncverify

import (
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// writeTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed.
func writeTestFile(t *testing.T, dir, relPath, content string) {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o700), "MkdirAll(%s)", filepath.Dir(fullPath))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600), "WriteFile(%s)", fullPath)
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

func newTestLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
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
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
			"notes.txt": {
				Path: "notes.txt", DriveID: driveid.New("d"), ItemID: "i2",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
	require.NoError(t, err)
	assert.Equal(t, 2, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestVerifyBaseline_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"ghost.txt": {
				Path: "ghost.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "somehash", LocalSize: 100, LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
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
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"changed.txt": {
				Path: "changed.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "wrong-hash", LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyHashMismatch, report.Mismatches[0].Status)
	assert.Equal(t, hashContent(t, content), report.Mismatches[0].Actual)
}

// Validates: R-2.7
func TestVerifyBaseline_EmptyBaseline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	bl := &synctypes.Baseline{
		ByPath:     make(map[string]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
	require.NoError(t, err)
	assert.Equal(t, 0, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestVerifyBaseline_SkipsFolders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "file content"

	writeTestFile(t, dir, "docs/file.txt", content)
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	hash := hashContent(t, content)
	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"docs": {
				Path: "docs", DriveID: driveid.New("d"), ItemID: "folder1",
				ItemType: synctypes.ItemTypeFolder,
			},
			"docs/file.txt": {
				Path: "docs/file.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
	require.NoError(t, err)
	assert.Equal(t, 1, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestVerifyBaseline_SizeMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "short"

	writeTestFile(t, dir, "size.txt", content)
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	bl := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"size.txt": {
				Path: "size.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "somehash", LocalSize: 99999, LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifySizeMismatch, report.Mismatches[0].Status)
}
