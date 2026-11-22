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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// hasRemoteMountRoot reports whether the run syncs a remote folder rather
// than the drive root.
func (flow *observation) hasRemoteMountRoot() bool {
	return flow.cfg.remoteRootItemID != ""
}

// mountRootObservation is what one mount-root observation saw.
type mountRootObservation struct {
	events []syncdomain.RemoteItem
	token  string
	mode   syncdomain.RemoteObservationMode
	// complete marks a whole-folder enumeration (a delta without a token,
	// after a requested refresh or an expired token).
	complete bool
	// orphansUndecided marks a complete enumeration that kept an orphan it
	// could not confirm.
	orphansUndecided bool
}

func (flow *observation) observeMountRootRemote(
	ctx context.Context,
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (mountRootObservation, error) {
	if !flow.hasRemoteMountRoot() {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate},
			fmt.Errorf("sync: mount-root observation requires a remote root item ID")
	}

	if flow.remote.preferredMountRootObservationMode() == syncdomain.RemoteObservationModeDelta {
		token := ""
		if !fullReconcile {
			state, err := flow.deps.store.ReadObservationState(ctx)
			if err != nil {
				return mountRootObservation{mode: syncdomain.RemoteObservationModeDelta}, fmt.Errorf("sync: reading observation state: %w", err)
			}

			token = state.Cursor
		}

		items, newToken, err := flow.remote.folderDelta.DeltaFolderAll(ctx, flow.cfg.driveID, flow.cfg.remoteRootItemID, token)
		orphanKind := syncdomain.ResyncKindApplyDifferences
		if err != nil && errors.Is(err, graph.ErrGone) && !fullReconcile {
			orphanKind = syncdomain.ResyncKindOf(syncdomain.NewDeltaExpiredError(err))
			// Self-healing: the next call re-enumerates the shared folder from
			// scratch, so nothing needs the user's attention.
			flow.deps.logger.Info("Change history for a shared folder expired; rescanning the folder",
				slog.String("drive_id", flow.cfg.driveID.String()),
				slog.String("remote_root_item_id", flow.cfg.remoteRootItemID),
			)

			items, newToken, err = flow.remote.folderDelta.DeltaFolderAll(ctx, flow.cfg.driveID, flow.cfg.remoteRootItemID, "")
			fullReconcile = true
		}
		if err == nil {
			flow.remote.deltaFallbackReported.Store(false)
			events, undecided := flow.remote.mountRootEvents(ctx, items, bl, fullReconcile, orphanKind)

			return mountRootObservation{
				events: events, token: newToken, mode: syncdomain.RemoteObservationModeDelta,
				complete: fullReconcile, orphansUndecided: undecided,
			}, nil
		}

		if flow.remote.recursiveLister == nil || !shouldFallbackMountRootDelta(err) {
			return mountRootObservation{mode: syncdomain.RemoteObservationModeDelta}, fmt.Errorf("sync: mount-root delta: %w", err)
		}

		// The fallback is retried on every poll, but the condition does not
		// change between polls: report it once, keep the repeats at Debug.
		fallbackAttrs := []any{
			slog.String("drive_id", flow.cfg.driveID.String()),
			slog.String("remote_root_item_id", flow.cfg.remoteRootItemID),
			slog.Any("error", err),
		}
		if flow.remote.deltaFallbackReported.CompareAndSwap(false, true) {
			flow.deps.logger.Info("Shared folder change tracking is unavailable; listing the whole folder on each check",
				fallbackAttrs...)
		} else {
			flow.deps.logger.Debug("mount-root delta unsupported or not ready, falling back to recursive listing",
				fallbackAttrs...)
		}
	}

	if flow.remote.recursiveLister == nil {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate},
			fmt.Errorf("sync: recursive lister not available for mount root %s", flow.cfg.remoteRootItemID)
	}

	items, err := flow.remote.recursiveLister.ListChildrenRecursive(ctx, flow.cfg.driveID, flow.cfg.remoteRootItemID)
	if err != nil {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate}, fmt.Errorf("sync: mount-root recursive listing: %w", err)
	}
	if err := validateMountRootListingParents(items, flow.cfg.remoteRootItemID); err != nil {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate}, err
	}

	// A listing keeps no position: an undecided orphan is judged again by
	// the next listing, every pass, so it needs no refresh pulled forward.
	events, _ := flow.remote.mountRootEvents(ctx, items, bl, true, syncdomain.ResyncKindApplyDifferences)

	return mountRootObservation{events: events, mode: syncdomain.RemoteObservationModeEnumerate, complete: true}, nil
}

func convertMountRootItems(
	ctx context.Context,
	items []graph.Item,
	remoteRootItemID string,
	remoteDriveID driveid.ID,
	bl syncdomain.BaselineView,
	logger *slog.Logger,
	itemClient syncdomain.ItemClient,
) []syncdomain.RemoteItem {
	converter := syncobserve.NewPrimaryConverter(bl, remoteDriveID, logger, nil, itemClient)
	converter.RemoteRootItemID = remoteRootItemID
	return converter.ConvertItems(ctx, items)
}

func shouldFallbackMountRootDelta(err error) bool {
	return errors.Is(err, graph.ErrMethodNotAllowed) || errors.Is(err, graph.ErrNotFound)
}

