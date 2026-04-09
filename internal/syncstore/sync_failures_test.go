package syncstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// newTestSyncStoreForFailures creates a test SyncStore with a fixed nowFunc.
func newTestSyncStoreForFailures(t *testing.T) (*SyncStore, time.Time) {
	t.Helper()

	fixedTime := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	return mgr, fixedTime
}

func TestRecordFailure_RepeatFailure(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, nil)
	require.NoError(t, err)

	// Advance time and record again.
	laterTime := fixedTime.Add(5 * time.Minute)
	mgr.SetNowFunc(func() time.Time { return laterTime })

	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "file.txt",
		DriveID:    driveid.ID{},
		Direction:  synctypes.DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "connection reset",
		HTTPStatus: 503,
	}, nil)
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

func TestRecordFailure_PermanentStatus(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "CON.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "invalid_filename",
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.CategoryActionable, issues[0].Category)
}

func TestRecordFailure_TransientStatus(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "big.zip",
		DriveID:    driveid.ID{},
		Direction:  synctypes.DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "server error",
		HTTPStatus: 500,
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.CategoryTransient, issues[0].Category)
}

func TestRecordFailure_NormalizesDirectionFromActionType(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "shared/new-folder",
		DriveID:    driveid.ID{},
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionFolderCreate,
		IssueType:  "folder_create_failed",
		ErrMsg:     "permission denied",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.ActionFolderCreate, issues[0].ActionType)
	assert.Equal(t, synctypes.DirectionDownload, issues[0].Direction)
}

func TestListLocalIssues_Empty(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestListLocalIssues_Multiple(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Insert 3 issues with different last_seen_at times.
	for i, p := range []string{"a.txt", "b.txt", "c.txt"} {
		mgr.SetNowFunc(func() time.Time { return fixedTime.Add(time.Duration(i) * time.Minute) })
		err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      p,
			DriveID:   driveid.ID{},
			Direction: synctypes.DirectionUpload,
			IssueType: "upload_failed",
			ErrMsg:    "err",
		}, nil)
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

func TestTakeSyncFailure_ReturnsAndDeletesRow(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()
	driveID := driveid.New("drive-1")

	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "docs/report.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueServiceOutage,
		ErrMsg:     "server error",
		HTTPStatus: 503,
	}, func(_ int) time.Duration {
		return time.Minute
	}))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "docs/report.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueServiceOutage,
		ErrMsg:     "server error again",
		HTTPStatus: 503,
	}, func(_ int) time.Duration {
		return time.Minute
	}))

	row, found, err := mgr.TakeSyncFailure(ctx, "docs/report.txt", driveID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, "docs/report.txt", row.Path)
	assert.Equal(t, driveID, row.DriveID)
	assert.Equal(t, synctypes.CategoryTransient, row.Category)
	assert.Equal(t, synctypes.FailureRoleItem, row.Role)
	assert.Equal(t, synctypes.IssueServiceOutage, row.IssueType)
	assert.Equal(t, synctypes.ActionUpload, row.ActionType)
	assert.Equal(t, 2, row.FailureCount)

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "taken row should be deleted")
}

func TestTakeSyncFailure_MissingRow(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	row, found, err := mgr.TakeSyncFailure(ctx, "missing.txt", driveid.New("drive-1"))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, row)
}

func TestClearLocalIssue(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "err",
	}, nil)
	require.NoError(t, err)

	err = mgr.ClearSyncFailure(ctx, "file.txt", driveid.ID{})
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestClearResolvedLocalIssues(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Insert 3 issues.
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      p,
			DriveID:   driveid.ID{},
			Direction: synctypes.DirectionUpload,
			IssueType: "upload_failed",
			ErrMsg:    "err",
		}, nil)
		require.NoError(t, err)
	}

	// Manually set one to actionable (ClearActionableSyncFailures removes actionable rows).
	_, err := mgr.DB().ExecContext(ctx,
		`UPDATE sync_failures SET category = 'actionable' WHERE path = 'b.txt'`)
	require.NoError(t, err)

	// Clear resolved (actionable).
	err = mgr.ClearActionableSyncFailures(ctx)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 2)

	paths := []string{issues[0].Path, issues[1].Path}
	assert.Contains(t, paths, "a.txt")
	assert.Contains(t, paths, "c.txt")
}

