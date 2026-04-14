package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.5.6
func TestNewSyncStore_AppliesCurrentGooseMigration(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	var version int64
	err := store.DB().QueryRowContext(t.Context(), `
		SELECT version_id
		FROM goose_db_version
		WHERE is_applied = 1
		ORDER BY version_id DESC
		LIMIT 1`).Scan(&version)
	require.NoError(t, err)
	assert.Equal(t, currentMigrationVersion, version)
}

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
		integrityCodeMissingScopeBlock,
		integrityCodeLegacyRemoteBoundary,
		integrityCodeVisibleProjectionOverlap,
	})
}

// Validates: R-2.3.6, R-2.3.12
func TestSyncStore_AuditIntegrityReportsDurableIntentWorkflowProblems(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.DB().ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO conflicts
			(id, drive_id, path, conflict_type, detected_at, resolution, resolved_at, resolved_by)
		VALUES
			('conflict-missing-request', ?, '/conflict-a.txt', 'edit_edit', 1, 'unresolved', NULL, NULL),
			('conflict-missing-resolving-at', ?, '/conflict-b.txt', 'edit_edit', 2, 'unresolved', NULL, NULL),
			('conflict-resolved-unresolved', ?, '/conflict-c.txt', 'edit_edit', 3, 'unresolved', 33, 'user'),
			('conflict-invalid-state', ?, '/conflict-d.txt', 'edit_edit', 4, 'unresolved', NULL, NULL),
			('conflict-invalid-resolution', ?, '/conflict-e.txt', 'edit_edit', 5, 'manual', NULL, NULL),
			('conflict-request-on-resolved', ?, '/conflict-f.txt', 'edit_edit', 6, 'keep_local', 66, 'user');
		INSERT INTO conflict_requests
			(conflict_id, requested_resolution, state, requested_at, applying_at, last_error)
		VALUES
			('conflict-missing-request', '', 'queued', NULL, NULL, NULL),
			('conflict-missing-resolving-at', 'keep_local', 'applying', 2, NULL, NULL),
			('conflict-invalid-state', 'keep_remote', 'manual', 4, NULL, NULL),
			('conflict-request-on-resolved', 'keep_remote', 'queued', 6, NULL, NULL),
			('conflict-orphaned', 'keep_both', 'queued', 7, NULL, NULL);
		INSERT INTO held_deletes
			(drive_id, action_type, path, item_id, state, held_at, approved_at, last_planned_at)
		VALUES
			(?, 'remote_delete', '/delete-a.txt', '', 'held', 1, NULL, 1),
			(?, 'remote_delete', '/delete-b.txt', 'item-b', 'approved', 1, NULL, 1),
			(?, 'remote_delete', '/delete-c.txt', 'item-c', 'manual', 1, NULL, 1)`,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `PRAGMA ignore_check_constraints = OFF`)
	require.NoError(t, err)

	report, err := store.AuditIntegrity(ctx)
	require.NoError(t, err)

	var codes []string
	var details []string
	for _, finding := range report.Findings {
		codes = append(codes, finding.Code)
		details = append(details, finding.Detail)
	}

	assert.Contains(t, codes, integrityCodeInvalidConflictWorkflow)
	assert.Contains(t, codes, integrityCodeInvalidHeldDelete)
	assert.Contains(t, details, "conflict conflict-missing-request is queued without requested_resolution")
	assert.Contains(t, details, "conflict conflict-missing-resolving-at is applying without applying_at")
	assert.Contains(t, details, "conflict conflict-resolved-unresolved is unresolved with resolved_at set")
	assert.Contains(t, details, `conflict conflict-invalid-state has invalid workflow state "manual"`)
	assert.Contains(t, details, `conflict conflict-invalid-resolution has invalid final resolution "manual"`)
	assert.Contains(t, details, "conflict request conflict-request-on-resolved targets already resolved conflict")
	assert.Contains(t, details, "conflict request conflict-orphaned has no conflict row")
	assert.Contains(t, details, "held delete /delete-a.txt is missing item_id")
	assert.Contains(t, details, "approved held delete /delete-b.txt is missing approved_at")
	assert.Contains(t, details, `held delete /delete-c.txt has invalid state "manual"`)
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
	assert.Equal(t, SKAuthAccount(), blocks[0].Key)
	assert.Equal(t, ScopeTimingNone, blocks[0].TimingSource)
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
		case "Shared/Docs/draft.txt":
			heldRowFound = true
			assert.Equal(t, FailureRoleHeld, row.Role)
		}
	}
	assert.True(t, itemRowFound)
	assert.True(t, heldRowFound)
}

// Validates: R-2.3.6, R-2.3.12
func TestSyncStore_RepairIntegritySafePreservesDurableUserIntent(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveID,
		ActionType:    ActionRemoteDelete,
		Path:          "/delete-me.txt",
		ItemID:        "item-delete",
		State:         HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.ApproveHeldDeletes(ctx))

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO conflicts
			(id, drive_id, path, conflict_type, detected_at, resolution)
		VALUES
			('conflict-requested', ?, '/conflict.txt', 'edit_edit', 1, 'unresolved')`,
		testDriveID,
	)
	require.NoError(t, err)
	result, err := store.RequestConflictResolution(ctx, "conflict-requested", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	repairs, err := store.RepairIntegritySafe(ctx)
	require.NoError(t, err)
	assert.Zero(t, repairs)

	approved, err := store.ListHeldDeletesByState(ctx, HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "item-delete", approved[0].ItemID)

	request, err := store.GetConflictRequest(ctx, "conflict-requested")
	require.NoError(t, err)
	assert.Equal(t, ConflictStateQueued, request.State)
	assert.Equal(t, ResolutionKeepLocal, request.RequestedResolution)
}

