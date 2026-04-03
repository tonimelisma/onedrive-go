package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/devtool"
)

const testRepoRoot = "/repo"

// Validates: R-6.2.1
func TestNewVerifyCmdDefaultsToDefaultProfile(t *testing.T) {
	t.Parallel()

	var got devtool.VerifyOptions

	cmd := newVerifyCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.VerifyOptions) error {
			got = opts
			return nil
		},
	)

	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.Equal(t, devtool.VerifyDefault, got.Profile)
	assert.InDelta(t, defaultCoverageThreshold, got.CoverageThreshold, 0.001)
}

// Validates: R-6.2.1
func TestNewVerifyCmdPassesFlagsThrough(t *testing.T) {
	t.Parallel()

	var got devtool.VerifyOptions

	cmd := newVerifyCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.VerifyOptions) error {
			got = opts
			return nil
		},
	)

	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"integration", "--coverage-threshold", "80.5", "--coverage-file", "/tmp/c.out", "--e2e-log-dir", "/tmp/e2e"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, devtool.VerifyIntegration, got.Profile)
	assert.InDelta(t, 80.5, got.CoverageThreshold, 0.001)
	assert.Equal(t, "/tmp/c.out", got.CoverageFile)
	assert.Equal(t, "/tmp/e2e", got.E2ELogDir)
}

// Validates: R-6.2.1
func TestNewWorktreeAddCmdRequiresFlags(t *testing.T) {
	t.Parallel()

	cmd := newWorktreeAddCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(context.Context, string, string, string) error { return nil },
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required flag")
}

// Validates: R-6.2.1
func TestNewWorktreeAddCmdCallsAddWorktree(t *testing.T) {
	t.Parallel()

	var (
		gotRepoRoot string
		gotPath     string
		gotBranch   string
	)

	cmd := newWorktreeAddCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, repoRoot, path, branch string) error {
			gotRepoRoot = repoRoot
			gotPath = path
			gotBranch = branch
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--path", "/wt", "--branch", "refactor/test"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, gotRepoRoot)
	assert.Equal(t, "/wt", gotPath)
	assert.Equal(t, "refactor/test", gotBranch)
}

// Validates: R-6.2.1
func TestNewWorktreeBootstrapCmdDefaultsTargetPathToRepoRoot(t *testing.T) {
	t.Parallel()

	var (
		gotRepoRoot string
		gotTarget   string
	)

	cmd := newWorktreeBootstrapCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(repoRoot, targetPath string) error {
			gotRepoRoot = repoRoot
			gotTarget = targetPath
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, gotRepoRoot)
	assert.Equal(t, testRepoRoot, gotTarget)
}

