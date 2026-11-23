package devtool

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// nightlyTestSeedsLog is the model-seeds job's id in the fixtures.
	nightlyTestSeedsLog = "15"
	// nightlyTestEarlierRun is an earlier night's run URL.
	nightlyTestEarlierRun = nightlyTestRunURL + "-earlier"
)

// nightlyTestSeedKeys are the keys of the fixture seeds, testEngineReplay's
// and testPlannerReplay's.
func nightlyTestSeedKeys() (string, string) {
	return "internal/sync.TestRunOnce_RandomizedBidirectionalEditsConverge/ONEDRIVE_GO_MODEL_SEED=7001",
		"internal/sync/internal/syncplan.TestPlannerModel_RandomTriplesConverge/ONEDRIVE_GO_PLANNER_SEED=12"
}

// seedFailureLog is the raw log of a job in which each replay's seed failed,
// behind the timestamps CI adds, with extra in the failing tests' own output.
// Each package's tests end with that package's FAIL line, as go test prints
// them.
func seedFailureLog(t *testing.T, extra string, commands ...string) string {
	t.Helper()

	var b strings.Builder
	for i, command := range commands {
		replay, ok := parseSeedReplay(command)
		require.True(t, ok, command)
		seed := replay.assignments[0].value
		b.WriteString("2026-11-14T10:40:00Z === RUN   " + replay.test + "/seed=" + seed + "\n")
		b.WriteString("2026-11-14T10:41:00Z         model_test.go:90: " + extra + "\n")
		b.WriteString("2026-11-14T10:41:00Z         test_helpers_test.go:480: " + seedReplayPrefix + command + "\n")
		b.WriteString("2026-11-14T10:41:00Z     --- FAIL: " + replay.test + "/seed=" + seed + " (9.00s)\n")
		b.WriteString("2026-11-14T10:41:00Z --- FAIL: " + replay.test + " (9.00s)\n")
		if i+1 < len(commands) {
			if next, nextOK := parseSeedReplay(commands[i+1]); nextOK && next.pkg == replay.pkg {
				continue
			}
		}
		b.WriteString("2026-11-14T10:41:01Z FAIL\t" + repoModulePath + strings.TrimPrefix(replay.pkg, ".") + "\t9.100s\n")
	}

	return b.String()
}

// seedNightlyRemote is a scheduled run, its report job still running, whose
// model-seeds job failed on the engine and planner fixture seeds.
func seedNightlyRemote(t *testing.T) *claimRemote {
	t.Helper()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{nightlySeedsJob: checkConclusionFailure}, nightlyReportJobEntry())
	remote.jobLogs[nightlyTestSeedsLog] = seedFailureLog(t, "converged after 3 passes", testEngineReplay, testPlannerReplay)

	return remote
}

// withOpenSeedIssue gives the remote the seed label and one open issue for
// key, opened by an earlier night.
func withOpenSeedIssue(remote *claimRemote, key string, body string) *fakeIssue {
	remote.labels[seedIssueLabel] = true
	issue := &fakeIssue{
		number: len(remote.issues) + 1, title: seedIssueTitlePrefix + key, labels: []string{seedIssueLabel}, body: body, open: true,
	}
	remote.issues = append(remote.issues, issue)

	return issue
}

// seedIssuesOf are the remote's issues labeled seed-failure.
func seedIssuesOf(remote *claimRemote) []*fakeIssue {
	issues := []*fakeIssue{}
	for _, issue := range remote.issues {
		if len(issue.labels) == 1 && issue.labels[0] == seedIssueLabel {
			issues = append(issues, issue)
		}
	}

	return issues
}

