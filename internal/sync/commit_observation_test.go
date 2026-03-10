package sync

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
)

// readRemoteStateRow is a test helper that reads a single remote_state row.
func readRemoteStateRow(t *testing.T, db *sql.DB, itemID string) *RemoteStateRow {
	t.Helper()

	var (
		row      RemoteStateRow
		parentID sql.NullString
		hash     sql.NullString
		size     sql.NullInt64
		mtime    sql.NullInt64
		etag     sql.NullString
		prevPath sql.NullString
	)

	err := db.QueryRowContext(t.Context(),
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state WHERE item_id = ?`,
		itemID,
	).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt,
	)
	if err == sql.ErrNoRows {
		return nil
	}

	require.NoError(t, err, "scanning remote_state row")

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String

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
		`SELECT token FROM delta_tokens WHERE drive_id = ? AND scope_id = ''`,
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

	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New(testDriveID)
	events := []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			ParentID: "root",
			Path:     "hello.txt",
			ItemType: "file",
			Hash:     "hash1",
			Size:     100,
			Mtime:    1000000,
			ETag:     "etag1",
		},
	}

	err := mgr.CommitObservation(ctx, events, "delta-token-1", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row, "row should exist")
	assert.Equal(t, "hello.txt", row.Path)
	assert.Equal(t, statusPendingDownload, row.SyncStatus)
	assert.Equal(t, "hash1", row.Hash)
	assert.Equal(t, int64(100), row.Size)
	assert.Equal(t, "etag1", row.ETag)

	// Delta token should be committed in the same transaction.
	token := readDeltaToken(t, mgr.rawDB(), testDriveID)
	assert.Equal(t, "delta-token-1", token)
}

func TestCommitObservation_DeletedUnknownItem_Noop(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New(testDriveID)
	events := []ObservedItem{
		{
			DriveID:   driveID,
			ItemID:    "unknown",
			Path:      "gone.txt",
			ItemType:  "file",
			IsDeleted: true,
		},
	}

	err := mgr.CommitObservation(ctx, events, "delta-token-2", driveID)
	require.NoError(t, err)

	// Should NOT create a row for a deleted item we've never seen.
	row := readRemoteStateRow(t, mgr.rawDB(), "unknown")
	assert.Nil(t, row, "no row should exist for deleted unknown item")

	// Delta token should still be committed.
	token := readDeltaToken(t, mgr.rawDB(), testDriveID)
	assert.Equal(t, "delta-token-2", token)
}

func TestCommitObservation_SyncedSameHash_NoChange(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Pre-populate with a synced item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'synced', ?)`,
		testDriveID, "item1", "hello.txt", "hash1", 999,
	)
	require.NoError(t, err)

	// Observe same hash again (delta redelivery).
	events := []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			Path:     "hello.txt",
			ItemType: "file",
			Hash:     "hash1",
		},
	}

	err = mgr.CommitObservation(ctx, events, "delta-token-3", driveID)
	require.NoError(t, err)

	// Status should remain synced (no re-download on delta redelivery).
	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusSynced, row.SyncStatus, "should remain synced")
}

func TestCommitObservation_HashChange_ResetsFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(2000, 0) }
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Pre-populate with a failed item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'download_failed', ?)`,
		testDriveID, "item1", "hello.txt", "old-hash", 999,
	)
	require.NoError(t, err)

	// Observe with different hash.
	events := []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			Path:     "hello.txt",
			ItemType: "file",
			Hash:     "new-hash",
		},
	}

	err = mgr.CommitObservation(ctx, events, "delta-token-4", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusPendingDownload, row.SyncStatus)
	assert.Equal(t, "new-hash", row.Hash)
	// Failure state is tracked in sync_failures table, not remote_state.
}

func TestCommitObservation_MoveTracking_SetsPreviousPath(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(3000, 0) }
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Pre-populate with an item at old path.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'synced', ?)`,
		testDriveID, "item1", "old/hello.txt", "hash1", 999,
	)
	require.NoError(t, err)

	// Observe at new path.
	events := []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item1",
			Path:     "new/hello.txt",
			ItemType: "file",
			Hash:     "hash1",
		},
	}

	err = mgr.CommitObservation(ctx, events, "delta-token-5", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, "new/hello.txt", row.Path)
	assert.Equal(t, "old/hello.txt", row.PreviousPath, "should track previous path")
}

