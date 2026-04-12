package syncstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// UpsertScopeBlock
// ---------------------------------------------------------------------------

// Validates: R-2.10.8
func TestSyncStore_UpsertScopeBlock(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	block := &ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		TimingSource:  synctypes.ScopeTimingServerRetryAfter,
		BlockedAt:     time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   time.Date(2025, 6, 15, 10, 0, 5, 0, time.UTC),
		PreserveUntil: time.Date(2025, 6, 15, 10, 0, 5, 0, time.UTC),
		TrialCount:    0,
	}

	err := mgr.UpsertScopeBlock(ctx, block)
	require.NoError(t, err)

	// Verify by listing.
	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, synctypes.SKThrottleAccount(), blocks[0].Key)
	assert.Equal(t, synctypes.IssueRateLimited, blocks[0].IssueType)
	assert.Equal(t, 0, blocks[0].TrialCount)

	// Upsert with updated values — should replace.
	block.TrialCount = 3
	block.TrialInterval = 20 * time.Second
	err = mgr.UpsertScopeBlock(ctx, block)
	require.NoError(t, err)

	blocks, err = mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "upsert should not create duplicate rows")
	assert.Equal(t, 3, blocks[0].TrialCount, "upsert should update trial count")
	assert.Equal(t, 20*time.Second, blocks[0].TrialInterval, "upsert should update interval")
	assert.Equal(t, block.PreserveUntil, blocks[0].PreserveUntil, "upsert should update preserve deadline")
}

// ---------------------------------------------------------------------------
// DeleteScopeBlock
// ---------------------------------------------------------------------------

// Validates: R-2.10.8
func TestSyncStore_DeleteScopeBlock(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	block := &ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     time.Now().UTC(),
		TrialInterval: 10 * time.Second,
		NextTrialAt:   time.Now().Add(10 * time.Second).UTC(),
		TrialCount:    1,
	}

	require.NoError(t, mgr.UpsertScopeBlock(ctx, block))

	// Delete it.
	err := mgr.DeleteScopeBlock(ctx, synctypes.SKService())
	require.NoError(t, err)

	// Verify it's gone.
	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	// Delete non-existent — should not error (DELETE WHERE is no-op).
	err = mgr.DeleteScopeBlock(ctx, synctypes.SKQuotaOwn())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// ListScopeBlocks
// ---------------------------------------------------------------------------

// Validates: R-2.10.8
func TestSyncStore_ListScopeBlocks(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	blocks := []*ScopeBlock{
		{
			Key:           synctypes.SKThrottleAccount(),
			IssueType:     synctypes.IssueRateLimited,
			TimingSource:  synctypes.ScopeTimingServerRetryAfter,
			BlockedAt:     now,
			TrialInterval: 5 * time.Second,
			NextTrialAt:   now.Add(5 * time.Second),
			PreserveUntil: now.Add(5 * time.Second),
			TrialCount:    0,
		},
		{
			Key:           synctypes.SKService(),
			IssueType:     synctypes.IssueServiceOutage,
			TimingSource:  synctypes.ScopeTimingBackoff,
			BlockedAt:     now.Add(-time.Minute),
			TrialInterval: 30 * time.Second,
			NextTrialAt:   now.Add(29 * time.Second),
			TrialCount:    2,
		},
		{
			Key:           synctypes.SKQuotaShortcut("drive1:item1"),
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			BlockedAt:     now.Add(-5 * time.Minute),
			TrialInterval: time.Minute,
			NextTrialAt:   now.Add(55 * time.Second),
			TrialCount:    5,
		},
	}

	for _, b := range blocks {
		require.NoError(t, mgr.UpsertScopeBlock(ctx, b))
	}

	got, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Build a map for easier assertion (order is not guaranteed).
	byKey := make(map[synctypes.ScopeKey]*ScopeBlock)
	for _, b := range got {
		byKey[b.Key] = b
	}

	// Verify throttle:account round-trip.
	ta := byKey[synctypes.SKThrottleAccount()]
	require.NotNil(t, ta, "throttle:account block should be listed")
	assert.Equal(t, synctypes.IssueRateLimited, ta.IssueType)
	assert.Equal(t, now, ta.BlockedAt)
	assert.Equal(t, 5*time.Second, ta.TrialInterval)
	assert.Equal(t, now.Add(5*time.Second), ta.NextTrialAt)
	assert.Equal(t, now.Add(5*time.Second), ta.PreserveUntil)
	assert.Equal(t, 0, ta.TrialCount)

	// Verify quota:shortcut round-trip (parameterized key).
	qs := byKey[synctypes.SKQuotaShortcut("drive1:item1")]
	require.NotNil(t, qs, "quota:shortcut block should be listed")
	assert.Equal(t, synctypes.IssueQuotaExceeded, qs.IssueType)
	assert.Equal(t, 5, qs.TrialCount)
}

