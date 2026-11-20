package syncobserve

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// RemoteChange is what one reported item changes, as the planner-wake
// decision reads it: the path, the path the item moved from, and its kind.
type RemoteChange struct {
	Path     string
	OldPath  string
	ItemType syncdomain.ItemType
}

type remoteObservationResult struct {
	Observed []syncdomain.ObservedItem
	Changes  []RemoteChange
}

// ProjectRemoteObservations splits reported items into the rows
// CommitObservation records and the change list. An item without an ID
// stores no row but stays a change: the list holds every reported item, and
// its length decides whether the cursor advances. Its callers hand it only
// items a remote observer built (delta and listing conversion, synthesized
// orphan deletes).
func ProjectRemoteObservations(logger *slog.Logger, items []syncdomain.RemoteItem) remoteObservationResult {
	result := remoteObservationResult{
		Observed: make([]syncdomain.ObservedItem, 0, len(items)),
		Changes:  remoteChangesOf(items),
	}

	for i := range items {
		if items[i].ItemID == "" {
			if logger != nil {
				logger.Debug("remote observation: no row for an item without an ID",
					slog.String("path", items[i].Path),
				)
			}

			continue
		}

		result.Observed = append(result.Observed, items[i].ObservedItem)
	}

	return result
}

// remoteChangesOf lists the change of every reported item, in order.
func remoteChangesOf(items []syncdomain.RemoteItem) []RemoteChange {
	changes := make([]RemoteChange, 0, len(items))
	for i := range items {
		changes = append(changes, RemoteChange{
			Path:     items[i].Path,
			OldPath:  items[i].OldPath,
			ItemType: items[i].ItemType,
		})
	}

	return changes
}
