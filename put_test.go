package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPutJSONOutput_Serialization(t *testing.T) {
	out := putJSONOutput{
		Path: "/remote/test.txt",
		ID:   "item-123",
		Size: 2048,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded putJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/remote/test.txt", decoded.Path)
	assert.Equal(t, "item-123", decoded.ID)
	assert.Equal(t, int64(2048), decoded.Size)
}

func TestPutFolderJSONOutput_Serialization(t *testing.T) {
	out := putFolderJSONOutput{
		Files: []putJSONOutput{
			{Path: "dir/a.txt", ID: "id-1", Size: 100},
			{Path: "dir/b.txt", ID: "id-2", Size: 200},
		},
		FoldersCreated: 2,
		TotalSize:      300,
		Errors:         []string{"dir/c.txt: upload failed"},
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded putFolderJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Len(t, decoded.Files, 2)
	assert.Equal(t, 2, decoded.FoldersCreated)
	assert.Equal(t, int64(300), decoded.TotalSize)
	assert.Len(t, decoded.Errors, 1)
}

// Validates: R-1.3.4
func TestPrintPutJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printPutJSON(&buf, putJSONOutput{
		Path: "/remote/upload.txt",
		ID:   "item-xyz",
		Size: 8192,
	})
	require.NoError(t, err)

	var decoded putJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "/remote/upload.txt", decoded.Path)
	assert.Equal(t, "item-xyz", decoded.ID)
	assert.Equal(t, int64(8192), decoded.Size)
}

// Validates: R-1.3.4
func TestPrintPutFolderJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printPutFolderJSON(&buf, putFolderJSONOutput{
		Files: []putJSONOutput{
			{Path: "/remote/dir/a.txt", ID: "id-1", Size: 100},
		},
		FoldersCreated: 3,
		TotalSize:      100,
		Errors:         nil,
	})
	require.NoError(t, err)

	var decoded putFolderJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Len(t, decoded.Files, 1)
	assert.Equal(t, 3, decoded.FoldersCreated)
	assert.Equal(t, int64(100), decoded.TotalSize)
}

func TestCountUploadFiles_NestedTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "docs"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "beta.txt"), []byte("bb"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "gamma.txt"), []byte("ccc"), 0o600))

	state := &uploadWalkState{}
	require.NoError(t, countUploadFiles(root, state))
	assert.Equal(t, 3, state.total)
	assert.Empty(t, state.result.Errors)
}

func TestWalkUploadTree_MissingRootReturnsError(t *testing.T) {
	t.Parallel()

	err := walkUploadTree(filepath.Join(t.TempDir(), "missing"), func(string, os.DirEntry) error {
		return nil
	}, func(string, error) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading upload root")
}

func TestWalkUploadTreeEntry_MissingChildDirectoryRecorded(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blockedDir := filepath.Join(root, "blocked")
	require.NoError(t, os.Mkdir(blockedDir, 0o700))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].IsDir())

	require.NoError(t, os.Remove(blockedDir))

	var visited []string
	var recordedPath string
	var recordedErr error

	err = walkUploadTreeEntry(blockedDir, entries[0], func(path string, _ os.DirEntry) error {
		visited = append(visited, path)
		return nil
	}, func(path string, callbackErr error) {
		recordedPath = path
		recordedErr = callbackErr
	})
	require.NoError(t, err)
	assert.Equal(t, []string{blockedDir}, visited)
	assert.Equal(t, blockedDir, recordedPath)
	require.Error(t, recordedErr)
	assert.ErrorIs(t, recordedErr, os.ErrNotExist)
}

func TestAppendUploadWalkError_AppendsPathAndMessage(t *testing.T) {
	t.Parallel()

	state := &uploadWalkState{}
	appendUploadWalkError(state, "/tmp/example.txt", errors.New("boom"))
	require.Len(t, state.result.Errors, 1)
	assert.Equal(t, "/tmp/example.txt: boom", state.result.Errors[0])
}

func TestIsFatalUploadWalkError(t *testing.T) {
	t.Parallel()

	assert.True(t, isFatalUploadWalkError(context.Canceled))
	assert.True(t, isFatalUploadWalkError(context.DeadlineExceeded))
	assert.False(t, isFatalUploadWalkError(errors.New("boom")))
}