func TestCommitObservation_AtomicWithDeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New(testDriveID)

	// Multiple items + delta token in a single CommitObservation call.
	events := []ObservedItem{
		{
			DriveID: driveID, ItemID: "a", Path: "a.txt",
			ItemType: "file", Hash: "h1",
		},
		{
			DriveID: driveID, ItemID: "b", Path: "b.txt",
			ItemType: "file", Hash: "h2",
		},
	}

	err := mgr.CommitObservation(ctx, events, "atomic-token", driveID)
	require.NoError(t, err)

	// Both items and token should exist.
	assert.NotNil(t, readRemoteStateRow(t, mgr.rawDB(), "a"))
	assert.NotNil(t, readRemoteStateRow(t, mgr.rawDB(), "b"))
	assert.Equal(t, "atomic-token", readDeltaToken(t, mgr.rawDB(), testDriveID))
}

func TestCommitObservation_AllMatrixCells(t *testing.T) {
	t.Parallel()

	// Table-driven through key cells of the 30-cell matrix.
	tests := []struct {
		name           string
		existingStatus string
		existingHash   string
		observedHash   string
		isDeleted      bool
		wantStatus     string
		wantChanged    bool
	}{
		// Same hash, not deleted.
		{"synced+same→noop", statusSynced, "h1", "h1", false, statusSynced, false},
		{"pending_download+same→noop", statusPendingDownload, "h1", "h1", false, statusPendingDownload, false},
		{"download_failed+same→retry", statusDownloadFailed, "h1", "h1", false, statusPendingDownload, true},
		{"deleted+same→restore", statusDeleted, "h1", "h1", false, statusPendingDownload, true},

		// Different hash, not deleted.
		{"synced+diff→pending", statusSynced, "h1", "h2", false, statusPendingDownload, true},
		{"downloading+diff→pending", statusDownloading, "h1", "h2", false, statusPendingDownload, true},
		{"download_failed+diff→pending", statusDownloadFailed, "h1", "h2", false, statusPendingDownload, true},

		// Deleted.
		{"synced+deleted→pending_delete", statusSynced, "h1", "", true, statusPendingDelete, true},
		{"pending_download+deleted→pending_delete", statusPendingDownload, "h1", "", true, statusPendingDelete, true},
		{"pending_delete+deleted→noop", statusPendingDelete, "h1", "", true, statusPendingDelete, false},
		{"deleted+deleted→noop", statusDeleted, "h1", "", true, statusDeleted, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestManager(t)
			mgr.nowFunc = func() time.Time { return time.Unix(5000, 0) }
			ctx := context.Background()

			driveID := driveid.New(testDriveID)

			// Insert existing row.
			_, err := mgr.rawDB().ExecContext(ctx,
				`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
				VALUES (?, ?, ?, 'file', ?, ?, ?)`,
				testDriveID, "item1", "file.txt", tt.existingHash, tt.existingStatus, 999,
			)
			require.NoError(t, err)

			events := []ObservedItem{
				{
					DriveID:   driveID,
					ItemID:    "item1",
					Path:      "file.txt",
					ItemType:  "file",
					Hash:      tt.observedHash,
					IsDeleted: tt.isDeleted,
				},
			}

			err = mgr.CommitObservation(ctx, events, "token", driveID)
			require.NoError(t, err)

			row := readRemoteStateRow(t, mgr.rawDB(), "item1")
			require.NotNil(t, row)
			assert.Equal(t, tt.wantStatus, row.SyncStatus)
		})
	}
}

// ---------------------------------------------------------------------------
// RecordFailureWithStateTransition tests (migrated from old RecordFailure)
// ---------------------------------------------------------------------------