// Validates: R-6.10.25
//
// A night on which seeds failed opens one issue per failing seed, labeled
// seed-failure, creating the label once, and titled by the seed's key; the
// night is red, so the nightly issue opens too.
func TestNightlyReportOpensOneIssuePerFailingSeed(t *testing.T) {
	t.Parallel()

	remote := seedNightlyRemote(t)
	runNightlyReport(t, remote, false)

	engine, planner := nightlyTestSeedKeys()
	require.Len(t, remote.statuses, 1)
	assert.Equal(t, nightlyStatusFailure, remote.statuses[0].state, "a failing seed makes the night red")
	assert.True(t, remote.labels[seedIssueLabel], "the label is created when missing")
	issues := seedIssuesOf(remote)
	require.Len(t, issues, 2)
	assert.Equal(t, seedIssueTitlePrefix+engine, issues[0].title)
	assert.Equal(t, seedIssueTitlePrefix+planner, issues[1].title)
	assert.Contains(t, issues[0].body, "    "+seedReplayPrefix+testEngineReplay+"\n")
	assert.Contains(t, issues[1].body, "    "+seedReplayPrefix+testPlannerReplay+"\n")
	labelCreates := 0
	for _, write := range remote.writes {
		if strings.HasPrefix(write, "gh label create "+seedIssueLabel) {
			labelCreates++
		}
	}
	assert.Equal(t, 1, labelCreates)
	require.Len(t, remote.issues, len(issues)+1, "and the nightly issue")
	assert.Contains(t, remote.issues[0].body, "failed tests: `"+engine+"`, `"+planner+"`")
}

// Validates: R-6.10.25
//
// A seed that fails on a second night keeps its one issue, which gets one
// comment naming the run and nothing else; a rerun of that report adds none.
func TestNightlyReportCommentsOnceOnASeedThatFailsAgain(t *testing.T) {
	t.Parallel()

	engine, planner := nightlyTestSeedKeys()
	remote := seedNightlyRemote(t)
	earlier := withOpenSeedIssue(remote, engine, "The seed failed in the scheduled CI run "+nightlyTestEarlierRun+".\n")
	runNightlyReport(t, remote, false)
	runNightlyReport(t, remote, false)

	require.Equal(t, []string{renderSeedRecurrence(nightlyTestRunURL)}, earlier.comments, "one comment, the run's link")
	assert.Equal(t, "The seed failed again in the scheduled CI run "+nightlyTestRunURL+".\n", earlier.comments[0])
	assert.True(t, earlier.open)
	issues := seedIssuesOf(remote)
	require.Len(t, issues, 2, "the planner seed's new issue beside the engine seed's old one")
	assert.Equal(t, seedIssueTitlePrefix+planner, issues[1].title)
	assert.Empty(t, issues[1].comments, "the second report finds the run named in the issue it opened")
}

// Validates: R-6.10.25
//
// No report closes a seed issue: a green night, or a red one that did not
// run the seed, says nothing about a seed of an earlier range.
func TestNightlyReportNeverClosesSeedIssues(t *testing.T) {
	t.Parallel()

	engine, _ := nightlyTestSeedKeys()
	for name, remote := range map[string]*claimRemote{"a green night": greenNightlyRemote(t), "a red night": redNightlyRemote(t)} {
		issue := withOpenSeedIssue(remote, engine, "earlier")
		runNightlyReport(t, remote, false)

		assert.True(t, issue.open, name)
		assert.Empty(t, issue.comments, name)
		for _, write := range remote.writes {
			assert.NotContains(t, write, "issue close "+strconv.Itoa(issue.number), name)
		}
	}
}

// Validates: R-6.10.25
//
// The issue is public and a job log can hold fixture names: its text is built
// only from the seed's variable, the seed, its test, the CI job it failed in,
// the replay command rebuilt from the words devtool parsed, and the run's
// URL.
func TestNightlyReportSeedIssueTextHoldsOnlyTheSeedAndTheRunURL(t *testing.T) {
	t.Parallel()

	const marker = "fixture-folder-7f3a"
	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{nightlySeedsJob: checkConclusionFailure}, nightlyReportJobEntry())
	remote.jobLogs[nightlyTestSeedsLog] = seedFailureLog(t, "upload /"+marker+"/a.txt failed", testEngineReplay) +
		"2026-11-14T10:41:02Z " + seedReplayPrefix + "ONEDRIVE_GO_MODEL_SEED=1 go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./" +
		marker + "/x " + marker + "\n"
	runNightlyReport(t, remote, false)

	engine, _ := nightlyTestSeedKeys()
	issues := seedIssuesOf(remote)
	require.Len(t, issues, 1, "a line that is not a replay is not a seed")
	body := issues[0].body
	assert.NotContains(t, body, marker)
	shape := strings.ReplaceAll(body, nightlyTestRunURL, "<run>")
	shape = strings.ReplaceAll(shape, seedReplayPrefix+testEngineReplay, "<replay line>")
	shape = strings.ReplaceAll(shape, "internal/sync.TestRunOnce_RandomizedBidirectionalEditsConverge", "<test>")
	assert.Equal(t, "The seeded model test `<test>` fails on `ONEDRIVE_GO_MODEL_SEED=7001` in the scheduled CI run <run>.\n"+
		"It failed in the CI job `"+nightlySeedsJob+"`.\n\n"+
		"The test printed this line; the command after `seed-replay: ` replays the seed. "+
		"Run it at the commit the run tested: what a seed does changes with the code.\n\n"+
		"    <replay line>\n\n"+
		"No report closes this issue: each night runs other seeds, so a green night says nothing about this one. "+
		"The change that fixes the seed closes it and keeps the seed as a named test.\n", shape)
	assert.Equal(t, seedIssueTitlePrefix+engine, issues[0].title)
}

