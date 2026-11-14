package devtool

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// benchTestSyncExitedOne is the error a measured command that exited 1
// returns.
const benchTestSyncExitedOne = "run benchmark subject command: exit status 1"

// Validates: R-6.6.28
//
// A timed one-shot sync that exits 1 because it left work is not a
// measurement of a full sync: the sample fails, and its excerpt carries the
// sync's own summary line beside the error it ended with, however long the
// debug log before that error is.
func TestClassifyBenchProcessFailure_ASyncThatLeftWorkFailsWithItsSummary(t *testing.T) {
	t.Parallel()

	summary := "Done in 2.1 s: downloaded 3 files; 1 will be retried on the next sync."
	process := benchMeasuredProcess{
		ExitCode: 1,
		Stdout:   []byte("Syncing ~/OneDrive with user@example.com...\n" + summary + "\n"),
		Stderr: []byte(strings.Repeat("level=DEBUG msg=\"retry_work recorded\"\n", benchFailureExcerptLimit) +
			"Error: sync left 1 file unsynced; run 'onedrive-go status' for details\n"),
	}

	sample := classifyBenchProcessFailure(t.Context(), benchSample{Status: BenchSampleSuccess}, process,
		errors.New(benchTestSyncExitedOne))

	assert.Equal(t, BenchSampleSubjectFailed, sample.Status)
	assert.Contains(t, sample.FailureExcerpt, summary)
	assert.Contains(t, sample.FailureExcerpt, "sync left 1 file unsynced")
	assert.LessOrEqual(t, len(sample.FailureExcerpt), benchFailureExcerptLimit)
}

// Validates: R-6.6.28
//
// A sync that failed before it could summarize (no summary line) keeps the
// excerpt every failed sample has.
func TestClassifyBenchProcessFailure_ASyncWithoutASummaryKeepsTheErrorExcerpt(t *testing.T) {
	t.Parallel()

	process := benchMeasuredProcess{ExitCode: 1, Stderr: []byte("Error: sign-in required\n")}
	runErr := errors.New(benchTestSyncExitedOne)

	sample := classifyBenchProcessFailure(t.Context(), benchSample{Status: BenchSampleSuccess}, process, runErr)

	assert.Equal(t, BenchSampleSubjectFailed, sample.Status)
	assert.Equal(t, failureExcerpt(runErr, process.Stdout, process.Stderr), sample.FailureExcerpt)
}
