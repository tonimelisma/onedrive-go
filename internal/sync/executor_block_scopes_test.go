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
	trialInterval := ComputeTrialInterval(time.Duration(math.MaxInt64))
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
	assert.Empty(t, dueTrials([]ActiveScope{ActiveScopeFromBlockScopeRow(blocks[0])}, now),
		"a trial OneDrive put past 2262 is not due after a restart")
}