// Validates: R-6.10.23, R-6.10.25
//
// A failing seed is keyed by its replay line, its test's key with the
// family's variable and the seed, in place of its test; a failed test whose
// log names no seed of it keeps its test's key.
func TestNightlyFailureKeysKeyAFailingSeedByItsReplay(t *testing.T) {
	t.Parallel()

	const other = "TestSeedRun_AppliesOneRuleToEveryFamily"
	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{nightlySeedsJob: checkConclusionFailure})
	remote.jobLogs[nightlyTestSeedsLog] = strings.Replace(seedFailureLog(t, "", testEngineReplay),
		"FAIL\t", "--- FAIL: "+other+" (0.00s)\nFAIL\t", 1)

	engine, _ := nightlyTestSeedKeys()
	assert.Equal(t, map[string][]string{
		engine:                   {nightlySeedsJob},
		"internal/sync." + other: {nightlySeedsJob},
	}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.25
//
// An open seed issue is a failing item whatever the night did: the check
// claims its key for the increment by the same compare-and-swap claim as a
// failing test, and prints the command that replays the seed beside its
// issue. A closed issue's seed is fixed and is not listed.
func TestNightlyCheckClaimsOpenSeedIssues(t *testing.T) {
	t.Parallel()

	engine, planner := nightlyTestSeedKeys()
	replay, ok := parseSeedReplay(testEngineReplay)
	require.True(t, ok)
	remote := newClaimRemote(t, checkConclusionSuccess)
	open := withOpenSeedIssue(remote, engine, renderSeedIssue(failingSeed{replay: replay, jobs: []string{nightlySeedsJob}}, nightlyTestEarlierRun))
	closed := withOpenSeedIssue(remote, planner, "fixed")
	closed.open = false

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, claimed := remote.claim(engine)
	require.True(t, claimed, "the open seed issue's key is claimed")
	assert.Equal(t, nightlyStateClaimed, claim.State)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	_, claimedClosed := remote.claim(planner)
	assert.False(t, claimedClosed, "a closed seed issue is not claimed")
	number := strconv.Itoa(open.number)
	assert.Contains(t, out, "  "+engine+": claimed by this increment; fix it here (new claim)\n"+
		"      replay: "+testEngineReplay+" (seed issue #"+number+")\n"+
		"      the pull request that fixes the seed closes seed issue #"+number+": put `Closes #"+number+"` in its body\n")
	assert.NotContains(t, out, planner)
}

// Validates: R-6.10.23, R-6.10.25
//
// A seed claim renewed on a night that did not run the seed keeps the jobs
// it failed in, which a run containing its fix must pass before the fixed
// claim is removed.
func TestNightlyCheckRenewsASeedClaimWithTheJobsItFailedIn(t *testing.T) {
	t.Parallel()

	engine, _ := nightlyTestSeedKeys()
	remote := newClaimRemote(t, checkConclusionSuccess)
	withOpenSeedIssue(remote, engine, "earlier")
	remote.seed(engine, &nightlyClaim{
		State: nightlyStateClaimed, Owner: nightlyTestOwner, Jobs: []string{nightlySeedsJob},
		At: nightlyTestNow().Add(-time.Hour).UTC().Format(time.RFC3339),
	})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, claimed := remote.claim(engine)
	require.True(t, claimed)
	assert.Equal(t, []string{nightlySeedsJob}, claim.Jobs)
	assert.Equal(t, nightlyTestNow().UTC().Format(time.RFC3339), claim.At, "the claim is renewed")
	assert.Contains(t, out, engine+": claimed by this increment; fix it here (claim renewed)\n"+
		"      replay: none: the issue holds no replay line (seed issue #1)\n")
}

// Validates: R-6.10.25
//
// The report's dry run on a recorded job result reads what it needs and
// prints every write to the seed issues, the label included, and writes
// nothing; a run that is not the newest scheduled run of main says it writes
// no seed issue.
func TestNightlyReportDryRunPrintsTheSeedIssuesItWouldWrite(t *testing.T) {
	t.Parallel()

	engine, planner := nightlyTestSeedKeys()
	remote := seedNightlyRemote(t)
	out := runNightlyReport(t, remote, true)

	assert.Empty(t, remote.writes)
	assert.Empty(t, remote.issues)
	assert.Contains(t, out, "would create the label "+seedIssueLabel+"\n")
	assert.Contains(t, out, "would open an issue \""+seedIssueTitlePrefix+engine+"\" labeled "+seedIssueLabel+" with:\n")
	assert.Contains(t, out, "would open an issue \""+seedIssueTitlePrefix+planner+"\" labeled "+seedIssueLabel+" with:\n")
	assert.Contains(t, out, "    "+seedReplayPrefix+testPlannerReplay+"\n")

	manual := seedNightlyRemote(t)
	manual.jobs = nightlyRunViewOf(t, ciEventWorkflowDispatch, mainBranchName,
		map[string]string{nightlySeedsJob: checkConclusionFailure}, nightlyReportJobEntry())
	out = runNightlyReport(t, manual, true)
	assert.Contains(t, out, "does not write the "+seedIssueLabel+" issues of its 2 failing seeds")
	assert.NotContains(t, out, "would open an issue \""+seedIssueTitlePrefix)
}

// Validates: R-6.10.24, R-6.10.25
//
// The night's verdict does not wait on the seed issues: when the seed label
// or the open seed issues cannot be read, after the client's retries, the
// report still posts the commit status and opens the nightly issue, writes no
// seed issue, and fails with an error that names the seed read.
func TestNightlyReportAppliesTheVerdictWhenTheSeedIssuesCannotBeRead(t *testing.T) {
	t.Parallel()

	failing := func() []error { return slices.Repeat([]error{githubBadGateway()}, githubAttempts) }
	cases := map[string]struct {
		breakRead func(remote *claimRemote)
		read      string
	}{
		"the seed label list": {
			breakRead: func(remote *claimRemote) { remote.labelSearchErrs = map[string][]error{seedIssueLabel: failing()} },
			read:      "seed label (GET",
		},
		"the seed issue list": {
			breakRead: func(remote *claimRemote) {
				remote.labels[seedIssueLabel] = true
				remote.issueListErrs = map[string][]error{seedIssueLabel: failing()}
			},
			read: "seed issue list",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := seedNightlyRemote(t)
			tc.breakRead(remote)
			err := RunNightly(t.Context(), remote, NightlyOptions{
				RepoRoot: "/repo", Action: NightlyActionReport, RunID: nightlyTestRunID,
				Stdout: &bytes.Buffer{}, Sleep: remote.sleeps.sleep,
			})

			require.Len(t, remote.statuses, 1)
			assert.Equal(t, nightlyStatusFailure, remote.statuses[0].state)
			require.Len(t, remote.issues, 1, "the nightly issue, and no seed issue")
			assert.Equal(t, nightlyIssueTitle, remote.issues[0].title)
			assert.Empty(t, seedIssuesOf(remote))
			require.ErrorContains(t, err, tc.read)
		})
	}
}

