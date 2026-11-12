package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ciWaitPR       = 7
	ciWaitRunID    = int64(111)
	ciWaitPushRun  = int64(222)
	ciWaitHeadSHA  = "head-sha"
	ciWaitMergeSHA = "merge-sha"
	ciWaitShard    = "verify-unit (macos-latest, 2/3)"

	ciWaitPRViewCommand      = "gh pr view 7 --json state,mergedAt,mergeCommit,statusCheckRollup,url"
	ciWaitPRHeadCommand      = "gh pr view 7 --json headRefOid"
	ciWaitRunViewCommand     = "gh run view 111 --json conclusion,headSha,jobs,status,updatedAt,url"
	ciWaitRerunCommand       = "gh run rerun 111 --failed"
	ciWaitPushRunViewCommand = "gh run view 222 --json conclusion,headSha,jobs,status,updatedAt,url"
	ciWaitPushRerunCommand   = "gh run rerun 222 --failed"
	ciWaitRunListCommand     = "gh run list --branch main --commit merge-sha " +
		"--json databaseId,name,event,status,conclusion,headSha,createdAt,updatedAt,url --limit 10"

	ciWaitFirstStart  = "2026-11-11T10:00:00Z"
	ciWaitSecondStart = "2026-11-11T11:00:00Z"
	ciWaitNotStarted  = "0001-01-01T00:00:00Z"

	// gh prints a pull request's check states in upper case and a run's own
	// states in lower case; the fixtures keep that difference.
	rollupCompleted  = "COMPLETED"
	rollupInProgress = "IN_PROGRESS"
	rollupCancelled  = "CANCELLED" //nolint:misspell // GitHub's spelling of the conclusion
	rollupQueued     = "QUEUED"
	runInProgress    = "in_progress"

	ciWaitTestStall = 30 * time.Minute
	ciWaitTestPoll  = 10 * time.Minute
	ciWaitTestCap   = 5 * time.Hour
)

// fakeCIClock is the clock of a CI wait under test: sleeping advances it and
// returns at once, so a wait of hours runs in microseconds.
type fakeCIClock struct {
	start   time.Time
	current time.Time
}

func newFakeCIClock() *fakeCIClock {
	start := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)

	return &fakeCIClock{start: start, current: start}
}

func (c *fakeCIClock) now() time.Time {
	return c.current
}

func (c *fakeCIClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake sleep: %w", err)
	}
	c.current = c.current.Add(d)

	return nil
}

func (c *fakeCIClock) elapsed() time.Duration {
	return c.current.Sub(c.start)
}

func testCIWaitConfig(clock *fakeCIClock, stdout io.Writer) ciWaitConfig {
	return ciWaitConfig{
		stallWindow:  ciWaitTestStall,
		hardCap:      ciWaitTestCap,
		maxReruns:    DefaultDODCIMaxReruns,
		pollInterval: ciWaitTestPoll,
		now:          clock.now,
		sleep:        clock.sleep,
		stdout:       stdout,
	}
}

// ghScript answers gh commands from a list of outputs per command. Each call
// takes the next output and the last one repeats. A command the script does
// not name fails, so a passing wait also proves what it did not ask.
type ghScript struct {
	outputs map[string][]string
	calls   map[string]int
}

func newGHScript(outputs map[string][]string) *ghScript {
	return &ghScript{outputs: outputs, calls: map[string]int{}}
}

func (s *ghScript) output(_ string, _ []string, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	steps := s.outputs[key]
	if len(steps) == 0 && strings.HasPrefix(key, ghRerunPrefix) {
		// GitHub accepts every rerun request; the test reads which were sent.
		return nil, nil
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("unscripted command %q", key)
	}
	index := min(s.calls[key], len(steps)-1)
	s.calls[key]++
	if stderr, failed := strings.CutPrefix(steps[index], ghScriptFailure); failed {
		return nil, ghFailure(stderr)
	}

	return []byte(steps[index]), nil
}

// ghScriptFailure starts a scripted answer that is a failure of the command,
// with the rest as gh's stderr.
const ghScriptFailure = "FAIL: "

func scriptedRunner(outputs map[string][]string) (*fakeRunner, *ghScript) {
	script := newGHScript(outputs)

	return &fakeRunner{outputFunc: script.output}, script
}

// ghRerunPrefix starts every rerun request the wait sends.
const ghRerunPrefix = "gh run rerun "

// recordedReruns lists the rerun requests the wait sent, in order.
func recordedReruns(runner *fakeRunner) []recordedCommand {
	reruns := []recordedCommand{}
	for _, command := range runner.outputCommands {
		if strings.HasPrefix(command.name+" "+strings.Join(command.args, " "), ghRerunPrefix) {
			reruns = append(reruns, command)
		}
	}

	return reruns
}

// recordedRerunKeys lists the command lines of the rerun requests the wait
// sent.
func recordedRerunKeys(runner *fakeRunner) []string {
	keys := []string{}
	for _, command := range recordedReruns(runner) {
		keys = append(keys, command.name+" "+strings.Join(command.args, " "))
	}

	return keys
}

// rollupCheck is one statusCheckRollup entry of `gh pr view`.
type rollupCheck struct {
	name       string
	status     string
	conclusion string
	startedAt  string
	runID      int64
}

