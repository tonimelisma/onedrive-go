package devtool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// nightlyTestOpenIssue is the number of the nightly issue a fixture has
	// open.
	nightlyTestOpenIssue = 1
	// nightlyTestReportJobID is the job id of the report job in the fixtures.
	nightlyTestReportJobID = 99
)

// nightlyReportJobEntry is the report job as its own run lists it: running.
func nightlyReportJobEntry() map[string]any {
	return map[string]any{"databaseId": nightlyTestReportJobID, "name": nightlyReportJob, "status": checkStatusInProgress, "conclusion": ""}
}

// redNightlyRemote is a scheduled run, its report job still running, whose
// e2e job failed one test.
func redNightlyRemote(t *testing.T) *claimRemote {
	t.Helper()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{nightlyTestE2EJob: checkConclusionFailure},
		nightlyReportJobEntry(), map[string]any{"databaseId": 98, "name": "verify-lint", "conclusion": checkConclusionSkipped})
	remote.jobLogs[nightlyTestE2ELog] = nightlyE2EFailureLog()

	return remote
}

// greenNightlyRemote is a scheduled run, its report job still running, in
// which every job the night requires passed.
func greenNightlyRemote(t *testing.T) *claimRemote {
	t.Helper()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, nil, nightlyReportJobEntry())

	return remote
}

// withOpenNightlyIssue gives the remote the label and one open nightly issue.
func withOpenNightlyIssue(remote *claimRemote) *claimRemote {
	remote.labels[nightlyIssueLabel] = true
	remote.issues = append(remote.issues, &fakeIssue{
		number: nightlyTestOpenIssue, title: nightlyIssueTitle, labels: []string{nightlyIssueLabel}, body: "earlier", open: true,
	})

	return remote
}

func runNightlyReport(t *testing.T, remote *claimRemote, dryRun bool) string {
	t.Helper()

	var stdout bytes.Buffer
	require.NoError(t, RunNightly(t.Context(), remote, NightlyOptions{
		RepoRoot: "/repo",
		Action:   NightlyActionReport,
		RunID:    nightlyTestRunID,
		DryRun:   dryRun,
		Stdout:   &stdout,
		Sleep:    remote.sleeps.sleep,
	}))

	return stdout.String()
}

// Validates: R-6.10.24
//
// The first red night opens one issue labeled nightly-failure, creating the
// label when the repository has none, and names the run, the failing job and
// its failed test.
func TestNightlyReportOpensIssueOnFirstRedRun(t *testing.T) {
	t.Parallel()

	remote := redNightlyRemote(t)
	runNightlyReport(t, remote, false)

	assert.True(t, remote.labels[nightlyIssueLabel], "the label is created when missing")
	require.Len(t, remote.issues, 1)
	issue := remote.issues[0]
	assert.True(t, issue.open)
	assert.Equal(t, nightlyIssueTitle, issue.title)
	assert.Equal(t, []string{nightlyIssueLabel}, issue.labels)
	assert.Contains(t, issue.body, nightlyTestRunURL)
	assert.Contains(t, issue.body, "`e2e`: failed")
	assert.Contains(t, issue.body, nightlyTestKey)
}

// Validates: R-6.10.24
//
// Another red night adds one comment to the open issue; it never opens a
// second one.
func TestNightlyReportCommentsOnTheOpenIssueWithoutDuplicating(t *testing.T) {
	t.Parallel()

	remote := withOpenNightlyIssue(redNightlyRemote(t))
	runNightlyReport(t, remote, false)

	require.Len(t, remote.issues, 1)
	issue := remote.issues[0]
	assert.True(t, issue.open)
	assert.Equal(t, "earlier", issue.body, "the issue's body stays as it was opened")
	require.Len(t, issue.comments, 1)
	assert.Contains(t, issue.comments[0], nightlyTestRunURL)
	assert.Contains(t, issue.comments[0], nightlyTestKey)
	assert.False(t, slices.ContainsFunc(remote.writes, func(write string) bool {
		return strings.HasPrefix(write, "gh label create")
	}), "an existing label is not created again")
}

// Validates: R-6.10.24
//
// The first green night closes the open issue with one comment naming the
// run.
func TestNightlyReportClosesIssueOnGreenRun(t *testing.T) {
	t.Parallel()

	remote := withOpenNightlyIssue(greenNightlyRemote(t))
	runNightlyReport(t, remote, false)

	require.Len(t, remote.issues, 1)
	issue := remote.issues[0]
	assert.False(t, issue.open)
	require.Len(t, issue.comments, 1)
	assert.Contains(t, issue.comments[0], nightlyTestRunURL)
	assert.Contains(t, issue.comments[0], "passed")

	quiet := greenNightlyRemote(t)
	runNightlyReport(t, quiet, false)
	assert.Empty(t, quiet.issues, "a green night with no open issue opens none")
}

