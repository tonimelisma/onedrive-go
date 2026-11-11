package devtool

import (
	"context"
	"encoding/json"
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

// goTestActionSkip is the `go test -json` action of a test that skipped.
const goTestActionSkip = "skip"

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

// goTestJSONEvent matches the stable field names emitted by `go test -json`.
//
//nolint:tagliatelle // Go owns these capitalized JSON keys.
type goTestJSONEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
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

	for _, command := range stressCommands {
		if !command.collectSlowTests {
			if err := collector.runStep(command.stepName, func() error {
				return runStressCommand(ctx, runner, repoRoot, env, stdout, stderr, command)
			}); err != nil {
				return err
			}

			continue
		}

		if err := collector.runStepWithSlowTests(command.stepName, func() ([]StressSlowTestSummary, error) {
			return runStressCommandWithSlowTests(ctx, runner, repoRoot, env, stdout, command)
		}); err != nil {
			return err
		}
	}

	return nil
}

func runStressCommand(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	command stressCommandSpec,
) error {
	if err := writeStatus(stdout, command.statusLine); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	// A step with a -run pattern is a targeted step, and a pattern that
	// matches nothing anywhere in the tree must fail it: go test exits 0.
	var err error
	if hasRunPattern(command.args) {
		err = runGoTestMatchingTree(ctx, runner, repoRoot, env, stdout, stderr, command.stepName, command.args...)
	} else {
		err = runner.Run(ctx, repoRoot, env, stdout, stderr, "go", command.args...)
	}
	if err != nil {
		return fmt.Errorf("stress tests: %w", err)
	}

	return nil
}

// hasRunPattern reports whether go test arguments select tests with -run.
func hasRunPattern(args []string) bool {
	for _, arg := range args {
		if arg == "-run" || strings.HasPrefix(arg, "-run=") {
			return true
		}
	}

	return false
}

func runStressCommandWithSlowTests(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout io.Writer,
	command stressCommandSpec,
) ([]StressSlowTestSummary, error) {
	if err := writeStatus(stdout, command.statusLine); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	output, err := runner.CombinedOutput(ctx, repoRoot, env, "go", command.args...)
	if writeErr := writeCommandOutput(stdout, output); writeErr != nil {
		return nil, fmt.Errorf("write stress output: %w", writeErr)
	}

	slowTests := summarizeStressSlowTests(output, stressSlowTestLimit)
	if len(slowTests) > 0 {
		if writeErr := writeStressSlowTestSummary(stdout, slowTests); writeErr != nil {
			return slowTests, fmt.Errorf("write stress timing summary: %w", writeErr)
		}
	}

	if err != nil {
		return slowTests, fmt.Errorf("stress tests: %w", err)
	}

	return slowTests, nil
}

// summarizeStressSlowTests totals the completed tests of `go test -json`
// output by package and test, slowest first.
func summarizeStressSlowTests(output []byte, limit int) []StressSlowTestSummary {
	totals := make(map[stressTestKey]*stressTestTiming)

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event goTestJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Test == "" {
			continue
		}
		if event.Action != verifySummaryStatusPass && event.Action != verifySummaryStatusFail {
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
