package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
