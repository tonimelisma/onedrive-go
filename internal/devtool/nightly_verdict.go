package devtool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// One rule judges a scheduled CI run, for the nightly check that hands its
// failures to increments and for the nightly report that shows its verdict
// on GitHub, so what an increment acts on and what the repository shows
// cannot disagree.
const (
	// nightlyReportJob is the job of ci.yml that reports the run it is part
	// of. It runs after every other job, so the report leaves it out.
	nightlyReportJob = "nightly-report"
	// nightlyAggregateJob fails whenever a verify job it waits for did not
	// succeed, so its failure repeats theirs.
	nightlyAggregateJob = "verify"
	// nightlySeedsJob runs the seeds profile, the night's search of new model
	// seeds.
	nightlySeedsJob = "model-seeds"
	// ciRaceJob runs the race profile on every push to main; the aggregate
	// waits for it, and the schedule skips it.
	ciRaceJob           = "verify-race"
	nightlyWorkflowFile = "ci.yml"
	nightlyRunListLimit = "10"
	// nightlyStaleAfter ends a nightly verdict's life: the schedule is daily,
	// and GitHub has started scheduled runs up to ten hours late.
	nightlyStaleAfter = 36 * time.Hour

	// A failed test's key is its package as go test names it, relative to the
	// module, a dot and its top-level name; a failing model seed's adds `/`,
	// its variable, `=` and the seed (seedReplay.key); a failure of a job that
	// no failed test names is keyed by each failed step the job's log names
	// (nightlyStepKey), or by the job when the log names none. A test's name
	// and a seed's assignment hold no dot, so the last dot of a test or seed
	// key ends its package, and a step or job key holds none.
	nightlyJobKeyPrefix  = "job-"
	nightlyStepKeyPrefix = "step-"
	nightlyKeySeparator  = "."
	// nightlyUnknownPackage and nightlyRootPackage stand for a log that never
	// named the package and for the module's root package. A Go import path
	// holds no parenthesis, so neither is the key of a real package.
	nightlyUnknownPackage = "(unknown-package)"
	nightlyRootPackage    = "(module-root)"

	checkConclusionTimedOut = "timed_out"

	nightlyRunListFields = "databaseId,status,conclusion,headSha,createdAt,startedAt,url"
	nightlyRunViewFields = "databaseId,event,headBranch,headSha,url,jobs"
)

// nightlyAggregateNeeds are the jobs the aggregate job waits for, whose
// failures it repeats. TestNightlyRequiredJobsExistInWorkflow holds the list
// to its needs: in ci.yml.
func nightlyAggregateNeeds() []string {
	return []string{"verify-lint", "verify-unit", ciRaceJob, "verify-full"}
}

// nightlyRequiredJobs are the jobs of ci.yml every scheduled run must pass,
// by job key; a matrix job's legs carry the key and their values in
// parentheses. TestNightlyRequiredJobsExistInWorkflow holds the list to the
// workflow's scheduled jobs and to the report job's needs.
func nightlyRequiredJobs() []string {
	return []string{"verify-full", nightlyAggregateJob, prCheckStress, nightlySeedsJob, prCheckIntegration, prCheckE2E}
}

// nightlyJobResult is how one job of a scheduled run ended, in the words the
// check and the report print.
type nightlyJobResult string

const (
	nightlyJobPassed nightlyJobResult = "passed"
	// nightlyJobSkipped is a job the night does not require, skipped by its
	// own condition: the PR lane on the schedule.
	nightlyJobSkipped  nightlyJobResult = "skipped"
	nightlyJobFailed   nightlyJobResult = "failed"
	nightlyJobTimedOut nightlyJobResult = "timed out"
	nightlyJobCanceled nightlyJobResult = "canceled"
	// nightlyJobNotRun is a job the night requires that was skipped or that
	// the run does not list, or a job canceled before a runner picked it up:
	// its tests did not run, which is not a pass. No change to the code can
	// make it run, so no increment claims it.
	nightlyJobNotRun nightlyJobResult = "did not run"
	// nightlyJobUnfinished is a job without a conclusion yet.
	nightlyJobUnfinished nightlyJobResult = "did not finish"
)

// failsTheNight reports whether a job that ended so makes the night red.
func (r nightlyJobResult) failsTheNight() bool {
	return r != nightlyJobPassed && r != nightlyJobSkipped
}

