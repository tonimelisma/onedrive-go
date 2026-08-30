package sync

import (
	"log/slog"
)

type remoteObservationResult struct {
	observed []observedItem
	emitted  []changeEvent
}

func projectRemoteObservations(
	logger *slog.Logger,
	events []changeEvent,
) remoteObservationResult {
	result := remoteObservationResult{
		observed: make([]observedItem, 0, len(events)),
		emitted:  make([]changeEvent, 0, len(events)),
	}

	for i := range events {
		ev := events[i]
		if ev.Source != sourceRemote {
			result.emitted = append(result.emitted, ev)
			continue
		}

		result.observed = appendObservedEvent(logger, result.observed, &ev)
		result.emitted = append(result.emitted, ev)
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
			logger.Warn("projectObservedItems: skipping event with empty ItemID",
				slog.String("path", ev.Path),
			)
		}

		return items
	}

	return append(items, observedItem{
		DriveID:   ev.DriveID,
		ItemID:    ev.ItemID,
		Path:      ev.Path,
		ItemType:  ev.ItemType,
		Hash:      ev.Hash,
		Size:      ev.Size,
		Mtime:     ev.Mtime,
		ETag:      ev.ETag,
		IsDeleted: ev.IsDeleted,
	})
}
