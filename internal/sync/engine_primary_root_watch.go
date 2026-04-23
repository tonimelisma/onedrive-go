package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"time"
)

func (rt *watchRuntime) startPrimaryRootWatch(
	ctx context.Context,
	obsWg *stdsync.WaitGroup,
	bl *Baseline,
	batches chan<- remoteObservationBatch,
	errs chan<- error,
	pollInterval time.Duration,
) {
	if rt.engine.hasRootedSubtree() {
		rt.warnRootedSubtreeWebsocketFallback()
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			defer close(batches)
			errs <- rt.watchRootedSubtreeRemote(ctx, bl, batches, pollInterval)
		}()
		return
	}

	remoteObs := NewRemoteObserver(rt.engine.fetcher, bl, rt.engine.driveID, rt.engine.logger)
	remoteObs.SetItemClient(rt.engine.itemsClient)
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
		defer close(batches)
		errs <- remoteObs.Watch(
			ctx,
			savedToken,
			batches,
			pollInterval,
			wakeCh,
			func(ctx context.Context, polledEvents []ChangeEvent, newToken string) (remoteObservationBatch, error) {
				_ = ctx
				_ = bl
				return buildPrimaryWatchBatch(rt.engine, polledEvents, newToken), nil
			},
		)
	}()
}

func (rt *watchRuntime) warnRootedSubtreeWebsocketFallback() {
	if !rt.engine.enableWebsocket {
		return
	}

	rt.engine.emitDebugEvent(engineDebugEvent{
		Type: engineDebugEventWebsocketFallback,
		Note: "rooted_subtree",
	})
	rt.engine.logger.Warn("websocket watch is not supported for rooted-subtree engines; falling back to polling",
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("root_item_id", rt.engine.rootItemID),
	)
}
