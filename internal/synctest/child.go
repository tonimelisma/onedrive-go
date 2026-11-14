package synctest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// childTestEnv names, in the environment of a child test binary RunInChild
// started, the test that child runs.
const childTestEnv = "ONEDRIVE_GO_CHILD_TEST"

// childTestTimeout bounds one child test binary, so a child that hangs fails
// its parent instead of holding the package's run to its deadline.
const childTestTimeout = 2 * time.Minute

// ChildResult is how a child test binary ended.
type ChildResult struct {
	ExitCode int
	Output   string
}

// InChild reports whether this process is the child test binary RunInChild
// started for tb's test.
func InChild(tb testing.TB) bool {
	tb.Helper()

	return os.Getenv(childTestEnv) == tb.Name()
}

// RunInChild runs tb's test again in a child test binary, in which InChild
// reports true, and returns how the child ended. A test whose subject may
// panic on a goroutine it does not own runs that subject in the child: when
// the panic escapes its boundary, the child dies with Go's exit status 2 and
// the parent test fails, instead of the panic ending every test of the
// package. A test that needs a process-wide setting the package's other
// tests must not see runs in a child for that reason too (RunUnderUmask).
func RunInChild(tb testing.TB) ChildResult {
	tb.Helper()

	return runInChild(tb)
}

// runInChild is RunInChild with more variables, each "NAME=value", added to
// the child's environment.
func runInChild(tb testing.TB, extraEnv ...string) ChildResult {
	tb.Helper()

	exe, err := os.Executable()
	require.NoError(tb, err)

	parts := strings.Split(tb.Name(), "/")
	for i := range parts {
		parts[i] = "^" + regexp.QuoteMeta(parts[i]) + "$"
	}

	ctx, cancel := context.WithTimeout(TestContext(tb), childTestTimeout)
	defer cancel()

	//nolint:gosec // The test binary runs itself with a pattern built from its own test name.
	cmd := exec.CommandContext(ctx, exe, "-test.run="+strings.Join(parts, "/"), "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childTestEnv+"="+tb.Name())
	cmd.Env = append(cmd.Env, extraEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		require.NoError(tb, runErr, "start the child test binary")
	}
	result := ChildResult{Output: output.String()}
	if exitErr != nil {
		result.ExitCode = exitErr.ExitCode()
	}

	return result
}

// RequireChildPassed fails tb unless its child test binary ran the test and
// passed: a child that died of a panic exits 2, one whose assertions failed
// exits 1.
func RequireChildPassed(tb testing.TB, result ChildResult) {
	tb.Helper()

	require.Equal(tb, 0, result.ExitCode, "the child test binary failed:\n%s", result.Output)
	require.Contains(tb, result.Output, "--- PASS: "+tb.Name()+" ", "the child test binary did not run the test:\n%s", result.Output)
}
