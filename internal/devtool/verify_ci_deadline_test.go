package devtool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ciJobSetupAllowance is the time a CI job spends outside go test's deadlines:
// checkout, the toolchain download, compiling devtool and the test binaries,
// the credential steps and the artifact uploads.
const ciJobSetupAllowance = 10 * time.Minute

// classifiedQuirkCount is the size of --classify-live-quirks' registry: the
// auth preflight (LI-20260405-06), the fast download test (LI-20260405-04)
// and the watch bucket's websocket restart test (LI-20260405-03).
const classifiedQuirkCount = 3

// Validates: R-6.10.26
//
// When a job's timeout-minutes ends a run before go test's own deadline does,
// the log has no goroutine dump naming the hung test and devtool never writes
// its summary. So every go test deadline a job's profile can start for an
// event, in the worst case, plus the job's setup, must fit inside the job's
// timeout for that event. The e2e job runs the fast tier on a pull request
// and e2e-full, every classified rerun taken, on the schedule; its timeout is
// one value for each.
func TestCIJobTimeoutsCoverEveryGoTestDeadline(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), ciWorkflowPath))
	require.NoError(t, err)

	cases := []struct {
		job       string
		event     string
		deadlines time.Duration
	}{
		{job: "verify-unit", event: ciEventPullRequest, deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyQuick, false)},
		{job: "verify-full", event: ciEventSchedule, deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyPublic, false)},
		{job: "stress", event: ciEventSchedule, deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyStress, false)},
		{job: "integration", event: ciEventPullRequest, deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyIntegration, false)},
		{job: "e2e", event: ciEventPullRequest, deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyE2E, false)},
		{job: "e2e", event: ciEventSchedule, deadlines: profileGoTestDeadlines(t, worstCaseE2EFullRunner(), VerifyE2EFull, true)},
	}
	for _, tc := range cases {
		timeout := ciJobTimeout(t, string(workflow), tc.job, tc.event)
		assert.LessOrEqualf(t, tc.deadlines+ciJobSetupAllowance, timeout,
			"job %s on %s: go test deadlines %s plus %s of setup exceed its timeout-minutes %s",
			tc.job, tc.event, tc.deadlines, ciJobSetupAllowance, timeout)
	}
}

// Validates: R-6.10.16, R-6.10.26
//
// The live jobs' pull request timeouts are what their profiles need and no
// more: they set the pull request's stall window, which a looser number
// would stretch for every wait.
func TestCILiveJobsPullRequestTimeoutsAreTheirNeed(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), ciWorkflowPath))
	require.NoError(t, err)

	for job, profile := range map[string]VerifyProfile{"e2e": VerifyE2E, "integration": VerifyIntegration} {
		need := profileGoTestDeadlines(t, &fakeRunner{}, profile, false) + ciJobSetupAllowance
		assert.Equal(t, need.Round(time.Minute), ciJobTimeout(t, string(workflow), job, ciEventPullRequest), "job %s", job)
	}
}

// worstCaseE2EFullRunner fails each step that has a classified rerun with the
// failure that triggers it, so the e2e-full run starts every rerun it can.
func worstCaseE2EFullRunner() *fakeRunner {
	authKey := goCommandKey(authE2EPreflightArgs())
	fastKey := goCommandKey(fastE2EJSONArgs())
	watchKey := goCommandKey(fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket()))

	return &fakeRunner{
		runErrSequenceByKey: map[string][]error{
			authKey:  {assert.AnError},
			fastKey:  {assert.AnError},
			watchKey: {assert.AnError},
		},
		runStdoutFunc: func(args []string) string {
			switch goCommandKey(args) {
			case authKey:
				return rawFailLines(authPreflightTestName)
			case fastKey:
				return jsonFailLines(fastDownloadTestName)
			case watchKey:
				return jsonFailLines(websocketRestartTestName)
			default:
				return passingGoTestOutput(args)
			}
		},
	}
}

// profileGoTestDeadlines runs one profile against the fake runner and sums the
// -timeout of every go test it started. A go test that runs tests without an
// explicit deadline fails the test: its deadline would be go test's default,
// which nothing here accounts for.
func profileGoTestDeadlines(t *testing.T, runner *fakeRunner, profile VerifyProfile, classify bool) time.Duration {
	t.Helper()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverFile := filepath.Join(repoRoot, "cover.out")
	runner.outputs = map[string][]byte{
		"go tool cover -func=" + coverFile: []byte("total:\t(statements)\t90.0%\n"),
	}
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      profile,
		CoverageFile: coverFile,
		// The CI runners have at least the reference core count.
		UnitTimeout:        unitTestTimeoutForCores(unitTestReferenceCores),
		E2ELogDir:          filepath.Join(t.TempDir(), "e2e-logs"),
		SummaryJSONPath:    summaryPath,
		ClassifyLiveQuirks: classify,
		Stdout:             &bytes.Buffer{},
		Stderr:             &bytes.Buffer{},
	})
	require.NoError(t, err, "profile %s", profile)

	if classify {
		var summary VerifySummary
		readVerifySummaryFile(t, summaryPath, &summary)
		require.Len(t, summary.ClassifiedReruns, classifiedQuirkCount,
			"the worst case takes every classified rerun")
	}

	commands := append(append([]recordedCommand{}, runner.runCommands...), runner.combinedCommands...)
	var total time.Duration
	for _, command := range commands {
		if command.name != goCommandName || len(command.args) == 0 || command.args[0] != goTestSubcommand ||
			slices.Contains(command.args, "-c") {
			continue
		}

		total += goTestDeadline(t, command.args)
	}

	return total
}

func goTestDeadline(t *testing.T, args []string) time.Duration {
	t.Helper()

	for _, arg := range args {
		value, ok := strings.CutPrefix(arg, "-timeout=")
		if !ok {
			continue
		}

		deadline, err := time.ParseDuration(value)
		require.NoError(t, err, "parse %s", arg)

		return deadline
	}

	require.Failf(t, "go test without an explicit deadline", "go %s", strings.Join(args, " "))

	return 0
}

// ciJobTimeout reads one job's timeout-minutes for an event through the
// reader the DoD stall window uses.
func ciJobTimeout(t *testing.T, workflow string, job string, event string) time.Duration {
	t.Helper()

	for _, candidate := range ciWorkflowJobs(workflow) {
		if candidate.name != job {
			continue
		}
		timeout, err := candidate.timeoutOn(event)
		require.NoError(t, err)

		return timeout
	}

	require.Failf(t, "job not found", "job %s in %s", job, ciWorkflowPath)

	return 0
}
