package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// Validates: R-2.8.10
//
// A folder too large to verify before its recursive delete does not clear by
// itself, and every retry relists up to 10,000 items before failing the same
// way. It used to retry on the transient curve, from one second, so a watch
// session relisted the folder on every pass.
func TestRetryDelayForCondition_ConditionsThatDoNotClearByThemselvesWaitOnTheDurableCurve(t *testing.T) {
	t.Parallel()

	for _, condition := range []IssueType{issueCaseCollision, issueRemoteFolderUnverifiable} {
		delay := retryDelayForCondition(condition)
		assert.Equal(t, durableRecheckDelay, delay(0), condition)
		assert.Equal(t, 2*durableRecheckDelay, delay(1), condition)
		assert.Equal(t, time.Hour, delay(20), "%s caps at an hour", condition)
	}

	transient := retryDelayForCondition(issueRequestTimeout)
	assert.Less(t, transient(0), durableRecheckDelay, "a condition that clears by itself keeps the transient curve, which starts at a second")
	assert.LessOrEqual(t, transient(0), 2*retry.ReconcilePolicy().Delay(0)+time.Second)
}

// The curve of every condition. The table must name every declared member,
// so a new member is put on a curve on purpose: a condition only the user
// clears that lands on the one-second curve is retried in a hot loop, which
// is how a too-large folder was relisted on every pass. A value outside the
// enum is not known to clear by itself, so it waits on the durable curve; it
// used to take the transient one.
// Validates: R-2.10.71, R-2.8.10
func TestRetryDelayForCondition_EveryMemberIsDecided(t *testing.T) {
	t.Parallel()

	waitsOnTheDurableCurve := func(condition IssueType) bool {
		return retryDelayForCondition(condition)(0) == durableRecheckDelay
	}

	requireEveryIssueTypeDecided(t, issueTypeVerdicts(t,
		issueTypeVerdictGroup[bool]{
			verdict: true,
			members: []IssueType{issueCaseCollision, issueRemoteFolderUnverifiable},
		},
		issueTypeVerdictGroup[bool]{
			// Every other condition clears by itself, is held under its block
			// scope until the scope's trial passes, is recorded as
			// ClassActionable, which retryDelayForDecision already puts on the
			// durable curve, or never becomes retry work.
			verdict: false,
			members: []IssueType{
				IssueInvalidFilename, issuePathTooLong, issueFileTooLarge, issueHashPanic,
				IssueUnauthorized, IssueRemoteWriteDenied, issueRemoteReadDenied, IssueQuotaExceeded,
				issueRateLimited, issueLocalReadDenied, issueLocalWriteDenied, issueRemoteNameTwin,
				issueDiskFull, issueServiceOutage, issueFileTooLargeForSpace, issueLocalNameTooLong,
				issueLocalReadOnly, issueDeletesHeld, issueLocalUnsyncableReplacement, issueLocalMountBoundary,
				issueLocalInterruptedRemoval, issueRemovedFolderKept, issueLocalFileTooLarge, issueLocalBusy, issueLocalCrossDevice,
				issueUnsupportedOperation, issueMalwareDetected, issueLocalDatabaseOpen, issueUploadVerification,
				issueDownloadIntegrity, issueRequestTimeout, issueTransientConflict, issueTransientNotFound,
				issueResourceLocked, issueTransferURLRejected,
			},
		},
	), waitsOnTheDurableCurve)

	assert.True(t, waitsOnTheDurableCurve(issueTypeOutsideTheEnum), "a value outside the enum")
	assert.False(t, waitsOnTheDurableCurve(""), "a failure with no condition is transient")
}
