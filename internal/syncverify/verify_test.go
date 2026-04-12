package syncverify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

type fakeVerifyTree struct {
	statFn func(rel string) (os.FileInfo, error)
	absFn  func(rel string) (string, error)
}

func (t fakeVerifyTree) Stat(rel string) (os.FileInfo, error) {
	if t.statFn == nil {
		return nil, fmt.Errorf("unexpected Stat(%q)", rel)
	}

	return t.statFn(rel)
}

func (t fakeVerifyTree) Abs(rel string) (string, error) {
	if t.absFn == nil {
		return "", fmt.Errorf("unexpected Abs(%q)", rel)
	}

	return t.absFn(rel)
}

type fakeFileInfo struct {
	name string
	size int64
	dir  bool
}

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return i.size }
func (i fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (i fakeFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i fakeFileInfo) IsDir() bool        { return i.dir }
func (i fakeFileInfo) Sys() any           { return nil }

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
	bl := &syncstore.Baseline{
		ByPath: map[string]*syncstore.BaselineEntry{
			"docs/readme.md": {
				Path: "docs/readme.md", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
			"notes.txt": {
				Path: "notes.txt", DriveID: driveid.New("d"), ItemID: "i2",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[syncstore.DirLowerKey][]*syncstore.BaselineEntry),
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

	bl := &syncstore.Baseline{
		ByPath: map[string]*syncstore.BaselineEntry{
			"ghost.txt": {
				Path: "ghost.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "somehash", LocalSize: 100, LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[syncstore.DirLowerKey][]*syncstore.BaselineEntry),
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

	bl := &syncstore.Baseline{
		ByPath: map[string]*syncstore.BaselineEntry{
			"changed.txt": {
				Path: "changed.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "wrong-hash", LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[syncstore.DirLowerKey][]*syncstore.BaselineEntry),
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

	bl := &syncstore.Baseline{
		ByPath:     make(map[string]*syncstore.BaselineEntry),
		ByDirLower: make(map[syncstore.DirLowerKey][]*syncstore.BaselineEntry),
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
	bl := &syncstore.Baseline{
		ByPath: map[string]*syncstore.BaselineEntry{
			"docs": {
				Path: "docs", DriveID: driveid.New("d"), ItemID: "folder1",
				ItemType: synctypes.ItemTypeFolder,
			},
			"docs/file.txt": {
				Path: "docs/file.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: hash, LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[syncstore.DirLowerKey][]*syncstore.BaselineEntry),
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

	bl := &syncstore.Baseline{
		ByPath: map[string]*syncstore.BaselineEntry{
			"size.txt": {
				Path: "size.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: synctypes.ItemTypeFile, LocalHash: "somehash", LocalSize: 99999, LocalSizeKnown: true,
			},
		},
		ByDirLower: make(map[syncstore.DirLowerKey][]*syncstore.BaselineEntry),
	}

	report, err := VerifyBaseline(t.Context(), bl, tree, newTestLogger())
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifySizeMismatch, report.Mismatches[0].Status)
}

// Validates: R-2.7
func TestVerifyBaseline_SortsMismatchesByPath(t *testing.T) {
	t.Parallel()

	bl := syncstore.NewBaselineForTest([]*syncstore.BaselineEntry{
		{
			Path: "zeta.txt", DriveID: driveid.New("d"), ItemID: "i3",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hash-z",
		},
		{
			Path: "alpha.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hash-a",
		},
		{
			Path: "mid.txt", DriveID: driveid.New("d"), ItemID: "i2",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hash-m",
		},
	})

	report, err := verifyBaselineWithHasher(
		t.Context(),
		bl,
		fakeVerifyTree{
			statFn: func(string) (os.FileInfo, error) {
				return nil, os.ErrNotExist
			},
		},
		func(string) (string, error) {
			return "", nil
		},
		newTestLogger(),
	)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 3)
	assert.Equal(t, []string{"alpha.txt", "mid.txt", "zeta.txt"}, []string{
		report.Mismatches[0].Path,
		report.Mismatches[1].Path,
		report.Mismatches[2].Path,
	})
}

// Validates: R-2.7
func TestVerifyBaseline_CanceledContextReturnsWrappedCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	bl := syncstore.NewBaselineForTest([]*syncstore.BaselineEntry{{
		Path: "alpha.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: "hash-a",
	}})

	report, err := verifyBaselineWithHasher(
		ctx,
		bl,
		fakeVerifyTree{
			statFn: func(string) (os.FileInfo, error) {
				return fakeFileInfo{name: "alpha.txt", size: 1}, nil
			},
		},
		func(string) (string, error) {
			return "", nil
		},
		newTestLogger(),
	)
	require.Error(t, err)
	assert.Nil(t, report)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "verify canceled")
}

// Validates: R-2.7
func TestVerifyBaseline_EmptyLocalHashSkipsHashCheck(t *testing.T) {
	t.Parallel()

	bl := syncstore.NewBaselineForTest([]*syncstore.BaselineEntry{{
		Path: "sharepoint.docx", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: "", LocalSize: 42, LocalSizeKnown: true,
	}})

	hashCalled := false
	report, err := verifyBaselineWithHasher(
		t.Context(),
		bl,
		fakeVerifyTree{
			statFn: func(string) (os.FileInfo, error) {
				return fakeFileInfo{name: "sharepoint.docx", size: 42}, nil
			},
		},
		func(string) (string, error) {
			hashCalled = true
			return "", nil
		},
		newTestLogger(),
	)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Verified)
	assert.Empty(t, report.Mismatches)
	assert.False(t, hashCalled, "verify should skip hashing when the baseline has no local hash")
}

// Validates: R-2.7
func TestVerifyBaseline_ClassifiesStatErrorsAsMissing(t *testing.T) {
	t.Parallel()

	report, err := verifyBaselineWithHasher(
		t.Context(),
		syncstore.NewBaselineForTest([]*syncstore.BaselineEntry{{
			Path: "problem.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: synctypes.ItemTypeFile, LocalHash: "expected-hash",
		}}),
		fakeVerifyTree{
			statFn: func(string) (os.FileInfo, error) {
				return nil, errors.New("disk I/O failed")
			},
		},
		func(string) (string, error) {
			return "", nil
		},
		newTestLogger(),
	)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyMissing, report.Mismatches[0].Status)
	assert.Equal(t, "expected-hash", report.Mismatches[0].Expected)
	assert.Equal(t, "disk I/O failed", report.Mismatches[0].Actual)
}

// Validates: R-2.7
func TestVerifyBaseline_ClassifiesRootedPathErrorsAsHashMismatch(t *testing.T) {
	t.Parallel()

	report, err := verifyBaselineWithHasher(
		t.Context(),
		syncstore.NewBaselineForTest([]*syncstore.BaselineEntry{{
			Path: "problem.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: synctypes.ItemTypeFile, LocalHash: "expected-hash",
		}}),
		fakeVerifyTree{
			statFn: func(string) (os.FileInfo, error) {
				return fakeFileInfo{name: "problem.txt", size: 42}, nil
			},
			absFn: func(string) (string, error) {
				return "", errors.New("root join failed")
			},
		},
		func(string) (string, error) {
			return "", nil
		},
		newTestLogger(),
	)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyHashMismatch, report.Mismatches[0].Status)
	assert.Equal(t, "expected-hash", report.Mismatches[0].Expected)
	assert.Equal(t, "root join failed", report.Mismatches[0].Actual)
}

// Validates: R-2.7
func TestVerifyBaseline_ClassifiesHashComputationErrorsAsHashMismatch(t *testing.T) {
	t.Parallel()

	report, err := verifyBaselineWithHasher(
		t.Context(),
		syncstore.NewBaselineForTest([]*syncstore.BaselineEntry{{
			Path: "problem.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: synctypes.ItemTypeFile, LocalHash: "expected-hash",
		}}),
		fakeVerifyTree{
			statFn: func(string) (os.FileInfo, error) {
				return fakeFileInfo{name: "problem.txt", size: 42}, nil
			},
			absFn: func(string) (string, error) {
				return "/sync/problem.txt", nil
			},
		},
		func(string) (string, error) {
			return "", errors.New("hash reader failed")
		},
		newTestLogger(),
	)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, VerifyHashMismatch, report.Mismatches[0].Status)
	assert.Equal(t, "expected-hash", report.Mismatches[0].Expected)
	assert.Equal(t, "hash reader failed", report.Mismatches[0].Actual)
}