// Store-level characterization: the engine owns failure lifecycle clearing.
func TestCommitOutcome_UploadSuccess_DoesNotClearSyncFailures(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record an issue.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "docs/file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, nil)
	require.NoError(t, err)

	// Commit a successful upload outcome.
	outcome := &synctypes.Outcome{
		Action:     synctypes.ActionUpload,
		Success:    true,
		Path:       "docs/file.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-1",
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		ItemType:   synctypes.ItemTypeFile,
	}
	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	// D-6: CommitOutcome does NOT clear sync_failures — that's the engine's
	// responsibility via clearFailureOnSuccess.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, issues, 1, "sync_failures should survive CommitOutcome")
}

func TestCommitOutcome_UploadSuccess_NoIssue_NoError(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Commit a successful upload without any prior issue.
	outcome := &synctypes.Outcome{
		Action:     synctypes.ActionUpload,
		Success:    true,
		Path:       "docs/file.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-1",
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		ItemType:   synctypes.ItemTypeFile,
	}
	err := mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)
}

// Validates: R-2.10.41, R-2.10.10
func TestCommitOutcome_DownloadSuccess_DoesNotClearSyncFailures(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record a sync failure for the file.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "docs/file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionDownload,
		IssueType: "download_failed",
		ErrMsg:    "timeout",
	}, nil)
	require.NoError(t, err)

	// Insert a remote_state row so the download outcome has something to update.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state
			(drive_id, item_id, path, parent_id, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		driveid.New(testDriveID).String(), "item-1", "docs/file.txt", "file", "file", synctypes.SyncStatusDownloading, 1)
	require.NoError(t, err)

	// Commit a successful download outcome.
	outcome := &synctypes.Outcome{
		Action:     synctypes.ActionDownload,
		Success:    true,
		Path:       "docs/file.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-1",
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		ItemType:   synctypes.ItemTypeFile,
	}
	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	// D-6: CommitOutcome does NOT clear sync_failures — engine owns that.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, issues, 1, "sync_failures should survive CommitOutcome")
}

