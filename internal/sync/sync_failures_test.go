package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// newTestSyncStoreForIssues creates a test SyncStore with a fixed nowFunc.
func newTestSyncStoreForIssues(t *testing.T) (*SyncStore, time.Time) {
	t.Helper()

	fixedTime := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return fixedTime }

	return mgr, fixedTime
}

func TestRecordLocalIssue_NewEntry(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "docs/report.xlsx", driveid.ID{}, "upload", "upload_failed", "connection reset", 500, 1024, "abc123", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	row := issues[0]
	assert.Equal(t, "docs/report.xlsx", row.Path)
	assert.Equal(t, "upload_failed", row.IssueType)
	assert.Equal(t, "transient", row.Category)
	assert.Equal(t, 1, row.FailureCount)
	assert.Equal(t, "connection reset", row.LastError)
	assert.Equal(t, 500, row.HTTPStatus)
	assert.Equal(t, int64(1024), row.FileSize)
	assert.Equal(t, "abc123", row.LocalHash)
	assert.Equal(t, fixedTime.UnixNano(), row.FirstSeenAt)
	assert.Equal(t, fixedTime.UnixNano(), row.LastSeenAt)
}

func TestRecordLocalIssue_RepeatFailure(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	// Advance time and record again.
	laterTime := fixedTime.Add(5 * time.Minute)
	mgr.nowFunc = func() time.Time { return laterTime }

	err = mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "connection reset", 503, 0, "", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	row := issues[0]
	assert.Equal(t, 2, row.FailureCount)
	assert.Equal(t, fixedTime.UnixNano(), row.FirstSeenAt)
	assert.Equal(t, laterTime.UnixNano(), row.LastSeenAt)
	assert.Equal(t, "connection reset", row.LastError)
	assert.Equal(t, 503, row.HTTPStatus)
}

func TestRecordLocalIssue_PermanentStatus(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "CON.txt", driveid.ID{}, "upload", "invalid_filename", "reserved name", 0, 0, "", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "permanent", issues[0].Category)
}

func TestRecordLocalIssue_TransientStatus(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "big.zip", driveid.ID{}, "upload", "upload_failed", "server error", 500, 0, "", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "transient", issues[0].Category)
}

func TestListLocalIssues_Empty(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestListLocalIssues_Multiple(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Insert 3 issues with different last_seen_at times.
	for i, p := range []string{"a.txt", "b.txt", "c.txt"} {
		mgr.nowFunc = func() time.Time { return fixedTime.Add(time.Duration(i) * time.Minute) }
		err := mgr.RecordSyncFailure(ctx, p, driveid.ID{}, "upload", "upload_failed", "err", 0, 0, "", "")
		require.NoError(t, err)
	}

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 3)
	// Ordered by last_seen_at DESC.
	assert.Equal(t, "c.txt", issues[0].Path)
	assert.Equal(t, "b.txt", issues[1].Path)
	assert.Equal(t, "a.txt", issues[2].Path)
}

func TestClearLocalIssue(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "err", 0, 0, "", "")
	require.NoError(t, err)

	err = mgr.ClearSyncFailure(ctx, "file.txt", driveid.ID{})
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestClearResolvedLocalIssues(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Insert 3 issues.
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		err := mgr.RecordSyncFailure(ctx, p, driveid.ID{}, "upload", "upload_failed", "err", 0, 0, "", "")
		require.NoError(t, err)
	}

	// Manually set one to permanent (ClearResolvedSyncFailures removes permanent rows).
	_, err := mgr.db.ExecContext(ctx,
		`UPDATE sync_failures SET category = 'permanent' WHERE path = 'b.txt'`)
	require.NoError(t, err)

	// Clear resolved (permanent).
	err = mgr.ClearResolvedSyncFailures(ctx)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 2)

	paths := []string{issues[0].Path, issues[1].Path}
	assert.Contains(t, paths, "a.txt")
	assert.Contains(t, paths, "c.txt")
}

