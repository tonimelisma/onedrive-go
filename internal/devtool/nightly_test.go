package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nightlyTestRunID     = 4242
	nightlyTestRunHead   = "aaaaaaaaaaaaaaaa"
	nightlyTestRunURL    = "https://example.invalid/run"
	nightlyTestOwner     = "fix/this-increment"
	nightlyTestGit       = "git"
	nightlyTestMergeBase = "merge-base"
	nightlyTestOther     = "fix/other-agent"
	nightlyTestE2EJob    = "e2e"
	nightlyTestE2ELog    = "1"
	nightlyTestTest      = "TestE2E_Edge_RapidFileChurn"
	nightlyTestKey       = "e2e." + nightlyTestTest
	nightlyTestStale     = "TestOldStyle"
)

// nightlyTestJobIDs are the job ids of a scheduled run in the fixtures: one
// per job the night requires, a matrix leg each.
func nightlyTestJobIDs() map[string]int64 {
	return map[string]int64{
		"verify-full (ubuntu-latest)": 10,
		"verify-full (macos-latest)":  11,
		"verify":                      12,
		"stress":                      13,
		"integration":                 14,
		nightlySeedsJob:               15,
		nightlyTestE2EJob:             1,
	}
}

// nightlyRunView is `gh run view --json` of a scheduled run of main in which
// every job the night requires passed, except where conclusions names the
// job's conclusion ("" for a job still running). Every job but a skipped one
// started on a runner and lists a step. An extra job replaces the job of its
// name, or is listed after them.
func nightlyRunView(t *testing.T, conclusions map[string]string, extra ...map[string]any) string {
	t.Helper()

	return nightlyRunViewOf(t, ciEventSchedule, mainBranchName, conclusions, extra...)
}

// nightlyRunViewOf is nightlyRunView of a run of event on branch.
func nightlyRunViewOf(t *testing.T, event string, branch string, conclusions map[string]string, extra ...map[string]any) string {
	t.Helper()

	ids := nightlyTestJobIDs()
	names := slices.Sorted(maps.Keys(ids))
	replaced := map[string]map[string]any{}
	added := []map[string]any{}
	for _, job := range extra {
		name, named := job["name"].(string)
		if _, required := ids[name]; named && required {
			replaced[name] = job
			continue
		}
		added = append(added, job)
	}
	jobs := make([]map[string]any, 0, len(names)+len(added))
	for _, name := range names {
		if job, ok := replaced[name]; ok {
			jobs = append(jobs, job)
			continue
		}
		conclusion, overridden := conclusions[name]
		if !overridden {
			conclusion = checkConclusionSuccess
		}
		jobs = append(jobs, nightlyStartedJob(ids[name], name, conclusion))
	}
	jobs = append(jobs, added...)
	view, err := json.Marshal(map[string]any{
		"databaseId": nightlyTestRunID, "headSha": nightlyTestRunHead, "url": nightlyTestRunURL,
		"event": event, "headBranch": branch, "jobs": jobs,
	})
	require.NoError(t, err)

	return string(view)
}

// nightlyStartedJob is a job a runner picked up, as `gh run view --json jobs`
// lists it: with a start time and its first step, unless it was skipped.
func nightlyStartedJob(id int64, name string, conclusion string) map[string]any {
	status := checkStatusCompleted
	if conclusion == "" {
		status = checkStatusInProgress
	}
	steps := []map[string]string{{"name": "Set up job"}}
	if conclusion == checkConclusionSkipped {
		steps = []map[string]string{}
	}

	return map[string]any{
		"databaseId": id, "name": name, "status": status, "conclusion": conclusion,
		"startedAt": nightlyTestNow().Add(-9 * time.Hour).Format(time.RFC3339), "steps": steps,
	}
}

// nightlyQueuedJob is a required job canceled while it waited for a runner or
// for its concurrency group, as GitHub lists it: a start time, the time it
// was queued, and no step.
func nightlyQueuedJob(name string) map[string]any {
	return map[string]any{
		"databaseId": nightlyTestJobIDs()[name], "name": name, "status": checkStatusCompleted, "conclusion": checkConclusionCancelled,
		"startedAt": nightlyTestNow().Add(-9 * time.Hour).Format(time.RFC3339), "steps": []map[string]string{},
	}
}

// nightlyRunList is `gh run list` of scheduled runs in the order given: each
// run is an id, a status, a conclusion, how long before nightlyTestNow it was
// created and, when its latest attempt started later, how long before
// nightlyTestNow that attempt started.
type nightlyListedRun struct {
	id         int64
	status     string
	conclusion string
	age        time.Duration
	attemptAge time.Duration
}

func nightlyRunList(t *testing.T, runs ...nightlyListedRun) string {
	t.Helper()

	return nightlyRunListAt(t, nightlyTestNow(), runs...)
}

// nightlyRunListAt is nightlyRunList with the runs' ages counted back from
// now.
func nightlyRunListAt(t *testing.T, now time.Time, runs ...nightlyListedRun) string {
	t.Helper()

	listed := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		started := run.age
		if run.attemptAge > 0 {
			started = run.attemptAge
		}
		listed = append(listed, map[string]any{
			"databaseId": run.id,
			"status":     run.status,
			"conclusion": run.conclusion,
			"headSha":    nightlyTestRunHead,
			"createdAt":  now.Add(-run.age).UTC().Format(time.RFC3339),
			"startedAt":  now.Add(-started).UTC().Format(time.RFC3339),
			"url":        nightlyTestRunURL,
		})
	}
	out, err := json.Marshal(listed)
	require.NoError(t, err)

	return string(out)
}

// nightlyE2EFailureLog is the e2e job's log when one test failed: the raw
// go test lines behind the job log's timestamps, closed by the package's
// result line.
func nightlyE2EFailureLog() string {
	return "2026-11-09T10:20:00Z === RUN   " + nightlyTestTest + "\n" +
		"2026-11-09T10:21:00Z --- FAIL: " + nightlyTestTest + " (49.50s)\n" +
		"2026-11-09T10:21:00Z     --- FAIL: " + nightlyTestTest + "/sub (1.00s)\n" +
		"2026-11-09T10:21:01Z FAIL\t" + repoModulePath + "/e2e\t49.600s\n"
}

type nightlyExitError struct{ code int }

func (e nightlyExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e nightlyExitError) ExitCode() int { return e.code }

// claimRemote is a commandRunner that behaves like the shared remote for
// claim refs and like GitHub for the nightly's runs, issues, labels and
// commit statuses: pushes are compare-and-swap, and a hook can move a ref
// between an agent's read and its push to model a concurrent agent.
type claimRemote struct {
	t          *testing.T
	refs       map[string]string
	commits    map[string]string
	nextCommit int
	runs       string
	jobs       string
	jobLogs    map[string]string
	prBody     string
	prBranch   string
	ancestors  map[string]bool
	beforePush func(remote *claimRemote)
	pushes     []string
	// jobLogErrs are the failures the job-log fetch of a job id answers
	// with, in order, before it returns the log; jobLogCalls counts the
	// fetches. runListErrs does the same for the run list, and labelListErrs
	// for the label list, the report's first read of the issue;
	// labelSearchErrs and issueListErrs do it for the label list and the open
	// issue list of one label.
	jobLogErrs      map[string][]error
	jobLogCalls     map[string]int
	runListErrs     []error
	labelListErrs   []error
	labelSearchErrs map[string][]error
	issueListErrs   map[string][]error
	// runListArgs are the arguments of every run list the fake answered.
	runListArgs [][]string
	// sleeps records the backoffs the check waited out.
	sleeps recordedSleeps

	// GitHub's side of the nightly report.
	labels   map[string]bool
	issues   []*fakeIssue
	statuses []fakeStatus
	// writes lists every gh command that changed something on GitHub.
	writes []string
}

// fakeIssue is one issue of the fake repository.
type fakeIssue struct {
	number   int
	title    string
	labels   []string
	body     string
	open     bool
	comments []string
}

// fakeStatus is one commit status posted to the fake repository.
type fakeStatus struct {
	sha         string
	state       string
	context     string
	targetURL   string
	description string
}

