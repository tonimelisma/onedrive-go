package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultDODCIMaxReruns is how often one DoD CI wait reruns the canceled
	// jobs of the commit it waits for before it fails (`--ci-max-reruns`).
	DefaultDODCIMaxReruns = 2

	// ciStallMargin is what the stall window adds to the longest timeout of
	// the jobs that run for the awaited event, so a job that runs into its own
	// timeout still changes state inside the window.
	ciStallMargin = 10 * time.Minute

	checkConclusionCancelled = "cancelled" //nolint:misspell // GitHub's spelling of the conclusion

	// The checks branch protection requires on a pull request. `verify` is
	// the aggregate job of the verify lane.
	prCheckVerify      = nightlyAggregateJob
	prCheckIntegration = "integration"
	prCheckE2E         = "e2e"
	// prCheckStress is not required, but a stress run that failed fails the
	// pull request.
	prCheckStress = "stress"

	prCILabel        = "dod pre-merge PR CI"
	postMergeCILabel = "dod post-merge CI"

	ciRunViewFields = "conclusion,headSha,jobs,status,updatedAt,url"
)

// actionsRunIDPattern takes the run id out of an Actions check's details URL:
// `.../actions/runs/<run id>/job/<job id>`.
var actionsRunIDPattern = regexp.MustCompile(`/actions/runs/(\d+)(?:/|$)`)

// ciWaitConfig is the policy and the clock of one DoD CI wait.
type ciWaitConfig struct {
	// stallWindow ends the wait when no check has changed state for this long.
	stallWindow time.Duration
	// notStartedWindows replaces stallWindow while a check of one of these
	// jobs has not started: the job sits in a concurrency group and can wait
	// for a whole run of the group from another event.
	notStartedWindows map[string]time.Duration
	// hardCap ends the wait this long after it began, changing or not.
	hardCap time.Duration
	// maxReruns bounds how often the wait reruns canceled jobs.
	maxReruns    int
	pollInterval time.Duration
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
	stdout       io.Writer
}

// newCIWaitConfig derives a wait's policy from the stage's options and the CI
// jobs that run for event. With no `--ci-timeout` the cap is one stall window
// for the first attempt and one for every rerun the wait may issue, plus the
// longest wait of a queued check for its concurrency group.
func newCIWaitConfig(opts *VerifyOptions, repoRoot string, event string, stdout io.Writer) (ciWaitConfig, error) {
	if opts.DODCIMaxReruns < 0 {
		return ciWaitConfig{}, fmt.Errorf("dod CI wait: --ci-max-reruns must not be negative, got %d", opts.DODCIMaxReruns)
	}

	jobs, err := readCIWorkflowJobs(repoRoot)
	if err != nil {
		return ciWaitConfig{}, fmt.Errorf("derive CI stall window: %w", err)
	}
	stallWindow, err := ciStallWindow(jobs, event)
	if err != nil {
		return ciWaitConfig{}, err
	}
	notStartedWindows, err := ciNotStartedWindows(jobs, event)
	if err != nil {
		return ciWaitConfig{}, err
	}

	hardCap := opts.DODCITimeout
	if hardCap <= 0 {
		longestQueue := time.Duration(0)
		for _, window := range notStartedWindows {
			longestQueue = max(longestQueue, window)
		}
		hardCap = time.Duration(opts.DODCIMaxReruns+1)*stallWindow + longestQueue
	}

	return ciWaitConfig{
		stallWindow:       stallWindow,
		notStartedWindows: notStartedWindows,
		hardCap:           hardCap,
		maxReruns:         opts.DODCIMaxReruns,
		pollInterval:      prCIPollInterval,
		now:               time.Now,
		sleep:             sleepContext,
		stdout:            stdout,
	}, nil
}

