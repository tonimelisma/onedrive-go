package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMvJSONOutput_Serialization(t *testing.T) {
	out := mvJSONOutput{
		Source:      "/docs/report.pdf",
		Destination: "/archive/report.pdf",
		ID:          "item-456",
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded mvJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/docs/report.pdf", decoded.Source)
	assert.Equal(t, "/archive/report.pdf", decoded.Destination)
	assert.Equal(t, "item-456", decoded.ID)
}

func TestMvJSONOutput_Fields(t *testing.T) {
	out := mvJSONOutput{
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
