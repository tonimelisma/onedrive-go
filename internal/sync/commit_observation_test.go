package sync

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// readRemoteStateRow is a test helper that reads a single remote_state row.
func readRemoteStateRow(t *testing.T, db *sql.DB, driveID, itemID string) *RemoteStateRow {
	t.Helper()

	var (
		row        RemoteStateRow
		parentID   sql.NullString
		hash       sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
		prevPath   sql.NullString
		nextRetry  sql.NullInt64
		lastError  sql.NullString
		httpStatus sql.NullInt64
	)

	err := db.QueryRow(
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at, failure_count, next_retry_at, last_error, http_status
		FROM remote_state WHERE drive_id = ? AND item_id = ?`,
		driveID, itemID,
	).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt, &row.FailureCount,
		&nextRetry, &lastError, &httpStatus,
	)
	if err == sql.ErrNoRows {
		return nil
	}

	require.NoError(t, err, "scanning remote_state row")

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String
	row.LastError = lastError.String

	if size.Valid {
		row.Size = size.Int64
	}

	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	if nextRetry.Valid {
		row.NextRetryAt = nextRetry.Int64
	}

	if httpStatus.Valid {
		row.HTTPStatus = int(httpStatus.Int64)
	}

	return &row
}

// readDeltaToken is a test helper that reads the delta token for a drive.
func readDeltaToken(t *testing.T, db *sql.DB, driveID string) string {
	t.Helper()

	var token string

	err := db.QueryRow(
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

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row, "row should exist")
	assert.Equal(t, "hello.txt", row.Path)
	assert.Equal(t, statusPendingDownload, row.SyncStatus)
	assert.Equal(t, "hash1", row.Hash)
	assert.Equal(t, int64(100), row.Size)
	assert.Equal(t, "etag1", row.ETag)
	assert.Equal(t, 0, row.FailureCount)

	// Delta token should be committed in the same transaction.
	token := readDeltaToken(t, mgr.DB(), testDriveID)
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
	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "unknown")
	assert.Nil(t, row, "no row should exist for deleted unknown item")

	// Delta token should still be committed.
	token := readDeltaToken(t, mgr.DB(), testDriveID)
	assert.Equal(t, "delta-token-2", token)
}

func TestCommitObservation_SyncedSameHash_NoChange(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
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
	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
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
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, sync_status,
			observed_at, failure_count, next_retry_at, last_error)
		VALUES (?, ?, ?, 'file', ?, 'download_failed', ?, 5, ?, 'some error')`,
		testDriveID, "item1", "hello.txt", "old-hash", 999, 1500,
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

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusPendingDownload, row.SyncStatus)
	assert.Equal(t, "new-hash", row.Hash)
	assert.Equal(t, 0, row.FailureCount, "failure count should reset on hash change")
	assert.Equal(t, int64(0), row.NextRetryAt, "next_retry_at should be cleared")
}

func TestCommitObservation_MoveTracking_SetsPreviousPath(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(3000, 0) }
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

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
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
	assert.NotNil(t, readRemoteStateRow(t, mgr.DB(), testDriveID, "a"))
	assert.NotNil(t, readRemoteStateRow(t, mgr.DB(), testDriveID, "b"))
	assert.Equal(t, "atomic-token", readDeltaToken(t, mgr.DB(), testDriveID))
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
			_, err := mgr.DB().ExecContext(ctx,
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

			row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
			require.NotNil(t, row)
			assert.Equal(t, tt.wantStatus, row.SyncStatus)
		})
	}
}

// ---------------------------------------------------------------------------
// RecordFailure tests
// ---------------------------------------------------------------------------

func TestRecordFailure_TransitionsDownloading(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'downloading', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailure(ctx, "hello.txt", "connection reset", 500)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDownloadFailed, row.SyncStatus)
	assert.Equal(t, 1, row.FailureCount)
	assert.Equal(t, "connection reset", row.LastError)
	assert.Equal(t, 500, row.HTTPStatus)
	assert.Greater(t, row.NextRetryAt, int64(0), "should have next_retry_at set")
}

func TestRecordFailure_OptimisticConcurrency_NoMatch(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a synced item (not downloading/deleting).
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'synced', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	// RecordFailure should be a no-op (row not in downloading/deleting).
	err = mgr.RecordFailure(ctx, "hello.txt", "some error", 500)
	require.NoError(t, err)

	// Status should remain synced.
	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusSynced, row.SyncStatus)
}

