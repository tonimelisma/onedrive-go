package syncdomain

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.5.6
func TestIssueType_EveryDeclaredConstantIsAMemberAndParses(t *testing.T) {
	t.Parallel()

	members := issueTypeConstantsInSource(t)
	seen := make(map[IssueType]struct{}, len(members))
	for _, member := range members {
		_, duplicate := seen[member]
		assert.False(t, duplicate, "two IssueType constants share the value %q", member)
		seen[member] = struct{}{}

		assert.True(t, member.IsMember(), "%q is declared but IsMember does not name it", member)
		parsed, err := ParseIssueType(string(member))
		require.NoError(t, err)
		assert.Equal(t, member, parsed)
	}
}

// Validates: R-6.6.28
//
// The one-shot exit verdict of every issue type: a drive whose store still
// holds an observation issue of a type that leaves a file unsynced has work
// left, and its one-shot sync exits 1. Only a deliberate hold, or a report
// that keeps no file from syncing, is not counted. The groups must name every
// declared member, so a new member is decided here as well as in the
// exhaustive switch.
func TestIssueTypes_EveryTypeIsClassifiedForOneShotExit(t *testing.T) {
	t.Parallel()

	requireEveryIssueTypeDecided(t, issueTypeVerdicts(t,
		issueTypeVerdictGroup[bool]{
			// Deletes held for `sync --confirm-deletes` are a deliberate hold.
			// The leftover of an interrupted removal sits at an internal name
			// sync never syncs; its report asks the user to look at it.
			verdict: false,
			members: []IssueType{IssueDeletesHeld, IssueLocalInterruptedRemoval, IssueRemovedFolderKept},
		},
		issueTypeVerdictGroup[bool]{
			// The scan found a path sync cannot represent, read or reach.
			verdict: true,
			members: []IssueType{
				IssueInvalidFilename, IssuePathTooLong, IssueFileTooLarge, IssueHashError, IssueCaseCollision,
				IssueLocalReadDenied, IssueLocalUnsyncableReplacement, IssueLocalMountBoundary, IssueLocalMountMissing,
				IssueRemoteNameTwin, IssueRemoteReadDenied, IssueRemoteRootNotFound,
			},
		},
		issueTypeVerdictGroup[bool]{
			// A refusal or a condition that holds the work back until it clears.
			verdict: true,
			members: []IssueType{
				IssueUnauthorized, IssueRemoteWriteDenied, IssueQuotaExceeded, IssueRateLimited,
				IssueLocalWriteDenied, IssueDiskFull, IssueServiceOutage, IssueFileTooLargeForSpace,
				IssueLocalNameTooLong, IssueLocalReadOnly, IssueRemoteFolderUnverifiable, IssueLocalFileTooLarge,
				IssueLocalBusy, IssueLocalCrossDevice, IssueUnsupportedOperation, IssueMalwareDetected,
				IssueLocalDatabaseOpen, IssueUploadVerification, IssueDownloadIntegrity,
			},
		},
		issueTypeVerdictGroup[bool]{
			// A transient failure of work not done; no store keeps one as an
			// observation issue, so this verdict only decides the case.
			verdict: true,
			members: []IssueType{
				IssueRequestTimeout, IssueTransientConflict, IssueTransientNotFound, IssueResourceLocked,
				IssueTransferURLRejected,
			},
		},
	), IssueTypeLeavesFileUnsynced)

	assert.True(t, IssueTypeLeavesFileUnsynced(issueTypeOutsideTheEnum),
		"a value outside the enum is not known to be done, so it counts")
}

// Validates: R-2.4.8, R-2.10.4
//
// Only OneDrive's two refusals to read the root, "forbidden" and "not found",
// make a whole tree unreadable. The groups must name every declared member,
// so a new member is decided here as well as in the exhaustive switch.
func TestIssueTypeIsRootReadFailure_EveryMemberIsDecided(t *testing.T) {
	t.Parallel()

	requireEveryIssueTypeDecided(t, issueTypeVerdicts(t,
		issueTypeVerdictGroup[bool]{
			// The two answers rootReadFailureIssueType records at the root.
			verdict: true,
			members: []IssueType{IssueRemoteReadDenied, IssueRemoteRootNotFound},
		},
		issueTypeVerdictGroup[bool]{
			// What a scan finds, or a refusal of one path's work.
			verdict: false,
			members: []IssueType{
				IssueInvalidFilename, IssuePathTooLong, IssueFileTooLarge, IssueHashError, IssueCaseCollision,
				IssueLocalReadDenied, IssueLocalUnsyncableReplacement, IssueLocalMountBoundary, IssueLocalMountMissing,
				IssueRemoteNameTwin, IssueUnauthorized, IssueRemoteWriteDenied, IssueQuotaExceeded, IssueRateLimited,
				IssueLocalWriteDenied, IssueDiskFull, IssueServiceOutage, IssueFileTooLargeForSpace,
				IssueLocalNameTooLong, IssueLocalReadOnly, IssueRemoteFolderUnverifiable, IssueLocalFileTooLarge,
				IssueLocalBusy, IssueLocalCrossDevice, IssueUnsupportedOperation, IssueMalwareDetected,
				IssueLocalDatabaseOpen, IssueUploadVerification, IssueDownloadIntegrity,
			},
		},
		issueTypeVerdictGroup[bool]{
			// A deliberate hold, or what a removal kept or set aside.
			verdict: false,
			members: []IssueType{IssueDeletesHeld, IssueLocalInterruptedRemoval, IssueRemovedFolderKept},
		},
		issueTypeVerdictGroup[bool]{
			// A transient failure no store keeps as an observation issue.
			verdict: false,
			members: []IssueType{
				IssueRequestTimeout, IssueTransientConflict, IssueTransientNotFound, IssueResourceLocked,
				IssueTransferURLRejected,
			},
		},
	), IssueTypeIsRootReadFailure)

	assert.False(t, IssueTypeIsRootReadFailure(issueTypeOutsideTheEnum), "a value outside the enum is not")
}

// Validates: R-2.5.6
func TestParseIssueType_RefusesAValueNoMemberHas(t *testing.T) {
	t.Parallel()

	for _, stored := range []string{"", "invalid_name", "Invalid_Filename", "hash_failed"} {
		parsed, err := ParseIssueType(stored)
		require.ErrorContains(t, err, "unknown issue type "+strconv.Quote(stored))
		assert.Empty(t, parsed)
		assert.False(t, IssueType(stored).IsMember(), "%q", stored)
	}
}
