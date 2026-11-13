package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const millisecondsPerSecond = 1000

// stressSyncTree is the stress lane's sync target: the whole tree, not the one
// directory, so a package split out of internal/sync stays under stress
// instead of silently leaving the lane.
const stressSyncTree = "./internal/sync/..."

// stressSyncConcurrencyPattern selects the sync tests that exercise
// goroutines, timers, cancellation and shutdown: the watch runtime and
// observers, the one-shot result loop, the worker pool and the concurrent
// store and graph tests. Over the tree, a package with none of them is
// expected; the step fails only when no package has one.
const stressSyncConcurrencyPattern = "TestRunWatch|TestWatch|TestOneShot|TestWorkerPool|Shutdown|Drain|Concurren|Cancel"

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
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	// Repetition goes where the concurrency is. One race iteration of the
	// whole sync tree takes about six minutes on a CI runner, so fifty no
	// longer fit any budget; its watch, runtime, shutdown and worker tests
	// (where the 1-in-50 flakes have come from) take well under a minute and
	// still run fifty times. Every step shuffles test order, so each
	// repetition also varies what runs beside what.
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
				stressSyncConcurrencyTimeout, stressSyncConcurrencyPattern, stressSyncTree,
			),
			args: []string{
				"test",
				"-race",
				"-count=50",
				"-shuffle=on",
				"-timeout=" + stressSyncConcurrencyTimeout,
				"-run", stressSyncConcurrencyPattern,
				stressSyncTree,
			},
		},
		{
			stepName: "sync race x3",
			statusLine: fmt.Sprintf(
				"==> go test -json -race -count=3 -shuffle=on -timeout=%s %s\n",
				stressSyncTimeout, stressSyncTree,
			),
			args: []string{
				"test",
				"-json",
				"-race",
				"-count=3",
				"-shuffle=on",
				"-timeout=" + stressSyncTimeout,
				stressSyncTree,
			},
			collectSlowTests: true,
		},
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
