package devtool

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLintWorkflow is a valid workflow that pins golangci-lint the way the
// repository's own does, so the lint profile's version check has its pin.
const testLintWorkflow = `name: CI
on: push
jobs:
  lint:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - uses: actions/checkout@v7.0.1
      - name: Install golangci-lint
        uses: golangci/golangci-lint-action@v9
        with:
          version: v2.11.3
          install-only: true
`

// testBrokenWorkflow writes its `run:` as a plain scalar that holds ": ", which
// YAML reads as a mapping inside the scalar, so GitHub cannot load the file.
// The offending line is the eighth.
const testBrokenWorkflow = `name: broken
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - run: echo note: the colon ends the scalar
`

// testLintVersionOutput is what the pinned golangci-lint prints for --version.
const testLintVersionOutput = "golangci-lint has version 2.11.3 built with go1.26.11 from 0123abcd on 2026-01-01T00:00:00Z\n"

func writeWorkflowFile(t *testing.T, repoRoot string, name string, content string) {
	t.Helper()

	require.NoError(t, writeFile(filepath.Join(repoRoot, ".github", "workflows", name), []byte(content)))
}

// workflowLintRunner runs `go run` for real, so a test sees what the pinned
// actionlint says about a file, and fakes every other command.
type workflowLintRunner struct {
	*fakeRunner
}

func (r workflowLintRunner) Run(
	ctx context.Context,
	cwd string,
	env []string,
	stdout, stderr io.Writer,
	name string,
	args ...string,
) error {
	if name == goCommandName && len(args) > 0 && args[0] == "run" {
		return ExecRunner{}.Run(ctx, cwd, env, stdout, stderr, name, args...)
	}

	return r.fakeRunner.Run(ctx, cwd, env, stdout, stderr, name, args...)
}

// Validates: R-6.10.2
//
// A workflow GitHub cannot load passed every Go test and every verify profile:
// nothing read the workflow files as YAML, so an invalid one surfaced only
// when GitHub refused to run it. The lint profile lints them, and its error
// names the file and the line.
func TestVerifyLint_RefusesAnInvalidWorkflowFile(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeWorkflowFile(t, repoRoot, filepath.Base(ciWorkflowPath), testLintWorkflow)
	writeWorkflowFile(t, repoRoot, "broken.yml", testBrokenWorkflow)
	runner := workflowLintRunner{fakeRunner: &fakeRunner{
		combinedOutputs: map[string][]byte{lintVersionCommand: []byte(testLintVersionOutput)},
	}}

	err := RunVerify(t.Context(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyLint,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".github/workflows/broken.yml:8:")
	assert.NotContains(t, err.Error(), "ci.yml:", "the valid workflow has no finding")
}

// Validates: R-6.10.2
//
// The lint is the pinned actionlint over every workflow file, with its
// shellcheck and pyflakes integrations off: neither tool is installed here or
// assumed in CI, and a runner that happened to have one would lint by
// another policy.
func TestRunVerifyLintRunsThePinnedActionlintOverEveryWorkflow(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeWorkflowFile(t, repoRoot, filepath.Base(ciWorkflowPath), testLintWorkflow)
	writeWorkflowFile(t, repoRoot, "release.yaml", testLintWorkflow)
	writeWorkflowFile(t, repoRoot, "README.md", "not a workflow\n")
	runner := &fakeRunner{combinedOutputs: map[string][]byte{lintVersionCommand: []byte(testLintVersionOutput)}}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyLint,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 2)
	assert.Equal(t, "golangci-lint", runner.runCommands[0].name)
	lint := runner.runCommands[1]
	assert.Equal(t, "go", lint.name)
	assert.Equal(t, []string{
		"run", actionlintModule, "-shellcheck=", "-pyflakes=", "-oneline",
		ciWorkflowPath, ".github/workflows/release.yaml",
	}, lint.args)
	assert.Equal(t, repoRoot, lint.cwd)
}

// Validates: R-6.10.2
//
// A tree without workflow files has nothing GitHub could refuse, so the step
// runs no command; every fixture tree of the profile tests is one.
func TestRunWorkflowLint_TreeWithoutWorkflowsRunsNothing(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	stdout := &bytes.Buffer{}
	require.NoError(t, runWorkflowLint(context.Background(), runner, t.TempDir(), nil, stdout, io.Discard))
	assert.Empty(t, runner.runCommands)
	assert.Contains(t, stdout.String(), "no workflow files")
}

// Validates: R-6.10.2
//
// A failed lint names each finding in its error; a run that failed before
// linting anything (the module download, say) keeps the command's error.
func TestRunWorkflowLint_NamesEveryFinding(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeWorkflowFile(t, repoRoot, filepath.Base(ciWorkflowPath), testLintWorkflow)
	lintKey := goCommandKey(append(actionlintArgs(), ciWorkflowPath))
	runner := &fakeRunner{
		runErrByKey: map[string]error{lintKey: assert.AnError},
		runStdoutFunc: func([]string) string {
			return ".github/workflows/ci.yml:3:1: \"jobs\" section is missing in workflow [syntax-check]\n" +
				".github/workflows/ci.yml:9:9: unknown input \"versio\" [action]\n"
		},
	}

	err := runWorkflowLint(context.Background(), runner, repoRoot, nil, io.Discard, io.Discard)
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), ".github/workflows/ci.yml:3:1:")
	assert.Contains(t, err.Error(), ".github/workflows/ci.yml:9:9:")

	runner.runStdoutFunc = func([]string) string { return "go: downloading the module failed\n" }
	err = runWorkflowLint(context.Background(), runner, repoRoot, nil, io.Discard, io.Discard)
	require.ErrorIs(t, err, assert.AnError)
	assert.NotContains(t, err.Error(), "workflow lint:", "no finding to name")
}

// Validates: R-6.10.2
//
// A Go compile error has a finding's shape, `<file>:<line>:<column>: ...`, so
// a failed build of actionlint itself read as findings in the workflows. Only
// a line that names a workflow file the lint was given is a finding.
func TestRunWorkflowLint_ACompileErrorIsNotAFinding(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeWorkflowFile(t, repoRoot, filepath.Base(ciWorkflowPath), testLintWorkflow)
	lintKey := goCommandKey(append(actionlintArgs(), ciWorkflowPath))
	runner := &fakeRunner{
		runErrByKey: map[string]error{lintKey: assert.AnError},
		runStdoutFunc: func([]string) string {
			return "# github.com/rhysd/actionlint\n" +
				"../../go/pkg/mod/github.com/rhysd/actionlint@v1.7.12/rule_expression.go:12:3: undefined: exprNode\n"
		},
	}

	err := runWorkflowLint(context.Background(), runner, repoRoot, nil, io.Discard, io.Discard)
	require.ErrorIs(t, err, assert.AnError)
	assert.NotContains(t, err.Error(), "workflow lint:", "a compile error is no workflow finding")
	assert.NotContains(t, err.Error(), "rule_expression.go")
}
