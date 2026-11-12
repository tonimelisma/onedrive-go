package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusByPath_ReturnsAvailableStatusForUnblockedPaths(t *testing.T) {
	t.Parallel()

	statuses := newTruthAvailabilityIndex(nil).StatusByPath([]string{"docs/readme.txt"})
	require.Len(t, statuses, 1)

	status, ok := statuses["docs/readme.txt"]
	require.True(t, ok)
	assert.True(t, status.Local.IsAvailable())
	assert.True(t, status.Remote.IsAvailable())
}

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusByPath_ReadBoundariesApplyToDescendants(t *testing.T) {
	t.Parallel()

	statuses := newTruthAvailabilityIndex(
		[]ObservationIssueRow{
			{Path: "Private", IssueType: issueLocalReadDenied, ScopeKey: sKPermLocalRead("Private")},
			{Path: "Shared", IssueType: issueRemoteReadDenied, ScopeKey: sKPermRemoteRead("Shared")},
		},
	).StatusByPath(
		[]string{"Private/sub/file.txt", "Shared/Docs/file.txt"},
	)

	localStatus, ok := statuses["Private/sub/file.txt"]
	require.True(t, ok)
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, localStatus.Local.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, localStatus.Local.Source)
	assert.Equal(t, sKPermLocalRead("Private"), localStatus.Local.ScopeKey)
	assert.True(t, localStatus.Remote.IsAvailable())

	remoteStatus, ok := statuses["Shared/Docs/file.txt"]
	require.True(t, ok)
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, remoteStatus.Remote.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, remoteStatus.Remote.Source)
	assert.Equal(t, sKPermRemoteRead("Shared"), remoteStatus.Remote.ScopeKey)
	assert.True(t, remoteStatus.Local.IsAvailable())
}

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusForPath_UsesObservationEvidence(t *testing.T) {
	t.Parallel()

	index := newTruthAvailabilityIndex(
		[]ObservationIssueRow{
			{Path: "blocked-local.txt", IssueType: IssueInvalidFilename},
			{Path: "Shared", IssueType: issueRemoteReadDenied, ScopeKey: sKPermRemoteRead("Shared")},
		},
	)

	localStatus := index.StatusForPath("blocked-local.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, localStatus.Local.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, localStatus.Local.Source)
	assert.Equal(t, IssueInvalidFilename, localStatus.Local.IssueType)
	assert.True(t, localStatus.Remote.IsAvailable())

	remoteStatus := index.StatusForPath("Shared/Docs/file.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, remoteStatus.Remote.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, remoteStatus.Remote.Source)
	assert.Equal(t, sKPermRemoteRead("Shared"), remoteStatus.Remote.ScopeKey)
	assert.True(t, remoteStatus.Local.IsAvailable())
}

// The verdict of every issue type on the truth of the path its observation
// issue names, and on OneDrive's truth beneath it. The groups must name every
// declared member, so a new member gets a deliberate verdict here as well as
// in the exhaustive switch.
// Validates: R-6.2.14, R-6.2.15, R-2.12.6
func TestObservationIssueBlocksTruth_VerdictForEveryIssueType(t *testing.T) {
	t.Parallel()

	requireEveryIssueTypeDecided(t, issueTypeVerdicts(t,
		issueTypeVerdictGroup[observationIssueTruthVerdict]{
			// The scan found the path but cannot represent it: unknown, not absent.
			verdict: observationIssueTruthVerdict{blocksLocal: true},
			members: []IssueType{
				IssueInvalidFilename, issuePathTooLong, issueFileTooLarge, issueCaseCollision,
				issueHashPanic, issueLocalReadDenied, issueLocalUnsyncableReplacement, issueLocalMountBoundary,
			},
		},
		issueTypeVerdictGroup[observationIssueTruthVerdict]{
			// OneDrive's side of the path is the unknown one; a folder's subtree
			// is covered by the issue's read scope key, not by its type.
			verdict: observationIssueTruthVerdict{blocksRemote: true},
			members: []IssueType{issueRemoteReadDenied},
		},
		issueTypeVerdictGroup[observationIssueTruthVerdict]{
			// OneDrive keeps two items under the path, so its side of the path
			// and of everything beneath it is unknown; a twin has no scope key.
			verdict: observationIssueTruthVerdict{blocksRemote: true, boundsRemoteSubtree: true},
			members: []IssueType{issueRemoteNameTwin},
		},
		issueTypeVerdictGroup[observationIssueTruthVerdict]{
			// An internal name the content filter hides from planning on both
			// sides, and runtime conditions that retry_work or a block scope
			// carries and observation never stores.
			verdict: observationIssueTruthVerdict{},
			members: []IssueType{
				issueLocalInterruptedRemoval, IssueUnauthorized, IssueRemoteWriteDenied, IssueQuotaExceeded,
				issueRateLimited, issueLocalWriteDenied, issueDiskFull, issueServiceOutage,
				issueFileTooLargeForSpace, issueLocalNameTooLong, issueLocalReadOnly, issueDeletesHeld,
				issueRemoteFolderUnverifiable, issueLocalFileTooLarge, issueLocalBusy, issueLocalCrossDevice,
				issueUnsupportedOperation, issueMalwareDetected, issueLocalDatabaseOpen, issueUploadVerification,
				issueDownloadIntegrity, issueRequestTimeout, issueTransientConflict, issueTransientNotFound,
				issueResourceLocked, issueTransferURLRejected,
			},
		},
	), func(member IssueType) observationIssueTruthVerdict {
		verdict := truthVerdictForIssueType(member)
		assert.Equal(t, verdict.blocksLocal, observationIssueBlocksLocalTruth(member), "local truth under %q", member)
		assert.Equal(t, verdict.blocksRemote, observationIssueBlocksRemoteTruth(member), "remote truth under %q", member)

		return verdict
	})

	assert.Equal(t, observationIssueTruthVerdict{blocksLocal: true, blocksRemote: true},
		truthVerdictForIssueType(issueTypeOutsideTheEnum),
		"a value outside the enum is unknown on both sides of its path; no read scope key says what subtree it covers")
}

// A remote twin bounds OneDrive's truth of its path and everything beneath
// it by its type, with no scope key; an issue whose type does not bound a
// subtree blocks only its own path.
// Validates: R-2.12.6, R-6.2.14
func TestTruthAvailabilityIndex_OnlyASubtreeBoundingTypeBlocksDescendants(t *testing.T) {
	t.Parallel()

	index := newTruthAvailabilityIndex([]ObservationIssueRow{
		{Path: "Twins", IssueType: issueRemoteNameTwin},
		{Path: "Odd", IssueType: issueTypeOutsideTheEnum},
	})

	twinChild := index.StatusForPath("Twins/inner/file.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, twinChild.Remote.Availability)
	assert.Equal(t, issueRemoteNameTwin, twinChild.Remote.IssueType)
	assert.True(t, twinChild.Local.IsAvailable())

	oddChild := index.StatusForPath("Odd/file.txt")
	assert.True(t, oddChild.Remote.IsAvailable())
	assert.True(t, oddChild.Local.IsAvailable())
}

// A value outside the enum (the store refuses one, so only a programming
// error makes it) leaves the path's local truth unavailable rather than
// readable as absent. Before the type, the switch's default said "not
// blocking" for any string it did not list.
// Validates: R-6.2.14
func TestObservationIssueBlocksLocalTruth_ValueOutsideTheEnumIsUnavailable(t *testing.T) {
	t.Parallel()

	const outsideTheEnum IssueType = issueTypeOutsideTheEnum
	require.False(t, outsideTheEnum.isMember())
	assert.True(t, observationIssueBlocksLocalTruth(outsideTheEnum))

	status := newTruthAvailabilityIndex([]ObservationIssueRow{
		{Path: "docs/report.txt", IssueType: outsideTheEnum},
	}).StatusForPath("docs/report.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, status.Local.Availability)
	assert.Equal(t, outsideTheEnum, status.Local.IssueType)
}

// The same value leaves the path's OneDrive truth unavailable too. Before,
// remote truth was blocked by two equality tests, so any value they did not
// name read as "not blocking": a remote side that might be absent for a
// reason no member names was planned from as if it were known.
// Validates: R-6.2.14
func TestObservationIssueBlocksRemoteTruth_ValueOutsideTheEnumIsUnavailable(t *testing.T) {
	t.Parallel()

	const outsideTheEnum IssueType = issueTypeOutsideTheEnum
	require.False(t, outsideTheEnum.isMember())
	assert.True(t, observationIssueBlocksRemoteTruth(outsideTheEnum))

	index := newTruthAvailabilityIndex([]ObservationIssueRow{
		{Path: "docs/report.txt", IssueType: outsideTheEnum},
	})
	status := index.StatusForPath("docs/report.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, status.Remote.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, status.Remote.Source)
	assert.Equal(t, outsideTheEnum, status.Remote.IssueType)
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, status.Local.Availability)

	sibling := index.StatusForPath("docs/other.txt")
	assert.True(t, sibling.Local.IsAvailable(), "the value blocks the path it names, not its neighbors")
	assert.True(t, sibling.Remote.IsAvailable())
}
