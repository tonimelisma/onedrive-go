package devtool

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// verifyProfileUsage lists every verify profile for the usage line.
const verifyProfileUsage = "default|public|quick|lint|platform|e2e|e2e-full|integration|stress|failed-test"

// shortUnitTestTimeout is go test's per-package deadline in the short unit
// tier: no race detector and no exhaustive tests, so a package that takes
// ten minutes is hung, not slow.
const shortUnitTestTimeout = "10m"

// verifyShard is one slice of the short unit tier, `--shard index/count`.
// A zero value means the whole tier.
type verifyShard struct {
	index int
	count int
}

func (s verifyShard) enabled() bool {
	return s.count > 1
}

// runPattern is the -run regexp selecting this shard's tests: the top-level
// tests whose name length is congruent to index-1 modulo count. Name length
// is a stable hash that needs no test list, spreads every family (the
// hundred-odd TestRunOnce_ scenarios included) across the shards, and keeps
// the pattern short however many tests exist. The patterns for one count
// partition every name, so running all shards runs every test once
// (TestVerifyShardPatternsPartitionEveryName). The pattern has no "/", so it
// never filters subtests.
func (s verifyShard) runPattern() string {
	remainder := s.index - 1
	pattern := "^(.{" + strconv.Itoa(s.count) + "})*"
	if remainder > 0 {
		pattern += ".{" + strconv.Itoa(remainder) + "}"
	}

	return pattern + "$"
}

func parseVerifyShard(spec string) (verifyShard, error) {
	index, count, ok := strings.Cut(spec, "/")
	if !ok {
		return verifyShard{}, fmt.Errorf("parse --shard %q: want index/count, for example 1/2", spec)
	}
	i, err := strconv.Atoi(index)
	if err != nil {
		return verifyShard{}, fmt.Errorf("parse --shard %q: index: %w", spec, err)
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return verifyShard{}, fmt.Errorf("parse --shard %q: count: %w", spec, err)
	}
	if n < 1 || i < 1 || i > n {
		return verifyShard{}, fmt.Errorf("parse --shard %q: index must be between 1 and count", spec)
	}

	return verifyShard{index: i, count: n}, nil
}

func resolveVerifyShard(plan verifyPlan, spec string) (verifyShard, error) {
	if spec == "" {
		return verifyShard{}, nil
	}
	if !plan.runQuickChecks && !plan.runPlatformChecks {
		return verifyShard{}, fmt.Errorf("--shard applies to the quick and platform profiles only")
	}

	return parseVerifyShard(spec)
}

// runPRLaneVerification runs the per-PR CI profiles.
//
//   - quick: format, the repository rules, the host and cross-platform builds,
//     the tagged vets, then the short unit tier. No lint (its own CI job), no
//     race detector, no coverage, no live E2E.
//   - lint: golangci-lint alone, so CI runs it beside the test shards instead
//     of in front of them.
//   - platform: the host build and the short unit tier, nothing else: the
//     secondary-OS leg exists to catch darwin-only build and behavior
//     breakage, and the repository rules are platform-independent.
//
// The race detector, the coverage gate, the compile guard and the exhaustive
// tests stay in default and public, which the local gate and nightly CI run.
func runPRLaneVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	plan verifyPlan,
	shard verifyShard,
) error {
	var steps []verifyStep
	switch {
	case plan.runQuickChecks:
		steps = []verifyStep{{"format", runFormat}}
		steps = append(steps, repoRuleSteps()...)
		steps = append(steps, verifyStep{"build", runBuild}, verifyStep{"tagged vet", runTaggedVet})
	case plan.runLint:
		steps = []verifyStep{{"lint", runLint}}
	case plan.runPlatformChecks:
		steps = []verifyStep{{"build", runHostBuild}}
	default:
		return nil
	}
	if err := runVerifySteps(ctx, runner, repoRoot, env, collector, stdout, stderr, steps); err != nil {
		return err
	}
	if plan.runLint {
		return nil
	}

	return collector.runStep("short unit tests", func() error {
		return runShortUnitTests(ctx, runner, repoRoot, env, shard, stdout, stderr)
	})
}

func runHostBuild(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go build\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", "build", "./..."); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	return nil
}

// runShortUnitTests runs the short unit tier: `go test -short` without the
// race detector or a coverage profile, optionally one shard of it. -short
// skips the tests marked exhaustive (grep `exhaustive: runs in the full
// verify profiles`), which are breadth, replay and randomized variants of
// behavior the targeted tests cover; nothing else changes.
func runShortUnitTests(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	shard verifyShard,
	stdout, stderr io.Writer,
) error {
	args := []string{"test", "-short", "-timeout=" + shortUnitTestTimeout}
	status := "==> go test -short"
	if shard.enabled() {
		args = append(args, "-run="+shard.runPattern())
		status += fmt.Sprintf(" (shard %d/%d)", shard.index, shard.count)
	}
	args = append(args, "./...")
	if err := writeStatus(stdout, status+"\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", args...); err != nil {
		return fmt.Errorf("short unit tests: %w", err)
	}

	return nil
}
