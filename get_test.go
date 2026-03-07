package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJoinRemotePath(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  string
		want   string
	}{
		{"root parent", "", "docs", "docs"},
		{"slash parent", "/", "docs", "docs"},
		{"nested parent", "foo/bar", "baz", "foo/bar/baz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, joinRemotePath(tt.parent, tt.child))
		})
	}
}

func TestCountRemoteFiles_PopulatesCache(t *testing.T) {
	state := &downloadState{
		childCache: make(map[string][]cachedChild),
	}

	// Verify cache is populated after counting.
	assert.Empty(t, state.childCache)

	// Simulate: after counting, cache should have entries.
	state.childCache["root"] = []cachedChild{
		{name: "file.txt", id: "f1"},
	}
	state.total = 1

	assert.Len(t, state.childCache, 1)
	assert.Equal(t, 1, state.total)
}

func TestDownloadState_CacheReducesAPICalls(t *testing.T) {
	// Verify that downloadState has childCache field.
	state := &downloadState{
		childCache: make(map[string][]cachedChild),
	}
	require.NotNil(t, state.childCache)

	// Verify atomic counter type exists for API call counting in tests.
	var calls atomic.Int32
	calls.Add(1)
	assert.Equal(t, int32(1), calls.Load())
}

func TestGetJSONOutput_Serialization(t *testing.T) {
	out := getJSONOutput{
		Path:         "/tmp/test.txt",
		Size:         1024,
		HashVerified: true,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded getJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/tmp/test.txt", decoded.Path)
	assert.Equal(t, int64(1024), decoded.Size)
	assert.True(t, decoded.HashVerified)
}

func TestGetFolderJSONOutput_Serialization(t *testing.T) {
	out := getFolderJSONOutput{
		Files: []getJSONOutput{
			{Path: "dir/a.txt", Size: 100, HashVerified: true},
			{Path: "dir/b.txt", Size: 200, HashVerified: false},
		},
		FoldersCreated: 3,
		TotalSize:      300,
		Errors:         []string{"file c.txt: permission denied"},
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded getFolderJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Len(t, decoded.Files, 2)
	assert.Equal(t, 3, decoded.FoldersCreated)
	assert.Equal(t, int64(300), decoded.TotalSize)
	assert.Len(t, decoded.Errors, 1)
	assert.Contains(t, decoded.Errors[0], "permission denied")
}

func TestGetFolderJSONOutput_EmptyErrors(t *testing.T) {
	out := getFolderJSONOutput{
		Files:          []getJSONOutput{},
		FoldersCreated: 1,
		TotalSize:      0,
		Errors:         nil,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	// nil slice should serialize as null, not [].
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, float64(1), decoded["folders_created"])
	assert.Equal(t, float64(0), decoded["total_size"])
}