// ciStallWindow is the longest timeout-minutes among the CI jobs that run
// for event, plus ciStallMargin. No check can legitimately stay in one state
// longer than the longest job ahead of it runs, and reading the numbers and
// the job conditions from the workflow means lengthening a job there cannot
// leave the wait giving up on it early. A job that does not run for the event
// does not count: a push to main runs only the PR lane, so the nightly-only
// jobs' hours say nothing about it. Every job's timeout is read, run or not,
// so a timeout the reader cannot evaluate fails the stage before it waits.
func ciStallWindow(jobs []ciWorkflowJob, event string) (time.Duration, error) {
	longest := time.Duration(0)
	for _, job := range jobs {
		timeout, timeoutErr := job.timeoutOn(event)
		if timeoutErr != nil {
			return 0, fmt.Errorf("derive CI stall window: %w", timeoutErr)
		}
		runs, runsErr := job.runsOn(event)
		if runsErr != nil {
			return 0, fmt.Errorf("derive CI stall window: %w", runsErr)
		}
		if runs {
			longest = max(longest, timeout)
		}
	}
	if longest == 0 {
		return 0, fmt.Errorf("derive CI stall window: no job of %s runs on %s", ciWorkflowPath, event)
	}

	return longest + ciStallMargin, nil
}

// ciNotStartedWindows gives every job that runs for event in a concurrency
// group the stall window of a check of it that has not started: the longest
// timeout any job of the group has on any event, plus ciStallMargin. Such a
// check can wait, without any state change, for one whole run of the group
// from another event, a pull request's e2e behind the nightly's. Once it
// starts, its state change restarts the window and the event's own applies.
func ciNotStartedWindows(jobs []ciWorkflowJob, event string) (map[string]time.Duration, error) {
	groupLongest := map[string]time.Duration{}
	groupOf := map[string]string{}
	for _, job := range jobs {
		group, inGroup, err := job.concurrencyGroup()
		if err != nil {
			return nil, fmt.Errorf("derive CI stall window: %w", err)
		}
		if !inGroup {
			continue
		}
		longest, err := job.longestTimeout()
		if err != nil {
			return nil, fmt.Errorf("derive CI stall window: %w", err)
		}
		groupLongest[group] = max(groupLongest[group], longest)
		runs, err := job.runsOn(event)
		if err != nil {
			return nil, fmt.Errorf("derive CI stall window: %w", err)
		}
		if runs {
			groupOf[job.name] = group
		}
	}

	windows := make(map[string]time.Duration, len(groupOf))
	for name, group := range groupOf {
		windows[name] = groupLongest[group] + ciStallMargin
	}

	return windows, nil
}

// stallWindowFor is the stall window while the named checks have not
// started: the longest of the event's window and the queue window of each
// such check's job.
func (c ciWaitConfig) stallWindowFor(notStarted []string) time.Duration {
	window := c.stallWindow
	for _, check := range notStarted {
		window = max(window, c.notStartedWindows[ciCheckJob(check)])
	}

	return window
}

// ciCheckJob is the workflow job a check belongs to: the check's name up to
// the parenthesized values of a matrix leg.
func ciCheckJob(check string) string {
	job, _, _ := strings.Cut(strings.ToLower(check), " (")

	return job
}

// sleepContext waits for d or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("sleep: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// ciSnapshot is one poll's reading of the CI a wait follows.
type ciSnapshot struct {
	state prCheckState
	// reason says where CI stands, for the message that ends the wait.
	reason string
	// pending names the required checks that have not finished, sorted.
	pending []string
	// notStarted names those of them that have not started, sorted: any
	// status but in progress, and a check not yet reported. GitHub has not
	// shown this repository's pull requests a status for a job waiting in its
	// concurrency group, so every such status counts.
	notStarted []string
	// failedCheck names the failure; failedRunID is the run to examine for
	// canceled jobs, zero when no rerun could clear the failure.
	failedCheck string
	failedRunID int64
	// fingerprint changes exactly when some check changes state.
	fingerprint string
}

// ciRunJobs sorts a run's finished jobs by what a wait can do about them.
type ciRunJobs struct {
	// failed failed for real: a rerun would only hide them for one more round.
	failed []string
	// canceled were stopped by the runner or displaced in a concurrency group.
	canceled []string
}

