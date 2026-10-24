package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

func (flow *engineFlow) reconcileSkippedObservationFindings(
	ctx context.Context,
	skipped []skippedItem,
) error {
	flow.logSkippedObservationFindings(skipped, flow.knownObservationIssues(ctx))
	batch := localObservationFindingsBatchFromSkippedItems(flow.engine.driveID, skipped)
	return flow.applyObservationFindingsBatch(
		ctx,
		&batch,
		"record skipped local files",
		engineDebugNoteLocalSkipped,
	)
}

// observationIssueKey identifies one persisted skipped-file finding.
type observationIssueKey struct {
	path      string
	issueType string
}

// knownObservationIssues returns the skipped-file findings already persisted
// before this reconcile. Every full scan (startup and each watch safety scan)
// re-reports every persistent skipped item, so only findings missing from
// this set are new to the user. A read failure returns nil, which treats
// every finding as new: an extra warning beats a hidden one.
func (flow *engineFlow) knownObservationIssues(ctx context.Context) map[observationIssueKey]struct{} {
	eng := flow.engine
	rows, err := eng.baseline.ListObservationIssues(ctx)
	if err != nil {
		eng.logger.Debug("list observation issues before skipped-file report",
			slog.String("error", err.Error()),
		)

		return nil
	}

	known := make(map[observationIssueKey]struct{}, len(rows))
	for i := range rows {
		known[observationIssueKey{path: rows[i].Path, issueType: rows[i].IssueType}] = struct{}{}
	}

	return known
}

// logSkippedObservationFindings warns once per newly skipped file, at the
// scan that first finds it; findings already persisted (and shown by
// `status`) stay at Debug so a persistent bad name does not warn on every
// safety scan.
func (flow *engineFlow) logSkippedObservationFindings(
	skipped []skippedItem,
	known map[observationIssueKey]struct{},
) {
	eng := flow.engine
	byReason := make(map[string][]skippedItem)
	for i := range skipped {
		_, alreadyReported := known[observationIssueKey{path: skipped[i].Path, issueType: skipped[i].Reason}]
		if alreadyReported {
			eng.logger.Debug("observation filter: skipped file (already recorded)",
				slog.String("path", skipped[i].Path),
				slog.String("issue_type", skipped[i].Reason),
				slog.String("detail", skipped[i].Detail),
			)

			continue
		}
		byReason[skipped[i].Reason] = append(byReason[skipped[i].Reason], skipped[i])
	}

	for reason, items := range byReason {
		const aggregateThreshold = 10
		if len(items) <= aggregateThreshold {
			for i := range items {
				eng.logger.Warn("Not syncing "+items[i].Path+": "+skippedItemReasonText(&items[i]),
					slog.String("action", "run onedrive-go status for the full list"),
				)
			}

			continue
		}

		const sampleCount = 3
		samples := make([]string, 0, sampleCount)
		for i := range items {
			if i >= sampleCount {
				break
			}
			samples = append(samples, items[i].Path)
		}

		eng.logger.Warn(fmt.Sprintf("Not syncing %d files: %s", len(items), skippedItemReasonText(&items[0])),
			slog.Any("examples", samples),
			slog.String("action", "run onedrive-go status for the full list"),
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
	}
}

// skippedItemReasonText is the user-facing reason for a skipped file: the
// scanner's detail when it has one, otherwise the issue type in words.
func skippedItemReasonText(item *skippedItem) string {
	if item.Detail != "" {
		return item.Detail
	}

	return strings.ReplaceAll(item.Reason, "_", " ")
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
