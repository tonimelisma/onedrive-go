package sync

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// remoteSource is the remote side of the drive as a run observes it: the
// Graph clients, named one by one, the drive and the folder it reads, and the
// store's remote_state rows for the paths of deleted items. It is what the
// observer goroutines and the full remote refresh reach on the remote side;
// nothing on it is run state.
type remoteSource struct {
	fetcher         DeltaFetcher
	items           ItemClient
	folderDelta     FolderDeltaFetcher // optional: mount-root delta observation
	recursiveLister RecursiveLister    // optional: mount-root recursive enumeration fallback
	socketIO        SocketIOEndpointFetcher
	// wakeSourceFactory replaces the watch-mode websocket wake source; nil
	// builds the production one (engineHooks.socketIOWakeSourceFactory).
	wakeSourceFactory func(
		SocketIOEndpointFetcher,
		driveid.ID,
		syncobserve.SocketIOWakeSourceOptions,
	) socketIOWakeSourceRunner

	store            *syncstore.SyncStore
	driveID          driveid.ID
	remoteRootItemID string
	rootDeltaCapable bool
	logger           *slog.Logger
	// clock is the engine's real clock, which every observer runs on.
	clock syncdomain.RealClock

	// deltaFallbackReported is the engine's: set once the engine has
	// reported, at Info, that folder delta is unavailable for its
	// shared-folder root, cleared when folder delta succeeds again.
	deltaFallbackReported *atomic.Bool
}

// remoteSource is the engine's remote side as it is now, with the wake-source
// seam of hooks. A run takes it once (flowInputs), with its own copy of the
// hooks.
func (e *Engine) remoteSource(hooks *engineHooks) *remoteSource {
	return &remoteSource{
		fetcher:               e.fetcher,
		items:                 e.itemsClient,
		folderDelta:           e.folderDelta,
		recursiveLister:       e.recursiveLister,
		socketIO:              e.socketIOFetcher,
		wakeSourceFactory:     hooks.socketIOWakeSourceFactory,
		store:                 e.baseline,
		driveID:               e.driveID,
		remoteRootItemID:      e.remoteRootItemID,
		rootDeltaCapable:      e.remoteRootDeltaCapable,
		logger:                e.logger,
		clock:                 e.wallClock,
		deltaFallbackReported: &e.mountRootDeltaFallbackReported,
	}
}

// observer builds an observer of the drive from in, completed with what the
// engine's remote side owns: the delta fetcher, the drive, the real clock,
// the item client and the remote_state path lookup for deleted items.
func (s *remoteSource) observer(ctx context.Context, in *syncobserve.RemoteObserverInputs) *syncobserve.RemoteObserver {
	in.Fetcher = s.fetcher
	in.DriveID = s.driveID
	in.Clock = s.clock
	in.Items = s.items
	in.RemotePathLookup = s.pathLookup(ctx)

	return syncobserve.NewRemoteObserver(in)
}

// pathLookup resolves an item ID to the path the engine's remote_state row
// stores, for deleted items the baseline does not hold (R-2.4.17). A lookup
// failure reads as unknown; the delete is then skipped as before.
func (s *remoteSource) pathLookup(ctx context.Context) func(itemID string) (string, bool) {
	return func(itemID string) (string, bool) {
		if s == nil || s.store == nil {
			return "", false
		}
		row, found, err := s.store.GetRemoteStateByID(ctx, s.driveID, itemID)
		if err != nil {
			s.logger.Debug("remote_state lookup for a deleted item failed",
				slog.String("item_id", itemID),
				slog.Any("error", err),
			)

			return "", false
		}
		if !found || row == nil {
			return "", false
		}

		return row.Path, true
	}
}

// newWakeSource builds the watch-mode websocket wake source.
func (s *remoteSource) newWakeSource(opts syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
	if s.wakeSourceFactory != nil {
		return s.wakeSourceFactory(s.socketIO, s.driveID, opts)
	}

	return syncobserve.NewSocketIOWakeSourceWithOptions(s.socketIO, s.driveID, opts)
}

func (s *remoteSource) mountRootDeltaSupported() bool {
	return s != nil && s.rootDeltaCapable && s.folderDelta != nil
}

