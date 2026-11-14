package sync

import (
	"log/slog"
)

type remoteObservationResult struct {
	observed []observedItem
	emitted  []changeEvent
}

// projectRemoteObservations converts a remote observer's events into the
// observed items CommitObservation records, and keeps every event as emitted.
// Its callers hand it only events a remote observer built (delta and listing
// conversion, synthesized orphan deletes).
func projectRemoteObservations(
	logger *slog.Logger,
	events []changeEvent,
) remoteObservationResult {
	result := remoteObservationResult{
		observed: make([]observedItem, 0, len(events)),
		emitted:  make([]changeEvent, 0, len(events)),
	}

	for i := range events {
		result.observed = appendObservedEvent(logger, result.observed, &events[i])
		result.emitted = append(result.emitted, events[i])
	}

	return result
}

// projectObservedItems converts remote ChangeEvents into ObservedItems for
// CommitObservation. It keeps malformed payload filtering at the observation
// projection boundary.
func projectObservedItems(logger *slog.Logger, events []changeEvent) []observedItem {
	return projectRemoteObservations(logger, events).observed
}

func appendObservedEvent(
	logger *slog.Logger,
	items []observedItem,
	ev *changeEvent,
) []observedItem {
	if ev.ItemID == "" {
		if logger != nil {
			logger.Debug("projectObservedItems: skipping event with empty ItemID",
				slog.String("path", ev.Path),
			)
		}

		return items
	}

	return append(items, observedItem{
		DriveID:       ev.DriveID,
		ItemID:        ev.ItemID,
		ParentID:      ev.ParentID,
		Path:          ev.Path,
		ItemType:      ev.ItemType,
		Hash:          ev.Hash,
		Size:          ev.Size,
		SizeUnknown:   ev.SizeUnknown,
		Mtime:         ev.Mtime,
		ETag:          ev.ETag,
		IsDeleted:     ev.IsDeleted,
		ReuploadLocal: ev.ReuploadLocal,
	})
}