// Validates: R-6.10.24
//
// Every report posts the commit status `nightly` on the run's head commit,
// linked to the run, so the commit the night ran on shows its verdict.
func TestNightlyReportPostsCommitStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		remote      func(t *testing.T) *claimRemote
		state       string
		description string
	}{
		{name: "green", remote: greenNightlyRemote, state: "success", description: "every nightly job passed"},
		{name: "red", remote: redNightlyRemote, state: "failure", description: "1 of 7 nightly jobs failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := tc.remote(t)
			runNightlyReport(t, remote, false)

			assert.Equal(t, []fakeStatus{{
				sha: nightlyTestRunHead, state: tc.state, context: nightlyStatusContext,
				targetURL: nightlyTestRunURL, description: tc.description,
			}}, remote.statuses)
		})
	}
}

// Validates: R-6.10.24
//
// A job the night requires that did not run is not a pass: a nightly without
// its live jobs is red.
func TestNightlyReportTreatsSkippedRequiredJobAsFailure(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{"integration": checkConclusionSkipped}, nightlyReportJobEntry())
	runNightlyReport(t, remote, false)

	require.Len(t, remote.statuses, 1)
	assert.Equal(t, "failure", remote.statuses[0].state)
	require.Len(t, remote.issues, 1)
	assert.Contains(t, remote.issues[0].body, "`integration`: did not run")
}

// Validates: R-6.10.24
//
// The report runs inside the run it reports on, so its own job is still
// running: it is neither a failure nor named.
func TestNightlyReportIgnoresItsOwnJob(t *testing.T) {
	t.Parallel()

	remote := withOpenNightlyIssue(greenNightlyRemote(t))
	runNightlyReport(t, remote, false)

	require.Len(t, remote.statuses, 1)
	assert.Equal(t, "success", remote.statuses[0].state)
	require.Len(t, remote.issues[0].comments, 1)
	assert.NotContains(t, remote.issues[0].comments[0], nightlyReportJob)
}

// Validates: R-6.10.24
//
// A dry run reads what it needs, prints the status and the issue text it
// would write, and writes nothing.
func TestNightlyReportDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	remote := redNightlyRemote(t)
	out := runNightlyReport(t, remote, true)

	assert.Empty(t, remote.writes)
	assert.Empty(t, remote.statuses)
	assert.Empty(t, remote.issues)
	assert.Contains(t, out, "dry run")
	assert.Contains(t, out, "would post commit status nightly=failure on "+nightlyTestRunHead)
	assert.Contains(t, out, "would create the label "+nightlyIssueLabel)
	assert.Contains(t, out, "would open an issue")
	assert.Contains(t, out, nightlyTestKey)

	open := withOpenNightlyIssue(greenNightlyRemote(t))
	out = runNightlyReport(t, open, true)
	assert.Empty(t, open.writes)
	assert.True(t, open.issues[0].open)
	assert.Contains(t, out, "would close issue #1")
}

// Validates: R-6.10.24
//
// Job logs can hold fixture names and account details, and the issue is
// public: its text is built only from job names, results, test names and the
// run's URL. Every other word of it is the report's own.
func TestNightlyReportIssueTextHoldsOnlyJobsTestsAndTheRunURL(t *testing.T) {
	t.Parallel()

	const marker = "fixture-folder-7f3a"
	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{
		nightlyTestE2EJob:            checkConclusionFailure,
		"stress":                     "timed_out",
		"integration":                checkConclusionSkipped,
		"verify-full (macos-latest)": checkConclusionFailure,
		"verify":                     checkConclusionFailure,
	}, nightlyReportJobEntry())
	remote.jobLogs[nightlyTestE2ELog] = "=== RUN   " + nightlyTestTest + "\n" +
		"    sync_test.go:40: upload /" + marker + "/a.txt: item " + marker + " not found\n" +
		"--- FAIL: " + nightlyTestTest + " (2.00s)\nFAIL\t" + repoModulePath + "/e2e\t2.100s\n"
	remote.jobLogs["13"] = "upload /" + marker + " stalled\n"
	remote.jobLogErrs["11"] = []error{ghFailure("HTTP 404: Not Found")}

	runNightlyReport(t, remote, false)
	require.Len(t, remote.issues, 1)
	body := remote.issues[0].body
	assert.NotContains(t, body, marker)

	shape := strings.ReplaceAll(body, nightlyTestRunURL, "<run>")
	shape = strings.ReplaceAll(shape, nightlyTestKey, "<test>")
	for _, job := range []string{"verify-full (macos-latest)", "integration", "stress", "verify", nightlyTestE2EJob} {
		shape = strings.ReplaceAll(shape, "`"+job+"`", "<job>")
	}
	assert.Equal(t, "The scheduled CI run failed: <run>\n\n"+
		"- <job>: failed; failed tests: `<test>`\n"+
		"- <job>: did not run\n"+
		"- <job>: timed out\n"+
		"- <job>: failed\n"+
		"- <job>: failed; its log could not be read (GitHub answered HTTP 404)\n\n"+
		"Each failure is claimed and fixed by an increment through `go run ./cmd/devtool nightly check`. "+
		"Only a scheduled run of main writes this issue, and it closes on the next one that passes; "+
		"a manual run posts its commit status only.\n", shape)
}

