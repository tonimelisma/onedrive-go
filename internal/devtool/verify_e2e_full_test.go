package devtool

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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
// line for each test named for the bucket whose command runs; every other
// command, and a bucket with no test named, passes.
func bucketFailureOutput(failedByBucket map[verifyStepName][]string) func(args []string) string {
	return func(args []string) string {
		for _, bucket := range fullE2EBuckets() {
			failed := failedByBucket[bucket.Name]
			if len(failed) > 0 && (slices.Equal(args, fullE2EBucketCommandArgs(bucket)) ||
				slices.Equal(args, fullE2EBucketJSONCommandArgs(bucket))) {
				return rawFailLines(failed...)
			}
		}

		return passingGoTestOutput(args)
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
		runStdoutFunc: bucketFailureOutput(map[verifyStepName][]string{misc.Name: {"TestE2E_RapidFileChurn"}}),
	}

	summary, _, err := runE2EFullForTest(t, runner, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(misc.Name))

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
//
// A bucket's -run pattern names its tests, and the fast suite runs the smoke
// tests; one that ran none of them passed while verifying nothing, which go
// test reports as success.
func TestRunVerifyE2EFullFailsASuiteThatRanNoTests(t *testing.T) {
	t.Parallel()

	serial := fullE2ESerialSyncBucket()
	emptyRun := "testing: warning: no tests to run\nPASS\nok  \t" + testE2EPackage + "\t0.101s [no tests to run]\n"
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if slices.Equal(args, fullE2EBucketCommandArgs(serial)) || slices.Equal(args, fastE2EArgs()) {
				return emptyRun
			}

			return passingGoTestOutput(args)
		},
	}

	summary, _, err := runE2EFullForTest(t, runner, false)
	require.ErrorContains(t, err, "fast e2e matched no tests")
	require.ErrorContains(t, err, "bucket "+string(serial.Name)+": full e2e matched no tests")
	assert.Equal(t,
		[]string{verifySummaryStatusPass, verifySummaryStatusFail, verifySummaryStatusPass},
		bucketStatuses(summary))
	assert.Equal(t, verifySummaryStatusFail, requireVerifySummaryStep(t, summary, fastE2EStepName).Status)
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
		runStdoutFunc: bucketFailureOutput(map[verifyStepName][]string{
			misc.Name:  {"TestE2E_RapidFileChurn", "TestE2E_InvalidFilenameRejection"},
			watch.Name: {"TestE2E_SyncWatch_PauseResume"},
		}),
	}

	summary, stdout, err := runE2EFullForTest(t, runner, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(misc.Name))
	assert.Contains(t, err.Error(), string(watch.Name))

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
				assert.Equal(t, string(bucket.Name), summary.E2EFullBuckets[i].Name)
				assert.Equal(t, verifySummaryStatusNotRun, summary.E2EFullBuckets[i].Status)
				assert.Contains(t, summary.E2EFullBuckets[i].Error, "preflight")
			}
			assert.Contains(t, stdout, string(buckets[0].Name)+": "+verifySummaryStatusNotRun)
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

	cases := []struct {
		name   string
		output string
	}{
		{
			name:   "a build failure",
			output: "e2e/helpers.go:12:2: undefined: missingHelper\nFAIL\t" + testE2EPackage + " [build failed]\n",
		},
		{
			// The panic names the preflight test as running, but a test killed
			// at the deadline did not fail by its own verdict.
			name: "the preflight hanging until the deadline",
			output: "panic: test timed out after 10m0s\n\trunning tests:\n\t\t" + authPreflightTestName + " (10m0s)\n\n" +
				"FAIL\t" + testE2EPackage + "\t600.123s\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			authKey := goCommandKey(authE2EPreflightArgs())
			runner := &fakeRunner{
				runErrByKey: map[string]error{authKey: assert.AnError},
				runStdoutFunc: func(args []string) string {
					if goCommandKey(args) != authKey {
						return passingGoTestOutput(args)
					}

					return tc.output
				},
			}

			summary, _, err := runE2EFullForTest(t, runner, true)
			require.ErrorIs(t, err, errE2EPreflight)
			require.Len(t, runner.runCommands, 1, "a failure that is not the preflight test failing is not rerun")
			assert.Empty(t, summary.ClassifiedReruns)
		})
	}
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
				return passingGoTestOutput(args)
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
	assert.Equal(t, string(authPreflightStepName), summary.ClassifiedReruns[0].Phase, "a rerun is recorded under its step's name")
}

// Validates: R-6.10.26
//
// A rerun that skipped the preflight test names no failed test, so the red
// step names the one the first attempt failed.
func TestRunVerifyE2EFullNamesTheFailedTestWhenTheAuthPreflightRerunSkipped(t *testing.T) {
	t.Parallel()

	authKey := goCommandKey(authE2EPreflightArgs())
	authRuns := 0
	runner := &fakeRunner{
		runErrSequenceByKey: map[string][]error{authKey: {assert.AnError}},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != authKey {
				return passingGoTestOutput(args)
			}

			authRuns++
			if authRuns == 1 {
				return rawFailLines(authPreflightTestName)
			}

			return "=== RUN   " + authPreflightTestName + "\n--- SKIP: " + authPreflightTestName + " (0.00s)\n" +
				"PASS\nok  \t" + testE2EPackage + "\t0.100s\n"
		},
	}

	summary, _, err := runE2EFullForTest(t, runner, true)
	require.ErrorIs(t, err, errE2EPreflight)
	require.ErrorContains(t, err, "skipped "+authPreflightTestName)
	assert.Equal(t, []string{authPreflightTestName}, requireVerifySummaryStep(t, summary, authPreflightStepName).FailedTests)
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
				return passingGoTestOutput(args)
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
	assert.Equal(t, verifySummaryStatusSkipped, requireVerifySummaryStep(t, summary, authPreflightStepName).Status)
	assert.Contains(t, stdout.String(), authPreflightSkippedNotice)
	assert.Contains(t, stdout.String(), string(authPreflightStepName)+": "+verifySummaryStatusSkipped)
}

