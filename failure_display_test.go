package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-2.3.7
func TestGroupFailures_MoreThanTen(t *testing.T) {
	t.Parallel()

	var failures []sync.SyncFailureRow
	for i := range 15 {
		failures = append(failures, sync.SyncFailureRow{
			Path:      fmt.Sprintf("/docs/file%02d.docx", i),
			IssueType: sync.IssueQuotaExceeded,
			ScopeKey:  "quota:own",
			Category:  "actionable",
		})
	}

	groups, heldDeletes := groupFailures(failures, nil)
	require.Len(t, groups, 1)
	assert.Empty(t, heldDeletes)
	assert.Equal(t, 15, groups[0].Count)
	assert.Equal(t, sync.IssueQuotaExceeded, groups[0].IssueType)
	assert.Len(t, groups[0].Paths, 15)
}

// Validates: R-2.3.8
func TestGroupFailures_HumanReadableMessages(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{Path: "/a.txt", IssueType: sync.IssueQuotaExceeded, ScopeKey: "quota:own", Category: "actionable"},
		{Path: "/b.txt", IssueType: sync.IssuePermissionDenied, ScopeKey: "", Category: "actionable"},
	}

	groups, _ := groupFailures(failures, nil)
	require.Len(t, groups, 2)

	// Each group has a human-readable message.
	for _, g := range groups {
		assert.NotEmpty(t, g.Message.Title)
		assert.NotEmpty(t, g.Message.Reason)
		assert.NotEmpty(t, g.Message.Action)
	}
}

// Validates: R-2.3.7
func TestGroupFailures_SeparatesHeldDeletes(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{Path: "/a.txt", IssueType: sync.IssueQuotaExceeded, Category: "actionable"},
		{Path: "/b.txt", IssueType: sync.IssueBigDeleteHeld, Category: "actionable"},
		{Path: "/c.txt", IssueType: sync.IssueBigDeleteHeld, Category: "actionable"},
	}

	groups, heldDeletes := groupFailures(failures, nil)
	assert.Len(t, groups, 1)
	assert.Len(t, heldDeletes, 2)
}

// Validates: R-2.10.22
func TestGroupFailures_ShortcutScopeKeyHumanized(t *testing.T) {
	t.Parallel()

	shortcuts := []sync.Shortcut{
		{
			RemoteDrive: "driveAAA",
			RemoteItem:  "itemBBB",
			LocalPath:   "Team Docs",
			Observation: sync.ObservationDelta,
		},
	}

	failures := []sync.SyncFailureRow{
		{
			Path:      "/Team Docs/report.docx",
			IssueType: sync.IssueQuotaExceeded,
			ScopeKey:  "quota:shortcut:driveAAA:itemBBB",
			Category:  "actionable",
		},
	}

	groups, _ := groupFailures(failures, shortcuts)
	require.Len(t, groups, 1)
	assert.Equal(t, "Team Docs", groups[0].ScopeKey)
}

// Validates: R-2.10.4
func TestGroupFailures_GroupsByScopeKey(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{Path: "/a.txt", IssueType: sync.IssueQuotaExceeded, ScopeKey: "quota:own", Category: "actionable"},
		{Path: "/b.txt", IssueType: sync.IssueQuotaExceeded, ScopeKey: "quota:shortcut:drive1:item1", Category: "actionable"},
	}

	groups, _ := groupFailures(failures, nil)

	// Same issue type but different scope → two separate groups.
	assert.Len(t, groups, 2)
}

func TestGroupFailures_SortedLargestFirst(t *testing.T) {
	t.Parallel()

	failures := []sync.SyncFailureRow{
		{Path: "/a.txt", IssueType: sync.IssuePermissionDenied, Category: "actionable"},
		{Path: "/b.txt", IssueType: sync.IssueQuotaExceeded, ScopeKey: "quota:own", Category: "actionable"},
		{Path: "/c.txt", IssueType: sync.IssueQuotaExceeded, ScopeKey: "quota:own", Category: "actionable"},
		{Path: "/d.txt", IssueType: sync.IssueQuotaExceeded, ScopeKey: "quota:own", Category: "actionable"},
	}

	groups, _ := groupFailures(failures, nil)
	require.Len(t, groups, 2)
	assert.Equal(t, 3, groups[0].Count)
	assert.Equal(t, 1, groups[1].Count)
}

