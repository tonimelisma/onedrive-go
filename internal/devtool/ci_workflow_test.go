package devtool

import (
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ciEventSchedule is the event of the nightly run, which no DoD wait follows.
const ciEventSchedule = "schedule"

// eventScopedWorkflowFixture has the job conditions, the timeout shape and the
// concurrency forms of ci.yml, with timeouts chosen so that every wrong
// reading gives its own window: counting the nightly-only job, reading the
// e2e expression's fallback on a pull request, or taking a step's timeout for
// its job's. The e2e job shares its concurrency group with a nightly-only job
// whose run is longer than any of e2e's own.
const eventScopedWorkflowFixture = `on:
  push:
jobs:
  verify-lint:
    if: github.event_name == 'pull_request' || github.event_name == 'push'
    timeout-minutes: 15
  verify-unit:
    timeout-minutes: 20
    if: github.event_name == 'pull_request' || github.event_name == 'push'
    steps:
      - name: Shard
        if: matrix.os == 'ubuntu-latest'
        timeout-minutes: 300
  verify-full:
    timeout-minutes: 120
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
  # The aggregate runs on every event.
  verify:
    timeout-minutes: 5
    if: always()
  e2e:
    timeout-minutes: ${{ github.event_name == 'pull_request' && 40 || 180 }}
    concurrency:
      group: live-account-e2e
      cancel-in-progress: false
    if: vars.AZURE_CLIENT_ID != '' && github.event_name != 'push'
  live-soak:
    timeout-minutes: 200
    concurrency: live-account-e2e
    if: github.event_name == 'schedule'
`

// Validates: R-6.10.16
//
// A push to main runs only the PR lane's 15- and 20-minute jobs, but the
// window used to be the longest timeout anywhere in the workflow, the
// nightly e2e job's 180 minutes, so a dead post-merge wait took over three
// hours to say so.
func TestCIStallWindow_PostMergeUsesOnlyPushJobs(t *testing.T) {
	t.Parallel()

	cfg, err := newCIWaitConfig(&VerifyOptions{}, writeWorkflowFixture(t, eventScopedWorkflowFixture), ciEventPush, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 20*time.Minute+ciStallMargin, cfg.stallWindow)
	assert.Empty(t, cfg.notStartedWindows, "no job of a concurrency group runs on a push")

	push, err := newCIWaitConfig(&VerifyOptions{}, repoRootForTest(t), ciEventPush, io.Discard)
	require.NoError(t, err)
	pullRequest, err := newCIWaitConfig(&VerifyOptions{}, repoRootForTest(t), ciEventPullRequest, io.Discard)
	require.NoError(t, err)
	assert.Greater(t, push.stallWindow, ciStallMargin)
	assert.Less(t, push.stallWindow, pullRequest.stallWindow, "the live jobs never run on a push to main")
}

// Validates: R-6.10.16
//
// A pull request runs the e2e job's fast tier under the expression's pull
// request value, and never the nightly-only full tier. While its e2e check
// has not started it can be waiting for a whole run of its concurrency group
// from another event, so that check's window is the group's longest timeout
// on any event; the default hard cap leaves room for one such wait.
func TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs(t *testing.T) {
	t.Parallel()

	opts := &VerifyOptions{DODCIMaxReruns: DefaultDODCIMaxReruns}
	cfg, err := newCIWaitConfig(opts, writeWorkflowFixture(t, eventScopedWorkflowFixture), ciEventPullRequest, io.Discard)
	require.NoError(t, err)

	eventWindow := 40*time.Minute + ciStallMargin
	groupWindow := 200*time.Minute + ciStallMargin
	assert.Equal(t, eventWindow, cfg.stallWindow)
	assert.Equal(t, map[string]time.Duration{"e2e": groupWindow}, cfg.notStartedWindows)
	assert.Equal(t, time.Duration(DefaultDODCIMaxReruns+1)*eventWindow+groupWindow, cfg.hardCap)
}

// Validates: R-6.10.16, R-6.10.26
//
// The reader understands the shapes the workflow uses and nothing more. A
// condition or a timeout it cannot evaluate fails loudly, naming the job,
// instead of silently leaving a job out of the stall window or the deadline
// check.
func TestCIWorkflow_RejectsAnExpressionItDoesNotKnow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		job     string
		wantErr string
	}{
		{
			name:    "a timeout from an input",
			job:     "    timeout-minutes: ${{ inputs.minutes }}\n",
			wantErr: "timeout-minutes",
		},
		{
			name:    "a zero that the expression would turn into its fallback",
			job:     "    timeout-minutes: ${{ github.event_name == 'pull_request' && 0 || 180 }}\n",
			wantErr: "timeout-minutes",
		},
		{
			name:    "a function the reader does not know",
			job:     "    timeout-minutes: 15\n    if: contains(github.ref, 'release')\n",
			wantErr: "if:",
		},
		{
			name:    "a negation",
			job:     "    timeout-minutes: 15\n    if: ${{ !failure() }}\n",
			wantErr: "if:",
		},
		{
			name:    "parentheses",
			job:     "    timeout-minutes: 15\n    if: (github.event_name == 'push') && always()\n",
			wantErr: "if:",
		},
		{
			name:    "a block scalar",
			job:     "    timeout-minutes: 15\n    if: >\n      always()\n",
			wantErr: "if:",
		},
		{
			name:    "a job that runs without a timeout",
			job:     "    if: github.event_name == 'pull_request'\n    steps: []\n",
			wantErr: "timeout-minutes",
		},
		{
			name:    "a concurrency group from an expression",
			job:     "    timeout-minutes: 15\n    concurrency: ci-${{ github.ref }}\n",
			wantErr: "concurrency",
		},
		{
			name:    "a concurrency key the reader does not know",
			job:     "    timeout-minutes: 15\n    concurrency:\n      group: live\n      queue: max\n",
			wantErr: "concurrency",
		},
		{
			name:    "a concurrency map without a group",
			job:     "    timeout-minutes: 15\n    concurrency:\n      cancel-in-progress: true\n",
			wantErr: "concurrency",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := writeWorkflowFixture(t, "jobs:\n  odd-job:\n"+tc.job)
			_, err := newCIWaitConfig(&VerifyOptions{}, root, ciEventPullRequest, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "odd-job")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// ciTestMasksFile is the file, under the runner's temp directory, in which
// the masking script lists every spelling it masked, for the step that
// redacts a job's artifacts.
const ciTestMasksFile = "onedrive-test-masks"

// ciMaskScript is the script that masks a live job's test drives and account
// allowlist and exports them to the job's later steps.
const ciMaskScript = "scripts/ci-mask-test-drives.sh"

// ciShortcutFixtureSources lists, per live job, which of its drive variables
// holds each shortcut fixture, by name: the masking step states it so, and
// the script exports the fixture variable from that drive.
func ciShortcutFixtureSources() map[string]map[string]string {
	return map[string]map[string]string{
		"e2e": {
			"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE_FROM": "ONEDRIVE_TEST_DRIVE",
			"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE_FROM": "ONEDRIVE_TEST_DRIVE_2",
		},
	}
}

// ciLiveJobVariables lists, per live job, the variables that carry its test
// drives and the account allowlist.
func ciLiveJobVariables() map[string][]string {
	return map[string][]string{
		"integration": {"ONEDRIVE_TEST_DRIVE", "ONEDRIVE_ALLOWED_TEST_ACCOUNTS"},
		"e2e":         {"ONEDRIVE_TEST_DRIVE", "ONEDRIVE_TEST_DRIVE_2", "ONEDRIVE_ALLOWED_TEST_ACCOUNTS"},
	}
}

// Validates: R-6.10.27
//
// The repository's logs are public. GitHub prints a job-level env value in
// the header of every step, so the test drives and the account allowlist
// were in every step of both live jobs. They now reach a job only through
// the step that runs the masking script, which nothing but the checkout
// precedes; every later step reads the masked values and the names the
// script derives; and an artifact, which masking does not reach, is
// uploaded only after a step redacts the masked spellings in it.
func TestCIWorkflowSetsNoTestDriveInJobEnv(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), ciWorkflowPath))
	require.NoError(t, err)
	workflow := string(data)
	assert.Empty(t, ciDriveExposures(workflow))

	// The same workflow with a step run ahead of the e2e job's masking step,
	// with the allowlist back in the integration job's env, and with a step
	// that derives a drive's spelling itself.
	e2eAt := strings.Index(workflow, "\n  e2e:\n")
	require.Positive(t, e2eAt)
	runFirst := workflow[:e2eAt] + strings.Replace(workflow[e2eAt:],
		"    steps:\n", "    steps:\n      - run: go version\n", 1)
	assert.Contains(t, ciDriveExposures(runFirst), "e2e: step 1 runs before the masking script")

	jobEnv := strings.Replace(workflow, "\n  integration:\n", "\n  integration:\n"+
		"    env:\n      ONEDRIVE_ALLOWED_TEST_ACCOUNTS: ${{ vars.ONEDRIVE_ALLOWED_TEST_ACCOUNTS }}\n", 1)
	assert.Contains(t, ciDriveExposures(jobEnv), "integration: job-level env sets ONEDRIVE_ALLOWED_TEST_ACCOUNTS")

	rederived := workflow[:e2eAt] + strings.Replace(workflow[e2eAt:],
		"      - name: Run E2E tests\n        run: |\n",
		"      - name: Run E2E tests\n        run: |\n          echo \"$ONEDRIVE_TEST_DRIVE\" | sed 's/:/_/'\n", 1)
	require.NotEqual(t, workflow, rederived)
	assert.Contains(t, ciDriveExposures(rederived), "e2e: a step derives a spelling with sed; the masking script exports it")

	// The same workflow uploading a file from outside the directory the
	// redaction step rewrites, and with that step rewriting another directory.
	const summary = "            /tmp/e2e-debug-logs/e2e-verify-summary.json\n"
	outside := strings.Replace(workflow, summary, "            ${{ runner.temp }}/e2e-verify-summary.json\n", 1)
	require.NotEqual(t, workflow, outside)
	assert.Contains(t, strings.Join(ciDriveExposures(outside), "\n"),
		"uploads ${{ runner.temp }}/e2e-verify-summary.json, which no earlier step redacted")

	elsewhere := strings.Replace(workflow, "find /tmp/e2e-debug-logs -type f", "find /tmp/other-logs -type f", 1)
	require.NotEqual(t, workflow, elsewhere)
	assert.Contains(t, strings.Join(ciDriveExposures(elsewhere), "\n"),
		"uploads /tmp/e2e-debug-logs, which no earlier step redacted")
}