// ciWaitTarget is what one DoD CI wait follows: how to read where CI stands,
// and the commit whose runs a rerun is allowed for.
type ciWaitTarget struct {
	label      string
	failedVerb string
	poll       func(context.Context) (ciSnapshot, error)
	headSHA    func(context.Context) (string, error)
}

// ciStep is what the wait does after one poll: stop, or keep waiting for the
// named checks.
type ciStep struct {
	done    bool
	reason  string
	pending []string
}

// describe names what the wait is still waiting for.
func (s ciStep) describe() string {
	if len(s.pending) == 0 {
		return s.reason
	}

	return "still pending: " + strings.Join(s.pending, ", ")
}

// ciWait is the state of one DoD CI wait. Its loop in run is the only owner.
type ciWait struct {
	gh       ghClient
	repoRoot string
	target   ciWaitTarget
	cfg      ciWaitConfig

	start           time.Time
	lastChange      time.Time
	lastFingerprint string
	rerunsUsed      int
	// rerunUpdatedAt holds each rerun run's updatedAt as it was when the rerun
	// was requested. A run that still shows that value has not started the
	// rerun, and must not be rerun a second time for the same cancellation.
	rerunUpdatedAt map[int64]string
}

// waitForPRCI waits until the pull request's required checks pass, and fails
// on the first real failure. A required check that failed only because jobs of
// the head commit's run were canceled is rerun, at most cfg.maxReruns times.
// The wait ends when no check has changed state for the stall window, or at the
// hard cap.
func waitForPRCI(ctx context.Context, runner commandRunner, repoRoot string, prNumber int, cfg ciWaitConfig) error {
	gh := newGHClient(runner, cfg.sleep)

	return runCIWait(ctx, gh, repoRoot, ciWaitTarget{
		label:      prCILabel,
		failedVerb: "failed",
		poll: func(ctx context.Context) (ciSnapshot, error) {
			state, err := loadPullRequestState(ctx, gh, repoRoot, prNumber)
			if err != nil {
				return ciSnapshot{}, err
			}

			return classifyPRChecks(state.StatusCheckRollup), nil
		},
		headSHA: func(ctx context.Context) (string, error) {
			return loadPullRequestHead(ctx, gh, repoRoot, prNumber)
		},
	}, cfg)
}

// waitForPostMergePushCI waits for the push run of the merge commit on main,
// under the same stall window, hard cap and rerun rule as waitForPRCI.
func waitForPostMergePushCI(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	mergeCommit string,
	cfg ciWaitConfig,
) error {
	if mergeCommit == "" {
		return fmt.Errorf("dod post-merge: missing merge commit")
	}

	gh := newGHClient(runner, cfg.sleep)

	return runCIWait(ctx, gh, mainRoot, ciWaitTarget{
		label:      postMergeCILabel,
		failedVerb: "is not clean",
		poll: func(ctx context.Context) (ciSnapshot, error) {
			runs, err := loadMainRunsForCommit(ctx, gh, mainRoot, mergeCommit)
			if err != nil {
				return ciSnapshot{}, err
			}

			return classifyPostMergeRuns(runs, mergeCommit), nil
		},
		headSHA: func(context.Context) (string, error) {
			return mergeCommit, nil
		},
	}, cfg)
}

func runCIWait(ctx context.Context, gh ghClient, repoRoot string, target ciWaitTarget, cfg ciWaitConfig) error {
	start := cfg.now()
	wait := &ciWait{
		gh:             gh,
		repoRoot:       repoRoot,
		target:         target,
		cfg:            cfg,
		start:          start,
		lastChange:     start,
		rerunUpdatedAt: map[int64]string{},
	}

	return wait.run(ctx)
}

func (w *ciWait) run(ctx context.Context) error {
	for {
		snapshot, err := w.target.poll(ctx)
		if err != nil {
			return err
		}
		if snapshot.fingerprint != w.lastFingerprint {
			w.lastFingerprint = snapshot.fingerprint
			w.lastChange = w.cfg.now()
		}

		step, err := w.decide(ctx, snapshot)
		if err != nil {
			return err
		}
		if step.done {
			return nil
		}
		if err := w.checkLimits(step, w.cfg.stallWindowFor(snapshot.notStarted)); err != nil {
			return err
		}
		if err := w.cfg.sleep(ctx, w.cfg.pollInterval); err != nil {
			return fmt.Errorf("%s wait: %w", w.target.label, err)
		}
	}
}