// The made-up live test and incident of the classified rerun tests.
const (
	rerunTestName     = "TestE2E_WidgetRoundTrip"
	rerunTestIncident = "LI-20990101-01"
)

// runClassifiedRerunForTest reruns rerunTestName the way a classified path
// does, the fake go test printing output and ending with goTestErr, and
// returns the collector, what the rerun printed and its error.
func runClassifiedRerunForTest(t *testing.T, output string, goTestErr error) (*verifySummaryCollector, string, error) {
	t.Helper()

	runner := &fakeRunner{runErr: goTestErr, runStdoutFunc: func([]string) string { return output }}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	collector := newVerifySummaryCollector(VerifyE2EFull, stdout, stderr, "", "")
	_, err := rerunClassifiedQuirk(t.Context(), runner, t.TempDir(), nil, collector, stdout, stderr, classifiedQuirkRerun{
		incidentID: rerunTestIncident,
		phase:      "widget bucket",
		trigger:    rerunTestName,
		args:       []string{"test", "-run=^" + rerunTestName + "$", "-count=1", "-v", "./e2e/..."},
		firstErr:   assert.AnError,
	})

	return collector, stdout.String(), err
}

// Validates: R-6.10.23
//
// The nightly's log reader takes a pass for a classified rerun's only inside
// the window the rerun's marker line opens, so the marker must come before
// the rerun's go test prints anything: the failure the rerun answers is then
// not keyed.
func TestRerunClassifiedQuirkPrintsItsMarkerBeforeItsGoTest(t *testing.T) {
	t.Parallel()

	rerun := "=== RUN   " + rerunTestName + "\n--- PASS: " + rerunTestName + " (1.00s)\nPASS\nok  \t" + testE2EPackage + "\t1.100s\n"
	_, printed, err := runClassifiedRerunForTest(t, rerun, nil)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(printed, classifiedRerunMarkerPrefix+rerunTestIncident+"\n"+rerun),
		"the marker line comes first, then the rerun's output: %q", printed)
	failure := jsonFailLines(rerunTestName)
	assert.Empty(t, parseGoTestLog(failure+printed).failedTopLevelTestsByPackage(), "the rerun's pass answers the failure")
}

// Validates: R-6.10.26
//
// A rerun that skips the test it reruns verified nothing, so it is no pass:
// go test exits 0 and its output shows a test selected, but the first
// failure stays and the rerun is recorded as failed. A subtest that skips
// inside a passing test is the test passing.
func TestRerunClassifiedQuirkFailsARerunThatSkippedItsTest(t *testing.T) {
	t.Parallel()

	const result = "PASS\nok  \t" + testE2EPackage + "\t0.100s\n"
	skippedRaw := "=== RUN   " + rerunTestName + "\n--- SKIP: " + rerunTestName + " (0.00s)\n    widget_test.go:12: no widget\n" + result
	skippedJSON := `{"Action":"skip","Package":"` + testE2EPackage + `","Test":"` + rerunTestName + `"}` + "\n" +
		`{"Action":"pass","Package":"` + testE2EPackage + `"}` + "\n"
	for name, output := range map[string]string{"raw": skippedRaw, "-json": skippedJSON} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			collector, printed, err := runClassifiedRerunForTest(t, output, nil)
			require.ErrorContains(t, err, "skipped "+rerunTestName)
			require.ErrorIs(t, err, assert.AnError, "the error carries the first attempt's")
			require.Len(t, collector.summary.ClassifiedReruns, 1)
			assert.Equal(t, verifySummaryStatusFail, collector.summary.ClassifiedReruns[0].Status)
			assert.NotContains(t, printed, "after successful rerun")
		})
	}

	subtestSkipped := "=== RUN   " + rerunTestName + "\n    --- SKIP: " + rerunTestName + "/personal (0.00s)\n" +
		"--- PASS: " + rerunTestName + " (0.20s)\n" + result
	_, _, err := runClassifiedRerunForTest(t, subtestSkipped, nil)
	require.NoError(t, err)
}

// Validates: R-6.10.26
//
// The skip verdict is for a rerun go test passed. A rerun go test failed
// keeps that failure as its error, whatever its output shows skipped.
func TestRerunClassifiedQuirkKeepsTheErrorOfARerunThatFailedAndSkipped(t *testing.T) {
	t.Parallel()

	goTestErr := errors.New("go test of the rerun failed")
	output := "=== RUN   " + rerunTestName + "\n--- SKIP: " + rerunTestName + " (0.00s)\nFAIL\t" + testE2EPackage + "\t0.100s\n"
	collector, _, err := runClassifiedRerunForTest(t, output, goTestErr)
	require.ErrorIs(t, err, goTestErr)
	require.ErrorIs(t, err, assert.AnError, "the error carries the first attempt's")
	assert.NotContains(t, err.Error(), "verified nothing")
	require.Len(t, collector.summary.ClassifiedReruns, 1)
	assert.Equal(t, verifySummaryStatusFail, collector.summary.ClassifiedReruns[0].Status)
}
