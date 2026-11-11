package devtool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// its summary. So every go test deadline a job's profile can start, in the
// worst case, plus the job's setup, must fit inside the job's timeout. The
// e2e-full run here takes every classified rerun the profile knows.
func TestCIJobTimeoutsCoverEveryGoTestDeadline(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), ciWorkflowPath))
	require.NoError(t, err)

	cases := []struct {
		job       string
		deadlines time.Duration
	}{
		{job: "verify-unit", deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyQuick, false)},
		{job: "verify-full", deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyPublic, false)},
		{job: "stress", deadlines: profileGoTestDeadlines(t, &fakeRunner{}, VerifyStress, false)},
		{job: "e2e", deadlines: profileGoTestDeadlines(t, worstCaseE2EFullRunner(), VerifyE2EFull, true)},
	}
	for _, tc := range cases {
		timeout := ciJobTimeout(t, string(workflow), tc.job)
		assert.LessOrEqualf(t, tc.deadlines+ciJobSetupAllowance, timeout,
			"job %s: go test deadlines %s plus %s of setup exceed its timeout-minutes %s",
			tc.job, tc.deadlines, ciJobSetupAllowance, timeout)
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
				return ""
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
		if command.name != "go" || len(command.args) == 0 || command.args[0] != "test" ||
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

// ciJobTimeout reads one job's timeout-minutes from the workflow. A job is a
// key indented by exactly two spaces under `jobs:`.
func ciJobTimeout(t *testing.T, workflow string, job string) time.Duration {
	t.Helper()

	inJobs := false
	inJob := false
	for _, line := range strings.Split(workflow, "\n") {
		if line == "jobs:" {
			inJobs = true
			continue
		}
		if !inJobs {
			continue
		}
		if key, ok := strings.CutPrefix(line, "  "); ok && key != "" && key[0] != ' ' && key[0] != '#' {
			inJob = strings.TrimSpace(key) == job+":"
			continue
		}
		if !inJob {
			continue
		}

		m := workflowTimeoutPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		minutes, err := strconv.Atoi(m[1])
		require.NoError(t, err)

		return time.Duration(minutes) * time.Minute
	}

	require.Failf(t, "job has no timeout-minutes", "job %s in %s", job, ciWorkflowPath)

	return 0
}