// ciDriveExposures lists every way the workflow's live jobs could print a test
// drive or the account allowlist in a public log or artifact.
func ciDriveExposures(workflow string) []string {
	var problems []string
	expected := ciLiveJobVariables()
	live := []string{}
	for _, job := range ciWorkflowJobs(workflow) {
		if !slices.ContainsFunc(job.lines, func(line string) bool { return strings.Contains(line, "ONEDRIVE_") }) {
			continue
		}
		live = append(live, job.name)
		names, known := expected[job.name]
		if !known {
			problems = append(problems, job.name+": a job that uses a test drive is not a known live job")
			continue
		}
		problems = append(problems, ciLiveJobExposures(job, names)...)
	}
	known := make([]string, 0, len(expected))
	for name := range expected {
		known = append(known, name)
	}
	sort.Strings(known)
	sort.Strings(live)
	if !slices.Equal(known, live) {
		problems = append(problems, fmt.Sprintf("live jobs are %v, want %v", live, known))
	}

	return problems
}

func ciLiveJobExposures(job ciWorkflowJob, names []string) []string {
	var problems []string
	for name := range ciBlockEntries(job.lines, "    env:") {
		if strings.HasPrefix(name, "ONEDRIVE_") {
			problems = append(problems, job.name+": job-level env sets "+name)
		}
	}

	steps := ciJobSteps(job)
	masking := slices.IndexFunc(steps, func(step []string) bool { return strings.Contains(ciStepRun(step), ciMaskScript) })
	if masking < 0 {
		return append(problems, job.name+": no step runs "+ciMaskScript)
	}
	// Only steps that run nothing of this repository's and see no drive may
	// come first: the checkout the script needs.
	for i, step := range steps[:masking] {
		if ciStepRun(step) != "" || slices.ContainsFunc(step, func(line string) bool { return strings.Contains(line, "ONEDRIVE_") }) {
			problems = append(problems, fmt.Sprintf("%s: step %d runs before the masking script", job.name, i+1))
		}
	}
	problems = append(problems, ciMaskingStepInputExposures(job.name, steps[masking], names)...)

	return append(problems, ciLaterStepExposures(job.name, steps[masking+1:], masking+2)...)
}