// Validates: R-2.10.41
func TestCommitOutcome_Success_DoesNotClearSyncFailures(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		direction    synctypes.Direction
		issueType    string
		remoteStatus synctypes.SyncStatus
		action       synctypes.ActionType
	}{
		{
			name:         "DeleteSuccess",
			path:         "docs/old.txt",
			direction:    synctypes.DirectionDelete,
			issueType:    "delete_failed",
			remoteStatus: synctypes.SyncStatusDeleting,
			action:       synctypes.ActionLocalDelete,
		},
		{
			name:         "MoveSuccess",
			path:         "docs/moved.txt",
			direction:    synctypes.DirectionUpload,
			issueType:    "upload_failed",
			remoteStatus: synctypes.SyncStatusSynced,
			action:       synctypes.ActionLocalMove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _ := newTestSyncStoreForFailures(t)
			ctx := context.Background()

			err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
				Path:      tt.path,
				DriveID:   driveid.ID{},
				Direction: tt.direction,
				IssueType: tt.issueType,
				ErrMsg:    "timeout",
			}, nil)
			require.NoError(t, err)

			_, err = mgr.DB().ExecContext(ctx,
				`INSERT INTO remote_state
					(drive_id, item_id, path, parent_id, item_type, sync_status, observed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				driveid.New(testDriveID).String(), "item-1", tt.path, "root", "file", tt.remoteStatus, 1)
			require.NoError(t, err)

			outcome := &synctypes.Outcome{
				Action:  tt.action,
				Success: true,
				Path:    tt.path,
				DriveID: driveid.New(testDriveID),
				ItemID:  "item-1",
			}
			require.NoError(t, mgr.CommitOutcome(ctx, outcome))

			issues, err := mgr.ListSyncFailures(ctx)
			require.NoError(t, err)
			assert.Len(t, issues, 1, "sync_failures should survive CommitOutcome")
		})
	}
}

func TestLocalIssueSyncStatus(t *testing.T) {
	tests := []struct {
		issueType string
		want      bool // true = actionable (user must fix)
	}{
		{synctypes.IssueInvalidFilename, true},
		{synctypes.IssuePathTooLong, true},
		{synctypes.IssueFileTooLarge, true},
		{synctypes.IssuePermissionDenied, true},
		{synctypes.IssueQuotaExceeded, true},
		{synctypes.IssueLocalPermissionDenied, true},
		{synctypes.IssueCaseCollision, true},
		{synctypes.IssueDiskFull, true},
		{synctypes.IssueFileTooLargeForSpace, true},
		{synctypes.IssueServiceOutage, false}, // transient — auto-resolves
		{"upload_failed", false},
		{"locked", false},
		{"sharepoint_restriction", false},
	}

	for _, tt := range tests {
		t.Run(tt.issueType, func(t *testing.T) {
			assert.Equal(t, tt.want, IsActionableIssue(tt.issueType))
		})
	}
}

func TestRecordFailure_TransientHasNextRetryAt(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, retry.ReconcilePolicy().Delay)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	// Transient issues should have a next_retry_at computed by the delay function.
	assert.Positive(t, issues[0].NextRetryAt,
		"transient issue should have next_retry_at set by delay function")
}

func TestRecordFailure_PermanentNoRetryAt(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "CON",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "invalid_filename",
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, int64(0), issues[0].NextRetryAt,
		"permanent issue should have no next_retry_at")
}

func TestRecordFailure_RepeatIncrementsCount(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// First failure.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, retry.ReconcilePolicy().Delay)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, 1, issues[0].FailureCount)

	// Advance time and record second failure.
	laterTime := fixedTime.Add(5 * time.Minute)
	mgr.SetNowFunc(func() time.Time { return laterTime })

	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout again",
	}, retry.ReconcilePolicy().Delay)
	require.NoError(t, err)

	issues, err = mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	// Failure count should increment on repeated failures.
	assert.Equal(t, 2, issues[0].FailureCount,
		"failure count should increase on repeated failures")
	// next_retry_at should be set by the delay function.
	assert.Positive(t, issues[0].NextRetryAt)
}

func TestListLocalIssuesForRetry_ReturnsResults(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record a transient issue with a delay function that sets next_retry_at.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "retry.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, retry.ReconcilePolicy().Delay)
	require.NoError(t, err)

	// ListSyncFailuresForRetry finds rows with next_retry_at <= the given time.
	// With a delay function, transient issues have next_retry_at set, so
	// querying far enough in the future should return the row.
	futureTime := fixedTime.Add(10 * time.Minute)
	rows, err := mgr.ListSyncFailuresForRetry(ctx, futureTime)
	require.NoError(t, err)
	assert.NotEmpty(t, rows, "transient issues with delay function should be returned for retry")
}

func TestEarliestLocalIssueRetryAt_ReturnsFutureTime(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// No issues — zero time.
	earliest, err := mgr.EarliestSyncFailureRetryAt(ctx, fixedTime)
	require.NoError(t, err)
	assert.True(t, earliest.IsZero())

	// Record a transient issue with a delay function that sets next_retry_at.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, retry.ReconcilePolicy().Delay)
	require.NoError(t, err)

	// With a delay function, transient issues have next_retry_at set, so
	// EarliestSyncFailureRetryAt should return a future time.
	earliest, err = mgr.EarliestSyncFailureRetryAt(ctx, fixedTime)
	require.NoError(t, err)
	assert.False(t, earliest.IsZero(), "transient issue with delay function should have a retry time")
}

func TestMarkLocalIssuePermanent(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record a transient issue.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, nil)
	require.NoError(t, err)

	// Verify it's transient.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.CategoryTransient, issues[0].Category)

	// Mark as permanent.
	err = mgr.MarkSyncFailureActionable(ctx, "file.txt", driveid.ID{})
	require.NoError(t, err)

	// Verify category changed.
	issues, err = mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.CategoryActionable, issues[0].Category)
	assert.Equal(t, int64(0), issues[0].NextRetryAt, "permanent issues should have no retry time")

	// Should not appear in retry list.
	rows, err := mgr.ListSyncFailuresForRetry(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestListLocalIssuesByType(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record issues of different types.
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "a.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: "upload_failed", ErrMsg: "err",
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "b.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "c.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "d.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: "upload_failed", ErrMsg: "err",
	}, nil))

	// Query by permission_denied type.
	issues, err := mgr.ListSyncFailuresByIssueType(ctx, synctypes.IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, synctypes.IssuePermissionDenied, issues[0].IssueType)
	assert.Equal(t, synctypes.IssuePermissionDenied, issues[1].IssueType)

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
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record issues at various paths.
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "shared", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "shared/file.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "shared/sub/file.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "other/file.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))

	// Clear by prefix "shared".
	err := mgr.ClearSyncFailuresByPrefix(ctx, "shared", synctypes.IssuePermissionDenied)
	require.NoError(t, err)

	// Only "other/file.txt" should remain.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "other/file.txt", issues[0].Path)
}

func TestClearLocalIssuesByPrefix_TypeFiltering(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Record a permission_denied and an upload_failed at the same prefix.
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "shared/a.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePermissionDenied, ErrMsg: "read-only", HTTPStatus: 403,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "shared/b.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: "upload_failed", ErrMsg: "err", HTTPStatus: 500,
	}, nil))

	// Clear permission_denied only.
	err := mgr.ClearSyncFailuresByPrefix(ctx, "shared", synctypes.IssuePermissionDenied)
	require.NoError(t, err)

	// upload_failed should remain.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "upload_failed", issues[0].IssueType)
}

func TestRecordFailure_ScopeKeyStored(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "big.zip",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleBoundary,
		Category:  synctypes.CategoryActionable,
		IssueType: synctypes.IssueQuotaExceeded,
		ErrMsg:    "quota exceeded",
		FileSize:  5000,
		ScopeKey:  synctypes.SKQuotaOwn(),
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.SKQuotaOwn(), issues[0].ScopeKey)
}

func TestRecordFailure_ScopeKeyDefaultEmpty(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.True(t, issues[0].ScopeKey.IsZero(), "default scope key should be zero")
}

func TestUpsertActionableFailures(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	failures := []synctypes.ActionableFailure{
		{Path: "CON.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssueInvalidFilename, Error: "reserved name"},
		{Path: "long/path.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePathTooLong, Error: "too long"},
	}

	err := mgr.UpsertActionableFailures(ctx, failures)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 2)

	// Both should be actionable.
	for _, row := range issues {
		assert.Equal(t, synctypes.CategoryActionable, row.Category)
	}
}

func TestUpsertActionableFailures_UpdateExisting(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Insert initial failure.
	failures := []synctypes.ActionableFailure{
		{Path: "CON.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssueInvalidFilename, Error: "reserved name v1"},
	}
	err := mgr.UpsertActionableFailures(ctx, failures)
	require.NoError(t, err)

	// Upsert with updated error message.
	failures[0].Error = "reserved name v2"
	err = mgr.UpsertActionableFailures(ctx, failures)
	require.NoError(t, err)

	// Should still be 1 row, with updated error.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "reserved name v2", issues[0].LastError)
}

func TestUpsertActionableFailures_EmptySlice(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Should not error on empty input.
	err := mgr.UpsertActionableFailures(ctx, nil)
	require.NoError(t, err)
}

// Validates: R-2.10.2
func TestClearResolvedActionableFailures(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Upsert 3 actionable failures of the same type.
	failures := []synctypes.ActionableFailure{
		{Path: "CON.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssueInvalidFilename, Error: "reserved"},
		{Path: "NUL.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssueInvalidFilename, Error: "reserved"},
		{Path: "PRN.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssueInvalidFilename, Error: "reserved"},
	}
	err := mgr.UpsertActionableFailures(ctx, failures)
	require.NoError(t, err)

	// Scanner now only sees CON.txt and NUL.txt — PRN.txt was resolved.
	currentPaths := []string{"CON.txt", "NUL.txt"}
	err = mgr.ClearResolvedActionableFailures(ctx, synctypes.IssueInvalidFilename, currentPaths)
	require.NoError(t, err)

	// Only CON.txt and NUL.txt should remain.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 2)

	paths := []string{issues[0].Path, issues[1].Path}
	assert.Contains(t, paths, "CON.txt")
	assert.Contains(t, paths, "NUL.txt")
}

// Validates: R-2.10.2
func TestClearResolvedActionableFailures_DifferentTypeNotCleared(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Upsert failures of two different types.
	failures := []synctypes.ActionableFailure{
		{Path: "CON.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssueInvalidFilename, Error: "reserved"},
		{Path: "long/path.txt", DriveID: driveid.ID{}, Direction: synctypes.DirectionUpload, IssueType: synctypes.IssuePathTooLong, Error: "too long"},
	}
	err := mgr.UpsertActionableFailures(ctx, failures)
	require.NoError(t, err)

	// Clear resolved for invalid_filename with empty current list — all resolved.
	err = mgr.ClearResolvedActionableFailures(ctx, synctypes.IssueInvalidFilename, nil)
	require.NoError(t, err)

	// path_too_long should remain untouched.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.IssuePathTooLong, issues[0].IssueType)
}

// ---------------------------------------------------------------------------
// RecordFailure (unified method) tests
// ---------------------------------------------------------------------------

func TestRecordFailure_NewEntry(t *testing.T) {
	t.Parallel()

	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "docs/report.xlsx",
		DriveID:    driveid.ID{},
		Direction:  synctypes.DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "connection reset",
		HTTPStatus: 500,
		FileSize:   1024,
		LocalHash:  "abc123",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	row := issues[0]
	assert.Equal(t, "docs/report.xlsx", row.Path)
	assert.Equal(t, "upload_failed", row.IssueType)
	assert.Equal(t, synctypes.CategoryTransient, row.Category)
	assert.Equal(t, 1, row.FailureCount)
	assert.Equal(t, "connection reset", row.LastError)
	assert.Equal(t, 500, row.HTTPStatus)
	assert.Equal(t, int64(1024), row.FileSize)
	assert.Equal(t, "abc123", row.LocalHash)
	assert.Equal(t, fixedTime.UnixNano(), row.FirstSeenAt)
	assert.Equal(t, fixedTime.UnixNano(), row.LastSeenAt)
}

func TestRecordFailure_ActionableClassification(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Actionable issue type → category="actionable", no next_retry_at.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "CON.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueInvalidFilename,
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "reserved name",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, synctypes.CategoryActionable, issues[0].Category)
	assert.Equal(t, int64(0), issues[0].NextRetryAt, "actionable issues should not have next_retry_at")
}

func TestRecordFailure_TransientHasDatabaseBackoff(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "file.txt",
		DriveID:    driveid.ID{},
		Direction:  synctypes.DirectionUpload,
		ErrMsg:     "timeout",
		HTTPStatus: 503,
	}, retry.ReconcilePolicy().Delay)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, synctypes.CategoryTransient, issues[0].Category)
	// next_retry_at should be set by the delay function.
	assert.Positive(t, issues[0].NextRetryAt,
		"transient issues should have next_retry_at set by delay function")
}

func TestRecordFailure_DownloadStateTransition(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		Direction:  synctypes.DirectionDownload,
		ErrMsg:     "connection reset",
		HTTPStatus: 500,
	}, nil)
	require.NoError(t, err)

	// remote_state should be transitioned to download_failed.
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusDownloadFailed, row.SyncStatus)

	// sync_failures row should exist with item_id auto-resolved.
	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "item1", issues[0].ItemID, "item_id should be auto-resolved from remote_state")
}

func TestRecordFailure_UploadNoStateTransition(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })
	ctx := context.Background()

	// Insert a synced item — uploads should not affect remote_state.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'synced', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "hello.txt",
		DriveID:   driveid.New(testDriveID),
		Direction: synctypes.DirectionUpload,
		ErrMsg:    "upload error",
	}, nil)
	require.NoError(t, err)

	// Status should remain synced — uploads don't transition remote_state.
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusSynced, row.SyncStatus)
}

func TestRecordFailure_PreservesExistingValuesOnConflict(t *testing.T) {
	t.Parallel()

	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// First record with file_size and local_hash.
	err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "upload_failed",
		ErrMsg:    "timeout",
		FileSize:  2048,
		LocalHash: "hash1",
	}, nil)
	require.NoError(t, err)

	// Second record without file_size and local_hash — should preserve originals.
	laterTime := fixedTime.Add(5 * time.Minute)
	mgr.SetNowFunc(func() time.Time { return laterTime })

	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "file.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		ErrMsg:    "new error",
	}, nil)
	require.NoError(t, err)

	issues, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, int64(2048), issues[0].FileSize, "file_size should be preserved via COALESCE")
	assert.Equal(t, "hash1", issues[0].LocalHash, "local_hash should be preserved via COALESCE")
	assert.Equal(t, 2, issues[0].FailureCount)
	assert.Equal(t, "new error", issues[0].LastError)
}

func TestDeleteSyncFailuresByScope(t *testing.T) {
	mgr, _ := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// Insert failures with different scope keys.
	for _, p := range []struct {
		path     string
		scopeKey synctypes.ScopeKey
	}{
		{"a.txt", synctypes.SKQuotaShortcut("drive1:item1")},
		{"b.txt", synctypes.SKQuotaShortcut("drive1:item1")},
		{"c.txt", synctypes.SKThrottleAccount()},
		{"d.txt", synctypes.SKQuotaShortcut("drive2:item2")},
	} {
		err := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      p.path,
			DriveID:   driveid.ID{},
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleHeld,
			IssueType: "upload_failed",
			ErrMsg:    "timeout",
			ScopeKey:  p.scopeKey,
		}, nil)
		require.NoError(t, err)
	}

	// Verify 4 failures exist.
	all, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 4)

	// Delete by scope key.
	err = mgr.DeleteSyncFailuresByScope(ctx, synctypes.SKQuotaShortcut("drive1:item1"))
	require.NoError(t, err)

	// Verify only 2 remain (c.txt and d.txt).
	remaining, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 2)

	paths := make(map[string]bool)
	for _, r := range remaining {
		paths[r.Path] = true
	}

	assert.True(t, paths["c.txt"], "c.txt should remain (different scope)")
	assert.True(t, paths["d.txt"], "d.txt should remain (different scope)")
}

func TestPendingRetrySummary(t *testing.T) {
	mgr, fixedTime := newTestSyncStoreForFailures(t)
	ctx := context.Background()

	// No failures → empty summary.
	groups, err := mgr.PendingRetrySummary(ctx)
	require.NoError(t, err)
	assert.Empty(t, groups)

	// Insert transient failures with different scope keys and retry times.
	for _, p := range []struct {
		path     string
		scopeKey synctypes.ScopeKey
	}{
		{"a.txt", synctypes.SKThrottleAccount()},
		{"b.txt", synctypes.SKThrottleAccount()},
		{"c.txt", synctypes.SKThrottleAccount()},
		{"d.txt", synctypes.SKQuotaOwn()},
		{"e.txt", synctypes.SKQuotaOwn()},
	} {
		recErr := mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      p.path,
			DriveID:   driveid.ID{},
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleItem,
			IssueType: "upload_failed",
			ErrMsg:    "timeout",
			ScopeKey:  p.scopeKey,
		}, retry.ReconcilePolicy().Delay)
		require.NoError(t, recErr)
	}

	// Also insert an actionable failure — should NOT appear in summary.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "actionable.txt",
		DriveID:   driveid.ID{},
		Direction: synctypes.DirectionUpload,
		IssueType: "invalid_filename",
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "bad name",
	}, nil)
	require.NoError(t, err)

	groups, err = mgr.PendingRetrySummary(ctx)
	require.NoError(t, err)
	require.Len(t, groups, 2, "should have 2 scope groups")

	// Ordered by count DESC, so throttle:account (3) comes first.
	assert.Equal(t, synctypes.SKThrottleAccount(), groups[0].ScopeKey)
	assert.Equal(t, 3, groups[0].Count)
	assert.True(t, groups[0].EarliestNext.After(fixedTime), "earliest retry should be after now")

	assert.Equal(t, synctypes.SKQuotaOwn(), groups[1].ScopeKey)
	assert.Equal(t, 2, groups[1].Count)
}
