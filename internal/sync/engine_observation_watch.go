package sync

import (
	"context"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (rt *watchRuntime) startPrimaryWatchPhase(
	ctx context.Context,
	obsWg *stdsync.WaitGroup,
	bl *synctypes.Baseline,
	events chan<- synctypes.ChangeEvent,
	errs chan<- error,
	pollInterval time.Duration,
	phase ObservationPhasePlan,
) {
	if err := validatePrimaryObservationPhase(phase); err != nil {
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- fmt.Errorf("start primary watch phase: %w", err)
		}()
		return
	}

	rt.warnPhaseWebsocketFallbackIfNeeded(phase)

	switch phase.Driver {
	case observationPhaseDriverScopedTarget:
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- rt.watchPrimaryScopedRemote(ctx, bl, events, pollInterval, phase)
		}()
	case observationPhaseDriverScopedRoot:
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- rt.watchScopedRootRemote(ctx, bl, events, pollInterval, phase)
		}()
	case observationPhaseDriverRootDelta:
		remoteObs := syncobserve.NewRemoteObserver(rt.engine.fetcher, bl, rt.engine.driveID, rt.engine.logger)
		remoteObs.SetObsWriter(rt.engine.baseline)
		remoteObs.SetWatchObservationPreparer(func(
			_ context.Context,
			events []synctypes.ChangeEvent,
		) ([]synctypes.ObservedItem, []synctypes.ChangeEvent, error) {
			scoped := applyRemoteScope(rt.engine.logger, rt.currentScopeSnapshot(), rt.currentScopeGeneration(), events)
			return scoped.observed, scoped.emitted, nil
		})
		remoteObs.SetWatchBatchPostProcessor(func(ctx context.Context, primaryEvents []synctypes.ChangeEvent) []synctypes.ChangeEvent {
			finalEvents, err := rt.processCommittedPrimaryBatch(
				ctx,
				bl,
				primaryEvents,
				rt.currentScopeSnapshot(),
				rt.currentScopeGeneration(),
				false,
				false,
			)
			if err != nil {
				rt.engine.logger.Warn("shortcut processing failed during remote watch batch",
					slog.String("error", err.Error()),
				)
				return filterOutShortcuts(primaryEvents)
			}

			return finalEvents
		})
		rt.remoteObs = remoteObs

		if rt.websocketEnabledForPrimaryPhase(phase) {
			rt.startSocketIOWakeSource(ctx, remoteObs)
		}

		savedToken, tokenErr := rt.engine.baseline.GetDeltaToken(ctx, rt.engine.driveID.String(), "")
		if tokenErr != nil {
			rt.engine.logger.Warn("failed to get delta token for watch",
				slog.String("error", tokenErr.Error()),
			)
		}

		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- remoteObs.Watch(ctx, savedToken, events, pollInterval)
		}()
	default:
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- fmt.Errorf("start primary watch phase: unknown driver %q", phase.Driver)
		}()
	}
}

func (rt *watchRuntime) warnPhaseWebsocketFallbackIfNeeded(phase ObservationPhasePlan) {
	switch phase.Driver {
	case observationPhaseDriverScopedRoot:
		if rt.engine.enableWebsocket {
			rt.engine.emitDebugEvent(engineDebugEvent{
				Type: engineDebugEventWebsocketFallback,
				Note: "scoped_root",
			})
			rt.engine.logger.Warn("websocket watch is not supported for scoped-root sessions; falling back to polling",
				slog.String("drive_id", rt.engine.driveID.String()),
				slog.String("root_item_id", rt.engine.rootItemID),
			)
		}
	case observationPhaseDriverScopedTarget:
		if rt.engine.enableWebsocket {
			rt.engine.emitDebugEvent(engineDebugEvent{
				Type: engineDebugEventWebsocketFallback,
				Note: "sync_paths",
			})
			rt.engine.logger.Warn("websocket watch is not supported for sync_paths-scoped sessions; falling back to polling",
				slog.String("drive_id", rt.engine.driveID.String()),
			)
		}
	case observationPhaseDriverRootDelta:
		if rt.engine.enableWebsocket && rt.engine.socketIOFetcher == nil {
			rt.engine.emitDebugEvent(engineDebugEvent{
				Type: engineDebugEventWebsocketFallback,
				Note: "missing_fetcher",
			})
			rt.engine.logger.Warn("websocket watch requested but no Socket.IO fetcher is available; falling back to polling",
				slog.String("drive_id", rt.engine.driveID.String()),
			)
		}
	}
}
