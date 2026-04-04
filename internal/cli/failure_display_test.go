package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.3.8, R-6.6.11
func TestPrintGroupedFailures_UsesSharedSummaryDescriptors(t *testing.T) {
	t.Parallel()

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummarySyncFailure,
			PrimaryIssueType: "",
			Paths:            []string{"/mystery.txt"},
			Count:            1,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedFailures(&buf, groups, false))

	output := buf.String()
	assert.Contains(t, output, "SYNC FAILURE")
	assert.Contains(t, output, "/mystery.txt")
}

// Validates: R-2.3.7
func TestPrintGroupedFailures_VerboseShowsAll(t *testing.T) {
	t.Parallel()

	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("/docs/file%02d.docx", i))
	}

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummaryQuotaExceeded,
			PrimaryIssueType: synctypes.IssueQuotaExceeded,
			Paths:            paths,
			Count:            12,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedFailures(&buf, groups, true))
	output := buf.String()

	for _, path := range paths {
		assert.Contains(t, output, path)
	}
	assert.NotContains(t, output, "... and")
}

// Validates: R-2.3.7
func TestPrintGroupedFailures_NonVerboseTruncates(t *testing.T) {
	t.Parallel()

	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("/docs/file%02d.docx", i))
	}

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummaryQuotaExceeded,
			PrimaryIssueType: synctypes.IssueQuotaExceeded,
			Paths:            paths,
			Count:            12,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedFailures(&buf, groups, false))
	output := buf.String()

	assert.Contains(t, output, paths[0])
	assert.Contains(t, output, paths[4])
	assert.NotContains(t, output, paths[5])
	assert.Contains(t, output, "... and 7 more")
}

func TestPrintGroupedFailures_ShowsScopeLabel(t *testing.T) {
	t.Parallel()

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummaryQuotaExceeded,
			PrimaryIssueType: synctypes.IssueQuotaExceeded,
			ScopeLabel:       "Team Docs",
			Paths:            []string{"/Team Docs/a.txt"},
			Count:            1,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedFailures(&buf, groups, false))
	assert.Contains(t, buf.String(), "Scope: Team Docs")
}

func TestPrintGroupedFailures_NoScopeLineWhenEmpty(t *testing.T) {
	t.Parallel()

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummaryInvalidFilename,
			PrimaryIssueType: synctypes.IssueInvalidFilename,
			Paths:            []string{"/bad:file.txt"},
			Count:            1,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedFailures(&buf, groups, false))
	assert.NotContains(t, buf.String(), "Scope:")
}

// Validates: R-2.3.10, R-2.10.45
func TestPrintGroupedFailures_ScopeOnlyIssueOmitsPathSection(t *testing.T) {
	t.Parallel()

	groups := []syncstore.IssueGroupSnapshot{
		{
			SummaryKey:       synctypes.SummaryAuthenticationRequired,
			PrimaryIssueType: synctypes.IssueUnauthorized,
			ScopeLabel:       "your OneDrive account authorization",
			Count:            1,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedFailures(&buf, groups, false))

	output := buf.String()
	assert.Contains(t, output, "AUTHENTICATION REQUIRED")
	assert.Contains(t, output, "Scope: your OneDrive account authorization")
	assert.NotContains(t, output, "  /")
}

// Validates: R-2.3.9
func TestPrintGroupedIssuesJSON_StructuredOutput(t *testing.T) {
	t.Parallel()

	snapshot := syncstore.IssuesSnapshot{
		Conflicts: []synctypes.ConflictRecord{
			{
				ID:           "abc123",
				Path:         "/conflict.txt",
				ConflictType: "content",
				DetectedAt:   1000000000,
			},
		},
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryQuotaExceeded,
				PrimaryIssueType: synctypes.IssueQuotaExceeded,
				ScopeLabel:       "your OneDrive storage",
				Paths:            []string{"/a.txt", "/b.txt"},
				Count:            2,
			},
		},
		HeldDeletes: []syncstore.HeldDeleteSnapshot{
			{Path: "/deleted.txt", LastSeenAt: 2000000000},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesJSON(&buf, snapshot))

	var out issuesOutputJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))

	assert.Len(t, out.Conflicts, 1)
	assert.Equal(t, "abc123", out.Conflicts[0].ID)

	require.Len(t, out.FailureGroups, 1)
	assert.Equal(t, "QUOTA EXCEEDED", out.FailureGroups[0].Title)
	assert.Equal(t, 2, out.FailureGroups[0].Count)
	assert.Equal(t, "your OneDrive storage", out.FailureGroups[0].Scope)
	assert.Len(t, out.FailureGroups[0].Paths, 2)

	require.Len(t, out.HeldDeletes, 1)
	assert.Equal(t, "/deleted.txt", out.HeldDeletes[0].Path)
}