func checkPassed(name string) rollupCheck {
	return rollupCheck{name: name, status: rollupCompleted, conclusion: "SUCCESS", startedAt: ciWaitFirstStart, runID: ciWaitRunID}
}

func verifyCheckFailed() rollupCheck {
	return rollupCheck{name: prCheckVerify, status: rollupCompleted, conclusion: "FAILURE", startedAt: ciWaitFirstStart, runID: ciWaitRunID}
}

func checkCancelled(name string) rollupCheck {
	return rollupCheck{name: name, status: rollupCompleted, conclusion: rollupCancelled, startedAt: ciWaitFirstStart, runID: ciWaitRunID}
}

func checkRunning(name string) rollupCheck {
	return rollupCheck{name: name, status: rollupInProgress, startedAt: ciWaitFirstStart, runID: ciWaitRunID}
}

func checkQueued(name string) rollupCheck {
	return rollupCheck{name: name, status: rollupQueued, startedAt: ciWaitNotStarted, runID: ciWaitRunID}
}

func (c rollupCheck) startedLater() rollupCheck {
	c.startedAt = ciWaitSecondStart

	return c
}

func (c rollupCheck) inRun(runID int64) rollupCheck {
	c.runID = runID

	return c
}

func (c rollupCheck) checkRun() prCheckRun {
	return prCheckRun{
		Name:         c.name,
		Status:       c.status,
		Conclusion:   c.conclusion,
		DetailsURL:   fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/5", c.runID),
		StartedAt:    c.startedAt,
		WorkflowName: "CI",
	}
}

func checkRuns(checks ...rollupCheck) []prCheckRun {
	runs := make([]prCheckRun, 0, len(checks))
	for _, check := range checks {
		runs = append(runs, check.checkRun())
	}

	return runs
}

// prViewJSON renders checks with the keys gh itself prints, so the tests also
// prove that the production structs decode gh's camelCase output.
func prViewJSON(t *testing.T, checks ...rollupCheck) string {
	t.Helper()

	entries := make([]map[string]string, 0, len(checks))
	for _, check := range checks {
		run := check.checkRun()
		entries = append(entries, map[string]string{
			"__typename":   "CheckRun",
			"name":         run.Name,
			"status":       run.Status,
			"conclusion":   run.Conclusion,
			"detailsUrl":   run.DetailsURL,
			"startedAt":    run.StartedAt,
			"workflowName": run.WorkflowName,
		})
	}
	data, err := json.Marshal(map[string]any{"state": "OPEN", "statusCheckRollup": entries})
	require.NoError(t, err)

	return string(data)
}

func prHeadJSON() string {
	return fmt.Sprintf(`{"headRefOid":%q}`, ciWaitHeadSHA)
}

type jobFixture struct {
	name       string
	status     string
	conclusion string
}

func jobDone(name, conclusion string) jobFixture {
	return jobFixture{name: name, status: checkStatusCompleted, conclusion: conclusion}
}

// canceledShardJobs is a run in which the runner canceled one macOS shard:
// the aggregate verify job then fails, and nothing else is wrong.
func canceledShardJobs() []jobFixture {
	return []jobFixture{
		jobDone("verify-lint", checkConclusionSuccess),
		jobDone("verify-unit (ubuntu-latest, 1/3)", checkConclusionSuccess),
		jobDone(ciWaitShard, checkConclusionCancelled),
		jobDone("verify-full (ubuntu-latest)", checkConclusionSkipped),
		jobDone(nightlyAggregateJob, checkConclusionFailure),
		jobDone("integration", checkConclusionSuccess),
		jobDone("e2e", checkConclusionSuccess),
	}
}

// runFixture is one `gh run view` answer.
type runFixture struct {
	id        int64
	headSHA   string
	status    string
	updatedAt string
	jobs      []jobFixture
}

func runViewJSON(t *testing.T, run runFixture) string {
	t.Helper()

	entries := make([]map[string]string, 0, len(run.jobs))
	for _, job := range run.jobs {
		entries = append(entries, map[string]string{"name": job.name, "status": job.status, "conclusion": job.conclusion})
	}
	conclusion := ""
	if run.status == checkStatusCompleted {
		conclusion = checkConclusionFailure
	}
	data, err := json.Marshal(map[string]any{
		"conclusion": conclusion,
		"headSha":    run.headSHA,
		"status":     run.status,
		"updatedAt":  run.updatedAt,
		"url":        fmt.Sprintf("https://github.com/o/r/actions/runs/%d", run.id),
		"jobs":       entries,
	})
	require.NoError(t, err)

	return string(data)
}

func canceledShardRun(t *testing.T, headSHA, updatedAt string) string {
	t.Helper()

	return runViewJSON(t, runFixture{
		id:        ciWaitRunID,
		headSHA:   headSHA,
		status:    checkStatusCompleted,
		updatedAt: updatedAt,
		jobs:      canceledShardJobs(),
	})
}

// canceledShardRollup is what the pull request shows for canceledShardJobs.
func canceledShardRollup(t *testing.T) string {
	t.Helper()

	return prViewJSON(t,
		checkCancelled(ciWaitShard),
		verifyCheckFailed(),
		checkPassed("integration"),
		checkPassed("e2e"),
	)
}

