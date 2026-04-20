package sync

// derivePathTruthStatusByPath computes current local/remote truth availability
// for each requested path from durable observation issues and active read
// scopes. This is a pure derived view over durable authorities; planner
// suppression and any future read-side consumers apply their own policy on top.
func derivePathTruthStatusByPath(
	paths []string,
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
) map[string]PathTruthStatus {
	if len(paths) == 0 {
		return nil
	}

	statusByPath := make(map[string]PathTruthStatus, len(paths))
	observationByPath := truthBlockingObservationByPath(observationIssues)
	for i := range paths {
		path := paths[i]
		statusByPath[path] = pathTruthStatusForPath(path, observationByPath, blockScopes)
	}

	return statusByPath
}

func truthBlockingObservationByPath(observationIssues []ObservationIssueRow) map[string]ObservationIssueRow {
	observationByPath := make(map[string]ObservationIssueRow, len(observationIssues))
	for i := range observationIssues {
		if !observationIssueBlocksTruth(observationIssues[i].IssueType) {
			continue
		}
		observationByPath[observationIssues[i].Path] = observationIssues[i]
	}

	return observationByPath
}

func pathTruthStatusForPath(
	path string,
	observationByPath map[string]ObservationIssueRow,
	blockScopes []*BlockScope,
) PathTruthStatus {
	observationIssue, hasObservationIssue := observationByPath[path]
	localScope := mostSpecificTruthReadScope(path, blockScopes, func(key ScopeKey) bool {
		return key.IsPermLocalRead()
	})
	remoteScope := mostSpecificTruthReadScope(path, blockScopes, func(key ScopeKey) bool {
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

	return status
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

func mostSpecificTruthReadScope(
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
		scopePath := blockScopePath(block)
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