// Validates: R-2.10.47
func TestSyncStore_RepairIntegritySafe_ReleasesLegacyThrottleAccountScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	now := time.Now().UTC()

	require.NoError(t, store.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:           SKThrottleAccount(),
		IssueType:     IssueRateLimited,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	}))
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "shared/rate-limited.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueRateLimited,
		ErrMsg:     "rate limited",
		ScopeKey:   SKThrottleAccount(),
	})
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "scope-boundary",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleBoundary,
		Category:   CategoryActionable,
		IssueType:  IssueRateLimited,
		ErrMsg:     "boundary",
		ScopeKey:   SKThrottleAccount(),
	})

	repairs, err := store.RepairIntegritySafe(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, repairs, 3)

	blocks, err := store.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks, "legacy throttle:account scope should be removed during safe repair")

	rows, err := store.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, FailureRoleItem, rows[0].Role)
	assert.Positive(t, rows[0].NextRetryAt, "held legacy throttle failures should become retryable immediately")
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
		SKAuthAccount().String(),
		IssueUnauthorized,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
		int64(time.Minute),
		time.Date(2026, 4, 3, 11, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC).UnixNano(),
		SKPermRemoteWrite("Shared/Docs").String(),
		IssueRemoteWriteDenied,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "docs/CON",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleItem,
		Category:   CategoryActionable,
		IssueType:  IssueInvalidFilename,
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
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "service/failure.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "service unavailable",
		ScopeKey:   SKService(),
	})
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "auth/account",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleBoundary,
		Category:   CategoryActionable,
		IssueType:  IssueUnauthorized,
		ErrMsg:     "auth required",
		ScopeKey:   SKAuthAccount(),
	})
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "Shared/Docs",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionFolderCreate,
		Role:       FailureRoleBoundary,
		Category:   CategoryActionable,
		IssueType:  IssueRemoteWriteDenied,
		ErrMsg:     "legacy remote boundary",
		ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
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
		SKAuthAccount().String(),
		IssueUnauthorized,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
		int64(5*time.Minute),
		time.Date(2026, 4, 3, 10, 5, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 4, 3, 10, 10, 0, 0, time.UTC).UnixNano(),
		SKPermRemoteWrite("Shared/Docs").String(),
		IssueRemoteWriteDenied,
		time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "docs/CON",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleItem,
		Category:   CategoryActionable,
		IssueType:  IssueInvalidFilename,
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
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "Shared/Docs",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionFolderCreate,
		Role:       FailureRoleBoundary,
		Category:   CategoryActionable,
		IssueType:  IssueRemoteWriteDenied,
		ErrMsg:     "legacy remote boundary",
		ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
	})
	recordIntegrityFailure(t, store, ctx, &SyncFailureParams{
		Path:       "Shared/Docs/draft.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueRemoteWriteDenied,
		ErrMsg:     "read-only",
		ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
	})
}

func recordIntegrityFailure(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	params *SyncFailureParams,
) {
	t.Helper()
	require.NoError(t, store.RecordFailure(ctx, params, nil))
}
