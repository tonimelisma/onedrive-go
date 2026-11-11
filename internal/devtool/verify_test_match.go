package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
	return runGoTestCheckingMatch(ctx, runner, repoRoot, env, stdout, stderr, assertTestsMatchedInTree, stepDescription, args...)
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
) error {
	var captured bytes.Buffer

	teedStdout := io.MultiWriter(stdout, &captured)
	teedStderr := io.MultiWriter(stderr, &captured)

	runErr := runner.Run(ctx, repoRoot, env, teedStdout, teedStderr, "go", args...)

	// The empty-run check comes first: a step that matched nothing is a
	// verification defect worth naming even if the command also failed.
	if matchErr := assertMatched(captured.String(), stepDescription); matchErr != nil {
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

// assertTestsMatchedInTree reports an error when go test output over a package
// tree shows the marker and no package ran a selected test. The marker alone
// cannot decide it: over a tree, every package without a match prints it.
func assertTestsMatchedInTree(output string, stepDescription string) error {
	if !strings.Contains(output, noTestsRanMarker) {
		return nil
	}

	for _, line := range strings.Split(output, "\n") {
		if lineShowsSelectedTest(strings.TrimSpace(line)) {
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
	if strings.HasPrefix(line, "{") {
		var event goTestJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return false
		}

		switch event.Action {
		case verifySummaryStatusPass, verifySummaryStatusFail, goTestActionSkip:
			return event.Test != ""
		default:
			return false
		}
	}

	switch {
	case strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "ok\t"):
		return !strings.Contains(line, noTestsRanMarker)
	case strings.HasPrefix(line, "FAIL\t"),
		strings.HasPrefix(line, "--- PASS"),
		strings.HasPrefix(line, "--- FAIL"):
		return true
	default:
		return false
	}
}