// nightlyJobOutcome gives one job its result. A job that timed out, or that
// was canceled after it started, failed the night under its own name; any
// conclusion this rule does not name (neutral, action_required, stale,
// startup_failure and the like) fails it too.
func nightlyJobOutcome(job githubJob, required bool) nightlyJobResult {
	switch strings.ToLower(job.Conclusion) {
	case "":
		return nightlyJobUnfinished
	case checkConclusionSuccess:
		return nightlyJobPassed
	case checkConclusionSkipped:
		if required {
			return nightlyJobNotRun
		}
		return nightlyJobSkipped
	case checkConclusionTimedOut:
		return nightlyJobTimedOut
	case checkConclusionCancelled:
		if !job.started() {
			return nightlyJobNotRun
		}
		return nightlyJobCanceled
	default:
		return nightlyJobFailed
	}
}

// nightlyJobReport is one job of a scheduled run under the verdict rule.
type nightlyJobReport struct {
	// name is the job as the run lists it, a matrix leg with its values.
	name string
	// id is the job's id, zero for a required job the run does not list.
	id     int64
	result nightlyJobResult
	// tests are the keys of the failed tests its log names, a failing seed
	// keyed by its replay line in place of its test.
	tests []string
	// seeds are the replay lines of the failing seeds its log names.
	seeds []seedReplay
	// steps are the keys of the failed steps its log's marker lines name,
	// each once and in log order: every step that runs no go test, and a go
	// test step only when a failure of the job is one no failed test names
	// (failsWithoutTest).
	steps []string
	// packageFailed records that its log names a package that failed without
	// naming a test: a build or setup failure, a TestMain exit, a package
	// timeout with no test running.
	packageFailed bool
	// logUnreadable records that its log could not be read.
	logUnreadable bool
}

// workflowJob is the ci.yml job the report's job is a run of.
func (r nightlyJobReport) workflowJob() string {
	return ciCheckJob(r.name)
}

// failsWithoutTest reports whether a failure of the job is one no failed test
// names: its log names no failed test, or names a package that failed without
// naming one. Such a failure is keyed by the job's failed steps, or by the
// job when its log names no failed step.
func (r nightlyJobReport) failsWithoutTest() bool {
	return len(r.tests) == 0 || r.packageFailed
}

// hasLog reports whether the job ran far enough to leave a log that can name
// failed tests.
func (r nightlyJobReport) hasLog() bool {
	return r.id != 0 && slices.Contains(
		[]nightlyJobResult{nightlyJobFailed, nightlyJobTimedOut, nightlyJobCanceled}, r.result)
}

// nightlyJobReports judges every job of a run but the one named ignore, and
// adds each required job the run does not list as not run.
func nightlyJobReports(jobs []githubJob, ignore string) []nightlyJobReport {
	required := nightlyRequiredJobs()
	listed := map[string]bool{}
	reports := make([]nightlyJobReport, 0, len(jobs)+len(required))
	for _, job := range jobs {
		if job.Name == ignore {
			continue
		}
		report := nightlyJobReport{name: job.Name, id: job.DatabaseID}
		listed[report.workflowJob()] = true
		report.result = nightlyJobOutcome(job, slices.Contains(required, report.workflowJob()))
		reports = append(reports, report)
	}
	for _, name := range required {
		if !listed[name] {
			reports = append(reports, nightlyJobReport{name: name, result: nightlyJobNotRun})
		}
	}

	return reports
}

// nightlyFailedJobs are the reports whose jobs made the night red.
func nightlyFailedJobs(reports []nightlyJobReport) []nightlyJobReport {
	failed := []nightlyJobReport{}
	for _, report := range reports {
		if report.result.failsTheNight() {
			failed = append(failed, report)
		}
	}

	return failed
}

// nightlyAggregateRepeats reports whether reports[i] is the aggregate job
// failing alongside a job it waits for, whose failure it only repeats.
func nightlyAggregateRepeats(reports []nightlyJobReport, i int) bool {
	if reports[i].workflowJob() != nightlyAggregateJob {
		return false
	}
	needs := nightlyAggregateNeeds()
	for _, other := range reports {
		if other.result.failsTheNight() && slices.Contains(needs, other.workflowJob()) {
			return true
		}
	}

	return false
}

