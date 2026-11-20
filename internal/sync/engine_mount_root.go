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

func (e *Engine) hasRemoteMountRoot() bool {
	return e != nil && e.remoteRootItemID != ""
}

func (e *Engine) mountRootDeltaSupported() bool {
	return e != nil && e.remoteRootDeltaCapable && e.folderDelta != nil
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

func (flow *engineFlow) observeMountRootRemote(
	ctx context.Context,
	bl *syncdomain.Baseline,
	fullReconcile bool,
) (mountRootObservation, error) {
	eng := flow.engine
	if !eng.hasRemoteMountRoot() {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate},
			fmt.Errorf("sync: mount-root observation requires a remote root item ID")
	}

	if eng.preferredMountRootObservationMode() == syncdomain.RemoteObservationModeDelta {
		token := ""
		if !fullReconcile {
			state, err := eng.baseline.ReadObservationState(ctx)
			if err != nil {
				return mountRootObservation{mode: syncdomain.RemoteObservationModeDelta}, fmt.Errorf("sync: reading observation state: %w", err)
			}

			token = state.Cursor
		}

		items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.remoteRootItemID, token)
		orphanKind := syncdomain.ResyncKindApplyDifferences
		if err != nil && errors.Is(err, graph.ErrGone) && !fullReconcile {
			orphanKind = syncdomain.ResyncKindOf(syncdomain.NewDeltaExpiredError(err))
			// Self-healing: the next call re-enumerates the shared folder from
			// scratch, so nothing needs the user's attention.
			eng.logger.Info("Change history for a shared folder expired; rescanning the folder",
				slog.String("drive_id", eng.driveID.String()),
				slog.String("remote_root_item_id", eng.remoteRootItemID),
			)

			items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, eng.remoteRootItemID, "")
			fullReconcile = true
		}
		if err == nil {
			eng.mountRootDeltaFallbackReported.Store(false)
			events, undecided := eng.mountRootEvents(ctx, items, bl, fullReconcile, orphanKind)

			return mountRootObservation{
				events: events, token: newToken, mode: syncdomain.RemoteObservationModeDelta,
				complete: fullReconcile, orphansUndecided: undecided,
			}, nil
		}

		if eng.recursiveLister == nil || !shouldFallbackMountRootDelta(err) {
			return mountRootObservation{mode: syncdomain.RemoteObservationModeDelta}, fmt.Errorf("sync: mount-root delta: %w", err)
		}

		// The fallback is retried on every poll, but the condition does not
		// change between polls: report it once, keep the repeats at Debug.
		fallbackAttrs := []any{
			slog.String("drive_id", eng.driveID.String()),
			slog.String("remote_root_item_id", eng.remoteRootItemID),
			slog.Any("error", err),
		}
		if eng.mountRootDeltaFallbackReported.CompareAndSwap(false, true) {
			eng.logger.Info("Shared folder change tracking is unavailable; listing the whole folder on each check",
				fallbackAttrs...)
		} else {
			eng.logger.Debug("mount-root delta unsupported or not ready, falling back to recursive listing",
				fallbackAttrs...)
		}
	}

	if eng.recursiveLister == nil {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate},
			fmt.Errorf("sync: recursive lister not available for mount root %s", eng.remoteRootItemID)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, eng.driveID, eng.remoteRootItemID)
	if err != nil {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate}, fmt.Errorf("sync: mount-root recursive listing: %w", err)
	}
	if err := validateMountRootListingParents(items, eng.remoteRootItemID); err != nil {
		return mountRootObservation{mode: syncdomain.RemoteObservationModeEnumerate}, err
	}

	// A listing keeps no position: an undecided orphan is judged again by
	// the next listing, every pass, so it needs no refresh pulled forward.
	events, _ := eng.mountRootEvents(ctx, items, bl, true, syncdomain.ResyncKindApplyDifferences)

	return mountRootObservation{events: events, mode: syncdomain.RemoteObservationModeEnumerate, complete: true}, nil
}

// mountRootEvents converts a mount-root enumeration, adding confirmed orphan
// deletes when the enumeration was complete. kind is the resync that forced
// the enumeration, if any: unless it applies differences, confirmed orphans
// are re-uploads, not deletes (R-2.15.3).
func (e *Engine) mountRootEvents(
	ctx context.Context, items []graph.Item, bl *syncdomain.Baseline, complete bool, kind syncdomain.ResyncKind,
) ([]syncdomain.RemoteItem, bool) {
	events := convertMountRootItems(ctx, items, e.remoteRootItemID, e.driveID, bl, e.logger, e.itemsClient)
	if !complete {
		return events, false
	}
	orphans, undecided := e.confirmMountRootOrphans(ctx, items, detectMountRootOrphans(items, e.driveID, bl))
	if !kind.OrphansAreDeletes() {
		markOrphansForReupload(orphans)
	}

	return append(events, orphans...), undecided
}

