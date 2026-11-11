package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	// ciStallMargin is what the stall window adds to the longest job timeout
	// in the workflow, so a job that runs into its own timeout still changes
	// state inside the window.
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

var (
	// workflowTimeoutPattern matches a `timeout-minutes: N` line of the CI
	// workflow.
	workflowTimeoutPattern = regexp.MustCompile(`(?m)^[ \t]*timeout-minutes:[ \t]*(\d+)[ \t]*(?:#.*)?$`)

	// actionsRunIDPattern takes the run id out of an Actions check's details
	// URL: `.../actions/runs/<run id>/job/<job id>`.
	actionsRunIDPattern = regexp.MustCompile(`/actions/runs/(\d+)(?:/|$)`)
)

// ciWaitConfig is the policy and the clock of one DoD CI wait.
type ciWaitConfig struct {
	// stallWindow ends the wait when no check has changed state for this long.
	stallWindow time.Duration
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
// workflow. With no `--ci-timeout` the cap is one stall window for the first
// attempt and one for every rerun the wait may issue.
func newCIWaitConfig(opts *VerifyOptions, repoRoot string, stdout io.Writer) (ciWaitConfig, error) {
	if opts.DODCIMaxReruns < 0 {
		return ciWaitConfig{}, fmt.Errorf("dod CI wait: --ci-max-reruns must not be negative, got %d", opts.DODCIMaxReruns)
	}

	stallWindow, err := ciStallWindow(repoRoot)
	if err != nil {
		return ciWaitConfig{}, err
	}

	hardCap := opts.DODCITimeout
	if hardCap <= 0 {
		hardCap = time.Duration(opts.DODCIMaxReruns+1) * stallWindow
	}

	return ciWaitConfig{
		stallWindow:  stallWindow,
		hardCap:      hardCap,
		maxReruns:    opts.DODCIMaxReruns,
		pollInterval: prCIPollInterval,
		now:          time.Now,
		sleep:        sleepContext,
		stdout:       stdout,
	}, nil
}

// ciStallWindow is the longest `timeout-minutes` in the CI workflow plus
// ciStallMargin. No check can legitimately stay in one state longer than the
// longest job ahead of it runs, and reading the number from the workflow means
// lengthening a job there cannot leave the wait giving up on it early.
func ciStallWindow(repoRoot string) (time.Duration, error) {
	data, err := readFile(filepath.Join(repoRoot, ciWorkflowPath))
	if err != nil {
		return 0, fmt.Errorf("derive CI stall window: %w", err)
	}

	longest := 0
	for _, m := range workflowTimeoutPattern.FindAllSubmatch(data, -1) {
		minutes, convErr := strconv.Atoi(string(m[1]))
		if convErr != nil {
			return 0, fmt.Errorf("derive CI stall window: parse %s timeout-minutes %q: %w", ciWorkflowPath, m[1], convErr)
		}
		longest = max(longest, minutes)
	}
	if longest == 0 {
		return 0, fmt.Errorf("derive CI stall window: %s sets no job timeout-minutes", ciWorkflowPath)
	}

	return time.Duration(longest)*time.Minute + ciStallMargin, nil
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
	// failedCheck names the failure; failedRunID is the run to examine for
	// canceled jobs, zero when no rerun could clear the failure.
	failedCheck string
	failedRunID int64
	// fingerprint changes exactly when some check changes state.
	fingerprint string
}

// ciRun is `gh run view --json` of one workflow run. Its jobs are those of the
// run's latest attempt.
type ciRun struct {
	Conclusion string
	HeadSHA    string
	Jobs       []ciRunJob
	Status     string
	UpdatedAt  string
	URL        string
}

type ciRunJob struct {
	Name       string
	Conclusion string
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
	runner   commandRunner
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
	return runCIWait(ctx, runner, repoRoot, ciWaitTarget{
		label:      prCILabel,
		failedVerb: "failed",
		poll: func(ctx context.Context) (ciSnapshot, error) {
			state, err := loadPullRequestState(ctx, runner, repoRoot, prNumber)
			if err != nil {
				return ciSnapshot{}, err
			}

			return classifyPRChecks(state.StatusCheckRollup), nil
		},
		headSHA: func(ctx context.Context) (string, error) {
			return loadPullRequestHead(ctx, runner, repoRoot, prNumber)
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

	return runCIWait(ctx, runner, mainRoot, ciWaitTarget{
		label:      postMergeCILabel,
		failedVerb: "is not clean",
		poll: func(ctx context.Context) (ciSnapshot, error) {
			runs, err := loadMainRunsForCommit(ctx, runner, mainRoot, mergeCommit)
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

func runCIWait(ctx context.Context, runner commandRunner, repoRoot string, target ciWaitTarget, cfg ciWaitConfig) error {
	start := cfg.now()
	wait := &ciWait{
		runner:         runner,
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
		if err := w.checkLimits(step); err != nil {
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
	run, err := loadCIRun(ctx, w.runner, w.repoRoot, runID)
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
	if err := w.rerun(ctx, runID, run, jobs.canceled); err != nil {
		return ciStep{}, err
	}

	return waitingOnFailure(snapshot, fmt.Sprintf("run %d was rerun", runID)), nil
}

// rerun asks GitHub to rerun the failed and canceled jobs of a finished run.
// A rerun is progress: the stall window starts again for the new attempt.
func (w *ciWait) rerun(ctx context.Context, runID int64, run ciRun, canceled []string) error {
	w.rerunsUsed++
	w.rerunUpdatedAt[runID] = run.UpdatedAt
	w.lastChange = w.cfg.now()

	if err := w.runner.Run(
		ctx, w.repoRoot, os.Environ(), w.cfg.stdout, w.cfg.stdout,
		githubCLI, "run", "rerun", strconv.FormatInt(runID, 10), "--failed",
	); err != nil {
		return fmt.Errorf("%s rerun run %d: %w", w.target.label, runID, err)
	}

	return writeStatus(w.cfg.stdout, fmt.Sprintf("%s: %s canceled in run %d (%s); rerun %d of %d requested\n",
		w.target.label, strings.Join(canceled, ", "), runID, run.URL, w.rerunsUsed, w.cfg.maxReruns))
}

// checkLimits ends a wait that has stalled or reached its hard cap.
func (w *ciWait) checkLimits(step ciStep) error {
	now := w.cfg.now()
	if now.Sub(w.lastChange) >= w.cfg.stallWindow {
		return fmt.Errorf("%s stalled: no check changed state for %s; %s", w.target.label, w.cfg.stallWindow, step.describe())
	}
	if now.Sub(w.start) >= w.cfg.hardCap {
		return fmt.Errorf("%s timed out after %s: %s", w.target.label, w.cfg.hardCap, step.reason)
	}

	return nil
}

// waitingOnFailure keeps waiting for a failed check whose run may yet clear it,
// alongside the checks still pending.
func waitingOnFailure(snapshot ciSnapshot, why string) ciStep {
	pending := slices.Clone(snapshot.pending)
	pending = append(pending, snapshot.failedCheck)
	sort.Strings(pending)

	return ciStep{reason: snapshot.reason + "; " + why, pending: pending}
}

func classifyCIRunJobs(jobs []ciRunJob) ciRunJobs {
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

func loadPullRequestHead(ctx context.Context, runner commandRunner, repoRoot string, prNumber int) (string, error) {
	out, err := runner.Output(ctx, repoRoot, os.Environ(), githubCLI, "pr", "view", strconv.Itoa(prNumber), "--json", "headRefOid")
	if err != nil {
		return "", fmt.Errorf("dod gh pr view %d head: %w", prNumber, err)
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

func loadCIRun(ctx context.Context, runner commandRunner, repoRoot string, runID int64) (ciRun, error) {
	id := strconv.FormatInt(runID, 10)
	out, err := runner.Output(ctx, repoRoot, os.Environ(), githubCLI, "run", "view", id, "--json", ciRunViewFields)
	if err != nil {
		return ciRun{}, fmt.Errorf("dod gh run view %s: %w", id, err)
	}
	var run ciRun
	if err := json.Unmarshal(out, &run); err != nil {
		return ciRun{}, fmt.Errorf("decode gh run view %s: %w", id, err)
	}

	return run, nil
}

func loadMainRunsForCommit(ctx context.Context, runner commandRunner, repoRoot string, commit string) ([]githubRun, error) {
	out, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		githubCLI,
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
		return nil, fmt.Errorf("dod post-merge run list: %w", err)
	}
	var runs []githubRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("decode post-merge run list: %w", err)
	}

	return runs, nil
}