// ciLaterStepExposures checks the steps after the masking step, the first of
// them numbered first: they read the masked values and the names the script
// exports, derive no spelling themselves, and upload an artifact only after
// a step redacted it.
func ciLaterStepExposures(job string, steps [][]string, first int) []string {
	var problems []string
	usesDerivedNames := false
	var redacted []string
	for i, step := range steps {
		number := first + i
		run := ciStepRun(step)
		if slices.ContainsFunc(step, func(line string) bool {
			return strings.Contains(line, "vars.ONEDRIVE_") || strings.Contains(line, "secrets.ONEDRIVE_")
		}) {
			problems = append(problems, fmt.Sprintf("%s: step %d reads a test drive variable itself", job, number))
		}
		if strings.Contains(run, "sed ") {
			problems = append(problems, job+": a step derives a spelling with sed; the masking script exports it")
		}
		if strings.Contains(run, "_VAULT_SECRET") && strings.Contains(run, "_TOKEN_FILE") {
			usesDerivedNames = true
		}
		if strings.Contains(run, ciTestMasksFile) {
			for _, m := range ciRedactedDirPattern.FindAllStringSubmatch(run, -1) {
				redacted = append(redacted, strings.TrimSuffix(m[1], "/"))
			}
		}
		if !slices.ContainsFunc(step, func(line string) bool { return strings.Contains(line, "actions/upload-artifact") }) {
			continue
		}
		paths := ciUploadPaths(step)
		if len(paths) == 0 {
			problems = append(problems, fmt.Sprintf("%s: step %d uploads an artifact whose path this check cannot read", job, number))
		}
		for _, path := range paths {
			if !slices.ContainsFunc(redacted, func(dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }) {
				problems = append(problems, fmt.Sprintf("%s: step %d uploads %s, which no earlier step redacted", job, number, path))
			}
		}
	}
	if !usesDerivedNames {
		problems = append(problems, job+": no credential step reads the vault secret and token file names the script exports")
	}

	return problems
}