func convertMountRootItems(
	ctx context.Context,
	items []graph.Item,
	remoteRootItemID string,
	remoteDriveID driveid.ID,
	bl *syncdomain.Baseline,
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

func detectMountRootOrphans(items []graph.Item, remoteDriveID driveid.ID, bl *syncdomain.Baseline) []syncdomain.RemoteItem {
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

// confirmMountRootOrphans keeps only the synthesized deletes a live lookup
// confirms. One enumeration of a shared folder (/children pages, or a folder
// delta) is not proof of absence: Graph omits items transiently, and a delete
// synthesized from that omission removes the local copy, which the next
// listing then downloads again (D-009). An item is gone when GetItem answers
// 404 or a deleted facet, or when its ancestors provably lead outside the
// mount; an item still inside, one whose parent cannot be resolved, or a
// lookup that failed is left for the next listing.
func (e *Engine) confirmMountRootOrphans(
	ctx context.Context, listed []graph.Item, orphans []syncdomain.RemoteItem,
) (confirmed []syncdomain.RemoteItem, undecided bool) {
	inside := make(map[string]struct{}, len(listed)+1)
	inside[e.remoteRootItemID] = struct{}{}
	for i := range listed {
		inside[listed[i].ID] = struct{}{}
	}

	return e.confirmEnumerationOrphans(ctx, orphans, inside)
}

// confirmEnumerationOrphans looks up each delete a complete enumeration
// synthesized and keeps only those confirmed gone. inside is nil for a
// drive-root enumeration, where any live item is still in scope; for a mount
// root it holds the mount root and every item the listing returned. An
// unconfirmed orphan produces no event, so its local copy stays and the next
// complete enumeration judges it again. undecided reports an orphan whose
// lookup failed: unlike one found live, it still needs that judgement, and
// soon (observeRemoteFullWithShortcutTopology).
func (e *Engine) confirmEnumerationOrphans(
	ctx context.Context,
	orphans []syncdomain.RemoteItem,
	inside map[string]struct{},
) (confirmed []syncdomain.RemoteItem, undecided bool) {
	if len(orphans) == 0 || e.itemsClient == nil {
		return orphans, false
	}

	confirmed = make([]syncdomain.RemoteItem, 0, len(orphans))
	for i := range orphans {
		orphan := orphans[i]
		live, err := e.itemsClient.GetItem(ctx, e.driveID, orphan.ItemID)
		switch {
		case errors.Is(err, graph.ErrNotFound):
			confirmed = append(confirmed, orphan)
		case err != nil:
			undecided = true
			e.logger.Debug("could not confirm an item missing from a listing is gone; keeping it",
				slog.String("path", orphan.Path), slog.Any("error", err))
		case live == nil:
			continue
		case live.IsDeleted:
			confirmed = append(confirmed, orphan)
		case inside == nil:
			e.logger.Debug("a full rescan omitted a live item; not deleting it",
				slog.String("path", orphan.Path), slog.String("item_id", orphan.ItemID))
		default:
			if e.liveItemLeftMount(ctx, live, inside) {
				confirmed = append(confirmed, orphan)
				continue
			}
			e.logger.Debug("shared-folder listing omitted a live item; not deleting it",
				slog.String("path", orphan.Path), slog.String("item_id", orphan.ItemID))
		}
	}

	return confirmed, undecided
}

// mountAncestorLookupLimit bounds the parent walk that proves an item left
// a mount.
const mountAncestorLookupLimit = 64

// liveItemLeftMount reports whether a live item's ancestors provably lead
// outside the mount: the parent walk reaches a drive root without passing
// through the mount. Anything less proves nothing and the item is treated as
// still inside: an empty parent anywhere on the walk (the item itself or an
// ancestor), a lookup that fails or finds a deleted parent, or a walk deeper
// than mountAncestorLookupLimit.
func (e *Engine) liveItemLeftMount(ctx context.Context, live *graph.Item, inside map[string]struct{}) bool {
	parentID := live.ParentID
	for range mountAncestorLookupLimit {
		if parentID == "" {
			return false
		}
		if _, ok := inside[parentID]; ok {
			return false
		}
		parent, err := e.itemsClient.GetItem(ctx, e.driveID, parentID)
		if err != nil || parent == nil || parent.IsDeleted {
			return false
		}
		if parent.IsRoot {
			return true
		}
		parentID = parent.ParentID
	}

	return false
}

func (flow *engineFlow) commitObservedItems(
	ctx context.Context,
	observed []syncdomain.ObservedItem,
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
	bl *syncdomain.Baseline,
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

func (rt *watchRuntime) handleMountRootObservationResult(
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

func (rt *watchRuntime) handleMountRootPollError(
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
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.String("remote_root_item_id", rt.engine.remoteRootItemID),
	)

	return rt.sleepMountRootWatch(ctx, delay, "backoff")
}

func (rt *watchRuntime) dispatchMountRootBatch(
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

func (rt *watchRuntime) sleepMountRootWatch(
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