func newClaimRemote(t *testing.T, conclusion string) *claimRemote {
	t.Helper()

	return &claimRemote{
		t:       t,
		refs:    map[string]string{},
		commits: map[string]string{},
		runs: nightlyRunList(t, nightlyListedRun{
			id: nightlyTestRunID, status: checkStatusCompleted, conclusion: conclusion, age: 10 * time.Hour,
		}),
		jobs:        nightlyRunView(t, nil),
		jobLogs:     map[string]string{},
		ancestors:   map[string]bool{},
		jobLogErrs:  map[string][]error{},
		jobLogCalls: map[string]int{},
		labels:      map[string]bool{},
	}
}

// seed stores a claim record of the current format at key's ref.
func (r *claimRemote) seed(key string, claim *nightlyClaim) {
	r.t.Helper()

	claim.Format = nightlyClaimFormat
	r.seedRecord(nightlyClaimRefName(key), key, claim)
}

// seedOldFormat stores a claim record that carries no format at a ref named
// by the key itself, as devtool wrote them before keys named their package.
func (r *claimRemote) seedOldFormat(key string, claim *nightlyClaim) {
	r.t.Helper()

	claim.Format = ""
	r.seedRecord(key, key, claim)
}

func (r *claimRemote) seedRecord(ref string, key string, claim *nightlyClaim) {
	r.t.Helper()

	claim.Key = key
	record, err := json.Marshal(claim)
	require.NoError(r.t, err)
	r.refs[ref] = r.commit(string(record))
}

// claimKeys are the keys the remote's claim records name, sorted.
func (r *claimRemote) claimKeys() []string {
	r.t.Helper()

	keys := make([]string, 0, len(r.refs))
	for _, sha := range r.refs {
		var claim nightlyClaim
		require.NoError(r.t, json.Unmarshal([]byte(r.commits[sha]), &claim))
		keys = append(keys, claim.Key)
	}
	sort.Strings(keys)

	return keys
}

func (r *claimRemote) commit(message string) string {
	r.nextCommit++
	sha := fmt.Sprintf("c%03d", r.nextCommit)
	r.commits[sha] = message
	return sha
}

func (r *claimRemote) claim(key string) (nightlyClaim, bool) {
	r.t.Helper()

	sha, ok := r.refs[nightlyClaimRefName(key)]
	if !ok {
		return nightlyClaim{}, false
	}
	var claim nightlyClaim
	require.NoError(r.t, json.Unmarshal([]byte(r.commits[sha]), &claim))
	return claim, true
}

func (r *claimRemote) Run(_ context.Context, _ string, _ []string, _, _ io.Writer, name string, args ...string) error {
	if name == nightlyTestGit && len(args) == 4 && args[0] == nightlyTestMergeBase {
		if r.ancestors[args[2]+".."+args[3]] {
			return nil
		}
		return fmt.Errorf("git merge-base: %w", nightlyExitError{code: 1})
	}
	return nil
}