func detectMountRootOrphans(items []graph.Item, remoteDriveID driveid.ID, bl syncdomain.BaselineView) []syncdomain.RemoteItem {
	seen := make(map[string]struct{}, len(items))
	for i := range items {
		seen[items[i].ID] = struct{}{}
	}

	return syncstore.FindBaselineOrphans(bl, seen, remoteDriveID, "")
}

// validateMountRootListingParents rejects a recursive listing whose items do
// not hang off the listed folder: every item's parent must be the mount root
// or a folder the same listing returned. A page that answered for another
// folder, or a child whose folder the listing skipped, means the listing is
// not a picture of the mount, and absence from it proves nothing (D-009).
func validateMountRootListingParents(items []graph.Item, remoteRootItemID string) error {
	folders := make(map[string]struct{}, len(items)+1)
	folders[remoteRootItemID] = struct{}{}
	for i := range items {
		if items[i].IsFolder || items[i].IsRoot {
			folders[items[i].ID] = struct{}{}
		}
	}
	for i := range items {
		if items[i].ID == remoteRootItemID || items[i].ParentID == "" {
			continue
		}
		if _, ok := folders[items[i].ParentID]; !ok {
			return fmt.Errorf("%w: shared folder listing returned %q under a folder it did not list",
				syncdomain.ErrRemoteObservationIncomplete, items[i].Name)
		}
	}

	return nil
}

func (flow *engineFlow) commitObservedItems(
	ctx context.Context,
	observed []syncdomain.ObservedItem,
	token string,
) error {
	if err := flow.deps.store.CommitObservation(ctx, observed, token, flow.cfg.driveID); err != nil {
		return fmt.Errorf("sync: committing observations: %w", err)
	}

	return nil
}

func (rt *observation) watchMountRootRemote(
	ctx context.Context,
	bl syncdomain.BaselineView,
	batches chan<- syncobserve.RemoteObservationBatch,
	interval time.Duration,
) error {
	if interval < syncdomain.MinPollInterval {
		interval = syncdomain.MinPollInterval
	}

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(interval)
	var outage syncobserve.PollOutageLog

	for {
		// When the poll began, before its first page was asked for: once the
		// batch is applied, the watch loop judges create intents by it, as it
		// does the primary watch's (RemoteObservationBatch.pollStartedAt). A
		// poll that saw no events still hands over a batch: its findings
		// manage the read-denied issue type, to clear it, so
		// shouldSkipMountRootWatchBatch does not skip it, and a create
		// attempt ends here the same way as after the primary watch's polls.
		pollStartedAt := rt.deps.now()
		observationBatch, err := rt.executeMountRootObservation(ctx, bl, false)
		if err != nil {
			stop, handleErr := rt.handleMountRootPollError(ctx, bo, &outage, err)
			if handleErr != nil || stop {
				return handleErr
			}
			continue
		}
		outage.Recovered(ctx, rt.deps.logger, "Checking the shared folder for changes works again")
		observationBatch.PollStartedAt = pollStartedAt
		if err := rt.handleMountRootObservationResult(ctx, batches, interval, bo, &observationBatch); err != nil {
			if syncdomain.RunEnded(ctx, err) {
				return nil
			}
			return err
		}
	}
}

func (rt *observation) handleMountRootObservationResult(
	ctx context.Context,
	batches chan<- syncobserve.RemoteObservationBatch,
	interval time.Duration,
	bo *retry.Backoff,
	observationBatch *syncobserve.RemoteObservationBatch,
) error {
	if shouldSkipMountRootWatchBatch(observationBatch) {
		bo.Reset()
		stop, sleepErr := rt.sleepMountRootWatch(ctx, interval, "zero-event")
		if stop || sleepErr != nil {
			return sleepErr
		}
		return nil
	}

	observationBatch.Source = syncobserve.RemoteObservationBatchMountRoot
	observationBatch.ApplyAck = make(chan error, 1)
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

func shouldSkipMountRootWatchBatch(batch *syncobserve.RemoteObservationBatch) bool {
	if batch == nil {
		return true
	}

	return len(batch.Changes) == 0 &&
		!batch.HasObservationFindings() &&
		batch.DeferredProgress() == nil &&
		batch.ObservationMode != syncdomain.RemoteObservationModeEnumerate
}

func (rt *observation) handleMountRootPollError(
	ctx context.Context,
	bo *retry.Backoff,
	outage *syncobserve.PollOutageLog,
	err error,
) (bool, error) {
	if syncdomain.RunEnded(ctx, err) {
		return true, nil
	}

	delay := bo.Next()
	outage.Failed(rt.deps.logger, "Can't check the shared folder for changes; retrying", err, delay,
		slog.String("drive_id", rt.cfg.driveID.String()),
		slog.String("remote_root_item_id", rt.cfg.remoteRootItemID),
	)

	return rt.sleepMountRootWatch(ctx, delay, "backoff")
}

func (rt *observation) dispatchMountRootBatch(
	ctx context.Context,
	batches chan<- syncobserve.RemoteObservationBatch,
	batch *syncobserve.RemoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	select {
	case batches <- *batch:
	case <-ctx.Done():
		return fmt.Errorf("dispatch mount-root watch batch: %w", ctx.Err())
	}

	return batch.WaitApplied(ctx)
}

func (rt *observation) sleepMountRootWatch(
	ctx context.Context,
	delay time.Duration,
	label string,
) (bool, error) {
	sleepErr := rt.deps.clock.Sleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}
	if syncdomain.RunEnded(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("mount-root watch %s sleep: %w", label, sleepErr)
}
