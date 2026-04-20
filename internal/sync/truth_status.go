package sync

// TruthAvailabilityIndex is the raw derived view over observation issues and
// read scopes used to answer current-truth availability questions for one
// observation snapshot.
type TruthAvailabilityIndex struct {
	observationByPath map[string]ObservationIssueRow
	blockScopes       []*BlockScope
}

func NewTruthAvailabilityIndex(
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
) TruthAvailabilityIndex {
	return TruthAvailabilityIndex{
		observationByPath: truthBlockingObservationByPath(observationIssues),
		blockScopes:       blockScopes,
	}
}

// StatusForPath returns the raw local/remote truth availability for one path.
func (index TruthAvailabilityIndex) StatusForPath(path string) PathTruthStatus {
	return pathTruthStatusForPath(path, index.observationByPath, index.blockScopes)
}

// StatusByPath returns the raw local/remote truth availability for a path set.
func (index TruthAvailabilityIndex) StatusByPath(paths []string) map[string]PathTruthStatus {
	if len(paths) == 0 {
		return nil
	}

	statusByPath := make(map[string]PathTruthStatus, len(paths))
	for i := range paths {
		statusByPath[paths[i]] = index.StatusForPath(paths[i])
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
		scopePath := block.CoveredPath()
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
