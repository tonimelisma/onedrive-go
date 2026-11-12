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

func observationIssueBlocksTruth(issueType IssueType) bool {
	verdict := truthVerdictForIssueType(issueType)
	return verdict.blocksLocal || verdict.blocksRemote
}

// observationIssueTruthVerdict is what an observation issue of one type says
// about the truth of the path it names: which side of the path is unknown
// rather than absent, and whether OneDrive's side of everything beneath the
// path is unknown too. A read scope key on the issue can bound a subtree as
// well (truthReadBoundaryIssues); this verdict is what the type alone says.
type observationIssueTruthVerdict struct {
	blocksLocal         bool
	blocksRemote        bool
	boundsRemoteSubtree bool
}

// truthVerdictForIssueType decides every truth verdict of an issue type in
// one switch that names every IssueType (the exhaustive linter holds it to
// that), so a new member is decided on both sides at once instead of falling
// through an equality test that did not name it.
//
// The last member group blocks nothing: local_interrupted_removal sits at an
// internal name the content filter hides from planning on both sides, so no
// synced path can read as deleted through it; every other member there is a
// runtime condition that retry_work or a block scope carries and observation
// never stores.
//
// A value outside the enum cannot come from the store, which refuses an
// unknown stored issue type (parseIssueType). Should one appear anyway, its
// path is unavailable on both sides: unknown is not absent (D-005). It bounds
// no subtree by its type, because nothing says what it would cover; the
// subtree boundaries are read scope keys and remote twins.
func truthVerdictForIssueType(issueType IssueType) observationIssueTruthVerdict {
	switch issueType {
	case IssueInvalidFilename,
		issuePathTooLong,
		issueFileTooLarge,
		issueCaseCollision,
		issueHashPanic,
		issueLocalReadDenied,
		issueLocalUnsyncableReplacement,
		issueLocalMountBoundary:
		// The scan found the path but cannot represent it.
		return observationIssueTruthVerdict{blocksLocal: true}
	case issueRemoteReadDenied:
		// A folder's subtree is bounded by the issue's read scope key.
		return observationIssueTruthVerdict{blocksRemote: true}
	case issueRemoteNameTwin:
		// OneDrive keeps two items under the path; the twin has no scope key,
		// so its type is what bounds the subtree it holds twice.
		return observationIssueTruthVerdict{blocksRemote: true, boundsRemoteSubtree: true}
	case issueLocalInterruptedRemoval,
		IssueUnauthorized,
		IssueRemoteWriteDenied,
		IssueQuotaExceeded,
		issueRateLimited,
		issueLocalWriteDenied,
		issueDiskFull,
		issueServiceOutage,
		issueFileTooLargeForSpace,
		issueLocalNameTooLong,
		issueLocalReadOnly,
		issueDeletesHeld,
		issueRemoteFolderUnverifiable,
		issueLocalFileTooLarge,
		issueLocalBusy,
		issueLocalCrossDevice,
		issueUnsupportedOperation,
		issueMalwareDetected,
		issueLocalDatabaseOpen,
		issueUploadVerification,
		issueDownloadIntegrity,
		issueRequestTimeout,
		issueTransientConflict,
		issueTransientNotFound,
		issueResourceLocked,
		issueTransferURLRejected:
		return observationIssueTruthVerdict{}
	default:
		return observationIssueTruthVerdict{blocksLocal: true, blocksRemote: true}
	}
}

// observationIssueBlocksLocalTruth reports whether an observation issue at a
// path makes that path's local truth unavailable, so the planner reads the
// path as unknown rather than absent (truthVerdictForIssueType).
func observationIssueBlocksLocalTruth(issueType IssueType) bool {
	return truthVerdictForIssueType(issueType).blocksLocal
}

// observationIssueBlocksRemoteTruth reports whether an observation issue at a
// path makes that path's OneDrive truth unavailable (truthVerdictForIssueType).
func observationIssueBlocksRemoteTruth(issueType IssueType) bool {
	return truthVerdictForIssueType(issueType).blocksRemote
}

func truthReadBoundaryIssues(observationIssues []ObservationIssueRow, local bool) []ObservationIssueRow {
	if len(observationIssues) == 0 {
		return nil
	}

	var boundaries []ObservationIssueRow
	for i := range observationIssues {
		issue := observationIssues[i]
		switch {
		case local && issue.ScopeKey.IsPermLocalRead():
			boundaries = append(boundaries, issue)
		case !local && (issue.ScopeKey.IsPermRemoteRead() || truthVerdictForIssueType(issue.IssueType).boundsRemoteSubtree):
			boundaries = append(boundaries, issue)
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
		if truthVerdictForIssueType(boundaries[i].IssueType).boundsRemoteSubtree {
			// A type that bounds the subtree by itself has no scope key: it
			// covers its own path and everything beneath it.
			scopePath = boundaries[i].Path
		}
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