func pushRunListJSON(t *testing.T, status, conclusion, updatedAt string) string {
	t.Helper()

	data, err := json.Marshal([]map[string]any{{
		"databaseId": ciWaitPushRun,
		"name":       "CI",
		"event":      "push",
		"status":     status,
		"conclusion": conclusion,
		"headSha":    ciWaitMergeSHA,
		"createdAt":  ciWaitFirstStart,
		"updatedAt":  updatedAt,
		"url":        "https://github.com/o/r/actions/runs/222",
	}})
	require.NoError(t, err)

	return string(data)
}

// Validates: R-6.10.16
//
// A fixed limit ends a healthy wait that is merely long: the live jobs queue
// behind one another. What separates a healthy wait from a dead one is whether
// anything still changes, so every change starts the stall window again.
func TestWaitForPRCI_ProgressExtendsTheWait(t *testing.T) {
	t.Parallel()

	allQueued := prViewJSON(t, checkQueued("verify"), checkQueued("integration"), checkQueued("e2e"))
	progressed := prViewJSON(t, checkPassed("verify"), checkRunning("integration"), checkQueued("e2e"))
	allPassed := prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkPassed("e2e"))
	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {allQueued, allQueued, progressed, progressed, allPassed},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.NoError(t, err)

	assert.Equal(t, 4*ciWaitTestPoll, clock.elapsed(), "the wait outlived one stall window because a check changed state")
	assert.Greater(t, clock.elapsed(), ciWaitTestStall)
	assert.Empty(t, recordedReruns(runner), "nothing was canceled, so nothing is rerun")
}

// Validates: R-6.10.16
func TestWaitForPRCI_StallEndsTheWaitNamingThePendingChecks(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {prViewJSON(t, checkPassed("verify"), checkQueued("integration"), checkRunning("e2e"))},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod pre-merge PR CI stalled")
	assert.Contains(t, err.Error(), "no check changed state for 30m0s")
	assert.Contains(t, err.Error(), "still pending: e2e, integration")
	assert.Equal(t, ciWaitTestStall, clock.elapsed(), "the wait ends as soon as the stall window has passed")
}

// Validates: R-6.10.16
//
// A required check that never reports is pending too, and the stall message
// has to name it: it is the one the reader must go and look for.
func TestWaitForPRCI_StallNamesARequiredCheckThatNeverReported(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {prViewJSON(t, checkPassed("verify"), checkPassed("e2e"))},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "still pending: integration")
}

// Validates: R-6.10.16
//
// The runner sometimes cancels a macOS shard. That fails the aggregate verify
// check although no test failed, and the remedy is always the same rerun, so
// the wait does it.
func TestWaitForPRCI_CancelledShardIsRerunOnceAndTheWaitThenSucceeds(t *testing.T) {
	t.Parallel()

	canceled := canceledShardRollup(t)
	rerunStarted := prViewJSON(t,
		checkCancelled(ciWaitShard),
		checkRunning(ciWaitShard).startedLater(),
		verifyCheckFailed(),
		checkPassed("integration"),
		checkPassed("e2e"),
	)
	rerunPassed := prViewJSON(t,
		checkCancelled(ciWaitShard),
		checkPassed(ciWaitShard).startedLater(),
		verifyCheckFailed(),
		checkPassed("verify").startedLater(),
		checkPassed("integration"),
		checkPassed("e2e"),
	)
	runner, script := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {canceled, canceled, rerunStarted, rerunPassed},
		ciWaitPRHeadCommand: {prHeadJSON()},
		ciWaitRunViewCommand: {
			canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:20:00Z"),
			// The rerun was requested but GitHub has not started it yet: the
			// run still reads as it did, and must not be rerun a second time.
			canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:20:00Z"),
			runViewJSON(t, runFixture{
				id: ciWaitRunID, headSHA: ciWaitHeadSHA, status: runInProgress, updatedAt: "2026-11-11T10:41:00Z",
				jobs: []jobFixture{{name: ciWaitShard, status: runInProgress}},
			}),
		},
	})
	clock := newFakeCIClock()
	stdout := &bytes.Buffer{}

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, stdout))
	require.NoError(t, err)

	assert.Equal(t, []string{ciWaitRerunCommand}, recordedRerunKeys(runner))
	assert.Equal(t, testCleanupRepoRoot, recordedReruns(runner)[0].cwd)
	assert.Equal(t, 1, script.calls[ciWaitPRHeadCommand], "the head commit is looked up only when a rerun is considered")
	assert.Contains(t, stdout.String(), ciWaitShard)
	assert.Contains(t, stdout.String(), "rerun 1 of 2")
	assert.Equal(t, 3*ciWaitTestPoll, clock.elapsed())
}

// Validates: R-6.10.16
//
// A 5xx from GitHub says nothing about the pull request, and one used to end
// the whole pre-merge stage. The poll is repeated after a backoff, which the
// wait's own clock waits out.
func TestWaitForPRCI_RepeatsAPollGitHubFailed(t *testing.T) {
	t.Parallel()

	passed := prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkPassed("e2e"))
	runner, script := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {ghScriptFailure + "HTTP 502: Bad Gateway (https://api.github.com/graphql)", passed},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.NoError(t, err)

	assert.Equal(t, 2, script.calls[ciWaitPRViewCommand])
	assert.Equal(t, githubFirstBackoff, clock.elapsed())
}

