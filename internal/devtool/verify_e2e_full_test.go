package devtool

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eFullCommandsBeforeBuckets is how many go test commands the e2e-full
// profile starts before its first bucket when nothing is rerun: the auth
// preflight, the fast fixture preflight, the fast suite and the full fixture
// preflight.
const e2eFullCommandsBeforeBuckets = 4

// bucketFailureOutput returns a fake go test stdout that prints a `--- FAIL:`
// line for each test named for the bucket whose command runs.
func bucketFailureOutput(failedByBucket map[string][]string) func(args []string) string {
	return func(args []string) string {
		for _, bucket := range fullE2EBuckets() {
			if slices.Equal(args, fullE2EBucketCommandArgs(bucket)) ||
				slices.Equal(args, fullE2EBucketJSONCommandArgs(bucket)) {
				return rawFailLines(failedByBucket[bucket.Name]...)
			}
		}

		return ""
	}
}

func runE2EFullForTest(t *testing.T, runner *fakeRunner, classify bool) (VerifySummary, string, error) {
	t.Helper()

	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:           t.TempDir(),
		Profile:            VerifyE2EFull,
		E2ELogDir:          filepath.Join(t.TempDir(), "e2e-logs"),
		SummaryJSONPath:    summaryPath,
		ClassifyLiveQuirks: classify,
		Stdout:             stdout,
		Stderr:             &bytes.Buffer{},
	})

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)

	return summary, stdout.String(), err
}

// Validates: R-6.10.26
//
// A failure in the first bucket used to end the profile, so the state of the
// other two buckets stayed unknown that night, and a fix claimed for a test in
// them read as confirmed although the test never ran.
func TestRunVerifyE2EFullRunsEveryBucketAfterOneFails(t *testing.T) {
	t.Parallel()

	misc := fullE2EParallelMiscBucket()
	runner := &fakeRunner{
		runErrByKey:   map[string]error{goCommandKey(fullE2EBucketCommandArgs(misc)): assert.AnError},
		runStdoutFunc: bucketFailureOutput(map[string][]string{misc.Name: {"TestE2E_RapidFileChurn"}}),
	}

	summary, _, err := runE2EFullForTest(t, runner, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), misc.Name)

	buckets := fullE2EBuckets()
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(buckets))
	for i, bucket := range buckets {
		assert.Equal(t, fullE2EBucketCommandArgs(bucket), runner.runCommands[e2eFullCommandsBeforeBuckets+i].args)
	}

	assert.Equal(t, verifySummaryStatusFail, summary.Status)
	require.Len(t, summary.E2EFullBuckets, len(buckets))
	assert.Equal(t,
		[]string{verifySummaryStatusFail, verifySummaryStatusPass, verifySummaryStatusPass},
		bucketStatuses(summary))
	assert.Equal(t, []string{"TestE2E_RapidFileChurn"}, summary.E2EFullBuckets[0].FailedTests)
}

// Validates: R-6.10.26
func TestRunVerifyE2EFullReportsEveryFailingBucket(t *testing.T) {
	t.Parallel()

	misc := fullE2EParallelMiscBucket()
	watch := fullE2ESerialWatchSharedBucket()
	runner := &fakeRunner{
		runErrByKey: map[string]error{
			goCommandKey(fullE2EBucketCommandArgs(misc)):  assert.AnError,
			goCommandKey(fullE2EBucketCommandArgs(watch)): assert.AnError,
		},
		runStdoutFunc: bucketFailureOutput(map[string][]string{
			misc.Name:  {"TestE2E_RapidFileChurn", "TestE2E_InvalidFilenameRejection"},
			watch.Name: {"TestE2E_SyncWatch_PauseResume"},
		}),
	}

	summary, stdout, err := runE2EFullForTest(t, runner, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), misc.Name)
	assert.Contains(t, err.Error(), watch.Name)

	require.Len(t, summary.E2EFullBuckets, len(fullE2EBuckets()))
	assert.Equal(t,
		[]string{verifySummaryStatusFail, verifySummaryStatusPass, verifySummaryStatusFail},
		bucketStatuses(summary))
	assert.Equal(t,
		[]string{"TestE2E_RapidFileChurn", "TestE2E_InvalidFilenameRejection"},
		summary.E2EFullBuckets[0].FailedTests)
	assert.Empty(t, summary.E2EFullBuckets[1].FailedTests)
	assert.Equal(t, []string{"TestE2E_SyncWatch_PauseResume"}, summary.E2EFullBuckets[2].FailedTests)
	assert.Contains(t, stdout, "failed test: TestE2E_InvalidFilenameRejection")
	assert.Contains(t, stdout, "failed test: TestE2E_SyncWatch_PauseResume")
}

