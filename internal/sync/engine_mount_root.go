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

func (e *Engine) hasRemoteMountRoot() bool {
	return e != nil && e.remoteRootItemID != ""
}

func (e *Engine) mountRootDeltaSupported() bool {
	return e != nil && e.remoteRootDeltaCapable && e.folderDelta != nil
}

func (flow *engineFlow) observeMountRootRemote(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) ([]ChangeEvent, string, remoteObservationMode, error) {
	eng := flow.engine
	if !eng.hasRemoteMountRoot() {
		return nil, "", remoteObservationModeEnumerate, fmt.Errorf("sync: mount-root observation requires a remote root item ID")
	}

	if eng.preferredMountRootObservationMode() == remoteObservationModeDelta {
		token := ""
		if !fullReconcile {
			state, err := eng.baseline.ReadObservationState(ctx)
			if err != nil {
				return nil, "", remoteObservationModeDelta, fmt.Errorf("sync: reading observation state: %w", err)
			}

			token = state.Cursor
		}

		items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.remoteRootItemID, token)
		if err != nil && errors.Is(err, graph.ErrGone) && !fullReconcile {
			eng.logger.Warn("mount-root delta token expired, performing full mount-root resync",
				slog.String("drive_id", eng.driveID.String()),
				slog.String("remote_root_item_id", eng.remoteRootItemID),
			)

			items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.remoteRootItemID, "")
			fullReconcile = true
		}
		if err == nil {
			events := convertMountRootItems(ctx, items, eng.remoteRootItemID, eng.driveID, bl, eng.logger, eng.itemsClient)
			if fullReconcile {
				events = append(events, detectMountRootOrphans(items, eng.driveID, bl)...)
			}

			return events, newToken, remoteObservationModeDelta, nil
		}

		if eng.recursiveLister == nil || !shouldFallbackMountRootDelta(err) {
			return nil, "", remoteObservationModeDelta, fmt.Errorf("sync: mount-root delta: %w", err)
		}

		eng.logger.Warn("mount-root delta unsupported or not ready, falling back to recursive listing",
			slog.String("drive_id", eng.driveID.String()),
			slog.String("remote_root_item_id", eng.remoteRootItemID),
			slog.String("error", err.Error()),
		)
	}

	if eng.recursiveLister == nil {
		return nil, "", remoteObservationModeEnumerate, fmt.Errorf("sync: recursive lister not available for mount root %s", eng.remoteRootItemID)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, eng.driveID, eng.remoteRootItemID)
	if err != nil {
		return nil, "", remoteObservationModeEnumerate, fmt.Errorf("sync: mount-root recursive listing: %w", err)
	}

	events := convertMountRootItems(ctx, items, eng.remoteRootItemID, eng.driveID, bl, eng.logger, eng.itemsClient)
	events = append(events, detectMountRootOrphans(items, eng.driveID, bl)...)

	return events, "", remoteObservationModeEnumerate, nil
}

func convertMountRootItems(
	ctx context.Context,
	items []graph.Item,
	remoteRootItemID string,
	remoteDriveID driveid.ID,
	bl *Baseline,
	logger *slog.Logger,
	itemClient ItemClient,
) []ChangeEvent {
	converter := NewPrimaryConverter(bl, remoteDriveID, logger, nil, itemClient)
	converter.RemoteRootItemID = remoteRootItemID
	return converter.ConvertItems(ctx, items)
}

func shouldFallbackMountRootDelta(err error) bool {
	return errors.Is(err, graph.ErrMethodNotAllowed) || errors.Is(err, graph.ErrNotFound)
}

func detectMountRootOrphans(items []graph.Item, remoteDriveID driveid.ID, bl *Baseline) []ChangeEvent {
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

func (rt *watchRuntime) watchMountRootRemote(
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
		observationBatch, err := rt.executeMountRootObservation(ctx, bl, false)
		if err != nil {
			stop, handleErr := rt.handleMountRootPollError(ctx, bo, err)
			if handleErr != nil || stop {
				return handleErr
			}
			continue
		}
		if err := rt.handleMountRootObservationResult(ctx, batches, interval, bo, &observationBatch); err != nil {
			if mountRootWatchStopped(ctx, err) {
				return nil
			}
			return err
		}
	}
}

func (rt *watchRuntime) handleMountRootObservationResult(
	ctx context.Context,
	batches chan<- remoteObservationBatch,
	interval time.Duration,
	bo *retry.Backoff,
	observationBatch *remoteObservationBatch,
) error {
	if shouldSkipMountRootWatchBatch(observationBatch) {
		bo.Reset()
		stop, sleepErr := rt.sleepMountRootWatch(ctx, interval, "zero-event")
		if stop || sleepErr != nil {
			return sleepErr
		}
		return nil
	}

	observationBatch.source = remoteObservationBatchMountRoot
	observationBatch.applyAck = make(chan error, 1)
	if dispatchErr := rt.dispatchMountRootBatch(ctx, batches, observationBatch); dispatchErr != nil {
		return dispatchErr
	}

	bo.Reset()
	stop, sleepErr := rt.sleepMountRootWatch(ctx, interval, "interval")
	if stop || sleepErr != nil {
		return sleepErr
	}

	return nil
}

func shouldSkipMountRootWatchBatch(batch *remoteObservationBatch) bool {
	if batch == nil {
		return true
	}

	return len(batch.emitted) == 0 &&
		!batch.hasObservationFindings() &&
		batch.deferredProgress() == nil &&
		batch.observationMode != remoteObservationModeEnumerate
}

func (rt *watchRuntime) handleMountRootPollError(
	ctx context.Context,
	bo *retry.Backoff,
	err error,
) (bool, error) {
	if mountRootWatchStopped(ctx, err) {
		return true, nil
	}

	delay := bo.Next()
	rt.engine.logger.Warn("mount-root watch poll failed, backing off",
		slog.String("error", err.Error()),
		slog.Duration("backoff", delay),
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("remote_root_item_id", rt.engine.remoteRootItemID),
	)

	return rt.sleepMountRootWatch(ctx, delay, "backoff")
}

func (rt *watchRuntime) dispatchMountRootBatch(
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
		return fmt.Errorf("dispatch mount-root watch batch: %w", ctx.Err())
	}

	return batch.waitApplied(ctx)
}

func (rt *watchRuntime) sleepMountRootWatch(
	ctx context.Context,
	delay time.Duration,
	label string,
) (bool, error) {
	sleepErr := TimeSleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}
	if mountRootWatchStopped(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("mount-root watch %s sleep: %w", label, sleepErr)
}

func mountRootWatchStopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
