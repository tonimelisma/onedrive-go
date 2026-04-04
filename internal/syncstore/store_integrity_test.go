package syncstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.10.47
func TestInspector_AuditIntegrityReportsPersistedProblems(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	seedAuditIntegrityProblems(t, store, ctx, driveID)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	report, err := inspector.AuditIntegrity(ctx)
	require.NoError(t, err)
	require.True(t, report.HasFindings())

	var codes []string
	for _, finding := range report.Findings {
		codes = append(codes, finding.Code)
	}

	assert.Subset(t, codes, []string{
		integrityCodeInvalidAuthScopeTiming,
		integrityCodeLegacyRemoteScope,
		integrityCodeInvalidFailureTiming,
		integrityCodeInvalidManualTrial,
		integrityCodeMissingScopeBlock,
		integrityCodeLegacyRemoteBoundary,
		integrityCodeVisibleProjectionOverlap,
	})
}

// Validates: R-2.10.47
func TestSyncStore_RepairIntegritySafeNormalizesDeterministicViolations(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	seedRepairIntegrityProblems(t, store, ctx, driveID)

	repairs, err := store.RepairIntegritySafe(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, repairs, 4)

	report, err := store.AuditIntegrity(ctx)
	require.NoError(t, err)
	assert.False(t, report.HasFindings(), "safe repairs should clear deterministic audit findings")

	blocks, err := store.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, synctypes.SKAuthAccount(), blocks[0].Key)
	assert.Equal(t, synctypes.ScopeTimingNone, blocks[0].TimingSource)
	assert.Zero(t, blocks[0].TrialInterval)
	assert.True(t, blocks[0].NextTrialAt.IsZero())
	assert.True(t, blocks[0].PreserveUntil.IsZero())
	assert.Zero(t, blocks[0].TrialCount)

	rows, err := store.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	var (
		heldRowFound bool
		itemRowFound bool
	)
	for _, row := range rows {
		switch row.Path {
		case "docs/CON":
			itemRowFound = true
			assert.Zero(t, row.NextRetryAt)
			assert.Zero(t, row.ManualTrialRequestedAt)
		case "Shared/Docs/draft.txt":
			heldRowFound = true
			assert.Equal(t, synctypes.FailureRoleHeld, row.Role)
		}
	}
	assert.True(t, itemRowFound)
	assert.True(t, heldRowFound)
}

// Validates: R-6.7.17
func TestSyncStore_AuditIntegrityIncludesBaselineCacheMismatch(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.DB().ExecContext(ctx, `INSERT INTO baseline
		(path, drive_id, item_id, parent_id, item_type, synced_at, local_hash)
		VALUES ('/docs/file.txt', ?, 'item-1', 'root', 'file', 1, 'hash-a')`, testDriveID)
	require.NoError(t, err)

	_, err = store.Load(ctx)
	require.NoError(t, err)

	store.baselineMu.Lock()
	store.baseline.ByPath["/docs/file.txt"].LocalHash = "corrupted-hash"
	store.baselineMu.Unlock()

	report, err := store.AuditIntegrity(ctx)
	require.NoError(t, err)

	var codes []string
	for _, finding := range report.Findings {
		codes = append(codes, finding.Code)
	}
	assert.Contains(t, codes, integrityCodeBaselineCacheMismatch)
}

func seedAuditIntegrityProblems(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	driveID driveid.ID,
) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO scope_blocks
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES
			(?, ?, 'backoff', ?, ?, ?, ?, 2),
			(?, ?, 'none', ?, 0, 0, 0, 0)`,
		synctypes.SKAuthAccount().String(),
		synctypes.IssueUnauthorized,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
		int64(time.Minute),
		time.Date(2026, 4, 3, 11, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC).UnixNano(),
		synctypes.SKPermRemote("Shared/Docs").String(),
		synctypes.IssueSharedFolderBlocked,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "docs/CON",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleItem,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssueInvalidFilename,
		ErrMsg:     "reserved name",
	})
	_, err = store.DB().ExecContext(ctx, `
		UPDATE sync_failures
		SET next_retry_at = ?
		WHERE path = ?`,
		time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC).UnixNano(),
		"docs/CON",
	)
	require.NoError(t, err)
	execIgnoringCheckConstraints(
		t,
		ctx,
		store.DB(),
		`UPDATE sync_failures SET manual_trial_requested_at = ? WHERE path = ?`,
		time.Date(2026, 4, 3, 12, 30, 0, 0, time.UTC).UnixNano(),
		"docs/CON",
	)

	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "service/failure.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueServiceOutage,
		ErrMsg:     "service unavailable",
		ScopeKey:   synctypes.SKService(),
	})
	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "auth/account",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleBoundary,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssueUnauthorized,
		ErrMsg:     "auth required",
		ScopeKey:   synctypes.SKAuthAccount(),
	})
	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionFolderCreate,
		Role:       synctypes.FailureRoleBoundary,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssuePermissionDenied,
		ErrMsg:     "legacy remote boundary",
		ScopeKey:   synctypes.SKPermRemote("Shared/Docs"),
	})
}

func seedRepairIntegrityProblems(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	driveID driveid.ID,
) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO scope_blocks
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES
			(?, ?, 'backoff', ?, ?, ?, ?, 3),
			(?, ?, 'none', ?, 0, 0, 0, 0)`,
		synctypes.SKAuthAccount().String(),
		synctypes.IssueUnauthorized,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
		int64(5*time.Minute),
		time.Date(2026, 4, 3, 10, 5, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 4, 3, 10, 10, 0, 0, time.UTC).UnixNano(),
		synctypes.SKPermRemote("Shared/Docs").String(),
		synctypes.IssueSharedFolderBlocked,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "docs/CON",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleItem,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssueInvalidFilename,
		ErrMsg:     "reserved name",
	})
	_, err = store.DB().ExecContext(ctx, `
		UPDATE sync_failures
		SET next_retry_at = ?
		WHERE path = ?`,
		time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC).UnixNano(),
		"docs/CON",
	)
	require.NoError(t, err)
	execIgnoringCheckConstraints(
		t,
		ctx,
		store.DB(),
		`UPDATE sync_failures SET manual_trial_requested_at = ? WHERE path = ?`,
		time.Date(2026, 4, 3, 12, 30, 0, 0, time.UTC).UnixNano(),
		"docs/CON",
	)

	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionFolderCreate,
		Role:       synctypes.FailureRoleBoundary,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssuePermissionDenied,
		ErrMsg:     "legacy remote boundary",
		ScopeKey:   synctypes.SKPermRemote("Shared/Docs"),
	})
	recordIntegrityFailure(t, store, ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs/draft.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "read-only",
		ScopeKey:   synctypes.SKPermRemote("Shared/Docs"),
	})
}

func recordIntegrityFailure(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	params *synctypes.SyncFailureParams,
) {
	t.Helper()
	require.NoError(t, store.RecordFailure(ctx, params, nil))
}

func execIgnoringCheckConstraints(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	query string,
	args ...any,
) {
	t.Helper()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, conn.Close())
	}()

	_, err = conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`)
	require.NoError(t, err)
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() {
		_, pragmaErr := conn.ExecContext(cleanupCtx, `PRAGMA ignore_check_constraints = OFF`)
		assert.NoError(t, pragmaErr)
	}()

	_, err = conn.ExecContext(ctx, query, args...)
	require.NoError(t, err)
}
