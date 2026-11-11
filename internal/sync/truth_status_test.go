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
// issue names. The table must name every declared member, so a new member
// gets a deliberate verdict here as well as in the exhaustive switch.
// Validates: R-6.2.14, R-6.2.15
func TestObservationIssueBlocksTruth_VerdictForEveryIssueType(t *testing.T) {
	t.Parallel()

	type verdict struct{ local, remote bool }
	verdicts := map[IssueType]verdict{
		// The scan found the path but cannot represent it: unknown, not absent.
		IssueInvalidFilename:            {local: true},
		issuePathTooLong:                {local: true},
		issueFileTooLarge:               {local: true},
		issueCaseCollision:              {local: true},
		issueHashPanic:                  {local: true},
		issueLocalReadDenied:            {local: true},
		issueLocalUnsyncableReplacement: {local: true},
		issueLocalMountBoundary:         {local: true},
		// OneDrive's side of the path is the unknown one.
		issueRemoteReadDenied: {remote: true},
		issueRemoteNameTwin:   {remote: true},
		// An internal name the content filter hides from planning on both
		// sides: no synced path reads as deleted through it.
		issueLocalInterruptedRemoval: {},
		// Runtime conditions that retry_work or a block scope carries;
		// observation never stores them.
		IssueUnauthorized:             {},
		IssueRemoteWriteDenied:        {},
		IssueQuotaExceeded:            {},
		issueRateLimited:              {},
		issueLocalWriteDenied:         {},
		issueDiskFull:                 {},
		issueServiceOutage:            {},
		issueFileTooLargeForSpace:     {},
		issueLocalNameTooLong:         {},
		issueLocalReadOnly:            {},
		issueDeletesHeld:              {},
		issueRemoteFolderUnverifiable: {},
		issueLocalFileTooLarge:        {},
		issueLocalBusy:                {},
		issueLocalCrossDevice:         {},
		issueUnsupportedOperation:     {},
		issueMalwareDetected:          {},
		issueLocalDatabaseOpen:        {},
		issueUploadVerification:       {},
		issueDownloadIntegrity:        {},
		issueRequestTimeout:           {},
		issueTransientConflict:        {},
		issueTransientNotFound:        {},
		issueResourceLocked:           {},
		issueTransferURLRejected:      {},
	}

	members := issueTypeConstantsInSource(t)
	require.Len(t, verdicts, len(members), "the table must give every IssueType member a verdict")
	for _, member := range members {
		want, ok := verdicts[member]
		require.True(t, ok, "no verdict for %q", member)
		assert.Equal(t, want.local, observationIssueBlocksLocalTruth(member), "local truth under %q", member)
		assert.Equal(t, want.remote, observationIssueBlocksRemoteTruth(member), "remote truth under %q", member)
	}
}

// A value outside the enum (the store refuses one, so only a programming
// error makes it) leaves the path's local truth unavailable rather than
// readable as absent. Before the type, the switch's default said "not
// blocking" for any string it did not list.
// Validates: R-6.2.14
func TestObservationIssueBlocksLocalTruth_ValueOutsideTheEnumIsUnavailable(t *testing.T) {
	t.Parallel()

	const outsideTheEnum IssueType = "not_an_issue_type"
	require.False(t, outsideTheEnum.isMember())
	assert.True(t, observationIssueBlocksLocalTruth(outsideTheEnum))

	status := newTruthAvailabilityIndex([]ObservationIssueRow{
		{Path: "docs/report.txt", IssueType: outsideTheEnum},
	}).StatusForPath("docs/report.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, status.Local.Availability)
	assert.Equal(t, outsideTheEnum, status.Local.IssueType)
	assert.True(t, status.Remote.IsAvailable())
}
