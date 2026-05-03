package sync

import (
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
		NextTrialAt:   eng.nowFn().Add(time.Minute),
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
		NextTrialAt:   eng.nowFn().Add(time.Minute),
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
		NextRetryAt:  eng.nowFn().Add(time.Minute).UnixNano(),
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
		NextRetryAt: eng.nowFn().Add(time.Minute).UnixNano(),
	}))

	err := flow.assertPersistedInvariants(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid attempt count")
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
		NextRetryAt:  eng.nowFn().Add(time.Minute).UnixNano(),
	}))

	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}
