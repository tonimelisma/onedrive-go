package testutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	standingBaselineStdout = "Main drive: Up to date (1.3 s).\n" +
		"Shared reports: Done in 2.0 s; 2 still not synced (see 'onedrive-go status').\n" +
		"Shared plans: Done in 0.4 s: downloaded 1 file (46 B); 1 still not synced (see 'onedrive-go status').\n"

	standingErrorTwoOfThree = "Error: 2 of 3 selected drives left files unsynced (see above)\n"
)

func recordedStandingConditions(t *testing.T) *StandingSyncConditions {
	t.Helper()

	var standing StandingSyncConditions
	standing.RecordBaseline(standingBaselineStdout)
	require.False(t, standing.Empty())

	return &standing
}

// Validates: R-6.6.28
func TestStandingSyncConditions_RecordsOnlyStandingLinesOfNamedMounts(t *testing.T) {
	t.Parallel()

	var standing StandingSyncConditions
	assert.True(t, standing.Empty())

	standing.RecordBaseline("Main drive: Up to date (1.3 s).\n" +
		"Main drive: Held back by --download-only: 3 uploads.\n" +
		"Shared reports: Done in 2.0 s; 2 still not synced (see 'onedrive-go status').\r\n" +
		"Flaky: Done in 2.0 s; 1 failed; 4 still not synced (see 'onedrive-go status').\n" +
		"Retrying: Done in 2.0 s: uploaded 1 file; 1 will be retried on the next sync.\n" +
		"Done in 0.2 s; 5 still not synced (see 'onedrive-go status').\n")
	assert.Equal(t, 1, standing.Mounts())
	assert.Equal(t, 2, standing.Items())
	assert.Equal(t, 5, standing.Unattributed(), "a standing line that names no mount is counted apart")

	// A second baseline pass keeps the larger count of a mount and adds new mounts.
	standing.RecordBaseline("Shared reports: Done in 1.0 s; 1 still not synced (see 'onedrive-go status').\n" +
		"Shared plans: Done in 1.0 s; 3 still not synced (see 'onedrive-go status').\n")
	assert.Equal(t, 2, standing.Mounts())
	assert.Equal(t, 5, standing.Items())
}

// Validates: R-6.6.28
func TestStandingSyncConditions_SettleRewritesAPassThatLeftOnlyRecordedConditions(t *testing.T) {
	t.Parallel()

	standing := recordedStandingConditions(t)

	stdout, stderr, settled := standing.Settle(
		"Main drive: Done in 2.7 s: downloaded 2 files (46 B), created 3 folders.\n"+
			"Shared reports: Done in 1 min 3 s; 1 still not synced (see 'onedrive-go status').\n"+
			"Shared plans: Done in 0.4 s: uploaded 1 file (1.2 KB); 1 still not synced (see 'onedrive-go status').\n",
		"time=2026-01-01T00:00:00Z level=DEBUG msg=\"pass finished\"\n"+standingErrorTwoOfThree,
	)
	require.True(t, settled)
	assert.Equal(t,
		"Main drive: Done in 2.7 s: downloaded 2 files (46 B), created 3 folders.\n"+
			"Shared reports: Up to date (1 min 3 s).\n"+
			"Shared plans: Done in 0.4 s: uploaded 1 file (1.2 KB).\n",
		stdout)
	assert.Equal(t, "time=2026-01-01T00:00:00Z level=DEBUG msg=\"pass finished\"\n", stderr)
}

// Validates: R-6.6.28
func TestStandingSyncConditions_SettleLeavesEveryOtherPassAlone(t *testing.T) {
	t.Parallel()

	const (
		standingLine = "Shared reports: Done in 2.0 s; 2 still not synced (see 'onedrive-go status').\n"
		oneOfThree   = "Error: 1 of 3 selected drives left files unsynced (see above)\n"
	)

	cases := []struct {
		name   string
		stdout string
		stderr string
	}{
		{
			name:   "nothing left",
			stdout: "Main drive: Up to date (1.3 s).\nShared reports: Up to date (0.2 s).\n",
			stderr: "",
		},
		{
			name:   "more items than recorded",
			stdout: "Shared reports: Done in 2.0 s; 3 still not synced (see 'onedrive-go status').\n",
			stderr: oneOfThree,
		},
		{
			name:   "a mount that was not recorded",
			stdout: "Main drive: Done in 2.0 s; 1 still not synced (see 'onedrive-go status').\n",
			stderr: oneOfThree,
		},
		{
			name:   "no mount named",
			stdout: "Done in 2.0 s; 1 still not synced (see 'onedrive-go status').\n",
			stderr: "Error: sync left 1 file unsynced; run 'onedrive-go status' for details\n",
		},
		{
			name:   "a failure beside the standing condition",
			stdout: "Shared reports: Done in 2.0 s; 1 failed; 2 still not synced (see 'onedrive-go status').\n",
			stderr: oneOfThree,
		},
		{
			name:   "another mount left work of its own",
			stdout: standingLine + "Main drive: Done in 2.0 s: uploaded 1 file; 1 will be retried on the next sync.\n",
			stderr: standingErrorTwoOfThree,
		},
		{
			name:   "the error counts more mounts than the standing lines",
			stdout: standingLine,
			stderr: standingErrorTwoOfThree,
		},
		{
			name:   "a different error",
			stdout: standingLine,
			stderr: "Error: Sign-in is required. Run 'onedrive-go login'.\n",
		},
		{
			name:   "a second error line",
			stdout: standingLine,
			stderr: "Error: something else\n" + oneOfThree,
		},
		{
			name:   "text after the error line",
			stdout: standingLine,
			stderr: oneOfThree + "Details: more\n",
		},
		{
			name:   "no error line",
			stdout: standingLine,
			stderr: "",
		},
	}

	standing := recordedStandingConditions(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, settled := standing.Settle(tc.stdout, tc.stderr)
			assert.False(t, settled)
			assert.Equal(t, tc.stdout, stdout)
			assert.Equal(t, tc.stderr, stderr)
		})
	}
}

// Validates: R-6.6.28
func TestStandingSyncConditions_ZeroValueSettlesNothing(t *testing.T) {
	t.Parallel()

	var standing StandingSyncConditions
	stdout, stderr, settled := standing.Settle(
		"Shared reports: Done in 2.0 s; 2 still not synced (see 'onedrive-go status').\n",
		"Error: 1 of 3 selected drives left files unsynced (see above)\n",
	)
	assert.False(t, settled)
	assert.Contains(t, stdout, "still not synced")
	assert.Contains(t, stderr, "Error: ")
}
