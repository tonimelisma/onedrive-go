package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.5, R-6.8.9
func TestStartRuntimeStage_ReleasesDueHeldTrialImmediately(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	scopeKey := syncdomain.SKService()
	runtime := &runtimePlan{
		Plan: &syncdomain.ActionPlan{
			Actions: []syncdomain.Action{{
				Type: syncdomain.ActionUpload,
				Path: "held.txt",
				View: &syncdomain.PathView{Path: "held.txt"},
			}},
			Deps: [][]int{nil},
		},
		RetryRows: []syncdomain.RetryWorkRow{{
			Path:         "held.txt",
			ActionType:   syncdomain.ActionUpload,
			ScopeKey:     scopeKey,
			Blocked:      true,
			AttemptCount: 1,
		}},
		BlockScopes: []*syncdomain.BlockScope{{
			Key:           scopeKey,
			TrialInterval: 10 * time.Second,
			NextTrialAt:   eng.nowFunc().Add(-time.Second),
		}},
	}

	outbox, dispatched, err := flow.startRuntimeStage(ctx, runtime, bl)
	require.NoError(t, err)
	require.True(t, dispatched)
	require.Len(t, outbox, 1)

	assert.Equal(t, "held.txt", outbox[0].Action.Path)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, scopeKey, outbox[0].TrialScopeKey)
	assert.Zero(t, flow.sched.HeldCount())
}
