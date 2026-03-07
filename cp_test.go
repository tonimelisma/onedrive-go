package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSelfReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sourceID string
		dest     destInfo
		want     bool
	}{
		{
			name:     "same ID",
			sourceID: "item-1",
			dest:     destInfo{existingID: "item-1"},
			want:     true,
		},
		{
			name:     "different ID",
			sourceID: "item-1",
			dest:     destInfo{existingID: "item-2"},
			want:     false,
		},
		{
			name:     "no existing ID",
			sourceID: "item-1",
			dest:     destInfo{existingID: ""},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSelfReference(tt.sourceID, tt.dest))
		})
	}
}

func TestCpJSONOutput_Serialization(t *testing.T) {
	out := cpJSONOutput{
		Source:      "/docs/report.pdf",
		Destination: "/backup/report.pdf",
		ID:          "item-789",
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded cpJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/docs/report.pdf", decoded.Source)
	assert.Equal(t, "/backup/report.pdf", decoded.Destination)
	assert.Equal(t, "item-789", decoded.ID)
}

func TestCpJSONOutput_Fields(t *testing.T) {
	out := cpJSONOutput{
		Source:      "a",
		Destination: "b",
		ID:          "c",
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &raw))

	assert.Contains(t, raw, "source")
	assert.Contains(t, raw, "destination")
	assert.Contains(t, raw, "id")
	assert.Len(t, raw, 3)
}
