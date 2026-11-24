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
//
// A completion the run began to apply while it went on becomes a stopping
// run's completion from the moment the run's context ends
// (syncexec.RunHasEnded): what it has left to do records and starts nothing.
func isStoppingRun(ctx context.Context) bool {
	stopping, ok := ctx.Value(stoppingRunKey{}).(bool)
	return (ok && stopping) || syncexec.RunHasEnded(ctx)
}

// withRunStop marks ctx as this run's context (syncexec.WithRunStop): once it
// ends, every detached store wait under it, a worker's or an executor's
// included, ends at the stop's one deadline when that comes first.
func (flow *engineFlow) withRunStop(ctx context.Context) context.Context {
	return syncexec.WithRunStop(ctx, flow.stopDeadlineAfterCancel, flow.deps.logger)
}

// stopDeadlineAfterCancel is the stop's one deadline as any goroutine reads it
// once the run's context ended: stopBound after the cancel (canceledAt), or
// after now while the callback that records the cancel has not run yet. It
// reads only the atomic canceledAt and the fixed stopBound, never the drain
// the run's goroutine owns.
func (flow *engineFlow) stopDeadlineAfterCancel() time.Time {
	if canceled := flow.canceledAt.Load(); canceled != nil {
		return canceled.Add(flow.stopBound)
	}

	return syncdomain.WallNow().Add(flow.stopBound)
}

// applyLiveCompletion applies a completion that reached the run while it went
// on. It runs on a context the run's cancel does not cut
// (syncexec.DetachedAtStopContext): a cancel that lands while the completion
// is being applied lets it finish under the stop's one deadline, as a
// completion that reaches the run after its stop does, and from that moment
// it starts nothing (isStoppingRun). A record that deadline cut short is
// counted in the stop's one warning (endStopDrain), not returned.
func (flow *engineFlow) applyLiveCompletion(
	ctx context.Context,
	completion *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) ([]*syncdomain.TrackedAction, error) {
	completionCtx, release := syncexec.DetachedAtStopContext(ctx)
	defer release()

	ready, err := flow.applyRuntimeCompletionStage(completionCtx, completion, bl)
	if err == nil || !syncexec.RunHasEnded(completionCtx) {
		return ready, err
	}

	var fatalErr *fatalCompletionError
	if errors.As(err, &fatalErr) || !errors.Is(context.Cause(completionCtx), syncdomain.ErrStoreAccessTimedOut) {
		return ready, err
	}
	flow.countUnrecorded(syncexec.StoreAccessError(completionCtx, err))

	return ready, nil
}

// countUnrecorded counts in the stop's drain a record its deadline cut short.
func (flow *engineFlow) countUnrecorded(err error) {
	flow.beginStopDrain()
	flow.stop.unrecorded++
	if flow.stop.firstErr == nil {
		flow.stop.firstErr = err
	}
}

// noteRunCancel records in canceledAt when ctx ends, so the stop's bound
// starts at the cancel even when the run's goroutine is in a store wait then
// and sees the stop only later. The callback runs on a goroutine of its own,
// inside the boundary of the engine's timer callbacks. The returned function
// stops the recording; the run calls it as it returns.
func (flow *engineFlow) noteRunCancel(ctx context.Context) func() bool {
	return context.AfterFunc(ctx, func() {
		defer flow.deps.wall.RecoverTimerPanic()
		// The bound is a context deadline, enforced on wall time.
		canceled := syncdomain.WallNow()
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

	flow.stop = &stopDrain{deadline: flow.stopDeadline()}
}

// stopDeadline is the stop's one deadline: the running drain's, or, while no
// drain runs, stopBound after the cancel (canceledAt), or after now for a stop
// no cancel caused. Reading it starts nothing.
func (flow *engineFlow) stopDeadline() time.Time {
	if flow.stop != nil {
		return flow.stop.deadline
	}

	start := syncdomain.WallNow()
	if canceled := flow.canceledAt.Load(); canceled != nil && canceled.Before(start) {
		start = *canceled
	}

	return start.Add(flow.stopBound)
}

// runStoppedStoreAccess runs a store wait that comes after the run's context
// ended, on the run's goroutine, through the one detached-access helper and
// under the stop's one deadline (stopDeadline), never under a bound of its
// own: an expired deadline carries ErrStoreAccessTimedOut.
func (flow *engineFlow) runStoppedStoreAccess(ctx context.Context, access func(context.Context) error) error {
	return syncexec.RunDetachedStoreAccessUntil(ctx, flow.stopDeadline(), access)
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
	bl syncdomain.BaselineView,
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
	flow.countUnrecorded(syncexec.StoreAccessError(drainCtx, err))

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

	flow.sched.Finished(ta)
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
	if isCancellationEcho(err) || errors.As(err, &fatalErr) {
		flow.deps.logger.Debug("suppressed action completion error during shutdown", fields...)
		return
	}

	flow.deps.logger.Warn("Could not record the result for "+completion.Path+
		" while stopping; the next sync will check it again", fields...)
}