func (w *ciWait) decide(ctx context.Context, snapshot ciSnapshot) (ciStep, error) {
	switch snapshot.state {
	case prCheckStatePass:
		return ciStep{done: true}, nil
	case prCheckStatePending:
		return ciStep{reason: snapshot.reason, pending: snapshot.pending}, nil
	case prCheckStateFail:
		return w.examineFailure(ctx, snapshot)
	}

	return ciStep{}, fmt.Errorf("%s: unknown CI state %q", w.target.label, snapshot.state)
}

// examineFailure decides about a failed check. A failure of a run whose jobs
// failed for real ends the wait. One whose only trouble is canceled jobs is
// rerun when the run has finished and belongs to the head commit, and waited
// for while either is not yet true.
func (w *ciWait) examineFailure(ctx context.Context, snapshot ciSnapshot) (ciStep, error) {
	failed := w.target.label + " " + w.target.failedVerb + ": " + snapshot.reason
	if snapshot.failedRunID == 0 {
		return ciStep{}, errors.New(failed)
	}

	runID := snapshot.failedRunID
	run, err := loadCIRun(ctx, w.gh, w.repoRoot, runID)
	if err != nil {
		return ciStep{}, err
	}
	if requestedAt, ok := w.rerunUpdatedAt[runID]; ok && requestedAt == run.UpdatedAt {
		return waitingOnFailure(snapshot, fmt.Sprintf("the rerun of run %d has not started", runID)), nil
	}

	jobs := classifyCIRunJobs(run.Jobs)
	if len(jobs.failed) > 0 {
		return ciStep{}, fmt.Errorf("%s: job %s failed (%s)", failed, strings.Join(jobs.failed, ", "), run.URL)
	}
	if !strings.EqualFold(run.Status, checkStatusCompleted) {
		// gh reruns only a finished run, and the jobs still going may yet
		// fail for real.
		return waitingOnFailure(snapshot, fmt.Sprintf("run %d is %s", runID, run.Status)), nil
	}
	if len(jobs.canceled) == 0 {
		return ciStep{}, fmt.Errorf("%s: run %d concluded %s with no failed or canceled job (%s)",
			failed, runID, run.Conclusion, run.URL)
	}

	head, err := w.target.headSHA(ctx)
	if err != nil {
		return ciStep{}, err
	}
	if run.HeadSHA != head {
		// A run of a commit the head replaced: nobody will merge it, so it is
		// not worth live-account time. The head commit's own run decides.
		return waitingOnFailure(snapshot, fmt.Sprintf("run %d is of superseded commit %s", runID, shortSHA(run.HeadSHA))), nil
	}
	if w.rerunsUsed >= w.cfg.maxReruns {
		return ciStep{}, fmt.Errorf("%s: %s canceled; reruns used: %d of %d (--ci-max-reruns); %s",
			failed, strings.Join(jobs.canceled, ", "), w.rerunsUsed, w.cfg.maxReruns, run.URL)
	}
	if err := w.rerun(ctx, runID, &run, jobs.canceled); err != nil {
		return ciStep{}, err
	}

	return waitingOnFailure(snapshot, fmt.Sprintf("run %d was rerun", runID)), nil
}

