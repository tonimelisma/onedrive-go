package sync

type observationFindingsReconcilePolicy struct {
	issues     observationIssueReconcilePolicy
	readScopes observationReadScopeReconcilePolicy
}

type observationIssueReconcilePolicy struct {
	managedPaths       []string
	currentPathsByType map[string][]string
}

type observationReadScopeReconcilePolicy struct {
	managed map[ScopeKey]struct{}
	desired map[ScopeKey]struct{}
}

func buildObservationFindingsReconcilePolicy(batch *ObservationFindingsBatch) observationFindingsReconcilePolicy {
	return observationFindingsReconcilePolicy{
		issues: observationIssueReconcilePolicy{
			managedPaths:       normalizedObservationManagedPaths(batch),
			currentPathsByType: observationIssuePathsByType(batch),
		},
		readScopes: observationReadScopeReconcilePolicy{
			managed: managedObservationReadScopes(batch),
			desired: desiredObservationReadScopes(batch),
		},
	}
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
