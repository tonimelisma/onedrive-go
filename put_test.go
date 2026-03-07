package main

import (
	"encoding/json"
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