func (r *claimRemote) Output(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	joined := name + " " + strings.Join(args, " ")
	if name == githubCLI {
		return r.github(joined, args)
	}
	switch {
	case strings.HasPrefix(joined, "git for-each-ref"):
		var b strings.Builder
		for _, ref := range slices.Sorted(maps.Keys(r.refs)) {
			sha := r.refs[ref]
			fmt.Fprintf(&b, "%s\t%s%s\t%s\n", sha, nightlyClaimRefPrefix, ref, r.commits[sha])
		}
		return []byte(b.String()), nil
	case strings.HasPrefix(joined, "git commit-tree"):
		return []byte(r.commit(args[len(args)-1]) + "\n"), nil
	case strings.HasPrefix(joined, "git rev-parse --abbrev-ref HEAD"):
		return []byte(nightlyTestOwner + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected command: %s", joined)
}

// github answers the gh reads of the nightly check and report.
func (r *claimRemote) github(joined string, args []string) ([]byte, error) {
	switch {
	case strings.HasPrefix(joined, "gh run list"):
		r.runListArgs = append(r.runListArgs, slices.Clone(args))
		if len(r.runListErrs) > 0 {
			failure := r.runListErrs[0]
			r.runListErrs = r.runListErrs[1:]
			return nil, failure
		}
		return []byte(r.runs), nil
	case strings.HasPrefix(joined, "gh run view"):
		return []byte(r.jobs), nil
	case strings.HasPrefix(joined, "gh api repos/{owner}/{repo}/actions/jobs/"):
		id := strings.TrimSuffix(strings.TrimPrefix(args[1], "repos/{owner}/{repo}/actions/jobs/"), "/logs")
		r.jobLogCalls[id]++
		if failures := r.jobLogErrs[id]; len(failures) > 0 {
			r.jobLogErrs[id] = failures[1:]
			return nil, failures[0]
		}
		return []byte(r.jobLogs[id]), nil
	case strings.HasPrefix(joined, "gh pr view"):
		body, err := json.Marshal(map[string]string{"body": r.prBody, "headRefName": r.prBranch})
		if err != nil {
			return nil, fmt.Errorf("encode pr view: %w", err)
		}
		return body, nil
	case strings.HasPrefix(joined, "gh label list"):
		if len(r.labelListErrs) > 0 {
			failure := r.labelListErrs[0]
			r.labelListErrs = r.labelListErrs[1:]
			return nil, failure
		}
		if failure := popRemoteErr(r.labelSearchErrs, ghFields(args)["--search"]); failure != nil {
			return nil, failure
		}
		return r.labelList()
	case strings.HasPrefix(joined, "gh issue list"):
		if failure := popRemoteErr(r.issueListErrs, ghFields(args)["--label"]); failure != nil {
			return nil, failure
		}
		return r.openIssueList(ghFields(args)["--label"])
	case strings.HasPrefix(joined, "gh issue view "):
		return r.issueView(args[2])
	}
	r.writes = append(r.writes, joined)
	return r.githubWrite(joined, args)
}

// popRemoteErr takes the next failure errs holds for key, nil for none.
func popRemoteErr(errs map[string][]error, key string) error {
	failures := errs[key]
	if len(failures) == 0 {
		return nil
	}
	errs[key] = failures[1:]

	return failures[0]
}

// githubWrite carries out the gh commands of the nightly report that change
// something on GitHub.
func (r *claimRemote) githubWrite(joined string, args []string) ([]byte, error) {
	switch {
	case strings.HasPrefix(joined, "gh api -X POST repos/{owner}/{repo}/statuses/"):
		fields := ghFields(args)
		r.statuses = append(r.statuses, fakeStatus{
			sha:         strings.TrimPrefix(args[3], "repos/{owner}/{repo}/statuses/"),
			state:       fields["state"],
			context:     fields["context"],
			targetURL:   fields["target_url"],
			description: fields["description"],
		})
		return []byte("{}"), nil
	case strings.HasPrefix(joined, "gh label create "):
		r.labels[args[2]] = true
		return nil, nil
	case strings.HasPrefix(joined, "gh issue create"):
		fields := ghFields(args)
		issue := &fakeIssue{
			number: len(r.issues) + 1, title: fields["--title"], labels: []string{fields["--label"]},
			body: fields["--body"], open: true,
		}
		r.issues = append(r.issues, issue)
		return fmt.Appendf(nil, "https://example.invalid/issues/%d\n", issue.number), nil
	case strings.HasPrefix(joined, "gh issue comment "):
		issue := r.issue(args[2])
		issue.comments = append(issue.comments, ghFields(args)["--body"])
		return nil, nil
	case strings.HasPrefix(joined, "gh issue close "):
		r.issue(args[2]).open = false
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command: %s", joined)
}

func (r *claimRemote) issue(number string) *fakeIssue {
	r.t.Helper()

	for _, issue := range r.issues {
		if strconv.Itoa(issue.number) == number {
			return issue
		}
	}
	require.Failf(r.t, "no such issue", "issue %s", number)
	return nil
}

// labelList answers `gh label list --search`, which prints nothing at all,
// not an empty list, when no label matches.
func (r *claimRemote) labelList() ([]byte, error) {
	if len(r.labels) == 0 {
		return nil, nil
	}
	labels := []map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(r.labels)) {
		labels = append(labels, map[string]string{"name": name})
	}
	out, err := json.Marshal(labels)
	if err != nil {
		return nil, fmt.Errorf("encode labels: %w", err)
	}
	return out, nil
}

// issueView answers `gh issue view <n> --json body,comments`.
func (r *claimRemote) issueView(number string) ([]byte, error) {
	issue := r.issue(number)
	comments := make([]map[string]string, 0, len(issue.comments))
	for _, comment := range issue.comments {
		comments = append(comments, map[string]string{"body": comment})
	}
	out, err := json.Marshal(map[string]any{"body": issue.body, "comments": comments})
	if err != nil {
		return nil, fmt.Errorf("encode issue view: %w", err)
	}
	return out, nil
}

// openIssueList answers `gh issue list --label <label> --state open`, the
// newest issue first, with each issue's number, title and body.
func (r *claimRemote) openIssueList(label string) ([]byte, error) {
	open := []map[string]any{}
	for i := len(r.issues) - 1; i >= 0; i-- {
		if r.issues[i].open && slices.Contains(r.issues[i].labels, label) {
			open = append(open, map[string]any{"number": r.issues[i].number, "title": r.issues[i].title, "body": r.issues[i].body})
		}
	}
	out, err := json.Marshal(open)
	if err != nil {
		return nil, fmt.Errorf("encode issues: %w", err)
	}
	return out, nil
}

// ghFields reads a gh command's flag values: `-f key=value` pairs by key,
// and `--flag value` pairs by flag.
func ghFields(args []string) map[string]string {
	fields := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		switch {
		case args[i] == "-f":
			key, value, _ := strings.Cut(args[i+1], "=")
			fields[key] = value
			i++
		case strings.HasPrefix(args[i], "--"):
			fields[args[i]] = args[i+1]
			i++
		}
	}
	return fields
}

func (r *claimRemote) CombinedOutput(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	if name != nightlyTestGit || args[0] != "push" {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	if r.beforePush != nil {
		hook := r.beforePush
		r.beforePush = nil
		hook(r)
	}
	lease := strings.TrimPrefix(args[2], "--force-with-lease="+nightlyClaimRefPrefix)
	key, expect, _ := strings.Cut(lease, ":")
	source, _, _ := strings.Cut(args[4], ":")
	if r.refs[key] != expect {
		return []byte("!\t" + args[4] + "\t[rejected] (stale info)\n"), errors.New("exit status 1")
	}
	r.pushes = append(r.pushes, args[4])
	if source == "" {
		delete(r.refs, key)
	} else {
		r.refs[key] = source
	}
	return nil, nil
}

var _ commandRunner = (*claimRemote)(nil)

func nightlyTestNow() time.Time {
	return time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
}

func failingNightlyRemote(t *testing.T) *claimRemote {
	t.Helper()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{nightlyTestE2EJob: checkConclusionFailure})
	remote.jobLogs[nightlyTestE2ELog] = nightlyE2EFailureLog()
	return remote
}

func runNightlyCheck(t *testing.T, remote *claimRemote) (string, error) {
	t.Helper()

	return runNightlyCheckIn(t, remote, "/repo")
}

// runNightlyCheckIn runs the check from repoRoot, which holds the workflow
// when a test needs a scheduled run's bound.
func runNightlyCheckIn(t *testing.T, remote *claimRemote, repoRoot string) (string, error) {
	t.Helper()

	var stdout bytes.Buffer
	err := RunNightly(t.Context(), remote, NightlyOptions{
		RepoRoot: repoRoot,
		Action:   NightlyActionCheck,
		Branch:   nightlyTestOwner,
		Stdout:   &stdout,
		Now:      nightlyTestNow,
		Sleep:    remote.sleeps.sleep,
	})
	return stdout.String(), err
}

// nightlyTestFailures reads the failures of the remote's run view the way the
// check does, logs included.
func nightlyTestFailures(t *testing.T, remote *claimRemote) map[string][]string {
	t.Helper()

	var run githubRun
	require.NoError(t, json.Unmarshal([]byte(remote.jobs), &run))
	reports := nightlyJobReports(run.Jobs, "")
	require.NoError(t, attributeNightlyFailures(t.Context(), newGHClient(remote, nil), "/repo", reports))

	return nightlyFailureKeys(reports)
}

// Validates: R-6.10.23
//
// A failed test is keyed by its package and its top-level name; a job whose
// log names no test is keyed by the job. The aggregate verify job only
// repeats the legs it waits for, so it yields no key while another job
// failed.
func TestNightlyFailureKeysNameTestsOrJobs(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobs = nightlyRunView(t, map[string]string{
		nightlyTestE2EJob:             checkConclusionFailure,
		"verify-full (ubuntu-latest)": checkConclusionFailure,
		"verify-full (macos-latest)":  checkConclusionFailure,
		"verify":                      checkConclusionFailure,
	})
	remote.jobLogs["10"] = `{"Action":"fail","Package":"` + repoModulePath + `/internal/sync","Test":"TestPlanner/case"}` + "\n" +
		`{"Action":"fail","Package":"` + repoModulePath + `/internal/sync"}` + "\n"
	remote.jobLogs["11"] = "the build failed before any test ran\n"

	assert.Equal(t, map[string][]string{
		nightlyTestKey:                  {nightlyTestE2EJob},
		"internal/sync.TestPlanner":     {"verify-full"},
		"job-verify-full--macos-latest": {"verify-full"},
	}, nightlyTestFailures(t, remote))
	assert.Zero(t, remote.jobLogCalls["12"], "the aggregate's log is not read while a leg failed")
}

// Validates: R-6.10.23
//
// Two packages can hold tests of one name, and one key for both let the fix
// of one hide the failure of the other.
func TestNightlyFailureKeysKeepSameNamedTestsOfTwoPackagesApart(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{
		"verify-full (ubuntu-latest)": checkConclusionFailure,
		"verify-full (macos-latest)":  checkConclusionFailure,
		"verify":                      checkConclusionFailure,
	})
	remote.jobLogs["10"] = "--- FAIL: TestRetry (0.10s)\nFAIL\t" + repoModulePath + "/internal/sync\t3.000s\n" +
		"--- FAIL: TestRetry (0.20s)\nFAIL\t" + repoModulePath + "/internal/graph\t1.000s\n"
	remote.jobLogs["11"] = "--- FAIL: TestRetry (0.10s)\nFAIL\t" + repoModulePath + "/internal/sync\t3.000s\n"

	assert.Equal(t, map[string][]string{
		"internal/graph.TestRetry": {"verify-full"},
		"internal/sync.TestRetry":  {"verify-full"},
	}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23
//
// A classified live-quirk rerun is a second go test of the failed test's
// package, later in the same job log: when it passes the test, the
// repository's own rule passed it, so the test is not keyed. A test that
// failed one iteration of -count and passed the next, inside one go test,
// failed, and is keyed.
func TestNightlyFailureKeysDropATestItsRerunPassed(t *testing.T) {
	t.Parallel()

	const quirk = "TestE2E_WebsocketRestart"
	e2e := repoModulePath + "/e2e"
	syncPackage := repoModulePath + "/internal/sync"
	remote := failingNightlyRemote(t)
	remote.jobs = nightlyRunView(t, map[string]string{
		nightlyTestE2EJob: checkConclusionFailure,
		"stress":          checkConclusionFailure,
	})
	remote.jobLogs[nightlyTestE2ELog] = `{"Action":"fail","Package":"` + e2e + `","Test":"` + quirk + `"}` + "\n" +
		`{"Action":"fail","Package":"` + e2e + `"}` + "\n" +
		"==> rerun known live quirk LI-20260101-01\n" +
		"=== RUN   " + quirk + "\n--- PASS: " + quirk + " (3.00s)\nPASS\nok  \t" + e2e + "\t3.100s\n" +
		`{"Action":"fail","Package":"` + e2e + `","Test":"` + nightlyTestTest + `"}` + "\n" +
		`{"Action":"fail","Package":"` + e2e + `"}` + "\n"
	remote.jobLogs["13"] = `{"Action":"fail","Package":"` + syncPackage + `","Test":"TestFlaky"}` + "\n" +
		`{"Action":"pass","Package":"` + syncPackage + `","Test":"TestFlaky"}` + "\n" +
		`{"Action":"fail","Package":"` + syncPackage + `"}` + "\n"

	assert.Equal(t, map[string][]string{
		nightlyTestKey:            {nightlyTestE2EJob},
		"internal/sync.TestFlaky": {"stress"},
	}, nightlyTestFailures(t, remote))
}

// goTestEventLine is one `go test -json` event of a fixture log; an empty
// test makes it the package's own event.
func goTestEventLine(action string, pkg string, test string) string {
	return fmt.Sprintf(`{"Action":%q,"Package":%q,"Test":%q}`, action, pkg, test) + "\n"
}

// Validates: R-6.10.23
//
// Only the repository's own classified rerun answers a failure: the go test
// that follows the line rerunClassifiedQuirk prints, before devtool's next
// status line, and only the failure of the go test of the package it
// directly follows. Any other later go test of the package that passes the
// test, raw or -json, such as the stress job's -json run of a tree after its
// -count=50 run of some of its tests, hides nothing. Failures are kept per
// go test of a package, so a failure in the rerun, after it or in an earlier
// go test than the one it reruns keys the test, and a later go test of the
// package that fails without naming a test keys the job as well. A -json
// result reaches the log twice, as go test's output line and as the
// package's action, and is one go test.
func TestNightlyFailureKeysOnlyAClassifiedRerunAnswersAFailure(t *testing.T) {
	t.Parallel()

	const (
		spins   = "TestWidgetSpins"
		syncs   = "TestWidgetSyncs"
		live    = "TestE2E_WidgetRoundTrip"
		shares  = "TestE2E_WidgetShares"
		marker  = "==> rerun known live quirk LI-20990101-01\n"
		rawRun  = "==> go test -race -count=50 -run TestWidget ./internal/widget/...\n"
		jsonRun = "==> go test -json -race -count=3 ./internal/widget/...\n"
		bucket  = "==> go test -json -tags='e2e e2e_full' widget-bucket\n"
		// classified is the line devtool prints after a classified rerun passed.
		classified = "warning: classified known live quirk LI-20990101-01 after successful rerun\n"
	)
	widget := repoModulePath + "/internal/widget"
	e2e := repoModulePath + "/e2e"
	rawFailure := rawRun + "--- FAIL: " + spins + " (0.10s)\nFAIL\t" + widget + "\t5.000s\n"
	jsonPasses := jsonRun + strings.Repeat(goTestEventLine(goTestActionPass, widget, spins), 3)
	liveFailure := goTestEventLine(goTestActionFail, e2e, live) + goTestEventLine(goTestActionFail, e2e, "")
	rerunPass := marker + "=== RUN   " + live + "\n--- PASS: " + live + " (1.00s)\nPASS\nok  \t" + e2e + "\t1.100s\n"
	stress, e2eJob := nightlyTestJobIDs()["stress"], nightlyTestJobIDs()[nightlyTestE2EJob]
	cases := map[string]struct {
		job  string
		log  string
		want map[string][]string
	}{
		"a pass in a later go test that is no rerun, beside another failure": {
			job: "stress",
			log: rawFailure + jsonPasses + goTestEventLine(goTestActionFail, widget, syncs) +
				goTestEventLine(goTestActionFail, widget, ""),
			want: map[string][]string{"internal/widget." + spins: {"stress"}, "internal/widget." + syncs: {"stress"}},
		},
		"a pass in a later go test that is no rerun, alone": {
			job:  "stress",
			log:  rawFailure + jsonPasses + goTestEventLine(goTestActionPass, widget, ""),
			want: map[string][]string{"internal/widget." + spins: {"stress"}},
		},
		"a raw pass in a later go test that is no rerun": {
			job:  "stress",
			log:  rawFailure + rawRun + "--- PASS: " + spins + " (0.10s)\nok  \t" + widget + "\t5.000s\n",
			want: map[string][]string{"internal/widget." + spins: {"stress"}},
		},
		"a passing classified rerun of a later go test than the failure": {
			job: nightlyTestE2EJob,
			log: goTestEventLine(goTestActionFail, e2e, live) + goTestEventLine(goTestActionFail, e2e, shares) +
				goTestEventLine(goTestActionFail, e2e, "") + bucket + liveFailure + rerunPass,
			want: map[string][]string{"e2e." + live: {nightlyTestE2EJob}, "e2e." + shares: {nightlyTestE2EJob}},
		},
		"a failure after a passing classified rerun": {
			job:  nightlyTestE2EJob,
			log:  liveFailure + rerunPass + classified + bucket + liveFailure,
			want: map[string][]string{"e2e." + live: {nightlyTestE2EJob}},
		},
		"two failures of one test, each answered by its own passing classified rerun": {
			job:  nightlyTestE2EJob,
			log:  liveFailure + rerunPass + classified + bucket + liveFailure + rerunPass + classified,
			want: map[string][]string{"job-" + nightlyTestE2EJob: {nightlyTestE2EJob}},
		},
		"a classified rerun that fails again": {
			job:  nightlyTestE2EJob,
			log:  liveFailure + marker + "=== RUN   " + live + "\n--- FAIL: " + live + " (1.00s)\nFAIL\nFAIL\t" + e2e + "\t1.100s\n",
			want: map[string][]string{"e2e." + live: {nightlyTestE2EJob}},
		},
		"a package failure without a test in a later go test": {
			job: "stress",
			log: rawFailure + jsonRun + `{"Action":"output","Package":"` + widget + `","Output":"FAIL\t` + widget + `\t0.500s\n"}` + "\n" +
				goTestEventLine(goTestActionFail, widget, ""),
			want: map[string][]string{"internal/widget." + spins: {"stress"}, "job-stress": {"stress"}},
		},
		"a -json failure whose result is printed as output too": {
			job: "stress",
			log: jsonRun + goTestEventLine(goTestActionFail, widget, spins) +
				`{"Action":"output","Package":"` + widget + `","Output":"FAIL\t` + widget + `\t0.500s\n"}` + "\n" +
				goTestEventLine(goTestActionFail, widget, ""),
			want: map[string][]string{"internal/widget." + spins: {"stress"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := newClaimRemote(t, checkConclusionFailure)
			remote.jobs = nightlyRunView(t, map[string]string{tc.job: checkConclusionFailure})
			logID := strconv.FormatInt(stress, 10)
			if tc.job == nightlyTestE2EJob {
				logID = strconv.FormatInt(e2eJob, 10)
			}
			remote.jobLogs[logID] = tc.log

			assert.Equal(t, tc.want, nightlyTestFailures(t, remote))
		})
	}
}

// Validates: R-6.10.23
//
// The check counts every job of the run, the report job too: a night whose
// required jobs all passed but whose report job failed is red, keyed by the
// report job. Leaving the job out would turn GitHub's red conclusion into a
// red run with no failed job, which fails every start stage.
func TestNightlyCheckKeysAFailedReportJob(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, nil, nightlyStartedJob(nightlyTestReportJobID, nightlyReportJob, checkConclusionFailure))

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Contains(t, out, "  "+nightlyReportJob+": failed\n")
	assert.Equal(t, []string{nightlyJobKeyPrefix + nightlyReportJob}, remote.claimKeys())
}

// Validates: R-6.10.23
//
// A package that failed without naming a test (a build or setup failure, a
// TestMain exit, a package timeout with no test running) is a failure of its
// own: the job is keyed as well as the tests its log names, so that failure
// can be claimed too.
func TestNightlyFailureKeysKeyTheJobOfAPackageFailureWithoutATest(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure})
	remote.jobLogs["13"] = "--- FAIL: TestWatchSoak (1.00s)\nFAIL\t" + repoModulePath + "/internal/multisync\t2.000s\n" +
		"FAIL\t" + repoModulePath + "/internal/sync [build failed]\n"

	assert.Equal(t, map[string][]string{
		"internal/multisync.TestWatchSoak": {"stress"},
		"job-stress":                       {"stress"},
	}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23
//
// The aggregate verify job repeats only the verify jobs it waits for: beside
// a failure of one of them it yields no key, but when it failed beside jobs
// it does not wait for, or alone, it is named.
func TestNightlyFailureKeysNameTheAggregateThatFailedOnItsOwn(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobs = nightlyRunView(t, map[string]string{
		nightlyTestE2EJob: checkConclusionFailure,
		"verify":          checkConclusionFailure,
	})
	remote.jobLogs["12"] = "the runner lost contact\n"

	assert.Equal(t, map[string][]string{
		nightlyTestKey: {nightlyTestE2EJob},
		"job-verify":   {"verify"},
	}, nightlyTestFailures(t, remote))
	assert.Equal(t, 1, remote.jobLogCalls["12"], "the aggregate's log is read when no job it waits for failed")
}

// Validates: R-6.10.23
//
// A claim's ref is named by an encoding of its key: no two keys share a
// name, whatever they hold, two names never differ by case alone, and every
// name is one valid git ref component that a command line cannot read as an
// option. The empty key, and a key whose ref would pass
// nightlyClaimRefMaxLen, keep the head of their encoding and end in a digest
// of the whole key, which no other name holds.
func TestNightlyClaimRefNameIsInjectiveAndAValidRefComponent(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("internal/sync.", 20)
	keys := []string{
		"a/b-c.TestX", "a-b/c.TestX", "a-b-c.TestX", "a.b.TestX",
		"x.TestA", "x.Testa", "X.TestA",
		"a..b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a b", "a\tb", "a\x00b", "a\x7fb", "a@{b", "@",
		"-lead", ".lead", "trail.", "name.lock", "a_b", "a__b", "a_095b", "a_-b", "\xffbyte", "",
		nightlyUnknownPackage + ".TestX", nightlyRootPackage + ".TestX",
		long + "TestA", long + "TestB", long + "TestA_",
	}
	byName := map[string]string{}
	for _, key := range keys {
		name := nightlyClaimRefName(key)
		assert.Empty(t, refComponentViolation(name), "key %q gives ref name %q", key, name)
		assert.LessOrEqual(t, len(nightlyClaimRefPrefix+name), nightlyClaimRefMaxLen, "key %q", key)
		folded := strings.ToLower(name)
		if other, taken := byName[folded]; taken {
			assert.Failf(t, "two keys share a ref name", "%q and %q both give %q", other, key, name)
		}
		byName[folded] = key
		if key != "" && len(key) < len(long) {
			assert.NotContains(t, name, nightlyClaimRefDigestMarker, "key %q", key)
		}
	}
	assert.Equal(t, "job-verify-full--macos-latest", nightlyClaimRefName("job-verify-full--macos-latest"),
		"a job key is its own ref name")
}

// refComponentViolation names the rule of git check-ref-format that name
// breaks as one component of a ref, or "" when it breaks none; a leading '-'
// is refused too, because a command line would read it as an option.
func refComponentViolation(name string) string {
	switch {
	case name == "" || name == "@":
		return "empty or @"
	case strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-"):
		return "leading . or -"
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock"):
		return "trailing . or .lock"
	case strings.Contains(name, "..") || strings.Contains(name, "@{"):
		return ".. or @{"
	case strings.ContainsAny(name, " ~^:?*[\\/"):
		return "a forbidden character"
	case strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == '\x7f' }):
		return "a control byte"
	}

	return ""
}