// attributeNightlyFailures reads the log of every job that failed the night
// and has one, and records the failed tests it names, whether a package
// failed without naming one, and the failed steps its marker lines name:
// each step that runs no go test, and each go test step too when a failure
// of the job is one no failed test names. A failed test whose
// seeds the log names in replay lines is recorded as those seeds, each keyed
// by its replay, instead of as the test. The aggregate job's log is not read
// while a job it waits for failed. A log that cannot be read, after the
// client's retries, is recorded as such and the next job's log is read: a
// job's verdict never depends on its log. Only a canceled context ends the
// reads.
func attributeNightlyFailures(ctx context.Context, gh ghClient, repoRoot string, reports []nightlyJobReport) error {
	for i := range reports {
		if !reports[i].hasLog() || nightlyAggregateRepeats(reports, i) {
			continue
		}
		id := strconv.FormatInt(reports[i].id, 10)
		log, err := gh.output(
			ctx, repoRoot,
			fmt.Sprintf("nightly job log %s (GET repos/{owner}/{repo}/actions/jobs/%s/logs)", reports[i].name, id),
			"api", "repos/{owner}/{repo}/actions/jobs/"+id+"/logs",
		)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("read nightly job log %s: %w", reports[i].name, ctx.Err())
			}
			reports[i].logUnreadable = true
			continue
		}
		parsed := parseGoTestLog(string(log))
		reports[i].tests, reports[i].seeds = nightlyFailureNames(parsed, seedReplaysFromLog(string(log)))
		reports[i].packageFailed = parsed.failedPackageWithoutTest()
		reports[i].steps = nightlyFailedStepKeys(reports[i].name, string(log), reports[i].failsWithoutTest())
	}

	return nil
}

// nightlyFailureNames keys a log's failed top-level tests: a test whose
// failing seeds the log names, in replay lines or in the names of failing
// subtests that printed none, by the key of each seed, once; any other test
// by its test key. A seeded family's failing subtest whose name holds no seed
// keeps its test's key beside the seeds, so the failure stays claimable.
func nightlyFailureNames(log goTestLog, replays []seedReplay) ([]string, []seedReplay) {
	keys := []string{}
	seeds := []seedReplay{}
	seen := map[string]bool{}
	for _, failure := range log.failedTopLevelTestsByPackage() {
		failing, complete := failingSeedsOf(failure, log.failures, replays)
		for _, seed := range failing {
			if key := seed.key(); !seen[key] {
				seen[key] = true
				keys = append(keys, key)
				seeds = append(seeds, seed)
			}
		}
		if len(failing) == 0 || !complete {
			keys = append(keys, nightlyFailedTestKey(failure))
		}
	}

	return keys, seeds
}

// nightlyFailureKeys names what failed in a run, each key with the ci.yml
// jobs it failed in: every failed test, every failed step that runs no go
// test, and, for a job whose log names no test or names a package that
// failed without naming one, every failed go test step its log names too,
// or the job itself when it names no failed step. A job that did not run
// yields no key, and the aggregate none while a job it waits for failed.
func nightlyFailureKeys(reports []nightlyJobReport) map[string][]string {
	jobsByKey := map[string]map[string]bool{}
	add := func(key string, job string) {
		if jobsByKey[key] == nil {
			jobsByKey[key] = map[string]bool{}
		}
		jobsByKey[key][job] = true
	}
	for i, report := range reports {
		if !report.result.failsTheNight() || report.result == nightlyJobNotRun || nightlyAggregateRepeats(reports, i) {
			continue
		}
		if report.failsWithoutTest() && len(report.steps) == 0 {
			add(nightlyJobKeyPrefix+sanitizeNightlyKey(report.name), report.workflowJob())
		}
		for _, step := range report.steps {
			add(step, report.workflowJob())
		}
		for _, test := range report.tests {
			add(test, report.workflowJob())
		}
	}

	failures := make(map[string][]string, len(jobsByKey))
	for key, jobs := range jobsByKey {
		failures[key] = sortedSetKeys(jobs)
	}

	return failures
}

// nightlyJobsPassed reports whether every named ci.yml job ran and passed in
// every leg. With no job named, every job of the run must have passed.
func nightlyJobsPassed(jobs []string, reports []nightlyJobReport) bool {
	if len(jobs) == 0 {
		return len(nightlyFailedJobs(reports)) == 0
	}
	for _, job := range jobs {
		ran := false
		for _, report := range reports {
			if report.workflowJob() != job {
				continue
			}
			if report.result != nightlyJobPassed {
				return false
			}
			ran = true
		}
		if !ran {
			return false
		}
	}

	return true
}

