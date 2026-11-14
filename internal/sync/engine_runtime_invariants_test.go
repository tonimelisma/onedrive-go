package sync

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_RejectsOrphanBlockScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           SKService(),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))

	err := flow.assertPersistedInvariants(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no related blocked retry_work rows")
}

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_AllowsBlockedRetryWorkBackedBlockScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))
	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), testRetryWorkKey("Shared/Docs/file.txt", "", ActionUpload), scopeKey)
	require.NoError(t, err)

	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_RejectsRetryWorkWithoutPath(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  eng.nowFunc().Add(time.Minute).UnixNano(),
	}))

	err := flow.assertPersistedInvariants(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry_work row missing path")
}

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_RejectsRetryWorkWithoutTiming(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "delayed.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
	}))

	err := flow.assertPersistedInvariants(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing retry timing")
}

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_RejectsRetryWorkWithoutAttempts(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:        "delayed.txt",
		ActionType:  ActionUpload,
		NextRetryAt: eng.nowFunc().Add(time.Minute).UnixNano(),
	}))

	err := flow.assertPersistedInvariants(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid attempt count")
}

// A delayed row's retry time at either end of the stored range is timing;
// only zero is none.
// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_AllowsDelayedRetryWorkAtBothEndsOfTheStoredRange(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	for path, stored := range map[string]int64{"before.txt": math.MinInt64, "past.txt": math.MaxInt64} {
		require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
			Path:         path,
			ActionType:   ActionUpload,
			AttemptCount: 1,
			NextRetryAt:  stored,
		}))
	}

	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_AllowsDelayedRetryWorkWithTiming(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "delayed.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  eng.nowFunc().Add(time.Minute).UnixNano(),
	}))

	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}
