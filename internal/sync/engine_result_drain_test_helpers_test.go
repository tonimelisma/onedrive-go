package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// startDrainLoop creates a real engine with DepGraph, watch-mode scope state,
// dispatchCh, dirty scheduler, and retry/trial timers. The returned stop
// cancels the loop and waits for it to return; only the loop's goroutine
// owns the scope ledger, so a test reads the ledger after stop. Calling stop
// again does nothing.
func startDrainLoop(t *testing.T) (chan syncdomain.ActionCompletion, func(), *testEngine) {
	t.Helper()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.sched.ResetWindows()
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	results := make(chan syncdomain.ActionCompletion, 16)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	cancel, done := syncfixture.RunInBackground(t, t.Context(), func(ctx context.Context) error {
		defer rt.stopTrialTimer()
		runResultDrainLoopForTest(ctx, rt, bl, results)
		return nil
	})

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}

	return results, stop, eng
}

func runResultDrainLoopForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl syncdomain.BaselineView,
	results <-chan syncdomain.ActionCompletion,
) {
	var outbox []*syncdomain.TrackedAction

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
	bl syncdomain.BaselineView,
	results <-chan syncdomain.ActionCompletion,
) ([]*syncdomain.TrackedAction, bool) {
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
	bl syncdomain.BaselineView,
	results <-chan syncdomain.ActionCompletion,
	outbox []*syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, bool) {
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
	bl syncdomain.BaselineView,
	outbox []*syncdomain.TrackedAction,
	workerResult *syncdomain.ActionCompletion,
) ([]*syncdomain.TrackedAction, bool) {
	ready, completionErr := rt.applyRuntimeCompletionStage(ctx, workerResult, bl)
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
func readReadyAction(t *testing.T, ready <-chan *syncdomain.TrackedAction) *syncdomain.TrackedAction {
	t.Helper()

	select {
	case ta := <-ready:
		return ta
	case <-time.After(syncfixture.EventWait):
		require.Fail(t, "timed out waiting for action on ready channel")
	}

	return nil
}

func readReady(t *testing.T, ready <-chan *syncdomain.TrackedAction) {
	t.Helper()
	_ = readReadyAction(t, ready)
}
