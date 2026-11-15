package devtool

import (
	"context"
	"fmt"
	"io"
	"time"
)

const (
	// raceSyncBaseTimeout is go test's deadline for the race profile's one run
	// of the sync tree on a host with at least unitTestReferenceCores cores.
	// The verify-race job, that run plus the job's setup, took 12.2 and 15.8
	// minutes on two pushes of 2026-11-15, and three runs took 31.3 minutes in
	// the scheduled stress step of 2026-11-14: a slow push passes, and a hang
	// ends here with a goroutine dump. The verify-race job's timeout-minutes
	// covers it and the job's setup, and the stress lane's race runs of the
	// tree take it once per run (TestCIJobTimeoutsCoverEveryGoTestDeadline).
	raceSyncBaseTimeout = 30 * time.Minute

	// raceSyncSmallHostCoreBudget is the deadline on a host with fewer cores
	// than the reference, as core time, measured like the unit tiers': one run
	// of this profile on a shared, loaded 2-core host took 87.1 minutes, 174
	// core-minutes. 240 leaves that run about 40% of headroom: 120 minutes on
	// 2 cores, 240 on one.
	raceSyncSmallHostCoreBudget = 240 * time.Minute
)

// raceSyncTimeoutForCores is the race profile's go test deadline for the
// cores the run may use, and the stress lane's for each of its race runs of
// the tree (stressSyncRaceTimeout).
func raceSyncTimeoutForCores(cores int) time.Duration {
	return deadlineForCores(cores, raceSyncBaseTimeout, raceSyncSmallHostCoreBudget)
}

// resolveRaceSyncTimeout returns the explicit deadline (`--race-timeout`)
// when one was given, else the race profile's deadline for the cores. The
// explicit one is the race profile's alone: the stress lane's race runs of
// the tree always take the deadline of the cores.
func resolveRaceSyncTimeout(explicit time.Duration, cores int) time.Duration {
	if explicit > 0 {
		return explicit
	}

	return raceSyncTimeoutForCores(cores)
}

// runRaceVerification runs the race profile: the whole sync tree under the
// race detector, unshortened, so the exhaustive tests run too, the model
// tests with their race-detector seeds among them, and never from go test's
// cache, which would report a cached result as a run. It is what CI runs on
// every push to main, so a data race in the sync packages fails the push that
// brought it. The step goes through the empty-run guard like every go test
// step.
func runRaceVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	timeout time.Duration,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep(raceStepName, func() error {
		deadline := "-timeout=" + timeout.String()
		if err := writeStatus(stdout, fmt.Sprintf("==> go test -race -count=1 %s %s\n", deadline, syncTestTree)); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		_, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, raceStepName,
			"test", "-race", "-count=1", deadline, syncTestTree)

		return err
	})
}

// raceStepName names the race profile's one step in its summary.
const raceStepName = "sync race"
