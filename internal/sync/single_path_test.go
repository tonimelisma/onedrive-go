package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func TestObserveSinglePath_HashFailureEmitsEventWithEmptyHash(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	relPath := "hash-failure.txt"
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, relPath), []byte("payload"), 0o600))

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), relPath, nil, time.Now().UnixNano(), func(string) (string, error) {
		return "", errors.New("boom")
	})
	require.NoError(t, err)
	require.NotNil(t, result.Event)
	assert.Nil(t, result.Skipped)
	assert.False(t, result.Resolved)
	assert.Empty(t, result.Event.Hash)
}

func TestObserveSinglePath_MissingPathResolves(t *testing.T) {
	t.Parallel()

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, t.TempDir()), "missing.txt", nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	assert.Nil(t, result.Event)
	assert.Nil(t, result.Skipped)
	assert.True(t, result.Resolved)
}

// Validates: R-2.10.7
func TestObserveSinglePath_ReusesBaselineHashWhenMetadataMatches(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	relPath := "baseline-reuse.txt"
	absPath := filepath.Join(syncRoot, relPath)
	content := []byte("payload")
	oldTime := time.Now().Add(-2 * time.Second).Round(0)

	require.NoError(t, os.WriteFile(absPath, content, 0o600))
	require.NoError(t, os.Chtimes(absPath, oldTime, oldTime))

	info, err := os.Stat(absPath)
	require.NoError(t, err)

	actualHash, err := ComputeStableHash(absPath)
	require.NoError(t, err)
	require.NotEqual(t, "cached-hash", actualHash)

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), relPath, &synctypes.BaselineEntry{
		Path:           relPath,
		ItemType:       synctypes.ItemTypeFile,
		LocalSize:      info.Size(),
		LocalSizeKnown: true,
		LocalMtime:     info.ModTime().UnixNano(),
		LocalHash:      "cached-hash",
	}, time.Now().UnixNano(), func(string) (string, error) {
		return "", errors.New("hash function should not be called when metadata matches")
	})
	require.NoError(t, err)
	require.NotNil(t, result.Event)
	assert.Equal(t, "cached-hash", result.Event.Hash)
	assert.Nil(t, result.Skipped)
	assert.False(t, result.Resolved)
}

// Validates: R-6.7.15
func TestObserveSinglePath_ReusesBaselineHashWhenSameSecondMtimeDiffersByFractionalSecond(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	relPath := "baseline-reuse-fractional.txt"
	absPath := filepath.Join(syncRoot, relPath)
	content := []byte("payload")
	fileTime := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Second).Add(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(absPath, content, 0o600))
	require.NoError(t, os.Chtimes(absPath, fileTime, fileTime))

	info, err := os.Stat(absPath)
	require.NoError(t, err)

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), relPath, &synctypes.BaselineEntry{
		Path:           relPath,
		ItemType:       synctypes.ItemTypeFile,
		LocalSize:      info.Size(),
		LocalSizeKnown: true,
		LocalMtime:     fileTime.Add(600 * time.Millisecond).UnixNano(),
		LocalHash:      "cached-hash",
	}, time.Now().UnixNano(), func(string) (string, error) {
		return "", errors.New("hash function should not be called for same-second timestamp drift")
	})
	require.NoError(t, err)
	require.NotNil(t, result.Event)
	assert.Equal(t, "cached-hash", result.Event.Hash)
	assert.Nil(t, result.Skipped)
	assert.False(t, result.Resolved)
}

func TestObserveSinglePath_DirectoryProducesFolderEvent(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	relPath := "docs"
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, relPath), 0o700))

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), relPath, nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	require.NotNil(t, result.Event)
	assert.Equal(t, synctypes.ItemTypeFolder, result.Event.ItemType)
	assert.Equal(t, synctypes.ChangeModify, result.Event.Type)
	assert.Empty(t, result.Event.Hash)
	assert.Nil(t, result.Skipped)
	assert.False(t, result.Resolved)
}

// Validates: R-2.4.6
func TestObserveSinglePath_SymlinkFollowedByDefault(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "real.txt"), []byte("payload"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(syncRoot, "real.txt"), filepath.Join(syncRoot, "link.txt")))

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), "link.txt", nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	require.NotNil(t, result.Event)
	assert.Equal(t, synctypes.ItemTypeFile, result.Event.ItemType)
	assert.Nil(t, result.Skipped)
	assert.False(t, result.Resolved)
}