// Validates: R-6.10.24
//
// A job whose log names no failed test but names its failed steps is shown
// with the steps' keys, the words a claim takes, and never with a line of the
// log.
func TestNightlyReportIssueNamesTheFailedStepsOfAJobWithoutAFailedTest(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure}, nightlyReportJobEntry())
	remote.jobLogs["13"] = "upload stalled\n" + verifyStepFailedPrefix + "stress multisync-race-repeated\n" +
		verifyStepFailedPrefix + "stress sync-concurrency-race-repeated\n"

	runNightlyReport(t, remote, false)
	require.Len(t, remote.issues, 1)
	body := remote.issues[0].body
	assert.Contains(t, body, "- `stress`: failed; failed steps: "+
		"`step-stress-multisync-race-repeated`, `step-stress-sync-concurrency-race-repeated`\n")
	assert.NotContains(t, body, "upload stalled")
}

// Validates: R-6.10.24
//
// A go test deadline keys only the test its panic shows hung, and the issue
// says which of the failed tests hung; a stress member over its budget of
// one run is named as such beside the job's failed tests.
func TestNightlyReportIssueSaysWhichTestHungAndWhichMemberRanOverItsBudget(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{"stress": "timed_out"}, nightlyReportJobEntry())
	remote.jobLogs["13"] = overBudgetMarker("./internal/multisync", "TestOrchestrator_Drains") +
		"panic: test timed out after 40m0s\n\trunning tests:\n" +
		"\t\tTestAlpha_Parallel (12s)\n\t\tTestWatchSoak (39m59s)\n\n" +
		"FAIL\t" + repoModulePath + "/internal/multisync\t2400.100s\n"

	runNightlyReport(t, remote, false)
	require.Len(t, remote.issues, 1)
	assert.Contains(t, remote.issues[0].body, "- `stress`: timed out; failed tests: `internal/multisync.TestWatchSoak`; "+
		"hung at the go test deadline: `internal/multisync.TestWatchSoak`; "+
		"over the stress budget of one run: `internal/multisync.TestOrchestrator_Drains`\n")
	assert.NotContains(t, remote.issues[0].body, "TestAlpha_Parallel")
}

// Validates: R-6.10.24
//
// A job whose log names failed tests and a failed step that runs no go test
// is shown with both: the tests' keys and the step's key, the words each
// claim takes.
func TestNightlyReportIssueNamesAStepOutsideGoTestBesideTheFailedTests(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{nightlyTestUbuntuLeg: checkConclusionFailure}, nightlyReportJobEntry())
	remote.jobLogs["10"] = nightlyTestPlannerFailure +
		verifyStepFailedPrefix + "public unit-tests\n" + verifyStepFailedPrefix + "public lint\n"

	runNightlyReport(t, remote, false)
	require.Len(t, remote.issues, 1)
	assert.Contains(t, remote.issues[0].body, "- `"+nightlyTestUbuntuLeg+"`: failed; failed tests: `internal/sync.TestPlanner`; "+
		"failed steps: `step-verify-full--ubuntu-latest-lint`\n")
}

// Validates: R-6.10.24
//
// A rerun of the report job reports the same run again. It posts the same
// commit status, and it writes nothing to the issue that the issue already
// says: no second issue, and no comment naming the run a second time.
func TestNightlyReportRunTwiceWritesTheIssueOnce(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		remote   func(t *testing.T) *claimRemote
		comments int
	}{
		"a red night opens the issue":       {remote: redNightlyRemote, comments: 0},
		"a red night comments on the issue": {remote: redNightlyRemoteWithOpenIssue, comments: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := tc.remote(t)
			runNightlyReport(t, remote, false)
			first := nightlyIssueWrites(remote)
			runNightlyReport(t, remote, false)

			assert.Equal(t, first, nightlyIssueWrites(remote), "the second report writes nothing to the issue")
			require.Len(t, remote.issues, 1)
			assert.Len(t, remote.issues[0].comments, tc.comments)
			require.Len(t, remote.statuses, 2)
			assert.Equal(t, remote.statuses[0], remote.statuses[1], "the same status is posted again")
		})
	}
}

