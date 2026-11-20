package syncfixture

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func TestDelayedRetryWorkRow(
	path string,
	oldPath string,
	actionType syncdomain.ActionType,
	attemptCount int,
	nextRetryAt int64,
) *syncdomain.RetryWorkRow {
	return &syncdomain.RetryWorkRow{
		Path:         path,
		OldPath:      oldPath,
		ActionType:   actionType,
		AttemptCount: attemptCount,
		NextRetryAt:  nextRetryAt,
	}
}

func TestBlockScope(
	scopeKey syncdomain.ScopeKey,
	trialInterval time.Duration,
	nextTrialAt time.Time,
) *syncdomain.BlockScope {
	return &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: trialInterval,
		NextTrialAt:   nextTrialAt,
	}
}
