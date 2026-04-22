package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

func (e *Engine) hasSharedRoot() bool {
	return e != nil && e.rootItemID != ""
}

func (flow *engineFlow) observeSharedRootRemote(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) ([]ChangeEvent, string, error) {
	eng := flow.engine
	if !eng.hasSharedRoot() {
		return nil, "", fmt.Errorf("sync: shared-root observation requires a root item ID")
	}

	if eng.sharedRootObservationMode() == sharedRootObservationDelta {
		token := ""
		if !fullReconcile {
			state, err := eng.baseline.ReadObservationState(ctx)
			if err != nil {
				return nil, "", fmt.Errorf("sync: reading observation state: %w", err)
			}

			token = state.Cursor
		}

		items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.rootItemID, token)
		if err != nil && errors.Is(err, graph.ErrGone) && !fullReconcile {
			eng.logger.Warn("shared-root delta token expired, performing full shared-root resync",
				slog.String("drive_id", eng.driveID.String()),
				slog.String("root_item_id", eng.rootItemID),
			)

			items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.rootItemID, "")
			fullReconcile = true
		}
		if err == nil {
			events := convertSharedRootItems(items, eng.rootItemID, eng.driveID, bl, eng.logger)
			if fullReconcile {
				events = append(events, detectSharedRootOrphans(items, eng.driveID, bl)...)
			}

			return events, newToken, nil
		}

		if eng.recursiveLister == nil || !shouldFallbackSharedRootDelta(err) {
			return nil, "", fmt.Errorf("sync: shared-root delta: %w", err)
		}

		eng.logger.Warn("shared-root delta unsupported or not ready, falling back to recursive listing",
			slog.String("drive_id", eng.driveID.String()),
			slog.String("root_item_id", eng.rootItemID),
			slog.String("error", err.Error()),
		)
	}

	if eng.recursiveLister == nil {
		return nil, "", fmt.Errorf("sync: recursive lister not available for shared root %s", eng.rootItemID)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, eng.driveID, eng.rootItemID)
	if err != nil {
		return nil, "", fmt.Errorf("sync: shared-root recursive listing: %w", err)
	}

	events := convertSharedRootItems(items, eng.rootItemID, eng.driveID, bl, eng.logger)
	events = append(events, detectSharedRootOrphans(items, eng.driveID, bl)...)

	return events, "", nil
}

func convertSharedRootItems(
	items []graph.Item,
	rootItemID string,
	remoteDriveID driveid.ID,
	bl *Baseline,
	logger *slog.Logger,
) []ChangeEvent {
	converter := NewPrimaryConverter(bl, remoteDriveID, logger, nil)
	converter.RootItemID = rootItemID
	return converter.ConvertItems(items)
}

func shouldFallbackSharedRootDelta(err error) bool {
	return errors.Is(err, graph.ErrMethodNotAllowed) || errors.Is(err, graph.ErrNotFound)
}

func detectSharedRootOrphans(items []graph.Item, remoteDriveID driveid.ID, bl *Baseline) []ChangeEvent {
	seen := make(map[string]struct{}, len(items))
	for i := range items {
		seen[items[i].ID] = struct{}{}
	}

	return findBaselineOrphans(bl, seen, remoteDriveID, "")
}

func (flow *engineFlow) commitObservedItems(
	ctx context.Context,
	observed []ObservedItem,
	token string,
) error {
	eng := flow.engine

	if err := eng.baseline.CommitObservation(ctx, observed, token, eng.driveID); err != nil {
		return fmt.Errorf("sync: committing observations: %w", err)
	}

	return nil
}

func (rt *watchRuntime) watchSharedRootRemote(
	ctx context.Context,
	bl *Baseline,
	batches chan<- remoteObservationBatch,
	interval time.Duration,
) error {
	if interval < MinPollInterval {
		interval = MinPollInterval
	}

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(interval)

	for {
		result, err := rt.executeSharedRootObservation(ctx, bl, false)
		if err != nil {
			stop, handleErr := rt.handleSharedRootPollError(ctx, bo, err)
			if handleErr != nil || stop {
				return handleErr
			}
			continue
		}

		if len(result.events) == 0 && !(&result).hasObservationFindings() && result.pending == nil {
			bo.Reset()
			stop, sleepErr := rt.sleepSharedRootWatch(ctx, interval, "zero-event")
			if sleepErr != nil || stop {
				return sleepErr
			}
			continue
		}

		batch := buildSharedRootWatchBatch(rt.engine, &result)
		if dispatchErr := rt.dispatchSharedRootBatch(ctx, batches, &batch); dispatchErr != nil {
			if sharedRootWatchStopped(ctx, dispatchErr) {
				return nil
			}
			return dispatchErr
		}
		bo.Reset()
		if stop, sleepErr := rt.sleepSharedRootWatch(ctx, interval, "interval"); sleepErr != nil || stop {
			return sleepErr
		}
	}
}

func (rt *watchRuntime) handleSharedRootPollError(
	ctx context.Context,
	bo *retry.Backoff,
	err error,
) (bool, error) {
	if sharedRootWatchStopped(ctx, err) {
		return true, nil
	}

	delay := bo.Next()
	rt.engine.logger.Warn("shared-root watch poll failed, backing off",
		slog.String("error", err.Error()),
		slog.Duration("backoff", delay),
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("root_item_id", rt.engine.rootItemID),
	)

	return rt.sleepSharedRootWatch(ctx, delay, "backoff")
}

func (rt *watchRuntime) dispatchSharedRootBatch(
	ctx context.Context,
	batches chan<- remoteObservationBatch,
	batch *remoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	select {
	case batches <- *batch:
	case <-ctx.Done():
		return fmt.Errorf("dispatch shared-root watch batch: %w", ctx.Err())
	}

	return batch.waitApplied(ctx)
}

func (rt *watchRuntime) sleepSharedRootWatch(
	ctx context.Context,
	delay time.Duration,
	label string,
) (bool, error) {
	sleepErr := TimeSleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}
	if sharedRootWatchStopped(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("shared-root watch %s sleep: %w", label, sleepErr)
}

func sharedRootWatchStopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
