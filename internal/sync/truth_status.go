package sync

// truthAvailabilityIndex is the raw derived view over observation issues used
// to answer current-truth availability questions for one observation snapshot.
type truthAvailabilityIndex struct {
	observationByPath    map[string]ObservationIssueRow
	localReadBoundaries  []ObservationIssueRow
	remoteReadBoundaries []ObservationIssueRow
}

func newTruthAvailabilityIndex(observationIssues []ObservationIssueRow) truthAvailabilityIndex {
	return truthAvailabilityIndex{
		observationByPath:    truthBlockingObservationByPath(observationIssues),
		localReadBoundaries:  truthReadBoundaryIssues(observationIssues, true),
		remoteReadBoundaries: truthReadBoundaryIssues(observationIssues, false),
	}
}

// StatusForPath returns the raw local/remote truth availability for one path.
func (index truthAvailabilityIndex) StatusForPath(path string) pathTruthStatus {
	return pathTruthStatusForPath(path, index.observationByPath, index.localReadBoundaries, index.remoteReadBoundaries)
}

// StatusByPath returns the raw local/remote truth availability for a path set.
func (index truthAvailabilityIndex) StatusByPath(paths []string) map[string]pathTruthStatus {
	if len(paths) == 0 {
		return nil
	}

	statusByPath := make(map[string]pathTruthStatus, len(paths))
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
	localReadBoundaries []ObservationIssueRow,
	remoteReadBoundaries []ObservationIssueRow,
) pathTruthStatus {
	observationIssue, hasObservationIssue := observationByPath[path]
	localBoundary, hasLocalBoundary := mostSpecificObservationReadBoundary(path, localReadBoundaries)
	remoteBoundary, hasRemoteBoundary := mostSpecificObservationReadBoundary(path, remoteReadBoundaries)

	status := pathTruthStatus{
		Local:  availablePathTruthSideStatus(),
		Remote: availablePathTruthSideStatus(),
	}

	switch {
	case hasLocalBoundary:
		status.Local = pathTruthSideStatus{
			Availability: truthAvailabilityBlockedObservationIssue,
			Source:       pathTruthSourceObservationIssue,
			IssueType:    localBoundary.IssueType,
			ScopeKey:     localBoundary.ScopeKey,
		}
	case hasObservationIssue && observationIssueBlocksLocalTruth(observationIssue.IssueType):
		status.Local = pathTruthSideStatus{
			Availability: truthAvailabilityBlockedObservationIssue,
			Source:       pathTruthSourceObservationIssue,
			IssueType:    observationIssue.IssueType,
			ScopeKey:     observationIssue.ScopeKey,
		}
	}

	switch {
	case hasRemoteBoundary:
		status.Remote = pathTruthSideStatus{
			Availability: truthAvailabilityBlockedObservationIssue,
			Source:       pathTruthSourceObservationIssue,
			IssueType:    remoteBoundary.IssueType,
			ScopeKey:     remoteBoundary.ScopeKey,
		}
	case hasObservationIssue && observationIssueBlocksRemoteTruth(observationIssue.IssueType):
		status.Remote = pathTruthSideStatus{
			Availability: truthAvailabilityBlockedObservationIssue,
			Source:       pathTruthSourceObservationIssue,
			IssueType:    observationIssue.IssueType,
			ScopeKey:     observationIssue.ScopeKey,
		}
	}

	return status
}

func availablePathTruthSideStatus() pathTruthSideStatus {
	return pathTruthSideStatus{
		Availability: truthAvailabilityAvailable,
	}
}

func observationIssueBlocksTruth(issueType string) bool {
	return observationIssueBlocksLocalTruth(issueType) || observationIssueBlocksRemoteTruth(issueType)
}

func observationIssueBlocksLocalTruth(issueType string) bool {
	switch issueType {
	case IssueInvalidFilename,
		issuePathTooLong,
		issueFileTooLarge,
		issueCaseCollision,
		issueHashPanic,
		issueLocalReadDenied:
		return true
	default:
		return false
	}
}

func observationIssueBlocksRemoteTruth(issueType string) bool {
	return issueType == issueRemoteReadDenied
}

func truthReadBoundaryIssues(observationIssues []ObservationIssueRow, local bool) []ObservationIssueRow {
	if len(observationIssues) == 0 {
		return nil
	}

	var boundaries []ObservationIssueRow
	for i := range observationIssues {
		switch {
		case local && observationIssues[i].ScopeKey.IsPermLocalRead():
			boundaries = append(boundaries, observationIssues[i])
		case !local && observationIssues[i].ScopeKey.IsPermRemoteRead():
			boundaries = append(boundaries, observationIssues[i])
		}
	}

	return boundaries
}

func mostSpecificObservationReadBoundary(
	path string,
	boundaries []ObservationIssueRow,
) (ObservationIssueRow, bool) {
	best := ObservationIssueRow{}
	bestLen := -1

	for i := range boundaries {
		scopePath := boundaries[i].ScopeKey.CoveredPath()
		if !scopePathMatches(path, scopePath) {
			continue
		}
		if len(scopePath) > bestLen {
			best = boundaries[i]
			bestLen = len(scopePath)
		}
	}

	return best, bestLen >= 0
}
