package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func goldenIssuesSnapshot() syncstore.IssuesSnapshot {
	return syncstore.IssuesSnapshot{
		Conflicts: []synctypes.ConflictRecord{
			{
				ID:           "conflict-0001",
				Path:         "/docs/report.txt",
				ConflictType: synctypes.ConflictEditEdit,
				DetectedAt:   time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
		Groups: []syncstore.IssueGroupSnapshot{
			{
				SummaryKey:       synctypes.SummaryAuthenticationRequired,
				PrimaryIssueType: synctypes.IssueUnauthorized,
				ScopeKey:         synctypes.SKAuthAccount(),
				ScopeLabel:       "your OneDrive account authorization",
				Count:            1,
			},
			{
				SummaryKey:       synctypes.SummarySharedFolderWritesBlocked,
				PrimaryIssueType: synctypes.IssueSharedFolderBlocked,
				ScopeKey:         synctypes.SKPermRemote("Shared/Docs"),
				ScopeLabel:       "Shared/Docs",
				Paths:            []string{"/Shared/Docs/a.txt", "/Shared/Docs/b.txt"},
				Count:            2,
			},
			{
				SummaryKey:       synctypes.SummaryInvalidFilename,
				PrimaryIssueType: synctypes.IssueInvalidFilename,
				Paths:            []string{"docs/CON", "docs/NUL.txt"},
				Count:            2,
			},
		},
		HeldDeletes: []syncstore.HeldDeleteSnapshot{
			{Path: "/archive/old-a.txt", LastSeenAt: time.Date(2026, 4, 3, 9, 0, 0, 0, time.UTC).UnixNano()},
			{Path: "/archive/old-b.txt", LastSeenAt: time.Date(2026, 4, 3, 9, 5, 0, 0, time.UTC).UnixNano()},
		},
	}
}

// Validates: R-2.3.7, R-2.3.8, R-2.3.10
func TestIssuesOutputGoldenText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesText(&buf, goldenIssuesSnapshot(), false, false))
	assertGoldenFile(t, "issues_text.golden", buf.Bytes())
}

// Validates: R-2.3.10
func TestIssuesOutputGoldenJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printGroupedIssuesJSON(&buf, goldenIssuesSnapshot()))
	assertGoldenFile(t, "issues_json.golden", buf.Bytes())
}
