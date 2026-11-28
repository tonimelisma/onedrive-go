package devtool

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	benchWorkflowPath = workflowDir + "/bench.yml"
	benchWorkflowJob  = "bench"

	// liveAccountE2EGroup is the concurrency group of the jobs that use the
	// e2e test drive.
	liveAccountE2EGroup = "live-account-e2e"

	// benchResultDir is the one directory the bench job writes its result
	// bundle to, redacts and uploads.
	benchResultDir = "/tmp/bench-results"
)

// Validates: R-6.10.15, R-6.10.27
//
// The live benchmark lane is bench.yml, and it holds to its contract: it
// runs on manual dispatch alone, never on a push, a pull request or the
// schedule; its one job joins the e2e jobs' concurrency group without
// canceling a run, has only the token it needs (read the code, federate
// with Azure), and times out within the longest run of that group that
// ci.yml states, which the DoD wait and the nightly bound read as the
// longest a queued e2e check waits; it gets the e2e job's test drive the
// way every live job does (masked first, redacted before upload); it runs
// `devtool bench` with the dispatched scenario, whose choices are the
// registry's and whose default is the canonical live one, and writes the
// result where it redacts and uploads; and every action is pinned.
func TestBenchWorkflowHoldsItsContract(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootFromDevtoolPackage(t)
	data, err := readFile(filepath.Join(repoRoot, benchWorkflowPath))
	require.NoError(t, err)
	workflow := string(data)

	on := regexp.MustCompile(`(?ms)^on:\n(.*?)\n\S`).FindStringSubmatch(workflow)
	require.Len(t, on, 2)
	var triggers []string
	for _, trigger := range regexp.MustCompile(`(?m)^ {2}(\w+):`).FindAllStringSubmatch(on[1], -1) {
		triggers = append(triggers, trigger[1])
	}
	assert.Equal(t, []string{ciEventWorkflowDispatch}, triggers, "bench.yml runs on dispatch alone")
	assert.Contains(t, workflow, "\npermissions:\n  contents: read\n\njobs:\n", "the workflow's token reads the code, nothing else")

	jobs := ciWorkflowJobs(workflow)
	require.Len(t, jobs, 1)
	job := jobs[0]
	require.Equal(t, benchWorkflowJob, job.name)

	group, inGroup, err := job.concurrencyGroup()
	require.NoError(t, err)
	require.True(t, inGroup)
	assert.Equal(t, liveAccountE2EGroup, group)
	assert.Contains(t, job.blockEntries(ciJobConcurrencyKey), "cancel-in-progress: false")
	assert.ElementsMatch(t, []string{"contents: read", "id-token: write"}, job.blockEntries(ciJobPermissionsKey))

	timeout, err := job.longestTimeout()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, timeout, 2*time.Hour, "the live scenario's default run fits")
	assert.LessOrEqual(t, timeout, ciGroupLongestTimeout(t, repoRoot, liveAccountE2EGroup),
		"a run of the group lasts no longer than ci.yml says it can")

	exposures := workflowDriveExposures(workflow, map[string]map[string]string{
		benchWorkflowJob: {
			"ONEDRIVE_TEST_DRIVE":            "onedrive-test-drive-e2e",
			"ONEDRIVE_ALLOWED_TEST_ACCOUNTS": "onedrive-test-allowed-accounts",
		},
	})
	assert.Empty(t, exposures)
	assert.Equal(t, ciMaskingStepDrive(t, ciJobNamed(t, ciWorkflowJobs(ciWorkflowText(t, repoRoot)), "e2e")),
		ciMaskingStepDrive(t, job), "the benchmark runs on the e2e job's test drive")
	assert.Empty(t, workflowPinFindings(benchWorkflowPath, workflow))

	scenario := regexp.MustCompile(`(?ms)^ {6}scenario:\n(.*?)\n {6}\S`).FindStringSubmatch(workflow)
	require.Len(t, scenario, 2)
	var options []string
	for _, option := range regexp.MustCompile(`(?m)^ {10}- (\S+)$`).FindAllStringSubmatch(scenario[1], -1) {
		options = append(options, option[1])
	}
	assert.ElementsMatch(t, BenchScenarioIDs(), options)
	assert.Contains(t, scenario[1], "default: "+syncBidirectionalCatchup100MID)

	steps := ciJobSteps(job)
	runAt := slices.IndexFunc(steps, func(step []string) bool {
		return strings.Contains(ciStepRun(step), "go run ./cmd/devtool bench ")
	})
	require.GreaterOrEqual(t, runAt, 0, "a step runs devtool bench")
	run := ciStepRun(steps[runAt])
	assert.Contains(t, run, `--scenario "${BENCH_SCENARIO}"`)
	assert.Contains(t, run, "--result-json "+benchResultDir+"/")
	assert.Contains(t, ciBlockEntries(steps[runAt], "        env:"), "BENCH_SCENARIO",
		"the input reaches the script through env")
	assert.NotContains(t, run, "${{", "no expression is spliced into the script")
	assert.True(t, slices.ContainsFunc(steps[runAt+1:], func(step []string) bool {
		return slices.Equal(ciUploadPaths(step), []string{benchResultDir})
	}), "the result directory is uploaded after the run")
	assert.True(t, slices.ContainsFunc(steps[runAt+1:], func(step []string) bool {
		return strings.Contains(ciStepRun(step), "scripts/ci-keyvault.sh upload ")
	}), "the drive's token is saved back after the run")
}

// ciWorkflowText is the repository's CI workflow.
func ciWorkflowText(t *testing.T, repoRoot string) string {
	t.Helper()

	data, err := readFile(filepath.Join(repoRoot, ciWorkflowPath))
	require.NoError(t, err)

	return string(data)
}

// ciJobNamed is the job of jobs with that name.
func ciJobNamed(t *testing.T, jobs []ciWorkflowJob, name string) ciWorkflowJob {
	t.Helper()

	index := slices.IndexFunc(jobs, func(job ciWorkflowJob) bool { return job.name == name })
	require.GreaterOrEqual(t, index, 0, "no job %s", name)

	return jobs[index]
}

// ciGroupLongestTimeout is the longest timeout any job of ci.yml in the
// concurrency group has on any event.
func ciGroupLongestTimeout(t *testing.T, repoRoot string, group string) time.Duration {
	t.Helper()

	longest := time.Duration(0)
	for _, job := range ciWorkflowJobs(ciWorkflowText(t, repoRoot)) {
		jobGroup, inGroup, err := job.concurrencyGroup()
		require.NoError(t, err)
		if !inGroup || jobGroup != group {
			continue
		}
		timeout, err := job.longestTimeout()
		require.NoError(t, err)
		longest = max(longest, timeout)
	}
	require.Positive(t, longest, "no ci.yml job is in %s", group)

	return longest
}

// ciMaskingStepDrive is the Key Vault secret a live job's masking step reads
// its ONEDRIVE_TEST_DRIVE from.
func ciMaskingStepDrive(t *testing.T, job ciWorkflowJob) string {
	t.Helper()

	steps := ciJobSteps(job)
	index := slices.IndexFunc(steps, func(step []string) bool { return strings.Contains(ciStepRun(step), ciMaskScript) })
	require.GreaterOrEqual(t, index, 0, "%s has no masking step", job.name)
	drive, ok := ciBlockEntries(steps[index], "        env:")["ONEDRIVE_TEST_DRIVE"+ciFromVaultSuffix]
	require.True(t, ok, "%s's masking step reads no ONEDRIVE_TEST_DRIVE", job.name)

	return drive
}
