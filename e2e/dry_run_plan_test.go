package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// This file has no build tags, so its tests run in the untagged quick lane
// without an account; the live dry-run waits in sync_full_test.go build on
// it.

const (
	// dryRunPlannedTransferPrefix starts every line of a `sync --dry-run`
	// plan that names an action.
	dryRunPlannedTransferPrefix = "Would "
	// dryRunNothingToDoSummary and dryRunPlannedSummaryPrefix start the
	// summary line a `sync --dry-run` pass prints after its plan: for an
	// empty plan, and for a plan with actions.
	dryRunNothingToDoSummary   = "Dry run: nothing to do."
	dryRunPlannedSummaryPrefix = "Dry run, nothing was changed."
)

// dryRunStdoutHasLineStarting reports whether the stdout of a
// `sync --dry-run` pass has a plan line that starts with prefix.
func dryRunStdoutHasLineStarting(stdout, prefix string) bool {
	return dryRunPlanHasLine(stdout, func(line string) bool {
		return strings.HasPrefix(line, prefix)
	})
}

// dryRunStdoutPlansNoTransferUnder reports whether the stdout of a
// `sync --dry-run` pass holds the pass's summary line and names no action
// under folder. Output without the summary, empty or cut short, does not hold
// the whole plan, so it proves nothing about what the plan leaves out.
func dryRunStdoutPlansNoTransferUnder(stdout, folder string) bool {
	return dryRunPlanHasLine(stdout, isDryRunSummaryLine) &&
		!dryRunPlanHasLine(stdout, func(line string) bool {
			return strings.HasPrefix(line, dryRunPlannedTransferPrefix) && strings.Contains(line, folder+"/")
		})
}

// isDryRunSummaryLine reports whether a trimmed line is a dry run's summary.
func isDryRunSummaryLine(line string) bool {
	return line == dryRunNothingToDoSummary || strings.HasPrefix(line, dryRunPlannedSummaryPrefix)
}

// dryRunPlanHasLine reports whether a trimmed line of stdout matches.
func dryRunPlanHasLine(stdout string, matches func(line string) bool) bool {
	for _, line := range strings.Split(stdout, "\n") {
		if matches(strings.TrimSpace(line)) {
			return true
		}
	}

	return false
}

// A dry run that plans nothing under the test's folder proves both sides
// already agree only when its output is whole: its summary line is the proof
// that the plan above it is complete.
func TestDryRunStdoutPlansNoTransferUnder(t *testing.T) {
	t.Parallel()

	const folder = "e2e-converge"
	tests := []struct {
		name   string
		stdout string
		want   bool
	}{
		{name: "nothing to do", stdout: "Syncing ~/x with alice@example.com...\nDry run: nothing to do.\n", want: true},
		{
			name:   "actions only outside the folder",
			stdout: "Would download other/a.txt\nDry run, nothing was changed. Would download 1 file.\n",
			want:   true,
		},
		{
			name:   "an upload under the folder",
			stdout: "  Would upload " + folder + "/a.txt\nDry run, nothing was changed. Would upload 1 file.\n",
			want:   false,
		},
		{
			name:   "a conflict under the folder",
			stdout: "Would keep both versions of " + folder + "/a.txt\nDry run, nothing was changed. Would keep both versions of 1 file.\n",
			want:   false,
		},
		{name: "no output", stdout: "", want: false},
		{name: "output cut before the summary", stdout: "Syncing ~/x with alice@example.com...\nWould download other/a.txt\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, dryRunStdoutPlansNoTransferUnder(tt.stdout, folder))
		})
	}
}

// A dry run names a remote change once delta carries it, as a plan line; the
// summary line, which holds the same verb after "Dry run", is not a plan line.
func TestDryRunStdoutHasLineStarting(t *testing.T) {
	t.Parallel()

	const stdout = "  Would download e2e-x/new.txt\nDry run, nothing was changed. Would download 1 file.\n"

	assert.True(t, dryRunStdoutHasLineStarting(stdout, "Would download e2e-x/"))
	assert.False(t, dryRunStdoutHasLineStarting(stdout, "Would upload "))
	assert.False(t, dryRunStdoutHasLineStarting("Dry run, nothing was changed. Would download 1 file.\n", "Would download "),
		"the summary is not a plan line")
}
