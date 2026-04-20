package sync

import (
	"context"
	"log/slog"
)

func (flow *engineFlow) reconcileSkippedObservationFindings(
	ctx context.Context,
	watch *watchRuntime,
	skipped []SkippedItem,
) {
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

	batch := localObservationFindingsBatchFromSkippedItems(eng.driveID, skipped)
	flow.reconcileObservationFindingsBatch(ctx, watch, &batch, "failed to reconcile local observation findings")
}

func (flow *engineFlow) reconcileObservationFindingsBatch(
	ctx context.Context,
	watch *watchRuntime,
	batch *ObservationFindingsBatch,
	failureMessage string,
) {
	eng := flow.engine
	if batch == nil {
		return
	}

	if err := eng.baseline.ReconcileObservationFindings(ctx, batch, eng.nowFunc()); err != nil {
		eng.logger.Error(failureMessage,
			slog.Int("issues", len(batch.Issues)),
			slog.String("error", err.Error()),
		)
		return
	}

	if watch != nil {
		if err := flow.scopeController().loadActiveScopes(ctx, watch); err != nil {
			eng.logger.Warn("failed to refresh watch scopes after observation reconcile",
				slog.String("error", err.Error()),
			)
		}
	}
}