// Validates: R-6.10.16
//
// A shard that is canceled every time is not a runner hiccup. The wait stops
// after the allowed reruns instead of rerunning for ever.
func TestWaitForPRCI_ThirdCancellationFails(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {canceledShardRollup(t)},
		ciWaitPRHeadCommand: {prHeadJSON()},
		ciWaitRunViewCommand: {
			canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:20:00Z"),
			canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:50:00Z"),
			canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T11:20:00Z"),
		},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod pre-merge PR CI failed")
	assert.Contains(t, err.Error(), ciWaitShard+" canceled")
	assert.Contains(t, err.Error(), "reruns used: 2 of 2 (--ci-max-reruns)")
	assert.Equal(t, []string{ciWaitRerunCommand, ciWaitRerunCommand}, recordedRerunKeys(runner))
	assert.Equal(t, 2*ciWaitTestPoll, clock.elapsed())
}

// Validates: R-6.10.16
func TestWaitForPRCI_NoRerunsAllowedFailsOnTheFirstCancellation(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand:  {canceledShardRollup(t)},
		ciWaitPRHeadCommand:  {prHeadJSON()},
		ciWaitRunViewCommand: {canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:20:00Z")},
	})
	clock := newFakeCIClock()
	cfg := testCIWaitConfig(clock, io.Discard)
	cfg.maxReruns = 0

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, cfg)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "reruns used: 0 of 0 (--ci-max-reruns)")
	assert.Empty(t, recordedReruns(runner))
}

// Validates: R-6.10.16
//
// A push to the pull request cancels the run of the commit it replaces. That
// run is finished business: rerunning it would spend live-account time on a
// commit nobody will merge, so the wait leaves it alone and goes on waiting
// for the head commit's run.
func TestWaitForPRCI_CancelledRunOfAnOlderCommitIsNotRerun(t *testing.T) {
	t.Parallel()

	headPassed := prViewJSON(t,
		checkPassed("verify").inRun(ciWaitPushRun),
		checkPassed("integration").inRun(ciWaitPushRun),
		checkPassed("e2e").inRun(ciWaitPushRun),
	)
	runner, script := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand:  {canceledShardRollup(t), headPassed},
		ciWaitPRHeadCommand:  {prHeadJSON()},
		ciWaitRunViewCommand: {canceledShardRun(t, "older-sha", "2026-11-11T10:20:00Z")},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.NoError(t, err)

	assert.Empty(t, recordedReruns(runner), "a run of an older commit is never rerun")
	assert.Equal(t, 1, script.calls[ciWaitRunViewCommand])
	assert.Equal(t, ciWaitTestPoll, clock.elapsed())
}

// Validates: R-6.10.16
//
// If the head commit's run never reports, the superseded run must not hold the
// wait open for ever: the stall window still ends it, naming the check.
func TestWaitForPRCI_SupersededRunStillStalls(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand:  {canceledShardRollup(t)},
		ciWaitPRHeadCommand:  {prHeadJSON()},
		ciWaitRunViewCommand: {canceledShardRun(t, "older-sha", "2026-11-11T10:20:00Z")},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "stalled")
	assert.Contains(t, err.Error(), "still pending: verify")
	assert.Empty(t, recordedReruns(runner))
}

// Validates: R-6.10.16
//
// A test failure is not a cancellation. When any job of the run really failed,
// a rerun would only hide it for one more round, so the wait fails at once and
// names the job.
func TestWaitForPRCI_RealJobFailureIsNotRerun(t *testing.T) {
	t.Parallel()

	const failedShard = "verify-unit (ubuntu-latest, 1/3)"
	runner, script := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {canceledShardRollup(t)},
		ciWaitRunViewCommand: {runViewJSON(t, runFixture{
			id: ciWaitRunID, headSHA: ciWaitHeadSHA, status: checkStatusCompleted, updatedAt: "2026-11-11T10:20:00Z",
			jobs: []jobFixture{
				jobDone(failedShard, checkConclusionFailure),
				jobDone(ciWaitShard, checkConclusionCancelled),
				jobDone(nightlyAggregateJob, checkConclusionFailure),
			},
		})},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod pre-merge PR CI failed")
	assert.Contains(t, err.Error(), failedShard)
	assert.Empty(t, recordedReruns(runner))
	assert.Zero(t, script.calls[ciWaitPRHeadCommand])
	assert.Zero(t, clock.elapsed(), "a real failure ends the wait on the poll that sees it")
}

// Validates: R-6.10.16
//
// The live jobs share one concurrency group, and a third arrival cancels the
// older pending run. A required check canceled that way is rerun the same way
// as a canceled shard.
func TestWaitForPRCI_CancelledRequiredCheckIsRerun(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {
			prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkCancelled("e2e")),
			prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkCancelled("e2e"), checkPassed("e2e").startedLater()),
		},
		ciWaitPRHeadCommand: {prHeadJSON()},
		ciWaitRunViewCommand: {runViewJSON(t, runFixture{
			id: ciWaitRunID, headSHA: ciWaitHeadSHA, status: checkStatusCompleted, updatedAt: "2026-11-11T10:20:00Z",
			jobs: []jobFixture{
				jobDone(nightlyAggregateJob, checkConclusionSuccess),
				jobDone("integration", checkConclusionSuccess),
				jobDone("e2e", checkConclusionCancelled),
			},
		})},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.NoError(t, err)

	assert.Equal(t, []string{ciWaitRerunCommand}, recordedRerunKeys(runner))
	assert.Equal(t, ciWaitTestPoll, clock.elapsed())
}