// redNightlyRemoteWithOpenIssue is redNightlyRemote with the nightly issue
// already open from an earlier night.
func redNightlyRemoteWithOpenIssue(t *testing.T) *claimRemote {
	t.Helper()

	return withOpenNightlyIssue(redNightlyRemote(t))
}

// nightlyIssueWrites are the remote's writes to the nightly issue and its
// label.
func nightlyIssueWrites(remote *claimRemote) []string {
	writes := []string{}
	for _, write := range remote.writes {
		if strings.HasPrefix(write, "gh issue ") || strings.HasPrefix(write, "gh label ") {
			writes = append(writes, write)
		}
	}

	return writes
}

// Validates: R-6.10.24
//
// The issue tells the nightly of main, which is what an increment acts on,
// so only a scheduled run of main writes it. A manual run with writes turned
// on posts its commit status and never touches the issue: it opens none,
// comments on none, closes none and creates no label.
func TestNightlyReportWritesTheIssueForAScheduledRunOfMainOnly(t *testing.T) {
	t.Parallel()

	red := map[string]string{nightlyTestE2EJob: checkConclusionFailure}
	cases := map[string]struct {
		event, branch string
		conclusions   map[string]string
		openIssue     bool
		wantIssues    int
	}{
		"a red scheduled run of main":    {event: ciEventSchedule, branch: mainBranchName, conclusions: red, wantIssues: 1},
		"a red manual run of main":       {event: ciEventWorkflowDispatch, branch: mainBranchName, conclusions: red},
		"a red manual run of a branch":   {event: ciEventWorkflowDispatch, branch: nightlyTestOwner, conclusions: red},
		"a green manual run, issue open": {event: ciEventWorkflowDispatch, branch: mainBranchName, openIssue: true, wantIssues: 1},
		"a red manual run, issue open":   {event: ciEventWorkflowDispatch, branch: mainBranchName, conclusions: red, openIssue: true, wantIssues: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := newClaimRemote(t, "")
			remote.jobs = nightlyRunViewOf(t, tc.event, tc.branch, tc.conclusions, nightlyReportJobEntry())
			remote.jobLogs[nightlyTestE2ELog] = nightlyE2EFailureLog()
			if tc.openIssue {
				withOpenNightlyIssue(remote)
			}
			out := runNightlyReport(t, remote, false)

			require.Len(t, remote.statuses, 1, "every run posts its commit status")
			require.Len(t, remote.issues, tc.wantIssues)
			if tc.event == ciEventSchedule {
				return
			}
			assert.Empty(t, nightlyIssueWrites(remote))
			if tc.openIssue {
				assert.True(t, remote.issues[0].open)
				assert.Empty(t, remote.issues[0].comments)
			}
			assert.Contains(t, out, "does not write the "+nightlyIssueLabel+" issue")
		})
	}
}

