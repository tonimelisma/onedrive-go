package syncstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// readRemoteStateRow is a test helper that reads a single remote_state row.
func readRemoteStateRow(t *testing.T, db *sql.DB, itemID string) *synctypes.RemoteStateRow {
	t.Helper()

	var (
		row          synctypes.RemoteStateRow
		parentID     sql.NullString
		hash         sql.NullString
		size         sql.NullInt64
		mtime        sql.NullInt64
		etag         sql.NullString
		prevPath     sql.NullString
		filterReason sql.NullString
	)

	err := db.QueryRowContext(t.Context(),
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at, filter_generation, filter_reason
		FROM remote_state WHERE item_id = ?`,
		itemID,
	).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt, &row.FilterGeneration, &filterReason,
	)
	if err == sql.ErrNoRows {
		return nil
	}

	require.NoError(t, err, "scanning remote_state row")

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String
	row.FilterReason = synctypes.RemoteFilterReason(filterReason.String)

	if size.Valid {
		row.Size = size.Int64
	}

	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row
}

// readDeltaToken is a test helper that reads the delta token for a drive.
func readDeltaToken(t *testing.T, db *sql.DB, driveID string) string {
	t.Helper()

	var token string

	err := db.QueryRowContext(t.Context(),
		`SELECT cursor FROM delta_tokens WHERE drive_id = ? AND scope_id = ''`,
		driveID,
	).Scan(&token)
	if err == sql.ErrNoRows {
		return ""
	}

	require.NoError(t, err, "reading delta token")

	return token
}

func TestCommitObservation_NewItem(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New(testDriveID)
	events := []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			ParentID: "root",
			Path:     "hello.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "hash1",
			Size:     100,
			Mtime:    1000000,
			ETag:     "etag1",
		},
	}

	err := mgr.CommitObservation(ctx, events, "delta-token-1", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row, "row should exist")
	assert.Equal(t, "hello.txt", row.Path)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, row.SyncStatus)
	assert.Equal(t, "hash1", row.Hash)
	assert.Equal(t, int64(100), row.Size)
	assert.Equal(t, "etag1", row.ETag)

	// Delta token should be committed in the same transaction.
	token := readDeltaToken(t, mgr.DB(), testDriveID)
	assert.Equal(t, "delta-token-1", token)
}

func TestCommitObservation_DeletedUnknownItem_Noop(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New(testDriveID)
	events := []synctypes.ObservedItem{
		{
			DriveID:   driveID,
			ItemID:    "unknown",
			Path:      "gone.txt",
			ItemType:  synctypes.ItemTypeFile,
			IsDeleted: true,
		},
	}

	err := mgr.CommitObservation(ctx, events, "delta-token-2", driveID)
	require.NoError(t, err)

	// Should NOT create a row for a deleted item we've never seen.
	row := readRemoteStateRow(t, mgr.DB(), "unknown")
	assert.Nil(t, row, "no row should exist for deleted unknown item")

	// Delta token should still be committed.
	token := readDeltaToken(t, mgr.DB(), testDriveID)
	assert.Equal(t, "delta-token-2", token)
}

func TestCommitObservation_SyncedSameHash_NoChange(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Pre-populate with a synced item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'synced', ?)`,
		testDriveID, "item1", "hello.txt", "hash1", 999,
	)
	require.NoError(t, err)

	// Observe same hash again (delta redelivery).
	events := []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			Path:     "hello.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "hash1",
		},
	}

	err = mgr.CommitObservation(ctx, events, "delta-token-3", driveID)
	require.NoError(t, err)

	// Status should remain synced (no re-download on delta redelivery).
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusSynced, row.SyncStatus, "should remain synced")
}

func TestCommitObservation_HashChange_ResetsFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(2000, 0) })
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Pre-populate with a failed item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'download_failed', ?)`,
		testDriveID, "item1", "hello.txt", "old-hash", 999,
	)
	require.NoError(t, err)

	// Observe with different hash.
	events := []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			Path:     "hello.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "new-hash",
		},
	}

	err = mgr.CommitObservation(ctx, events, "delta-token-4", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, row.SyncStatus)
	assert.Equal(t, "new-hash", row.Hash)
	// Failure state is tracked in sync_failures table, not remote_state.
}

func TestCommitObservation_MoveTracking_SetsPreviousPath(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(3000, 0) })
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Pre-populate with an item at old path.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'synced', ?)`,
		testDriveID, "item1", "old/hello.txt", "hash1", 999,
	)
	require.NoError(t, err)

	// Observe at new path.
	events := []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			Path:     "new/hello.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "hash1",
		},
	}

	err = mgr.CommitObservation(ctx, events, "delta-token-5", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, "new/hello.txt", row.Path)
	assert.Equal(t, "old/hello.txt", row.PreviousPath, "should track previous path")
}

