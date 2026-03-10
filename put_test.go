package main

import (
	"bytes"
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