// Validates: R-6.10.24
//
// Only the newest scheduled run of main writes the issue, so a rerun of an
// older night, or a report that ends after the next night's run was created,
// never writes an older verdict over the newest. The report asks which run
// is the newest with the run list the nightly check reads. An older run's
// report posts its commit status, leaves the issue as it is and says so, in
// a dry run too; the newest run's report writes the issue while its own run
// is still running. The run reported on is the one the command was given,
// whatever its run view decodes.
func TestNightlyReportWritesTheIssueForTheNewestScheduledRunOnly(t *testing.T) {
	t.Parallel()

	const newerRunID = nightlyTestRunID + 1
	greenWithOpenIssue := func(t *testing.T) *claimRemote {
		t.Helper()
		return withOpenNightlyIssue(greenNightlyRemote(t))
	}
	viewWithoutID := func(t *testing.T) *claimRemote {
		t.Helper()
		remote := redNightlyRemoteWithOpenIssue(t)
		remote.jobs = withNightlyRunField(t, remote.jobs, "databaseId", nil)
		return remote
	}
	cases := map[string]struct {
		remote func(t *testing.T) *claimRemote
		newest int64
	}{
		"an older red run, issue open":          {remote: redNightlyRemoteWithOpenIssue, newest: newerRunID},
		"an older green run, issue open":        {remote: greenWithOpenIssue, newest: newerRunID},
		"an older red run, no issue":            {remote: redNightlyRemote, newest: newerRunID},
		"the newest red run, issue open":        {remote: redNightlyRemoteWithOpenIssue, newest: nightlyTestRunID},
		"the newest red run, its view of no id": {remote: viewWithoutID, newest: nightlyTestRunID},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := tc.remote(t)
			remote.runs = nightlyRunList(t, nightlyListedRun{id: tc.newest, status: checkStatusInProgress, age: time.Hour})
			issues := len(remote.issues)
			dry := runNightlyReport(t, remote, true)
			out := runNightlyReport(t, remote, false)

			require.Len(t, remote.runListArgs, 2, "each report reads the run list once")
			flags := ghFields(remote.runListArgs[1])
			assert.Equal(t, map[string]string{
				"--workflow": nightlyWorkflowFile, "--event": ciEventSchedule, "--branch": mainBranchName,
				"--limit": nightlyRunListLimit, "--json": nightlyRunListFields,
			}, flags)
			require.Len(t, remote.statuses, 1, "every run posts its commit status")
			if tc.newest == nightlyTestRunID {
				require.Len(t, remote.issues, 1)
				assert.Len(t, remote.issues[0].comments, 1, "the newest run writes the issue")
				return
			}
			assert.Empty(t, nightlyIssueWrites(remote), "an older run opens, comments on and closes nothing")
			require.Len(t, remote.issues, issues)
			for _, issue := range remote.issues {
				assert.True(t, issue.open)
				assert.Empty(t, issue.comments)
			}
			notNewest := fmt.Sprintf("scheduled run %d of %s is not the newest (run %d is)", nightlyTestRunID, mainBranchName, newerRunID)
			assert.Contains(t, dry, notNewest)
			assert.Contains(t, out, notNewest)
		})
	}
}

// Validates: R-6.10.24
//
// The newest scheduled run is the one created last, whatever order GitHub
// lists the runs in.
func TestNightlyReportFindsTheNewestRunByItsCreation(t *testing.T) {
	t.Parallel()

	remote := redNightlyRemoteWithOpenIssue(t)
	remote.runs = nightlyRunList(t,
		nightlyListedRun{id: nightlyTestRunID - 1, status: checkStatusCompleted, conclusion: checkConclusionFailure, age: 2 * time.Hour},
		nightlyListedRun{id: nightlyTestRunID, status: checkStatusInProgress, age: time.Hour},
	)
	runNightlyReport(t, remote, false)

	require.Len(t, remote.issues, 1)
	assert.Len(t, remote.issues[0].comments, 1, "the run created last is the newest, listed first or not")
}

// Validates: R-6.10.24
//
// A report that cannot read which scheduled run is the newest, after the
// client's retries, does not know whether its verdict is the newest: it
// fails with the client's plain sentence and leaves the issue as it is. A
// list that names no run, not even the one reported on, is no answer either.
// The commit status depends on the run alone, so it is posted first, once.
func TestNightlyReportPostsOnlyItsStatusWhenTheNewestRunIsUnknown(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		failures []error
		runs     string
		want     []string
	}{
		"GitHub keeps failing the run list": {
			failures: slices.Repeat([]error{githubBadGateway()}, githubAttempts),
			want:     []string{"nightly run list", fmt.Sprintf("GitHub answered HTTP 502 on all %d attempts", githubAttempts)},
		},
		"GitHub lists no scheduled run": {
			runs: "[]",
			want: []string{"GitHub lists no scheduled run of " + mainBranchName, "the issue is left as it is"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := redNightlyRemoteWithOpenIssue(t)
			remote.runListErrs = tc.failures
			if tc.runs != "" {
				remote.runs = tc.runs
			}
			assertNightlyReportPostsOnlyItsStatus(t, remote, tc.want...)
		})
	}
}

// Validates: R-6.10.24
//
// A report of the newest run that cannot read the issue, after the client's
// retries, fails with the client's plain sentence, its commit status posted
// once and the issue as it is.
func TestNightlyReportPostsOnlyItsStatusWhenTheIssueCannotBeRead(t *testing.T) {
	t.Parallel()

	remote := redNightlyRemoteWithOpenIssue(t)
	remote.labelListErrs = slices.Repeat([]error{githubBadGateway()}, githubAttempts)
	assertNightlyReportPostsOnlyItsStatus(t, remote,
		"nightly label (GET", fmt.Sprintf("GitHub answered HTTP 502 on all %d attempts", githubAttempts))
}

