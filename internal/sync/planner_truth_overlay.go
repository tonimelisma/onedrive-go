package sync

type plannerTruthAvailability struct {
	LocalTruthAvailability  TruthAvailability
	RemoteTruthAvailability TruthAvailability
	LocalTruthIssueType     string
	RemoteTruthIssueType    string
	LocalTruthScopeKey      ScopeKey
	RemoteTruthScopeKey     ScopeKey
}

func normalizePlannerTruthAvailability(
	comparisons []SQLiteComparisonRow,
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
) map[string]plannerTruthAvailability {
	if len(comparisons) == 0 {
		return nil
	}

	observationByPath := make(map[string]ObservationIssueRow, len(observationIssues))
	for i := range observationIssues {
		if !observationIssueBlocksTruth(observationIssues[i].IssueType) {
			continue
		}
		observationByPath[observationIssues[i].Path] = observationIssues[i]
	}

	availabilityByPath := make(map[string]plannerTruthAvailability, len(comparisons))
	for i := range comparisons {
		path := comparisons[i].Path
		observationIssue, hasObservationIssue := observationByPath[path]
		localScope := mostSpecificPlannerReadScope(path, blockScopes, func(key ScopeKey) bool {
			return key.IsPermLocalRead()
		})
		remoteScope := mostSpecificPlannerReadScope(path, blockScopes, func(key ScopeKey) bool {
			return key.IsPermRemoteRead()
		})

		availability := plannerTruthAvailability{
			LocalTruthAvailability:  TruthAvailabilityAvailable,
			RemoteTruthAvailability: TruthAvailabilityAvailable,
		}

		switch {
		case localScope.IsPermLocalRead():
			availability.LocalTruthAvailability = TruthAvailabilityBlockedLocalReadScope
			availability.LocalTruthScopeKey = localScope
		case hasObservationIssue && observationIssueBlocksLocalTruth(observationIssue.IssueType):
			availability.LocalTruthAvailability = TruthAvailabilityBlockedObservationIssue
			availability.LocalTruthIssueType = observationIssue.IssueType
			availability.LocalTruthScopeKey = observationIssue.ScopeKey
		}

		switch {
		case remoteScope.IsPermRemoteRead():
			availability.RemoteTruthAvailability = TruthAvailabilityBlockedRemoteReadScope
			availability.RemoteTruthScopeKey = remoteScope
		case hasObservationIssue && observationIssueBlocksRemoteTruth(observationIssue.IssueType):
			availability.RemoteTruthAvailability = TruthAvailabilityBlockedObservationIssue
			availability.RemoteTruthIssueType = observationIssue.IssueType
			availability.RemoteTruthScopeKey = observationIssue.ScopeKey
		}

		if availability.LocalTruthAvailability != TruthAvailabilityAvailable ||
			availability.RemoteTruthAvailability != TruthAvailabilityAvailable {
			availabilityByPath[path] = availability
		}
	}

	return availabilityByPath
}

func observationIssueBlocksTruth(issueType string) bool {
	return observationIssueBlocksLocalTruth(issueType) || observationIssueBlocksRemoteTruth(issueType)
}

func observationIssueBlocksLocalTruth(issueType string) bool {
	switch issueType {
	case IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueHashPanic,
		IssueLocalReadDenied:
		return true
	default:
		return false
	}
}

func observationIssueBlocksRemoteTruth(issueType string) bool {
	return issueType == IssueRemoteReadDenied
}

func mostSpecificPlannerReadScope(
	path string,
	blockScopes []*BlockScope,
	matches func(ScopeKey) bool,
) ScopeKey {
	best := ScopeKey{}
	bestLen := -1

	for i := range blockScopes {
		block := blockScopes[i]
		if block == nil || !matches(block.Key) {
			continue
		}
		scopePath := plannerScopePath(block.Key)
		if !scopePathMatches(path, scopePath) {
			continue
		}
		if len(scopePath) > bestLen {
			best = block.Key
			bestLen = len(scopePath)
		}
	}

	return best
}

func plannerScopePath(key ScopeKey) string {
	switch {
	case key.IsPermDir():
		return key.DirPath()
	case key.IsPermRemote():
		return key.RemotePath()
	default:
		return ""
	}
}
