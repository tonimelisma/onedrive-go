package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.5, R-6.8.9
func TestStartRuntimeStage_ReleasesDueHeldTrialImmediately(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	scopeKey := SKService()
	runtime := &runtimePlan{
		Plan: &ActionPlan{
			Actions: []Action{{
				Type: ActionUpload,
				Path: "held.txt",
				View: &PathView{Path: "held.txt"},
			}},
			Deps: [][]int{nil},
		},
		RetryRows: []RetryWorkRow{{
			Path:         "held.txt",
			ActionType:   ActionUpload,
			ScopeKey:     scopeKey,
			Blocked:      true,
			AttemptCount: 1,
		}},
		BlockScopes: []*BlockScope{{
			Key:           scopeKey,
			TrialInterval: 10 * time.Second,
			NextTrialAt:   eng.nowFn().Add(-time.Second),
		}},
	}

	outbox, dispatched, err := flow.startRuntimeStage(ctx, runtime, bl, nil)
	require.NoError(t, err)
	require.True(t, dispatched)
	require.Len(t, outbox, 1)

	assert.Equal(t, "held.txt", outbox[0].Action.Path)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, scopeKey, outbox[0].TrialScopeKey)
	assert.Empty(t, flow.heldByKey)
}
