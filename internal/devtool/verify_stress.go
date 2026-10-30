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

// stressSyncConcurrencyPattern selects the internal/sync tests that exercise
// goroutines, timers, cancellation and shutdown: the watch runtime and
// observers, the one-shot result loop, the worker pool and the concurrent
// store and graph tests.
const stressSyncConcurrencyPattern = "TestRunWatch|TestWatch|TestOneShot|TestWorkerPool|Shutdown|Drain|Concurren|Cancel"

type stressCommandSpec struct {
	stepName         string
	statusLine       string
	args             []string
	collectSlowTests bool
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
	// Repetition goes where the concurrency is. One race iteration of all of
	// internal/sync takes about six minutes on a CI runner, so fifty no longer
	// fit any budget; its watch, runtime, shutdown and worker tests (where the
	// 1-in-50 flakes have come from) take well under a minute and still run
	// fifty times. Every step shuffles test order, so each repetition also
	// varies what runs beside what.
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
				"==> go test -race -count=50 -shuffle=on -timeout=%s -run %s ./internal/sync\n",
				stressSyncConcurrencyTimeout, stressSyncConcurrencyPattern,
			),
			args: []string{
				"test",
				"-race",
				"-count=50",
				"-shuffle=on",
				"-timeout=" + stressSyncConcurrencyTimeout,
				"-run", stressSyncConcurrencyPattern,
				"./internal/sync",
			},
		},
		{
			stepName: "sync race x3",
			statusLine: fmt.Sprintf(
				"==> go test -json -race -count=3 -shuffle=on -timeout=%s ./internal/sync\n",
				stressSyncTimeout,
			),
			args: []string{
				"test",
				"-json",
				"-race",
				"-count=3",
				"-shuffle=on",
				"-timeout=" + stressSyncTimeout,
				"./internal/sync",
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
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", command.args...); err != nil {
		return fmt.Errorf("stress tests: %w", err)
	}

	return nil
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

func summarizeStressSlowTests(output []byte, limit int) []StressSlowTestSummary {
	totals := make(map[string]*stressTestTiming)

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
		timing := totals[event.Test]
		if timing == nil {
			timing = &stressTestTiming{}
			totals[event.Test] = timing
		}

		timing.runs++
		timing.totalElapsedMS += elapsedMS
		if elapsedMS > timing.maxElapsedMS {
			timing.maxElapsedMS = elapsedMS
		}
	}

	summaries := make([]StressSlowTestSummary, 0, len(totals))
	for test, timing := range totals {
		summaries = append(summaries, StressSlowTestSummary{
			Test:           test,
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
				slow.Test,
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

func durationMSFromSeconds(seconds float64) int64 {
	return int64(seconds * millisecondsPerSecond)
}