// rerun asks GitHub to rerun the failed and canceled jobs of a finished run.
// A rerun is progress: the stall window starts again for the new attempt. A
// request GitHub failed transiently is repeated: GitHub refuses to rerun a run
// that is running, so a repeat cannot start a second attempt, and that
// refusal after a 5xx means GitHub started the rerun anyway, which the run is
// read again to confirm.
func (w *ciWait) rerun(ctx context.Context, runID int64, run *githubRun, canceled []string) error {
	w.rerunsUsed++
	w.rerunUpdatedAt[runID] = run.UpdatedAt
	w.lastChange = w.cfg.now()

	id := strconv.FormatInt(runID, 10)
	call := fmt.Sprintf("%s rerun run %s", w.target.label, id)
	startedEarlier, err := w.gh.outputUnlessDone(ctx, w.repoRoot, call, isRerunAlreadyRunning, "run", "rerun", id, "--failed")
	if err != nil {
		return err
	}
	if startedEarlier {
		current, loadErr := loadCIRun(ctx, w.gh, w.repoRoot, runID)
		if loadErr != nil {
			return loadErr
		}
		if strings.EqualFold(current.Status, checkStatusCompleted) {
			return fmt.Errorf("%s: GitHub refused the repeat of a failed request as already running, but run %d reads %s",
				call, runID, strings.ToLower(current.Status))
		}
	}

	return writeStatus(w.cfg.stdout, fmt.Sprintf("%s: %s canceled in run %d (%s); rerun %d of %d requested\n",
		w.target.label, strings.Join(canceled, ", "), runID, run.URL, w.rerunsUsed, w.cfg.maxReruns))
}

// checkLimits ends a wait that has stalled for window or reached its hard
// cap.
func (w *ciWait) checkLimits(step ciStep, window time.Duration) error {
	now := w.cfg.now()
	if now.Sub(w.lastChange) >= window {
		return fmt.Errorf("%s stalled: no check changed state for %s; %s", w.target.label, window, step.describe())
	}
	if now.Sub(w.start) >= w.cfg.hardCap {
		return fmt.Errorf("%s timed out after %s: %s", w.target.label, w.cfg.hardCap, step.reason)
	}

	return nil
}

// isRerunAlreadyRunning recognizes GitHub's refusal to rerun a run that is
// running: "This workflow is already running".
func isRerunAlreadyRunning(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "already running")
}

// waitingOnFailure keeps waiting for a failed check whose run may yet clear it,
// alongside the checks still pending.
func waitingOnFailure(snapshot ciSnapshot, why string) ciStep {
	pending := slices.Clone(snapshot.pending)
	pending = append(pending, snapshot.failedCheck)
	sort.Strings(pending)

	return ciStep{reason: snapshot.reason + "; " + why, pending: pending}
}

func classifyCIRunJobs(jobs []githubJob) ciRunJobs {
	var sorted ciRunJobs
	for _, job := range jobs {
		switch strings.ToLower(job.Conclusion) {
		case "", checkConclusionSuccess, checkConclusionSkipped:
			// Still running, or nothing to do about it.
		case checkConclusionCancelled:
			sorted.canceled = append(sorted.canceled, job.Name)
		default:
			// The aggregate verify job fails whenever a leg did not succeed,
			// so its own failure says nothing the legs do not.
			if job.Name != nightlyAggregateJob {
				sorted.failed = append(sorted.failed, job.Name)
			}
		}
	}

	return sorted
}

// classifyPRChecks reads a pull request's statusCheckRollup. The required
// checks must all succeed; a failure outranks checks still pending, because no
// amount of waiting clears it.
func classifyPRChecks(checks []prCheckRun) ciSnapshot {
	snapshot := ciSnapshot{fingerprint: prChecksFingerprint(checks)}
	reported := map[string]bool{}
	firstPending := ""
	for _, check := range latestPRCheckRuns(checks) {
		name := strings.ToLower(check.Name)
		required := isRequiredPRCheck(name)
		if required {
			reported[name] = true
		}

		status := strings.ToLower(check.Status)
		if status != checkStatusCompleted {
			if required {
				snapshot.pending = append(snapshot.pending, name)
				if status != checkStatusInProgress {
					snapshot.notStarted = append(snapshot.notStarted, name)
				}
				if firstPending == "" {
					firstPending = check.Name + " is " + status
				}
			}
			continue
		}

		conclusion := strings.ToLower(check.Conclusion)
		if snapshot.failedCheck == "" && prCheckFailed(name, required, conclusion) {
			snapshot.failedCheck = check.Name
			snapshot.reason = check.Name + " concluded " + conclusion
			snapshot.failedRunID = rerunCandidateRunID(conclusion, check.DetailsURL)
		}
	}

	missing := missingRequiredPRChecks(reported)
	snapshot.pending = append(snapshot.pending, missing...)
	sort.Strings(snapshot.pending)
	snapshot.notStarted = append(snapshot.notStarted, missing...)
	sort.Strings(snapshot.notStarted)

	switch {
	case snapshot.failedCheck != "":
		snapshot.state = prCheckStateFail
	case firstPending != "":
		snapshot.state, snapshot.reason = prCheckStatePending, firstPending
	case len(missing) > 0:
		snapshot.state, snapshot.reason = prCheckStatePending, "missing required checks: "+strings.Join(missing, ", ")
	default:
		snapshot.state, snapshot.reason = prCheckStatePass, "required PR checks passed"
	}

	return snapshot
}

