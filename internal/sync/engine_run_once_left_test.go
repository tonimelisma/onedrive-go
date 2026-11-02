package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.6.19, R-6.8.24
//
// Live, 2026-11-02: a one-shot confirm of 6,000 deletes met a 429 after
// 5,557; the scope took the remaining 442 back for a trial after the pass
// had ended, and the summary said "1 will be retried", counting only the
// failed one. The report now counts the planned work the pass neither
// completed nor failed, and a converged pass counts none.
func TestRunOnce_ReportCountsTheWorkAThrottleLeftForTheNextSync(t *testing.T) {
	t.Parallel()

	const files = 40
	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	clock := newManualClock(time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC))
	limiter := newThrottleWindowClient(clock.Now, 10, time.Second)
	eng, syncRoot := newTestEngine(t, limiter.wrap(remote.client(t)))
	eng.transferWorkers = 4
	installManualClockForTestEngine(eng, clock)
	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("bulk/file-%02d.txt", i), fmt.Sprintf("content %d\n", i))
	}

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	planned := report.FolderCreates + report.Uploads
	require.Less(t, report.Succeeded, planned, "the window of 10 requests cannot carry the whole plan")
	assert.Greater(t, report.LeftForNextSync, 0, "the 429 took work back for a trial after the pass")
	assert.Equal(t, planned-report.Succeeded-report.Failed, report.LeftForNextSync,
		"everything planned is completed, failed or left for the next sync")

	for i := 0; i < 40 && report.LeftForNextSync+report.Failed > 0; i++ {
		clock.Advance(time.Second)
		report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}
	assert.Zero(t, report.LeftForNextSync, "a converged pass leaves nothing")
	assert.Equal(t, files, remote.uploads, "every upload completes exactly once")
	quiet, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, quiet.LeftForNextSync)
	assert.Zero(t, quiet.Succeeded)
}