// Validates: R-6.10.25
//
// One read lists at most seedIssueListLimit open seed issues. A list that
// full may be missing some, and a seed whose issue it missed would get a
// second issue, so the read fails instead of answering.
func TestFindSeedIssuesRefusesAListThatMayMissSome(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	for seed := range seedIssueListLimit {
		withOpenSeedIssue(remote, "internal/sync."+testCrashFamily+"/"+seedsModelSeedEnv+"="+strconv.Itoa(seed+1), "earlier")
	}
	_, err := findSeedIssues(t.Context(), newGHClient(remote, nil), "/repo", nil)
	require.ErrorContains(t, err, "listed "+strconv.Itoa(seedIssueListLimit)+" open "+seedIssueLabel+" issues")

	remote.issues = remote.issues[1:]
	issues, err := findSeedIssues(t.Context(), newGHClient(remote, nil), "/repo", nil)
	require.NoError(t, err)
	assert.Len(t, issues.open, seedIssueListLimit-1)
}

// Validates: R-6.10.23, R-6.10.25
//
// A claimed seed stays failing for as long as its issue is open, so a fix
// that merges without closing it would be claimed again: the gate wants a
// pull request that names a claimed seed with an open issue to close that
// issue with `Closes #N`, and takes no other number for it.
func TestNightlyPreMergeGateRequiresAClaimedSeedsIssueToClose(t *testing.T) {
	t.Parallel()

	engine, _ := nightlyTestSeedKeys()
	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.prBranch = nightlyTestOwner
	issue := withOpenSeedIssue(remote, engine, "earlier")
	number := strconv.Itoa(issue.number)
	remote.seed(engine, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner})

	for _, body := range []string{
		"## Nightly failures\n- " + engine + ": fixed the crash recovery.",
		"## Nightly failures\n- " + engine + ": fixed.\n\nCloses #" + number + "0",
	} {
		remote.prBody = body
		_, err := requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7)
		require.ErrorContains(t, err, "Closes #"+number, body)
	}

	remote.prBody = "## Nightly failures\n- " + engine + ": fixed.\n\nCloses #" + number
	head, err := requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7)
	require.NoError(t, err)
	assert.Equal(t, nightlyTestOwner, head)
}