// Validates: R-6.10.16
//
// An aggregate verify failure with no failed and no canceled leg has no cause
// a rerun could clear, so the wait fails instead of rerunning it.
func TestWaitForPRCI_FailureWithoutACancelledJobIsNotRerun(t *testing.T) {
	t.Parallel()

	runner, script := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {prViewJSON(t, verifyCheckFailed(), checkPassed("integration"), checkPassed("e2e"))},
		ciWaitRunViewCommand: {runViewJSON(t, runFixture{
			id: ciWaitRunID, headSHA: ciWaitHeadSHA, status: checkStatusCompleted, updatedAt: "2026-11-11T10:20:00Z",
			jobs: []jobFixture{
				jobDone("verify-lint", checkConclusionSuccess),
				jobDone(nightlyAggregateJob, checkConclusionFailure),
			},
		})},
	})

	err := waitForPRCI(
		context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(newFakeCIClock(), io.Discard),
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod pre-merge PR CI failed: verify concluded failure")
	assert.Contains(t, err.Error(), "no failed or canceled job")
	assert.Empty(t, recordedReruns(runner))
	assert.Zero(t, script.calls[ciWaitPRHeadCommand])
}

// Validates: R-6.10.16
//
// Only a failed or a canceled check can hide a canceled job. A skipped check
// is a decision, and a timed-out one hit its own limit: rerunning either would
// paper over a real result.
func TestClassifyPRChecks_OnlyFailedOrCancelledChecksNameARunToExamine(t *testing.T) {
	t.Parallel()

	for conclusion, wantRunID := range map[string]int64{
		"FAILURE":       ciWaitRunID,
		rollupCancelled: ciWaitRunID,
		"TIMED_OUT":     0,
		"SKIPPED":       0,
	} {
		e2e := rollupCheck{name: "e2e", status: rollupCompleted, conclusion: conclusion, startedAt: ciWaitFirstStart, runID: ciWaitRunID}
		snapshot := classifyPRChecks(checkRuns(checkPassed("verify"), checkPassed("integration"), e2e))

		assert.Equal(t, prCheckStateFail, snapshot.state, conclusion)
		assert.Equal(t, "e2e", snapshot.failedCheck, conclusion)
		assert.Equal(t, wantRunID, snapshot.failedRunID, conclusion)
	}
}

// Validates: R-6.10.16
//
// gh refuses to rerun a run that is still going, and the jobs still running
// may yet fail for real. The rerun waits for the run to finish.
func TestWaitForPRCI_WaitsForTheRunToFinishBeforeRerunning(t *testing.T) {
	t.Parallel()

	stillRunning := prViewJSON(t,
		checkCancelled(ciWaitShard),
		verifyCheckFailed(),
		checkPassed("integration"),
		checkRunning("e2e"),
	)
	allPassed := prViewJSON(t,
		checkPassed(ciWaitShard).startedLater(),
		checkPassed("verify").startedLater(),
		checkPassed("integration"),
		checkPassed("e2e"),
	)
	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {stillRunning, canceledShardRollup(t), allPassed},
		ciWaitPRHeadCommand: {prHeadJSON()},
		ciWaitRunViewCommand: {
			runViewJSON(t, runFixture{
				id: ciWaitRunID, headSHA: ciWaitHeadSHA, status: runInProgress, updatedAt: "2026-11-11T10:20:00Z",
				jobs: []jobFixture{
					jobDone(ciWaitShard, checkConclusionCancelled),
					jobDone(nightlyAggregateJob, checkConclusionFailure),
					{name: "e2e", status: runInProgress},
				},
			}),
			// A different timestamp: had the first poll already rerun the run,
			// this one would count as a second cancellation and rerun again.
			canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:50:00Z"),
		},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, io.Discard))
	require.NoError(t, err)

	assert.Equal(t, []string{ciWaitRerunCommand}, recordedRerunKeys(runner))
	assert.Equal(t, 2*ciWaitTestPoll, clock.elapsed())
}

// Validates: R-6.10.16
//
// --ci-timeout stays the hard cap: a wait whose checks keep changing state
// without ever finishing still ends.
func TestWaitForPRCI_HardCapEndsAWaitThatKeepsChanging(t *testing.T) {
	t.Parallel()

	queued := prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkQueued("e2e"))
	running := prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkRunning("e2e"))
	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {queued, running, queued, running, queued, running, queued, running},
	})
	clock := newFakeCIClock()
	cfg := testCIWaitConfig(clock, io.Discard)
	cfg.hardCap = 45 * time.Minute

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, cfg)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod pre-merge PR CI timed out after 45m0s")
	assert.Contains(t, err.Error(), "e2e is")
	assert.Equal(t, 50*time.Minute, clock.elapsed(), "the first poll at or past the cap ends the wait")
}

// Validates: R-6.10.16
func TestWaitForPRCI_ContextEndStopsTheWait(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {prViewJSON(t, checkQueued("verify"), checkQueued("integration"), checkQueued("e2e"))},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForPRCI(ctx, runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(newFakeCIClock(), io.Discard))
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "dod pre-merge PR CI wait")
}

