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

func (e *Engine) hasRootedSubtree() bool {
	return e != nil && e.rootItemID != ""
}

func (e *Engine) rootedSubtreeDeltaSupported() bool {
	return e != nil && e.rootedSubtreeDeltaCapable && e.folderDelta != nil
}

func (flow *engineFlow) observeRootedSubtreeRemote(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) ([]ChangeEvent, string, remoteObservationMode, error) {
	eng := flow.engine
	if !eng.hasRootedSubtree() {
		return nil, "", remoteObservationModeEnumerate, fmt.Errorf("sync: rooted-subtree observation requires a root item ID")
	}

	if eng.preferredRootedSubtreeObservationMode() == remoteObservationModeDelta {
		token := ""
		if !fullReconcile {
			state, err := eng.baseline.ReadObservationState(ctx)
			if err != nil {
				return nil, "", remoteObservationModeDelta, fmt.Errorf("sync: reading observation state: %w", err)
			}

			token = state.Cursor
		}

		items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.rootItemID, token)
		if err != nil && errors.Is(err, graph.ErrGone) && !fullReconcile {
			eng.logger.Warn("rooted-subtree delta token expired, performing full rooted-subtree resync",
				slog.String("drive_id", eng.driveID.String()),
				slog.String("root_item_id", eng.rootItemID),
			)

			items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.rootItemID, "")
			fullReconcile = true
		}
		if err == nil {
			events := convertRootedSubtreeItems(ctx, items, eng.rootItemID, eng.driveID, bl, eng.logger, eng.itemsClient)
			if fullReconcile {
				events = append(events, detectRootedSubtreeOrphans(items, eng.driveID, bl)...)
			}

			return events, newToken, remoteObservationModeDelta, nil
		}

		if eng.recursiveLister == nil || !shouldFallbackRootedSubtreeDelta(err) {
			return nil, "", remoteObservationModeDelta, fmt.Errorf("sync: rooted-subtree delta: %w", err)
		}

		eng.logger.Warn("rooted-subtree delta unsupported or not ready, falling back to recursive listing",
			slog.String("drive_id", eng.driveID.String()),
			slog.String("root_item_id", eng.rootItemID),
			slog.String("error", err.Error()),
		)
	}

	if eng.recursiveLister == nil {
		return nil, "", remoteObservationModeEnumerate, fmt.Errorf("sync: recursive lister not available for rooted subtree %s", eng.rootItemID)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, eng.driveID, eng.rootItemID)
	if err != nil {
		return nil, "", remoteObservationModeEnumerate, fmt.Errorf("sync: rooted-subtree recursive listing: %w", err)
	}

	events := convertRootedSubtreeItems(ctx, items, eng.rootItemID, eng.driveID, bl, eng.logger, eng.itemsClient)
	events = append(events, detectRootedSubtreeOrphans(items, eng.driveID, bl)...)

	return events, "", remoteObservationModeEnumerate, nil
}

func convertRootedSubtreeItems(
	ctx context.Context,
	items []graph.Item,
	rootItemID string,
	remoteDriveID driveid.ID,
	bl *Baseline,
	logger *slog.Logger,
	itemClient ItemClient,
) []ChangeEvent {
	converter := NewPrimaryConverter(bl, remoteDriveID, logger, nil, itemClient)
	converter.RootItemID = rootItemID
	return converter.ConvertItems(ctx, items)
}

func shouldFallbackRootedSubtreeDelta(err error) bool {
	return errors.Is(err, graph.ErrMethodNotAllowed) || errors.Is(err, graph.ErrNotFound)
}

func detectRootedSubtreeOrphans(items []graph.Item, remoteDriveID driveid.ID, bl *Baseline) []ChangeEvent {
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

func (rt *watchRuntime) watchRootedSubtreeRemote(
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
		observationBatch, err := rt.executeRootedSubtreeObservation(ctx, bl, false)
		if err != nil {
			stop, handleErr := rt.handleRootedSubtreePollError(ctx, bo, err)
			if handleErr != nil || stop {
				return handleErr
			}
			continue
		}
		if err := rt.handleRootedSubtreeObservationResult(ctx, batches, interval, bo, &observationBatch); err != nil {
			if rootedSubtreeWatchStopped(ctx, err) {
				return nil
			}
			return err
		}
	}
}

func (rt *watchRuntime) handleRootedSubtreeObservationResult(
	ctx context.Context,
	batches chan<- remoteObservationBatch,
	interval time.Duration,
	bo *retry.Backoff,
	observationBatch *remoteObservationBatch,
) error {
	if shouldSkipRootedSubtreeWatchBatch(observationBatch) {
		bo.Reset()
		stop, sleepErr := rt.sleepRootedSubtreeWatch(ctx, interval, "zero-event")
		if stop || sleepErr != nil {
			return sleepErr
		}
		return nil
	}

	observationBatch.source = remoteObservationBatchRootedSubtree
	observationBatch.applyAck = make(chan error, 1)
	if dispatchErr := rt.dispatchRootedSubtreeBatch(ctx, batches, observationBatch); dispatchErr != nil {
		return dispatchErr
	}

	bo.Reset()
	stop, sleepErr := rt.sleepRootedSubtreeWatch(ctx, interval, "interval")
	if stop || sleepErr != nil {
		return sleepErr
	}

	return nil
}

func shouldSkipRootedSubtreeWatchBatch(batch *remoteObservationBatch) bool {
	if batch == nil {
		return true
	}

	return len(batch.emitted) == 0 &&
		!batch.hasObservationFindings() &&
		batch.deferredProgress() == nil &&
		batch.observationMode != remoteObservationModeEnumerate
}

func (rt *watchRuntime) handleRootedSubtreePollError(
	ctx context.Context,
	bo *retry.Backoff,
	err error,
) (bool, error) {
	if rootedSubtreeWatchStopped(ctx, err) {
		return true, nil
	}

	delay := bo.Next()
	rt.engine.logger.Warn("rooted-subtree watch poll failed, backing off",
		slog.String("error", err.Error()),
		slog.Duration("backoff", delay),
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("root_item_id", rt.engine.rootItemID),
	)

	return rt.sleepRootedSubtreeWatch(ctx, delay, "backoff")
}

func (rt *watchRuntime) dispatchRootedSubtreeBatch(
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
		return fmt.Errorf("dispatch rooted-subtree watch batch: %w", ctx.Err())
	}

	return batch.waitApplied(ctx)
}

func (rt *watchRuntime) sleepRootedSubtreeWatch(
	ctx context.Context,
	delay time.Duration,
	label string,
) (bool, error) {
	sleepErr := TimeSleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}
	if rootedSubtreeWatchStopped(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("rooted-subtree watch %s sleep: %w", label, sleepErr)
}

func rootedSubtreeWatchStopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
