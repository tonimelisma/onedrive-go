package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// handleMaintenanceTick processes a periodic watch summary/maintenance tick.
func (rt *watchRuntime) handleMaintenanceTick(ctx context.Context) {
	rt.logWatchSummary(ctx)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventMaintenanceTickHandled})
}

func (e *Engine) fullRemoteRefreshDelay(ctx context.Context) (time.Duration, error) {
	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return 0, fmt.Errorf("sync: reading observation state for remote refresh cadence: %w", err)
	}
	if state.NextFullRemoteRefreshAt == 0 {
		return 0, nil
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

	rt.resetRefreshTimer(rt.engine.afterFunc(delay, func() {
		select {
		case rt.refreshCh <- rt.engine.nowFunc():
		default:
		}
	}))

	rt.engine.logger.Info("full remote refresh armed",
		slog.Duration("delay", delay),
		slog.Time("due_at", time.Unix(0, state.NextFullRemoteRefreshAt)),
	)

	return nil
}

// runFullRemoteRefreshAsync spawns a goroutine for full delta enumeration +
// orphan detection. Non-blocking — the watch loop continues processing events
// while refresh work runs. The goroutine sends one loop-owned remote batch back
// to the watch loop; the loop owns durable apply and dirty marking.
func (rt *watchRuntime) runFullRemoteRefreshAsync(ctx context.Context, bl *Baseline) {
	if rt.refreshActive {
		rt.engine.logger.Info("full remote refresh skipped — previous still running")
		return
	}
	rt.refreshActive = true
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshStarted})

	go func() {
		result := rt.performFullRemoteRefresh(ctx, bl)
		rt.finishFullRemoteRefresh(ctx, &result)
	}()
}

func (rt *watchRuntime) performFullRemoteRefresh(
	ctx context.Context,
	bl *Baseline,
) remoteObservationBatch {
	result := remoteObservationBatch{
		source: remoteObservationBatchFullRefresh,
	}
	start := rt.engine.nowFunc()
	defer func() {
		rt.engine.collector().RecordRefresh(len(result.emitted), rt.engine.since(start))
	}()

	rt.engine.logger.Info("periodic full remote refresh starting")

	observationBatch, err := rt.executePrimaryRootObservation(ctx, bl, true)
	if err != nil {
		if ctx.Err() == nil {
			rt.engine.logger.Error("full remote refresh failed",
				slog.String("error", err.Error()),
			)
		}
		result.markFullRefreshIfIdle = true
		return result
	}

	observationBatch.source = remoteObservationBatchFullRefresh
	observationBatch.armFullRefreshTimer = true
	observationBatch.markFullRefreshIfIdle = len(observationBatch.emitted) == 0
	result = observationBatch
	if len(result.emitted) == 0 {
		rt.engine.logger.Info("periodic full remote refresh complete: no changes",
			slog.Duration("duration", rt.engine.since(start)),
		)
		return result
	}

	rt.engine.logger.Info("periodic full remote refresh complete",
		slog.Int("events", len(result.emitted)),
		slog.Duration("duration", rt.engine.since(start)),
	)

	return result
}

func (rt *watchRuntime) finishFullRemoteRefresh(ctx context.Context, result *remoteObservationBatch) {
	select {
	case rt.refreshResults <- *result:
	case <-ctx.Done():
		select {
		case rt.refreshResults <- *result:
		default:
		}
	}
}

func (rt *watchRuntime) applyRemoteRefreshResult(
	ctx context.Context,
	result *remoteObservationBatch,
) error {
	return rt.handleRemoteObservationBatch(ctx, result)
}

func (rt *watchRuntime) dropRemoteRefreshResultOnShutdown() {
	rt.refreshActive = false
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshDroppedOnShutdown})
}