// Validates: R-6.10.16
//
// The push run of the merge commit is the last gate of an increment. A shard
// the runner canceled there is rerun the same way as on the pull request.
func TestWaitForPostMergePushCI_CancelledPushRunIsRerunOnce(t *testing.T) {
	t.Parallel()

	runner, script := scriptedRunner(map[string][]string{
		ciWaitRunListCommand: {
			pushRunListJSON(t, checkStatusCompleted, checkConclusionFailure, "2026-11-11T10:20:00Z"),
			pushRunListJSON(t, runInProgress, "", "2026-11-11T10:31:00Z"),
			pushRunListJSON(t, checkStatusCompleted, checkConclusionSuccess, "2026-11-11T10:45:00Z"),
		},
		ciWaitPushRunViewCommand: {runViewJSON(t, runFixture{
			id: ciWaitPushRun, headSHA: ciWaitMergeSHA, status: checkStatusCompleted, updatedAt: "2026-11-11T10:20:00Z",
			jobs: canceledShardJobs(),
		})},
	})
	clock := newFakeCIClock()
	stdout := &bytes.Buffer{}

	err := waitForPostMergePushCI(context.Background(), runner, testCleanupRepoRoot, ciWaitMergeSHA, testCIWaitConfig(clock, stdout))
	require.NoError(t, err)

	assert.Equal(t, []string{ciWaitPushRerunCommand}, recordedRerunKeys(runner))
	assert.Equal(t, 1, script.calls[ciWaitPushRunViewCommand])
	assert.Contains(t, stdout.String(), "dod post-merge CI")
	assert.Contains(t, stdout.String(), "rerun 1 of 2")
	assert.Equal(t, 2*ciWaitTestPoll, clock.elapsed())
}

// Validates: R-6.10.16
func TestWaitForPostMergePushCI_SkippedRunFailsWithoutLookingForCancelledJobs(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitRunListCommand: {pushRunListJSON(t, checkStatusCompleted, checkConclusionSkipped, "2026-11-11T10:20:00Z")},
	})

	err := waitForPostMergePushCI(
		context.Background(), runner, testCleanupRepoRoot, ciWaitMergeSHA, testCIWaitConfig(newFakeCIClock(), io.Discard),
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod post-merge CI is not clean")
	assert.Contains(t, err.Error(), "every push to main must run the quick lane")
	assert.Empty(t, recordedReruns(runner))
}

// Validates: R-6.10.16
func TestWaitForPostMergePushCI_StallNamesTheMissingRun(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{ciWaitRunListCommand: {"[]"}})
	clock := newFakeCIClock()

	err := waitForPostMergePushCI(context.Background(), runner, testCleanupRepoRoot, ciWaitMergeSHA, testCIWaitConfig(clock, io.Discard))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "dod post-merge CI stalled")
	assert.Contains(t, err.Error(), ciWaitMergeSHA)
	assert.Equal(t, ciWaitTestStall, clock.elapsed())
}

