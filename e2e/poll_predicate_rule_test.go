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
// transient state into a test that did not run. A predicate a suite function
// builds and returns (dryRunPlanHasLineStarting) is read at its call as the
// literal the function returns, so the literal's failing call counts and the
// builder's own, which runs before the wait, does not. One kept in a variable
// is read where the variable is handed to a wait, as the literal or the
// builder's call the variable was set to. One a function hands on through a
// parameter (ready in requireSyncWithModeEventuallyConverges) is read at the
// call that gives it, since a function that hands a parameter on to a wait
// reaches the wait itself. Outside the scan stays a predicate that goes
// through a method or a struct field, which the suite has none of: telling
// which method a call reaches needs type information the scan does not build.
// A builder's predicate is read only when the builder returns a function
// literal directly (returnedLiterals); one that returns a variable or a named
// function stays outside too, and the suite's builders return literals.

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
			for _, predicate := range src.predicatesOf(passed) {
				if fails := predicate.failingCall(canFail); fails != "" {
					findings = append(findings, pollPredicateFinding{at: passed.at, in: name, wait: passed.to, fails: fails})
				}
			}
		}
	}

	return findings
}

// predicatesOf is what a wait runs as the predicate passed: passed itself,
// or for a suite function's call the literals that function returns.
func (src *suiteSources) predicatesOf(passed passedFunc) []passedFunc {
	if passed.built == "" {
		return []passedFunc{passed}
	}
	if builder, ok := src.funcs[passed.built]; ok {
		return builder.returned
	}

	return nil
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
		{at: "planted_test.go:30", in: "TestPlantedPredicates", wait: "waitForPlantedSize", fails: "require.True"},
		{at: "planted_test.go:32", in: "TestPlantedPredicates", wait: "forwardPlanted", fails: "require.True"},
	}, src.failingPollPredicates(),
		"a predicate that asserts itself, one that asserts through a helper, that helper by name, one that calls "+
			"t.Fatalf, one that skips, one that asserts two calls down inside a function that waits through another, "+
			"the asserting builder's predicate at its call, and the asserting predicate a variable hands on through a "+
			"parameter; not the one that reports, the two actions, the predicate of a function that does not wait, "+
			"the pure builder's (whose own assertion runs before the wait), or the pure forwarded one")
}
