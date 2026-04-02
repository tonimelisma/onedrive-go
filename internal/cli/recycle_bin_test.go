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

func TestFormatRecycleBinTable_Empty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, formatRecycleBinTable(&buf, nil))
	assert.Equal(t, "Recycle bin is empty\n", buf.String())
}

func TestFormatRecycleBinTable_Items(t *testing.T) {
	t.Parallel()

	items := []graph.Item{
		{
			ID:       "id-1",
			Name:     "deleted-file.txt",
			Size:     1024,
			IsFolder: false,
			ModifiedAt: time.Date(
				2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			ID:       "id-2",
			Name:     "deleted-folder",
			IsFolder: true,
			ModifiedAt: time.Date(
				2024, 6, 14, 8, 0, 0, 0, time.UTC),
		},
	}

	var buf bytes.Buffer
	require.NoError(t, formatRecycleBinTable(&buf, items))
	output := buf.String()

	assert.Contains(t, output, "NAME")
	assert.Contains(t, output, "SIZE")
	assert.Contains(t, output, "TYPE")
	assert.Contains(t, output, "DELETED")
	assert.Contains(t, output, "ID")
	assert.Contains(t, output, "deleted-file.txt")
	assert.Contains(t, output, "file")
	assert.Contains(t, output, "deleted-folder")
	assert.Contains(t, output, "folder")
	assert.Contains(t, output, "id-1")
}

func TestFormatRecycleBinJSON(t *testing.T) {
	t.Parallel()

	items := []graph.Item{
		{
			ID:       "id-1",
			Name:     "file.txt",
			Size:     512,
			IsFolder: false,
			ModifiedAt: time.Date(
				2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
	}

	var buf bytes.Buffer
	err := formatRecycleBinJSON(&buf, items)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, `"name": "file.txt"`)
	assert.Contains(t, output, `"id": "id-1"`)
	assert.Contains(t, output, `"type": "file"`)
}

// Validates: R-1.9
func TestNewRecycleBinCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newRecycleBinCmd()
	assert.Equal(t, "recycle-bin", cmd.Use)

	subNames := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		subNames = append(subNames, sub.Name())
	}

	assert.Contains(t, subNames, "list")
	assert.Contains(t, subNames, "restore")
	assert.Contains(t, subNames, "empty")
}

func TestNewRecycleBinCmd_EmptyRequiresConfirm(t *testing.T) {
	t.Parallel()

	cmd := newRecycleBinCmd()
	emptySub, _, err := cmd.Find([]string{"empty"})
	require.NoError(t, err)
	assert.NotNil(t, emptySub.Flags().Lookup("confirm"))
}

func TestItemType_File(t *testing.T) {
	t.Parallel()

	item := &graph.Item{IsFolder: false}
	assert.Equal(t, "file", itemType(item))
}

func TestItemType_Folder(t *testing.T) {
	t.Parallel()

	item := &graph.Item{IsFolder: true}
	assert.Equal(t, "folder", itemType(item))
}

func TestFormatRecycleBinJSON_Empty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := formatRecycleBinJSON(&buf, nil)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "[]")
}

// Validates: R-1.9.4
func TestPrintRecycleBinRestoreJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printRecycleBinRestoreJSON(&buf, recycleBinJSONItem{
		ID:      "item-restored-1",
		Name:    "recovered-file.txt",
		Size:    2048,
		Type:    "file",
		Deleted: "2024-06-15T10:30:00Z",
	})
	require.NoError(t, err)

	var decoded recycleBinJSONItem
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "item-restored-1", decoded.ID)
	assert.Equal(t, "recovered-file.txt", decoded.Name)
	assert.Equal(t, int64(2048), decoded.Size)
	assert.Equal(t, "file", decoded.Type)
	assert.Equal(t, "2024-06-15T10:30:00Z", decoded.Deleted)
}

// Validates: R-1.9.4
func TestPrintRecycleBinRestoreJSON_Folder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printRecycleBinRestoreJSON(&buf, recycleBinJSONItem{
		ID:      "folder-restored-1",
		Name:    "recovered-folder",
		Size:    0,
		Type:    "folder",
		Deleted: "2024-06-14T08:00:00Z",
	})
	require.NoError(t, err)

	var decoded recycleBinJSONItem
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "folder", decoded.Type)
	assert.Equal(t, "recovered-folder", decoded.Name)
}
