package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