// Validates: R-6.10.23
//
// The keys for "the log never named a package" and "the module's root
// package" are not the key of any real package path, and a package path's
// separators stay in its key, so two packages never share one.
func TestNightlyFailedTestKeyKeepsPackagesApart(t *testing.T) {
	t.Parallel()

	key := func(pkg string) string { return nightlyFailedTestKey(goTestFailure{pkg: pkg, test: "TestX"}) }
	assert.Equal(t, "internal/sync.TestX", key("./internal/sync"))
	assert.NotEqual(t, key("./a/b-c"), key("./a-b/c"))
	assert.NotEqual(t, key(""), key("./unknown-package"))
	assert.NotEqual(t, key("."), key("./module-root"))
	assert.NotEqual(t, key("."), key(""))
	for _, sentinel := range []string{nightlyUnknownPackage, nightlyRootPackage} {
		assert.True(t, strings.ContainsAny(sentinel, "()"), "%s holds a character no Go import path holds", sentinel)
	}
}

// Validates: R-6.10.23
//
// A required job canceled while it waited for a runner or for its
// concurrency group never started: it did not run, which keeps the night
// red, but no increment can fix it, so the check prints it, claims nothing
// for it, reads no log of it, and does not fail on it. A job canceled after
// it started keeps its key.
func TestNightlyCheckClaimsNothingForAJobThatDidNotRun(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionCancelled)
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionCancelled},
		nightlyQueuedJob(nightlyTestE2EJob))

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Contains(t, out, "  e2e: did not run; nothing to claim\n")
	assert.Contains(t, out, "  stress: canceled\n")
	assert.Equal(t, []string{"job-stress"}, remote.claimKeys())
	assert.Zero(t, remote.jobLogCalls[nightlyTestE2ELog], "a job that did not run has no log")

	queuedOnly := newClaimRemote(t, checkConclusionCancelled)
	queuedOnly.jobs = nightlyRunView(t, nil, nightlyQueuedJob(nightlyTestE2EJob))
	out, err = runNightlyCheck(t, queuedOnly)
	require.NoError(t, err, "a night red only because a job did not run fails no start stage")
	assert.Contains(t, out, "e2e: did not run")
	assert.Empty(t, queuedOnly.refs)
}

