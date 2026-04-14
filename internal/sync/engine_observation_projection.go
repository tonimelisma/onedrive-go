package sync

import (
	"context"
	"log/slog"
)

type remoteObservationResult struct {
	observed []ObservedItem
	emitted  []ChangeEvent
}

func (flow *engineFlow) applyObservationState(
	ctx context.Context,
	dryRun bool,
	session *ObservationSession,
	plan *ObservationSessionPlan,
) error {
	_ = ctx
	_ = dryRun
	_ = session
	_ = plan
	return nil
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

func appendObservedEvent(
	logger *slog.Logger,
	items []ObservedItem,
	ev *ChangeEvent,
) []ObservedItem {
	if ev.ItemID == "" {
		if logger != nil {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
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
