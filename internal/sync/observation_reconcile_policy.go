package sync

import "sort"

type observationReconcileState struct {
	issues []ObservationIssueRow
}

type observationReconcilePlan struct {
	issueUpserts []ObservationIssue
	issueDeletes []observationIssueDelete
}

type observationIssueDelete struct {
	path      string
	issueType string
}

func buildObservationReconcilePlan(
	batch *ObservationFindingsBatch,
	state observationReconcileState,
) observationReconcilePlan {
	return observationReconcilePlan{
		issueUpserts: observationIssueUpserts(batch),
		issueDeletes: buildObservationIssueDeletes(batch, state.issues),
	}
}

func observationIssueUpserts(batch *ObservationFindingsBatch) []ObservationIssue {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	return batch.Issues
}

func buildObservationIssueDeletes(
	batch *ObservationFindingsBatch,
	currentIssues []ObservationIssueRow,
) []observationIssueDelete {
	if batch == nil || len(batch.ManagedIssueTypes) == 0 || len(currentIssues) == 0 {
		return nil
	}

	managedPaths := normalizedObservationManagedPaths(batch)
	managedPathSet := stringSet(managedPaths)
	managedIssueTypes := stringSet(batch.ManagedIssueTypes)
	currentPathsByType := observationIssuePathsByType(batch)

	deletes := make([]observationIssueDelete, 0, len(currentIssues))
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
		if _, ok := currentPathsByType[current.IssueType][current.Path]; ok {
			continue
		}
		deletes = append(deletes, observationIssueDelete{
			path:      current.Path,
			issueType: current.IssueType,
		})
	}

	sort.Slice(deletes, func(i, j int) bool {
		if deletes[i].issueType != deletes[j].issueType {
			return deletes[i].issueType < deletes[j].issueType
		}
		return deletes[i].path < deletes[j].path
	})

	return deletes
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

func observationIssuePathsByType(batch *ObservationFindingsBatch) map[string]map[string]struct{} {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	currentByType := make(map[string]map[string]struct{})
	for i := range batch.Issues {
		issueType := batch.Issues[i].IssueType
		path := batch.Issues[i].Path
		if issueType == "" || path == "" {
			continue
		}
		if currentByType[issueType] == nil {
			currentByType[issueType] = make(map[string]struct{})
		}
		currentByType[issueType][path] = struct{}{}
	}

	return currentByType
}

func normalizedObservationManagedPaths(batch *ObservationFindingsBatch) []string {
	if batch == nil || len(batch.ManagedPaths) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(batch.ManagedPaths))
	normalized := make([]string, 0, len(batch.ManagedPaths))
	for i := range batch.ManagedPaths {
		path := batch.ManagedPaths[i]
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}

	return normalized
}