// Validates: R-6.10.16
func TestWaitForPostMergePushCI_RequiresAMergeCommit(t *testing.T) {
	t.Parallel()

	err := waitForPostMergePushCI(
		context.Background(), &fakeRunner{}, testCleanupRepoRoot, "", testCIWaitConfig(newFakeCIClock(), io.Discard),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing merge commit")
}

// Validates: R-6.10.16
//
// A rerun adds a second run of the same check to the rollup and gh lists both.
// Only the newest says where the check stands now.
func TestClassifyPRChecks_UsesTheNewestRunOfARepeatedCheck(t *testing.T) {
	t.Parallel()

	snapshot := classifyPRChecks(checkRuns(
		verifyCheckFailed(),
		checkPassed("verify").startedLater(),
		checkPassed("integration"),
		checkPassed("e2e"),
	))
	assert.Equal(t, prCheckStatePass, snapshot.state)

	snapshot = classifyPRChecks(checkRuns(
		checkPassed("verify").startedLater(),
		verifyCheckFailed(),
		checkPassed("integration"),
		checkPassed("e2e"),
	))
	assert.Equal(t, prCheckStatePass, snapshot.state, "the order gh lists them in does not decide")
}

// Validates: R-6.10.16
//
// A failed check cannot be waited away, so it is reported while others still
// run, together with the run to look at.
func TestClassifyPRChecks_FailureOutranksPending(t *testing.T) {
	t.Parallel()

	snapshot := classifyPRChecks(checkRuns(
		checkRunning("e2e"),
		verifyCheckFailed(),
		checkPassed("integration"),
	))

	assert.Equal(t, prCheckStateFail, snapshot.state)
	assert.Equal(t, "verify concluded failure", snapshot.reason)
	assert.Equal(t, "verify", snapshot.failedCheck)
	assert.Equal(t, ciWaitRunID, snapshot.failedRunID)
	assert.Equal(t, []string{"e2e"}, snapshot.pending)
}

// Validates: R-6.10.16
func TestClassifyPRChecks_NamesEveryPendingRequiredCheckInOrder(t *testing.T) {
	t.Parallel()

	snapshot := classifyPRChecks(checkRuns(
		checkQueued("verify"),
		checkRunning("e2e"),
		checkPassed("stress"),
	))

	assert.Equal(t, prCheckStatePending, snapshot.state)
	assert.Equal(t, []string{"e2e", "integration", "verify"}, snapshot.pending)
	assert.Equal(t, "verify is queued", snapshot.reason)

	snapshot = classifyPRChecks(checkRuns(checkPassed("verify")))
	assert.Equal(t, prCheckStatePending, snapshot.state)
	assert.Equal(t, "missing required checks: e2e, integration", snapshot.reason)
}

// Validates: R-6.10.16
//
// The stall window is about any check changing state, required or not, and
// not about the order gh happens to list them in.
func TestClassifyPRChecks_FingerprintFollowsCheckStatesOnly(t *testing.T) {
	t.Parallel()

	base := classifyPRChecks(checkRuns(checkQueued("verify"), checkQueued("verify-lint")))
	reordered := classifyPRChecks(checkRuns(checkQueued("verify-lint"), checkQueued("verify")))
	progressed := classifyPRChecks(checkRuns(checkQueued("verify"), checkRunning("verify-lint")))

	assert.Equal(t, base.fingerprint, reordered.fingerprint)
	assert.NotEqual(t, base.fingerprint, progressed.fingerprint)
}

// Validates: R-6.10.16
func TestClassifyPostMergeRuns_FailedRunNamesTheRunToExamine(t *testing.T) {
	t.Parallel()

	snapshot := classifyPostMergeRuns([]githubRun{{
		DatabaseID: ciWaitPushRun,
		Name:       "CI",
		Event:      "push",
		Status:     checkStatusCompleted,
		Conclusion: checkConclusionFailure,
		HeadSHA:    ciWaitMergeSHA,
	}}, ciWaitMergeSHA)

	assert.Equal(t, prCheckStateFail, snapshot.state)
	assert.Equal(t, "CI concluded failure", snapshot.reason)
	assert.Equal(t, ciWaitPushRun, snapshot.failedRunID)
}

// Validates: R-6.10.16
func TestCIRunIDFromDetailsURL(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int64(111), ciRunIDFromDetailsURL("https://github.com/o/r/actions/runs/111/job/5"))
	assert.Equal(t, int64(222), ciRunIDFromDetailsURL("https://github.com/o/r/actions/runs/222"))
	assert.Zero(t, ciRunIDFromDetailsURL("https://ci.example.com/status/9"), "a check that is not an Actions job has no run")
	assert.Zero(t, ciRunIDFromDetailsURL(""))
}

const stallWorkflowFixture = `jobs:
  verify-lint:
    timeout-minutes: 15
  stress:
    timeout-minutes: 90
  integration:
    timeout-minutes: 45
`

// Validates: R-6.10.16
//
// The window is derived from the workflow, so lengthening a job there cannot
// leave the wait giving up on a job that is still inside its own timeout.
func TestCIStallWindow_IsTheLongestJobTimeoutPlusAMargin(t *testing.T) {
	t.Parallel()

	cfg, err := newCIWaitConfig(&VerifyOptions{}, writeWorkflowFixture(t, stallWorkflowFixture), ciEventPullRequest, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 100*time.Minute, cfg.stallWindow)

	cfg, err = newCIWaitConfig(&VerifyOptions{}, repoRootForTest(t), ciEventPullRequest, io.Discard)
	require.NoError(t, err)
	assert.Greater(t, cfg.stallWindow, ciStallMargin)
}

// Validates: R-6.10.16
func TestCIStallWindow_FailsWithoutAWorkflowTimeout(t *testing.T) {
	t.Parallel()

	_, err := newCIWaitConfig(&VerifyOptions{}, t.TempDir(), ciEventPush, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ciWorkflowPath)

	_, err = newCIWaitConfig(&VerifyOptions{}, writeWorkflowFixture(t, "jobs:\n  verify:\n    steps: []\n"), ciEventPush, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout-minutes")
}

// Validates: R-6.10.16
func TestNewCIWaitConfig(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, stallWorkflowFixture)

	cfg, err := newCIWaitConfig(&VerifyOptions{DODCIMaxReruns: DefaultDODCIMaxReruns}, root, ciEventPullRequest, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 100*time.Minute, cfg.stallWindow)
	assert.Equal(t, 300*time.Minute, cfg.hardCap, "one stall window for the first attempt and one per rerun")
	assert.Equal(t, DefaultDODCIMaxReruns, cfg.maxReruns)
	assert.Equal(t, prCIPollInterval, cfg.pollInterval)
	require.NotNil(t, cfg.now)
	require.NotNil(t, cfg.sleep)

	cfg, err = newCIWaitConfig(&VerifyOptions{DODCITimeout: 45 * time.Minute}, root, ciEventPullRequest, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 45*time.Minute, cfg.hardCap, "--ci-timeout is the hard cap when given")
	assert.Zero(t, cfg.maxReruns)

	cfg, err = newCIWaitConfig(&VerifyOptions{}, root, ciEventPush, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 100*time.Minute, cfg.hardCap)

	_, err = newCIWaitConfig(&VerifyOptions{DODCIMaxReruns: -1}, root, ciEventPush, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ci-max-reruns")
}

// Validates: R-6.10.16
func TestSleepContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sleepContext(ctx, time.Hour), context.Canceled)

	require.NoError(t, sleepContext(context.Background(), time.Nanosecond))
}

