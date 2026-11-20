package syncfixture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FakeSeedReplayTB is a seed's subtest as LogSeedReplayOnFailure sees it.
type FakeSeedReplayTB struct {
	TestName  string
	HasFailed bool
	cleanups  []func()
	logs      []string
}

func (f *FakeSeedReplayTB) Helper()                {}
func (f *FakeSeedReplayTB) Name() string           { return f.TestName }
func (f *FakeSeedReplayTB) Failed() bool           { return f.HasFailed }
func (f *FakeSeedReplayTB) Cleanup(cleanup func()) { f.cleanups = append(f.cleanups, cleanup) }
func (f *FakeSeedReplayTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

// Logs are the lines the subtest logged.
func (f *FakeSeedReplayTB) Logs() []string { return f.logs }

// End runs the registered cleanups as the testing package does when the
// subtest ends: the last registered first.
func (f *FakeSeedReplayTB) End() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// SeedReplayCommand is a replay line's command, read as a shell reads it.
type SeedReplayCommand struct {
	Assignments []string
	Race        bool
	RunPattern  string
	Pkg         string
}

// ParseSeedReplayCommand reads the command after the replay prefix: the
// assignments, then `go test`, -race or not, -run and its single-quoted
// pattern, and the package, nothing else.
func ParseSeedReplayCommand(t *testing.T, command string) SeedReplayCommand {
	t.Helper()

	words := shellWords(t, command)
	var replay SeedReplayCommand
	for len(words) > 0 && strings.Contains(words[0], "=") {
		replay.Assignments = append(replay.Assignments, words[0])
		words = words[1:]
	}
	goTest := []string{"go", "test"}
	require.Greater(t, len(words), len(goTest), command)
	require.Equal(t, goTest, words[:len(goTest)], command)
	words = words[len(goTest):]
	if words[0] == "-race" {
		replay.Race = true
		words = words[1:]
	}
	// -run, its pattern and the package, nothing after them.
	const tail = 3
	require.Len(t, words, tail, command)
	require.Equal(t, "-run", words[0], command)
	replay.RunPattern, replay.Pkg = words[1], words[2]

	return replay
}

// shellWords splits command into words as a POSIX shell does for what a
// replay line holds: blanks separate words, and single quotes keep a word's
// characters as they are. An unclosed quote fails the test.
func shellWords(t *testing.T, command string) []string {
	t.Helper()

	words := []string{}
	var word strings.Builder
	inWord, quoted := false, false
	for _, r := range command {
		switch {
		case r == '\'':
			quoted, inWord = !quoted, true
		case r == ' ' && !quoted:
			if inWord {
				words = append(words, word.String())
				word.Reset()
			}
			inWord = false
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	require.False(t, quoted, "unclosed quote in %q", command)
	if inWord {
		words = append(words, word.String())
	}

	return words
}

// PackageTestSources is the text of the test files of the package the test
// runs in: go test runs a package's tests in its directory.
func PackageTestSources(t *testing.T) string {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var sources strings.Builder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(entry.Name())
		require.NoError(t, readErr)
		sources.Write(data)
	}

	return sources.String()
}

// AssertIsThisPackage holds that pkg, a path from the module root, is the
// package the test runs in.
func AssertIsThisPackage(t *testing.T, pkg string) {
	t.Helper()

	here, err := filepath.Abs(".")
	require.NoError(t, err)
	assert.Equal(t, here, filepath.Join(ModuleRoot(t), filepath.FromSlash(strings.TrimPrefix(pkg, "./"))),
		"the replay runs this package")
}