// assertNightlyReportPostsOnlyItsStatus runs a report of the red run of the
// remote, whose nightly issue is open, and asserts that it fails with an
// error holding every one of want, after one write, the commit status, with
// the issue as it was.
func assertNightlyReportPostsOnlyItsStatus(t *testing.T, remote *claimRemote, want ...string) {
	t.Helper()

	var stdout bytes.Buffer
	err := RunNightly(t.Context(), remote, NightlyOptions{
		RepoRoot: "/repo",
		Action:   NightlyActionReport,
		RunID:    nightlyTestRunID,
		Stdout:   &stdout,
		Sleep:    remote.sleeps.sleep,
	})

	require.Error(t, err)
	for _, text := range want {
		assert.Contains(t, err.Error(), text)
	}
	assert.Len(t, remote.writes, 1, "the commit status is the one write")
	require.Len(t, remote.statuses, 1)
	assert.Equal(t, nightlyStatusFailure, remote.statuses[0].state)
	assert.Empty(t, nightlyIssueWrites(remote))
	require.Len(t, remote.issues, 1)
	assert.True(t, remote.issues[0].open)
	assert.Empty(t, remote.issues[0].comments)
}

// Validates: R-6.10.24
//
// A green report whose comment the open issue already holds, from an earlier
// attempt that commented and then failed to close it, closes the issue and
// posts no second comment.
func TestNightlyReportClosesAnIssueThatHoldsItsCommentWithoutCommenting(t *testing.T) {
	t.Parallel()

	remote := withOpenNightlyIssue(greenNightlyRemote(t))
	remote.issues[0].comments = []string{"The scheduled CI run passed: " + nightlyTestRunURL + "\n\nan earlier attempt\n"}
	out := runNightlyReport(t, remote, false)

	assert.False(t, remote.issues[0].open)
	assert.Len(t, remote.issues[0].comments, 1)
	assert.False(t, slices.ContainsFunc(remote.writes, func(write string) bool {
		return strings.HasPrefix(write, "gh issue comment")
	}), "the held comment is not posted again")
	assert.Contains(t, out, "which already holds the comment")
}

// Validates: R-6.10.24
//
// The issue holds a run's verdict only as a whole line: a line naming a run
// whose URL starts with this run's URL, or that this run's URL starts with,
// is another run's verdict, so the report still comments.
func TestNightlyReportMatchesTheHeldVerdictLineWhole(t *testing.T) {
	t.Parallel()

	const runs = "https://example.invalid/actions/runs/"
	cases := map[string]struct {
		run, held string
		red       bool
	}{
		"a green run of a longer URL":  {run: runs + "1234", held: runs + "123"},
		"a green run of a shorter URL": {run: runs + "123", held: runs + "1234"},
		"a red run of a longer URL":    {run: runs + "1234", held: runs + "123", red: true},
		"a red run of a shorter URL":   {run: runs + "123", held: runs + "1234", red: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote, verdict := withOpenNightlyIssue(greenNightlyRemote(t)), "passed"
			if tc.red {
				remote, verdict = redNightlyRemoteWithOpenIssue(t), "failed"
			}
			remote.jobs = withNightlyRunField(t, remote.jobs, "url", tc.run)
			remote.issues[0].comments = []string{"The scheduled CI run " + verdict + ": " + tc.held + "\n"}
			runNightlyReport(t, remote, false)

			require.Len(t, remote.issues[0].comments, 2, "the held line names another run")
			assert.Contains(t, remote.issues[0].comments[1], "The scheduled CI run "+verdict+": "+tc.run+"\n")
		})
	}
}

// withNightlyRunField is a run view with one field of its run replaced.
func withNightlyRunField(t *testing.T, view string, field string, value any) string {
	t.Helper()

	var run map[string]any
	require.NoError(t, json.Unmarshal([]byte(view), &run))
	run[field] = value
	out, err := json.Marshal(run)
	require.NoError(t, err)

	return string(out)
}