func TestRecordFailure_IncreasesFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	callCount := 0
	mgr.nowFunc = func() time.Time {
		callCount++
		return time.Unix(int64(1000+callCount*100), 0)
	}
	ctx := context.Background()

	// Insert a downloading item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at, failure_count)
		VALUES (?, ?, ?, 'file', 'downloading', ?, 0)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	// First failure.
	err = mgr.RecordFailure(ctx, "hello.txt", "err1", 500)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, 1, row.FailureCount)

	// Set status back to downloading for second failure.
	_, err = mgr.DB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = 'downloading' WHERE item_id = ?`, "item1")
	require.NoError(t, err)

	// Second failure.
	err = mgr.RecordFailure(ctx, "hello.txt", "err2", 503)
	require.NoError(t, err)

	row = readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, 2, row.FailureCount)
}

func TestRecordFailure_DeleteTransitionsDeleting(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()

	// Insert a deleting item.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', 'deleting', ?)`,
		testDriveID, "item1", "hello.txt", 999,
	)
	require.NoError(t, err)

	err = mgr.RecordFailure(ctx, "hello.txt", "permission denied", 403)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDeleteFailed, row.SyncStatus)
	assert.Equal(t, 1, row.FailureCount)
	assert.Equal(t, 403, row.HTTPStatus)
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
		{"first failure", 0, 20, 45},     // 30s base ± jitter
		{"second failure", 1, 45, 90},    // 60s ± jitter
		{"third failure", 2, 90, 180},    // 120s ± jitter
		{"capped", 10, 2700, 4500},       // should not exceed 3600
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
	_, err = mgr.DB().ExecContext(ctx,
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

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
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
	_, err = mgr.DB().ExecContext(ctx,
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
	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
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
	_, err = mgr.DB().ExecContext(ctx,
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

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
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

	mgr := newTestManager(t)
	ctx := context.Background()

	now := time.Unix(5000, 0)

	// Insert: one ready for retry, one not yet.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at, next_retry_at)
		VALUES (?, ?, ?, 'file', ?, ?, ?)`,
		testDriveID, "ready", "ready.txt", statusDownloadFailed, 999, now.Add(-time.Minute).UnixNano(),
	)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at, next_retry_at)
		VALUES (?, ?, ?, 'file', ?, ?, ?)`,
		testDriveID, "not-ready", "not-ready.txt", statusDownloadFailed, 999, now.Add(time.Hour).UnixNano(),
	)
	require.NoError(t, err)

	// One with NULL next_retry_at (new pending item).
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, ?, ?, 'file', ?, ?)`,
		testDriveID, "null-retry", "null-retry.txt", statusPendingDownload, 999,
	)
	require.NoError(t, err)

	rows, err := mgr.ListFailedForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "should include ready + null-retry")
}

func TestFailureCount(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	// Insert some rows.
	for _, s := range []struct {
		id     string
		status string
	}{
		{"a", statusDownloadFailed},
		{"b", statusDeleteFailed},
		{"c", statusSynced},
		{"d", statusPendingDownload},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	count, err := mgr.FailureCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "should count download_failed + delete_failed")
}

func TestResetFailure(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at,
			failure_count, next_retry_at, last_error)
		VALUES (?, ?, ?, 'file', ?, ?, 5, 9999, 'old error')`,
		testDriveID, "item1", "hello.txt", statusDownloadFailed, 999,
	)
	require.NoError(t, err)

	err = mgr.ResetFailure(ctx, "hello.txt")
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusPendingDownload, row.SyncStatus)
	assert.Equal(t, 0, row.FailureCount)
	assert.Equal(t, int64(0), row.NextRetryAt)
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
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at, failure_count)
			VALUES (?, ?, ?, 'file', ?, ?, 3)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	err := mgr.ResetAllFailures(ctx)
	require.NoError(t, err)

	// download_failed should become pending_download.
	rowA := readRemoteStateRow(t, mgr.DB(), testDriveID, "a")
	assert.Equal(t, statusPendingDownload, rowA.SyncStatus)

	// delete_failed should become pending_delete.
	rowB := readRemoteStateRow(t, mgr.DB(), testDriveID, "b")
	assert.Equal(t, statusPendingDelete, rowB.SyncStatus)

	// synced should not change.
	rowC := readRemoteStateRow(t, mgr.DB(), testDriveID, "c")
	assert.Equal(t, statusSynced, rowC.SyncStatus)
}

func TestResetInProgressStates(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	for _, s := range []struct {
		id     string
		status string
	}{
		{"a", statusDownloading},
		{"b", statusDeleting},
		{"c", statusSynced},
		{"d", statusPendingDownload},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			VALUES (?, ?, ?, 'file', ?, ?)`,
			testDriveID, s.id, s.id+".txt", s.status, 999,
		)
		require.NoError(t, err)
	}

	err := mgr.ResetInProgressStates(ctx)
	require.NoError(t, err)

	rowA := readRemoteStateRow(t, mgr.DB(), testDriveID, "a")
	assert.Equal(t, statusPendingDownload, rowA.SyncStatus, "downloading→pending_download")

	rowB := readRemoteStateRow(t, mgr.DB(), testDriveID, "b")
	assert.Equal(t, statusPendingDelete, rowB.SyncStatus, "deleting→pending_delete")

	rowC := readRemoteStateRow(t, mgr.DB(), testDriveID, "c")
	assert.Equal(t, statusSynced, rowC.SyncStatus, "synced unchanged")

	rowD := readRemoteStateRow(t, mgr.DB(), testDriveID, "d")
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
	_, err = mgr.DB().ExecContext(ctx,
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

	row := readRemoteStateRow(t, mgr.DB(), testDriveID, "item1")
	require.NotNil(t, row)
	assert.Equal(t, statusDeleted, row.SyncStatus)
}
