package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.5, R-6.8.9
func TestStartPreparedRuntime_ReleasesDueHeldTrialImmediately(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	scopeKey := SKService()
	prepared := &PreparedCurrentPlan{
		Plan: &ActionPlan{
			Actions: []Action{{
				Type: ActionUpload,
				Path: "held.txt",
			}},
			Deps: [][]int{nil},
		},
		RetryRows: []RetryWorkRow{{
			WorkKey:       serializeRetryWorkKey(retryWorkKey("held.txt", "", ActionUpload)),
			Path:          "held.txt",
			ActionType:    ActionUpload,
			ConditionType: IssueServiceOutage,
			ScopeKey:      scopeKey,
			Blocked:       true,
			AttemptCount:  1,
			FirstSeenAt:   eng.nowFn().Add(-time.Minute).UnixNano(),
			LastSeenAt:    eng.nowFn().UnixNano(),
		}},
		BlockScopes: []*BlockScope{{
			Key:           scopeKey,
			BlockedAt:     eng.nowFn().Add(-time.Minute),
			TrialInterval: 10 * time.Second,
			NextTrialAt:   eng.nowFn().Add(-time.Second),
		}},
	}

	outbox, dispatched, err := flow.startPreparedRuntime(ctx, prepared, bl, nil)
	require.NoError(t, err)
	require.True(t, dispatched)
	require.Len(t, outbox, 1)

	assert.Equal(t, "held.txt", outbox[0].Action.Path)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, scopeKey, outbox[0].TrialScopeKey)
	assert.Empty(t, flow.heldByKey)
}