// ciRedactedDirPattern finds the directory a redaction step rewrites: the one
// its `find` walks.
var ciRedactedDirPattern = regexp.MustCompile(`\bfind (/\S+) -type f\b`)

// ciUploadPaths returns the paths an upload-artifact step uploads, from a
// single-line or a block `path:`, with a directory's trailing slash dropped.
func ciUploadPaths(step []string) []string {
	const pathKey = "          path: "
	var paths []string
	in := false
	for _, line := range step {
		if value, ok := strings.CutPrefix(line, pathKey); ok {
			if value != "|" {
				return []string{strings.TrimSuffix(strings.TrimSpace(value), "/")}
			}
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "            ") {
			break
		}
		paths = append(paths, strings.TrimSuffix(strings.TrimSpace(line), "/"))
	}

	return paths
}

// ciSecretOrVariablePattern is the one shape a masking step's input takes: a
// secret of the variable's name, if the owner creates one, wins and is masked
// by GitHub everywhere, this step's header included.
var ciSecretOrVariablePattern = regexp.MustCompile(`^\$\{\{ secrets\.([A-Z0-9_]+) \|\| vars\.([A-Z0-9_]+) \}\}$`)

// ciMaskingStepInputExposures checks that the masking step receives each of
// the job's variables, and in the one shape that lets a secret win.
func ciMaskingStepInputExposures(job string, step []string, names []string) []string {
	var problems []string
	env := ciBlockEntries(step, "        env:")
	for _, name := range names {
		value, ok := env[name]
		if !ok {
			problems = append(problems, job+": the masking step does not receive "+name)
			continue
		}
		m := ciSecretOrVariablePattern.FindStringSubmatch(value)
		if len(m) != 3 || m[1] != m[2] {
			problems = append(problems, fmt.Sprintf("%s: the masking step reads %s as %q", job, name, value))
		}
	}
	sources := map[string]string{}
	for name, value := range env {
		if strings.HasSuffix(name, "_FROM") {
			sources[name] = value
		}
	}
	if want := ciShortcutFixtureSources()[job]; !maps.Equal(sources, want) && (len(sources) > 0 || len(want) > 0) {
		problems = append(problems, fmt.Sprintf("%s: the masking step maps the shortcut fixtures as %v, want %v", job, sources, want))
	}

	return problems
}