func (s *remoteSource) preferredMountRootObservationMode() syncdomain.RemoteObservationMode {
	if s.mountRootDeltaSupported() {
		return syncdomain.RemoteObservationModeDelta
	}

	return syncdomain.RemoteObservationModeEnumerate
}

// mountRootItems converts a mount-root enumeration, adding confirmed orphan
// deletes when the enumeration was complete. kind is the resync that forced
// the enumeration, if any: unless it applies differences, confirmed orphans
// are re-uploads, not deletes (R-2.15.3).
func (s *remoteSource) mountRootItems(
	ctx context.Context, items []graph.Item, bl syncdomain.BaselineView, complete bool, kind syncdomain.ResyncKind,
) ([]syncdomain.RemoteItem, bool) {
	events := convertMountRootItems(ctx, items, s.remoteRootItemID, s.driveID, bl, s.logger, s.items)
	if !complete {
		return events, false
	}
	orphans, undecided := s.confirmMountRootOrphans(ctx, items, detectMountRootOrphans(items, s.driveID, bl))
	if !kind.OrphansAreDeletes() {
		markOrphansForReupload(orphans)
	}

	return append(events, orphans...), undecided
}

// confirmMountRootOrphans keeps only the synthesized deletes a live lookup
// confirms. One enumeration of a shared folder (/children pages, or a folder
// delta) is not proof of absence: Graph omits items transiently, and a delete
// synthesized from that omission removes the local copy, which the next
// listing then downloads again (D-009). An item is gone when GetItem answers
// 404 or a deleted facet, or when its ancestors provably lead outside the
// mount; an item still inside, one whose parent cannot be resolved, or a
// lookup that failed is left for the next listing.
func (s *remoteSource) confirmMountRootOrphans(
	ctx context.Context, listed []graph.Item, orphans []syncdomain.RemoteItem,
) (confirmed []syncdomain.RemoteItem, undecided bool) {
	inside := make(map[string]struct{}, len(listed)+1)
	inside[s.remoteRootItemID] = struct{}{}
	for i := range listed {
		inside[listed[i].ID] = struct{}{}
	}

	return s.confirmEnumerationOrphans(ctx, orphans, inside)
}

// confirmEnumerationOrphans looks up each delete a complete enumeration
// synthesized and keeps only those confirmed gone. inside is nil for a
// drive-root enumeration, where any live item is still in scope; for a mount
// root it holds the mount root and every item the listing returned. An
// unconfirmed orphan produces no event, so its local copy stays and the next
// complete enumeration judges it again. undecided reports an orphan whose
// lookup failed: unlike one found live, it still needs that judgement, and
// soon (observeRemoteFullWithShortcutTopology).
func (s *remoteSource) confirmEnumerationOrphans(
	ctx context.Context,
	orphans []syncdomain.RemoteItem,
	inside map[string]struct{},
) (confirmed []syncdomain.RemoteItem, undecided bool) {
	if len(orphans) == 0 || s.items == nil {
		return orphans, false
	}

	confirmed = make([]syncdomain.RemoteItem, 0, len(orphans))
	for i := range orphans {
		orphan := orphans[i]
		live, err := s.items.GetItem(ctx, s.driveID, orphan.ItemID)
		switch {
		case errors.Is(err, graph.ErrNotFound):
			confirmed = append(confirmed, orphan)
		case err != nil:
			undecided = true
			s.logger.Debug("could not confirm an item missing from a listing is gone; keeping it",
				slog.String("path", orphan.Path), slog.Any("error", err))
		case live == nil:
			continue
		case live.IsDeleted:
			confirmed = append(confirmed, orphan)
		case inside == nil:
			s.logger.Debug("a full rescan omitted a live item; not deleting it",
				slog.String("path", orphan.Path), slog.String("item_id", orphan.ItemID))
		default:
			if s.liveItemLeftMount(ctx, live, inside) {
				confirmed = append(confirmed, orphan)
				continue
			}
			s.logger.Debug("shared-folder listing omitted a live item; not deleting it",
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
func (s *remoteSource) liveItemLeftMount(ctx context.Context, live *graph.Item, inside map[string]struct{}) bool {
	parentID := live.ParentID
	for range mountAncestorLookupLimit {
		if parentID == "" {
			return false
		}
		if _, ok := inside[parentID]; ok {
			return false
		}
		parent, err := s.items.GetItem(ctx, s.driveID, parentID)
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
