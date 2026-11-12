package devtool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// noTestsRanMarker is what `go test` prints when a -run pattern matches
// nothing.
const noTestsRanMarker = "no tests to run"

// runGoTestMatching runs a targeted `go test` of one package and fails when
// the -run pattern matched no tests.
//
// go test exits 0 when a pattern matches nothing, so a verification step whose
// target was renamed or deleted keeps reporting success while running nothing.
// That is how the stress profile's watch-ordering step passed for months
// against tests that do not exist. Every targeted step in this package routes
// through here, or through runGoTestMatchingTree for a ./pkg/... target, so a
// silently-empty run is a failure, not a green check.
//
// Output is teed rather than captured so long live runs keep streaming, and
// returned too, for a caller that reads which tests failed or skipped.
func runGoTestMatching(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	stepDescription string,
	args ...string,
) (string, error) {
	return runGoTestCheckingMatch(ctx, runner, repoRoot, env, stdout, stderr, assertTestsMatched, stepDescription, args...)
}

// runGoTestMatchingTree is runGoTestMatching for a ./pkg/... target: a package
// of the tree without a matching test is expected, a run in which no package
// ran one is not.
func runGoTestMatchingTree(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	stepDescription string,
	args ...string,
) error {
	_, err := runGoTestCheckingMatch(
		ctx, runner, repoRoot, env, stdout, stderr, assertTestsMatchedInTree, stepDescription, args...,
	)

	return err
}

// runGoTestCheckingMatch runs go test with its output teed into a buffer that
// assertMatched then judges.
func runGoTestCheckingMatch(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	assertMatched func(output string, stepDescription string) error,
	stepDescription string,
	args ...string,
) (string, error) {
	output, runErr := runGoTestTeed(ctx, runner, repoRoot, env, stdout, stderr, stepDescription, args...)

	// The empty-run check comes first: a step that matched nothing is a
	// verification defect worth naming even if the command also failed.
	if matchErr := assertMatched(output, stepDescription); matchErr != nil {
		return output, matchErr
	}

	return output, runErr
}

// runGoTestTeed runs go test with its output streamed to stdout and stderr as
// it is printed, and captured for the caller to read once it exits. Streaming
// is the point on CI: a job that is killed keeps every line printed before.
func runGoTestTeed(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	stepDescription string,
	args ...string,
) (string, error) {
	var captured outputCapture

	runErr := runner.Run(
		ctx, repoRoot, env, io.MultiWriter(stdout, &captured), io.MultiWriter(stderr, &captured), "go", args...,
	)
	output := captured.String()
	if runErr != nil {
		return output, fmt.Errorf("%s: %w", stepDescription, runErr)
	}

	return output, nil
}

// outputCapture collects a command's stdout and stderr together. exec copies
// two different writers on two goroutines, so writes are serialized here.
type outputCapture struct {
	mu  sync.Mutex // guards buf
	buf bytes.Buffer
}

func (c *outputCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, err := c.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("capture command output: %w", err)
	}

	return n, nil
}

func (c *outputCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.buf.String()
}

// assertTestsMatched reports an error when go test output shows the run pattern
// selected no tests.
func assertTestsMatched(output string, stepDescription string) error {
	if !strings.Contains(output, noTestsRanMarker) {
		return nil
	}

	return fmt.Errorf(
		"%s matched no tests: the -run pattern selects nothing, so this step verified nothing",
		stepDescription,
	)
}

// assertTestsMatchedInTree reports an error when go test output over a package
// tree shows the marker and no package ran a selected test. The marker alone
// cannot decide it: over a tree, every package without a match prints it.
func assertTestsMatchedInTree(output string, stepDescription string) error {
	if !strings.Contains(output, noTestsRanMarker) {
		return nil
	}

	for line := range strings.Lines(output) {
		if lineShowsSelectedTest(line) {
			return nil
		}
	}

	return fmt.Errorf(
		"%s matched no tests in any package: the -run pattern selects nothing, so this step verified nothing",
		stepDescription,
	)
}

// lineShowsSelectedTest reports whether one line of go test output shows that
// the run pattern selected a test. In plain output that is a package result
// line without the marker (`ok` or `FAIL`), or a verbose test result; in -json
// output it is an event naming a test that passed, failed or skipped. A
// skipped test was still selected, and plain output cannot tell it apart.
func lineShowsSelectedTest(line string) bool {
	text, event, isEvent := decodeGoTestLogLine(line)
	if isEvent {
		switch event.Action {
		case goTestActionPass, goTestActionFail, goTestActionSkip:
			return event.Test != ""
		default:
			return false
		}
	}

	trimmed := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(text, rawGoTestPackagePassPrefix):
		return !strings.Contains(text, noTestsRanMarker)
	case strings.HasPrefix(text, rawGoTestPackageFailPrefix),
		strings.HasPrefix(trimmed, "--- PASS"),
		strings.HasPrefix(trimmed, rawGoTestFailurePrefix):
		return true
	default:
		return false
	}
}
