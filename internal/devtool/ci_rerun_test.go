package devtool

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ciRerunTestRunID = 4242

// ciRerunJob builds a job of a finished run: started says whether a runner
// picked it up, which a job lists as its first step.
func ciRerunJob(name, conclusion string, started bool) githubJob {
	job := githubJob{Name: name, Conclusion: conclusion, StartedAt: "2026-11-23T18:09:21Z"}
	if started {
		job.Steps = []json.RawMessage{json.RawMessage(`{"name":"Set up job"}`)}
	}

	return job
}

// ciRerunRun is a finished run whose aggregate verify failed because the
// jobs given did not all succeed.
func ciRerunRun(attempt int, jobs ...githubJob) githubRun {
	all := append([]githubJob{ciRerunJob(nightlyAggregateJob, "failure", true)}, jobs...)

	return githubRun{
		Status: "completed", Conclusion: "failure", Attempt: attempt,
		URL: "https://example.test/run", Jobs: all,
	}
}

// Validates: R-6.10.39
func TestPlanUnstartedRerun(t *testing.T) {
	t.Parallel()

	unstarted := ciRerunJob("verify-unit (macos-latest, 3/3)", checkConclusionCancelled, false)
	passed := ciRerunJob("verify-lint (linux)", "success", true)

	cases := []struct {
		name       string
		run        githubRun
		wantRerun  bool
		wantReason string
	}{
		{
			name: "jobs no runner took, nothing else wrong", run: ciRerunRun(1, passed, unstarted),
			wantRerun: true, wantReason: "no runner took verify-unit (macos-latest, 3/3)",
		},
		{
			name: "a job failed for real", run: ciRerunRun(1, unstarted, ciRerunJob("verify-unit (ubuntu-latest, 1/3)", "failure", true)),
			wantReason: "verify-unit (ubuntu-latest, 1/3) failed",
		},
		{
			name: "a job canceled while it ran, a timeout or a person", run: ciRerunRun(1, unstarted, ciRerunJob("e2e", checkConclusionCancelled, true)),
			wantReason: "e2e was canceled while it ran",
		},
		{
			name: "attempts used up", run: ciRerunRun(ciRerunMaxAttempts, unstarted),
			wantReason: "attempt 3 of at most 3",
		},
		{
			name: "the whole run canceled: a newer push superseded it",
			run: githubRun{
				Status: "completed", Conclusion: checkConclusionCancelled, Attempt: 1,
				Jobs: []githubJob{unstarted},
			},
			wantReason: "the run concluded " + checkConclusionCancelled,
		},
		{
			name: "still running", run: githubRun{Status: "in_progress", Attempt: 1, Jobs: []githubJob{unstarted}},
			wantReason: "the run is in_progress",
		},
		{
			name: "the aggregate failed with no leg to blame", run: ciRerunRun(1, passed),
			wantReason: "no job waited for a runner in vain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan := planUnstartedRerun(&tc.run, ciRerunMaxAttempts)
			assert.Equal(t, tc.wantRerun, plan.rerun)
			assert.Contains(t, plan.reason, tc.wantReason)
		})
	}
}

// Validates: R-6.10.39
func TestRerunUnstartedCIJobsRerunsOnlyWhenThePlanSays(t *testing.T) {
	t.Parallel()

	viewKey := "gh run view 4242 --json " + ciRerunViewFields
	for _, tc := range []struct {
		name      string
		run       githubRun
		wantRerun bool
	}{
		{name: "unstarted", run: ciRerunRun(1, ciRerunJob("e2e", checkConclusionCancelled, false)), wantRerun: true},
		{name: "failed", run: ciRerunRun(1, ciRerunJob("e2e", "failure", true))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			view, err := json.Marshal(tc.run)
			require.NoError(t, err)
			runner := &fakeRunner{outputs: map[string][]byte{viewKey: view}}
			var stdout bytes.Buffer

			require.NoError(t, RerunUnstartedCIJobs(t.Context(), runner, CIRerunOptions{
				RepoRoot: t.TempDir(), RunID: ciRerunTestRunID, Stdout: &stdout,
			}))

			var reruns int
			for _, command := range runner.outputCommands {
				if strings.Join(command.args, " ") == "run rerun 4242 --failed" {
					reruns++
				}
			}
			if tc.wantRerun {
				assert.Equal(t, 1, reruns)
				assert.Contains(t, stdout.String(), "rerun requested")
			} else {
				assert.Zero(t, reruns)
				assert.Contains(t, stdout.String(), "no rerun")
			}
		})
	}
}

// Validates: R-6.10.39
func TestRerunUnstartedCIJobsFailsWhenTheRunCannotBeRead(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputErr: errInjectedRemove}
	err := RerunUnstartedCIJobs(t.Context(), runner, CIRerunOptions{
		RepoRoot: t.TempDir(), RunID: ciRerunTestRunID, Stdout: &bytes.Buffer{},
	})
	require.ErrorIs(t, err, errInjectedRemove)
}
