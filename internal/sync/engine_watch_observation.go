package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// commitLocalScan commits one full local scan, a watch snapshot or the
// replan's own, through the one snapshot commit (localSource.commitSnapshot):
// its finding set as the whole set of local findings (clearing earlier ones
// the set lacks), its rows and the mounts it crossed, in one transaction. A
// newly skipped file is reported first. It reports what the commit changed.
func (flow *engineFlow) commitLocalScan(
	ctx context.Context,
	rows []syncdomain.LocalStateRow,
	skipped []syncdomain.SkippedItem,
	crossedMounts []string,
) (syncstore.LocalSnapshotCommitResult, error) {
	flow.logSkippedObservationFindings(skipped, flow.knownObservationIssues(ctx))
	findings := localObservationFindingsBatchFromSkippedItems(flow.cfg.driveID, skipped)
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type: syncdomain.EngineDebugEventObservationFindingsReconcileStarted,
		Note: engineDebugNoteLocalSkipped,
	})

	return flow.local.commitSnapshot(ctx, syncstore.LocalSnapshotCommit{
		Rows:          rows,
		Findings:      &findings,
		CrossedMounts: crossedMounts,
	})
}

// observationIssueKey identifies one persisted skipped-file finding.
type observationIssueKey struct {
	path      string
	issueType syncdomain.IssueType
}

// knownObservationIssues returns the skipped-file findings already persisted
// before this reconcile. Every full scan (startup and each watch safety scan)
// re-reports every persistent skipped item, so only findings missing from
// this set are new to the user. A read failure returns nil, which treats
// every finding as new: an extra warning beats a hidden one.
func (flow *engineFlow) knownObservationIssues(ctx context.Context) map[observationIssueKey]struct{} {
	rows, err := flow.deps.store.ListObservationIssues(ctx)
	if err != nil {
		flow.deps.logger.Debug("list observation issues before skipped-file report",
			slog.Any("error", err),
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
	skipped []syncdomain.SkippedItem,
	known map[observationIssueKey]struct{},
) {
	byReason := make(map[syncdomain.IssueType][]syncdomain.SkippedItem)
	for i := range skipped {
		_, alreadyReported := known[observationIssueKey{path: skipped[i].Path, issueType: skipped[i].Reason}]
		if alreadyReported {
			flow.deps.logger.Debug("observation filter: skipped file (already recorded)",
				slog.String("path", skipped[i].Path),
				slog.String("issue_type", string(skipped[i].Reason)),
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
				// No per-line hint: the line is complete, and the watch
				// condition summary that follows names `status` once.
				flow.deps.logger.Warn("Not syncing " + items[i].Path + ": " + skippedItemReasonText(&items[i]))
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

		flow.deps.logger.Warn(fmt.Sprintf("Not syncing %s, such as %s",
			aggregateSkippedText(reason, items), strings.Join(samples, ", ")))
		// Keep full per-path visibility at Debug while avoiding a warning
		// storm once a single scanner issue fans out across many files.
		for i := range items {
			flow.deps.logger.Debug("observation filter: skipped file",
				slog.String("path", items[i].Path),
				slog.String("issue_type", string(reason)),
				slog.String("detail", items[i].Detail),
			)
		}
	}
}

// aggregateSkippedText words a group of skipped files: "11 files: <detail>"
// when every file shares one detail, otherwise the condition's label ("11
// files with names OneDrive does not allow"). One file's detail once spoke for
// eleven different invalid names as "the name starts with a space".
func aggregateSkippedText(reason syncdomain.IssueType, items []syncdomain.SkippedItem) string {
	shared := skippedItemReasonText(&items[0])
	for i := 1; i < len(items); i++ {
		if skippedItemReasonText(&items[i]) == shared {
			continue
		}
		if key, ok := syncdomain.ConditionKeyForIssueType(reason); ok {
			return fmt.Sprintf("%d %s", len(items), conditionLabel(key, len(items)))
		}
		return fmt.Sprintf("%d files: %s", len(items), strings.ReplaceAll(string(reason), "_", " "))
	}

	return fmt.Sprintf("%d files: %s", len(items), shared)
}

// skippedItemReasonText is the user-facing reason for a skipped file: the
// scanner's detail when it has one, otherwise the issue type in words.
func skippedItemReasonText(item *syncdomain.SkippedItem) string {
	if item.Detail != "" {
		return item.Detail
	}

	return strings.ReplaceAll(string(item.Reason), "_", " ")
}

// applyObservationFindingsBatch persists one findings batch.
func (flow *engineFlow) applyObservationFindingsBatch(
	ctx context.Context,
	batch *syncdomain.ObservationFindingsBatch,
	failureMessage string,
	reconcileNote string,
) error {
	if batch == nil {
		return nil
	}

	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type: syncdomain.EngineDebugEventObservationFindingsReconcileStarted,
		Note: reconcileNote,
	})
	if err := flow.deps.store.ReconcileObservationFindings(ctx, batch); err != nil {
		return fmt.Errorf("%s: %w", failureMessage, err)
	}

	return nil
}
