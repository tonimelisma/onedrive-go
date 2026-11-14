package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.7
//
// A 429 pauses the drive and ends the pass with the remaining work taken
// back. The pause is usually seconds, so a one-shot sync that reported that
// work as "left for the next sync" (443 of 6,000 deletes after a 44 s pause,
// soak of 2026-11-02) made the user run it again by hand. The run now waits
// for the scope's trial and passes again, within the pass bound.
func TestRunOnce_WaitsOutAShortPauseBeforeTheNextPass(t *testing.T) {
	t.Parallel()

	// Twelve files against a window of ten requests: the first pass is
	// throttled, the second finishes, within the four-pass bound.
	const files = 12
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	clock := newManualClock(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	const window = 2 * time.Second
	limiter := newThrottleWindowClient(clock.Now, 10, window)
	eng, syncRoot := newTestEngine(t, limiter.wrap(remote.client(t)))
	eng.transferWorkers = 4
	installManualClockForTestEngine(eng, clock)
	var waits []time.Duration
	eng.waitFn = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		clock.Advance(d)

		return nil
	}
	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("bulk/file-%02d.txt", i), fmt.Sprintf("content %d", i))
	}

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.LeftForNextSync, "the pauses were waited out inside the run: %+v", report)
	assert.Equal(t, files, remote.uploads, "every upload completed in one run")
	require.NotEmpty(t, waits, "the run waited for the throttle's Retry-After")
	for _, wait := range waits {
		// The throttle lengthens its penalty for every request refused
		// inside it, so a wait can be a few windows; never beyond the limit.
		assert.Positive(t, wait)
		assert.LessOrEqual(t, wait, oneShotScopeWaitLimit, "a wait is at most the limit a sync waits")
	}
}

// Validates: R-2.1.7, R-6.6.28
//
// A pause longer than a sync waits (ten minutes) is the next sync's: the run
// reports the work left and returns without sleeping.
func TestRunOnce_LeavesWorkBehindAPauseLongerThanTenMinutes(t *testing.T) {
	t.Parallel()

	const files = 20
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	clock := newManualClock(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	limiter := newThrottleWindowClient(clock.Now, 10, 20*time.Minute)
	eng, syncRoot := newTestEngine(t, limiter.wrap(remote.client(t)))
	eng.transferWorkers = 4
	installManualClockForTestEngine(eng, clock)
	waited := 0
	eng.waitFn = func(_ context.Context, d time.Duration) error {
		waited++
		clock.Advance(d)

		return nil
	}
	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("bulk/file-%02d.txt", i), fmt.Sprintf("content %d", i))
	}

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Positive(t, report.LeftForNextSync, "a twenty-minute pause is not waited out: %+v", report)
	assert.Zero(t, waited)
	assert.Less(t, remote.uploads, files)

	// The work the pause holds is still in the store when the run ends, and
	// each of its rows is an action the report already counts as failed or
	// left for the next sync (R-6.6.28).
	blocked, err := eng.baseline.ListBlockedRetryWork(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, blocked, "the pause holds blocked work")
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.LessOrEqual(t, len(rows), report.LastPassFailed+report.LeftForNextSync)
}