func TestObserveSinglePath_UnexpectedStatErrorReturnsWrappedError(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	blocker := filepath.Join(syncRoot, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not-a-directory"), 0o600))

	_, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), "blocker/child.txt", nil, time.Now().UnixNano(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "observe single path blocker/child.txt: stat:")
}

func TestObserveSinglePath_InvalidNameReturnsSkipped(t *testing.T) {
	t.Parallel()

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, t.TempDir()), "bad?.txt", nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	require.NotNil(t, result.Skipped)
	assert.Nil(t, result.Event)
	assert.False(t, result.Resolved)
	assert.Equal(t, synctypes.IssueInvalidFilename, result.Skipped.Reason)
}

// Validates: R-2.11.3
func TestObserveSinglePathWithFilter_SharePointRootFormsReturnsSkipped(t *testing.T) {
	t.Parallel()

	result, err := ObserveSinglePathWithFilter(
		nil,
		mustOpenSyncTree(t, t.TempDir()),
		"forms",
		nil,
		time.Now().UnixNano(),
		nil,
		synctypes.LocalFilterConfig{},
		synctypes.LocalObservationRules{RejectSharePointRootForms: true},
	)
	require.NoError(t, err)
	require.NotNil(t, result.Skipped)
	assert.Nil(t, result.Event)
	assert.False(t, result.Resolved)
	assert.Equal(t, synctypes.IssueInvalidFilename, result.Skipped.Reason)
}

func TestObserveSinglePath_PathTooLongReturnsSkipped(t *testing.T) {
	t.Parallel()

	segments := []string{"file.txt"}
	for len(strings.Join(segments, "/")) <= MaxOneDrivePathLength {
		segments = append([]string{"segment"}, segments...)
	}
	relPath := strings.Join(segments, "/")

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, t.TempDir()), relPath, nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	require.NotNil(t, result.Skipped)
	assert.Nil(t, result.Event)
	assert.False(t, result.Resolved)
	assert.Equal(t, synctypes.IssuePathTooLong, result.Skipped.Reason)
}

func TestObserveSinglePath_OversizedFileReturnsSkipped(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	file, err := os.CreateTemp(syncRoot, "oversized-*.bin")
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, file.Truncate(MaxOneDriveFileSize+1))
	relPath := filepath.Base(file.Name())

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, syncRoot), relPath, nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	require.NotNil(t, result.Skipped)
	assert.Nil(t, result.Event)
	assert.False(t, result.Resolved)
	assert.Equal(t, synctypes.IssueFileTooLarge, result.Skipped.Reason)
	assert.Equal(t, int64(MaxOneDriveFileSize+1), result.Skipped.FileSize)
}

func TestObserveSinglePath_InternalExclusionResolves(t *testing.T) {
	t.Parallel()

	result, err := ObserveSinglePath(nil, mustOpenSyncTree(t, t.TempDir()), "file.tmp", nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	assert.Nil(t, result.Event)
	assert.Nil(t, result.Skipped)
	assert.True(t, result.Resolved)
}

// Validates: R-2.4.4, R-2.4.5
func TestObserveSinglePathWithScope_ResolvesOutOfScopePathsSilently(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "docs"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "docs", "keep.txt"), []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "docs", "drop.txt"), []byte("drop"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "docs", ".odignore"), []byte("marker"), 0o600))

	scopeSnapshot, err := syncscope.NewSnapshot(syncscope.Config{
		SyncPaths:    []string{"/docs/keep.txt"},
		IgnoreMarker: ".odignore",
	}, []string{"docs"})
	require.NoError(t, err)

	for _, relPath := range []string{"docs/.odignore", "docs/drop.txt"} {
		result, err := ObserveSinglePathWithScope(
			nil,
			mustOpenSyncTree(t, syncRoot),
			relPath,
			nil,
			time.Now().UnixNano(),
			nil,
			synctypes.LocalFilterConfig{},
			synctypes.LocalObservationRules{},
			scopeSnapshot,
		)
		require.NoError(t, err)
		assert.Nil(t, result.Event)
		assert.Nil(t, result.Skipped)
		assert.True(t, result.Resolved)
	}
}
