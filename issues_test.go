package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestNewIssuesCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newIssuesCmd()
	assert.Equal(t, "issues", cmd.Use)
	assert.NotNil(t, cmd.RunE)

	// Has a "clear" subcommand.
	clearCmd, _, err := cmd.Find([]string{"clear"})
	require.NoError(t, err)
	assert.Equal(t, "clear [path]", clearCmd.Use)
	assert.NotNil(t, clearCmd.Flags().Lookup("all"))
}

func TestToIssueJSON(t *testing.T) {
	t.Parallel()

	row := &sync.LocalIssueRow{
		Path:         "docs/CON",
		IssueType:    "invalid_filename",
		SyncStatus:   "permanently_failed",
		FailureCount: 1,
		LastError:    "file name is not valid for OneDrive: CON",
		HTTPStatus:   0,
		FileSize:     1024,
		FirstSeenAt:  1700000000000000000,
		LastSeenAt:   1700000001000000000,
	}

	j := toIssueJSON(row)
	assert.Equal(t, "docs/CON", j.Path)
	assert.Equal(t, "invalid_filename", j.IssueType)
	assert.Equal(t, "permanently_failed", j.SyncStatus)
	assert.Equal(t, 1, j.FailureCount)
	assert.Equal(t, "file name is not valid for OneDrive: CON", j.LastError)
	assert.Equal(t, int64(1024), j.FileSize)
	assert.NotEmpty(t, j.FirstSeenAt)
	assert.NotEmpty(t, j.LastSeenAt)
}

func TestPrintIssuesJSON_EmptyList(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printIssuesJSON(&buf, nil)
	require.NoError(t, err)

	var result []issueJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result)
}

func TestPrintIssuesJSON_WithIssues(t *testing.T) {
	t.Parallel()

	issues := []sync.LocalIssueRow{
		{
			Path:         "docs/CON",
			IssueType:    "invalid_filename",
			SyncStatus:   "permanently_failed",
			FailureCount: 1,
			LastError:    "reserved name",
			FirstSeenAt:  1700000000000000000,
			LastSeenAt:   1700000000000000000,
		},
		{
			Path:         "data/huge.bin",
			IssueType:    "file_too_large",
			SyncStatus:   "permanently_failed",
			FailureCount: 1,
			LastError:    "exceeds 250 GB",
			FileSize:     300 * 1024 * 1024 * 1024,
			FirstSeenAt:  1700000001000000000,
			LastSeenAt:   1700000001000000000,
		},
	}

	var buf bytes.Buffer
	err := printIssuesJSON(&buf, issues)
	require.NoError(t, err)

	var result []issueJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result, 2)
	assert.Equal(t, "docs/CON", result[0].Path)
	assert.Equal(t, "invalid_filename", result[0].IssueType)
	assert.Equal(t, "file_too_large", result[1].IssueType)
	assert.Equal(t, int64(300*1024*1024*1024), result[1].FileSize)
}

func TestPrintIssuesTable(t *testing.T) {
	t.Parallel()

	issues := []sync.LocalIssueRow{
		{
			Path:         "docs/CON",
			IssueType:    "invalid_filename",
			SyncStatus:   "permanently_failed",
			FailureCount: 1,
			LastError:    "reserved name",
			LastSeenAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printIssuesTable(&buf, issues)

	output := buf.String()
	assert.Contains(t, output, "PATH")
	assert.Contains(t, output, "TYPE")
	assert.Contains(t, output, "STATUS")
	assert.Contains(t, output, "docs/CON")
	assert.Contains(t, output, "invalid_filename")
	assert.Contains(t, output, "permanently_failed")
}

func TestPrintIssuesTable_TruncatesLongErrors(t *testing.T) {
	t.Parallel()

	longErr := "this is a very long error message that should be truncated to sixty characters total for table display purposes"
	issues := []sync.LocalIssueRow{
		{
			Path:         "file.txt",
			IssueType:    "upload_failed",
			SyncStatus:   "upload_failed",
			FailureCount: 3,
			LastError:    longErr,
			LastSeenAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printIssuesTable(&buf, issues)

	output := buf.String()
	assert.Contains(t, output, "...")
	assert.NotContains(t, output, longErr) // full message should not appear
}
