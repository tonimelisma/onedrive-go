package sync

import (
	"context"
	"testing"
	"time"

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

func TestWatchRuntime_RunNonDrainingWatchStep_BootstrapDispatchUsesSharedHandler(t *testing.T) {
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
	rt.enterBootstrap()

	done, err := rt.runNonDrainingWatchStep(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, 1, rt.runningCount)
	assert.Empty(t, rt.currentOutbox())
}

func TestWatchRuntime_RunNonDrainingWatchStep_ContextCancelStartsDrain(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done, err := rt.runNonDrainingWatchStep(ctx, &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.True(t, rt.isDraining())
}

func TestWatchRuntime_RunNonDrainingWatchStep_ConsumesReplanReady(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	replanReady := make(chan dirtyBatch, 1)
	replanReady <- dirtyBatch{}

	type idleResult struct {
		done bool
		err  error
	}
	doneCh := make(chan idleResult, 1)
	go func() {
		done, err := rt.runNonDrainingWatchStep(t.Context(), &watchPipeline{
			runtime:     rt,
			replanReady: replanReady,
		}, nil)
		doneCh <- idleResult{done: done, err: err}
	}()

	select {
	case result := <-doneCh:
		assert.False(t, result.done)
		require.ErrorContains(t, result.err, "steady-state replan requires loaded baseline")
	case <-time.After(time.Second):
		t.Fatalf("runNonDrainingWatchStep did not consume replanReady while idle")
	}
}
