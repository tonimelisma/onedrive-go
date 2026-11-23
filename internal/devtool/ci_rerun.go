package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// ciRerunMaxAttempts bounds the attempts of one CI run that the rerun of
	// unstarted jobs asks for: the first, and two reruns, the same as the DoD
	// stages' --ci-max-reruns default. A shortage of runners that outlasts
	// three attempts is GitHub's, and a person decides what to do about it.
	ciRerunMaxAttempts = 3

	ciRerunViewFields = "attempt,conclusion,jobs,status,url"
)

// CIRerunOptions configures RerunUnstartedCIJobs.
type CIRerunOptions struct {
	// RepoRoot is the checkout gh runs in; it names the repository.
	RepoRoot string
	// RunID is the finished CI run to judge.
	RunID  int64
	Stdout io.Writer
}

// unstartedRerunPlan is the verdict on one finished run: whether to rerun
// it, and why, in a sentence that names the jobs.
type unstartedRerunPlan struct {
	rerun  bool
	reason string
}

// RerunUnstartedCIJobs reruns the jobs of a finished CI run that no runner
// ever took, when that is all that went wrong with it (R-6.10.39). GitHub
// cancels a job it could not find a runner for (macOS runners run short), and
// the aggregate verify job then fails a change nothing is wrong with; a
// person, or the agent watching the pull request, used to notice and rerun
// it. The decision is planUnstartedRerun's; this function reads the run and,
// when the plan says so, asks GitHub for the rerun.
func RerunUnstartedCIJobs(ctx context.Context, runner commandRunner, opts CIRerunOptions) error {
	if opts.RunID <= 0 {
		return errors.New("ci rerun-unstarted: --run-id must name a workflow run")
	}
	gh := newGHClient(runner, nil)
	id := strconv.FormatInt(opts.RunID, 10)
	out, err := gh.output(ctx, opts.RepoRoot, "ci gh run view "+id, "run", "view", id, "--json", ciRerunViewFields)
	if err != nil {
		return fmt.Errorf("ci rerun-unstarted: %w", err)
	}
	var run githubRun
	if err := json.Unmarshal(out, &run); err != nil {
		return fmt.Errorf("ci rerun-unstarted: decode gh run view %s: %w", id, err)
	}

	plan := planUnstartedRerun(&run, ciRerunMaxAttempts)
	if !plan.rerun {
		return writeStatus(opts.Stdout, fmt.Sprintf("run %s: no rerun: %s\n", id, plan.reason))
	}
	// GitHub refuses to rerun a run that is running, so a repeat after a
	// failed answer cannot start a second attempt.
	if _, err := gh.outputUnlessDone(ctx, opts.RepoRoot, "ci gh run rerun "+id, isRerunAlreadyRunning,
		"run", "rerun", id, "--failed"); err != nil {
		return fmt.Errorf("ci rerun-unstarted: %w", err)
	}

	return writeStatus(opts.Stdout, fmt.Sprintf("run %s: %s; rerun requested (attempt %d of at most %d)\n",
		id, plan.reason, run.Attempt+1, ciRerunMaxAttempts))
}

// planUnstartedRerun decides whether a finished run is worth a rerun: only
// when it failed, under maxAttempts, and every job that did not succeed was
// canceled before a runner took it, the aggregate verify job aside, whose
// failure only repeats its legs'. A job that failed, or was canceled while it
// ran (a timeout, or a person), is a verdict a rerun must not paper over, and
// a run canceled as a whole was superseded by a newer push or stopped by a
// person. The plan is deterministic: it reads the run and nothing else.
func planUnstartedRerun(run *githubRun, maxAttempts int) unstartedRerunPlan {
	if !strings.EqualFold(run.Status, checkStatusCompleted) {
		return unstartedRerunPlan{reason: "the run is " + strings.ToLower(run.Status)}
	}
	if !strings.EqualFold(run.Conclusion, "failure") {
		return unstartedRerunPlan{reason: "the run concluded " + strings.ToLower(run.Conclusion)}
	}
	if run.Attempt >= maxAttempts {
		return unstartedRerunPlan{reason: fmt.Sprintf("attempt %d of at most %d", run.Attempt, maxAttempts)}
	}

	var unstarted []string
	for _, job := range run.Jobs {
		switch strings.ToLower(job.Conclusion) {
		case "", checkConclusionSuccess, checkConclusionSkipped:
		case checkConclusionCancelled:
			if job.started() {
				return unstartedRerunPlan{reason: job.Name + " was canceled while it ran"}
			}
			unstarted = append(unstarted, job.Name)
		default:
			if job.Name != nightlyAggregateJob {
				return unstartedRerunPlan{reason: job.Name + " failed"}
			}
		}
	}
	if len(unstarted) == 0 {
		return unstartedRerunPlan{reason: "no job waited for a runner in vain"}
	}

	return unstartedRerunPlan{rerun: true, reason: "no runner took " + strings.Join(unstarted, ", ")}
}