// Validates: R-6.10.24
//
// A required job canceled before a runner picked it up did not run: the
// night is red, and the commit status and the issue say which job did not
// run. The job has no log to read.
func TestNightlyReportNamesAJobThatDidNotRun(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, nil, nightlyQueuedJob(nightlyTestE2EJob), nightlyReportJobEntry())
	runNightlyReport(t, remote, false)

	require.Len(t, remote.statuses, 1)
	assert.Equal(t, nightlyStatusFailure, remote.statuses[0].state)
	assert.Equal(t, "did not run: e2e", remote.statuses[0].description)
	require.Len(t, remote.issues, 1)
	assert.Contains(t, remote.issues[0].body, "- `e2e`: did not run\n")
	assert.Zero(t, remote.jobLogCalls[nightlyTestE2ELog])

	both := newClaimRemote(t, "")
	both.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure},
		nightlyQueuedJob(prCheckIntegration), nightlyReportJobEntry())
	runNightlyReport(t, both, false)
	require.Len(t, both.statuses, 1)
	assert.Equal(t, "1 of 7 nightly jobs failed; did not run: integration", both.statuses[0].description)

	const legs = 12
	many := nightlyRunReport{}
	for leg := range legs {
		many.jobs = append(many.jobs, nightlyJobReport{name: fmt.Sprintf("verify-full (ubuntu-latest, leg %d)", leg), result: nightlyJobNotRun})
	}
	description := many.statusDescription()
	assert.LessOrEqual(t, len(description), nightlyStatusDescriptionMax, "GitHub refuses a longer status description")
	assert.True(t, strings.HasPrefix(description, "did not run: verify-full (ubuntu-latest, leg 0), "))
	assert.True(t, strings.HasSuffix(description, " more"))
}

// Validates: R-6.10.24
//
// The report job's decisions are Go; the workflow only has to run it after
// every job it reports on. Every job the night requires is a job of ci.yml
// that runs on the schedule under its own name, the report job needs each,
// runs on the schedule and manual dispatch only, even after a failure, and
// holds the smallest permissions and no secret or variable.
func TestNightlyRequiredJobsExistInWorkflow(t *testing.T) {
	t.Parallel()

	jobs, err := readCIWorkflowJobs(repoRootFromDevtoolPackage(t))
	require.NoError(t, err)
	byName := map[string]ciWorkflowJob{}
	for _, job := range jobs {
		byName[job.name] = job
	}
	report, ok := byName[nightlyReportJob]
	require.True(t, ok, "ci.yml has no %s job", nightlyReportJob)

	needs, err := report.needs()
	require.NoError(t, err)
	scheduled := []string{}
	for _, job := range jobs {
		runs, runsErr := job.runsOn(ciEventSchedule)
		require.NoError(t, runsErr)
		if runs && job.name != nightlyReportJob {
			scheduled = append(scheduled, job.name)
		}
	}
	assert.ElementsMatch(t, scheduled, nightlyRequiredJobs(), "every scheduled job is required, and only those")
	for _, required := range nightlyRequiredJobs() {
		job, exists := byName[required]
		require.True(t, exists, "required job %s is not a job of ci.yml", required)
		_, renamed, keyErr := job.key(ciJobNameKey)
		require.NoError(t, keyErr)
		assert.False(t, renamed, "job %s sets name:, so its runs would not carry its key", required)
		assert.Contains(t, needs, required, "%s does not need %s", nightlyReportJob, required)
	}

	for event, want := range map[string]bool{
		ciEventSchedule: true, ciEventWorkflowDispatch: true, ciEventPullRequest: false, ciEventPush: false,
	} {
		runs, runsErr := report.runsOn(event)
		require.NoError(t, runsErr)
		assert.Equal(t, want, runs, "%s on %s", nightlyReportJob, event)
	}
	condition, _, err := report.key(ciJobIfKey)
	require.NoError(t, err)
	assert.Contains(t, condition, ciAlwaysCondition, "the report runs after a failed job too")

	assert.ElementsMatch(t, []string{"contents: read", "actions: read", "issues: write", "statuses: write"},
		report.blockEntries(ciJobPermissionsKey),
		"a permissions block sets every permission it does not name to none: the job reads the code and the run, "+
			"and writes statuses and issues, nothing else")
	assert.ElementsMatch(t, []string{"group: " + nightlyReportJob, "cancel-in-progress: false"},
		report.blockEntries(ciJobConcurrencyKey), "one report runs at a time, and a running report is never canceled")
	assertNightlyReportDryRun(t, report)

	aggregate, ok := byName[nightlyAggregateJob]
	require.True(t, ok)
	aggregateNeeds, err := aggregate.needs()
	require.NoError(t, err)
	assert.ElementsMatch(t, nightlyAggregateNeeds(), aggregateNeeds, "the aggregate repeats exactly the jobs it waits for")
	for _, line := range report.lines {
		assert.NotContains(t, line, "secrets.", "the report job reads no repository secret")
		assert.NotContains(t, line, "vars.", "the report job reads no repository variable")
	}
}

