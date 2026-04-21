package sync

import "sort"

type observationReconcileStoreState struct {
	issues []ObservationIssueRow
}

type observationReconcilePlan struct {
	issueUpserts []ObservationIssue
	issueDeletes []managedObservationIssueKey
}

type managedObservationIssueKey struct {
	path      string
	issueType string
}

func buildObservationReconcilePlan(
	batch *ObservationFindingsBatch,
	state observationReconcileStoreState,
) observationReconcilePlan {
	desiredManagedIssues := desiredManagedObservationIssues(batch)
	currentManagedIssues := currentManagedObservationIssues(batch, state.issues)

	return observationReconcilePlan{
		issueUpserts: observationIssueUpserts(batch),
		issueDeletes: observationIssueDeletesNotInDesired(currentManagedIssues, desiredManagedIssues),
	}
}

func observationIssueUpserts(batch *ObservationFindingsBatch) []ObservationIssue {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	upserts := make([]ObservationIssue, len(batch.Issues))
	copy(upserts, batch.Issues)
	return upserts
}

func observationIssueDeletesNotInDesired(
	currentManagedIssues map[managedObservationIssueKey]ObservationIssueRow,
	desiredManagedIssues map[managedObservationIssueKey]ObservationIssue,
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

// currentManagedObservationIssues filters raw durable rows down to the exact
// managed issue set this batch is allowed to replace.
func currentManagedObservationIssues(
	batch *ObservationFindingsBatch,
	currentIssues []ObservationIssueRow,
) map[managedObservationIssueKey]ObservationIssueRow {
	if batch == nil || len(batch.ManagedIssueTypes) == 0 || len(currentIssues) == 0 {
		return nil
	}

	managedPathSet := managedObservationPathSet(batch)
	managedIssueTypes := managedObservationTypeSet(batch)

	currentManagedIssues := make(map[managedObservationIssueKey]ObservationIssueRow)
	for i := range currentIssues {
		current := currentIssues[i]
		if current.IssueType == "" {
			continue
		}
		if _, ok := managedIssueTypes[current.IssueType]; !ok {
			continue
		}
		if len(managedPathSet) > 0 {
			if _, ok := managedPathSet[current.Path]; !ok {
				continue
			}
		}
		currentManagedIssues[managedObservationIssueKey{
			path:      current.Path,
			issueType: current.IssueType,
		}] = current
	}

	return currentManagedIssues
}

// desiredManagedObservationIssues is the exact managed set the batch proved
// should still exist after reconciliation.
func desiredManagedObservationIssues(
	batch *ObservationFindingsBatch,
) map[managedObservationIssueKey]ObservationIssue {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	desired := make(map[managedObservationIssueKey]ObservationIssue, len(batch.Issues))
	for i := range batch.Issues {
		issue := batch.Issues[i]
		if issue.Path == "" || issue.IssueType == "" {
			continue
		}
		desired[managedObservationIssueKey{
			path:      issue.Path,
			issueType: issue.IssueType,
		}] = issue
	}

	return desired
}

func managedObservationTypeSet(batch *ObservationFindingsBatch) map[string]struct{} {
	if batch == nil {
		return nil
	}

	return stringSet(batch.ManagedIssueTypes)
}

func managedObservationPathSet(batch *ObservationFindingsBatch) map[string]struct{} {
	if batch == nil || len(batch.ManagedPaths) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(batch.ManagedPaths))
	paths := make(map[string]struct{}, len(batch.ManagedPaths))
	for i := range batch.ManagedPaths {
		path := batch.ManagedPaths[i]
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths[path] = struct{}{}
	}

	return paths
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}

	normalized := make(map[string]struct{}, len(values))
	for i := range values {
		if values[i] == "" {
			continue
		}
		normalized[values[i]] = struct{}{}
	}

	return normalized
}
