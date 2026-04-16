package sync

import (
	"context"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"
)

func (rt *watchRuntime) startPrimaryRootWatch(
	ctx context.Context,
	obsWg *stdsync.WaitGroup,
	bl *Baseline,
	events chan<- ChangeEvent,
	errs chan<- error,
	pollInterval time.Duration,
	plan primaryRootObservationPlan,
) {
	switch plan.kind {
	case primaryRootObservationSharedRoot:
		rt.warnSharedRootWebsocketFallback()
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- rt.watchSharedRootRemote(ctx, bl, events, pollInterval)
		}()
	case primaryRootObservationDriveRoot:
		remoteObs := NewRemoteObserver(rt.engine.fetcher, bl, rt.engine.driveID, rt.engine.logger)
		rt.remoteObs = remoteObs
		wakeCh := rt.startSocketIOWakeSource(ctx)

		state, tokenErr := rt.engine.baseline.ReadObservationState(ctx)
		if tokenErr != nil {
			rt.engine.logger.Warn("failed to get delta token for watch",
				slog.String("error", tokenErr.Error()),
			)
		}
		savedToken := ""
		if state != nil {
			savedToken = state.Cursor
		}

		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- remoteObs.Watch(
				ctx,
				savedToken,
				events,
				pollInterval,
				wakeCh,
				func(ctx context.Context, polledEvents []ChangeEvent, newToken string) ([]ChangeEvent, error) {
					return rt.processCommittedPrimaryWatchBatch(ctx, bl, polledEvents, newToken)
				},
			)
		}()
	default:
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- fmt.Errorf("start primary root watch: unknown plan kind %q", plan.kind)
		}()
	}
}

func (rt *watchRuntime) warnSharedRootWebsocketFallback() {
	if !rt.engine.enableWebsocket {
		return
	}

	rt.engine.emitDebugEvent(engineDebugEvent{
		Type: engineDebugEventWebsocketFallback,
		Note: "shared_root",
	})
	rt.engine.logger.Warn("websocket watch is not supported for shared-root drives; falling back to polling",
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("root_item_id", rt.engine.rootItemID),
	)
}
