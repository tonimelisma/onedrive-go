package devtool

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.1
//
// go test exits 0 when a -run pattern matches nothing, so a targeted step whose
// target was renamed away keeps reporting success while verifying nothing. That
// is exactly how the stress profile's watch-ordering step stayed green against
// tests that do not exist.
func TestAssertTestsMatched_RejectsEmptyRun(t *testing.T) {
	t.Parallel()

	err := assertTestsMatched("testing: warning: no tests to run\nPASS\nok  \tpkg\t0.1s [no tests to run]\n", "watch-ordering stress")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch-ordering stress matched no tests")
}

// Validates: R-6.10.1
//
// The marker also has to be caught inside `go test -json` output, which is how
// the e2e buckets run.
func TestAssertTestsMatched_RejectsEmptyRunInJSONOutput(t *testing.T) {
	t.Parallel()

	err := assertTestsMatched(
		`{"Action":"output","Output":"testing: warning: no tests to run\n"}`,
		"full e2e bucket",
	)
	require.Error(t, err)
}

// Validates: R-6.10.1
func TestAssertTestsMatched_AcceptsRealRun(t *testing.T) {
	t.Parallel()

	require.NoError(t, assertTestsMatched("--- PASS: TestSomething (0.01s)\nPASS\nok\tpkg\t0.2s\n", "step"))
}

// Validates: R-6.10.1, R-6.10.8
//
// A -run pattern over a ./pkg/... tree matches in some packages and not in
// others. A package without a match is expected and must not fail the step,
// or the stress lane could never target the sync tree.
func TestAssertTestsMatched_AcceptsTreeWithSomeEmptyPackages(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync\t41.2s",
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync/planner\t0.1s [no tests to run]",
		"?   \tgithub.com/tonimelisma/onedrive-go/internal/sync/testdata\t[no test files]",
	}, "\n")

	require.NoError(t, assertTestsMatched(output, "sync concurrency race x50"))
}

// Validates: R-6.10.1, R-6.10.8
//
// When every package of the tree reports that the pattern selected nothing,
// the step verified nothing anywhere and fails, like a single-package step.
func TestAssertTestsMatched_RejectsTreeWhereNothingRan(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync\t0.3s [no tests to run]",
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync/planner\t0.1s [no tests to run]",
		"?   \tgithub.com/tonimelisma/onedrive-go/internal/sync/testdata\t[no test files]",
	}, "\n")

	err := assertTestsMatched(output, "sync concurrency race x50")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync concurrency race x50 matched no tests")
}

// Validates: R-6.10.1, R-6.10.8, R-6.10.26
//
// A package without test files prints `[no test files]`, not the marker, and
// go test exits 0 over a tree of them. A guard keyed on the marker passed such
// a step although it ran nothing, and so did one handed no output at all.
func TestAssertTestsMatched_RejectsATreeWithNoTestFiles(t *testing.T) {
	t.Parallel()

	const syncPackage = "github.com/tonimelisma/onedrive-go/internal/sync"
	const plannerPackage = syncPackage + "/planner"
	cases := []struct {
		name   string
		output string
	}{
		{
			name: "plain output",
			output: "?   \t" + syncPackage + "\t[no test files]\n" +
				"?   \t" + plannerPackage + "\t[no test files]\n",
		},
		{
			name: "json output",
			output: strings.Join([]string{
				`{"Action":"start","Package":"` + syncPackage + `"}`,
				`{"Action":"output","Package":"` + syncPackage + `","Output":"?   \t` + syncPackage + `\t[no test files]\n"}`,
				`{"Action":"skip","Package":"` + syncPackage + `","Elapsed":0}`,
			}, "\n"),
		},
		{name: "no output", output: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorContains(t, assertTestsMatched(tc.output, "sync race x3"), "sync race x3 matched no tests")
		})
	}
}

// Validates: R-6.10.1, R-6.10.8
//
// Verbose output names each test it ran, and a package that failed ran tests
// too; either is a match even when another package printed the marker.
func TestAssertTestsMatched_AcceptsVerboseAndFailedPackages(t *testing.T) {
	t.Parallel()

	verbose := strings.Join([]string{
		"=== RUN   TestRunWatch_Stops",
		"--- PASS: TestRunWatch_Stops (0.02s)",
		"PASS",
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync\t0.3s",
		"testing: warning: no tests to run",
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync/planner\t0.1s [no tests to run]",
	}, "\n")
	require.NoError(t, assertTestsMatched(verbose, "step"))

	failed := strings.Join([]string{
		"--- FAIL: TestRunWatch_Stops (0.02s)",
		"FAIL\tgithub.com/tonimelisma/onedrive-go/internal/sync\t0.3s",
		"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync/planner\t0.1s [no tests to run]",
	}, "\n")
	require.NoError(t, assertTestsMatched(failed, "step"))
}

// Validates: R-6.10.1, R-6.10.26
//
// A package whose test binary was never built or set up ran no test, though
// its result line starts with FAIL like one whose tests failed. Such a run is
// judged as one that ran nothing, as it is in -json output.
func TestAssertTestsMatched_ABinaryThatNeverRanRanNoTest(t *testing.T) {
	t.Parallel()

	for _, suffix := range []string{rawGoTestBuildFailedSuffix, rawGoTestSetupFailedSuffix} {
		output := "e2e/helpers.go:12:2: undefined: missingHelper\nFAIL\t" + testE2EPackage + suffix + "\n"
		require.ErrorContains(t, assertTestsMatched(output, "step"), "step matched no tests", "suffix %q", suffix)
	}
}

// Validates: R-6.10.1, R-6.10.8
//
// In `go test -json` output the marker sits inside output events and a test
// that ran is an event naming it, so a tree step run with -json is judged by
// the same rule.
func TestAssertTestsMatched_HandlesJSONOutput(t *testing.T) {
	t.Parallel()

	matched := strings.Join([]string{
		`{"Action":"run","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestRunWatch_Stops"}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestRunWatch_Stops","Elapsed":0.02}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Elapsed":0.3}`,
		`{"Action":"output","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Output":"testing: warning: no tests to run\n"}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Elapsed":0.1}`,
	}, "\n")
	require.NoError(t, assertTestsMatched(matched, "step"))

	// A selected test that skipped was still matched, as in plain output,
	// where its package prints `ok` without the marker.
	skipped := strings.Join([]string{
		`{"Action":"skip","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestRunWatch_Stops","Elapsed":0}`,
		`{"Action":"output","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Output":"testing: warning: no tests to run\n"}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Elapsed":0.1}`,
	}, "\n")
	require.NoError(t, assertTestsMatched(skipped, "step"))

	nothing := strings.Join([]string{
		`{"Action":"output","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Output":"testing: warning: no tests to run\n"}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Elapsed":0.3}`,
		`{"Action":"output","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Output":"testing: warning: no tests to run\n"}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Elapsed":0.1}`,
	}, "\n")
	err := assertTestsMatched(nothing, "step")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "step matched no tests")
}
