package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestPrintItemsTable(t *testing.T) {
	items := []graph.Item{
		{
			Name:       "readme.txt",
			Size:       1024,
			IsFolder:   false,
			ModifiedAt: time.Date(2025, time.January, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			Name:       "docs",
			Size:       0,
			IsFolder:   true,
			ModifiedAt: time.Date(2025, time.February, 1, 9, 0, 0, 0, time.UTC),
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printItemsTable(&buf, items))
	output := buf.String()

	// Headers should be present.
	assert.Contains(t, output, "NAME")
	assert.Contains(t, output, "SIZE")
	assert.Contains(t, output, "MODIFIED")
	// Folders sort first and get a trailing slash.
	assert.Contains(t, output, "docs/")
	assert.Contains(t, output, "readme.txt")
}

// --- printItemsJSON ---

// Validates: R-1.1.1
func TestPrintItemsJSON(t *testing.T) {
	items := []graph.Item{
		{Name: "file.txt", Size: 100, ID: "id1", ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "dir", IsFolder: true, ID: "id2", ModifiedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
	}

	var buf bytes.Buffer
	require.NoError(t, printItemsJSON(&buf, items))
	out := buf.String()

	assert.Contains(t, out, `"file.txt"`)
	assert.Contains(t, out, `"id1"`)

	var parsed []lsJSONItem
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))
	assert.Len(t, parsed, 2)
}

// --- newLsCmd ---

// Validates: R-1.1
func TestNewLsCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newLsCmd()
	assert.Equal(t, "ls [path]", cmd.Use)
}
