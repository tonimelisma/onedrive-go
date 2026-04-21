package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.8
func TestValidateBlockScope(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	nextTrialAt := now.Add(time.Minute)

	tests := []struct {
		name    string
		block   *BlockScope
		wantErr string
	}{
		{
			name:    "missing key",
			block:   &BlockScope{},
			wantErr: "missing scope key",
		},
		{
			name: "missing blocked time",
			block: &BlockScope{
				Key: SKService(),
			},
			wantErr: "missing blocked_at",
		},
		{
			name: "missing interval",
			block: &BlockScope{
				Key:       SKService(),
				BlockedAt: now,
			},
			wantErr: "positive trial interval",
		},
		{
			name: "missing next trial",
			block: &BlockScope{
				Key:           SKService(),
				BlockedAt:     now,
				TrialInterval: time.Second,
			},
			wantErr: "timed scope requires next_trial_at",
		},
		{
			name: "valid scope",
			block: &BlockScope{
				Key:           SKService(),
				BlockedAt:     now,
				TrialInterval: time.Second,
				NextTrialAt:   nextTrialAt,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBlockScope(tc.block)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Validates: R-2.10.8
func TestSyncStore_UpsertBlockScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	block := &BlockScope{
		Key:           SKThrottleDrive(driveid.New("0000000000000001")),
		BlockedAt:     time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   time.Date(2025, 6, 15, 10, 0, 5, 0, time.UTC),
	}

	require.NoError(t, store.UpsertBlockScope(ctx, block))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, block.Key, blocks[0].Key)
	assert.Equal(t, block.BlockedAt, blocks[0].BlockedAt)
	assert.Equal(t, block.TrialInterval, blocks[0].TrialInterval)
	assert.Equal(t, block.NextTrialAt, blocks[0].NextTrialAt)

	block.TrialInterval = 20 * time.Second
	block.NextTrialAt = block.BlockedAt.Add(block.TrialInterval)
	require.NoError(t, store.UpsertBlockScope(ctx, block))

	blocks, err = store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, 20*time.Second, blocks[0].TrialInterval)
}

// Validates: R-2.10.8
func TestSyncStore_UpsertBlockScope_RejectsReadBoundaryScopes(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	err := store.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKPermRemoteRead("Shared/TeamDocs"),
		BlockedAt:     now,
		TrialInterval: time.Second,
		NextTrialAt:   now.Add(time.Second),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read boundaries belong in observation_issues")
}

// Validates: R-2.10.8
func TestSyncStore_DeleteBlockScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	block := &BlockScope{
		Key:           SKService(),
		BlockedAt:     time.Now().UTC(),
		TrialInterval: 10 * time.Second,
		NextTrialAt:   time.Now().Add(10 * time.Second).UTC(),
	}

	require.NoError(t, store.UpsertBlockScope(ctx, block))
	require.NoError(t, store.DeleteBlockScope(ctx, SKService()))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes_Empty(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	blocks, err := store.ListBlockScopes(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, blocks)
	assert.Empty(t, blocks)
}

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes_SkipsZeroScopeKeys(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	_, err := store.db.ExecContext(
		ctx,
		`INSERT INTO block_scopes (scope_key, blocked_at, trial_interval, next_trial_at)
		VALUES (?, ?, ?, ?)`,
		"",
		now.UnixNano(),
		int64(time.Second),
		now.Add(time.Second).UnixNano(),
	)
	require.NoError(t, err)

	require.NoError(t, store.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		BlockedAt:     now,
		TrialInterval: 5 * time.Second,
		NextTrialAt:   now.Add(5 * time.Second),
	}))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKService(), blocks[0].Key)
}

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes_SkipsUnknownScopeKeys(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	_, err := store.db.ExecContext(
		ctx,
		`INSERT INTO block_scopes (scope_key, blocked_at, trial_interval, next_trial_at)
		VALUES (?, ?, ?, ?)`,
		"auth:account",
		now.UnixNano(),
		int64(time.Second),
		now.Add(time.Second).UnixNano(),
	)
	require.NoError(t, err)

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// Validates: R-2.10.8
func TestSyncStore_ListBlockScopes_RejectsReadBoundaryScopes(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	_, err := store.db.ExecContext(
		ctx,
		`INSERT INTO block_scopes (scope_key, blocked_at, trial_interval, next_trial_at)
		VALUES (?, ?, ?, ?)`,
		SKPermRemoteRead("Shared/Readonly").String(),
		now.UnixNano(),
		int64(time.Second),
		now.Add(time.Second).UnixNano(),
	)
	require.NoError(t, err)

	_, err = store.ListBlockScopes(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs in observation_issues")
}

// Validates: R-2.10.33, R-2.10.34
func TestSyncStore_BlockScope_Roundtrip(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	original := &BlockScope{
		Key:           SKPermRemoteWrite("Shared/TeamDocs"),
		BlockedAt:     time.Date(2025, 3, 14, 9, 26, 53, 123456789, time.UTC),
		TrialInterval: 2*time.Minute + 500*time.Millisecond,
		NextTrialAt:   time.Date(2025, 3, 14, 9, 28, 53, 987654321, time.UTC),
	}

	require.NoError(t, store.UpsertBlockScope(ctx, original))

	got, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, original.Key, got[0].Key)
	assert.Equal(t, original.BlockedAt, got[0].BlockedAt)
	assert.Equal(t, original.TrialInterval, got[0].TrialInterval)
	assert.Equal(t, original.NextTrialAt, got[0].NextTrialAt)
}
