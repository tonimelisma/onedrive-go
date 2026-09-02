package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.11, R-6.7.1
//
// Losing every observer outside shutdown means watch mode has no source of
// fresh truth left. Continuing would silently look healthy while observing
// nothing, so the runtime must fail instead.
func TestHandleObserverExit_LastObserverOutsideShutdownReturnsError(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)
	rt.resources.activeObservers = 1

	err := rt.handleObserverExit(&watchPipeline{}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all observers exited")
	assert.Equal(t, 0, rt.resources.activeObservers)
}

// Validates: R-2.11, R-6.7.1
func TestHandleObserverExit_RemainingObserverKeepsWatchAlive(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)
	rt.resources.activeObservers = 2

	require.NoError(t, rt.handleObserverExit(&watchPipeline{}, false))
	assert.Equal(t, 1, rt.resources.activeObservers)
}

// Validates: R-2.11, R-6.7.1
//
// During shutdown the same condition is the expected end state, not a failure.
func TestHandleObserverExit_LastObserverDuringShutdownIsNotAnError(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)
	rt.resources.activeObservers = 1

	require.NoError(t, rt.handleObserverExit(&watchPipeline{}, true))
	assert.Equal(t, 0, rt.resources.activeObservers)
}

// Validates: R-2.11, R-6.7.1
//
// A draining runtime reaching zero observers outside shutdown is an invariant
// violation, not an ordinary error path.
func TestHandleObserverExit_DrainingOutsideShutdownViolatesInvariant(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)
	rt.resources.activeObservers = 1
	require.True(t, rt.enterDraining())

	var err error

	assert.Panics(t, func() {
		err = rt.handleObserverExit(&watchPipeline{}, false)
	})
	require.NoError(t, err, "the invariant must panic before producing a return value")
}