// Validates: R-6.10.27
//
// The masking script, run as a live job runs it with synthetic values: it
// prints nothing but mask commands, masks every spelling the job, its tests
// and Graph URLs can print, lists them for the artifact redaction, and
// exports the values and each drive's vault secret and token file names.
func TestCIMaskScript_MasksEverySpellingAndExportsTheNames(t *testing.T) {
	t.Parallel()

	const (
		first     = "personal:Alice.A@example.test"
		second    = "business:bob_b@example.test"
		allowlist = first + ", " + second
	)
	dir := t.TempDir()
	githubEnv := filepath.Join(dir, "github-env")
	stdout, environment := runCIMaskScript(t, dir, githubEnv, map[string]string{
		"ONEDRIVE_TEST_DRIVE":                               first,
		"ONEDRIVE_TEST_DRIVE_2":                             second,
		"ONEDRIVE_ALLOWED_TEST_ACCOUNTS":                    allowlist,
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE_FROM": "ONEDRIVE_TEST_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE_FROM": "ONEDRIVE_TEST_DRIVE_2",
	})

	masked := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		spelling, ok := strings.CutPrefix(line, "::add-mask::")
		require.True(t, ok, "the script printed %q", line)
		masked[spelling] = true
	}
	for _, spelling := range []string{
		first, second, allowlist,
		"Alice.A@example.test", "alice.a@example.test", "bob_b@example.test",
		"personal:alice.a@example.test", "personal%3AAlice.A%40example.test", "Alice.A%40example.test",
		"alice.a%40example.test", "personal-Alice-A-example-test", "personal_Alice.A@example.test",
		"business-bob-b-example-test", "business_bob_b@example.test",
	} {
		assert.True(t, masked[spelling], "%q is not masked", spelling)
	}

	list, err := readFile(filepath.Join(dir, ciTestMasksFile))
	require.NoError(t, err)
	assert.ElementsMatch(t, sortedKeys(masked), strings.Split(strings.TrimSpace(string(list)), "\n"))

	assert.ElementsMatch(t, []string{
		"ONEDRIVE_TEST_DRIVE=" + first,
		"ONEDRIVE_TEST_DRIVE_2=" + second,
		"ONEDRIVE_ALLOWED_TEST_ACCOUNTS=" + allowlist,
		"ONEDRIVE_TEST_DRIVE_VAULT_SECRET=onedrive-cache-personal-Alice-A-example-test",
		"ONEDRIVE_TEST_DRIVE_TOKEN_FILE=token_personal_Alice.A@example.test.json",
		"ONEDRIVE_TEST_DRIVE_2_VAULT_SECRET=onedrive-cache-business-bob-b-example-test",
		"ONEDRIVE_TEST_DRIVE_2_TOKEN_FILE=token_business_bob_b@example.test.json",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE=" + first,
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE=" + second,
	}, environment)

	// A job with one drive exports nothing for the second.
	single := t.TempDir()
	_, environment = runCIMaskScript(t, single, filepath.Join(single, "github-env"), map[string]string{
		"ONEDRIVE_TEST_DRIVE":            first,
		"ONEDRIVE_ALLOWED_TEST_ACCOUNTS": first,
	})
	assert.ElementsMatch(t, []string{
		"ONEDRIVE_TEST_DRIVE=" + first,
		"ONEDRIVE_ALLOWED_TEST_ACCOUNTS=" + first,
		"ONEDRIVE_TEST_DRIVE_VAULT_SECRET=onedrive-cache-personal-Alice-A-example-test",
		"ONEDRIVE_TEST_DRIVE_TOKEN_FILE=token_personal_Alice.A@example.test.json",
	}, environment)
}

