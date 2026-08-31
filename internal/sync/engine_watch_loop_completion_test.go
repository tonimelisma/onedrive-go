package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.11, R-6.7.1
//
// Bootstrap and steady state share one loop but may not end for the same
// reasons. Bootstrap ends at quiescence; steady state may only end by
// draining. A steady-state loop that reported completion any other way used
// to return nil, so watch mode stopped observing while its caller still
// believed the mount was live -- silent staleness dressed as clean shutdown.
func TestRunWatchLoop_SteadyStateCompletionWithoutDrainingIsAnError(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)

	// Bootstrap phase is quiescent here, so the shared loop reports done on
	// its first step. Reached from the steady-state entry point, that is a
	// stop without a drain.
	rt.enterBootstrap()

	err := rt.runWatchLoop(t.Context(), &watchPipeline{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without draining")
}

// Validates: R-2.11, R-6.7.1
func TestRunWatchUntilQuiescent_BootstrapCompletionStaysClean(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)

	require.NoError(t, rt.runWatchUntilQuiescent(t.Context(), &watchPipeline{}, nil))
	assert.Equal(t, watchRuntimePhaseRunning, rt.phase())
}

// Validates: R-2.11, R-6.7.1
//
// Cancellation can land between bootstrap draining and observer startup.
// startObservers then refuses to start observers during a drain, and that
// refusal has to read as a clean stop rather than a watch-mode failure.
func TestStartObservers_RefusalDuringDrainClassifiesAsShutdown(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	rt := newWatchRuntime(eng.Engine)
	rt.enterDraining()

	err = rt.startObservers(ctx, bl, WatchOptions{})
	require.Error(t, err)
	assert.True(t, isWatchObserverStartupStopped(ctx, err),
		"a refusal to start observers during drain must classify as clean shutdown")
}