// Validates: R-6.2.1
func TestNewWorktreeBootstrapCmdSupportsExplicitSourceRoot(t *testing.T) {
	t.Parallel()

	var (
		gotRepoRoot string
		gotTarget   string
	)

	cmd := newWorktreeBootstrapCmd(
		func() (string, error) { return "/ignored", nil },
		func(repoRoot, targetPath string) error {
			gotRepoRoot = repoRoot
			gotTarget = targetPath
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--source-root", "/source", "--path", "/target"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, "/source", gotRepoRoot)
	assert.Equal(t, "/target", gotTarget)
}

// Validates: R-6.2.1
func TestNewVerifyCmdWrapsWorkingDirectoryError(t *testing.T) {
	t.Parallel()

	cmd := newVerifyCmd(
		func() (string, error) { return "", errors.New("boom") },
		func(context.Context, devtool.VerifyOptions) error { return nil },
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get working directory")
}

// Sanity check: the root command still assembles into a valid Cobra tree.
func TestNewRootCmd(t *testing.T) {
	t.Parallel()

	cmd := newRootCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "devtool", cmd.Use)
	assert.Len(t, cmd.Commands(), 2)
}

// Validates: R-6.2.1
func TestDefaultCWD(t *testing.T) {
	t.Parallel()

	cwd, err := defaultCWD()
	require.NoError(t, err)
	assert.NotEmpty(t, cwd)
}

// Validates: R-6.2.1
func TestDefaultVerifyWrapsRunVerifyError(t *testing.T) {
	t.Parallel()

	err := defaultVerify(context.Background(), devtool.VerifyOptions{
		RepoRoot: testRepoRoot,
		Profile:  devtool.VerifyProfile("weird"),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run verify")
}

// Validates: R-6.2.1
func TestDefaultBootstrapWorktreeWrapsError(t *testing.T) {
	t.Parallel()

	err := defaultBootstrapWorktree(t.TempDir(), filepath.Join(t.TempDir(), "wt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap worktree")
}

// Validates: R-6.2.1
func TestDefaultAddWorktreeWrapsError(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	err := defaultAddWorktree(context.Background(), repoRoot, filepath.Join(repoRoot, "wt"), "refactor/test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add worktree")
}

// Validates: R-6.2.1
func TestMainIntegrationHelpersUseRealFilesystemState(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte(".env\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".env"), []byte("TOKEN=1\n"), 0o600))

	require.NoError(t, defaultBootstrapWorktree(repoRoot, targetRoot))
}

var _ = cobra.Command{}

func repoRootFromTest(t *testing.T) string {
	t.Helper()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	return filepath.Clean(filepath.Join(cwd, "..", ".."))
}

func buildDevtoolBinary(t *testing.T) string {
	t.Helper()

	binPath := filepath.Join(t.TempDir(), "devtool-test")
	//nolint:gosec // test builds the repo-owned devtool binary with fixed arguments.
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binPath, "./cmd/devtool")
	cmd.Dir = repoRootFromTest(t)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	return binPath
}

func runBinary(t *testing.T, cwd, binPath string, args ...string) (string, string, error) {
	t.Helper()

	//nolint:gosec // test executes the temp-built devtool binary with test-controlled args.
	cmd := exec.CommandContext(t.Context(), binPath, args...)
	cmd.Dir = cwd

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

func runGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()

	//nolint:gosec // test drives git against temporary repositories with explicit args.
	cmd := exec.CommandContext(t.Context(), "git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	return string(output)
}

// Validates: R-6.2.1
func TestDevtoolBinary_VerifyRejectsUnknownProfile(t *testing.T) {
	binPath := buildDevtoolBinary(t)

	_, stderr, err := runBinary(t, t.TempDir(), binPath, "verify", "weird")
	require.Error(t, err)
	assert.Contains(t, stderr, "usage: devtool verify [default|public|e2e|e2e-full|integration]")
}

// Validates: R-6.2.1
func TestDevtoolBinary_WorktreeBootstrapCopiesAndSymlinks(t *testing.T) {
	binPath := buildDevtoolBinary(t)

	sourceRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(sourceRoot, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, ".testdata", "config.toml"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, ".env"), []byte("TOKEN=1\n"), 0o600))

	stdout, stderr, err := runBinary(t, sourceRoot, binPath, "worktree", "bootstrap", "--path", targetRoot)
	require.NoError(t, err, stdout+stderr)

	//nolint:gosec // test reads a temp file created under the test-owned target root.
	envData, readErr := os.ReadFile(filepath.Join(targetRoot, ".env"))
	require.NoError(t, readErr)
	assert.Equal(t, "TOKEN=1\n", string(envData))

	linkTarget, linkErr := os.Readlink(filepath.Join(targetRoot, ".testdata"))
	require.NoError(t, linkErr)
	assert.Equal(t, filepath.Join(sourceRoot, ".testdata"), linkTarget)
}

// Validates: R-6.2.1
func TestDevtoolBinary_WorktreeAddCreatesBootstrappedWorktree(t *testing.T) {
	binPath := buildDevtoolBinary(t)

	remoteRoot := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", remoteRoot)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "clone", remoteRoot, repoRoot)
	runGit(t, repoRoot, "checkout", "-b", "main")
	runGit(t, repoRoot, "config", "user.name", "Test User")
	runGit(t, repoRoot, "config", "user.email", "test@example.com")

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".env"), []byte("TOKEN=1\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".testdata", "config.toml"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("test repo\n"), 0o600))

	runGit(t, repoRoot, "add", ".")
	runGit(t, repoRoot, "commit", "-m", "initial")
	runGit(t, repoRoot, "push", "-u", "origin", "main")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	stdout, stderr, err := runBinary(t, repoRoot, binPath, "worktree", "add", "--path", worktreePath, "--branch", "refactor/test")
	require.NoError(t, err, stdout+stderr)

	branchName := strings.TrimSpace(runGit(t, worktreePath, "branch", "--show-current"))
	assert.Equal(t, "refactor/test", branchName)

	//nolint:gosec // test reads a temp file created under the test-owned worktree root.
	envData, readErr := os.ReadFile(filepath.Join(worktreePath, ".env"))
	require.NoError(t, readErr)
	assert.Equal(t, "TOKEN=1\n", string(envData))

	linkTarget, linkErr := os.Readlink(filepath.Join(worktreePath, ".testdata"))
	require.NoError(t, linkErr)
	assert.Equal(t, filepath.Join(repoRoot, ".testdata"), linkTarget)
}
