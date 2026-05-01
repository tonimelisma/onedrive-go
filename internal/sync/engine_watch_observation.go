package sync

import (
	"context"
	"fmt"
	"log/slog"
)

func (flow *engineFlow) reconcileSkippedObservationFindings(
	ctx context.Context,
	skipped []SkippedItem,
) error {
	flow.logSkippedObservationFindings(skipped)
	batch := localObservationFindingsBatchFromSkippedItems(flow.engine.driveID, skipped)
	return flow.applyObservationFindingsBatch(
		ctx,
		&batch,
		"failed to reconcile local observation findings",
		engineDebugNoteLocalSkipped,
	)
}

func (flow *engineFlow) logSkippedObservationFindings(skipped []SkippedItem) {
	eng := flow.engine
	byReason := make(map[string][]SkippedItem)
	for i := range skipped {
		byReason[skipped[i].Reason] = append(byReason[skipped[i].Reason], skipped[i])
	}

	for reason, items := range byReason {
		const aggregateThreshold = 10
		if len(items) > aggregateThreshold {
			const sampleCount = 3
			samples := make([]string, 0, sampleCount)
			for i := range items {
				if i >= sampleCount {
					break
				}
				samples = append(samples, items[i].Path)
			}

			eng.logger.Warn("observation filter: skipped files",
				slog.String("issue_type", reason),
				slog.Int("count", len(items)),
				slog.Any("sample_paths", samples),
			)
			// Keep full per-path visibility at Debug while avoiding a warning
			// storm once a single scanner issue fans out across many files.
			for i := range items {
				eng.logger.Debug("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		} else {
			for i := range items {
				eng.logger.Warn("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		}
	}
}

func (flow *engineFlow) applyObservationFindingsBatch(
	ctx context.Context,
	batch *ObservationFindingsBatch,
	failureMessage string,
	reconcileNote string,
) error {
	eng := flow.engine
	if batch == nil {
		return nil
	}

	eng.emitDebugEvent(engineDebugEvent{
		Type: engineDebugEventObservationFindingsReconcileStarted,
		Note: reconcileNote,
	})
	if err := eng.baseline.ReconcileObservationFindings(ctx, batch, eng.nowFunc()); err != nil {
		return fmt.Errorf("%s: %w", failureMessage, err)
	}

	return nil
}
