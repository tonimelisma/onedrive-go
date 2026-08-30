package devtool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// noTestsRanMarker is what `go test` prints when a -run pattern matches
// nothing.
const noTestsRanMarker = "no tests to run"

// runGoTestMatching runs a targeted `go test` and fails when the -run pattern
// matched no tests.
//
// go test exits 0 when a pattern matches nothing, so a verification step whose
// target was renamed or deleted keeps reporting success while running nothing.
// That is how the stress profile's watch-ordering step passed for months
// against tests that do not exist. Every targeted step in this package routes
// through here so a silently-empty run is a failure, not a green check.
//
// Output is teed rather than captured so long live runs keep streaming.
func runGoTestMatching(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	stepDescription string,
	args ...string,
) error {
	var captured bytes.Buffer

	teedStdout := io.MultiWriter(stdout, &captured)
	teedStderr := io.MultiWriter(stderr, &captured)

	runErr := runner.Run(ctx, repoRoot, env, teedStdout, teedStderr, "go", args...)

	// The empty-run check comes first: a step that matched nothing is a
	// verification defect worth naming even if the command also failed.
	if matchErr := assertTestsMatched(captured.String(), stepDescription); matchErr != nil {
		return matchErr
	}

	if runErr != nil {
		return fmt.Errorf("%s: %w", stepDescription, runErr)
	}

	return nil
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