// Validates: R-6.10.25
//
// A seed that fails in two jobs of one run, a platform each, gets one issue,
// which names both jobs.
func TestNightlyReportOpensOneIssueForASeedThatFailsInTwoJobs(t *testing.T) {
	t.Parallel()

	const linux, mac = "verify-full (ubuntu-latest)", "verify-full (macos-latest)"
	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t,
		map[string]string{linux: checkConclusionFailure, mac: checkConclusionFailure}, nightlyReportJobEntry())
	ids := nightlyTestJobIDs()
	for _, job := range []string{linux, mac} {
		remote.jobLogs[strconv.FormatInt(ids[job], 10)] = seedFailureLog(t, "converged after 3 passes", testEngineReplay)
	}
	runNightlyReport(t, remote, false)

	engine, _ := nightlyTestSeedKeys()
	issues := seedIssuesOf(remote)
	require.Len(t, issues, 1, "one seed is one issue, however many jobs it failed in")
	assert.Equal(t, seedIssueTitlePrefix+engine, issues[0].title)
	assert.Contains(t, issues[0].body, "It failed in the CI jobs `"+mac+"`, `"+linux+"`.\n")
	assert.Equal(t, 1, strings.Count(issues[0].body, seedReplayPrefix+testEngineReplay), "the replay line is given once")
}

// Validates: R-6.10.25
//
// A seed that go test's deadline ended printed no replay line, yet it gets
// its issue like any failing seed: the issue says that its line is built from
// the name of the subtest, holds that line, and says what the line cannot
// know: whether this seed is the one that hung, and whether the run had the
// race detector.
func TestNightlyReportOpensAnIssueForASeedThatPrintedNoReplayLine(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, "")
	remote.jobs = nightlyRunView(t, map[string]string{nightlySeedsJob: checkConclusionFailure}, nightlyReportJobEntry())
	remote.jobLogs[nightlyTestSeedsLog] = seedDeadlineLog("")
	runNightlyReport(t, remote, false)

	issues := seedIssuesOf(remote)
	require.Len(t, issues, 1)
	assert.Equal(t, seedIssueTitlePrefix+"internal/sync."+testCrashFamily+"/"+seedsModelSeedEnv+"="+testTimedOutSeed, issues[0].title)
	assert.Contains(t, issues[0].body, "since go test's deadline ended the run before its cleanup ran, "+
		"so this line is built from the name of its subtest; ")
	assert.Contains(t, issues[0].body, "it may be the seed that hung or one that ran beside it")
	assert.Contains(t, issues[0].body, "so the command has no `"+seedReplayRace+"`: add it when the job ran its tests under the race detector")
	assert.Contains(t, issues[0].body, "It failed in the CI job `"+nightlySeedsJob+"`.\n")
	assert.Contains(t, issues[0].body, "    "+seedReplayPrefix+seedsModelSeedEnv+"="+testTimedOutSeed+
		" go test -run '^"+testCrashFamily+"$' ./internal/sync\n")
}