// nightlyFailedTestKey is the claim key of a failed test: its package as go
// test names it, relative to the module (internal/sync), a dot and its
// top-level name. The key keeps the package's separators, so two packages
// never share one; nightlyClaimRefName makes a ref name of it.
func nightlyFailedTestKey(failure goTestFailure) string {
	qualifier := strings.TrimPrefix(failure.pkg, "./")
	switch failure.pkg {
	case "":
		qualifier = nightlyUnknownPackage
	case ".":
		qualifier = nightlyRootPackage
	}

	return qualifier + nightlyKeySeparator + failure.test
}

// sanitizeNightlyKey maps a job's name, a matrix leg's values included, or a
// step's name onto letters, digits, '_' and '-', so a job or step key needs no
// quoting on a command line: `verify-full (macos-latest)` is keyed
// job-verify-full--macos-latest.
func sanitizeNightlyKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// nightlyStepKey is the claim key of a failed step of a job: step-, the job
// and the step, each through sanitizeNightlyKey, so the key of the lint step
// of `verify-full (macos-latest)` is step-verify-full--macos-latest-lint. It
// is its own claim ref name (TestNightlyCheckClaimsAFailedStepUnderItsOwnKey).
func nightlyStepKey(job string, step verifyStepName) string {
	return nightlyStepKeyPrefix + sanitizeNightlyKey(job) + "-" + sanitizeNightlyKey(string(step))
}

// nightlyFailedStepKeys keys the failed steps the marker lines of a job's log
// name, each once and in log order: every step that runs no go test, whose
// failure no failed test can name, and a go test step only withGoTestSteps,
// when a failure of the job is one no failed test names. Nothing depends on
// where a marker lands among the tests' lines, which reach the log on
// another pipe. A line is read the way go test lines are,
// after the byte-order mark and the timestamp CI adds, and a -json event is
// never a marker, since a step prints its marker outside go test's output.
func nightlyFailedStepKeys(job string, log string, withGoTestSteps bool) []string {
	keys := []string{}
	seen := map[string]bool{}
	for line := range strings.Lines(log) {
		text, _, isEvent := decodeGoTestLogLine(line)
		if isEvent {
			continue
		}
		step, isMarker := readFailedStepMarker(text)
		if !isMarker || (step.runsGoTest() && !withGoTestSteps) {
			continue
		}
		if key := nightlyStepKey(job, step); !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}

	return keys
}

func sortedSetKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// nightlyVerdict is what the scheduled runs say about main now.
type nightlyVerdict struct {
	// run is the newest completed scheduled run, its jobs included; found is
	// false when every listed run is still within its bound.
	run   githubRun
	found bool
	// reports are run's jobs under the verdict rule.
	reports []nightlyJobReport
	// finding is set when the nightly has no verdict to act on: one plain
	// sentence that says why.
	finding string
}

// loadNightlyVerdict reads the newest completed scheduled run of ci.yml on
// main and its jobs. A newer run still running within the longest a
// scheduled run can take, counted from the start of its latest attempt, has
// no verdict yet and is passed over; one running past it, and no scheduled
// run started in the last nightlyStaleAfter, are findings. The workflow is
// read only when a run is still running.
func loadNightlyVerdict(ctx context.Context, gh ghClient, repoRoot string, now time.Time) (nightlyVerdict, error) {
	runs, err := listNightlyRuns(ctx, gh, repoRoot, nightlyRunListLimit)
	if err != nil {
		return nightlyVerdict{}, err
	}

	if finding, staleErr := nightlyStaleFinding(runs, now); staleErr != nil || finding != "" {
		return nightlyVerdict{finding: finding}, staleErr
	}
	bound := time.Duration(0)
	for i := range runs {
		if strings.EqualFold(runs[i].Status, checkStatusCompleted) {
			return loadNightlyVerdictOf(ctx, gh, repoRoot, &runs[i])
		}
		if bound == 0 {
			if bound, err = readNightlyRunBound(repoRoot); err != nil {
				return nightlyVerdict{}, err
			}
		}
		started, startedAt, parseErr := parseAttemptStart(&runs[i])
		if parseErr != nil {
			return nightlyVerdict{}, parseErr
		}
		if running := now.Sub(started); running > bound {
			return nightlyVerdict{finding: fmt.Sprintf(
				"nightly: scheduled run %d (%s) started at %s and is still running after %s, "+
					"longer than a scheduled run can take (%s), so the nightly has no verdict",
				runs[i].DatabaseID, runs[i].URL, startedAt, running.Round(time.Minute), bound)}, nil
		}
	}

	return nightlyVerdict{}, nil
}

