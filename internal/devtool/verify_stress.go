package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const millisecondsPerSecond = 1000

// syncTestTree is the sync target of the stress, race and seeds profiles: the
// whole tree, not the one directory, so a package split out of internal/sync
// stays under them instead of silently leaving their lanes.
const syncTestTree = "./internal/sync/..."

// stressSyncConcurrencyPattern selects the sync tests that exercise
// goroutines, timers, cancellation and shutdown: the watch runtime and
// observers, the one-shot result loop, the worker pool and the concurrent
// store and graph tests. Over the tree, a package with none of them is
// expected; the step fails only when no package has one.
const stressSyncConcurrencyPattern = "TestRunWatch|TestWatch|TestOneShot|TestWorkerPool|Shutdown|Drain|Concurren|Cancel"

// stressSyncRaceRuns is how many race runs of the whole sync tree the stress
// lane makes, as the -count of one go test. The step's name, its status line
// and its deadline all follow from it, the deadline as this many times the
// race profile's deadline for one run on the same cores, so a run of the tree
// that grows moves the two lanes' budgets together.
const stressSyncRaceRuns = 3

// stressSyncRaceTimeout is go test's deadline for the stress lane's race runs
// of the sync tree on a host with the given cores.
func stressSyncRaceTimeout(cores int) time.Duration {
	return stressSyncRaceRuns * raceSyncTimeoutForCores(cores)
}

// stressSyncRaceStepName names the stress lane's race runs of the sync tree
// in the summary, by their number.
func stressSyncRaceStepName() string {
	return fmt.Sprintf("sync race x%d", stressSyncRaceRuns)
}

// stressSyncRaceCommand is the stress lane's step of race runs of the whole
// sync tree for the given cores. It runs through `go test -json`, so the
// slowest tests can be read from its output.
func stressSyncRaceCommand(cores int) stressCommandSpec {
	count := "-count=" + strconv.Itoa(stressSyncRaceRuns)
	deadline := "-timeout=" + stressSyncRaceTimeout(cores).String()

	return stressCommandSpec{
		stepName:         stressSyncRaceStepName(),
		statusLine:       fmt.Sprintf("==> go test -json -race %s -shuffle=on %s %s\n", count, deadline, syncTestTree),
		args:             []string{"test", "-json", "-race", count, "-shuffle=on", deadline, syncTestTree},
		collectSlowTests: true,
	}
}

type stressCommandSpec struct {
	stepName         string
	statusLine       string
	args             []string
	collectSlowTests bool
}

// stressTestKey identifies one test across a package tree: two packages may
// each hold a test of the same name.
type stressTestKey struct {
	pkg  string
	test string
}

type stressTestTiming struct {
	runs           int
	totalElapsedMS int64
	maxElapsedMS   int64
}

func runStress(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	cores int,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	// Repetition goes where the concurrency is. One race run of the whole
	// sync tree, with the job's setup, took 12.2 and 15.8 minutes in CI's
	// verify-race job on 2026-11-15; three took 31.3 minutes in this lane on
	// 2026-11-14 and had not finished at a fixed 40-minute deadline on
	// 2026-11-15. So fifty no longer fit any budget, and the few runs of the
	// tree take the race profile's deadline once per run. The tree's watch,
	// runtime, shutdown and worker tests (where the 1-in-50 flakes have come
	// from) still run fifty times: one pass of them took about 47 seconds on
	// 2026-11-15, 39.4 minutes for the fifty. Every step shuffles test order,
	// so each repetition also varies what runs beside what.
	stressCommands := []stressCommandSpec{
		{
			stepName: "multisync race x50",
			statusLine: fmt.Sprintf(
				"==> go test -race -count=50 -shuffle=on -timeout=%s ./internal/multisync\n",
				stressMultisyncTimeout,
			),
			args: []string{
				"test",
				"-race",
				"-count=50",
				"-shuffle=on",
				"-timeout=" + stressMultisyncTimeout,
				"./internal/multisync",
			},
		},
		{
			stepName: "sync concurrency race x50",
			statusLine: fmt.Sprintf(
				"==> go test -race -count=50 -shuffle=on -timeout=%s -run %s %s\n",
				stressSyncConcurrencyTimeout, stressSyncConcurrencyPattern, syncTestTree,
			),
			args: []string{
				"test",
				"-race",
				"-count=50",
				"-shuffle=on",
				"-timeout=" + stressSyncConcurrencyTimeout,
				"-run", stressSyncConcurrencyPattern,
				syncTestTree,
			},
		},
		stressSyncRaceCommand(cores),
	}

	// Every step runs whatever an earlier one returned: a multisync flake says
	// nothing about the sync tree, and the nightly is the only run of this
	// lane, so stopping would hide the other steps' state for a day.
	var stepErrs []error
	for _, command := range stressCommands {
		err := collector.runStepWithOutcome(command.stepName, func() (stepOutcome, error) {
			slowTests, runErr := runStressCommand(ctx, runner, repoRoot, env, stdout, stderr, command)
			return stepOutcome{slowTests: slowTests}, runErr
		})
		if err != nil {
			stepErrs = append(stepErrs, err)
		}
	}

	return errors.Join(stepErrs...)
}