func TestCommitObservation_AtomicWithDeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Multiple items + delta token in a single CommitObservation call.
	events := []synctypes.ObservedItem{
		{
			DriveID: driveID, ItemID: "a", Path: "a.txt",
			ItemType: synctypes.ItemTypeFile, Hash: "h1",
		},
		{
			DriveID: driveID, ItemID: "b", Path: "b.txt",
			ItemType: synctypes.ItemTypeFile, Hash: "h2",
		},
	}

	err := mgr.CommitObservation(ctx, events, "atomic-token", driveID)
	require.NoError(t, err)

	// Both items and token should exist.
	assert.NotNil(t, readRemoteStateRow(t, mgr.DB(), "a"))
	assert.NotNil(t, readRemoteStateRow(t, mgr.DB(), "b"))
	assert.Equal(t, "atomic-token", readDeltaToken(t, mgr.DB(), testDriveID))
}

func TestCommitObservation_AllMatrixCells(t *testing.T) {
	t.Parallel()

	// Table-driven through key cells of the 30-cell matrix.
	tests := []struct {
		name           string
		existingStatus synctypes.SyncStatus
		existingHash   string
		observedHash   string
		isDeleted      bool
		wantStatus     synctypes.SyncStatus
		wantChanged    bool
	}{
		// Same hash, not deleted.
		{"synced+same→noop", synctypes.SyncStatusSynced, "h1", "h1", false, synctypes.SyncStatusSynced, false},
		{"pending_download+same→noop", synctypes.SyncStatusPendingDownload, "h1", "h1", false, synctypes.SyncStatusPendingDownload, false},
		{"download_failed+same→retry", synctypes.SyncStatusDownloadFailed, "h1", "h1", false, synctypes.SyncStatusPendingDownload, true},
		{"deleted+same→restore", synctypes.SyncStatusDeleted, "h1", "h1", false, synctypes.SyncStatusPendingDownload, true},

		// Different hash, not deleted.
		{"synced+diff→pending", synctypes.SyncStatusSynced, "h1", "h2", false, synctypes.SyncStatusPendingDownload, true},
		{"downloading+diff→pending", synctypes.SyncStatusDownloading, "h1", "h2", false, synctypes.SyncStatusPendingDownload, true},
		{"download_failed+diff→pending", synctypes.SyncStatusDownloadFailed, "h1", "h2", false, synctypes.SyncStatusPendingDownload, true},

		// Deleted.
		{"synced+deleted→pending_delete", synctypes.SyncStatusSynced, "h1", "", true, synctypes.SyncStatusPendingDelete, true},
		{"pending_download+deleted→pending_delete", synctypes.SyncStatusPendingDownload, "h1", "", true, synctypes.SyncStatusPendingDelete, true},
		{"pending_delete+deleted→noop", synctypes.SyncStatusPendingDelete, "h1", "", true, synctypes.SyncStatusPendingDelete, false},
		{"deleted+deleted→noop", synctypes.SyncStatusDeleted, "h1", "", true, synctypes.SyncStatusDeleted, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })
			ctx := context.Background()

			driveID := driveid.New(testDriveID)

			// Insert existing row.
			_, err := mgr.DB().ExecContext(ctx,
				`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
				VALUES (?, ?, ?, 'file', ?, ?, ?)`,
				testDriveID, "item1", "file.txt", tt.existingHash, tt.existingStatus, 999,
			)
			require.NoError(t, err)

			events := []synctypes.ObservedItem{
				{
					DriveID:   driveID,
					ItemID:    "item1",
					Path:      "file.txt",
					ItemType:  synctypes.ItemTypeFile,
					Hash:      tt.observedHash,
					IsDeleted: tt.isDeleted,
				},
			}

			err = mgr.CommitObservation(ctx, events, "token", driveID)
			require.NoError(t, err)

			row := readRemoteStateRow(t, mgr.DB(), "item1")
			require.NotNil(t, row)
			assert.Equal(t, tt.wantStatus, row.SyncStatus)
		})
	}
}

// ---------------------------------------------------------------------------
// RecordFailure tests (migrated from old RecordFailure)
// ---------------------------------------------------------------------------

