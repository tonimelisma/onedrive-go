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
	if rt.engine.hasRemoteMountRoot() {
		rt.warnMountRootWebsocketFallback()
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			defer close(batches)
			errs <- rt.watchMountRootRemote(ctx, bl, batches, pollInterval)
		}()
		return
	}

	remoteObs := NewRemoteObserver(rt.engine.fetcher, bl, rt.engine.driveID, rt.engine.logger)
	remoteObs.SetItemClient(rt.engine.itemsClient)
	remoteObs.SetShortcutTopology(rt.engine.shortcutTopologyNamespaceID, rt.engine.localFilter.ManagedRoots)
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
			func(ctx context.Context, polledEvents []ChangeEvent, newToken string, topology ShortcutTopologyBatch) (remoteObservationBatch, error) {
				_ = ctx
				_ = bl
				batch := buildPrimaryWatchBatch(rt.engine, polledEvents, newToken)
				batch.shortcutTopology = topology
				if topology.ShouldApply() && batch.cursorToken == "" {
					batch.cursorToken = newToken
				}
				return batch, nil
			},
		)
	}()
}

func (rt *watchRuntime) warnMountRootWebsocketFallback() {
	if !rt.engine.enableWebsocket {
		return
	}

	rt.engine.emitDebugEvent(engineDebugEvent{
		Type: engineDebugEventWebsocketFallback,
		Note: "mount_root",
	})
	rt.engine.logger.Warn("websocket watch is not supported for mount-root engines; falling back to polling",
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("remote_root_item_id", rt.engine.remoteRootItemID),
	)
}
