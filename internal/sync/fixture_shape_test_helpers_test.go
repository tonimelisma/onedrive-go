package sync

import (
	"time"
)

func testDelayedRetryWorkRow(
	path string,
	oldPath string,
	actionType ActionType,
	attemptCount int,
	nextRetryAt int64,
) *RetryWorkRow {
	return &RetryWorkRow{
		Path:         path,
		OldPath:      oldPath,
		ActionType:   actionType,
		AttemptCount: attemptCount,
		NextRetryAt:  nextRetryAt,
	}
}

func testBlockScope(
	scopeKey ScopeKey,
	trialInterval time.Duration,
	nextTrialAt time.Time,
) *BlockScope {
	return &BlockScope{
		Key:           scopeKey,
		TrialInterval: trialInterval,
		NextTrialAt:   nextTrialAt,
	}
}
