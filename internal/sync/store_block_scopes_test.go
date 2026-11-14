package sync

import (
	"context"
	"math"
	"path/filepath"
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
			block:   testBlockScope(ScopeKey{}, 0, time.Time{}),
			wantErr: "missing scope key",
		},
		{
			name:    "missing interval",
			block:   testBlockScope(SKService(), 0, time.Time{}),
			wantErr: "positive trial interval",
		},
		{
			name:    "missing next trial",
			block:   testBlockScope(SKService(), time.Second, time.Time{}),
			wantErr: "timed scope requires next_trial_at",
		},
		{
			name:  "valid scope",
			block: testBlockScope(SKService(), time.Second, nextTrialAt),
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

	block := testBlockScope(
		SKThrottleDrive(driveid.New("0000000000000001")),
		5*time.Second,
		time.Date(2025, 6, 15, 10, 0, 5, 0, time.UTC),
	)

	require.NoError(t, store.UpsertBlockScope(ctx, block))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, block.Key, blocks[0].Key)
	assert.Equal(t, block.TrialInterval, blocks[0].TrialInterval)
	assert.Equal(t, block.NextTrialAt, blocks[0].NextTrialAt)

	block.TrialInterval = 20 * time.Second
	block.NextTrialAt = time.Date(2025, 6, 15, 10, 0, 20, 0, time.UTC)
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

	err := store.UpsertBlockScope(ctx, testBlockScope(
		sKPermRemoteRead("Shared/TeamDocs"),
		time.Second,
		now.Add(time.Second),
	))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read boundaries belong in observation_issues")
}

// Validates: R-2.10.8
func TestSyncStore_DeleteBlockScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	block := testBlockScope(SKService(), 10*time.Second, time.Now().Add(10*time.Second).UTC())

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
		`INSERT INTO block_scopes (scope_key, trial_interval, next_trial_at)
		VALUES (?, ?, ?)`,
		"",
		int64(time.Second),
		now.Add(time.Second).UnixNano(),
	)
	require.NoError(t, err)

	require.NoError(t, store.UpsertBlockScope(ctx, testBlockScope(SKService(), 5*time.Second, now.Add(5*time.Second))))

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
		`INSERT INTO block_scopes (scope_key, trial_interval, next_trial_at)
		VALUES (?, ?, ?)`,
		"auth:account",
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
		`INSERT INTO block_scopes (scope_key, trial_interval, next_trial_at)
		VALUES (?, ?, ?)`,
		sKPermRemoteRead("Shared/Readonly").String(),
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

	original := testBlockScope(
		SKPermRemoteWrite("Shared/TeamDocs"),
		2*time.Minute+500*time.Millisecond,
		time.Date(2025, 3, 14, 9, 28, 53, 987654321, time.UTC),
	)
	original.SlowStartThreshold = 16

	require.NoError(t, store.UpsertBlockScope(ctx, original))

	got, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, original.Key, got[0].Key)
	assert.Equal(t, original.TrialInterval, got[0].TrialInterval)
	assert.Equal(t, original.NextTrialAt, got[0].NextTrialAt)
	assert.Equal(t, 16, got[0].SlowStartThreshold, "the release ramp's memory survives a restart (R-6.8.24)")
}

// Validates: R-2.10.7, R-2.5.1
func TestUpsertBlockScope_SaturatedTrialSurvivesAReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	logger := newTestLogger(t)
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	// A Retry-After too long for a Duration saturates at the longest one
	// (retry.ResponseRetryAfter), and the trial it asks for lies near 2318,
	// past the last time Unix nanoseconds can hold.
	trialInterval := computeTrialInterval(time.Duration(math.MaxInt64))
	key := SKThrottleDrive(driveid.New("0000000000000001"))

	store, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	require.NoError(t, store.UpsertBlockScope(ctx, testBlockScope(key, trialInterval, now.Add(trialInterval))))
	require.NoError(t, store.Close(ctx))

	reopened, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	blocks, err := reopened.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, trialInterval, blocks[0].TrialInterval)
	assert.Equal(t, time.Unix(0, math.MaxInt64).UTC(), blocks[0].NextTrialAt, "the trial reads back as the last storable time")
	assert.Empty(t, dueTrials([]activeScope{activeScopeFromBlockScopeRow(blocks[0])}, now),
		"a trial OneDrive put past 2262 is not due after a restart")
}