func TestRecordFailure_TransitionsDownloading(t *testing.T) {
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

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusDownloadFailed, row.SyncStatus)

	// Failure metadata is now in sync_failures, not remote_state.
	var sfCount int
	var sfError string
	var sfHTTP int
	var sfRetry *int64 // nullable — retrier handles retry, not sync_failures
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT failure_count, last_error, http_status, next_retry_at FROM sync_failures WHERE path = ?",
		"hello.txt",
	).Scan(&sfCount, &sfError, &sfHTTP, &sfRetry)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)
	assert.Equal(t, "connection reset", sfError)
	assert.Equal(t, 500, sfHTTP)
	assert.Nil(t, sfRetry, "transient issues have no next_retry_at (retrier handles retry)")
}

func TestRecordFailure_OptimisticConcurrency_NoMatch(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })
	ctx := context.Background()

	// Insert a synced item (not downloading/deleting).
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'synced', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	// RecordFailure should be a no-op for the state
	// transition (row not in downloading/deleting), but still records the
	// sync_failure entry.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		Direction:  synctypes.DirectionDownload,
		ErrMsg:     "some error",
		HTTPStatus: 500,
	}, nil)
	require.NoError(t, err)

	// Status should remain synced.
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusSynced, row.SyncStatus)
}

func TestRecordFailure_IncreasesFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	callCount := 0
	mgr.SetNowFunc(func() time.Time {
		callCount++
		return time.Unix(int64(1000+callCount*100), 0)
	})
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	// First failure.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		Direction:  synctypes.DirectionDownload,
		ErrMsg:     "err1",
		HTTPStatus: 500,
	}, nil)
	require.NoError(t, err)

	// Failure count is now in sync_failures.
	var sfCount int
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT failure_count FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)

	// Set status back to downloading for second failure.
	_, err = mgr.DB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = 'downloading' WHERE item_id = ?`, "item1")
	require.NoError(t, err)

	// Second failure.
	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		Direction:  synctypes.DirectionDownload,
		ErrMsg:     "err2",
		HTTPStatus: 503,
	}, nil)
	require.NoError(t, err)

	err = mgr.DB().QueryRowContext(ctx,
		"SELECT failure_count FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 2, sfCount)
}

func TestRecordFailure_DeleteTransitionsDeleting(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })
	ctx := context.Background()

	// Insert a deleting item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'deleting', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		Direction:  synctypes.DirectionDownload,
		ErrMsg:     "permission denied",
		HTTPStatus: 403,
	}, nil)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusDeleteFailed, row.SyncStatus)

	// Failure metadata is now in sync_failures.
	var sfCount int
	var sfHTTP int
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT failure_count, http_status FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount, &sfHTTP)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)
	assert.Equal(t, 403, sfHTTP)
}

// ---------------------------------------------------------------------------
// RecordFailure tests
// ---------------------------------------------------------------------------

func TestRecordFailure_Download(t *testing.T) {
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

	// remote_state should transition to download_failed.
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusDownloadFailed, row.SyncStatus)

	// sync_failures should have the failure recorded.
	var sfCount int
	var sfError string
	var sfHTTP int
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT failure_count, last_error, http_status FROM sync_failures WHERE path = ?",
		"hello.txt",
	).Scan(&sfCount, &sfError, &sfHTTP)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)
	assert.Equal(t, "connection reset", sfError)
	assert.Equal(t, 500, sfHTTP)
}

func TestRecordFailure_SetsIssueTypeAndScopeKey(t *testing.T) {
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
		Role:       synctypes.FailureRoleBoundary,
		IssueType:  synctypes.IssueQuotaExceeded,
		Category:   synctypes.CategoryActionable,
		ErrMsg:     "quota full",
		HTTPStatus: 507,
		ScopeKey:   synctypes.SKQuotaOwn(),
	}, nil)
	require.NoError(t, err)

	// Verify issue_type, scope_key, and category are set correctly.
	var issueType, scopeKey, category string
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT issue_type, scope_key, category FROM sync_failures WHERE path = ?",
		"hello.txt",
	).Scan(&issueType, &scopeKey, &category)
	require.NoError(t, err)
	assert.Equal(t, synctypes.IssueQuotaExceeded, issueType)
	assert.Equal(t, "quota:own", scopeKey)
	assert.Equal(t, "actionable", category, "quota_exceeded is an actionable issue")
}

// TestRecordFailure_BackoffCalculation was removed: computeNextRetry is
// deleted because the retrier is the sole retry mechanism (R-6.8.10).
// sync_failures no longer drives retry scheduling.

// ---------------------------------------------------------------------------
// ResetRetryTimesForScope (R-2.10.11, R-2.10.15)
// ---------------------------------------------------------------------------

// Validates: R-2.10.11, R-2.10.15
func TestResetRetryTimesForScope(t *testing.T) {
	t.Parallel()

	now := time.Unix(2000, 0)
	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return now })
	ctx := context.Background()

	futureNano := now.Add(10 * time.Minute).UnixNano()
	pastNano := now.Add(-1 * time.Minute).UnixNano()

	// Insert failures: one with future retry (matching scope), one with past retry (matching scope),
	// one with future retry (different scope), one actionable (matching scope).
	for _, tc := range []struct {
		path, scope, role, category string
		retryAt                     int64
	}{
		{"future-match.txt", "throttle:account", "item", "transient", futureNano},
		{"past-match.txt", "throttle:account", "item", "transient", pastNano},
		{"future-other.txt", "service", "item", "transient", futureNano},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO sync_failures
				(path, drive_id, direction, action_type, failure_role, category, failure_count, next_retry_at,
				 last_error, http_status, first_seen_at, last_seen_at, scope_key)
			VALUES (?, ?, 'download', 'download', ?, ?, 1, ?, 'err', 429, ?, ?, ?)`,
			tc.path, testDriveID, tc.role, tc.category, tc.retryAt,
			now.UnixNano(), now.UnixNano(), tc.scope,
		)
		require.NoError(t, err, "inserting %s", tc.path)
	}

	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures
			(path, drive_id, direction, action_type, failure_role, category, failure_count, next_retry_at,
			 last_error, http_status, first_seen_at, last_seen_at, scope_key)
		VALUES (?, ?, 'download', 'download', 'item', 'actionable', 1, NULL, 'err', 429, ?, ?, ?)`,
		"actionable-match.txt", testDriveID, now.UnixNano(), now.UnixNano(), "throttle:account",
	)
	require.NoError(t, err)

	err = mgr.ResetRetryTimesForScope(ctx, synctypes.SKThrottleAccount(), now)
	require.NoError(t, err)

	// future-match.txt: transient + matching scope + future retry → should be reset to now
	var retryAt int64
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT next_retry_at FROM sync_failures WHERE path = ?", "future-match.txt",
	).Scan(&retryAt)
	require.NoError(t, err)
	assert.Equal(t, now.UnixNano(), retryAt, "future transient matching scope should be reset to now")

	// past-match.txt: retry already in the past → should NOT be changed
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT next_retry_at FROM sync_failures WHERE path = ?", "past-match.txt",
	).Scan(&retryAt)
	require.NoError(t, err)
	assert.Equal(t, pastNano, retryAt, "past retry should not be changed")

	// future-other.txt: different scope → should NOT be changed
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT next_retry_at FROM sync_failures WHERE path = ?", "future-other.txt",
	).Scan(&retryAt)
	require.NoError(t, err)
	assert.Equal(t, futureNano, retryAt, "different scope should not be changed")

	// actionable-match.txt: actionable category -> should remain NULL
	var actionableRetry sql.NullInt64
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT next_retry_at FROM sync_failures WHERE path = ?", "actionable-match.txt",
	).Scan(&actionableRetry)
	require.NoError(t, err)
	assert.False(t, actionableRetry.Valid, "actionable failures should not be changed")
}

// ---------------------------------------------------------------------------
// CommitOutcome remote_state extension tests
// ---------------------------------------------------------------------------

func TestCommitOutcome_UpdatesRemoteState_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })
	ctx := context.Background()

	// Load baseline so CommitOutcome can update cache.
	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a downloading remote_state row.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", "hash1", 999,
	)
	require.NoError(t, err)

	outcome := &synctypes.Outcome{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "hello.txt",
		DriveID:         driveid.New(testDriveID),
		ItemID:          "item1",
		ItemType:        synctypes.ItemTypeFile,
		LocalHash:       "hash1",
		RemoteHash:      "hash1",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
		LocalMtime:      2000000,
		RemoteMtime:     2000000,
		ETag:            "etag1",
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusSynced, row.SyncStatus)
}

func TestCommitOutcome_HashGuard_PreventsStaleOverwrite(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })
	ctx := context.Background()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a downloading row with hash "new-hash" (new observation arrived).
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", "new-hash", 999,
	)
	require.NoError(t, err)

	// Outcome from old download with old hash.
	outcome := &synctypes.Outcome{
		Action:     synctypes.ActionDownload,
		Success:    true,
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "old-hash",
		RemoteHash: "old-hash",
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	// Should NOT transition to synced (hash mismatch guard).
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusDownloading, row.SyncStatus, "hash guard should prevent stale overwrite")
}

func TestCommitOutcome_Upload_UnconditionalUpdate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })
	ctx := context.Background()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a remote_state row in any status.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'pending_download', ?)`,
		testDriveID, "item1", "hello.txt", "old-hash", 999,
	)
	require.NoError(t, err)

	outcome := &synctypes.Outcome{
		Action:          synctypes.ActionUpload,
		Success:         true,
		Path:            "hello.txt",
		DriveID:         driveid.New(testDriveID),
		ItemID:          "item1",
		ItemType:        synctypes.ItemTypeFile,
		LocalHash:       "upload-hash",
		RemoteHash:      "upload-hash",
		LocalSize:       500,
		LocalSizeKnown:  true,
		RemoteSize:      500,
		RemoteSizeKnown: true,
		LocalMtime:      3000000,
		RemoteMtime:     3000000,
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusSynced, row.SyncStatus)
	assert.Equal(t, "upload-hash", row.Hash)
	assert.Equal(t, int64(500), row.Size)
}

