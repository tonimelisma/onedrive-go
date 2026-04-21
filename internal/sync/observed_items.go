package sync

import (
	"log/slog"
)

type remoteObservationResult struct {
	observed []ObservedItem
	emitted  []ChangeEvent
}

func projectRemoteObservations(
	logger *slog.Logger,
	events []ChangeEvent,
) remoteObservationResult {
	result := remoteObservationResult{
		observed: make([]ObservedItem, 0, len(events)),
		emitted:  make([]ChangeEvent, 0, len(events)),
	}

	for i := range events {
		ev := events[i]
		if ev.Source != SourceRemote {
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
func projectObservedItems(logger *slog.Logger, events []ChangeEvent) []ObservedItem {
	return projectRemoteObservations(logger, events).observed
}

func appendObservedEvent(
	logger *slog.Logger,
	items []ObservedItem,
	ev *ChangeEvent,
) []ObservedItem {
	if ev.ItemID == "" {
		if logger != nil {
			logger.Warn("projectObservedItems: skipping event with empty ItemID",
				slog.String("path", ev.Path),
			)
		}

		return items
	}

	return append(items, ObservedItem{
		DriveID:   ev.DriveID,
		ItemID:    ev.ItemID,
		ParentID:  ev.ParentID,
		Path:      ev.Path,
		ItemType:  ev.ItemType,
		Hash:      ev.Hash,
		Size:      ev.Size,
		Mtime:     ev.Mtime,
		ETag:      ev.ETag,
		IsDeleted: ev.IsDeleted,
	})
}
