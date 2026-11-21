package sync

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// setWatchPhase puts a test runtime in a named phase without walking the
// lifecycle, for tests that call one loop step directly.
func setWatchPhase(rt *watchRuntime, phase watchPhase) {
	rt.loop.phase = phase
}

func TestWatchPhase_TransitionTableIsTotal(t *testing.T) {
	t.Parallel()

	listed := 0
	for from := range watchPhaseCount {
		for ev := range watchEventCount {
			next, err := nextWatchPhase(from, ev)
			if want, ok := watchTransitions[from][ev]; ok {
				listed++
				require.NoError(t, err, "%s x %s", from, ev)
				assert.Equal(t, want, next, "%s x %s", from, ev)
				continue
			}
			require.Error(t, err, "%s x %s", from, ev)
			assert.Contains(t, err.Error(), from.String())
			assert.Contains(t, err.Error(), ev.String())
			assert.Equal(t, from, next, "a refused event leaves the phase alone")
		}
	}
	assert.Equal(t, 11, listed, "the lifecycle table lists eleven transitions")
}

func TestWatchPhase_NothingLeavesDrainingExceptDrained(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		phase := watchPhase(rng.IntN(int(watchPhaseCount)))
		sealed := phase == watchPhaseDraining || phase == watchPhaseStopped
		for range 12 {
			ev := watchEvent(rng.IntN(int(watchEventCount)))
			next, err := nextWatchPhase(phase, ev)
			if err != nil {
				continue
			}
			if sealed {
				require.True(t, next == watchPhaseDraining || next == watchPhaseStopped,
					"%s x %s reopened admission as %s", phase, ev, next)
				if phase == watchPhaseDraining && next == watchPhaseStopped {
					require.Equal(t, watchEventDrained, ev)
				}
			}
			sealed = sealed || next == watchPhaseDraining
			phase = next
		}
	}
}

// Every input is ready; the select in each phase may read only what the
// phase's row of the input table enables.
func TestWatchLoop_ServicesOnlyThePhasesEnabledInputs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		phase watchPhase
		reads map[string]bool
	}{
		{watchPhaseBootstrap, map[string]bool{"local": false, "remote": false, "refreshTick": false, "observerErrs": false, "log": true}},
		{watchPhaseRunning, map[string]bool{"local": true, "remote": true, "refreshTick": true, "observerErrs": true, "log": false}},
	} {
		t.Run(tc.phase.String(), func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			setupWatchEngine(t, eng)
			rt := testWatchRuntime(t, eng)
			setWatchPhase(rt, tc.phase)

			local := make(chan syncobserve.LocalObservation, 1)
			local <- syncobserve.LocalObservation{}
			remote := make(chan syncobserve.RemoteObservationBatch, 1)
			remote <- syncobserve.RemoteObservationBatch{}
			errs := make(chan error, 1)
			errs <- assert.AnError
			logC := make(chan time.Time, 1)
			logC <- time.Time{}
			rt.resources.localBatches = local
			rt.resources.remoteBatches = remote
			rt.resources.protectedRootEvents = make(chan syncshortcut.ProtectedRootEvent)
			rt.resources.observerErrs = errs
			rt.resources.activeObservers = 2
			// A refresh tick starts a real refresh, so only the phase that
			// must ignore it is given one.
			if !tc.reads["refreshTick"] {
				rt.resources.refreshCh <- time.Time{}
			}

			// The loop runs until every enabled input has been read and it
			// blocks; an input the phase disables is still full by then.
			ctx, cancel := context.WithCancel(t.Context())
			stepped := make(chan struct{})
			go func() {
				defer close(stepped)
				for ctx.Err() == nil {
					_, _ = rt.runNonDrainingWatchStep(ctx, &watchPipeline{runtime: rt}, logC)
				}
			}()
			enabledRead := func() bool {
				return (!tc.reads["local"] || len(local) == 0) &&
					(!tc.reads["remote"] || len(remote) == 0) &&
					(!tc.reads["observerErrs"] || len(errs) == 0) &&
					(!tc.reads["log"] || len(logC) == 0)
			}
			require.Eventually(t, enabledRead, 10*time.Second, time.Millisecond)
			cancel()
			<-stepped

			assert.Equal(t, tc.reads["local"], len(local) == 0, "local batches")
			assert.Equal(t, tc.reads["remote"], len(remote) == 0, "remote batches")
			if !tc.reads["refreshTick"] {
				assert.Len(t, rt.resources.refreshCh, 1, "refresh tick")
			}
			assert.Equal(t, tc.reads["observerErrs"], len(errs) == 0, "observer errors")
			assert.Equal(t, tc.reads["log"], len(logC) == 0, "bootstrap log tick")
		})
	}
	assert.Empty(t, watchInputsEnabled[watchPhaseStarting], "nothing is read before bootstrap")
	assert.Empty(t, watchInputsEnabled[watchPhaseStopped], "nothing is read once stopped")
	assert.False(t, watchInputOn(watchPhaseDraining, watchInputDispatch), "draining never dispatches")
}

// A stop that lands during bootstrap ends the watch in Stopped; no observer
// is started, rather than reaching startObservers and relying on its refusal.
func TestRunWatch_CancelDuringBootstrapStopsWithoutStartingObservers(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	rt := newWatchRuntime(eng.Engine)
	require.NoError(t, rt.transition(watchEventBootstrapPlanned))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, rt.runLoopSteps(ctx, &watchPipeline{runtime: rt}, nil, true))
	assert.Equal(t, watchPhaseStopped, rt.phase())
	require.ErrorIs(t, rt.beginObserverBackedRunning(), errWatchObserversDuringDrain)
	assert.Zero(t, rt.resources.activeObservers)
}
