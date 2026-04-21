package sync

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.6.7
func TestEngineFlow_LogFailureSummary_AggregatesRetryWorkWarnings(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)

	for i := range 11 {
		flow.recordError(&ResultDecision{
			Persistence:   persistRetryWork,
			ConditionType: IssueServiceOutage,
		}, &ActionCompletion{
			Path:   fmt.Sprintf("retry-%02d.txt", i),
			Err:    errors.New("boom"),
			ErrMsg: "boom",
		})
	}

	succeeded, failed, errs := flow.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 11, failed)
	require.Len(t, errs, 11)

	flow.logFailureSummary()

	output := logBuf.String()
	assert.Contains(t, output, "msg=\"sync retry work (aggregated)\"")
	assert.Equal(t, 11, strings.Count(output, "msg=\"sync retry work\""))
	assert.Contains(t, output, "condition_type=service_outage")
	assert.Contains(t, output, "retry-00.txt")
	assert.Contains(t, output, "retry-10.txt")

	flow.resetResultStats()
	succeeded, failed, errs = flow.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 0, failed)
	assert.Empty(t, errs)
}

// Validates: R-2.10.33
func TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	now := eng.nowFn()
	rt.holdAction(&TrackedAction{
		Action: Action{
			Path: "due.txt",
			Type: ActionUpload,
		},
		ID: 1,
	}, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))

	rt.armRetryTimer()

	select {
	case <-rt.retryTimerCh:
	default:
		require.FailNow(t, "retry timer should kick immediately for due retry_work")
	}
}

// Validates: R-2.10.33
func TestWatchRuntime_ArmRetryTimer_FiresAfterDelay(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	clock := newManualClock(eng.nowFn())
	installManualClock(eng.Engine, clock)
	rt := testWatchRuntime(t, eng)
	now := clock.Now()

	rt.holdAction(&TrackedAction{
		Action: Action{
			Path: "later.txt",
			Type: ActionUpload,
		},
		ID: 1,
	}, heldReasonRetry, ScopeKey{}, now.Add(30*time.Second))

	rt.armRetryTimer()

	select {
	case <-rt.retryTimerCh:
		require.FailNow(t, "retry timer should not fire before the scheduled delay")
	default:
	}

	clock.Advance(29 * time.Second)
	select {
	case <-rt.retryTimerCh:
		require.FailNow(t, "retry timer should still be waiting before the deadline")
	default:
	}

	clock.Advance(1 * time.Second)
	select {
	case <-rt.retryTimerCh:
	default:
		require.FailNow(t, "retry timer should fire once the delay elapses")
	}
}