func TestGroupFailures_Empty(t *testing.T) {
	t.Parallel()

	groups, heldDeletes := groupFailures(nil, nil)
	assert.Empty(t, groups)
	assert.Empty(t, heldDeletes)
}

// Validates: R-2.3.7
func TestPrintGroupedFailures_VerboseShowsAll(t *testing.T) {
	t.Parallel()

	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("/docs/file%02d.docx", i))
	}

	groups := []failureGroup{
		{
			IssueType: sync.IssueQuotaExceeded,
			Message:   sync.MessageForIssueType(sync.IssueQuotaExceeded),
			Paths:     paths,
			Count:     12,
		},
	}

	var buf bytes.Buffer
	printGroupedFailures(&buf, groups, true)
	output := buf.String()

	// All 12 paths should be present.
	for _, p := range paths {
		assert.Contains(t, output, p)
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

	groups := []failureGroup{
		{
			IssueType: sync.IssueQuotaExceeded,
			Message:   sync.MessageForIssueType(sync.IssueQuotaExceeded),
			Paths:     paths,
			Count:     12,
		},
	}

	var buf bytes.Buffer
	printGroupedFailures(&buf, groups, false)
	output := buf.String()

	// Only first 5 should be shown.
	assert.Contains(t, output, paths[0])
	assert.Contains(t, output, paths[4])
	assert.NotContains(t, output, paths[5])
	assert.Contains(t, output, "... and 7 more")
}

func TestPrintGroupedFailures_ShowsScopeKey(t *testing.T) {
	t.Parallel()

	groups := []failureGroup{
		{
			IssueType: sync.IssueQuotaExceeded,
			ScopeKey:  "Team Docs",
			Message:   sync.MessageForIssueType(sync.IssueQuotaExceeded),
			Paths:     []string{"/Team Docs/a.txt"},
			Count:     1,
		},
	}

	var buf bytes.Buffer
	printGroupedFailures(&buf, groups, false)
	assert.Contains(t, buf.String(), "Scope: Team Docs")
}

func TestPrintGroupedFailures_NoScopeLineWhenEmpty(t *testing.T) {
	t.Parallel()

	groups := []failureGroup{
		{
			IssueType: sync.IssueInvalidFilename,
			ScopeKey:  "",
			Message:   sync.MessageForIssueType(sync.IssueInvalidFilename),
			Paths:     []string{"/bad:file.txt"},
			Count:     1,
		},
	}

	var buf bytes.Buffer
	printGroupedFailures(&buf, groups, false)
	assert.NotContains(t, buf.String(), "Scope:")
}

// Validates: R-2.3.9
func TestPrintGroupedIssuesJSON_StructuredOutput(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "abc123",
			Path:         "/conflict.txt",
			ConflictType: "content",
			DetectedAt:   1000000000,
		},
	}

	groups := []failureGroup{
		{
			IssueType: sync.IssueQuotaExceeded,
			ScopeKey:  "your OneDrive storage",
			Message:   sync.MessageForIssueType(sync.IssueQuotaExceeded),
			Paths:     []string{"/a.txt", "/b.txt"},
			Count:     2,
		},
	}

	heldDeletes := []sync.SyncFailureRow{
		{Path: "/deleted.txt", LastSeenAt: 2000000000},
	}

	var buf bytes.Buffer
	err := printGroupedIssuesJSON(&buf, conflicts, groups, heldDeletes)
	require.NoError(t, err)

	var out issuesOutputJSON
	err = json.Unmarshal(buf.Bytes(), &out)
	require.NoError(t, err)

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

	conflicts := []sync.ConflictRecord{
		{ID: "abc123", Path: "/conflict.txt", ConflictType: "content", DetectedAt: 1000000000},
	}

	groups := []failureGroup{
		{
			IssueType: sync.IssueQuotaExceeded,
			Message:   sync.MessageForIssueType(sync.IssueQuotaExceeded),
			Paths:     []string{"/a.txt"},
			Count:     1,
		},
	}

	heldDeletes := []sync.SyncFailureRow{
		{Path: "/deleted.txt", LastSeenAt: 2000000000},
	}

	var buf bytes.Buffer
	printGroupedIssuesText(&buf, conflicts, groups, heldDeletes, nil, nil, false, false)
	output := buf.String()

	assert.True(t, strings.Contains(output, "CONFLICTS"))
	assert.True(t, strings.Contains(output, "HELD DELETES"))
	assert.True(t, strings.Contains(output, "QUOTA EXCEEDED"))
}