func isRequiredPRCheck(name string) bool {
	return slices.Contains(requiredPRChecks(), name)
}

// requiredPRChecks lists the checks branch protection requires, sorted.
func requiredPRChecks() []string {
	return []string{prCheckE2E, prCheckIntegration, prCheckVerify}
}

func missingRequiredPRChecks(reported map[string]bool) []string {
	missing := []string{}
	for _, name := range requiredPRChecks() {
		if !reported[name] {
			missing = append(missing, name)
		}
	}

	return missing
}

func prCheckFailed(name string, required bool, conclusion string) bool {
	switch {
	case required:
		return conclusion != checkConclusionSuccess
	case name == prCheckStress:
		return conclusion != checkConclusionSuccess && conclusion != checkConclusionSkipped
	default:
		return false
	}
}

// rerunCandidateRunID returns the run to examine for canceled jobs: a check
// that concluded failure (the aggregate verify fails when a leg was canceled)
// or canceled. Any other conclusion, skipped or timed_out among them, is a
// decision or a real failure that a rerun must not paper over.
func rerunCandidateRunID(conclusion, detailsURL string) int64 {
	if conclusion != checkConclusionFailure && conclusion != checkConclusionCancelled {
		return 0
	}

	return ciRunIDFromDetailsURL(detailsURL)
}

// latestPRCheckRuns keeps the newest run of every check, in the order the
// checks first appear. A rerun lists a second run of a check beside the
// first, and only the newer one says where the check stands.
func latestPRCheckRuns(checks []prCheckRun) []prCheckRun {
	latest := make([]prCheckRun, 0, len(checks))
	index := make(map[string]int, len(checks))
	for _, check := range checks {
		key := check.WorkflowName + "\x00" + strings.ToLower(check.Name)
		i, seen := index[key]
		if !seen {
			index[key] = len(latest)
			latest = append(latest, check)
			continue
		}
		if checkStartedAt(check).After(checkStartedAt(latest[i])) {
			latest[i] = check
		}
	}

	return latest
}

// checkStartedAt parses a check's start time. A check that has not started
// carries the zero time or no time at all, and an unreadable time counts as
// not started: either way any started run of the same check is newer.
func checkStartedAt(check prCheckRun) time.Time {
	started, err := time.Parse(time.RFC3339, check.StartedAt)
	if err != nil {
		return time.Time{}
	}

	return started
}

func prChecksFingerprint(checks []prCheckRun) string {
	lines := make([]string, 0, len(checks))
	for _, check := range checks {
		lines = append(lines, strings.Join([]string{
			check.WorkflowName, check.Name, check.Status, check.Conclusion, check.StartedAt,
		}, "|"))
	}
	sort.Strings(lines)

	return strings.Join(lines, "\n")
}