// listNightlyRuns lists at most limit scheduled runs of ci.yml on main,
// through the retrying client: the one list the nightly check and the
// nightly report read. The runs are sorted newest first by their creation,
// whatever order GitHub lists them in.
func listNightlyRuns(ctx context.Context, gh ghClient, repoRoot string, limit string) ([]githubRun, error) {
	out, err := gh.output(
		ctx, repoRoot,
		fmt.Sprintf("nightly run list (GET repos/{owner}/{repo}/actions/workflows/%s/runs)", nightlyWorkflowFile),
		"run", "list",
		"--workflow", nightlyWorkflowFile,
		"--event", ciEventSchedule,
		"--branch", mainBranchName,
		"--limit", limit,
		"--json", nightlyRunListFields,
	)
	if err != nil {
		return nil, err
	}
	var runs []githubRun
	if decodeErr := json.Unmarshal(out, &runs); decodeErr != nil {
		return nil, fmt.Errorf("decode nightly run list: %w", decodeErr)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt > runs[j].CreatedAt })

	return runs, nil
}

// parseAttemptStart is when a run's latest attempt started, and the time as
// GitHub wrote it: a rerun keeps the run's creation time and starts a new
// attempt, so the bound on a running run counts from the attempt. A run that
// names no start is read from its creation.
func parseAttemptStart(run *githubRun) (time.Time, string, error) {
	startedAt := run.StartedAt
	if startedAt == "" {
		startedAt = run.CreatedAt
	}
	started, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("nightly run %d attempt start %q: %w", run.DatabaseID, startedAt, err)
	}

	return started, startedAt, nil
}

// nightlyStaleFinding is the finding that no scheduled run started in the
// last nightlyStaleAfter, or "" when one did; runs are newest first. A
// scheduled run starts when it is created, and a rerun of an old one does
// not make the nightly current, so the run's creation counts.
func nightlyStaleFinding(runs []githubRun, now time.Time) (string, error) {
	newest := ""
	if len(runs) > 0 {
		created, err := parseRunCreated(&runs[0])
		if err != nil {
			return "", err
		}
		if now.Sub(created) <= nightlyStaleAfter {
			return "", nil
		}
		newest = fmt.Sprintf(" (the newest, run %d, started at %s)", runs[0].DatabaseID, runs[0].CreatedAt)
	}

	return fmt.Sprintf("nightly: no scheduled run of %s on %s started in the last %d hours%s, so the nightly has no verdict",
		nightlyWorkflowFile, mainBranchName, int(nightlyStaleAfter.Hours()), newest), nil
}

func parseRunCreated(run *githubRun) (time.Time, error) {
	created, err := time.Parse(time.RFC3339, run.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("nightly run %d creation %q: %w", run.DatabaseID, run.CreatedAt, err)
	}

	return created, nil
}

// loadNightlyVerdictOf reads the jobs of the completed run listed.
func loadNightlyVerdictOf(ctx context.Context, gh ghClient, repoRoot string, listed *githubRun) (nightlyVerdict, error) {
	viewed, err := loadNightlyRun(ctx, gh, repoRoot, listed.DatabaseID)
	if err != nil {
		return nightlyVerdict{}, err
	}
	run := *listed
	run.Jobs = viewed.Jobs

	return nightlyVerdict{run: run, found: true, reports: nightlyJobReports(run.Jobs, "")}, nil
}

// loadNightlyRun reads one workflow run with the jobs of its latest attempt.
func loadNightlyRun(ctx context.Context, gh ghClient, repoRoot string, runID int64) (githubRun, error) {
	id := strconv.FormatInt(runID, 10)
	out, err := gh.output(
		ctx, repoRoot,
		fmt.Sprintf("nightly run view %s (GET repos/{owner}/{repo}/actions/runs/%s/jobs)", id, id),
		"run", "view", id, "--json", nightlyRunViewFields,
	)
	if err != nil {
		return githubRun{}, err
	}
	var run githubRun
	if decodeErr := json.Unmarshal(out, &run); decodeErr != nil {
		return githubRun{}, fmt.Errorf("decode nightly run view %s: %w", id, decodeErr)
	}

	return run, nil
}

