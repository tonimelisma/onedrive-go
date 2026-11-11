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

	transient := retryDelayForCondition(issueRemoteFolderUnverifiable + "-other")
	assert.Less(t, transient(0), durableRecheckDelay, "any other condition keeps the transient curve, which starts at a second")
	assert.LessOrEqual(t, transient(0), 2*retry.ReconcilePolicy().Delay(0)+time.Second)
}