func TestRecordFailureWithStateTransition_TransitionsDownloading(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", "", "connection reset", 500, "")
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDownloadFailed, row.SyncStatus)

	// Failure metadata is now in sync_failures, not remote_state.
	var sfCount int
	var sfError string
	var sfHTTP int
	var sfRetry int64
	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT failure_count, last_error, http_status, next_retry_at FROM sync_failures WHERE path = ?",
		"hello.txt",
	).Scan(&sfCount, &sfError, &sfHTTP, &sfRetry)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)
	assert.Equal(t, "connection reset", sfError)
	assert.Equal(t, 500, sfHTTP)
	assert.Greater(t, sfRetry, int64(0), "should have next_retry_at set")
}

func TestRecordFailureWithStateTransition_OptimisticConcurrency_NoMatch(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a synced item (not downloading/deleting).
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'synced', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	// RecordFailureWithStateTransition should be a no-op for the state
	// transition (row not in downloading/deleting), but still records the
	// sync_failure entry.
	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", "", "some error", 500, "")
	require.NoError(t, err)

	// Status should remain synced.
	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusSynced, row.SyncStatus)
}

func TestRecordFailureWithStateTransition_IncreasesFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	callCount := 0
	mgr.nowFunc = func() time.Time {
		callCount++
		return time.Unix(int64(1000+callCount*100), 0)
	}
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	// First failure.
	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", "", "err1", 500, "")
	require.NoError(t, err)

	// Failure count is now in sync_failures.
	var sfCount int
	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT failure_count FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)

	// Set status back to downloading for second failure.
	_, err = mgr.rawDB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = 'downloading' WHERE item_id = ?`, "item1")
	require.NoError(t, err)

	// Second failure.
	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", "", "err2", 503, "")
	require.NoError(t, err)

	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT failure_count FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 2, sfCount)
}

func TestRecordFailureWithStateTransition_DeleteTransitionsDeleting(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a deleting item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'deleting', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", "", "permission denied", 403, "")
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDeleteFailed, row.SyncStatus)

	// Failure metadata is now in sync_failures.
	var sfCount int
	var sfHTTP int
	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT failure_count, http_status FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount, &sfHTTP)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)
	assert.Equal(t, 403, sfHTTP)
}

// ---------------------------------------------------------------------------
// RecordFailureWithStateTransition tests
// ---------------------------------------------------------------------------

func TestRecordFailureWithStateTransition_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", "", "connection reset", 500, "")
	require.NoError(t, err)

	// remote_state should transition to download_failed.
	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDownloadFailed, row.SyncStatus)

	// sync_failures should have the failure recorded.
	var sfCount int
	var sfError string
	var sfHTTP int
	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT failure_count, last_error, http_status FROM sync_failures WHERE path = ?",
		"hello.txt",
	).Scan(&sfCount, &sfError, &sfHTTP)
	require.NoError(t, err)
	assert.Equal(t, 1, sfCount)
	assert.Equal(t, "connection reset", sfError)
	assert.Equal(t, 500, sfHTTP)
}

func TestRecordFailureWithStateTransition_Delete(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a deleting item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'deleting', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"delete", "", "permission denied", 403, "")
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDeleteFailed, row.SyncStatus)
}

func TestRecordFailureWithStateTransition_SetsIssueTypeAndScopeKey(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailureWithStateTransition(ctx, "hello.txt", driveid.New(testDriveID),
		"download", IssueQuotaExceeded, "quota full", 507, "quota:own")
	require.NoError(t, err)

	// Verify issue_type, scope_key, and category are set correctly.
	var issueType, scopeKey, category string
	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT issue_type, scope_key, category FROM sync_failures WHERE path = ?",
		"hello.txt",
	).Scan(&issueType, &scopeKey, &category)
	require.NoError(t, err)
	assert.Equal(t, IssueQuotaExceeded, issueType)
	assert.Equal(t, "quota:own", scopeKey)
	assert.Equal(t, "actionable", category, "quota_exceeded is an actionable issue")
}

func TestRecordFailure_BackoffCalculation(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)

	tests := []struct {
		name         string
		failureCount int
		wantMinSec   int64 // minimum seconds from now
		wantMaxSec   int64 // maximum seconds from now
	}{
		{"first failure", 0, 20, 45},  // 30s base ± jitter
		{"second failure", 1, 45, 90}, // 60s ± jitter
		{"third failure", 2, 90, 180}, // 120s ± jitter
		{"capped", 10, 2700, 4500},    // should not exceed 3600
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			retry := computeNextRetry(now, tt.failureCount)
			diffSec := retry.Unix() - now.Unix()
			assert.GreaterOrEqual(t, diffSec, tt.wantMinSec,
				"retry should be at least %ds from now", tt.wantMinSec)
			assert.LessOrEqual(t, diffSec, tt.wantMaxSec,
				"retry should be at most %ds from now", tt.wantMaxSec)
		})
	}
}

// ---------------------------------------------------------------------------
// CommitOutcome remote_state extension tests
// ---------------------------------------------------------------------------

func TestCommitOutcome_UpdatesRemoteState_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(5000, 0) }
	ctx := context.Background()

	// Load baseline so CommitOutcome can update cache.
	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a downloading remote_state row.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", "hash1", 999,
	)
	require.NoError(t, err)

	outcome := &Outcome{
		Action:     ActionDownload,
		Success:    true,
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hash1",
		RemoteHash: "hash1",
		Size:       100,
		Mtime:      2000000,
		ETag:       "etag1",
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusSynced, row.SyncStatus)
}

func TestCommitOutcome_HashGuard_PreventsStaleOverwrite(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(5000, 0) }
	ctx := context.Background()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a downloading row with hash "new-hash" (new observation arrived).
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", "new-hash", 999,
	)
	require.NoError(t, err)

	// Outcome from old download with old hash.
	outcome := &Outcome{
		Action:     ActionDownload,
		Success:    true,
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "old-hash",
		RemoteHash: "old-hash",
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	// Should NOT transition to synced (hash mismatch guard).
	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDownloading, row.SyncStatus, "hash guard should prevent stale overwrite")
}

func TestCommitOutcome_Upload_UnconditionalUpdate(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(5000, 0) }
	ctx := context.Background()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a remote_state row in any status.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, 'pending_download', ?)`,
		testDriveID, "item1", "hello.txt", "old-hash", 999,
	)
	require.NoError(t, err)

	outcome := &Outcome{
		Action:     ActionUpload,
		Success:    true,
		Path:       "hello.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "upload-hash",
		RemoteHash: "upload-hash",
		Size:       500,
		Mtime:      3000000,
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusSynced, row.SyncStatus)
	assert.Equal(t, "upload-hash", row.Hash)
	assert.Equal(t, int64(500), row.Size)
}

