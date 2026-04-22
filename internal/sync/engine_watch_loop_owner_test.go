package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchRuntime_RunWatchLoop_BootstrapPhaseQuiescesAndReturnsToRunning(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.enterBootstrap()

	err := rt.runWatchLoop(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.Equal(t, watchRuntimePhaseRunning, rt.phase())
}

func TestWatchRuntime_RunBootstrapStep_DispatchUsesSharedWatchHandler(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	action := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "bootstrap.txt",
		DriveID: eng.driveID,
		ItemID:  "bootstrap-item",
	}, 1, nil)
	require.NotNil(t, action)
	rt.replaceOutbox([]*TrackedAction{action})

	done, err := rt.runBootstrapStep(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, 1, rt.runningCount)
	assert.Empty(t, rt.currentOutbox())
}

func TestWatchRuntime_RunWatchStepIdle_ContextCancelStartsDrain(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done, err := rt.runWatchStepIdle(ctx, &watchPipeline{runtime: rt})
	require.NoError(t, err)
	assert.False(t, done)
	assert.True(t, rt.isDraining())
}