// Validates: R-2.10.8
func TestSyncStore_ListScopeBlocks_Empty(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	got, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.NotNil(t, got, "empty result should be non-nil slice")
	assert.Empty(t, got)
}

// ---------------------------------------------------------------------------
// Round-trip: all field types survive serialization
// ---------------------------------------------------------------------------

// Validates: R-2.10.33, R-2.10.34
func TestSyncStore_ScopeBlock_Roundtrip(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	// Use specific values that exercise edge cases in serialization:
	// - Timestamps with nanosecond precision
	// - Duration in nanoseconds
	// - Parameterized scope key (perm:dir)
	original := &ScopeBlock{
		Key:           synctypes.SKQuotaShortcut("drive1:item1"),
		IssueType:     synctypes.IssueQuotaExceeded,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     time.Date(2025, 3, 14, 9, 26, 53, 123456789, time.UTC),
		TrialInterval: 2*time.Minute + 500*time.Millisecond,
		NextTrialAt:   time.Date(2025, 3, 14, 9, 28, 53, 987654321, time.UTC),
		PreserveUntil: time.Date(2025, 3, 14, 9, 28, 53, 987654321, time.UTC),
		TrialCount:    42,
	}

	require.NoError(t, mgr.UpsertScopeBlock(ctx, original))

	got, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// Verify every field round-trips correctly.
	assert.Equal(t, original.Key, got[0].Key, "scope key should round-trip")
	assert.Equal(t, original.IssueType, got[0].IssueType, "issue type should round-trip")
	assert.Equal(t, original.BlockedAt, got[0].BlockedAt, "blocked_at should round-trip with nanosecond precision")
	assert.Equal(t, original.TrialInterval, got[0].TrialInterval, "trial_interval should round-trip")
	assert.Equal(t, original.NextTrialAt, got[0].NextTrialAt, "next_trial_at should round-trip with nanosecond precision")
	assert.Equal(t, original.PreserveUntil, got[0].PreserveUntil, "preserve_until should round-trip with nanosecond precision")
	assert.Equal(t, original.TrialCount, got[0].TrialCount, "trial_count should round-trip")
}

// Validates: R-2.10.8
func TestSyncStore_ScopeBlock_Roundtrip_ZeroNextTrialAt(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	original := &ScopeBlock{
		Key:           synctypes.SKPermRemote("Shared/TeamDocs"),
		IssueType:     synctypes.IssuePermissionDenied,
		TimingSource:  synctypes.ScopeTimingNone,
		BlockedAt:     time.Date(2025, 3, 14, 9, 26, 53, 123456789, time.UTC),
		TrialInterval: 0,
		NextTrialAt:   time.Time{},
		TrialCount:    0,
	}

	require.NoError(t, mgr.UpsertScopeBlock(ctx, original))

	got, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].NextTrialAt.IsZero(), "zero trial timestamps must round-trip as a true zero time")
	assert.True(t, got[0].PreserveUntil.IsZero(), "zero preserve deadlines must round-trip as a true zero time")
	assert.Equal(t, original.TrialInterval, got[0].TrialInterval)
}
