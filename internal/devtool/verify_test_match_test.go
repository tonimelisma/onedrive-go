package devtool

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.1
//
// go test exits 0 when a -run pattern matches nothing, so a targeted step whose
// target was renamed away keeps reporting success while verifying nothing. That
// is exactly how the stress profile's watch-ordering step stayed green against
// tests that do not exist.
func TestAssertTestsMatched_RejectsEmptyRun(t *testing.T) {
	t.Parallel()

	err := assertTestsMatched("testing: warning: no tests to run\nPASS\nok  \tpkg\t0.1s\n", "watch-ordering stress")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch-ordering stress matched no tests")
}

// Validates: R-6.2.1
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

// Validates: R-6.2.1
func TestAssertTestsMatched_AcceptsRealRun(t *testing.T) {
	t.Parallel()

	require.NoError(t, assertTestsMatched("--- PASS: TestSomething (0.01s)\nPASS\nok\tpkg\t0.2s\n", "step"))
}