// writeNightlyRunVerdict prints a run's verdict: one line for the run and
// one for each job that made the night red, in the words of the verdict rule,
// saying when a job's log could not be read and that a job that did not run
// leaves nothing to claim.
func writeNightlyRunVerdict(b *strings.Builder, run *githubRun, reports []nightlyJobReport) {
	fmt.Fprintf(b, "nightly: run %d on %s concluded %s (%s)\n", run.DatabaseID, shortSHA(run.HeadSHA), run.Conclusion, run.URL)
	for _, report := range nightlyFailedJobs(reports) {
		fmt.Fprintf(b, "  %s: %s", report.name, report.result)
		switch {
		case report.result == nightlyJobNotRun:
			b.WriteString("; nothing to claim")
		case report.logUnreadable:
			b.WriteString("; its log could not be read")
		}
		b.WriteString("\n")
	}
}

// readNightlyRunBound reads the bound on a scheduled run from repoRoot's
// workflow.
func readNightlyRunBound(repoRoot string) (time.Duration, error) {
	jobs, err := readCIWorkflowJobs(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("nightly run bound: %w", err)
	}

	return nightlyRunBound(jobs)
}

// nightlyRunBound is the longest a scheduled run can take: the longest chain
// of jobs that run on the schedule, linked by needs:, in which each job first
// waits one whole run of its concurrency group (the longest timeout any job
// of the group has on any event) and then runs to its timeout, plus
// ciStallMargin. A job that does not run on the schedule is skipped at once.
// A run still running past it has gone wrong and gives no verdict in time.
func nightlyRunBound(jobs []ciWorkflowJob) (time.Duration, error) {
	chain := nightlyJobChain{
		jobs:         map[string]ciWorkflowJob{},
		groupLongest: map[string]time.Duration{},
		finish:       map[string]time.Duration{},
		visiting:     map[string]bool{},
	}
	for _, job := range jobs {
		chain.jobs[job.name] = job
		group, inGroup, err := job.concurrencyGroup()
		if err != nil {
			return 0, fmt.Errorf("nightly run bound: %w", err)
		}
		if !inGroup {
			continue
		}
		longest, err := job.longestTimeout()
		if err != nil {
			return 0, fmt.Errorf("nightly run bound: %w", err)
		}
		chain.groupLongest[group] = max(chain.groupLongest[group], longest)
	}

	longest := time.Duration(0)
	for _, job := range jobs {
		finish, err := chain.finishOf(job.name)
		if err != nil {
			return 0, err
		}
		longest = max(longest, finish)
	}

	return longest + ciStallMargin, nil
}

// nightlyJobChain computes, job by job, the latest a scheduled run's job can
// finish.
type nightlyJobChain struct {
	jobs         map[string]ciWorkflowJob
	groupLongest map[string]time.Duration
	finish       map[string]time.Duration
	visiting     map[string]bool
}

func (c *nightlyJobChain) finishOf(name string) (time.Duration, error) {
	if finish, done := c.finish[name]; done {
		return finish, nil
	}
	job, exists := c.jobs[name]
	if !exists {
		return 0, fmt.Errorf("nightly run bound: %s: a job needs %s, which is not a job of it", ciWorkflowPath, name)
	}
	if c.visiting[name] {
		return 0, fmt.Errorf("nightly run bound: %s: job %s needs itself", ciWorkflowPath, name)
	}
	c.visiting[name] = true
	defer delete(c.visiting, name)

	runs, err := job.runsOn(ciEventSchedule)
	if err != nil || !runs {
		return 0, err
	}
	needs, err := job.needs()
	if err != nil {
		return 0, err
	}
	start := time.Duration(0)
	for _, need := range needs {
		finish, needErr := c.finishOf(need)
		if needErr != nil {
			return 0, needErr
		}
		start = max(start, finish)
	}
	timeout, err := job.timeoutOn(ciEventSchedule)
	if err != nil {
		return 0, err
	}
	group, inGroup, err := job.concurrencyGroup()
	if err != nil {
		return 0, err
	}
	queue := time.Duration(0)
	if inGroup {
		queue = c.groupLongest[group]
	}
	c.finish[name] = start + queue + timeout

	return c.finish[name], nil
}