func TestPrintGroupedIssuesText_AllSections(t *testing.T) {
	t.Parallel()

	snapshot := syncstore.IssuesSnapshot{
		Conflicts: []synctypes.ConflictRecord{
			{ID: "abc123", Path: "/conflict.txt", ConflictType: "content", DetectedAt: 1000000000},
		},
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryQuotaExceeded,
				PrimaryIssueType: synctypes.IssueQuotaExceeded,
				Paths:            []string{"/a.txt"},
				Count:            1,
			},
		},
		HeldDeletes: []syncstore.HeldDeleteSnapshot{
			{Path: "/deleted.txt", LastSeenAt: 2000000000},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, snapshot, false, false))
	output := buf.String()

	assert.Contains(t, output, "CONFLICTS")
	assert.Contains(t, output, "HELD DELETES")
	assert.Contains(t, output, "QUOTA EXCEEDED")
}

func TestPrintPendingRetries(t *testing.T) {
	t.Parallel()

	groups := []syncstore.PendingRetrySnapshot{
		{
			ScopeKey:     synctypes.SKThrottleAccount(),
			ScopeLabel:   "your OneDrive account rate limit",
			Count:        8,
			EarliestNext: time.Now().Add(2*time.Minute + 30*time.Second),
		},
		{
			ScopeKey:     synctypes.SKQuotaOwn(),
			ScopeLabel:   "your OneDrive storage",
			Count:        4,
			EarliestNext: time.Now().Add(4*time.Minute + 15*time.Second),
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printPendingRetries(&buf, groups))
	output := buf.String()

	assert.Contains(t, output, "PENDING RETRIES (12 items)")
	assert.Contains(t, output, "8 items")
	assert.Contains(t, output, "4 items")
}

func TestPrintHeldDeletesGrouped_SmallCount(t *testing.T) {
	t.Parallel()

	var heldDeletes []syncstore.HeldDeleteSnapshot
	for i := range 5 {
		heldDeletes = append(heldDeletes, syncstore.HeldDeleteSnapshot{
			Path:       fmt.Sprintf("dir/file%d.txt", i),
			LastSeenAt: 1700000000000000000,
		})
	}

	var buf bytes.Buffer
	require.NoError(t, printHeldDeletesGrouped(&buf, heldDeletes, false))
	output := buf.String()

	assert.Contains(t, output, "HELD DELETES (5 files")
	assert.Contains(t, output, "dir/file0.txt")
}

func TestPrintHeldDeletesGrouped_LargeCount(t *testing.T) {
	t.Parallel()

	var heldDeletes []syncstore.HeldDeleteSnapshot
	for i := range 30 {
		dir := "Documents/Archive"
		if i >= 20 {
			dir = "Photos/2024"
		}

		heldDeletes = append(heldDeletes, syncstore.HeldDeleteSnapshot{
			Path:       fmt.Sprintf("%s/file%d.txt", dir, i),
			LastSeenAt: 1700000000000000000,
		})
	}

	var buf bytes.Buffer
	require.NoError(t, printHeldDeletesGrouped(&buf, heldDeletes, false))
	output := buf.String()

	assert.Contains(t, output, "HELD DELETES (30 files")
	assert.Contains(t, output, "Documents/Archive/")
	assert.Contains(t, output, "Photos/2024/")
	assert.NotContains(t, output, "file0.txt")
	assert.Contains(t, output, "--verbose")
}
