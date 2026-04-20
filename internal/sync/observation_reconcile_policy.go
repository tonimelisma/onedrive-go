package sync

import "sort"

type observationFindingsReconcilePlan struct {
	issueUpserts      []ObservationIssue
	issueClears       []observationIssueClearPlan
	readScopeUpserts  []ScopeKey
	readScopeReleases []ScopeKey
}

type observationIssueClearPlan struct {
	issueType    string
	managedPaths []string
	currentPaths []string
}

func buildObservationFindingsReconcilePlan(
	batch *ObservationFindingsBatch,
	currentBlocks []*BlockScope,
) observationFindingsReconcilePlan {
	return observationFindingsReconcilePlan{
		issueUpserts:      observationIssueUpserts(batch),
		issueClears:       buildObservationIssueClearPlan(batch),
		readScopeUpserts:  buildObservationReadScopeUpserts(batch, currentBlocks),
		readScopeReleases: buildObservationReadScopeReleases(batch, currentBlocks),
	}
}

func observationIssueUpserts(batch *ObservationFindingsBatch) []ObservationIssue {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	return batch.Issues
}

func buildObservationIssueClearPlan(batch *ObservationFindingsBatch) []observationIssueClearPlan {
	if batch == nil || len(batch.ManagedIssueTypes) == 0 {
		return nil
	}

	managedPaths := normalizedObservationManagedPaths(batch)
	currentPathsByType := observationIssuePathsByType(batch)

	plans := make([]observationIssueClearPlan, 0, len(batch.ManagedIssueTypes))
	for i := range batch.ManagedIssueTypes {
		issueType := batch.ManagedIssueTypes[i]
		if issueType == "" {
			continue
		}
		plans = append(plans, observationIssueClearPlan{
			issueType:    issueType,
			managedPaths: managedPaths,
			currentPaths: currentPathsByType[issueType],
		})
	}

	return plans
}

func buildObservationReadScopeUpserts(
	batch *ObservationFindingsBatch,
	currentBlocks []*BlockScope,
) []ScopeKey {
	managed := managedObservationReadScopes(batch)
	if len(managed) == 0 {
		return nil
	}

	desired := desiredObservationReadScopes(batch)
	if len(desired) == 0 {
		return nil
	}

	current := currentObservationReadScopes(currentBlocks, managed)
	return missingObservationReadScopes(current, desired)
}

func buildObservationReadScopeReleases(
	batch *ObservationFindingsBatch,
	currentBlocks []*BlockScope,
) []ScopeKey {
	managed := managedObservationReadScopes(batch)
	if len(managed) == 0 {
		return nil
	}

	desired := desiredObservationReadScopes(batch)
	current := currentObservationReadScopes(currentBlocks, managed)

	return missingObservationReadScopes(desired, current)
}

func currentObservationReadScopes(
	currentBlocks []*BlockScope,
	managed map[ScopeKey]struct{},
) map[ScopeKey]struct{} {
	if len(managed) == 0 || len(currentBlocks) == 0 {
		return nil
	}

	current := make(map[ScopeKey]struct{}, len(currentBlocks))
	for i := range currentBlocks {
		block := currentBlocks[i]
		if block == nil || !managedObservationReadScopeContains(managed, block.Key) {
			continue
		}
		current[block.Key] = struct{}{}
	}

	return current
}

func missingObservationReadScopes(
	present map[ScopeKey]struct{},
	required map[ScopeKey]struct{},
) []ScopeKey {
	if len(required) == 0 {
		return nil
	}

	missing := make([]ScopeKey, 0, len(required))
	for key := range required {
		if _, ok := present[key]; ok {
			continue
		}
		missing = append(missing, key)
	}

	sort.Slice(missing, func(i, j int) bool {
		return missing[i].String() < missing[j].String()
	})

	return missing
}

func observationIssuePathsByType(batch *ObservationFindingsBatch) map[string][]string {
	if batch == nil || len(batch.Issues) == 0 {
		return nil
	}

	currentByType := make(map[string][]string)
	for i := range batch.Issues {
		currentByType[batch.Issues[i].IssueType] = append(currentByType[batch.Issues[i].IssueType], batch.Issues[i].Path)
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

func managedObservationReadScopes(batch *ObservationFindingsBatch) map[ScopeKey]struct{} {
	if batch == nil {
		return nil
	}
	if managed, ok := exactManagedObservationReadScopes(batch); ok {
		return managed
	}

	return managedObservationReadScopeKeysForKinds(batch.ManagedReadScopeKinds)
}

func desiredObservationReadScopes(batch *ObservationFindingsBatch) map[ScopeKey]struct{} {
	if batch == nil {
		return nil
	}

	managed := managedObservationReadScopes(batch)
	if len(managed) == 0 {
		return nil
	}

	desired := make(map[ScopeKey]struct{})
	for i := range batch.ReadScopes {
		key := batch.ReadScopes[i]
		if !managedObservationReadScopeContains(managed, key) {
			continue
		}
		desired[key] = struct{}{}
	}

	return desired
}

func exactManagedObservationReadScopes(batch *ObservationFindingsBatch) (map[ScopeKey]struct{}, bool) {
	if batch == nil || len(batch.ManagedReadScopes) == 0 {
		return nil, false
	}

	managed := make(map[ScopeKey]struct{}, len(batch.ManagedReadScopes))
	for i := range batch.ManagedReadScopes {
		if batch.ManagedReadScopes[i].IsZero() {
			continue
		}
		managed[batch.ManagedReadScopes[i]] = struct{}{}
	}

	return managed, len(managed) > 0
}

func managedObservationReadScopeKeysForKinds(kinds []ScopeKeyKind) map[ScopeKey]struct{} {
	if len(kinds) == 0 {
		return nil
	}

	managed := make(map[ScopeKey]struct{})
	for i := range kinds {
		switch kinds[i] {
		case ScopePermDirRead:
			managed[SKPermLocalRead("")] = struct{}{}
		case ScopePermRemoteRead:
			managed[SKPermRemoteRead("")] = struct{}{}
		case ScopeThrottleTarget,
			ScopeService,
			ScopeQuotaOwn,
			ScopePermDirWrite,
			ScopePermRemoteWrite,
			ScopeDiskLocal:
			continue
		}
	}

	return managed
}

func managedObservationReadScopeContains(managed map[ScopeKey]struct{}, key ScopeKey) bool {
	if _, ok := managed[key]; ok {
		return true
	}
	if key.IsZero() {
		return false
	}

	switch key.Kind {
	case ScopePermDirRead:
		_, ok := managed[SKPermLocalRead("")]
		return ok
	case ScopePermRemoteRead:
		_, ok := managed[SKPermRemoteRead("")]
		return ok
	case ScopeThrottleTarget,
		ScopeService,
		ScopeQuotaOwn,
		ScopePermDirWrite,
		ScopePermRemoteWrite,
		ScopeDiskLocal:
		return false
	default:
		return false
	}
}
