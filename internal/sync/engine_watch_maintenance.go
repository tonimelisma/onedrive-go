package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// handleRecheckTick processes a maintenance tick for watch-mode summary
// logging and debug-event bookkeeping.
func (rt *watchRuntime) handleRecheckTick(ctx context.Context) {
	rt.logWatchSummary(ctx)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRecheckTickHandled})
}

func (e *Engine) fullRemoteRefreshDelay(ctx context.Context) (time.Duration, error) {
	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return 0, fmt.Errorf("sync: reading observation state for remote refresh cadence: %w", err)
	}
	if state.NextFullRemoteRefreshAt == 0 {
		if state.LastFullRemoteRefreshAt == 0 {
			return 0, nil
		}
		state.NextFullRemoteRefreshAt = time.Unix(0, state.LastFullRemoteRefreshAt).
			Add(remoteRefreshIntervalForMode(state.RemoteRefreshMode)).
			UnixNano()
	}

	dueAt := time.Unix(0, state.NextFullRemoteRefreshAt)
	delay := dueAt.Sub(e.nowFunc())
	if delay < 0 {
		return 0, nil
	}

	return delay, nil
}

func (e *Engine) shouldRunFullRemoteRefresh(ctx context.Context, requested bool) (bool, error) {
	if requested {
		return true, nil
	}

	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: reading observation state for full remote refresh: %w", err)
	}
	if state.Cursor == "" || state.NextFullRemoteRefreshAt == 0 {
		return true, nil
	}

	dueAt := time.Unix(0, state.NextFullRemoteRefreshAt)
	return !e.nowFunc().Before(dueAt), nil
}

func (rt *watchRuntime) armFullRefreshTimer(ctx context.Context) error {
	delay, err := rt.engine.fullRemoteRefreshDelay(ctx)
	if err != nil {
		return err
	}
	state, err := rt.engine.baseline.ReadObservationState(ctx)
	if err != nil {
		return fmt.Errorf("sync: reading observation state for remote refresh timer: %w", err)
	}
	interval := remoteRefreshIntervalForMode(state.RemoteRefreshMode)

	rt.resetRefreshTimer(rt.engine.afterFunc(delay, func() {
		select {
		case rt.refreshCh <- rt.engine.nowFunc():
		default:
		}
	}))

	rt.engine.logger.Info("full remote refresh armed",
		slog.Duration("delay", delay),
		slog.Duration("interval", interval),
	)

	return nil
}

// runFullRemoteRefreshAsync spawns a goroutine for full delta enumeration +
// orphan detection. Non-blocking — the watch loop continues processing events
// while refresh work runs. The goroutine sends a remoteRefreshResult back to the
// watch loop, and the loop feeds the returned events into its buffer from its
// own goroutine.
func (rt *watchRuntime) runFullRemoteRefreshAsync(ctx context.Context, bl *Baseline) {
	if rt.refreshActive {
		rt.engine.logger.Info("full remote refresh skipped — previous still running")
		return
	}
	rt.refreshActive = true
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshStarted})

	go func() {
		rt.finishFullRemoteRefresh(ctx, rt.performFullRemoteRefresh(ctx, bl))
	}()
}

func (rt *watchRuntime) performFullRemoteRefresh(
	ctx context.Context,
	bl *Baseline,
) remoteRefreshResult {
	result := remoteRefreshResult{}
	start := rt.engine.nowFunc()
	defer func() {
		rt.engine.collector().RecordRefresh(len(result.events), rt.engine.since(start))
	}()

	rt.engine.logger.Info("periodic full remote refresh starting")

	plan := rt.buildPrimaryRootObservationPlan(true)
	projectedPrimary, err := rt.observeCommittedFullRemoteRefreshBatch(ctx, bl, plan)
	if err != nil {
		if ctx.Err() == nil {
			rt.engine.logger.Error("full remote refresh failed",
				slog.String("error", err.Error()),
			)
		}
		return result
	}

	if ctx.Err() != nil {
		rt.engine.logger.Info("full remote refresh: observations committed, stopping for shutdown")
		return result
	}

	events := append([]ChangeEvent(nil), projectedPrimary.emitted...)
	if len(events) == 0 {
		rt.engine.logger.Info("periodic full remote refresh complete: no changes",
			slog.Duration("duration", rt.engine.since(start)),
		)
		return result
	}

	result.events = events

	rt.engine.logger.Info("periodic full remote refresh complete",
		slog.Int("events", len(events)),
		slog.Duration("duration", rt.engine.since(start)),
	)

	return result
}

func (rt *watchRuntime) observeCommittedFullRemoteRefreshBatch(
	ctx context.Context,
	bl *Baseline,
	plan primaryRootObservationPlan,
) (remoteObservationResult, error) {
	fetchResult, err := rt.executePrimaryRootObservation(ctx, bl, plan)
	if err != nil {
		return remoteObservationResult{}, err
	}

	projectedPrimary := projectRemoteObservations(rt.engine.logger, fetchResult.events)
	if commitErr := rt.commitObservedItems(ctx, projectedPrimary.observed, ""); commitErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full remote refresh observations: %w", commitErr)
	}
	if tokenErr := rt.commitPendingPrimaryCursor(ctx, fetchResult.pending); tokenErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full remote refresh primary cursor: %w", tokenErr)
	}
	if armErr := rt.armFullRefreshTimer(ctx); armErr != nil {
		return remoteObservationResult{}, fmt.Errorf("arm full remote refresh timer: %w", armErr)
	}

	if rt.afterRefreshCommit != nil {
		rt.afterRefreshCommit()
	}

	return projectedPrimary, nil
}

func (rt *watchRuntime) finishFullRemoteRefresh(ctx context.Context, result remoteRefreshResult) {
	select {
	case rt.refreshResults <- result:
	case <-ctx.Done():
		select {
		case rt.refreshResults <- result:
		default:
		}
	}
}

func (rt *watchRuntime) applyRemoteRefreshResult(result remoteRefreshResult) {
	rt.refreshActive = false

	if rt.dirtyBuf != nil {
		if len(result.events) == 0 {
			rt.dirtyBuf.MarkFullRefresh()
		}
		for i := range result.events {
			if result.events[i].Path != "" {
				rt.dirtyBuf.MarkPath(result.events[i].Path)
			}
			if result.events[i].OldPath != "" {
				rt.dirtyBuf.MarkPath(result.events[i].OldPath)
			}
		}
	}

	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshApplied})
}

func (rt *watchRuntime) dropRemoteRefreshResultOnShutdown() {
	rt.refreshActive = false
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshDroppedOnShutdown})
}
