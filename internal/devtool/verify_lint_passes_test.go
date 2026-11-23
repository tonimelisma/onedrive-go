package devtool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.22
//
// Every Go file of the module is read by a lint pass on every pull request:
// the passes build it, or it is a declared exception no pass can build.
func TestEveryGoFileIsBuiltByALintPass(t *testing.T) {
	t.Parallel()

	unread, err := filesNoLintPassBuilds(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, unread)
}

// Validates: R-6.10.22
//
// A file whose constraint no pass builds, by file name or by its //go:build
// line, fails the lint step's source check, which names it; the passes still
// run, so their findings are not hidden. A file each pass builds passes, and
// so does a file the go command never builds as part of the module: under
// testdata, in a directory whose name starts with a dot or an underscore,
// or in a nested module.
func TestLintStepFailsOnAFileNoLintPassBuilds(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	files := map[string]string{
		"internal/a/a.go":                  "package a\n",
		"internal/a/a_windows.go":          "package a\n",
		"internal/a/a_openbsd_only.go":     "//go:build openbsd\n\npackage a\n",
		"internal/a/a_race.go":             "//go:build race\n\npackage a\n",
		"internal/a/a_norace.go":           "//go:build !race\n\npackage a\n",
		"internal/a/a_darwin.go":           "package a\n",
		"internal/a/a_otherunix.go":        "//go:build unix && !linux && !darwin\n\npackage a\n",
		"internal/a/a_e2e_test.go":         "//go:build e2e && e2e_full\n\npackage a\n",
		"internal/a/testdata/t_windows.go": "package t\n",
		"internal/.hidden/h_windows.go":    "package h\n",
		"internal/_skip/s_windows.go":      "package s\n",
		"tools/go.mod":                     "module tools\n",
		"tools/x_windows.go":               "package tools\n",
	}
	for rel, content := range files {
		require.NoError(t, writeFile(filepath.Join(repoRoot, filepath.FromSlash(rel)), []byte(content)))
	}

	unread, err := filesNoLintPassBuilds(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, []string{"internal/a/a_openbsd_only.go", "internal/a/a_windows.go"}, unread)

	runner := &fakeRunner{combinedOutputs: map[string][]byte{lintVersionCommand: []byte(testLintVersionOutput)}}
	err = lintStepRun(lintPasses(), lintWholeModule)(context.Background(), runner, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no lint pass builds internal/a/a_openbsd_only.go, internal/a/a_windows.go")
	assert.Len(t, runner.runCommands, len(lintPasses()), "every pass still runs")
}

// Validates: R-6.10.22
//
// Each declared exception is a file of the tree that no pass builds, with
// its reason, so an exception cannot outlive the file or the constraint.
func TestUnlintedFileExceptionsAreFilesNoPassBuilds(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootForTest(t)
	for _, exception := range unlintedFileExceptions() {
		assert.NotEmpty(t, exception.reason, exception.path)
		path := filepath.Join(repoRoot, filepath.FromSlash(exception.path))
		_, err := os.Stat(path)
		require.NoError(t, err, "%s exists", exception.path)
		built, err := builtByALintPass(filepath.Dir(path), filepath.Base(path))
		require.NoError(t, err)
		assert.False(t, built, "a pass builds %s, so it is no exception", exception.path)
	}
}

// Validates: R-6.10.22
//
// The lint step runs one golangci-lint per pass, each on its platform and
// with its tags; --lint-pass runs the one it names, as a CI matrix leg does,
// names the passes when it names none of them, and is refused by a profile
// that does not lint.
func TestRunVerifyLintRunsTheNamedLintPass(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyLint,
		LintPass: "darwin-race",
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	lint := []recordedCommand{}
	for _, command := range runner.runCommands {
		if command.name == lintBinaryName {
			lint = append(lint, command)
		}
	}
	require.Len(t, lint, 1)
	assert.Equal(t, append(hostLintCommandArgs(), "--build-tags=e2e,e2e_full,integration,race"), lint[0].args)
	assertCommandHasEnvVar(t, lint[0], "GOOS=darwin")

	err = RunVerify(context.Background(), &fakeRunner{}, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyLint,
		LintPass: "windows",
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `--lint-pass "windows" is no lint pass: the passes are linux, darwin-race, freebsd`)

	err = RunVerify(context.Background(), &fakeRunner{}, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyQuick,
		LintPass: "linux",
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--lint-pass applies to the profiles that lint")
}

// Validates: R-6.10.22
//
// The CI lint job has one matrix leg per lint pass, in the passes' order,
// and each leg runs its own pass, so the per-PR lint reads every build the
// passes read.
func TestCILintJobRunsEveryLintPass(t *testing.T) {
	t.Parallel()

	jobs, err := readCIWorkflowJobs(repoRootFromDevtoolPackage(t))
	require.NoError(t, err)
	var lintJob *ciWorkflowJob
	for i := range jobs {
		if jobs[i].name == "verify-lint" {
			lintJob = &jobs[i]
		}
	}
	require.NotNil(t, lintJob, "ci.yml has the verify-lint job")

	names := []string{}
	for _, pass := range lintPasses() {
		names = append(names, pass.name)
	}
	matrix := ""
	runsNamedPass := false
	for _, line := range lintJob.lines {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "pass: ["); ok {
			matrix = strings.TrimSuffix(rest, "]")
		}
		if strings.Contains(trimmed, "verify lint --lint-pass \"${LINT_PASS}\"") {
			runsNamedPass = true
		}
	}
	assert.Equal(t, strings.Join(names, ", "), matrix)
	assert.True(t, runsNamedPass, "each leg runs the pass its matrix value names")
}
