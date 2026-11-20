package sync

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// An engine built with zero hooks behaves as production does: the real
// clock, the real websocket wake source, the full worker pool and pass
// bound, the SQLite store driver and no invariant checks.
//
// Validates: R-6.10.29
func TestEngineHooks_ZeroValueIsProduction(t *testing.T) {
	t.Parallel()

	// Above the pool's floor, so the size read back is the configured one.
	const configuredWorkers = 9

	inputs := newTestEngineInputs(t, &syncfixture.EngineMockClient{}, t.TempDir())
	inputs.TransferWorkers = configuredWorkers
	eng, err := newEngine(t.Context(), inputs)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(context.WithoutCancel(t.Context())))
	})

	assert.IsType(t, syncdomain.RealClock{}, eng.runClock(), "the run clock is the real clock")
	assert.Equal(t, eng.wallClock, eng.runClock(), "the run clock is the engine's own real clock")
	assert.Equal(t, syncstore.SqliteDriverName, inputs.hooks.storeDriverName(), "the store opens through SQLite")
	assert.Equal(t, configuredWorkers, eng.workerPoolSize(), "the pool runs the configured workers")
	assert.Equal(t, oneShotMaxPasses, eng.oneShotPasses(), "one-shot runs make the full pass bound")
	assert.False(t, newEngineFlow(eng).invariantChecksEnabled(), "invariant checks are off")

	wake := eng.newSocketIOWakeSource(syncobserve.SocketIOWakeSourceOptions{})
	assert.Equal(t, "*syncobserve.socketIOWakeSource", fmt.Sprintf("%T", wake), "the wake source is the real one")
}
