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

func TestPrintStatText(t *testing.T) {
	item := &graph.Item{
		ID:         "item-123",
		Name:       "photo.jpg",
		Size:       2048,
		IsFolder:   false,
		MimeType:   "image/jpeg",
		ModifiedAt: time.Date(2025, time.March, 10, 14, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2025, time.January, 5, 8, 0, 0, 0, time.UTC),
	}

	var buf bytes.Buffer
	require.NoError(t, printStatText(&buf, item))
	output := buf.String()

	assert.Contains(t, output, "photo.jpg")
	assert.Contains(t, output, "file")
	assert.Contains(t, output, "2048 bytes")
	assert.Contains(t, output, "item-123")
	assert.Contains(t, output, "image/jpeg")
}

func TestPrintStatText_Folder(t *testing.T) {
	item := &graph.Item{
		ID:         "folder-456",
		Name:       "Documents",
		Size:       0,
		IsFolder:   true,
		ModifiedAt: time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	var buf bytes.Buffer
	require.NoError(t, printStatText(&buf, item))
	output := buf.String()

	assert.Contains(t, output, "Documents")
	assert.Contains(t, output, "folder")
	assert.Contains(t, output, "folder-456")
	// MIME should not appear for folders (empty string).
	assert.NotContains(t, output, "MIME:")
}

// --- printStatJSON ---

func TestPrintStatJSON(t *testing.T) {
	item := &graph.Item{
		ID:         "id1",
		Name:       "test.txt",
		Size:       42,
		ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC),
		MimeType:   "text/plain",
		ETag:       "etag1",
	}

	var buf bytes.Buffer
	require.NoError(t, printStatJSON(&buf, item))
	out := buf.String()

	var parsed statJSONOutput
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))
	assert.Equal(t, "id1", parsed.ID)
	assert.Equal(t, int64(42), parsed.Size)
	assert.Equal(t, "text/plain", parsed.MimeType)
}

// --- newStatCmd ---

// Validates: R-1.6
func TestNewStatCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newStatCmd()
	assert.Equal(t, "stat <path>", cmd.Use)
}