// Validates: R-6.10.26
//
// A preflight failure means the account or the fixtures are unusable, so the
// buckets do not run; the summary still lists all three, as not run, so a
// reader can tell "did not run" from "absent".
func TestRunVerifyE2EFullRecordsBucketsNotRunAfterPreflightFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		failing     []string
		commandsRun int
	}{
		{name: "auth preflight", failing: authE2EPreflightArgs(), commandsRun: 1},
		{name: "fast fixture preflight", failing: fastE2EPreflightArgs(), commandsRun: 2},
		{name: "full fixture preflight", failing: fullE2EPreflightArgs(), commandsRun: e2eFullCommandsBeforeBuckets},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{runErrByKey: map[string]error{goCommandKey(tc.failing): assert.AnError}}

			summary, stdout, err := runE2EFullForTest(t, runner, false)
			require.ErrorIs(t, err, errE2EPreflight)
			require.Len(t, runner.runCommands, tc.commandsRun)

			buckets := fullE2EBuckets()
			require.Len(t, summary.E2EFullBuckets, len(buckets))
			for i, bucket := range buckets {
				assert.Equal(t, bucket.Name, summary.E2EFullBuckets[i].Name)
				assert.Equal(t, verifySummaryStatusNotRun, summary.E2EFullBuckets[i].Status)
				assert.Contains(t, summary.E2EFullBuckets[i].Error, "preflight")
			}
			assert.Contains(t, stdout, buckets[0].Name+": "+verifySummaryStatusNotRun)
		})
	}
}

// Validates: R-6.10.26
//
// The auth preflight's classified rerun exists for one documented provider
// quirk, which shows as the preflight test failing. A build failure, a pattern
// that matched nothing or a timeout is not that quirk and is not rerun.
func TestRunVerifyE2EFullDoesNotRerunAuthPreflightWithoutItsTestFailing(t *testing.T) {
	t.Parallel()

	authKey := goCommandKey(authE2EPreflightArgs())
	runner := &fakeRunner{
		runErrByKey: map[string]error{authKey: assert.AnError},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != authKey {
				return ""
			}

			return "e2e/helpers.go:12:2: undefined: missingHelper\nFAIL\t" + testE2EPackage + " [build failed]\n"
		},
	}

	summary, _, err := runE2EFullForTest(t, runner, true)
	require.ErrorIs(t, err, errE2EPreflight)
	require.Len(t, runner.runCommands, 1, "a failure that is not the preflight test failing is not rerun")
	assert.Empty(t, summary.ClassifiedReruns)
}

// Validates: R-6.10.26
func TestRunVerifyE2EFullReportsTheAuthPreflightRerunFailure(t *testing.T) {
	t.Parallel()

	authKey := goCommandKey(authE2EPreflightArgs())
	rerunErr := errors.New("rerun of the auth preflight failed")
	runner := &fakeRunner{
		runErrSequenceByKey: map[string][]error{authKey: {assert.AnError, rerunErr}},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != authKey {
				return ""
			}

			return rawFailLines(authPreflightTestName)
		},
	}

	summary, _, err := runE2EFullForTest(t, runner, true)
	require.ErrorIs(t, err, errE2EPreflight)
	require.ErrorIs(t, err, rerunErr, "the error a failed rerun returns is the rerun's")
	require.Len(t, runner.runCommands, 2)
	require.Len(t, summary.ClassifiedReruns, 1)
	assert.Equal(t, verifySummaryStatusFail, summary.ClassifiedReruns[0].Status)
}

// Validates: R-6.10.26
//
// The auth preflight skips when Microsoft reports a read-only window: the
// lane could not run. Recording that as a pass would claim a verification
// that did not happen.
func TestRunVerifyRecordsASkippedAuthPreflightAsSkipped(t *testing.T) {
	t.Parallel()

	authKey := goCommandKey(authE2EPreflightArgs())
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != authKey {
				return ""
			}

			return "=== RUN   " + authPreflightTestName + "\n" +
				"    --- SKIP: " + authPreflightTestName + "/personal_example (0.20s)\n" +
				"--- PASS: " + authPreflightTestName + " (0.20s)\n"
		},
	}

	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        t.TempDir(),
		Profile:         VerifyE2E,
		E2ELogDir:       filepath.Join(t.TempDir(), "e2e-logs"),
		SummaryJSONPath: summaryPath,
		Stdout:          stdout,
		Stderr:          &bytes.Buffer{},
	})
	require.NoError(t, err)

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, verifySummaryStatusSkipped, requireVerifySummaryStep(t, summary, "auth preflight").Status)
	assert.Contains(t, stdout.String(), authPreflightSkippedNotice)
	assert.Contains(t, stdout.String(), "auth preflight: "+verifySummaryStatusSkipped)
}
