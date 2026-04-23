package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startDrainLoop creates a real engine with DepGraph, watch-mode scope state,
// dispatchCh, dirty scheduler, and retry/trial timers.
func startDrainLoop(t *testing.T) (chan ActionCompletion, context.CancelFunc, *testEngine) {
	t.Helper()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	results := make(chan ActionCompletion, 16)

	ctx, cancel := context.WithCancel(t.Context())
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rt.stopTrialTimer()
		runResultDrainLoopForTest(ctx, rt, bl, results)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return results, cancel, eng
}

func runResultDrainLoopForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *Baseline,
	results <-chan ActionCompletion,
) {
	var outbox []*TrackedAction

	for {
		if len(outbox) == 0 {
			nextOutbox, done := runResultDrainLoopIdleForTest(ctx, rt, bl, results)
			outbox = nextOutbox
			if done {
				return
			}
			continue
		}

		nextOutbox, done := runResultDrainLoopWithOutboxForTest(ctx, rt, bl, results, outbox)
		outbox = nextOutbox
		if done {
			return
		}
	}
}

func runResultDrainLoopIdleForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *Baseline,
	results <-chan ActionCompletion,
) ([]*TrackedAction, bool) {
	select {
	case workerResult, ok := <-results:
		if !ok {
			return nil, true
		}
		return appendDrainOutcome(rt, ctx, bl, nil, &workerResult)
	case <-rt.trialTimerChan():
		released, err := rt.releaseDueHeldTrialsNow(ctx, bl)
		mustNoDrainLoopError(err)
		return released, false
	case <-rt.retryTimerChan():
		released, err := rt.releaseDueHeldRetriesNow(ctx, bl)
		mustNoDrainLoopError(err)
		return released, false
	case <-ctx.Done():
		return nil, true
	}
}

func runResultDrainLoopWithOutboxForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *Baseline,
	results <-chan ActionCompletion,
	outbox []*TrackedAction,
) ([]*TrackedAction, bool) {
	select {
	case rt.dispatchCh <- outbox[0]:
		return outbox[1:], false
	case workerResult, ok := <-results:
		if !ok {
			return outbox, true
		}
		return appendDrainOutcome(rt, ctx, bl, outbox, &workerResult)
	case <-rt.trialTimerChan():
		released, err := rt.releaseDueHeldTrialsNow(ctx, bl)
		mustNoDrainLoopError(err)
		return append(outbox, released...), false
	case <-rt.retryTimerChan():
		released, err := rt.releaseDueHeldRetriesNow(ctx, bl)
		mustNoDrainLoopError(err)
		return append(outbox, released...), false
	case <-ctx.Done():
		return outbox, true
	}
}

func appendDrainOutcome(
	rt *watchRuntime,
	ctx context.Context,
	bl *Baseline,
	outbox []*TrackedAction,
	workerResult *ActionCompletion,
) ([]*TrackedAction, bool) {
	ready, completionErr := rt.applyRuntimeCompletionStage(ctx, rt, workerResult, bl)
	if completionErr != nil {
		return outbox, true
	}

	return append(outbox, ready...), false
}

func mustNoDrainLoopError(err error) {
	if err != nil {
		panic(err)
	}
}

// readReadyAction reads one TrackedAction from the ready channel with a
// race-detector-friendly timeout.
func readReadyAction(t *testing.T, ready <-chan *TrackedAction) *TrackedAction {
	t.Helper()

	select {
	case ta := <-ready:
		return ta
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for action on ready channel")
	}

	return nil
}

func readReady(t *testing.T, ready <-chan *TrackedAction) {
	t.Helper()
	_ = readReadyAction(t, ready)
}
