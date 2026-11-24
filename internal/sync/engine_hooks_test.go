package sync

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// An engine built with zero hooks behaves as production does: the real
// clock, the real websocket wake source, the full worker pool and pass
// bound, no invariant checks and a store that waits for the disk. The executor's, store's and observer's
// hooks prove their own zero value.
//
// Validates: R-6.10.29
func TestEngineHooks_ZeroValueIsProduction(t *testing.T) {
	t.Parallel()

	// Above the pool's floor, so the size read back is the configured one.
	const configuredWorkers = 9

	inputs := newTestEngineInputs(t, &syncfixture.EngineMockClient{}, t.TempDir())
	// Zero hooks, but for the store's OnOpen, which only keeps its database
	// for storeSynchronous to read the pragma through.
	inputs.hooks = engineHooks{store: syncstoretest.WithRawDB(t, syncstore.Hooks{})}
	inputs.TransferWorkers = configuredWorkers
	eng, err := newEngine(t.Context(), inputs)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(context.WithoutCancel(t.Context())))
	})

	assert.IsType(t, syncdomain.RealClock{}, eng.runClock(), "the run clock is the real clock")
	assert.Equal(t, eng.wallClock, eng.runClock(), "the run clock is the engine's own real clock")
	assert.Equal(t, configuredWorkers, eng.hooks.workerPoolSize(eng.transferWorkers), "the pool runs the configured workers")
	assert.Equal(t, oneShotMaxPasses, eng.hooks.oneShotPasses(), "one-shot runs make the full pass bound")
	assert.False(t, newEngineFlow(eng.flowInputs()).invariantChecksEnabled(), "invariant checks are off")
	assert.Equal(t, sqliteSynchronousFull, storeSynchronous(t, eng), "the store commits with synchronous=FULL")
	assert.Equal(t, syncdomain.OutcomeCommitTimeout, newEngineFlow(eng.flowInputs()).stopDrainBound(),
		"a stop records its results within an outcome's commit bound")
	assert.Equal(t, reflect.ValueOf(isOnReadOnlyFilesystem).Pointer(), reflect.ValueOf(eng.permHandler.readOnlyDir).Pointer(),
		"the permission handler probes the real filesystem")

	wake := eng.remoteSource(&eng.hooks).newWakeSource(syncobserve.SocketIOWakeSourceOptions{})
	assert.Equal(t, "*syncobserve.socketIOWakeSource", fmt.Sprintf("%T", wake), "the wake source is the real one")
}

// A run reads its hooks from the copy its flow took when it started: a hook a
// test sets on the engine afterwards reaches the next run, not the one that
// exists already. The pool size, the run clock and the one-shot pause wait,
// the pass bound, the websocket wake source, the local observer seams and the
// startup proof's catalog clear all come from the flow's copy.
//
// Validates: R-6.10.29
func TestEngineHooks_SetAfterAFlowExistsDoNotReachThatFlow(t *testing.T) {
	t.Parallel()

	const configuredWorkers = 9

	inputs := newTestEngineInputs(t, &syncfixture.EngineMockClient{}, t.TempDir())
	inputs.hooks = engineHooks{}
	inputs.TransferWorkers = configuredWorkers
	eng, err := newEngine(t.Context(), inputs)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(context.WithoutCancel(t.Context())))
	})

	runner := newOneShotRunner(eng)

	manual := syncfixture.NewManualClock(time.Unix(0, 0))
	waited := false
	factoryCalled := false
	eng.hooks.clock = manual
	eng.hooks.serializeWorkers = true
	eng.hooks.oneShotPassLimit = 1
	eng.hooks.wait = func(context.Context, time.Duration) error {
		waited = true
		return nil
	}
	eng.hooks.socketIOWakeSourceFactory = func(
		SocketIOEndpointFetcher, driveid.ID, syncobserve.SocketIOWakeSourceOptions,
	) socketIOWakeSourceRunner {
		factoryCalled = true
		return nil
	}
	eng.hooks.observe.AfterSafetyScan = func() {}
	eng.hooks.clearAccountAuthRequirement = func(context.Context, string, driveid.CanonicalID) error { return nil }

	assert.Equal(t, eng.wallClock, runner.deps.clock, "the run keeps the clock it started with")
	assert.Equal(t, configuredWorkers, runner.cfg.poolSize, "the run keeps the pool it started with")
	assert.Equal(t, oneShotMaxPasses, runner.hooks.oneShotPasses(), "the run keeps the pass bound it started with")
	require.NoError(t, runner.waitFor(t.Context(), 0))
	assert.False(t, waited, "the run's pause wait is not the hook set after it started")
	wake := runner.remote.newWakeSource(syncobserve.SocketIOWakeSourceOptions{})
	assert.False(t, factoryCalled, "the run's wake source is not the factory set after it started")
	assert.Equal(t, "*syncobserve.socketIOWakeSource", fmt.Sprintf("%T", wake))
	assert.Nil(t, runner.local.hooks.AfterSafetyScan, "the run's local observers keep the seams it started with")
	assert.Nil(t, runner.hooks.clearAccountAuthRequirement, "the run's startup clear is not the hook set after it started")

	next := newEngineFlow(eng.flowInputs())
	assert.Same(t, manual, next.deps.clock, "the next run takes the clock set since")
	assert.Equal(t, 1, next.cfg.poolSize, "the next run takes the serialized pool")
	assert.Equal(t, 1, next.hooks.oneShotPasses(), "the next run takes the pass bound set since")
	assert.NotNil(t, next.local.hooks.AfterSafetyScan, "the next run's local observers take the seams set since")
}
