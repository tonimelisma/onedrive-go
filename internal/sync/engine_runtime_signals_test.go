package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSignals counts what a flow asked the runtime to schedule.
type recordingSignals struct {
	held   int
	retry  int
	trial  int
	kick   int
	replan int
}

func (s *recordingSignals) armHeldTimers()      { s.held++ }
func (s *recordingSignals) armRetryTimer()      { s.retry++ }
func (s *recordingSignals) armTrialTimer()      { s.trial++ }
func (s *recordingSignals) kickDueHeldRelease() { s.kick++ }
func (s *recordingSignals) markReplanNeeded()   { s.replan++ }

// A one-shot flow must default to signals that schedule nothing. Nothing armed
// during a one-shot pass could ever fire, and the previous design expressed
// that by passing a nil *watchRuntime and checking for it at every call site.
//
// Validates: R-2.1, R-2.8.5
func TestEngineFlow_OneShotDefaultsToNoRuntimeSignals(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)

	require.IsType(t, noRuntimeSignals{}, runner.signals,
		"a one-shot runner has no future in which a timer could fire")
	require.IsType(t, noWatchInspector{}, runner.inspect)

	// The no-op set must be callable without a runtime behind it; that is the
	// property the nil checks used to provide.
	assert.NotPanics(t, func() {
		runner.signals.armHeldTimers()
		runner.signals.armRetryTimer()
		runner.signals.armTrialTimer()
		runner.signals.kickDueHeldRelease()
		runner.signals.markReplanNeeded()
	})
}

// A watch runtime is its own signal sink: it is the live future that
// completion and scope decisions are asking about.
//
// Validates: R-2.8.5
func TestWatchRuntime_IsItsOwnSignalSinkAndInspector(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := newWatchRuntime(eng.Engine)

	require.Same(t, rt, rt.signals, "watch supplies its own runtime signals")
	require.Same(t, rt, rt.inspect, "watch supplies its own inspector")
}

// Scope release must report through the signal seam rather than reaching for a
// concrete watch runtime, so one-shot and watch travel the same code path.
//
// Validates: R-2.8.5
func TestEngineFlow_ReleaseScope_ReportsThroughRuntimeSignals(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	rec := &recordingSignals{}
	flow.signals = rec

	key := SKDiskLocal()
	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           key,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))
	require.NoError(t, flow.releaseScope(t.Context(), key))

	assert.Equal(t, 1, rec.kick, "release must ask the runtime to release due held work")
	assert.Equal(t, 1, rec.trial, "release must ask the runtime to rearm the trial timer")
	assert.False(t, flow.hasActiveScope(key))
}

// markReplanNeeded must tolerate a watch runtime whose dirty buffer has not
// been installed yet; the old code nil-checked the buffer at the call site.
//
// Validates: R-2.8.5
func TestWatchRuntime_MarkReplanNeededWithoutDirtyBufferIsSafe(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := newWatchRuntime(eng.Engine)
	require.Nil(t, rt.dirtyBuf)

	assert.NotPanics(t, rt.markReplanNeeded)
}
