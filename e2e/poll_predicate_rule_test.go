package e2e

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file holds a second rule of the live suite from its sources, read by
// the same scan as daemon_wait_rule_test.go: a poll predicate reports what it
// saw and never fails the test. A predicate that fails the test turns one
// transient state into a failure instead of waiting through it, and testify's
// Eventually runs its condition on another goroutine, where a failing call
// cannot stop the test at all. The test asserts after the wait.
//
// A predicate is a function literal with a result, or a suite function with a
// result passed by name, given to a poll primitive (suitePollPrimitives,
// testify's Eventually and Never families) or to a suite function that
// reaches one. It fails the test when it calls a testify assertion on t or a
// method of t that fails or skips the test (testEndingMethods), directly or
// through the suite's functions to any depth; a skip counts because it turns a
// transient state into a test that did not run. Two shapes are outside the
// scan: a predicate a suite function builds and returns
// (dryRunPlanHasLineStarting), and one a function forwards from a variable or
// a parameter instead of writing it at the call. No predicate of the suite
// goes through a method or a struct field.

// pollPredicateFinding is a predicate given to a wait that can fail the test.
type pollPredicateFinding struct {
	// at is the predicate's file and line.
	at string
	// in is the suite function whose body passes it.
	in string
	// wait is the poll primitive or the waiting suite function it is given to.
	wait string
	// fails is the call in it that can fail the test: an assertion, or the
	// suite function that makes one.
	fails string
}

func (finding pollPredicateFinding) message() string {
	return finding.at + ": the predicate " + finding.in + " passes to " + finding.wait + " calls " + finding.fails +
		", which can fail the test: return what it saw (a bool, or an error the wait carries) and assert after the wait"
}

// failingPollPredicates lists, in function name and source order, the
// predicates of src that can fail the test.
func (src *suiteSources) failingPollPredicates() []pollPredicateFinding {
	waiting := src.reachingFuncs(func(fn *suiteFunc) bool { return fn.polls })
	canFail := src.reachingFuncs(func(fn *suiteFunc) bool { return fn.fails != "" })

	var findings []pollPredicateFinding
	for _, name := range slices.Sorted(maps.Keys(src.funcs)) {
		for _, passed := range src.funcs[name].passed {
			if !passed.toPrimitive && !waiting[passed.to] {
				continue
			}
			if passed.named != "" && !src.funcs[passed.named].returnsValue {
				continue
			}
			if fails := passed.failingCall(canFail); fails != "" {
				findings = append(findings, pollPredicateFinding{at: passed.at, in: name, wait: passed.to, fails: fails})
			}
		}
	}

	return findings
}

// failingCall names the call in passed that can fail the test: its own
// assertion, or else the first suite function it calls that can.
func (passed passedFunc) failingCall(canFail map[string]bool) string {
	if passed.fails != "" {
		return passed.fails
	}
	if index := slices.IndexFunc(passed.refs, func(ref string) bool { return canFail[ref] }); index >= 0 {
		return passed.refs[index]
	}

	return ""
}

// reachingFuncs is the set of suite functions of which holds is true, or that
// reach one through the suite's functions to any depth.
func (src *suiteSources) reachingFuncs(holds func(*suiteFunc) bool) map[string]bool {
	reached := map[string]bool{}
	for name, fn := range src.funcs {
		if holds(fn) {
			reached[name] = true
		}
	}

	for isGrowing := true; isGrowing; {
		isGrowing = false
		for name, fn := range src.funcs {
			if !reached[name] && slices.ContainsFunc(fn.refs, func(ref string) bool { return reached[ref] }) {
				reached[name] = true
				isGrowing = true
			}
		}
	}

	return reached
}

// Validates: R-6.10.17
func TestE2EPollPredicatesAssertNothing(t *testing.T) {
	t.Parallel()

	src, err := readSuiteSources(".")
	require.NoError(t, err)

	for _, finding := range src.failingPollPredicates() {
		assert.Fail(t, finding.message())
	}
}

// Validates: R-6.10.17
func TestPollPredicateScanReportsFailingPredicates(t *testing.T) {
	t.Parallel()

	src := readPlantedSuiteSources(t)

	assert.Equal(t, []pollPredicateFinding{
		{at: "planted_test.go:21", in: "TestPlantedPredicates", wait: "assert.Eventually", fails: "require.NoError"},
		{at: "planted_test.go:22", in: "TestPlantedPredicates", wait: "waitForPlantedSize", fails: "requirePlantedState"},
		{at: "planted_test.go:23", in: "TestPlantedPredicates", wait: "waitForPlantedSize", fails: "requirePlantedState"},
		{at: "planted_test.go:26", in: "TestPlantedPredicates", wait: "waitForPlantedSize", fails: "t.Fatalf"},
		{at: "planted_test.go:27", in: "TestPlantedPredicates", wait: "waitForPlantedSize", fails: "t.Skip"},
		{at: "planted_test.go:28", in: "TestPlantedPredicates", wait: "readPlanted", fails: "plantedStateHolds"},
	}, src.failingPollPredicates(),
		"a predicate that asserts itself, one that asserts through a helper, that helper by name, one that calls "+
			"t.Fatalf, one that skips, and one that asserts two calls down inside a function that waits through another; "+
			"not the one that reports, the two actions, or the predicate of a function that does not wait")
}
