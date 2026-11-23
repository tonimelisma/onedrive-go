//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordQuirkEvent keeps event in the suite's quirk recorder.
func recordQuirkEvent(t *testing.T, event *e2eQuirkEvent) {
	t.Helper()
	suiteQuirkRecorder.recordFor(t, event)
}

// recordCLIQuirkEvents keeps, in the suite's quirk recorder, the Graph
// quirks a CLI run's --debug stderr logs.
func recordCLIQuirkEvents(t *testing.T, args []string, stderr string, cmdErr error) {
	t.Helper()
	suiteQuirkRecorder.recordCLIEvents(t, args, stderr, cmdErr)
}

// recordAuthPreflightDecisionEvent keeps an auth preflight's retried attempts
// in the suite's quirk recorder.
func recordAuthPreflightDecisionEvent(
	t *testing.T,
	driveID string,
	endpoint string,
	attempts []authPreflightAttempt,
	finalErr error,
) {
	t.Helper()
	suiteQuirkRecorder.recordAuthPreflightDecision(t, driveID, endpoint, attempts, finalErr)
}

// recordLiveProviderRecurrenceEvent keeps a known live provider recurrence
// in the suite's quirk recorder.
func recordLiveProviderRecurrenceEvent(
	t *testing.T,
	operation string,
	decision liveProviderRecurrenceDecision,
	outcome string,
	finalError string,
) {
	t.Helper()
	suiteQuirkRecorder.recordLiveProviderRecurrence(t, operation, decision, outcome, finalError)
}

func installSuiteQuirkRecorderForTest(t *testing.T, recorder *e2eQuirkRecorder) {
	t.Helper()

	previous := suiteQuirkRecorder
	suiteQuirkRecorder = recorder
	t.Cleanup(func() {
		suiteQuirkRecorder = previous
	})
}

func TestInstallSuiteQuirkRecorderForTestRestoresPreviousRecorder(t *testing.T) {
	original := suiteQuirkRecorder
	t.Cleanup(func() {
		suiteQuirkRecorder = original
	})

	previous, err := newE2EQuirkRecorder(t.TempDir(), suiteDriveRoles)
	require.NoError(t, err)
	suiteQuirkRecorder = previous

	t.Run("override", func(t *testing.T) {
		current, err := newE2EQuirkRecorder(t.TempDir(), suiteDriveRoles)
		require.NoError(t, err)

		installSuiteQuirkRecorderForTest(t, current)
		assert.Same(t, current, suiteQuirkRecorder)
	})

	assert.Same(t, previous, suiteQuirkRecorder)
}