// ---------------------------------------------------------------------------
// StateReader + StateAdmin tests
// ---------------------------------------------------------------------------

func TestListUnreconciled(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()

	// Insert rows in various states.
	for _, s := range []struct {
		id     string
		status synctypes.SyncStatus
	}{
		{"a", synctypes.SyncStatusPendingDownload},
		{"b", synctypes.SyncStatusSynced},
		{"c", synctypes.SyncStatusDownloadFailed},
		{"d", synctypes.SyncStatusDeleted},
		{"e", synctypes.SyncStatusPendingDelete},
		{"f", synctypes.SyncStatusFiltered},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	rows, err := mgr.ListUnreconciled(ctx)
	require.NoError(t, err)

	// Should include: pending_download, download_failed, pending_delete
	// Should exclude: synced, deleted, filtered
	assert.Len(t, rows, 3)

	ids := make(map[string]bool)
	for _, r := range rows {
		ids[r.ItemID] = true
	}

	assert.True(t, ids["a"], "pending_download should be unreconciled")
	assert.True(t, ids["c"], "download_failed should be unreconciled")
	assert.True(t, ids["e"], "pending_delete should be unreconciled")
}

func TestListFailedForRetry(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()

	// Insert rows with various statuses.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "pending", "pending.txt", synctypes.SyncStatusPendingDownload, 999,
	)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "failed", "failed.txt", synctypes.SyncStatusDownloadFailed, 999,
	)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "synced", "synced.txt", synctypes.SyncStatusSynced, 999,
	)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "del-failed", "del-failed.txt", synctypes.SyncStatusDeleteFailed, 999,
	)
	require.NoError(t, err)

	// ListActionableRemoteState returns all rows with pending/failed statuses.
	rows, err := mgr.ListActionableRemoteState(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 3, "should include pending_download + download_failed + delete_failed")

	ids := make(map[string]bool)
	for _, r := range rows {
		ids[r.ItemID] = true
	}
	assert.True(t, ids["pending"], "pending_download should be included")
	assert.True(t, ids["failed"], "download_failed should be included")
	assert.True(t, ids["del-failed"], "delete_failed should be included")
	assert.False(t, ids["synced"], "synced should not be included")
}

func TestFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	nowNano := time.Now().UnixNano()

	// SyncFailureCount queries sync_failures (category='transient'), not remote_state.
	for _, s := range []struct {
		path      string
		category  string
		direction string
		action    string
	}{
		{"a.txt", "transient", "download", "download"},
		{"b.txt", "transient", "delete", "remote_delete"},
		{"c.txt", "actionable", "download", "download"}, // actionable should not be counted
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, failure_count, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, ?, 'item', ?, 1, ?, ?)`,
			s.path, testDriveID, s.direction, s.action, s.category, nowNano, nowNano,
		)
		require.NoError(t, err)
	}

	count, err := mgr.SyncFailureCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "should count transient sync_failures only")
}

// Validates: R-2.10.1
func TestResetFailure(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	nowNano := time.Now().UnixNano()

	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "item1", "hello.txt", synctypes.SyncStatusDownloadFailed, 999,
	)
	require.NoError(t, err)

	// Insert a corresponding sync_failures row.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, failure_count,
			next_retry_at, last_error, first_seen_at, last_seen_at)
		VALUES (?, ?, 'download', 'download', 'item', 'transient', 5, 9999, 'old error', ?, ?)`,
		"hello.txt", testDriveID, nowNano, nowNano,
	)
	require.NoError(t, err)

	err = mgr.ResetFailure(ctx, "hello.txt")
	require.NoError(t, err)

	// remote_state should be transitioned to pending_download.
	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, row.SyncStatus)

	// sync_failures row should be deleted.
	var sfCount int
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 0, sfCount, "sync_failures row should be removed")
}

