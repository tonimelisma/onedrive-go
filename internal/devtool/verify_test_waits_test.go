package devtool

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// literalWaitFixture is a test file of internal/sync whose waits are bounded
// every way the rule reads, through aliased imports of time and testify: each
// literal bound is refused, and each named bound, tick and non-wait use of a
// unit is not.
const literalWaitFixture = `package sync

import (
	"context"
	"net"
	"testing"
	tm "time"

	is "github.com/stretchr/testify/assert"
	must "github.com/stretchr/testify/require"
)

const eventWait = tm.Minute

func TestLiteralWaits(t *testing.T) {
	done := make(chan struct{})
	select {
	case <-done:
	case <-tm.After(5 * tm.Second):
	}
	ctx, cancel := context.WithTimeout(t.Context(), tm.Second)
	defer cancel()
	var conn net.Conn
	_ = conn.SetReadDeadline(tm.Now().Add(2 * tm.Second))
	must.Eventually(t, func() bool { return ctx != nil }, 10*tm.Second, tm.Millisecond)
	is.New(t).Eventually(func() bool { return true }, tm.Minute, tm.Millisecond)
}

func TestNamedWaits(t *testing.T) {
	done := make(chan struct{})
	select {
	case <-done:
	case <-tm.After(eventWait):
	}
	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()
	var conn net.Conn
	_ = conn.SetReadDeadline(tm.Now().Add(eventWait))
	must.Eventually(t, func() bool { return ctx != nil }, eventWait, 10*tm.Millisecond)
	_ = 3 * tm.Second
}
`

// Validates: R-6.10.17
//
// In the packages whose tests wait on the sync engine's events, a wait bound
// written with the time package's units is refused as the bound of
// time.After, context.WithTimeout, SetReadDeadline and testify's Eventually,
// in its package form and as an Assertions method, also when time and
// testify are imported under other names. A named bound, the tick of
// Eventually and a duration that bounds no wait are not, and neither is a
// literal wait outside those packages or in a file that is not a test.
func TestRunTestConventionsRefusesALiteralWaitBound(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(filepath.Join(repoRoot, "internal", "sync", "waits_test.go"), []byte(literalWaitFixture)))
	require.NoError(t, writeFile(filepath.Join(repoRoot, "internal", "graph", "waits_test.go"),
		[]byte(stringsReplacePackage(literalWaitFixture, "graph"))))
	require.NoError(t, writeFile(filepath.Join(repoRoot, "internal", "cli", "waits.go"),
		[]byte(stringsReplacePackage(literalWaitFixture, "cli"))))

	violations, err := findTestConventionViolations(repoRoot)
	require.NoError(t, err)

	got := map[int]string{}
	for _, violation := range violations {
		assert.Equal(t, "internal/sync/waits_test.go", violation.Path, "only the sync test file is read")
		got[violation.Line] = violation.Message
	}
	want := map[int]string{
		19: "time.After",
		21: "context.WithTimeout",
		24: "SetReadDeadline",
		25: "Eventually",
		26: "Eventually",
	}
	require.Len(t, got, len(want), "%v", got)
	for line, call := range want {
		assert.Contains(t, got[line], call+" waits on a literal duration", "line %d", line)
	}

	err = runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/sync/waits_test.go:19: time.After waits on a literal duration")
}

// stringsReplacePackage gives the fixture another package clause.
func stringsReplacePackage(source string, pkg string) string {
	return "package " + pkg + source[len("package sync"):]
}

// Validates: R-6.10.17
//
// Each declared exception names a test file of the rule's packages that
// still has a literal wait of the call it names, so an exception cannot
// outlive its reason, and every exception states one.
func TestTestWaitExceptionsNameLiteralWaitsOfTheTree(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootForTest(t)
	for _, exception := range testWaitExceptions() {
		assert.NotEmpty(t, exception.reason, exception.path)
		require.True(t, isTestWaitRulePath(exception.path), "%s is a test file the rule reads", exception.path)

		source, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(exception.path)))
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), exception.path, source, parser.SkipObjectResolution)
		require.NoError(t, err)

		calls := []string{}
		for _, site := range literalWaitSites(file) {
			calls = append(calls, site.call)
		}
		assert.Contains(t, calls, exception.call, "%s still waits on a literal through %s", exception.path, exception.call)
	}
}