func TestCommitOutcome_UploadSuccess_ClearsLocalIssue(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record an issue.
	err := mgr.RecordSyncFailure(ctx, "docs/file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	// Commit a successful upload outcome.
	outcome := &Outcome{
		Action:     ActionUpload,
		Success:    true,
		Path:       "docs/file.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-1",
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		ItemType:   ItemTypeFile,
	}
	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	// Verify issue is gone.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestCommitOutcome_UploadSuccess_NoIssue_NoError(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Commit a successful upload without any prior issue.
	outcome := &Outcome{
		Action:     ActionUpload,
		Success:    true,
		Path:       "docs/file.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-1",
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		ItemType:   ItemTypeFile,
	}
	err := mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)
}

func TestCommitOutcome_DownloadSuccess_DoesNotClearLocalIssue(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record an issue.
	err := mgr.RecordSyncFailure(ctx, "docs/file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	// Insert a remote_state row so the download outcome has something to update.
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO remote_state
			(drive_id, item_id, path, parent_id, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		driveid.New(testDriveID).String(), "item-1", "docs/file.txt", "file", "file", statusDownloading, 1)
	require.NoError(t, err)

	// Commit a successful download outcome.
	outcome := &Outcome{
		Action:     ActionDownload,
		Success:    true,
		Path:       "docs/file.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-1",
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		ItemType:   ItemTypeFile,
	}
	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	// Verify issue still exists.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, issues, 1)
}

func TestLocalIssueSyncStatus(t *testing.T) {
	tests := []struct {
		issueType string
		want      bool // true = permanent
	}{
		{IssueInvalidFilename, true},
		{IssuePathTooLong, true},
		{IssueFileTooLarge, true},
		{"upload_failed", false},
		{"permission_denied", false},
		{"quota_exceeded", false},
		{"locked", false},
		{"sharepoint_restriction", false},
	}

	for _, tt := range tests {
		t.Run(tt.issueType, func(t *testing.T) {
			assert.Equal(t, tt.want, isPermanentIssue(tt.issueType))
		})
	}
}

func TestRecordLocalIssue_TransientSetsNextRetryAt(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	// next_retry_at should be set (non-zero) and after fixedTime.
	assert.Greater(t, issues[0].NextRetryAt, fixedTime.UnixNano(),
		"transient issue should have next_retry_at in the future")
}

func TestRecordLocalIssue_PermanentNoRetryAt(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	err := mgr.RecordSyncFailure(ctx, "CON", driveid.ID{}, "upload", "invalid_filename", "reserved name", 0, 0, "", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, int64(0), issues[0].NextRetryAt,
		"permanent issue should have no next_retry_at")
}

func TestRecordLocalIssue_RepeatBackoffIncreases(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// First failure.
	err := mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	firstRetry := issues[0].NextRetryAt

	// Advance time and record second failure.
	laterTime := fixedTime.Add(5 * time.Minute)
	mgr.nowFunc = func() time.Time { return laterTime }

	err = mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "timeout again", 0, 0, "", "")
	require.NoError(t, err)

	issues, err = mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	// Second retry should be further in the future than the first.
	assert.Greater(t, issues[0].NextRetryAt, firstRetry,
		"backoff should increase on repeated failures")
}

func TestListLocalIssuesForRetry(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record a transient issue (will have next_retry_at set).
	err := mgr.RecordSyncFailure(ctx, "retry.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	// Record a permanent issue (should not appear in retry list).
	err = mgr.RecordSyncFailure(ctx, "CON", driveid.ID{}, "upload", "invalid_filename", "reserved", 0, 0, "", "")
	require.NoError(t, err)

	// Query at a time before next_retry_at — should be empty.
	rows, err := mgr.ListSyncFailuresForRetry(ctx, fixedTime)
	require.NoError(t, err)
	assert.Empty(t, rows, "should not return issues before their retry time")

	// Query at a time after next_retry_at — should return the transient issue.
	futureTime := fixedTime.Add(10 * time.Minute)
	rows, err = mgr.ListSyncFailuresForRetry(ctx, futureTime)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "retry.txt", rows[0].Path)
}

