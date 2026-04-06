package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (e *Engine) hasScopedRoot() bool {
	return e != nil && e.rootItemID != ""
}

func (e *Engine) scopedRootShortcut() *synctypes.Shortcut {
	if !e.hasScopedRoot() {
		return nil
	}

	return &synctypes.Shortcut{
		ItemID:      e.rootItemID,
		RemoteDrive: e.driveID.String(),
		RemoteItem:  e.rootItemID,
		LocalPath:   "",
	}
}

func (flow *engineFlow) observeScopedRemote(
	ctx context.Context,
	bl *synctypes.Baseline,
	fullReconcile bool,
) ([]synctypes.ChangeEvent, string, error) {
	eng := flow.engine
	sc := eng.scopedRootShortcut()
	if sc == nil {
		return nil, "", fmt.Errorf("sync: scoped remote observation requires a root item ID")
	}

	if eng.folderDelta != nil {
		token := ""
		if !fullReconcile {
			savedToken, err := eng.baseline.GetDeltaToken(ctx, eng.driveID.String(), eng.rootItemID)
			if err != nil {
				return nil, "", fmt.Errorf("sync: getting scoped delta token: %w", err)
			}

			token = savedToken
		}

		items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.rootItemID, token)
		if err != nil && errors.Is(err, graph.ErrGone) && !fullReconcile {
			eng.logger.Warn("scoped delta token expired, performing full scoped resync",
				slog.String("drive_id", eng.driveID.String()),
				slog.String("root_item_id", eng.rootItemID),
			)

			items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.rootItemID, "")
			fullReconcile = true
		}
		if err == nil {
			events := syncobserve.ConvertShortcutItems(items, sc, eng.driveID, bl, eng.logger)
			if fullReconcile {
				events = append(events, syncobserve.DetectShortcutOrphans(sc, eng.driveID, items, bl)...)
			}

			return events, newToken, nil
		}

		if eng.recursiveLister == nil {
			return nil, "", fmt.Errorf("sync: scoped folder delta: %w", err)
		}

		eng.logger.Warn("scoped folder delta unavailable, falling back to recursive listing",
			slog.String("drive_id", eng.driveID.String()),
			slog.String("root_item_id", eng.rootItemID),
			slog.String("error", err.Error()),
		)
	}

	if eng.recursiveLister == nil {
		return nil, "", fmt.Errorf("sync: recursive lister not available for scoped root %s", eng.rootItemID)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, eng.driveID, eng.rootItemID)
	if err != nil {
		return nil, "", fmt.Errorf("sync: scoped recursive listing: %w", err)
	}

	events := syncobserve.ConvertShortcutItems(items, sc, eng.driveID, bl, eng.logger)
	events = append(events, syncobserve.DetectShortcutOrphans(sc, eng.driveID, items, bl)...)

	return events, "", nil
}

func (flow *engineFlow) commitObservedRemote(
	ctx context.Context,
	events []synctypes.ChangeEvent,
	token string,
) error {
	return flow.commitObservedItems(ctx, changeEventsToObservedItems(flow.engine.logger, events), token)
}

func (flow *engineFlow) commitObservedItems(
	ctx context.Context,
	observed []synctypes.ObservedItem,
	token string,
) error {
	eng := flow.engine

	if eng.hasScopedRoot() {
		if err := eng.baseline.CommitObservationForScope(ctx, observed, token, eng.driveID, eng.rootItemID); err != nil {
			return fmt.Errorf("sync: committing scoped observations: %w", err)
		}

		return nil
	}

	if err := eng.baseline.CommitObservation(ctx, observed, token, eng.driveID); err != nil {
		return fmt.Errorf("sync: committing observations: %w", err)
	}

	return nil
}

func (rt *watchRuntime) watchScopedRootRemote(
	ctx context.Context,
	bl *synctypes.Baseline,
	events chan<- synctypes.ChangeEvent,
	interval time.Duration,
) error {
	if interval < syncobserve.MinPollInterval {
		interval = syncobserve.MinPollInterval
	}

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(interval)

	for {
		polledEvents, newToken, err := rt.observeScopedRemote(ctx, bl, false)
		if err != nil {
			stop, handleErr := rt.handleScopedRootPollError(ctx, bo, err)
			if handleErr != nil || stop {
				return handleErr
			}
			continue
		}

		if len(polledEvents) == 0 {
			bo.Reset()
			stop, sleepErr := rt.sleepScopedRootWatch(ctx, interval, "zero-event")
			if sleepErr != nil || stop {
				return sleepErr
			}
			continue
		}

		scoped := applyRemoteScope(rt.engine.logger, rt.currentScopeSnapshot(), rt.currentScopeGeneration(), polledEvents)

		if !rt.commitScopedRootWatchEvents(ctx, scoped.observed, newToken) {
			continue
		}

		if stop := rt.dispatchScopedRootEvents(ctx, events, scoped.emitted); stop {
			return nil
		}
		bo.Reset()
		if stop, sleepErr := rt.sleepScopedRootWatch(ctx, interval, "interval"); sleepErr != nil || stop {
			return sleepErr
		}
	}
}

func (rt *watchRuntime) handleScopedRootPollError(
	ctx context.Context,
	bo *retry.Backoff,
	err error,
) (bool, error) {
	if scopedRootWatchStopped(ctx, err) {
		return true, nil
	}

	delay := bo.Next()
	rt.engine.logger.Warn("scoped remote watch poll failed, backing off",
		slog.String("error", err.Error()),
		slog.Duration("backoff", delay),
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("root_item_id", rt.engine.rootItemID),
	)

	return rt.sleepScopedRootWatch(ctx, delay, "backoff")
}

func (rt *watchRuntime) commitScopedRootWatchEvents(
	ctx context.Context,
	observed []synctypes.ObservedItem,
	newToken string,
) bool {
	if err := rt.commitObservedItems(ctx, observed, newToken); err != nil {
		rt.engine.logger.Error("failed to commit scoped observations in watch",
			slog.String("error", err.Error()),
			slog.Int("events", len(observed)),
		)
		return false
	}

	return true
}

func (rt *watchRuntime) dispatchScopedRootEvents(
	ctx context.Context,
	events chan<- synctypes.ChangeEvent,
	polledEvents []synctypes.ChangeEvent,
) bool {
	for i := range polledEvents {
		select {
		case events <- polledEvents[i]:
		case <-ctx.Done():
			return true
		}
	}

	return false
}

func (rt *watchRuntime) sleepScopedRootWatch(
	ctx context.Context,
	delay time.Duration,
	label string,
) (bool, error) {
	sleepErr := syncobserve.TimeSleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}
	if scopedRootWatchStopped(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("scoped remote watch %s sleep: %w", label, sleepErr)
}

func scopedRootWatchStopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
