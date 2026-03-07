package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitParentAndName(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantParent string
		wantName   string
	}{
		{"nested path", "foo/bar/baz", "foo/bar", "baz"},
		{"single segment", "baz", "", "baz"},
		{"empty string", "", "", ""},
		{"trailing slash top-level", "/top/", "", "top"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, name := splitParentAndName(tt.path)
			assert.Equal(t, tt.wantParent, parent)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

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