// runStressCommand runs one stress step through the empty-run guard, whose
// tee streams the output as go test prints it: a step that hangs shows every
// line up to the hang, and a job killed meanwhile keeps them. Every step
// fails when no package of its target ran a test, with or without a -run
// pattern: a multisync or sync tree that lost its tests verifies nothing
// either. A step that collects slow tests reads them from the captured -json
// output once go test exits.
func runStressCommand(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	command stressCommandSpec,
) ([]StressSlowTestSummary, error) {
	if err := writeStatus(stdout, command.statusLine); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	output, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, command.stepName, command.args...)

	var slowTests []StressSlowTestSummary
	if command.collectSlowTests {
		slowTests = summarizeStressSlowTests(output, stressSlowTestLimit)
		if writeErr := writeStressSlowTestSummary(stdout, slowTests); writeErr != nil {
			return slowTests, errors.Join(
				fmt.Errorf("write stress timing summary: %w", writeErr),
				wrapStressError(err),
			)
		}
	}

	return slowTests, wrapStressError(err)
}

// wrapStressError names a failed stress step's error as a stress failure.
func wrapStressError(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("stress tests: %w", err)
}

// summarizeStressSlowTests totals the completed tests of `go test -json`
// output by package and test, slowest first.
func summarizeStressSlowTests(output string, limit int) []StressSlowTestSummary {
	totals := make(map[stressTestKey]*stressTestTiming)

	for line := range strings.Lines(output) {
		_, event, isEvent := decodeGoTestLogLine(line)
		if !isEvent || event.Test == "" {
			continue
		}
		if event.Action != goTestActionPass && event.Action != goTestActionFail {
			continue
		}

		elapsedMS := durationMSFromSeconds(event.Elapsed)
		key := stressTestKey{pkg: event.Package, test: event.Test}
		timing := totals[key]
		if timing == nil {
			timing = &stressTestTiming{}
			totals[key] = timing
		}

		timing.runs++
		timing.totalElapsedMS += elapsedMS
		if elapsedMS > timing.maxElapsedMS {
			timing.maxElapsedMS = elapsedMS
		}
	}

	summaries := make([]StressSlowTestSummary, 0, len(totals))
	for key, timing := range totals {
		summaries = append(summaries, StressSlowTestSummary{
			Package:        key.pkg,
			Test:           key.test,
			Runs:           timing.runs,
			TotalElapsedMS: timing.totalElapsedMS,
			MaxElapsedMS:   timing.maxElapsedMS,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].TotalElapsedMS != summaries[j].TotalElapsedMS {
			return summaries[i].TotalElapsedMS > summaries[j].TotalElapsedMS
		}
		if summaries[i].MaxElapsedMS != summaries[j].MaxElapsedMS {
			return summaries[i].MaxElapsedMS > summaries[j].MaxElapsedMS
		}
		if summaries[i].Package != summaries[j].Package {
			return summaries[i].Package < summaries[j].Package
		}

		return summaries[i].Test < summaries[j].Test
	})

	if limit > 0 && len(summaries) > limit {
		summaries = summaries[:limit]
	}

	return summaries
}

func writeStressSlowTestSummary(w io.Writer, slowTests []StressSlowTestSummary) error {
	if len(slowTests) == 0 {
		return nil
	}

	if err := writeStatus(w, "==> slowest completed sync race tests\n"); err != nil {
		return err
	}

	for _, slow := range slowTests {
		if err := writeStatus(
			w,
			fmt.Sprintf(
				"  %s runs=%d total=%s max=%s\n",
				slow.name(),
				slow.Runs,
				formatDurationMS(slow.TotalElapsedMS),
				formatDurationMS(slow.MaxElapsedMS),
			),
		); err != nil {
			return err
		}
	}

	return nil
}

// name is the test's package and name, so tests of one name in two packages
// of the tree read apart. A summary without a package prints the test alone.
func (s StressSlowTestSummary) name() string {
	if s.Package == "" {
		return s.Test
	}

	return s.Package + " " + s.Test
}

func durationMSFromSeconds(seconds float64) int64 {
	return int64(seconds * millisecondsPerSecond)
}
