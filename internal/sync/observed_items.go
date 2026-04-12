package sync

import (
	"log/slog"
)

// changeEventsToObservedItems converts remote ChangeEvents into ObservedItems
// for CommitObservation. It keeps malformed payload filtering at the engine
// boundary even though remote observation and one-shot reconciliation now live
// in the same package.
func changeEventsToObservedItems(logger *slog.Logger, events []ChangeEvent) []ObservedItem {
	var items []ObservedItem

	for i := range events {
		if events[i].Source != SourceRemote {
			continue
		}

		if events[i].ItemID == "" {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", events[i].Path),
			)

			continue
		}

		items = append(items, ObservedItem{
			DriveID:   events[i].DriveID,
			ItemID:    events[i].ItemID,
			ParentID:  events[i].ParentID,
			Path:      events[i].Path,
			ItemType:  events[i].ItemType,
			Hash:      events[i].Hash,
			Size:      events[i].Size,
			Mtime:     events[i].Mtime,
			ETag:      events[i].ETag,
			IsDeleted: events[i].IsDeleted,
		})
	}

	return items
}
