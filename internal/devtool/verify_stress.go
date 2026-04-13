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
	stressCommands := []stressCommandSpec{
		{
			stepName: "watch-ordering stress",
			statusLine: fmt.Sprintf(
				"==> go test -tags=stress -race -count=50 -timeout=%s "+
					"-run TestWatchOrderingStress_ ./internal/sync\n",
				stressWatchOrderingTimeout,
			),
			args: []string{
				"test",
				"-tags=stress",
				"-race",
				"-count=50",
				"-timeout=" + stressWatchOrderingTimeout,
				"-run", "TestWatchOrderingStress_",
				"./internal/sync",
			},
		},
		{
			stepName: "multisync race x50",
			statusLine: fmt.Sprintf(
				"==> go test -race -count=50 -timeout=%s ./internal/multisync\n",
				stressMultisyncTimeout,
			),
			args: []string{
				"test",
				"-race",
				"-count=50",
				"-timeout=" + stressMultisyncTimeout,
				"./internal/multisync",
			},
		},
		{
			stepName: "sync race x50",
			statusLine: fmt.Sprintf(
				"==> go test -json -race -count=50 -timeout=%s ./internal/sync\n",
				stressSyncTimeout,
			),
			args: []string{
				"test",
				"-json",
				"-race",
				"-count=50",
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
