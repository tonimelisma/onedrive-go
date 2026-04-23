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
	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "Shared/Docs/file.txt",
		ActionType:    ActionUpload,
		ConditionType: IssueRemoteWriteDenied,
		ScopeKey:      scopeKey,
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}
