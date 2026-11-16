//go:build e2e

package e2e

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// printable is a command's output, or any text that may name a test account,
// as a failure message or a test log prints it: every account, drive, item
// and fixture of the suite named by role, the rest of an identifier masked,
// and cut to its last printedOutputLimit bytes (printableWith).
func printable(text string) string {
	return printableWith(suiteDriveRoles, text)
}

// assertOutputContains is assert.Contains for a command's output: a failure
// prints the output masked and bounded, never whole.
func assertOutputContains(t *testing.T, output, want string, msgAndArgs ...any) {
	t.Helper()

	if !strings.Contains(output, want) {
		assert.Fail(t, outputAssertionFailure(suiteDriveRoles, output, want, true), msgAndArgs...)
	}
}

// assertOutputNotContains is assert.NotContains for a command's output.
func assertOutputNotContains(t *testing.T, output, unwanted string, msgAndArgs ...any) {
	t.Helper()

	if strings.Contains(output, unwanted) {
		assert.Fail(t, outputAssertionFailure(suiteDriveRoles, output, unwanted, false), msgAndArgs...)
	}
}

// assertOutputEqual is assert.Equal for a value a command's output gave: a
// failure prints both values masked and bounded.
func assertOutputEqual(t *testing.T, want, got string, msgAndArgs ...any) {
	t.Helper()

	if got != want {
		assert.Fail(t, outputMismatch(suiteDriveRoles, want, got), msgAndArgs...)
	}
}

// requireOutputContains is require.Contains for a command's output.
func requireOutputContains(t *testing.T, output, want string, msgAndArgs ...any) {
	t.Helper()

	if !strings.Contains(output, want) {
		require.FailNow(t, outputAssertionFailure(suiteDriveRoles, output, want, true), msgAndArgs...)
	}
}

// assertOutputMatches is assert.Regexp for a command's output.
func assertOutputMatches(t *testing.T, pattern *regexp.Regexp, output string, msgAndArgs ...any) {
	t.Helper()

	if !pattern.MatchString(output) {
		assert.Fail(t, outputMatchFailure(pattern, output, true), msgAndArgs...)
	}
}

// requireOutputMatches is require.Regexp for a command's output.
func requireOutputMatches(t *testing.T, pattern *regexp.Regexp, output string, msgAndArgs ...any) {
	t.Helper()

	if !pattern.MatchString(output) {
		require.FailNow(t, outputMatchFailure(pattern, output, true), msgAndArgs...)
	}
}

// assertOutputDoesNotMatch is assert.NotRegexp for a command's output.
func assertOutputDoesNotMatch(t *testing.T, pattern *regexp.Regexp, output string, msgAndArgs ...any) {
	t.Helper()

	if pattern.MatchString(output) {
		assert.Fail(t, outputMatchFailure(pattern, output, false), msgAndArgs...)
	}
}

func outputMatchFailure(pattern *regexp.Regexp, output string, wantMatch bool) string {
	verb := "does not match"
	if !wantMatch {
		verb = "matches"
	}

	return "the output " + verb + " " + pattern.String() + "\noutput: " + printable(output)
}

// localPathKind is what assertLocalPath expects a local path to be.
type localPathKind string

const (
	localFolder   localPathKind = "a folder"
	localFile     localPathKind = "a file"
	localNoFolder localPathKind = "no folder"
)

// assertLocalPath is assert.DirExists, FileExists or NoDirExists for a local
// path that may hold a fixture's name: a failure prints the path by role.
func assertLocalPath(t *testing.T, path string, want localPathKind) bool {
	t.Helper()

	info, statErr := os.Lstat(path)
	isFolder := statErr == nil && info.IsDir()
	holds := false
	switch want {
	case localFolder:
		holds = isFolder
	case localFile:
		holds = statErr == nil && !isFolder
	case localNoFolder:
		holds = !isFolder
	}

	return assert.Truef(t, holds, "%s is not %s", suiteDriveRoles.Redact(path), want)
}

// assertSamePath asserts that got is the local path want, printing both by
// role: a failure needs the paths' shape, and a fixture's name or a mount's
// ID in them stays out.
func assertSamePath(t *testing.T, what, want, got string) {
	t.Helper()

	isSame := got == want
	assert.Truef(t, isSame, "%s: %s, want %s", what, suiteDriveRoles.Redact(got), suiteDriveRoles.Redact(want))
}

// requireNoLocalError is require.NoError for a local file operation: its
// error quotes the path, which may hold a fixture's name, so a failure
// prints the error masked.
func requireNoLocalError(t *testing.T, err error, what string) {
	t.Helper()

	if err != nil {
		require.FailNow(t, what+": "+printable(err.Error()))
	}
}
