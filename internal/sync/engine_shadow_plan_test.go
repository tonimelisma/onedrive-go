package sync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// decideNewLocalFileAsNoop is the decision table with a new local file left
// alone: the current plan uploads nothing, while the identity-keyed shadow
// planner, which keeps its own reading of a new item, uploads it. That is a
// seeded difference no known reason covers.
func decideNewLocalFileAsNoop(s syncdomain.DecisionState) syncdomain.Decision {
	decision := syncdomain.Decide(s)
	if decision.Comparison == syncdomain.ComparisonLocalOnlyCreate && decision.Reconciliation == syncdomain.ReconcileUpload {
		decision.Reconciliation = syncdomain.ReconcileNoop
	}

	return decision
}

// Validates: R-2.3.1
//
// A watch replan, in the phase that reads replan requests, plans the
// identity-keyed shadow plan beside its own and reports where they differ as
// a debug event; the shadow's upload is neither dispatched nor run.
func TestRunSteadyStateReplan_ReportsAShadowPlanDifferenceAsADebugEvent(t *testing.T) {
	t.Parallel()

	eng, syncRoot, uploads := newUnclassifiedPlanTestEngine(t)
	eng.hooks.decideOverride = decideNewLocalFileAsNoop
	recorder := attachDebugEventRecorder(eng)
	ctx := t.Context()
	setupWatchEngine(t, eng)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	syncfixture.WriteLocalFile(t, syncRoot, "new.txt", "local content")

	rt := testWatchRuntime(t, eng)
	setWatchPhase(rt, watchPhaseRunning)
	require.True(t, watchInputOn(watchPhaseRunning, watchInputReplanReady))
	applied, err := rt.runSteadyStateReplan(ctx, &watchPipeline{bl: bl, mode: syncdomain.SyncBidirectional}, syncobserve.DirtyBatch{})
	require.NoError(t, err)
	assert.True(t, applied)

	assert.True(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventShadowPlanDiffered &&
			event.Path == "new.txt" && event.Count == 1 && event.Error == "" &&
			strings.HasPrefix(event.Note, "1 unexplained; ") &&
			strings.Contains(event.Note, "shadow [upload new.txt]")
	}), "the replan reports the shadow plan's upload as a difference")
	assert.Empty(t, rt.currentOutbox(), "the shadow plan is never dispatched")
	assert.Zero(t, uploads.Load(), "the shadow plan is never executed")
}

// A watch replan whose plan the shadow plan matches reports nothing.
//
// Validates: R-2.3.1
func TestRunSteadyStateReplan_ReportsNothingWhenTheShadowPlanAgrees(t *testing.T) {
	t.Parallel()

	eng, _, _ := newUnclassifiedPlanTestEngine(t)
	eng.hooks.decideOverride = nil
	recorder := attachDebugEventRecorder(eng)
	ctx := t.Context()
	setupWatchEngine(t, eng)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt := testWatchRuntime(t, eng)
	setWatchPhase(rt, watchPhaseRunning)
	_, err = rt.runSteadyStateReplan(ctx, &watchPipeline{bl: bl, mode: syncdomain.SyncBidirectional}, syncobserve.DirtyBatch{})
	require.NoError(t, err)

	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventShadowPlanDiffered
	}))
}
