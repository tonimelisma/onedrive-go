package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestNewFailuresCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newFailuresCmd()
	assert.Equal(t, "failures", cmd.Use)
	assert.NotNil(t, cmd.RunE)
	assert.NotNil(t, cmd.Flags().Lookup("direction"))
	assert.NotNil(t, cmd.Flags().Lookup("category"))

	// Has a "clear" subcommand.
	clearCmd, _, err := cmd.Find([]string{"clear"})
	require.NoError(t, err)
	assert.Equal(t, "clear [path]", clearCmd.Use)
	assert.NotNil(t, clearCmd.Flags().Lookup("all"))
}

func TestNewIssuesCmd_HiddenAlias(t *testing.T) {
	t.Parallel()

	cmd := newIssuesCmd()
	assert.Equal(t, "issues", cmd.Use)
	assert.True(t, cmd.Hidden, "issues should be a hidden alias")
	assert.NotNil(t, cmd.RunE)
}

func TestToFailureJSON(t *testing.T) {
	t.Parallel()

	row := &sync.SyncFailureRow{
		Path:         "docs/CON",
		DriveID:      driveid.New("test-drive-id"),
		Direction:    "upload",
		Category:     "permanent",
		IssueType:    "invalid_filename",
		ItemID:       "item-123",
		FailureCount: 1,
		LastError:    "file name is not valid for OneDrive: CON",
		HTTPStatus:   0,
		FileSize:     1024,
		FirstSeenAt:  1700000000000000000,
		LastSeenAt:   1700000001000000000,
	}

	j := toFailureJSON(row)
	assert.Equal(t, "docs/CON", j.Path)
	assert.Equal(t, "upload", j.Direction)
	assert.Equal(t, "permanent", j.Category)
	assert.Equal(t, "invalid_filename", j.IssueType)
	assert.Equal(t, 1, j.FailureCount)
	assert.Equal(t, "file name is not valid for OneDrive: CON", j.LastError)
	assert.Equal(t, int64(1024), j.FileSize)
	assert.NotEmpty(t, j.FirstSeenAt)
	assert.NotEmpty(t, j.LastSeenAt)
}

func TestPrintFailuresJSON_EmptyList(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printFailuresJSON(&buf, nil)
	require.NoError(t, err)

	var result []failureJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result)
}

func TestPrintFailuresJSON_WithFailures(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{
			Path:         "docs/CON",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "permanent",
			IssueType:    "invalid_filename",
			FailureCount: 1,
			LastError:    "reserved name",
			FirstSeenAt:  1700000000000000000,
			LastSeenAt:   1700000000000000000,
		},
		{
			Path:         "data/huge.bin",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "permanent",
			IssueType:    "file_too_large",
			FailureCount: 1,
			LastError:    "exceeds 250 GB",
			FileSize:     300 * 1024 * 1024 * 1024,
			FirstSeenAt:  1700000001000000000,
			LastSeenAt:   1700000001000000000,
		},
	}

	var buf bytes.Buffer
	err := printFailuresJSON(&buf, failures)
	require.NoError(t, err)

	var result []failureJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result, 2)
	assert.Equal(t, "docs/CON", result[0].Path)
	assert.Equal(t, "invalid_filename", result[0].IssueType)
	assert.Equal(t, "file_too_large", result[1].IssueType)
	assert.Equal(t, int64(300*1024*1024*1024), result[1].FileSize)
}

func TestPrintFailuresTable(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{
			Path:         "docs/CON",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "permanent",
			IssueType:    "invalid_filename",
			FailureCount: 1,
			LastError:    "reserved name",
			LastSeenAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printFailuresTable(&buf, failures)

	output := buf.String()
	assert.Contains(t, output, "PATH")
	assert.Contains(t, output, "DIRECTION")
	assert.Contains(t, output, "CATEGORY")
	assert.Contains(t, output, "docs/CON")
	assert.Contains(t, output, "upload")
	assert.Contains(t, output, "permanent")
}

func TestPrintFailuresTable_TruncatesLongErrors(t *testing.T) {
	t.Parallel()

	longErr := "this is a very long error message that should be truncated to sixty characters total for table display purposes"
	failures := []sync.SyncFailureRow{
		{
			Path:         "file.txt",
			DriveID:      driveid.New("drive-1"),
			Direction:    "upload",
			Category:     "transient",
			IssueType:    "upload_failed",
			FailureCount: 3,
			LastError:    longErr,
			LastSeenAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printFailuresTable(&buf, failures)

	output := buf.String()
	assert.Contains(t, output, longErr[:maxFailureErrorLen-3]+"...")
	assert.NotContains(t, output, longErr) // full message should not appear
}

func TestFilterFailures_ByDirection(t *testing.T) {
	t.Parallel()

	rows := []sync.SyncFailureRow{
		{Path: "a.txt", Direction: "upload", Category: "transient"},
		{Path: "b.txt", Direction: "download", Category: "transient"},
		{Path: "c.txt", Direction: "upload", Category: "permanent"},
	}

	result := filterFailures(rows, "upload", "")
	require.Len(t, result, 2)
	assert.Equal(t, "a.txt", result[0].Path)
	assert.Equal(t, "c.txt", result[1].Path)
}

func TestFilterFailures_ByCategory(t *testing.T) {
	t.Parallel()

	rows := []sync.SyncFailureRow{
		{Path: "a.txt", Direction: "upload", Category: "transient"},
		{Path: "b.txt", Direction: "download", Category: "permanent"},
		{Path: "c.txt", Direction: "upload", Category: "permanent"},
	}

	result := filterFailures(rows, "", "permanent")
	require.Len(t, result, 2)
	assert.Equal(t, "b.txt", result[0].Path)
	assert.Equal(t, "c.txt", result[1].Path)
}

func TestFilterFailures_BothFilters(t *testing.T) {
	t.Parallel()

	rows := []sync.SyncFailureRow{
		{Path: "a.txt", Direction: "upload", Category: "transient"},
		{Path: "b.txt", Direction: "download", Category: "permanent"},
		{Path: "c.txt", Direction: "upload", Category: "permanent"},
	}

	result := filterFailures(rows, "upload", "permanent")
	require.Len(t, result, 1)
	assert.Equal(t, "c.txt", result[0].Path)
}

func TestFilterFailures_NoMatch(t *testing.T) {
	t.Parallel()

	rows := []sync.SyncFailureRow{
		{Path: "a.txt", Direction: "upload", Category: "transient"},
	}

	result := filterFailures(rows, "delete", "")
	assert.Empty(t, result)
}