// Validates: R-6.10.23, R-6.10.24
//
// One rule gives every job of a scheduled run its result, for the check and
// for the report: a timed-out or canceled job fails the night under its own
// name, and a job the night requires that was skipped did not run.
func TestNightlyJobOutcomeNamesEveryResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		conclusion string
		required   bool
		// queued is a job no runner picked up: no step.
		queued     bool
		want       nightlyJobResult
		failsNight bool
	}{
		{conclusion: checkConclusionSuccess, required: true, want: nightlyJobPassed},
		{conclusion: checkConclusionFailure, required: true, want: nightlyJobFailed, failsNight: true},
		{conclusion: "timed_out", required: true, want: nightlyJobTimedOut, failsNight: true},
		{conclusion: checkConclusionCancelled, required: true, want: nightlyJobCanceled, failsNight: true},
		{conclusion: checkConclusionCancelled, required: true, queued: true, want: nightlyJobNotRun, failsNight: true},
		{conclusion: checkConclusionCancelled, required: false, queued: true, want: nightlyJobNotRun, failsNight: true},
		{conclusion: checkConclusionSkipped, required: true, want: nightlyJobNotRun, failsNight: true},
		{conclusion: checkConclusionSkipped, required: false, want: nightlyJobSkipped},
		{conclusion: "", required: true, want: nightlyJobUnfinished, failsNight: true},
		{conclusion: "startup_failure", required: false, want: nightlyJobFailed, failsNight: true},
		{conclusion: "neutral", required: true, want: nightlyJobFailed, failsNight: true},
	}
	for _, tc := range cases {
		job := githubJob{Name: "job", Conclusion: tc.conclusion, StartedAt: nightlyTestNow().Format(time.RFC3339)}
		if !tc.queued {
			job.Steps = []json.RawMessage{json.RawMessage(`{"name":"Set up job"}`)}
		}
		got := nightlyJobOutcome(job, tc.required)
		assert.Equal(t, tc.want, got, "conclusion %q, required %v, queued %v", tc.conclusion, tc.required, tc.queued)
		assert.Equal(t, tc.failsNight, got.failsTheNight(), "conclusion %q", tc.conclusion)
	}
	assert.Equal(t, nightlyJobNotRun, nightlyJobOutcome(githubJob{Conclusion: checkConclusionCancelled}, true),
		"a job with no start time did not run")

	reports := nightlyJobReports([]githubJob{{DatabaseID: 1, Name: "stress", Conclusion: checkConclusionSuccess}}, "")
	missing := []string{}
	for _, report := range reports {
		if report.result == nightlyJobNotRun {
			missing = append(missing, report.name)
		}
	}
	assert.Equal(t, []string{"verify-full", "verify", nightlySeedsJob, "integration", "e2e"}, missing,
		"a required job the run does not list did not run")
}

// Validates: R-6.10.23
//
// GitHub answers a share of requests with a 5xx, and one such answer to a
// job-log fetch used to end the DoD start stage. The fetch is repeated after
// a backoff and the check goes on.
func TestNightlyCheck_RetriesAJobLog5xxThenSucceeds(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobLogErrs["1"] = []error{githubBadGateway()}

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Equal(t, 2, remote.jobLogCalls["1"])
	assert.Equal(t, githubBackoffs()[:1], remote.sleeps.waits)
	claim, ok := remote.claim(nightlyTestKey)
	require.True(t, ok)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, nightlyTestKey+": claimed by this increment")
}

// Validates: R-6.10.23
//
// When GitHub keeps failing the run list, the check gives up after the retry
// bound and says so: which call, and what GitHub answered. It claims nothing,
// since it does not know what failed.
func TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.runListErrs = slices.Repeat([]error{githubBadGateway()}, githubAttempts+1)

	_, err := runNightlyCheck(t, remote)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "nightly run list")
	assert.Contains(t, err.Error(), fmt.Sprintf("GitHub answered HTTP 502 on all %d attempts", githubAttempts))
	assert.Len(t, remote.runListArgs, githubAttempts)
	assert.Equal(t, githubBackoffs(), remote.sleeps.waits)
	assert.Empty(t, remote.pushes)
	assert.Empty(t, remote.refs)
}

