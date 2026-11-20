package sync

import (
	"log/slog"
)

// RemoteItem is one item a remote observer reports: the row it stores and,
// when the item moved, the path that row had before. The converter and the
// orphan finders build it; the old path is never stored.
type RemoteItem struct {
	ObservedItem
	OldPath string
}

// RemoteChange is what one reported item changes, as the planner-wake
// decision reads it: the path, the path the item moved from, and its kind.
type RemoteChange struct {
	Path     string
	OldPath  string
	ItemType ItemType
}

type remoteObservationResult struct {
	Observed []ObservedItem
	Changes  []RemoteChange
}

// ProjectRemoteObservations splits reported items into the rows
// CommitObservation records and the change list. An item without an ID
// stores no row but stays a change: the list holds every reported item, and
// its length decides whether the cursor advances. Its callers hand it only
// items a remote observer built (delta and listing conversion, synthesized
// orphan deletes).
func ProjectRemoteObservations(logger *slog.Logger, items []RemoteItem) remoteObservationResult {
	result := remoteObservationResult{
		Observed: make([]ObservedItem, 0, len(items)),
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
func remoteChangesOf(items []RemoteItem) []RemoteChange {
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