func TestEarliestLocalIssueRetryAt(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// No issues — zero time.
	earliest, err := mgr.EarliestSyncFailureRetryAt(ctx, fixedTime)
	require.NoError(t, err)
	assert.True(t, earliest.IsZero())

	// Record a transient issue.
	err = mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	// Query from fixedTime — should return the next_retry_at (which is in the future).
	earliest, err = mgr.EarliestSyncFailureRetryAt(ctx, fixedTime)
	require.NoError(t, err)
	assert.False(t, earliest.IsZero(), "should return a future retry time")
	assert.True(t, earliest.After(fixedTime))
}

func TestMarkLocalIssuePermanent(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record a transient issue.
	err := mgr.RecordSyncFailure(ctx, "file.txt", driveid.ID{}, "upload", "upload_failed", "timeout", 0, 0, "", "")
	require.NoError(t, err)

	// Verify it's transient.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "transient", issues[0].Category)

	// Mark as permanent.
	err = mgr.MarkSyncFailurePermanent(ctx, "file.txt", driveid.ID{})
	require.NoError(t, err)

	// Verify category changed.
	issues, err = mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "permanent", issues[0].Category)
	assert.Equal(t, int64(0), issues[0].NextRetryAt, "permanent issues should have no retry time")

	// Should not appear in retry list.
	rows, err := mgr.ListSyncFailuresForRetry(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestListLocalIssuesByType(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record issues of different types.
	require.NoError(t, mgr.RecordSyncFailure(ctx, "a.txt", driveid.ID{}, "upload", "upload_failed", "err", 0, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "b.txt", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "c.txt", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "d.txt", driveid.ID{}, "upload", "upload_failed", "err", 0, 0, "", ""))

	// Query by permission_denied type.
	issues, err := mgr.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, IssuePermissionDenied, issues[0].IssueType)
	assert.Equal(t, IssuePermissionDenied, issues[1].IssueType)

	// Query by upload_failed type.
	issues, err = mgr.ListSyncFailuresByIssueType(ctx, "upload_failed")
	require.NoError(t, err)
	require.Len(t, issues, 2)

	// Query by nonexistent type.
	issues, err = mgr.ListSyncFailuresByIssueType(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestClearLocalIssuesByPrefix(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record issues at various paths.
	require.NoError(t, mgr.RecordSyncFailure(ctx, "shared", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "shared/file.txt", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "shared/sub/file.txt", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "other/file.txt", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))

	// Clear by prefix "shared".
	err := mgr.ClearSyncFailuresByPrefix(ctx, "shared", IssuePermissionDenied)
	require.NoError(t, err)

	// Only "other/file.txt" should remain.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "other/file.txt", issues[0].Path)
}

func TestClearLocalIssuesByPrefix_TypeFiltering(t *testing.T) {
	mgr, _ := newTestSyncStoreForIssues(t)
	ctx := context.Background()

	// Record a permission_denied and an upload_failed at the same prefix.
	require.NoError(t, mgr.RecordSyncFailure(ctx, "shared/a.txt", driveid.ID{}, "upload", IssuePermissionDenied, "read-only", 403, 0, "", ""))
	require.NoError(t, mgr.RecordSyncFailure(ctx, "shared/b.txt", driveid.ID{}, "upload", "upload_failed", "err", 500, 0, "", ""))

	// Clear permission_denied only.
	err := mgr.ClearSyncFailuresByPrefix(ctx, "shared", IssuePermissionDenied)
	require.NoError(t, err)

	// upload_failed should remain.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "upload_failed", issues[0].IssueType)
}