// Validates: R-6.10.23
//
// A job log the check cannot read, after the client's retries, never changes
// a verdict and never stops the check: the job is keyed by its name, the
// check says in the job's line that its log could not be read, and the next
// job's log is read and keyed as usual.
func TestNightlyCheckKeysAJobWhoseLogCannotBeRead(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobs = nightlyRunView(t, map[string]string{
		nightlyTestE2EJob: checkConclusionFailure,
		"stress":          checkConclusionFailure,
	})
	remote.jobLogErrs[nightlyTestE2ELog] = slices.Repeat([]error{githubBadGateway()}, githubAttempts)
	remote.jobLogs["13"] = "--- FAIL: TestWatchSoak (1.00s)\nFAIL\t" + repoModulePath + "/internal/multisync\t2.000s\n"

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Equal(t, githubAttempts, remote.jobLogCalls[nightlyTestE2ELog], "the log is still read with the client's retries")
	assert.Contains(t, out, "  e2e: failed; its log could not be read\n")
	assert.Equal(t, []string{"internal/multisync.TestWatchSoak", "job-e2e"}, remote.claimKeys())
}

// Validates: R-6.10.23
//
// The check reads scheduled runs of ci.yml on main only: push, pull-request
// and manual runs would otherwise fill the page or read as the nightly.
func TestNightlyCheckListsScheduledRunsOfMainOnly(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	_, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	require.Len(t, remote.runListArgs, 1)
	flags := ghFields(remote.runListArgs[0])
	assert.Equal(t, ciEventSchedule, flags["--event"])
	assert.Equal(t, mainBranchName, flags["--branch"])
	assert.Equal(t, nightlyWorkflowFile, flags["--workflow"])
}

// Validates: R-6.10.16, R-6.10.23
//
// GitHub can fail a request more than once in a row and then answer: the
// check waits out each backoff and goes on.
func TestNightlyCheck_GitHubFailingTwiceThenAnsweringPasses(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.runListErrs = []error{githubBadGateway(), githubBadGateway()}

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Equal(t, githubBackoffs()[:2], remote.sleeps.waits)
	assert.Contains(t, out, nightlyTestKey+": claimed by this increment")
}

// Validates: R-6.10.23
func TestNightlyCheckClaimsUnownedFailure(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, ok := remote.claim(nightlyTestKey)
	require.True(t, ok)
	assert.Equal(t, nightlyStateClaimed, claim.State)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Equal(t, int64(nightlyTestRunID), claim.RunID)
	assert.Equal(t, nightlyClaimFormat, claim.Format)
	assert.Equal(t, []string{nightlyTestE2EJob}, claim.Jobs, "the claim records the job a fix must make pass")
	assert.Contains(t, out, nightlyTestKey+": claimed by this increment")
}

// nightlyBoundWorkflow is a workflow whose scheduled run can take at most
// 380 minutes: the e2e job waits up to one 180-minute run of its group, runs
// up to 180 minutes, and the report job after it up to 10, plus the margin.
const nightlyBoundWorkflow = `jobs:
  verify-unit:
    timeout-minutes: 20
    if: github.event_name == 'pull_request' || github.event_name == 'push'
  verify-full:
    timeout-minutes: 45
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
  verify:
    timeout-minutes: 5
    needs: [verify-unit, verify-full]
    if: always()
  e2e:
    timeout-minutes: ${{ github.event_name == 'pull_request' && 40 || 180 }}
    concurrency:
      group: live-account-e2e
      cancel-in-progress: false
    if: vars.AZURE_CLIENT_ID != '' && github.event_name != 'push'
  nightly-report:
    timeout-minutes: 10
    needs: [verify, e2e]
    if: always() && github.event_name == 'schedule' || always() && github.event_name == 'workflow_dispatch'
`

// nightlyTestBound is nightlyBoundWorkflow's bound.
const nightlyTestBound = 380 * time.Minute

// Validates: R-6.10.23
//
// The bound on a scheduled run is read from the workflow: the longest chain
// of jobs that run on the schedule, linked by needs:, each waiting a whole
// run of its concurrency group and then running to its timeout.
func TestNightlyRunBoundIsTheLongestScheduledChain(t *testing.T) {
	t.Parallel()

	bound, err := nightlyRunBound(ciWorkflowJobs(nightlyBoundWorkflow))
	require.NoError(t, err)
	assert.Equal(t, nightlyTestBound, bound)

	jobs, err := readCIWorkflowJobs(repoRootForTest(t))
	require.NoError(t, err)
	repoBound, err := nightlyRunBound(jobs)
	require.NoError(t, err)
	assert.Greater(t, repoBound, 3*time.Hour, "the repository's nightly e2e job alone runs up to three hours")
}

// Validates: R-6.10.23
//
// A newer scheduled run that is still within its bound has no verdict yet;
// the newest completed run decides, whatever its conclusion.
func TestNightlyCheckReadsTheNewestCompletedRun(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.runs = nightlyRunList(t,
		nightlyListedRun{id: 5000, status: checkStatusInProgress, age: time.Hour},
		nightlyListedRun{id: nightlyTestRunID, status: checkStatusCompleted, conclusion: checkConclusionCancelled, age: 25 * time.Hour},
	)

	out, err := runNightlyCheckIn(t, remote, writeWorkflowFixture(t, nightlyBoundWorkflow))
	require.NoError(t, err)

	assert.Contains(t, out, fmt.Sprintf("run %d", nightlyTestRunID))
	_, claimed := remote.claim(nightlyTestKey)
	assert.True(t, claimed)
}

// Validates: R-6.10.23
//
// A scheduled run still running past the longest a scheduled run can take
// has no verdict and will not get one in time: the check says so in one
// sentence and fails, instead of reading an older verdict as current.
func TestNightlyCheckFindsARunStillRunningPastItsBound(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.runs = nightlyRunList(t,
		nightlyListedRun{id: 5000, status: checkStatusInProgress, age: nightlyTestBound + time.Minute},
		nightlyListedRun{id: nightlyTestRunID, status: checkStatusCompleted, conclusion: checkConclusionFailure, age: 30 * time.Hour},
	)

	_, err := runNightlyCheckIn(t, remote, writeWorkflowFixture(t, nightlyBoundWorkflow))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "run 5000")
	assert.Contains(t, err.Error(), "still running")
	assert.NotContains(t, err.Error(), "\n")
	assert.Empty(t, remote.pushes, "a run without a verdict claims nothing")
}

// Validates: R-6.10.23
//
// A rerun puts a scheduled run back to running and keeps the time the run was
// created: the bound counts from the start of its latest attempt, so a rerun
// started within it is passed over and the older completed run decides.
func TestNightlyCheckPassesOverARerunStartedWithinItsBound(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.runs = nightlyRunList(t,
		nightlyListedRun{id: 5000, status: checkStatusInProgress, age: 30 * time.Hour, attemptAge: time.Hour},
		nightlyListedRun{id: nightlyTestRunID, status: checkStatusCompleted, conclusion: checkConclusionFailure, age: 34 * time.Hour},
	)

	out, err := runNightlyCheckIn(t, remote, writeWorkflowFixture(t, nightlyBoundWorkflow))
	require.NoError(t, err)

	assert.Contains(t, out, fmt.Sprintf("run %d", nightlyTestRunID))
	_, claimed := remote.claim(nightlyTestKey)
	assert.True(t, claimed, "the older completed run decides")
}

// Validates: R-6.10.23
//
// No scheduled run in the last 36 hours is itself a finding: a nightly that
// stopped running must not read as a nightly that passed.
func TestNightlyCheckFindsNoScheduledRunInTheLast36Hours(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"the newest run is old": nightlyRunList(t, nightlyListedRun{
			id: nightlyTestRunID, status: checkStatusCompleted, conclusion: checkConclusionSuccess,
			age: nightlyStaleAfter + time.Minute,
		}),
		"there is no run": "[]",
	}
	for name, runs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := newClaimRemote(t, checkConclusionSuccess)
			remote.runs = runs

			_, err := runNightlyCheck(t, remote)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "36 hours")
			assert.NotContains(t, err.Error(), "\n")
		})
	}
}

// Validates: R-6.10.23
//
// A red run from which no failure can be keyed would leave nothing to claim
// and pass the check; it is a finding instead.
func TestNightlyCheckFailsOnRedRunWithoutKeys(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)

	_, err := runNightlyCheck(t, remote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no job of it failed")
	assert.Empty(t, remote.pushes)
}

