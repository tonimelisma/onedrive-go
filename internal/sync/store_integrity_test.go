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
func TestNewSyncStore_StoresObservationStateRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	state, err := store.ReadObservationState(t.Context())
	require.NoError(t, err)
	require.NotNil(t, state)

	var configuredDriveID string
	err = store.DB().QueryRowContext(t.Context(), `
		SELECT configured_drive_id
		FROM observation_state
		WHERE singleton_id = 1`).Scan(&configuredDriveID)
	require.NoError(t, err)
	assert.Empty(t, configuredDriveID)
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
		integrityCodeLegacyRemoteScope,
		integrityCodeInvalidFailureTiming,
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
	assert.GreaterOrEqual(t, repairs, 3)

	report, err := store.AuditIntegrity(ctx)
	require.NoError(t, err)
	assert.False(t, report.HasFindings(), "safe repairs should clear deterministic audit findings")

	blocks, err := store.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

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
	require.NoError(t, store.CommitObservationCursor(ctx, driveid.New(testDriveID), ""))

	_, err := store.DB().ExecContext(ctx, `INSERT INTO baseline
		(path, item_id, parent_id, item_type, local_hash)
		VALUES ('/docs/file.txt', 'item-1', 'root', 'file', 'hash-a')`)
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
		VALUES (?, ?, 'none', ?, 0, 0, 0, 0)`,
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
		VALUES (?, ?, 'none', ?, 0, 0, 0, 0)`,
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
