package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

func (rt *watchRuntime) startPrimaryRootWatch(
	ctx context.Context,
	bl syncdomain.BaselineView,
	batches chan<- syncobserve.RemoteObservationBatch,
	errs chan<- error,
	pollInterval time.Duration,
) {
	// The observer goroutines hold the observation and their send-only
	// outputs only: they cannot reach loop state.
	obs := &rt.observation
	if rt.hasRemoteMountRoot() {
		rt.warnMountRootWebsocketFallback()
		rt.resources.goroutines.start(ctx, watchGoroutineRemoteObserver, func(ctx context.Context) {
			defer obs.deps.emit(syncdomain.EngineDebugEvent{
				Type: syncdomain.EngineDebugEventObserverExited, Observer: syncdomain.EngineDebugObserverRemote,
			})
			defer close(batches)
			defer recoverObserverPanic(obs.deps.logger, observerNameRemote, errs)

			errs <- observerExitError(observerNameRemote, obs.watchMountRootRemote(ctx, bl, batches, pollInterval))
		})
		return
	}

	remoteObs := rt.engine.buildRemoteObserver(bl, rt.deps.logger)
	remoteObs.SetItemClient(rt.engine.itemsClient)
	remoteObs.SetRemotePathLookup(rt.engine.remotePathLookup(ctx))
	remoteObs.SetShortcutTopology(rt.cfg.shortcutNamespaceID, rt.engine.protectedRoots.current())
	remoteObs.SetDeltaExpiredRecovery(func(ctx context.Context, kind syncdomain.ResyncKind) (syncobserve.RemoteObservationBatch, error) {
		batch, err := obs.executeDriveRootFullObservation(ctx, bl, kind)
		if err != nil {
			return syncobserve.RemoteObservationBatch{}, err
		}
		batch.Source = syncobserve.RemoteObservationBatchPrimaryWatch
		batch.ArmFullRefreshTimer = true
		batch.ApplyAck = make(chan error, 1)

		return batch, nil
	})
	remoteObs.SetDebugEvents(rt.deps.emit)
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

	rt.resources.goroutines.start(ctx, watchGoroutineRemoteObserver, func(ctx context.Context) {
		defer obs.deps.emit(syncdomain.EngineDebugEvent{
			Type: syncdomain.EngineDebugEventObserverExited, Observer: syncdomain.EngineDebugObserverRemote,
		})
		defer close(batches)
		defer recoverObserverPanic(obs.deps.logger, observerNameRemote, errs)

		watchErr := remoteObs.Watch(
			ctx,
			savedToken,
			batches,
			pollInterval,
			wakeCh,
			func(
				_ context.Context, polledEvents []syncdomain.RemoteItem, newToken string, topology syncshortcut.ShortcutTopologyBatch,
			) (syncobserve.RemoteObservationBatch, error) {
				batch := buildPrimaryWatchBatch(obs.engine, bl, polledEvents, newToken)
				batch.ShortcutTopology = topology
				if topology.ShouldApply() && batch.CursorToken == "" {
					batch.CursorToken = newToken
				}
				return batch, nil
			},
		)
		errs <- observerExitError(observerNameRemote, watchErr)
	})
}

func (rt *watchRuntime) warnMountRootWebsocketFallback() {
	if !rt.cfg.enableWebsocket {
		return
	}

	rt.deps.emit(syncdomain.EngineDebugEvent{
		Type: syncdomain.EngineDebugEventWebsocketFallback,
		Note: "mount_root",
	})
	// Debug: a by-design limitation with nothing for the user to do; it
	// fired at every watch start for every shared folder.
	rt.deps.logger.Debug("websocket watch is not supported for mount-root engines; falling back to polling",
		slog.String("drive_id", rt.cfg.driveID.String()),
		slog.String("remote_root_item_id", rt.cfg.remoteRootItemID),
	)
}