// ---------------------------------------------------------------------------
// StateReader + StateAdmin tests
// ---------------------------------------------------------------------------

func TestListUnreconciled(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	// Insert rows in various states.
	for _, s := range []struct {
		id     string
		status string
	}{
		{"a", statusPendingDownload},
		{"b", statusSynced},
		{"c", statusDownloadFailed},
		{"d", statusDeleted},
		{"e", statusPendingDelete},
		{"f", statusFiltered},
	} {
		_, err := mgr.rawDB().ExecContext(ctx,
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

	mgr := newTestManager(t)
	ctx := context.Background()

	// Insert rows with various statuses.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "pending", "pending.txt", statusPendingDownload, 999,
	)
	require.NoError(t, err)

	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "failed", "failed.txt", statusDownloadFailed, 999,
	)
	require.NoError(t, err)

	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "synced", "synced.txt", statusSynced, 999,
	)
	require.NoError(t, err)

	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "del-failed", "del-failed.txt", statusDeleteFailed, 999,
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

	mgr := newTestManager(t)
	ctx := context.Background()
	nowNano := time.Now().UnixNano()

	// SyncFailureCount queries sync_failures (category='transient'), not remote_state.
	for _, s := range []struct {
		path      string
		category  string
		direction string
	}{
		{"a.txt", "transient", "download"},
		{"b.txt", "transient", "delete"},
		{"c.txt", "actionable", "download"}, // actionable should not be counted
	} {
		_, err := mgr.rawDB().ExecContext(ctx,
			`INSERT INTO sync_failures (path, drive_id, direction, category, failure_count, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, ?, 1, ?, ?)`,
			s.path, testDriveID, s.direction, s.category, nowNano, nowNano,
		)
		require.NoError(t, err)
	}

	count, err := mgr.SyncFailureCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "should count transient sync_failures only")
}