// Validates: R-6.10.27
//
// A shortcut fixture may be mapped only to one of the job's drives, by
// name; any other value stops the job, naming the variable, not the value.
func TestCIMaskScript_RefusesAShortcutMappingToAnotherVariable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cmd := ciMaskScriptCommand(t, dir, filepath.Join(dir, "github-env"), map[string]string{
		"ONEDRIVE_TEST_DRIVE":                               "personal:alice@example.test",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE_FROM": "HOME",
	})
	var stderr strings.Builder
	cmd.Stderr = &stderr
	require.Error(t, cmd.Run())
	assert.Contains(t, stderr.String(), "ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE_FROM must name")
}

// ciMaskScriptCommand is the masking script, ready to run with only the given
// variables and the runner's.
func ciMaskScriptCommand(t *testing.T, runnerTemp, githubEnv string, vars map[string]string) *exec.Cmd {
	t.Helper()

	script := filepath.Join(repoRootFromDevtoolPackage(t), ciMaskScript)
	env := []string{"PATH=" + os.Getenv("PATH"), "RUNNER_TEMP=" + runnerTemp, "GITHUB_ENV=" + githubEnv}
	for name, value := range vars {
		env = append(env, name+"="+value)
	}
	cmd := exec.CommandContext(t.Context(), "bash", script) //nolint:gosec // the repository's own script under a fixed interpreter
	cmd.Env = env

	return cmd
}

// runCIMaskScript runs the masking script with only the given variables and
// the runner's, and returns what it printed and what it exported.
func runCIMaskScript(t *testing.T, runnerTemp, githubEnv string, vars map[string]string) (string, []string) {
	t.Helper()

	cmd := ciMaskScriptCommand(t, runnerTemp, githubEnv, vars)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "stderr: %s", stderr.String())
	assert.Empty(t, stderr.String())

	exported, err := readFile(githubEnv)
	require.NoError(t, err)

	return stdout.String(), strings.Split(strings.TrimSpace(string(exported)), "\n")
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// ciJobSteps splits a job's `steps:` list into its steps' lines.
func ciJobSteps(job ciWorkflowJob) [][]string {
	var steps [][]string
	inSteps := false
	for _, line := range job.lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			inSteps = line == "    steps:"
			continue
		}
		if !inSteps {
			continue
		}
		if strings.HasPrefix(line, "      - ") {
			steps = append(steps, []string{"        " + strings.TrimPrefix(line, "      - ")})
			continue
		}
		if len(steps) > 0 {
			steps[len(steps)-1] = append(steps[len(steps)-1], line)
		}
	}

	return steps
}

// ciBlockEntries returns the `KEY: value` entries of the map that header
// opens, two spaces deeper than the header.
func ciBlockEntries(lines []string, header string) map[string]string {
	entries := map[string]string{}
	indent := strings.Repeat(" ", len(header)-len(strings.TrimLeft(header, " "))+2)
	in := false
	for _, line := range lines {
		if line == header {
			in = true
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !in || trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(line, indent+" ") {
			continue
		}
		if !strings.HasPrefix(line, indent) {
			in = false
			continue
		}
		if key, value, ok := strings.Cut(trimmed, ": "); ok {
			entries[key] = value
		}
	}

	return entries
}

// ciStepRun returns a step's `run:` script, block or single line.
func ciStepRun(step []string) string {
	const runKey = "        run: "
	var script []string
	in := false
	for _, line := range step {
		if value, ok := strings.CutPrefix(line, runKey); ok {
			if value != "|" {
				return value
			}
			in = true
			continue
		}
		if in {
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "          ") {
				break
			}
			script = append(script, strings.TrimPrefix(line, "          "))
		}
	}

	return strings.Join(script, "\n")
}