// Validates: R-6.10.23
//
// A job that timed out or that the runner canceled failed the night: its
// tests, or the job itself when its log names none, are claimed, and the
// check names how each job ended.
func TestNightlyCheckCountsTimedOutAndCanceledJobsAsFailures(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{
		"stress":                     "timed_out",
		"verify-full (macos-latest)": checkConclusionCancelled,
		"verify":                     checkConclusionFailure,
	})
	remote.jobLogs["13"] = "panic: test timed out after 40m0s\n\trunning tests:\n\t\tTestWatchSoak (39m59s)\n" +
		"FAIL\t" + repoModulePath + "/internal/multisync\t2400.100s\n"

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Contains(t, out, "stress: timed out")
	assert.Contains(t, out, "verify-full (macos-latest): canceled")
	assert.Equal(t, []string{"internal/multisync.TestWatchSoak", "job-verify-full--macos-latest"}, remote.claimKeys())
}

// Validates: R-6.10.23
func TestNightlyCheckSkipsFailureHeldByAnotherIncrement(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, &nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-time.Hour).Format(time.RFC3339),
	})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestOther, claim.Owner)
	assert.Empty(t, remote.pushes)
	assert.Contains(t, out, "being fixed elsewhere")
}

// Validates: R-6.10.23
func TestNightlyCheckLosesRaceToConcurrentClaim(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.beforePush = func(remote *claimRemote) {
		remote.seed(nightlyTestKey, &nightlyClaim{
			State: nightlyStateClaimed,
			Owner: nightlyTestOther,
			At:    nightlyTestNow().Format(time.RFC3339),
		})
	}

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestOther, claim.Owner, "the agent that pushed first keeps the claim")
	assert.Empty(t, remote.pushes)
	assert.Contains(t, out, "being fixed elsewhere")
}

// Validates: R-6.10.23
func TestNightlyCheckTakesOverExpiredClaim(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, &nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-NightlyClaimLease).Format(time.RFC3339),
	})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, "taken over from "+nightlyTestOther)
}

// Validates: R-6.10.23
func TestNightlyCheckRenewsOwnClaim(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, &nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOwner,
		At:    nightlyTestNow().Add(-23 * time.Hour).Format(time.RFC3339),
	})

	_, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestNow().Format(time.RFC3339), claim.At)
}

// Validates: R-6.10.23
func TestNightlyCheckWaitsForMergedFixTheRunDidNotContain(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyStateFixed, claim.State)
	assert.Contains(t, out, "awaiting the next nightly")
}

// Validates: R-6.10.23
func TestNightlyCheckReclaimsFailureWhoseFixDidNotHold(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})
	remote.ancestors["fix1.."+nightlyTestRunHead] = true

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyStateClaimed, claim.State)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, "did not hold")
}

// Validates: R-6.10.23
func TestNightlyCheckFailsWhenNoIncrementCanOwnTheFailure(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	var stdout bytes.Buffer
	err := checkNightly(t.Context(), remote, newGHClient(remote, nil), "/repo", "", nightlyTestNow, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), nightlyTestKey)
	assert.Contains(t, stdout.String(), "UNCLAIMED")
	assert.Empty(t, remote.refs)
}

// Validates: R-6.10.23
func TestNightlyCheckPrunesConfirmedFixesAndExpiredStaleClaims(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seed("e2e.TestConfirmed", &nightlyClaim{
		State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1", Jobs: []string{nightlyTestE2EJob},
	})
	remote.ancestors["fix1.."+nightlyTestRunHead] = true
	remote.seed("TestNotYetRun", &nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix2"})
	remote.seed("TestAbandoned", &nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-2 * NightlyClaimLease).Format(time.RFC3339),
	})
	remote.seed("TestInProgress", &nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-time.Hour).Format(time.RFC3339),
	})

	_, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Equal(t, []string{"TestInProgress", "TestNotYetRun"}, remote.claimKeys())
}

// Validates: R-6.10.23
//
// A key missing from a run's failures does not mean its test passed: the job
// that ran it may not have run, or may have failed before reaching it. A
// fixed claim is confirmed, and deleted, only when every job that produced
// it passed in a run that contains the fix.
func TestNightlyCheckKeepsAFixedClaimWhoseJobDidNotPass(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobs = nightlyRunView(t, map[string]string{
		nightlyTestE2EJob: checkConclusionFailure,
		"integration":     checkConclusionSkipped,
	})
	remote.seed("TestFixedWithoutJobs", &nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})
	remote.seed("e2e.TestFixedButE2EFailed", &nightlyClaim{
		State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1", Jobs: []string{nightlyTestE2EJob},
	})
	remote.seed("graph.TestFixedButNotRun", &nightlyClaim{
		State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1", Jobs: []string{"integration"},
	})
	remote.seed("internal/sync.TestFixedAndPassed", &nightlyClaim{
		State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1", Jobs: []string{"verify-full"},
	})
	remote.ancestors["fix1.."+nightlyTestRunHead] = true

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"TestFixedWithoutJobs", nightlyTestKey, "e2e.TestFixedButE2EFailed", "graph.TestFixedButNotRun",
	}, remote.claimKeys(), "only the claim whose job passed is confirmed, and a job that did not run is claimed by no one")
	assert.Contains(t, out, "integration: did not run")
}

// Validates: R-6.10.23
//
// Claims written before keys named their package are stale: the check names
// each one and how to delete it, and replaces one whose key fails again; it
// never ignores one or reads it as the claim of another test.
func TestNightlyCheckReportsAnOldFormatClaimAsStale(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure})
	remote.jobLogs["13"] = "the stress step never started\n"
	fresh := nightlyTestNow().Add(-time.Hour).Format(time.RFC3339)
	remote.seedOldFormat(nightlyTestStale, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther, At: fresh})
	remote.seedOldFormat("job-stress", &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther, At: fresh})
	const misnamed = "internal-sync.TestPlanner"
	remote.seedRecord(misnamed, "internal/sync.TestPlanner", &nightlyClaim{
		State: nightlyStateClaimed, Format: nightlyClaimFormat, Owner: nightlyTestOther, At: fresh,
	})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Contains(t, out, nightlyTestStale+": stale claim")
	assert.Contains(t, out, misnamed+": stale claim", "a record at a ref its key does not name is stale")
	assert.Contains(t, out, "devtool nightly release")
	assert.Contains(t, remote.refs, nightlyTestStale, "the check reports a stale claim; release deletes it")
	assert.Contains(t, remote.refs, misnamed)
	replaced, ok := remote.claim("job-stress")
	require.True(t, ok)
	assert.Equal(t, nightlyTestOwner, replaced.Owner, "a failing key's stale claim is replaced, not held")
	assert.Equal(t, nightlyClaimFormat, replaced.Format)
}

// Validates: R-6.10.23
func TestNightlyPreMergeGateRequiresHeldClaimsInPRBody(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.prBranch = nightlyTestOwner
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner})
	remote.seed("TestSomeoneElses", &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther})

	remote.prBody = "## Summary\nUnrelated change."
	_, err := requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), nightlyTestKey)
	assert.NotContains(t, err.Error(), "TestSomeoneElses")

	remote.prBody = "## Nightly failures\n- " + nightlyTestKey + ": reruns sync until it converges."
	head, err := requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7)
	require.NoError(t, err)
	assert.Equal(t, nightlyTestOwner, head)
}

// Validates: R-6.10.23
//
// The body names a key only as a whole word: a longer key that starts with
// it, or one that ends with it, is another test.
func TestNightlyPreMergeGateMatchesWholeKeys(t *testing.T) {
	t.Parallel()

	const key = "internal/sync.TestFoo"
	cases := map[string]bool{
		"- `internal/sync.TestFoo`: fixed.":      true,
		"Fixed internal/sync.TestFoo.":           true,
		"internal/sync.TestFoo":                  true,
		"(internal/sync.TestFoo, the race)":      true,
		"- internal/sync.TestFooBar: fixed.":     false,
		"- internal/sync.TestFoo_Leg: fixed.":    false,
		"- internal/sync.TestFoo.Sub: fixed.":    false,
		"- internal/sync.TestFoo/sub: fixed.":    false,
		"- x-internal/sync.TestFoo: fixed.":      false,
		"- multi.internal/sync.TestFoo: fixed.":  false,
		"- other/internal/sync.TestFoo: fixed.":  false,
		"- sync.TestFoo and internal/sync.TestF": false,
	}
	for body, want := range cases {
		remote := newClaimRemote(t, checkConclusionSuccess)
		remote.prBranch = nightlyTestOwner
		remote.prBody = body
		remote.seed(key, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner})

		_, err := requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7)
		assert.Equal(t, want, err == nil, "body %q", body)
	}
}