func TestResetFailure(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()
	nowNano := time.Now().UnixNano()

	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "item1", "hello.txt", statusDownloadFailed, 999,
	)
	require.NoError(t, err)

	// Insert a corresponding sync_failures row.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, category, failure_count,
			next_retry_at, last_error, first_seen_at, last_seen_at)
		VALUES (?, ?, 'download', 'transient', 5, 9999, 'old error', ?, ?)`,
		"hello.txt", testDriveID, nowNano, nowNano,
	)
	require.NoError(t, err)

	err = mgr.ResetFailure(ctx, "hello.txt")
	require.NoError(t, err)

	// remote_state should be transitioned to pending_download.
	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusPendingDownload, row.SyncStatus)

	// sync_failures row should be deleted.
	var sfCount int
	err = mgr.rawDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sync_failures WHERE path = ?", "hello.txt",
	).Scan(&sfCount)
	require.NoError(t, err)
	assert.Equal(t, 0, sfCount, "sync_failures row should be removed")
}

func TestResetAllFailures(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	for _, s := range []struct {
		id     string
		status string
	}{
		{"a", statusDownloadFailed},
		{"b", statusDeleteFailed},
		{"c", statusSynced},
	} {
		_, err := mgr.rawDB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	err := mgr.ResetAllFailures(ctx)
	require.NoError(t, err)

	// download_failed should become pending_download.
	rowA := readRemoteStateRow(t, mgr.rawDB(), "a")
	assert.Equal(t, statusPendingDownload, rowA.SyncStatus)

	// delete_failed should become pending_delete.
	rowB := readRemoteStateRow(t, mgr.rawDB(), "b")
	assert.Equal(t, statusPendingDelete, rowB.SyncStatus)

	// synced should not change.
	rowC := readRemoteStateRow(t, mgr.rawDB(), "c")
	assert.Equal(t, statusSynced, rowC.SyncStatus)
}

func TestResetInProgressStates(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()
	syncRoot := t.TempDir()

	// Create file for deleting item "b" so it's reset to pending_delete.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "b.txt"), []byte("x"), 0o600))

	for _, s := range []struct {
		id     string
		status string
	}{
		{"a", statusDownloading},
		{"b", statusDeleting}, // file exists → pending_delete
		{"c", statusSynced},
		{"d", statusPendingDownload},
	} {
		_, err := mgr.rawDB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	err := mgr.ResetInProgressStates(ctx, syncRoot)
	require.NoError(t, err)

	rowA := readRemoteStateRow(t, mgr.rawDB(), "a")
	assert.Equal(t, statusPendingDownload, rowA.SyncStatus, "downloading→pending_download")

	rowB := readRemoteStateRow(t, mgr.rawDB(), "b")
	assert.Equal(t, statusPendingDelete, rowB.SyncStatus, "deleting+file exists→pending_delete")

	rowC := readRemoteStateRow(t, mgr.rawDB(), "c")
	assert.Equal(t, statusSynced, rowC.SyncStatus, "synced unchanged")

	rowD := readRemoteStateRow(t, mgr.rawDB(), "d")
	assert.Equal(t, statusPendingDownload, rowD.SyncStatus, "pending_download unchanged")
}

func TestCommitOutcome_LocalDelete_MarksDeleted(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(5000, 0) }
	ctx := context.Background()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a deleting remote_state row.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'deleting', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	outcome := &Outcome{
		Action:  ActionLocalDelete,
		Success: true,
		Path:    "hello.txt",
		DriveID: driveid.New(testDriveID),
		ItemID:  "item1",
	}

	err = mgr.CommitOutcome(ctx, outcome)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDeleted, row.SyncStatus)
}
