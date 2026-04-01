package syncobserve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func TestObserveSinglePath_HashFailureEmitsEventWithEmptyHash(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	relPath := "hash-failure.txt"
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, relPath), []byte("payload"), 0o600))

	result, err := ObserveSinglePath(nil, syncRoot, relPath, nil, time.Now().UnixNano(), func(string) (string, error) {
		return "", errors.New("boom")
	})
	require.NoError(t, err)
	require.NotNil(t, result.Event)
	assert.Nil(t, result.Skipped)
	assert.False(t, result.Resolved)
	assert.Empty(t, result.Event.Hash)
}

func TestObserveSinglePath_InvalidNameReturnsSkipped(t *testing.T) {
	t.Parallel()

	result, err := ObserveSinglePath(nil, t.TempDir(), "bad?.txt", nil, time.Now().UnixNano(), nil)
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

	result, err := ObserveSinglePath(nil, t.TempDir(), relPath, nil, time.Now().UnixNano(), nil)
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

	result, err := ObserveSinglePath(nil, syncRoot, relPath, nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	require.NotNil(t, result.Skipped)
	assert.Nil(t, result.Event)
	assert.False(t, result.Resolved)
	assert.Equal(t, synctypes.IssueFileTooLarge, result.Skipped.Reason)
	assert.Equal(t, int64(MaxOneDriveFileSize+1), result.Skipped.FileSize)
}

func TestObserveSinglePath_InternalExclusionResolves(t *testing.T) {
	t.Parallel()

	result, err := ObserveSinglePath(nil, t.TempDir(), "file.tmp", nil, time.Now().UnixNano(), nil)
	require.NoError(t, err)
	assert.Nil(t, result.Event)
	assert.Nil(t, result.Skipped)
	assert.True(t, result.Resolved)
}
