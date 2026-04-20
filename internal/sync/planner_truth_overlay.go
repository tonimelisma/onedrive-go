package sync

func derivePlannerTruthStatus(
	comparisons []SQLiteComparisonRow,
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
) map[string]PathTruthStatus {
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

	statusByPath := make(map[string]PathTruthStatus, len(comparisons))
	for i := range comparisons {
		path := comparisons[i].Path
		observationIssue, hasObservationIssue := observationByPath[path]
		localScope := mostSpecificPlannerReadScope(path, blockScopes, func(key ScopeKey) bool {
			return key.IsPermLocalRead()
		})
		remoteScope := mostSpecificPlannerReadScope(path, blockScopes, func(key ScopeKey) bool {
			return key.IsPermRemoteRead()
		})

		status := PathTruthStatus{
			Local:  availablePathTruthSideStatus(),
			Remote: availablePathTruthSideStatus(),
		}

		switch {
		case localScope.IsPermLocalRead():
			status.Local = PathTruthSideStatus{
				Availability: TruthAvailabilityBlockedLocalReadScope,
				Source:       PathTruthSourceReadScope,
				ScopeKey:     localScope,
			}
		case hasObservationIssue && observationIssueBlocksLocalTruth(observationIssue.IssueType):
			status.Local = PathTruthSideStatus{
				Availability: TruthAvailabilityBlockedObservationIssue,
				Source:       PathTruthSourceObservationIssue,
				IssueType:    observationIssue.IssueType,
				ScopeKey:     observationIssue.ScopeKey,
			}
		}

		switch {
		case remoteScope.IsPermRemoteRead():
			status.Remote = PathTruthSideStatus{
				Availability: TruthAvailabilityBlockedRemoteReadScope,
				Source:       PathTruthSourceReadScope,
				ScopeKey:     remoteScope,
			}
		case hasObservationIssue && observationIssueBlocksRemoteTruth(observationIssue.IssueType):
			status.Remote = PathTruthSideStatus{
				Availability: TruthAvailabilityBlockedObservationIssue,
				Source:       PathTruthSourceObservationIssue,
				IssueType:    observationIssue.IssueType,
				ScopeKey:     observationIssue.ScopeKey,
			}
		}

		if status.SuppressesStructuralActions() {
			statusByPath[path] = status
		}
	}

	return statusByPath
}

func availablePathTruthSideStatus() PathTruthSideStatus {
	return PathTruthSideStatus{
		Availability: TruthAvailabilityAvailable,
	}
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
		scopePath := DescribeScopeKey(block.Key).ScopePath()
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