// Validates: R-6.10.16
//
// GitHub can start a rerun and still answer the request with a 502. The
// repeat is refused because the run is already running, which means the
// first request worked: the wait re-reads the run, finds it going, and waits
// for it instead of failing on the refusal or asking a third time.
func TestWaitForPRCI_RerunGitHubStartedDespiteA5xxIsWaitedFor(t *testing.T) {
	t.Parallel()

	rerunStarted := prViewJSON(t,
		checkCancelled(ciWaitShard),
		checkRunning(ciWaitShard).startedLater(),
		verifyCheckFailed(),
		checkPassed("integration"),
		checkPassed("e2e"),
	)
	rerunPassed := prViewJSON(t,
		checkCancelled(ciWaitShard),
		checkPassed(ciWaitShard).startedLater(),
		verifyCheckFailed(),
		checkPassed("verify").startedLater(),
		checkPassed("integration"),
		checkPassed("e2e"),
	)
	running := runViewJSON(t, runFixture{
		id: ciWaitRunID, headSHA: ciWaitHeadSHA, status: runInProgress, updatedAt: "2026-11-11T10:41:00Z",
		jobs: []jobFixture{{name: ciWaitShard, status: runInProgress}},
	})
	runner, script := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand:  {canceledShardRollup(t), rerunStarted, rerunPassed},
		ciWaitPRHeadCommand:  {prHeadJSON()},
		ciWaitRunViewCommand: {canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:20:00Z"), running},
		ciWaitRerunCommand: {
			ghScriptFailure + "HTTP 502: Bad Gateway (https://api.github.com/repos/o/r/actions/runs/111/rerun-failed-jobs)",
			ghScriptFailure + "HTTP 403: This workflow is already running (https://api.github.com/repos/o/r/actions/runs/111/rerun-failed-jobs)",
		},
	})
	clock := newFakeCIClock()
	stdout := &bytes.Buffer{}

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(clock, stdout))
	require.NoError(t, err)

	assert.Equal(t, 2, script.calls[ciWaitRerunCommand], "the refused repeat is not followed by a third request")
	assert.Equal(t, 3, script.calls[ciWaitRunViewCommand], "examined, read again after the refusal, examined on the next poll")
	assert.Contains(t, stdout.String(), "rerun 1 of 2")
}

// Validates: R-6.10.16
//
// A rerun refusal after a 5xx counts only when the run is in fact running
// again. A run that still reads finished was not started by this wait.
func TestWaitForPRCI_RefusedRerunOfAFinishedRunFails(t *testing.T) {
	t.Parallel()

	finished := canceledShardRun(t, ciWaitHeadSHA, "2026-11-11T10:20:00Z")
	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand:  {canceledShardRollup(t)},
		ciWaitPRHeadCommand:  {prHeadJSON()},
		ciWaitRunViewCommand: {finished},
		ciWaitRerunCommand: {
			ghScriptFailure + "HTTP 502: Bad Gateway (https://api.github.com/repos/o/r/actions/runs/111/rerun-failed-jobs)",
			ghScriptFailure + "HTTP 403: This workflow is already running (https://api.github.com/repos/o/r/actions/runs/111/rerun-failed-jobs)",
		},
	})

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, testCIWaitConfig(newFakeCIClock(), io.Discard))
	require.ErrorContains(t, err, "already running")
	assert.Contains(t, err.Error(), "reads completed")
}

// ciWaitQueuedWindow is the window of a check whose job waits in a
// concurrency group: one whole run of the group's longest job, plus the
// margin.
const ciWaitQueuedWindow = 190 * time.Minute

// queuedCheckWaitConfig is a pull request wait whose e2e job sits in a
// concurrency group, polled every minute.
func queuedCheckWaitConfig(clock *fakeCIClock) ciWaitConfig {
	cfg := testCIWaitConfig(clock, io.Discard)
	cfg.stallWindow = 50 * time.Minute
	cfg.notStartedWindows = map[string]time.Duration{prCheckE2E: ciWaitQueuedWindow}
	cfg.pollInterval = time.Minute

	return cfg
}

// Validates: R-6.10.16
//
// A pull request's e2e job that waits in its concurrency group behind a
// nightly run changes no state for as long as that run takes. It is not
// stalled after the pull request's own window; the wait gives up only after
// the group's longest run.
func TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkQueued("e2e"))},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, queuedCheckWaitConfig(clock))
	require.ErrorContains(t, err, "stalled")
	assert.Contains(t, err.Error(), "e2e")
	assert.Equal(t, ciWaitQueuedWindow, clock.elapsed(), "not stalled at 56 minutes: the check had not started")
}

// Validates: R-6.10.16
//
// Once the queued job runs, its own event's window applies again.
func TestWaitForPRCI_RunningCheckStallsAtTheEventsWindow(t *testing.T) {
	t.Parallel()

	runner, _ := scriptedRunner(map[string][]string{
		ciWaitPRViewCommand: {prViewJSON(t, checkPassed("verify"), checkPassed("integration"), checkRunning("e2e"))},
	})
	clock := newFakeCIClock()

	err := waitForPRCI(context.Background(), runner, testCleanupRepoRoot, ciWaitPR, queuedCheckWaitConfig(clock))
	require.ErrorContains(t, err, "stalled")
	assert.Equal(t, 50*time.Minute, clock.elapsed())
}