func TestPrintPendingRetries(t *testing.T) {
	t.Parallel()

	groups := []sync.PendingRetryGroup{
		{ScopeKey: "throttle:account", Count: 8, EarliestNext: time.Now().Add(2*time.Minute + 30*time.Second)},
		{ScopeKey: "quota:own", Count: 4, EarliestNext: time.Now().Add(4*time.Minute + 15*time.Second)},
	}

	var buf bytes.Buffer
	printPendingRetries(&buf, groups, nil)
	output := buf.String()

	assert.Contains(t, output, "PENDING RETRIES (12 items)")
	assert.Contains(t, output, "8 items")
	assert.Contains(t, output, "4 items")
}

func TestPrintHeldDeletesGrouped_SmallCount(t *testing.T) {
	t.Parallel()

	// Under threshold: should show individual paths.
	var heldDeletes []sync.SyncFailureRow
	for i := range 5 {
		heldDeletes = append(heldDeletes, sync.SyncFailureRow{
			Path:       fmt.Sprintf("dir/file%d.txt", i),
			LastSeenAt: 1700000000000000000,
		})
	}

	var buf bytes.Buffer
	printHeldDeletesGrouped(&buf, heldDeletes, false)
	output := buf.String()

	assert.Contains(t, output, "HELD DELETES (5 files")
	assert.Contains(t, output, "dir/file0.txt")
}

func TestPrintHeldDeletesGrouped_LargeCount(t *testing.T) {
	t.Parallel()

	// Over threshold: should group by directory.
	var heldDeletes []sync.SyncFailureRow
	for i := range 30 {
		dir := "Documents/Archive"
		if i >= 20 {
			dir = "Photos/2024"
		}

		heldDeletes = append(heldDeletes, sync.SyncFailureRow{
			Path:       fmt.Sprintf("%s/file%d.txt", dir, i),
			LastSeenAt: 1700000000000000000,
		})
	}

	var buf bytes.Buffer
	printHeldDeletesGrouped(&buf, heldDeletes, false)
	output := buf.String()

	assert.Contains(t, output, "HELD DELETES (30 files")
	assert.Contains(t, output, "Documents/Archive/")
	assert.Contains(t, output, "Photos/2024/")
	// Should NOT show individual files.
	assert.NotContains(t, output, "file0.txt")
	assert.Contains(t, output, "--verbose")
}

func TestPrintHeldDeletesGrouped_LargeCountVerbose(t *testing.T) {
	t.Parallel()

	// Over threshold but verbose: should show individual paths.
	var heldDeletes []sync.SyncFailureRow
	for i := range 25 {
		heldDeletes = append(heldDeletes, sync.SyncFailureRow{
			Path:       fmt.Sprintf("dir/file%d.txt", i),
			LastSeenAt: 1700000000000000000,
		})
	}

	var buf bytes.Buffer
	printHeldDeletesGrouped(&buf, heldDeletes, true)
	output := buf.String()

	// Verbose mode should show individual paths.
	assert.Contains(t, output, "dir/file0.txt")
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "now", formatDuration(0))
	assert.Equal(t, "now", formatDuration(500*time.Millisecond))
	assert.Equal(t, "30s", formatDuration(30*time.Second))
	assert.Equal(t, "2m30s", formatDuration(2*time.Minute+30*time.Second))
	assert.Equal(t, "5m", formatDuration(5*time.Minute))
	assert.Equal(t, "1h30m", formatDuration(90*time.Minute))
	assert.Equal(t, "2h", formatDuration(2*time.Hour))
}
