package syncstore

import (
	"fmt"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

type observationReconcileStoreState struct {
	issues []syncdomain.ObservationIssueRow
}

type observationReconcilePlan struct {
	issueUpserts []syncdomain.ObservationIssue
	issueDeletes []managedObservationIssueKey
	// changesIssues is true when applying the plan leaves a different
	// persisted issue set: a finding appears, disappears, or changes type or
	// scope. Every full scan re-reports every persistent finding, and such
	// a re-report is not news the planner needs to hear (R-2.8.21).
	changesIssues bool
}

type managedObservationIssueKey struct {
	path      string
	issueType syncdomain.IssueType
}

// buildObservationReconcilePlan computes the exact upserts and deletes that
// make the stored issues of the batch's managed types match the batch. It
// refuses a batch that reports an issue of a type it does not manage (see
// requireManagedIssueTypes).
func buildObservationReconcilePlan(
	batch *syncdomain.ObservationFindingsBatch,
	state observationReconcileStoreState,
) (observationReconcilePlan, error) {
	if err := requireManagedIssueTypes(batch); err != nil {
		return observationReconcilePlan{}, err
	}

	desiredManagedIssues := desiredManagedObservationIssues(batch)
	currentManagedIssues := currentManagedObservationIssues(batch, state.issues)
	upserts := observationIssueUpserts(batch)
	deletes := observationIssueDeletesNotInDesired(currentManagedIssues, desiredManagedIssues)

	return observationReconcilePlan{
		issueUpserts:  upserts,
		issueDeletes:  deletes,
		changesIssues: len(deletes) > 0 || observationIssueUpsertsChangeRows(upserts, state.issues),
	}, nil
}

// requireManagedIssueTypes refuses a batch that reports an issue of a type it
// does not manage. A batch deletes only stored rows of its managed types, so
// such a row would outlive every later batch of the same kind that stops
// reporting it: the local scan once reported the leftover of an interrupted
// removal without managing its type, and the report outlived the file for
// good. The error names the issue's type and path, so the batch constructor
// at fault is found from the message.
func requireManagedIssueTypes(batch *syncdomain.ObservationFindingsBatch) error {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	managed := managedObservationTypeSet(batch)
	for i := range batch.Issues {
		issue := &batch.Issues[i]
		if _, ok := managed[issue.IssueType]; !ok {
			return fmt.Errorf("syncstore: observation findings batch reports issue type %q at %s without managing that type",
				issue.IssueType, issue.Path)
		}
	}

	return nil
}

// observationIssueUpsertsChangeRows reports whether any upsert differs from
// the row persisted for its path. The table keys issues by path, so a new
// type or scope at a known path counts, and a path with no row counts.
func observationIssueUpsertsChangeRows(upserts []syncdomain.ObservationIssue, currentIssues []syncdomain.ObservationIssueRow) bool {
	if len(upserts) == 0 {
		return false
	}

	byPath := make(map[string]syncdomain.ObservationIssueRow, len(currentIssues))
	for i := range currentIssues {
		byPath[currentIssues[i].Path] = currentIssues[i]
	}
	for i := range upserts {
		current, ok := byPath[upserts[i].Path]
		if !ok || current.IssueType != upserts[i].IssueType || current.ScopeKey != upserts[i].ScopeKey {
			return true
		}
	}

	return false
}

func observationIssueUpserts(batch *syncdomain.ObservationFindingsBatch) []syncdomain.ObservationIssue {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	upserts := make([]syncdomain.ObservationIssue, len(batch.Issues))
	copy(upserts, batch.Issues)
	return upserts
}

func observationIssueDeletesNotInDesired(
	currentManagedIssues map[managedObservationIssueKey]syncdomain.ObservationIssueRow,
	desiredManagedIssues map[managedObservationIssueKey]syncdomain.ObservationIssue,
) []managedObservationIssueKey {
	if len(currentManagedIssues) == 0 {
		return nil
	}

	deletes := make([]managedObservationIssueKey, 0, len(currentManagedIssues))
	for key := range currentManagedIssues {
		if _, ok := desiredManagedIssues[key]; ok {
			continue
		}

		deletes = append(deletes, key)
	}

	sort.Slice(deletes, func(i, j int) bool {
		if deletes[i].issueType != deletes[j].issueType {
			return deletes[i].issueType < deletes[j].issueType
		}
		return deletes[i].path < deletes[j].path
	})

	return deletes
}

// currentManagedObservationIssues filters raw durable rows down to the stored
// issues of the batch's managed types: the set this batch replaces.
func currentManagedObservationIssues(
	batch *syncdomain.ObservationFindingsBatch,
	currentIssues []syncdomain.ObservationIssueRow,
) map[managedObservationIssueKey]syncdomain.ObservationIssueRow {
	if batch == nil || len(batch.ManagedIssueTypes) == 0 || len(currentIssues) == 0 {
		return nil
	}

	managedIssueTypes := managedObservationTypeSet(batch)

	currentManagedIssues := make(map[managedObservationIssueKey]syncdomain.ObservationIssueRow)
	for i := range currentIssues {
		current := currentIssues[i]
		if _, ok := managedIssueTypes[current.IssueType]; !ok {
			continue
		}
		currentManagedIssues[managedObservationIssueKey{
			path:      current.Path,
			issueType: current.IssueType,
		}] = current
	}

	return currentManagedIssues
}

// desiredManagedObservationIssues is the exact managed set the batch proved
// should still exist after reconciliation. Every issue is of a managed type
// (requireManagedIssueTypes ran first); one without a path is refused when
// the store writes it, which fails the whole reconcile.
func desiredManagedObservationIssues(
	batch *syncdomain.ObservationFindingsBatch,
) map[managedObservationIssueKey]syncdomain.ObservationIssue {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	desired := make(map[managedObservationIssueKey]syncdomain.ObservationIssue, len(batch.Issues))
	for i := range batch.Issues {
		issue := batch.Issues[i]
		if issue.Path == "" {
			continue
		}
		desired[managedObservationIssueKey{
			path:      issue.Path,
			issueType: issue.IssueType,
		}] = issue
	}

	return desired
}

func managedObservationTypeSet(batch *syncdomain.ObservationFindingsBatch) map[syncdomain.IssueType]struct{} {
	if batch == nil || len(batch.ManagedIssueTypes) == 0 {
		return nil
	}

	managed := make(map[syncdomain.IssueType]struct{}, len(batch.ManagedIssueTypes))
	for _, issueType := range batch.ManagedIssueTypes {
		if issueType == "" {
			continue
		}
		managed[issueType] = struct{}{}
	}

	return managed
}
