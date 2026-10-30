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
			defer rt.deps.emit(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			defer close(batches)
			defer recoverObserverPanic(rt.deps.logger, observerNameRemote, errs)

			errs <- observerExitError(observerNameRemote, rt.watchMountRootRemote(ctx, bl, batches, pollInterval))
		}()
		return
	}

	remoteObs := newRemoteObserver(rt.engine.fetcher, bl, rt.engine.driveID, rt.deps.logger)
	remoteObs.SetItemClient(rt.engine.itemsClient)
	remoteObs.SetShortcutTopology(rt.engine.shortcutNamespaceID, rt.engine.protectedRoots)
	remoteObs.SetDeltaExpiredRecovery(func(ctx context.Context, kind resyncKind) (remoteObservationBatch, error) {
		batch, err := rt.executeDriveRootFullObservation(ctx, bl, kind)
		if err != nil {
			return remoteObservationBatch{}, err
		}
		batch.source = remoteObservationBatchPrimaryWatch
		batch.armFullRefreshTimer = true
		batch.applyAck = make(chan error, 1)

		return batch, nil
	})
	rt.resources.remoteObs = remoteObs
	wakeCh := rt.startSocketIOWakeSource(ctx)

	state, tokenErr := rt.deps.store.ReadObservationState(ctx)
	if tokenErr != nil {
		rt.deps.logger.Info("Could not read the saved OneDrive sync position; rescanning OneDrive",
			slog.Any("error", tokenErr),
		)
	}
	savedToken := ""
	if state != nil {
		savedToken = state.Cursor
	}

	go func() {
		defer obsWg.Done()
		defer rt.deps.emit(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
		defer close(batches)
		defer recoverObserverPanic(rt.deps.logger, observerNameRemote, errs)

		watchErr := remoteObs.Watch(
			ctx,
			savedToken,
			batches,
			pollInterval,
			wakeCh,
			func(ctx context.Context, polledEvents []changeEvent, newToken string, topology shortcutTopologyBatch) (remoteObservationBatch, error) {
				_ = ctx
				batch := buildPrimaryWatchBatch(rt.engine, bl, polledEvents, newToken)
				batch.shortcutTopology = topology
				if topology.shouldApply() && batch.cursorToken == "" {
					batch.cursorToken = newToken
				}
				return batch, nil
			},
		)
		errs <- observerExitError(observerNameRemote, watchErr)
	}()
}

func (rt *watchRuntime) warnMountRootWebsocketFallback() {
	if !rt.engine.enableWebsocket {
		return
	}

	rt.deps.emit(engineDebugEvent{
		Type: engineDebugEventWebsocketFallback,
		Note: "mount_root",
	})
	// Debug: a by-design limitation with nothing for the user to do; it
	// fired at every watch start for every shared folder.
	rt.deps.logger.Debug("websocket watch is not supported for mount-root engines; falling back to polling",
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("remote_root_item_id", rt.engine.remoteRootItemID),
	)
}
