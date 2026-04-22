package sync

import (
	"context"
	"fmt"
	"log/slog"
)

// runWatchUntilQuiescent drives the bootstrap watch loop until all work due
// now has drained through the shared runtime. Future-held retry/scope work may
// remain unresolved in the graph, so bootstrap quiescence is engine-owned
// rather than defined by graph emptiness.
func (rt *watchRuntime) runWatchUntilQuiescent(
	ctx context.Context,
	p *watchPipeline,
	initialOutbox []*TrackedAction,
) error {
	ticker := rt.engine.newTicker(quiescenceLogInterval)
	defer stopTicker(ticker)

	rt.replaceOutbox(initialOutbox)
	logC := tickerChan(ticker)

	for {
		if ctx.Err() != nil && !rt.isDraining() {
			rt.beginWatchDrain(ctx, p)
		}
		if rt.isDraining() {
			done, err := rt.runDrainStep(ctx, p)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}

		if rt.isBootstrapQuiescent() {
			return nil
		}

		done, err := rt.runBootstrapStep(ctx, p, logC)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed replans, action completions, retry/trial timers, reconcile
// completions, and outbox draining.
func (rt *watchRuntime) runWatchLoop(ctx context.Context, p *watchPipeline) error {
	rt.replaceOutbox(nil)

	for {
		if ctx.Err() != nil && !rt.isDraining() {
			rt.beginWatchDrain(ctx, p)
		}
		if rt.isDraining() {
			done, err := rt.runDrainStep(ctx, p)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}
		replanned, err := rt.runPendingReplan(ctx, p)
		if err != nil {
			return err
		}
		if replanned {
			continue
		}

		done, err := rt.runWatchStep(ctx, p)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (rt *watchRuntime) runPendingReplan(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	if !rt.canPrepareNow() {
		return false, nil
	}

	batch, ok := rt.takePendingReplan()
	if !ok {
		return false, nil
	}

	return true, rt.runSteadyStateReplan(ctx, p, batch)
}

func (rt *watchRuntime) runWatchStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	event := rt.waitWatchEvent(ctx, p)
	return rt.handleWatchEvent(ctx, p, &event)
}

func (rt *watchRuntime) appendReadyFrontier(
	ctx context.Context,
	p *watchPipeline,
	ready []*TrackedAction,
) error {
	reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, ready)
	nextOutbox := append(rt.currentOutbox(), reduced...)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.completeOutboxAsShutdown(nextOutbox)
		return err
	}

	rt.maybeFinishSyncStatusBatch(ctx, p.mode, nextOutbox)
	rt.replaceOutbox(nextOutbox)
	return nil
}

func (rt *watchRuntime) releaseHeldFrontier(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	var (
		released []*TrackedAction
		err      error
	)
	if trial {
		released, err = rt.releaseDueHeldTrialsNow(ctx)
	} else {
		released, err = rt.releaseDueHeldRetriesNow(ctx)
	}
	if err != nil {
		return err
	}

	return rt.appendReadyFrontier(ctx, p, released)
}

func (rt *watchRuntime) applyRuntimeCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
) error {
	ready, err := rt.processActionCompletion(ctx, rt, completion, p.bl)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.completeOutboxAsShutdown(ready)
		return err
	}

	return rt.appendReadyFrontier(ctx, p, ready)
}

func (rt *watchRuntime) handleObserverExit(p *watchPipeline, shuttingDown bool) error {
	rt.mustAssertObserverExitPhase(rt, shuttingDown, "handle observer exit")

	p.activeObs--
	if p.activeObs > 0 {
		return nil
	}

	if shuttingDown {
		rt.engine.logger.Info("all observers exited during shutdown")
		return nil
	}

	rt.engine.logger.Error("all observers have exited, stopping watch mode")
	return fmt.Errorf("sync: all observers exited")
}

func (rt *watchRuntime) logObserverError(obsErr error) {
	if obsErr == nil {
		return
	}

	rt.engine.logger.Warn("observer error",
		slog.String("error", obsErr.Error()),
	)
}

func (rt *watchRuntime) dispatchChannelForOutbox() (chan<- *TrackedAction, *TrackedAction) {
	outbox := rt.currentOutbox()
	nextAction := firstOutbox(outbox)
	if nextAction == nil {
		return nil, nil
	}

	if rt.isDraining() {
		rt.mustAssertDispatchAdmissionSealed(rt, outbox, "dispatch channel for outbox")
		return nil, nil
	}

	return rt.dispatchCh, nextAction
}

func firstOutbox(outbox []*TrackedAction) *TrackedAction {
	if len(outbox) == 0 {
		return nil
	}

	return outbox[0]
}
