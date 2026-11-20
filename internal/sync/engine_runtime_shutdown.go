package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// stopDrain is the one bound a stopping run records the results of its
// completions under. A completion that reaches a run after its stop is the
// result of work that already happened (a transfer finished, OneDrive refused
// a request, an item proved gone), so its records are written on a context the
// stop does not cancel. One deadline covers the whole drain's records and is
// measured from the cancel, so a store that stops answering ends them one
// bound after the cancel whatever the number of completions, and a wait the
// run's goroutine was in when the cancel landed does not add a bound of its
// own. The run's one goroutine owns it.
type stopDrain struct {
	// deadline ends the bound: stopBound after the run's context ended, or
	// after the run first saw a stop no cancel caused, on the real clock that
	// context deadlines run on.
	deadline time.Time

	// unrecorded counts the completions whose records the bound cut short;
	// firstErr is the first of their errors.
	unrecorded int
	firstErr   error
}

// stoppingRunKey marks the context a stopping run applies its completions on.
type stoppingRunKey struct{}

// isStoppingRun reports the context of a stopping run's completion. Such a
// completion records what happened and starts nothing: admission, the
// frontier's reduction (publication and the release of due held work), the
// permission probes, a scope trial's included, and the persisted invariant
// checks leave its frontier to complete as shutdown and its due held work
// held. A scope trial that succeeded still releases its scope's next batch,
// which is a record of the success: the batch completes as shutdown, its rows
// unblocked for the next sync.
func isStoppingRun(ctx context.Context) bool {
	stopping, ok := ctx.Value(stoppingRunKey{}).(bool)
	return ok && stopping
}

// noteRunCancel records in canceledAt when ctx ends, so the stop's bound
// starts at the cancel even when the run's goroutine is in a store wait then
// and sees the stop only later. The callback runs on a goroutine of its own,
// inside the boundary of the engine's timer callbacks. The returned function
// stops the recording; the run calls it as it returns.
func (flow *engineFlow) noteRunCancel(ctx context.Context) func() bool {
	return context.AfterFunc(ctx, func() {
		defer flow.engine.wallClock.RecoverTimerPanic()
		canceled := time.Now()
		flow.canceledAt.CompareAndSwap(nil, &canceled)
	})
}

// beginStopDrain sets the stop's one bound the first time the run sees its
// stop; a later call keeps the bound already running. The bound starts at the
// cancel (canceledAt), or now for a stop no cancel caused. It waits for
// nothing and holds nothing: a stop that drains no completion costs nothing.
func (flow *engineFlow) beginStopDrain() {
	if flow.stop != nil {
		return
	}

	start := time.Now()
	if canceled := flow.canceledAt.Load(); canceled != nil && canceled.Before(start) {
		start = *canceled
	}
	flow.stop = &stopDrain{deadline: start.Add(flow.stopBound)}
}

// endStopDrain ends the stop's bound once the drain is over and says, once,
// how many results the bound cut short. It returns the store failure the run
// then ends with, as a store failure ends a run that goes on, or nil when
// every record was written. The bound runs out when the sync database does
// not answer within it, or when a completion reaches the drain after it, so
// both the warning and the error say that the stop's time ran out; the
// store's error and ErrStoreAccessTimedOut stay the error's cause.
func (flow *engineFlow) endStopDrain() error {
	drain := flow.stop
	if drain == nil {
		return nil
	}
	flow.stop = nil
	if drain.unrecorded == 0 {
		return nil
	}

	flow.deps.logger.Warn(unrecordedResultsMessage(drain.unrecorded),
		slog.Int("unrecorded", drain.unrecorded),
		slog.Any("error", drain.firstErr),
	)

	return fmt.Errorf("sync: the stop's time ran out before recording %d of its results: %w",
		drain.unrecorded, drain.firstErr)
}

// unrecordedResultsMessage is the one warning of a drain whose bound cut
// count records short.
func unrecordedResultsMessage(count int) string {
	if count == 1 {
		return "The stop's time ran out before 1 result was recorded; the next sync will check it again"
	}

	return fmt.Sprintf("The stop's time ran out before %d results were recorded; the next sync will check them again", count)
}

// processShutdownCompletion applies a stopping run's completion on a context
// that keeps ctx's values, not its cancellation, and ends with the stop's one
// bound (beginStopDrain): its records are written as a live run writes them,
// and nothing new starts (isStoppingRun), so the frontier it makes ready
// completes as shutdown instead of reaching dispatch. A record the bound cut
// short is counted for endStopDrain to say, not returned.
func (flow *engineFlow) processShutdownCompletion(
	ctx context.Context,
	completion *syncdomain.ActionCompletion,
	bl *syncdomain.Baseline,
) error {
	flow.beginStopDrain()
	drain := flow.stop
	detached, cancel := syncexec.DetachedStoreContext(ctx, drain.deadline)
	defer cancel()
	drainCtx := context.WithValue(detached, stoppingRunKey{}, true)

	ready, err := flow.applyRuntimeCompletionStage(drainCtx, completion, bl)
	flow.completeOutboxAsShutdown(ready)

	var fatalErr *fatalCompletionError
	if err == nil || errors.As(err, &fatalErr) || !errors.Is(context.Cause(drainCtx), syncdomain.ErrStoreAccessTimedOut) {
		return err
	}
	drain.unrecorded++
	if drain.firstErr == nil {
		drain.firstErr = syncexec.StoreAccessError(drainCtx, err)
	}

	return nil
}

func (flow *engineFlow) completeOutboxAsShutdown(outbox []*syncdomain.TrackedAction) {
	for _, ta := range outbox {
		flow.completeTrackedActionAsShutdown(ta)
	}
}

func (flow *engineFlow) completeTrackedActionAsShutdown(ta *syncdomain.TrackedAction) {
	if ta == nil {
		return
	}

	flow.sched.markFinished(ta)
	ready := flow.completeDepGraphAction(ta.ID, "completeTrackedActionAsShutdown")
	flow.completeSubtree(ready)
}

func (flow *engineFlow) logSuppressedShutdownCompletionError(
	completion *syncdomain.ActionCompletion,
	err error,
) {
	if completion == nil || err == nil {
		return
	}

	fields := []any{
		slog.String("path", completion.Path),
		slog.String("action_type", completion.ActionType.String()),
		slog.Any("error", err),
	}

	// Only a stopping run gets here, with a record that failed for a reason
	// other than the stop's bound, which processShutdownCompletion counts
	// instead. A repeated sign-in failure is already the run's returned error,
	// and the echo of a cancel adds nothing to the stop the user asked for;
	// neither is news to the user. Anything else means a finished transfer may
	// not be recorded; the next sync rediscovers it from durable truth
	// (R-2.5.3).
	var fatalErr *fatalCompletionError
	if syncdomain.IsCancellationEcho(err) || errors.As(err, &fatalErr) {
		flow.deps.logger.Debug("suppressed action completion error during shutdown", fields...)
		return
	}

	flow.deps.logger.Warn("Could not record the result for "+completion.Path+
		" while stopping; the next sync will check it again", fields...)
}
