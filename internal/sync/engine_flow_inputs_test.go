package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Every run gets a run ID no earlier run of the engine had, so the run_id of
// its classified logs tells its lines from those of the run before.
//
// Validates: R-6.6.13
func TestEngine_EachRunGetsADistinctRunID(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	const runs = 3
	seen := make(map[string]bool, runs)
	for range runs {
		flow := newEngineFlow(eng.flowInputs())
		assert.NotEmpty(t, flow.runID)
		assert.False(t, seen[flow.runID], "run ID %s was already taken", flow.runID)
		seen[flow.runID] = true
	}
}

// A run reads the engine's configuration and hooks from the copies its flow
// took when it started, not through the engine.
//
// Validates: R-6.10.29
func TestEngine_FlowInputsCopyTheConfigurationAndHooks(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
	in := eng.flowInputs()

	assert.Equal(t, eng.driveID, in.cfg.driveID)
	assert.Equal(t, syncRoot, in.cfg.syncRoot)
	assert.Equal(t, eng.hooks.workerPoolSize(eng.transferWorkers), in.cfg.poolSize)
	assert.Same(t, eng.syncTree, in.cfg.syncTree)
	assert.True(t, in.hooks.assertInvariants, "the test engine's invariant checks are on")
	assert.Same(t, eng.baseline, in.deps.store)

	eng.hooks.assertInvariants = false
	assert.True(t, newEngineFlow(in).invariantChecksEnabled(), "a running flow keeps the hooks it started with")
}
