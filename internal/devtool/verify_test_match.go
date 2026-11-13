package devtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	// noTestsRanMarker is what `go test` prints when a -run pattern matches
	// nothing in a package.
	noTestsRanMarker = "no tests to run"

	// rawGoTestBuildFailedSuffix and rawGoTestSetupFailedSuffix end the FAIL
	// result line of a package whose test binary never ran.
	rawGoTestBuildFailedSuffix = " [build failed]"
	rawGoTestSetupFailedSuffix = " [setup failed]"
)

// runGoTestMatching runs a go test step and fails it when no package of its
// target ran a test.
//
// go test exits 0 when a -run pattern matches nothing and when the packages of
// its target have no test files, so a step whose tests were renamed, deleted or
// moved keeps reporting success while verifying nothing. That is how the
// stress profile's watch-ordering step passed for months against tests that do
// not exist. Every stress step, preflight, classified rerun and failed-test
// rerun routes through here. The rule is the same for one package and for a
// ./pkg/... tree: a package of the tree without a selected test is expected, a
// run in which no package ran one is not.
//
// Output is teed rather than captured so long runs keep streaming, and
// returned too, for a caller that reads which tests failed, skipped or took
// longest.
func runGoTestMatching(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	stepDescription string,
	args ...string,
) (string, error) {
	output, runErr := runGoTeed(ctx, runner, repoRoot, env, stdout, stderr, stepDescription, args...)

	// The empty-run finding comes first: a step that ran nothing is a
	// verification defect worth naming even when the command also failed,
	// whose own error follows it.
	if matchErr := assertTestsMatched(output, stepDescription); matchErr != nil {
		return output, errors.Join(matchErr, runErr)
	}

	return output, runErr
}

// runGoTeed runs a go command with its output streamed to stdout and stderr as
// it is printed, and captured for the caller to read once it exits. Streaming
// is the point on CI: a job that is killed keeps every line printed before.
func runGoTeed(
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

// assertTestsMatched reports an error when go test output shows no package
// running a selected test: a -run pattern that matches nothing, packages
// without test files, or a run that ended before its first test. Over a tree,
// every package without a match prints the marker, so the marker alone
// decides nothing; a line that shows a test is what counts.
func assertTestsMatched(output string, stepDescription string) error {
	for line := range strings.Lines(output) {
		if lineShowsSelectedTest(line) {
			return nil
		}
	}

	return fmt.Errorf(
		"%s matched no tests: no package of its target ran a test (a -run pattern that selects nothing, "+
			"packages without test files, or a run that ended before its first test), so this step verified nothing",
		stepDescription,
	)
}

// lineShowsSelectedTest reports whether one line of go test output shows that
// a test was selected and ran. In plain output that is a package result line
// without the marker (`ok`, or `FAIL` for a binary that ran), or a verbose
// test result; in -json output it is an event naming a test that passed,
// failed or skipped. A skipped test was still selected, and plain output
// cannot tell it apart. A package without test files prints neither.
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
	case strings.HasPrefix(text, rawGoTestPackageFailPrefix):
		return !strings.HasSuffix(trimmed, rawGoTestBuildFailedSuffix) &&
			!strings.HasSuffix(trimmed, rawGoTestSetupFailedSuffix)
	case strings.HasPrefix(trimmed, rawGoTestPassPrefix),
		strings.HasPrefix(trimmed, rawGoTestFailurePrefix):
		return true
	default:
		return false
	}
}