// Validates: R-2.10.1
func TestResetFailure_DeleteFailedTransitionsToPendingDelete(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	nowNano := time.Now().UnixNano()

	// Insert a delete_failed item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "del-item", "deleted.txt", synctypes.SyncStatusDeleteFailed, 999,
	)
	require.NoError(t, err)

	// Insert corresponding sync_failures row.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, failure_count,
			next_retry_at, last_error, first_seen_at, last_seen_at)
		VALUES (?, ?, 'delete', 'remote_delete', 'item', 'transient', 3, 9999, 'delete failed', ?, ?)`,
		"deleted.txt", testDriveID, nowNano, nowNano,
	)
	require.NoError(t, err)

	err = mgr.ResetFailure(ctx, "deleted.txt")
	require.NoError(t, err)

	// delete_failed should transition to pending_delete (NOT pending_download).
	row := readRemoteStateRow(t, mgr.DB(), "del-item")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusPendingDelete, row.SyncStatus,
		"delete_failed must transition to pending_delete, not pending_download")

	// sync_failures row should be deleted.
	var sfCount int
	err = mgr.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sync_failures WHERE path = ?", "deleted.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 0, sfCount, "sync_failures row should be removed")
}

// Validates: R-2.10.1
func TestResetAllFailures(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()

	for _, s := range []struct {
		id     string
		status synctypes.SyncStatus
	}{
		{"a", synctypes.SyncStatusDownloadFailed},
		{"b", synctypes.SyncStatusDeleteFailed},
		{"c", synctypes.SyncStatusSynced},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	err := mgr.ResetAllFailures(ctx)
	require.NoError(t, err)

	// download_failed should become pending_download.
	rowA := readRemoteStateRow(t, mgr.DB(), "a")
	assert.Equal(t, synctypes.SyncStatusPendingDownload, rowA.SyncStatus)

	// delete_failed should become pending_delete.
	rowB := readRemoteStateRow(t, mgr.DB(), "b")
	assert.Equal(t, synctypes.SyncStatusPendingDelete, rowB.SyncStatus)

	// synced should not change.
	rowC := readRemoteStateRow(t, mgr.DB(), "c")
	assert.Equal(t, synctypes.SyncStatusSynced, rowC.SyncStatus)
}

func TestResetInProgressStates(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	syncRoot := t.TempDir()

	// Create file for deleting item "b" so it's reset to pending_delete.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "b.txt"), []byte("x"), 0o600))

	for _, s := range []struct {
		id     string
		status synctypes.SyncStatus
	}{
		{"a", synctypes.SyncStatusDownloading},
		{"b", synctypes.SyncStatusDeleting}, // file exists → pending_delete
		{"c", synctypes.SyncStatusSynced},
		{"d", synctypes.SyncStatusPendingDownload},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	rowA := readRemoteStateRow(t, mgr.DB(), "a")
	assert.Equal(t, synctypes.SyncStatusPendingDownload, rowA.SyncStatus, "downloading→pending_download")

	rowB := readRemoteStateRow(t, mgr.DB(), "b")
	assert.Equal(t, synctypes.SyncStatusPendingDelete, rowB.SyncStatus, "deleting+file exists→pending_delete")

	rowC := readRemoteStateRow(t, mgr.DB(), "c")
	assert.Equal(t, synctypes.SyncStatusSynced, rowC.SyncStatus, "synced unchanged")

	rowD := readRemoteStateRow(t, mgr.DB(), "d")
	assert.Equal(t, synctypes.SyncStatusPendingDownload, rowD.SyncStatus, "pending_download unchanged")
}

func TestCommitOutcome_LocalDelete_MarksDeleted(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })
	ctx := context.Background()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a deleting remote_state row.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'deleting', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	outcome := &synctypes.Outcome{
		Action:  synctypes.ActionLocalDelete,
		Success: true,
		Path:    "hello.txt",
		DriveID: driveid.New(testDriveID),
		ItemID:  "item1",
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, synctypes.SyncStatusDeleted, row.SyncStatus)
}