// Validates: R-6.10.23
//
// pre-merge's nightly step owns a pull request's claims through its head
// branch: the gate refuses a body that does not name a claim the head holds,
// which fails the stage, and once the body names them it renews them for
// that branch and no other, and prints the latest verdict.
func TestDODPreMergeNightlyGatesAndRenewsThePRHeadsClaims(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.prBranch = nightlyTestOther
	old := nightlyTestNow().Add(-20 * time.Hour).Format(time.RFC3339)
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther, At: old})
	remote.seed("e2e.TestNotThisPRs", &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner, At: old})
	gh := newGHClient(remote, nil)

	var stdout bytes.Buffer
	remote.prBody = "## Summary\nNames no nightly failure."
	err := runDODPreMergeNightly(t.Context(), remote, gh, "/repo", 7, nightlyTestNow, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), nightlyTestKey)
	assert.Empty(t, remote.pushes, "a refused gate renews nothing")

	remote.prBody = "## Nightly failures\n- " + nightlyTestKey + ": fixed."
	require.NoError(t, runDODPreMergeNightly(t.Context(), remote, gh, "/repo", 7, nightlyTestNow, &stdout))

	head, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestNow().Format(time.RFC3339), head.At, "the PR head's claim is renewed")
	other, _ := remote.claim("e2e.TestNotThisPRs")
	assert.Equal(t, old, other.At, "a claim of another branch is not")
	assert.Contains(t, stdout.String(), "nightly: renewed "+nightlyTestOther+"'s claim on "+nightlyTestKey)
	assert.Contains(t, stdout.String(), fmt.Sprintf("nightly: run %d on", nightlyTestRunID))
}

// Validates: R-6.10.23
func TestNightlyPostMergeMarksHeldClaimsFixed(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner})
	remote.seed("TestSomeoneElses", &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther})

	require.NoError(t, markNightlyClaimsFixed(t.Context(), remote, "/repo", nightlyTestOwner, "merge1", nightlyTestNow()))

	mine, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyStateFixed, mine.State)
	assert.Equal(t, "merge1", mine.FixCommit)
	other, _ := remote.claim("TestSomeoneElses")
	assert.Equal(t, nightlyStateClaimed, other.State)
}

// Validates: R-6.10.23
func TestNightlyReleaseDeletesOnlyTheHoldersClaim(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther})

	release := NightlyOptions{RepoRoot: "/repo", Action: NightlyActionRelease, Branch: nightlyTestOwner, Key: nightlyTestKey}
	require.Error(t, RunNightly(t.Context(), remote, release))
	_, held := remote.claim(nightlyTestKey)
	assert.True(t, held)

	release.Branch = nightlyTestOther
	require.NoError(t, RunNightly(t.Context(), remote, release))
	_, held = remote.claim(nightlyTestKey)
	assert.False(t, held)
}

// Validates: R-6.10.23
//
// A stale claim, here of the old key format, belongs to no current increment, so any
// branch may release it.
func TestNightlyReleaseDeletesAStaleOldFormatClaim(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seedOldFormat(nightlyTestStale, &nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})

	var stdout bytes.Buffer
	require.NoError(t, RunNightly(t.Context(), remote, NightlyOptions{
		RepoRoot: "/repo", Action: NightlyActionRelease, Branch: nightlyTestOwner, Key: nightlyTestStale, Stdout: &stdout,
	}))
	assert.NotContains(t, remote.refs, nightlyTestStale)
	assert.Contains(t, stdout.String(), "stale")
}

// Validates: R-6.10.23
//
// An increment renews its claims at every stage, not only when it starts, so
// one that runs for more than a day keeps the failures it is fixing.
func TestNightlyRenewClaimsRestampsOnlyTheOwnersClaims(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	old := nightlyTestNow().Add(-20 * time.Hour).Format(time.RFC3339)
	remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner, At: old, Jobs: []string{nightlyTestE2EJob}})
	remote.seed("e2e.TestFixedHere", &nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOwner, At: old, FixCommit: "fix1"})
	remote.seed("e2e.TestSomeoneElses", &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther, At: old})

	renewed, err := renewNightlyClaims(t.Context(), remote, "/repo", nightlyTestOwner, nightlyTestNow())
	require.NoError(t, err)

	assert.Equal(t, []string{nightlyTestKey}, renewed)
	mine, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestNow().Format(time.RFC3339), mine.At)
	assert.Equal(t, []string{nightlyTestE2EJob}, mine.Jobs)
	fixed, _ := remote.claim("e2e.TestFixedHere")
	assert.Equal(t, old, fixed.At)
	other, _ := remote.claim("e2e.TestSomeoneElses")
	assert.Equal(t, old, other.At)
}

// Validates: R-6.10.16, R-6.10.23
//
// When GitHub still fails a nightly read after every retry, the start stage
// fails with one plain sentence that names GitHub and the endpoint, and no
// output of gh beyond it.
func TestDODNightlyStartFailsWithOneSentenceWhenGitHubKeepsFailing(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.runListErrs = slices.Repeat([]error{githubBadGateway()}, githubAttempts)

	var stdout bytes.Buffer
	err := runDODNightly(t.Context(), remote, newGHClient(remote, remote.sleeps.sleep), "/repo",
		DODStageStart, nightlyTestOwner, nightlyTestNow, &stdout)
	require.Error(t, err)

	assert.Equal(t, fmt.Sprintf(
		"GitHub answered HTTP 502 on all %d attempts at nightly run list "+
			"(GET repos/{owner}/{repo}/actions/workflows/ci.yml/runs); run it again once GitHub answers",
		githubAttempts), err.Error())
	assert.Empty(t, remote.pushes)
}

// Validates: R-6.10.16, R-6.10.23
//
// An increment that has started is not stopped by GitHub failing a nightly
// read: pre-pr, pre-merge and post-merge print the same sentence as a
// warning and pass. pre-pr and pre-merge renew the branch's claims first.
func TestDODNightlyLaterStagesRenewAndWarn(t *testing.T) {
	t.Parallel()

	for _, stage := range []DODStage{DODStagePrePR, DODStagePreMerge, DODStagePostMerge} {
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()

			remote := failingNightlyRemote(t)
			remote.runListErrs = slices.Repeat([]error{githubBadGateway()}, githubAttempts)
			old := nightlyTestNow().Add(-20 * time.Hour).Format(time.RFC3339)
			remote.seed(nightlyTestKey, &nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner, At: old})

			var stdout bytes.Buffer
			require.NoError(t, runDODNightly(t.Context(), remote, newGHClient(remote, remote.sleeps.sleep), "/repo",
				stage, nightlyTestOwner, nightlyTestNow, &stdout))

			assert.Contains(t, stdout.String(), "warning: GitHub answered HTTP 502 on all")
			claim, _ := remote.claim(nightlyTestKey)
			if stage == DODStagePostMerge {
				assert.Equal(t, old, claim.At, "post-merge marks claims fixed; it renews none")
			} else {
				assert.Equal(t, nightlyTestNow().Format(time.RFC3339), claim.At)
			}
		})
	}
}

// Validates: R-6.10.16, R-6.10.23
//
// A later stage reports the nightly's verdict and its findings without
// failing on them: a red nightly the increment did not touch is not its
// failure.
func TestDODNightlyLaterStagesReportARedNightlyWithoutFailing(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	var stdout bytes.Buffer
	require.NoError(t, runDODNightly(t.Context(), remote, newGHClient(remote, nil), "/repo",
		DODStagePostMerge, nightlyTestOwner, nightlyTestNow, &stdout))

	assert.Contains(t, stdout.String(), "e2e: failed")
	assert.Empty(t, remote.pushes, "a later stage claims nothing")

	remote.runs = "[]"
	stdout.Reset()
	require.NoError(t, runDODNightly(t.Context(), remote, newGHClient(remote, nil), "/repo",
		DODStagePreMerge, "", nightlyTestNow, &stdout))
	assert.Contains(t, stdout.String(), "warning: nightly: no scheduled run")
}
