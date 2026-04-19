package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// UpsertBlockScope
// ---------------------------------------------------------------------------

// Validates: R-2.10.8
func TestSyncStore_UpsertBlockScope(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	block := &BlockScope{
		Key:           SKThrottleDrive(driveid.New("0000000000000001")),
		IssueType:     IssueRateLimited,
		TimingSource:  ScopeTimingServerRetryAfter,
		BlockedAt:     time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   time.Date(2025, 6, 15, 10, 0, 5, 0, time.UTC),
		TrialCount:    0,
	}

	err := mgr.UpsertBlockScope(ctx, block)
	require.NoError(t, err)

	// Verify by listing.
	blocks, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKThrottleDrive(driveid.New("0000000000000001")), blocks[0].Key)
	assert.Equal(t, IssueRateLimited, blocks[0].IssueType)
	assert.Equal(t, 0, blocks[0].TrialCount)

	// Upsert with updated values — should replace.
	block.TrialCount = 3
	block.TrialInterval = 20 * time.Second
	err = mgr.UpsertBlockScope(ctx, block)
	require.NoError(t, err)

	blocks, err = mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "upsert should not create duplicate rows")
	assert.Equal(t, 3, blocks[0].TrialCount, "upsert should update trial count")
	assert.Equal(t, 20*time.Second, blocks[0].TrialInterval, "upsert should update interval")
}

// ---------------------------------------------------------------------------
// DeleteBlockScope
// ---------------------------------------------------------------------------

// Validates: R-2.10.8
func TestSyncStore_DeleteBlockScope(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	block := &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Now().UTC(),
		TrialInterval: 10 * time.Second,
		NextTrialAt:   time.Now().Add(10 * time.Second).UTC(),
		TrialCount:    1,
	}

	require.NoError(t, mgr.UpsertBlockScope(ctx, block))

	// Delete it.
	err := mgr.DeleteBlockScope(ctx, SKService())
	require.NoError(t, err)

	// Verify it's gone.
	blocks, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	// Delete non-existent — should not error (DELETE WHERE is no-op).
	err = mgr.DeleteBlockScope(ctx, SKQuotaOwn())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// ListBlockScopes
// ---------------------------------------------------------------------------

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	blocks := []*BlockScope{
		{
			Key:           SKThrottleDrive(driveid.New("0000000000000001")),
			IssueType:     IssueRateLimited,
			TimingSource:  ScopeTimingServerRetryAfter,
			BlockedAt:     now,
			TrialInterval: 5 * time.Second,
			NextTrialAt:   now.Add(5 * time.Second),
			TrialCount:    0,
		},
		{
			Key:           SKService(),
			IssueType:     IssueServiceOutage,
			TimingSource:  ScopeTimingBackoff,
			BlockedAt:     now.Add(-time.Minute),
			TrialInterval: 30 * time.Second,
			NextTrialAt:   now.Add(29 * time.Second),
			TrialCount:    2,
		},
		{
			Key:           SKPermRemote("Shared/TeamDocs"),
			IssueType:     IssueSharedFolderBlocked,
			TimingSource:  ScopeTimingBackoff,
			BlockedAt:     now.Add(-5 * time.Minute),
			TrialInterval: time.Minute,
			NextTrialAt:   now.Add(55 * time.Second),
			TrialCount:    5,
		},
	}

	for _, b := range blocks {
		require.NoError(t, mgr.UpsertBlockScope(ctx, b))
	}

	got, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Build a map for easier assertion (order is not guaranteed).
	byKey := make(map[ScopeKey]*BlockScope)
	for _, b := range got {
		byKey[b.Key] = b
	}

	// Verify target throttle round-trip.
	ta := byKey[SKThrottleDrive(driveid.New("0000000000000001"))]
	require.NotNil(t, ta, "target throttle block should be listed")
	assert.Equal(t, IssueRateLimited, ta.IssueType)
	assert.Equal(t, now, ta.BlockedAt)
	assert.Equal(t, 5*time.Second, ta.TrialInterval)
	assert.Equal(t, now.Add(5*time.Second), ta.NextTrialAt)
	assert.Equal(t, 0, ta.TrialCount)

	// Verify perm:remote round-trip (parameterized key).
	remotePerm := byKey[SKPermRemote("Shared/TeamDocs")]
	require.NotNil(t, remotePerm, "perm:remote block should be listed")
	assert.Equal(t, IssueSharedFolderBlocked, remotePerm.IssueType)
	assert.Equal(t, 5, remotePerm.TrialCount)
}

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes_Empty(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	got, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.NotNil(t, got, "empty result should be non-nil slice")
	assert.Empty(t, got)
}

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes_SkipsUnknownWireKeys(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	_, err := mgr.db.ExecContext(
		ctx,
		`INSERT INTO block_scopes
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, trial_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"auth:account",
		IssueUnauthorized,
		ScopeTimingNone,
		now.UnixNano(),
		int64(0),
		int64(0),
		0,
	)
	require.NoError(t, err)

	require.NoError(t, mgr.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     now,
		TrialInterval: 5 * time.Second,
		NextTrialAt:   now.Add(5 * time.Second),
	}))

	got, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, SKService(), got[0].Key)
}

// ---------------------------------------------------------------------------
// Round-trip: all field types survive serialization
// ---------------------------------------------------------------------------

// Validates: R-2.10.33, R-2.10.34
func TestSyncStore_BlockScope_Roundtrip(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	// Use specific values that exercise edge cases in serialization:
	// - Timestamps with nanosecond precision
	// - Duration in nanoseconds
	// - Parameterized scope key (perm:local-write)
	original := &BlockScope{
		Key:           SKPermRemote("Shared/TeamDocs"),
		IssueType:     IssueSharedFolderBlocked,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Date(2025, 3, 14, 9, 26, 53, 123456789, time.UTC),
		TrialInterval: 2*time.Minute + 500*time.Millisecond,
		NextTrialAt:   time.Date(2025, 3, 14, 9, 28, 53, 987654321, time.UTC),
		TrialCount:    42,
	}

	require.NoError(t, mgr.UpsertBlockScope(ctx, original))

	got, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// Verify every field round-trips correctly.
	assert.Equal(t, original.Key, got[0].Key, "scope key should round-trip")
	assert.Equal(t, original.IssueType, got[0].IssueType, "issue type should round-trip")
	assert.Equal(t, original.BlockedAt, got[0].BlockedAt, "blocked_at should round-trip with nanosecond precision")
	assert.Equal(t, original.TrialInterval, got[0].TrialInterval, "trial_interval should round-trip")
	assert.Equal(t, original.NextTrialAt, got[0].NextTrialAt, "next_trial_at should round-trip with nanosecond precision")
	assert.Equal(t, original.TrialCount, got[0].TrialCount, "trial_count should round-trip")
}

// Validates: R-2.10.8
func TestSyncStore_BlockScope_Roundtrip_ZeroNextTrialAt(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	original := &BlockScope{
		Key:           SKPermRemoteWrite("Shared/TeamDocs"),
		IssueType:     IssueRemoteWriteDenied,
		TimingSource:  ScopeTimingNone,
		BlockedAt:     time.Date(2025, 3, 14, 9, 26, 53, 123456789, time.UTC),
		TrialInterval: 0,
		NextTrialAt:   time.Time{},
		TrialCount:    0,
	}

	require.NoError(t, mgr.UpsertBlockScope(ctx, original))

	got, err := mgr.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].NextTrialAt.IsZero(), "zero trial timestamps must round-trip as a true zero time")
	assert.Equal(t, original.TrialInterval, got[0].TrialInterval)
}