// classifyPostMergeRuns reads the CI run of the merge commit on main. Every
// push to main runs the quick lane (branch protection no longer requires a PR
// branch to be up to date, so a stale-branch merge can break main and the
// push run is what catches it), so a skipped run is a failure, not the old
// squash-merge skip rule.
func classifyPostMergeRuns(runs []githubRun, commit string) ciSnapshot {
	snapshot := ciSnapshot{fingerprint: postMergeRunsFingerprint(runs)}
	if len(runs) == 0 {
		snapshot.state, snapshot.reason = prCheckStatePending, "no main push CI run found for "+commit
		return snapshot
	}
	for i := range runs {
		run := runs[i]
		if run.HeadSHA != commit {
			continue
		}
		status := strings.ToLower(run.Status)
		conclusion := strings.ToLower(run.Conclusion)
		if status != checkStatusCompleted {
			snapshot.state, snapshot.reason = prCheckStatePending, run.Name+" is "+status
			return snapshot
		}
		switch conclusion {
		case checkConclusionSuccess:
			snapshot.state, snapshot.reason = prCheckStatePass, run.Name+" succeeded"
		case checkConclusionSkipped:
			snapshot.state, snapshot.reason = prCheckStateFail, run.Name+" skipped; every push to main must run the quick lane"
			snapshot.failedCheck = run.Name
		default:
			snapshot.state, snapshot.reason = prCheckStateFail, run.Name+" concluded "+conclusion
			snapshot.failedCheck = run.Name
			if conclusion == checkConclusionFailure || conclusion == checkConclusionCancelled {
				snapshot.failedRunID = run.DatabaseID
			}
		}
		return snapshot
	}

	snapshot.state, snapshot.reason = prCheckStatePending, "no run matched merge commit "+commit
	return snapshot
}

func postMergeRunsFingerprint(runs []githubRun) string {
	lines := make([]string, 0, len(runs))
	for i := range runs {
		run := runs[i]
		lines = append(lines, strings.Join([]string{
			strconv.FormatInt(run.DatabaseID, 10), run.Name, run.Status, run.Conclusion, run.UpdatedAt,
		}, "|"))
	}
	sort.Strings(lines)

	return strings.Join(lines, "\n")
}

// ciRunIDFromDetailsURL returns the Actions run a check belongs to, or zero
// for a check that is not an Actions job.
func ciRunIDFromDetailsURL(detailsURL string) int64 {
	m := actionsRunIDPattern.FindStringSubmatch(detailsURL)
	if m == nil {
		return 0
	}
	runID, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		// Digits that overflow an int64 name no run GitHub has.
		return 0
	}

	return runID
}

func loadPullRequestHead(ctx context.Context, gh ghClient, repoRoot string, prNumber int) (string, error) {
	out, err := gh.output(
		ctx, repoRoot, fmt.Sprintf("dod gh pr view %d head", prNumber), "pr", "view", strconv.Itoa(prNumber), "--json", "headRefOid",
	)
	if err != nil {
		return "", err
	}
	var head struct {
		HeadRefOID string
	}
	if err := json.Unmarshal(out, &head); err != nil {
		return "", fmt.Errorf("decode gh pr view %d head: %w", prNumber, err)
	}
	if head.HeadRefOID == "" {
		return "", fmt.Errorf("dod gh pr view %d: no head commit", prNumber)
	}

	return head.HeadRefOID, nil
}

func loadCIRun(ctx context.Context, gh ghClient, repoRoot string, runID int64) (githubRun, error) {
	id := strconv.FormatInt(runID, 10)
	out, err := gh.output(ctx, repoRoot, "dod gh run view "+id, "run", "view", id, "--json", ciRunViewFields)
	if err != nil {
		return githubRun{}, err
	}
	var run githubRun
	if err := json.Unmarshal(out, &run); err != nil {
		return githubRun{}, fmt.Errorf("decode gh run view %s: %w", id, err)
	}

	return run, nil
}

func loadMainRunsForCommit(ctx context.Context, gh ghClient, repoRoot string, commit string) ([]githubRun, error) {
	out, err := gh.output(
		ctx,
		repoRoot,
		"dod post-merge run list",
		"run",
		"list",
		"--branch",
		mainBranchName,
		"--commit",
		commit,
		"--json",
		"databaseId,name,event,status,conclusion,headSha,createdAt,updatedAt,url",
		"--limit",
		postMergeRunListLimit,
	)
	if err != nil {
		return nil, err
	}
	var runs []githubRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("decode post-merge run list: %w", err)
	}

	return runs, nil
}