// assertNightlyReportDryRun holds when the report job writes: a scheduled run
// always does, and a manual run is a dry run unless its dry_run input, a
// boolean that defaults to true, is turned off.
func assertNightlyReportDryRun(t *testing.T, report ciWorkflowJob) {
	t.Helper()

	const dryRunEnv = "DRY_RUN:"
	dryRun, passed := "", false
	for _, line := range report.lines {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), dryRunEnv); ok {
			dryRun = strings.TrimSpace(value)
		}
		passed = passed || strings.Contains(line, `--dry-run="${DRY_RUN}"`)
	}
	assert.Equal(t, "${{ github.event_name != 'schedule' && inputs.dry_run }}", dryRun,
		"a scheduled run always writes; a manual run follows its dry_run input")
	assert.True(t, passed, "the report command takes DRY_RUN as its --dry-run")

	workflow, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), ciWorkflowPath))
	require.NoError(t, err)
	input := ciDispatchInput(string(workflow), "dry_run")
	assert.Contains(t, input, "type: boolean")
	assert.Contains(t, input, "default: true", "a manual run is a dry run unless its input is turned off")
}

// ciDispatchInput is the trimmed lines of one workflow_dispatch input of the
// workflow: the lines indented under its key, before the jobs.
func ciDispatchInput(workflow string, name string) []string {
	lines := []string{}
	indent := -1
	for line := range strings.Lines(workflow) {
		text := strings.TrimRight(line, "\n")
		trimmed := strings.TrimSpace(text)
		depth := len(text) - len(strings.TrimLeft(text, " "))
		switch {
		case text == ciJobsKey:
			return lines
		case indent < 0 && trimmed == name+":":
			indent = depth
		case indent >= 0 && trimmed != "" && depth <= indent:
			return lines
		case indent >= 0 && trimmed != "":
			lines = append(lines, trimmed)
		}
	}

	return lines
}

// Validates: R-6.10.24
//
// A push run and a scheduled run of one commit no longer queue behind or
// cancel each other: the workflow's concurrency group is per event. Each
// live job keeps one fixed group across events, because two live suites on
// one account break each other.
func TestCIWorkflowGroupsRunsPerEventAndLiveJobsPerAccount(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), ciWorkflowPath))
	require.NoError(t, err)
	workflow := string(data)
	assert.Contains(t, workflow, "\nconcurrency:\n"+
		"  group: ci-${{ github.event_name }}-${{ github.event_name == 'pull_request' && github.head_ref || github.sha }}\n"+
		"  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n",
		"a newer push to a pull request cancels its older run; no push, scheduled or manual run is canceled")

	groups := map[string]string{}
	for _, job := range ciWorkflowJobs(workflow) {
		if job.name != prCheckIntegration && job.name != prCheckE2E {
			continue
		}
		group, inGroup, groupErr := job.concurrencyGroup()
		require.NoError(t, groupErr, "a live job's group is a plain name, the same on every event")
		require.True(t, inGroup, "live job %s has no concurrency group", job.name)
		assert.Contains(t, job.blockEntries(ciJobConcurrencyKey), "cancel-in-progress: false",
			"a running live suite is never canceled by the next one")
		groups[job.name] = group
	}
	require.Len(t, groups, 2)
	assert.NotEqual(t, groups[prCheckIntegration], groups[prCheckE2E], "each live job has its own account")
}

// Validates: R-6.10.24
//
// The README's CI badge shows the scheduled run of main, the full tier, not
// whichever run happened last.
func TestREADMEBadgeShowsTheScheduledRunOfMain(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repoRootFromDevtoolPackage(t), "README.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "/actions/workflows/ci.yml/badge.svg?branch=main&event=schedule)")
}

// Validates: R-6.10.24
//
// The report's issue says only that a log could not be read, in its own
// words. Why the read failed went nowhere, so a failure in CI could not be
// diagnosed from the run: the report's job log, which only the run's
// readers see, now carries gh's own words, one line per job, with every
// URL's query cut, since a log's redirect carries a signed token.
func TestNightlyReportPrintsWhyAJobLogCouldNotBeRead(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure}, nightlyReportJobEntry())
	failure := ghFailure("error connecting to https://blob.example.net/logs/13.txt?sv=2026&sig=SECRET\nsecond line")
	remote.jobLogErrs["13"] = slices.Repeat([]error{failure}, githubAttempts)

	out := runNightlyReport(t, remote, false)

	assert.Contains(t, out, "nightly report: the log of stress could not be read: ")
	assert.Contains(t, out, "gh [run view 1]: error connecting to https://blob.example.net/logs/13.txt?<query removed>\n")
	assert.NotContains(t, out, "SECRET")
	assert.NotContains(t, out, "second line")
	require.Len(t, remote.issues, 1)
	assert.NotContains(t, remote.issues[0].body, "blob.example.net", "the public issue keeps the report's own words")
}
