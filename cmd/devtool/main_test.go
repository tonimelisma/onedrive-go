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
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/devtool"
)

const testRepoRoot = "/repo"

// Validates: R-6.10.1, R-6.10.16
//
// With no profile argument the command names none, and the verifier picks the
// default of what it is asked to run: `default` for a plain verify, `pr` for
// the DoD pre-pr stage. Filling in `default` here would make that stage run
// the full tier on every host.
func TestNewVerifyCmdLeavesTheProfileToTheVerifierByDefault(t *testing.T) {
	t.Parallel()

	var got devtool.VerifyOptions

	cmd := newVerifyCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts *devtool.VerifyOptions) error {
			got = *opts
			return nil
		},
	)

	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.Empty(t, got.Profile)
	assert.Zero(t, got.CoverageThreshold, "no threshold given: the gate uses the number requirement R-6.10.4 states")
	assert.False(t, got.DOD)
	assert.Equal(t, devtool.DefaultDODRecentMerged, got.DODRecentMerged)
	assert.Zero(t, got.DODCITimeout, "no hard cap given: the wait derives one from the stall window")
	assert.Equal(t, devtool.DefaultDODCIMaxReruns, got.DODCIMaxReruns)
	assert.Zero(t, got.UnitTimeout, "no deadline given: the verifier scales it with the host's cores")
	assert.Zero(t, got.ShortUnitTimeout, "no deadline given: the verifier scales it with the host's cores")
	assert.Zero(t, got.RaceTimeout, "no deadline given: the verifier scales it with the host's cores")
}

// Validates: R-6.10.1
func TestNewVerifyCmdPassesFlagsThrough(t *testing.T) {
	t.Parallel()

	var got devtool.VerifyOptions

	cmd := newVerifyCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts *devtool.VerifyOptions) error {
			got = *opts
			return nil
		},
	)

	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"integration",
		"--coverage-threshold", "80.5",
		"--coverage-file", "/tmp/c.out",
		"--shard", "2/3",
		"--lint-pass", "darwin-race",
		"--e2e-log-dir", "/tmp/e2e",
		"--summary-json", "/tmp/verify-summary.json",
		"--verify-log-dir", "/tmp/verify-logs",
		"--classify-live-quirks",
		"--dod",
		"--stage", "pre-merge",
		"--pr", "682",
		"--recent-merged", "12",
		"--comment-manifest", "/tmp/dod-comments.json",
		"--worktree", "/tmp/wt",
		"--branch", "feat/dod",
		"--ci-timeout", "45m",
		"--ci-max-reruns", "1",
		"--unit-timeout", "50m",
		"--short-unit-timeout", "25m",
		"--race-timeout", "70m",
		"--failed-log", "/tmp/ci.log",
		"--check-format",
		"--as-user", "dev",
	})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, devtool.VerifyIntegration, got.Profile)
	assert.InDelta(t, 80.5, got.CoverageThreshold, 0.001)
	assert.Equal(t, "/tmp/c.out", got.CoverageFile)
	assert.Equal(t, "2/3", got.Shard)
	assert.Equal(t, "darwin-race", got.LintPass)
	assert.Equal(t, "/tmp/e2e", got.E2ELogDir)
	assert.Equal(t, "/tmp/verify-summary.json", got.SummaryJSONPath)
	assert.Equal(t, "/tmp/verify-logs", got.VerifyLogDir)
	assert.True(t, got.ClassifyLiveQuirks)
	assert.True(t, got.DOD)
	assert.Equal(t, devtool.DODStagePreMerge, got.DODStage)
	assert.Equal(t, 682, got.DODPR)
	assert.Equal(t, 12, got.DODRecentMerged)
	assert.Equal(t, "/tmp/dod-comments.json", got.DODCommentManifest)
	assert.Equal(t, "/tmp/wt", got.DODWorktree)
	assert.Equal(t, "feat/dod", got.DODBranch)
	assert.Equal(t, 45*time.Minute, got.DODCITimeout)
	assert.Equal(t, 1, got.DODCIMaxReruns)
	assert.Equal(t, 50*time.Minute, got.UnitTimeout)
	assert.Equal(t, 25*time.Minute, got.ShortUnitTimeout)
	assert.Equal(t, 70*time.Minute, got.RaceTimeout)
	assert.Equal(t, "/tmp/ci.log", got.FailedTestLog)
	assert.True(t, got.CheckFormat)
	assert.Equal(t, "dev", got.AsUser)
}

// Validates: R-6.10.38
//
// A cloud environment sets the test user once, for every session, through
// the environment; the flag still overrides it.
func TestNewVerifyCmdTakesTheTestUserFromTheEnvironment(t *testing.T) {
	t.Setenv(devtool.TestUserEnv, "builder")

	var got devtool.VerifyOptions
	newCmd := func(args ...string) error {
		cmd := newVerifyCmd(
			func() (string, error) { return testRepoRoot, nil },
			func(_ context.Context, opts *devtool.VerifyOptions) error {
				got = *opts
				return nil
			},
		)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)

		return cmd.Execute()
	}

	require.NoError(t, newCmd("quick"))
	assert.Equal(t, "builder", got.AsUser)

	require.NoError(t, newCmd("quick", "--as-user", "dev"))
	assert.Equal(t, "dev", got.AsUser)
}

// Validates: R-6.10.14
func TestNewBenchCmdRequiresScenarioFlag(t *testing.T) {
	t.Parallel()

	cmd := newBenchCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(context.Context, devtool.BenchOptions) error { return nil },
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required flag")
}

// Validates: R-6.10.14
func TestNewBenchCmdDefaultsSubjectToOnedriveGo(t *testing.T) {
	t.Parallel()

	var got devtool.BenchOptions

	cmd := newBenchCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.BenchOptions) error {
			got = opts
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--scenario", "startup-empty-config"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.Equal(t, devtool.DefaultBenchSubjectID, got.Subject)
	assert.Equal(t, "startup-empty-config", got.Scenario)
}

// Validates: R-6.10.14
func TestNewBenchCmdPassesFlagsThrough(t *testing.T) {
	t.Parallel()

	var got devtool.BenchOptions

	cmd := newBenchCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.BenchOptions) error {
			got = opts
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--scenario", "startup-empty-config",
		"--subject", "onedrive-go",
		"--runs", "9",
		"--warmup", "2",
		"--json",
		"--result-json", "/tmp/bench-result.json",
		"--fixture-slot", "slot-03",
	})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.Equal(t, "startup-empty-config", got.Scenario)
	assert.Equal(t, "onedrive-go", got.Subject)
	assert.Equal(t, 9, got.Runs)
	assert.Equal(t, 2, got.Warmup)
	assert.True(t, got.JSON)
	assert.Equal(t, "/tmp/bench-result.json", got.ResultJSONPath)
	assert.Equal(t, "slot-03", got.FixtureSlot)
}

// Validates: R-6.10.11
func TestNewCleanupAuditCmdPassesFlagsThrough(t *testing.T) {
	t.Parallel()

	var got devtool.CleanupAuditOptions

	cmd := newCleanupAuditCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.CleanupAuditOptions) error {
			got = opts
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--json"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.True(t, got.JSON)
}

// Validates: R-6.10.21
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

// Validates: R-6.10.21
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

// Validates: R-6.10.37
func TestNewWorktreePruneMergedCmdPassesDryRun(t *testing.T) {
	t.Parallel()

	var got devtool.WorktreePruneOptions
	calls := 0
	cmd := newWorktreePruneMergedCmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.WorktreePruneOptions) error {
			calls++
			got = opts
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--dry-run"})

	require.NoError(t, cmd.Execute())
	require.Equal(t, 1, calls)
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.True(t, got.DryRun)
	assert.NotNil(t, got.Stdout)
}

// Validates: R-6.10.21
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

// Validates: R-6.10.21
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

// Validates: R-6.10.1
func TestNewVerifyCmdWrapsWorkingDirectoryError(t *testing.T) {
	t.Parallel()

	cmd := newVerifyCmd(
		func() (string, error) { return "", errors.New("boom") },
		func(context.Context, *devtool.VerifyOptions) error { return nil },
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
	names := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
	}
	assert.ElementsMatch(t, []string{"verify", "bench", "cleanup-audit", "worktree", "nightly", "ci"}, names)
}

// Validates: R-6.10.1
func TestDefaultCWD(t *testing.T) {
	t.Parallel()

	cwd, err := defaultCWD()
	require.NoError(t, err)
	assert.NotEmpty(t, cwd)
}

// Validates: R-6.10.1
func TestDefaultVerifyWrapsRunVerifyError(t *testing.T) {
	t.Parallel()

	err := defaultVerify(context.Background(), &devtool.VerifyOptions{
		RepoRoot: testRepoRoot,
		Profile:  devtool.VerifyProfile("weird"),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run verify")
}

// Validates: R-6.10.14
func TestDefaultBenchWrapsRunBenchError(t *testing.T) {
	t.Parallel()

	err := defaultBench(context.Background(), devtool.BenchOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run bench")
}

// Validates: R-6.10.21
func TestDefaultBootstrapWorktreeWrapsError(t *testing.T) {
	t.Parallel()

	err := defaultBootstrapWorktree(t.TempDir(), filepath.Join(t.TempDir(), "wt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap worktree")
}

// Validates: R-6.10.21
func TestDefaultAddWorktreeWrapsError(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	err := defaultAddWorktree(context.Background(), repoRoot, filepath.Join(repoRoot, "wt"), "refactor/test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add worktree")
}

// Validates: R-6.10.11
func TestDefaultCleanupAuditWrapsError(t *testing.T) {
	t.Parallel()

	err := defaultCleanupAudit(context.Background(), devtool.CleanupAuditOptions{
		RepoRoot: filepath.Join(t.TempDir(), "missing"),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run cleanup audit")
}

// Validates: R-6.10.1
func TestRequireFlagPanicsForUnknownFlag(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "test"}
	assert.Panics(t, func() {
		requireFlag(cmd, "missing")
	})
}

// Validates: R-6.10.1
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

// Validates: R-6.10.1
func TestDevtoolBinary_VerifyRejectsUnknownProfile(t *testing.T) {
	binPath := buildDevtoolBinary(t)

	_, stderr, err := runBinary(t, t.TempDir(), binPath, "verify", "weird")
	require.Error(t, err)
	assert.Contains(t, stderr,
		"usage: devtool verify [default|public|pr|quick|lint|platform|e2e|e2e-full|integration|stress|race|seeds|failed-test]")
}

// Validates: R-6.10.22, R-6.10.25
//
// The race and seeds profiles are profiles the command passes on to the
// verifier, and --seed-start reaches the seeds profile; without it the start
// is left to the verifier, which derives it from the date.
func TestNewVerifyCmdAcceptsTheRaceAndSeedsProfiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		args      []string
		profile   devtool.VerifyProfile
		seedStart uint64
	}{
		{args: []string{"race"}, profile: devtool.VerifyRace},
		{args: []string{"seeds"}, profile: devtool.VerifySeeds},
		{args: []string{"seeds", "--seed-start", "318001"}, profile: devtool.VerifySeeds, seedStart: 318001},
	}
	for _, tc := range cases {
		var got devtool.VerifyOptions
		cmd := newVerifyCmd(
			func() (string, error) { return testRepoRoot, nil },
			func(_ context.Context, opts *devtool.VerifyOptions) error {
				got = *opts
				return nil
			},
		)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(tc.args)

		require.NoError(t, cmd.Execute(), tc.args)
		assert.Equal(t, tc.profile, got.Profile, tc.args)
		assert.Equal(t, tc.seedStart, got.SeedStart, tc.args)
		assert.Contains(t, cmd.Use, "|"+string(tc.profile)+"|", "the usage line names the profile")
	}
}

// Validates: R-6.10.21
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

// Validates: R-6.10.21
func TestDevtoolBinary_WorktreeBootstrapHonorsExplicitSourceRoot(t *testing.T) {
	binPath := buildDevtoolBinary(t)

	cwdRoot := t.TempDir()
	sourceRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(cwdRoot, ".worktreeinclude"), []byte(".env\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cwdRoot, ".env"), []byte("TOKEN=wrong\n"), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(sourceRoot, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, ".testdata", "config.toml"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, ".env"), []byte("TOKEN=right\n"), 0o600))

	stdout, stderr, err := runBinary(
		t,
		cwdRoot,
		binPath,
		"worktree",
		"bootstrap",
		"--source-root",
		sourceRoot,
		"--path",
		targetRoot,
	)
	require.NoError(t, err, stdout+stderr)

	//nolint:gosec // test reads a temp file created under the test-owned target root.
	envData, readErr := os.ReadFile(filepath.Join(targetRoot, ".env"))
	require.NoError(t, readErr)
	assert.Equal(t, "TOKEN=right\n", string(envData))

	linkTarget, linkErr := os.Readlink(filepath.Join(targetRoot, ".testdata"))
	require.NoError(t, linkErr)
	assert.Equal(t, filepath.Join(sourceRoot, ".testdata"), linkTarget)
}

// Validates: R-6.10.21
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

// Validates: R-6.10.11
func TestDevtoolBinary_CleanupAuditClassifiesRepoState(t *testing.T) {
	binPath := buildDevtoolBinary(t)

	remoteRoot := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", remoteRoot)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "clone", remoteRoot, repoRoot)
	runGit(t, repoRoot, "checkout", "-b", "main")
	runGit(t, repoRoot, "config", "user.name", "Test User")
	runGit(t, repoRoot, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("main\n"), 0o600))
	runGit(t, repoRoot, "add", "README.md")
	runGit(t, repoRoot, "commit", "-m", "initial")
	runGit(t, repoRoot, "push", "-u", "origin", "main")

	runGit(t, repoRoot, "checkout", "-b", "refactor/merged")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "merged.txt"), []byte("merged\n"), 0o600))
	runGit(t, repoRoot, "add", "merged.txt")
	runGit(t, repoRoot, "commit", "-m", "merged branch")
	runGit(t, repoRoot, "checkout", "main")
	runGit(t, repoRoot, "merge", "--ff-only", "refactor/merged")
	runGit(t, repoRoot, "push", "origin", "main")
	runGit(t, repoRoot, "push", "-u", "origin", "refactor/merged")

	runGit(t, repoRoot, "checkout", "-b", "refactor/topic")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "topic.txt"), []byte("topic\n"), 0o600))
	runGit(t, repoRoot, "add", "topic.txt")
	runGit(t, repoRoot, "commit", "-m", "topic branch")
	runGit(t, repoRoot, "push", "-u", "origin", "refactor/topic")
	runGit(t, repoRoot, "checkout", "main")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoRoot, "worktree", "add", "-b", "refactor/attached", worktreePath, "main")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0o600))
	canonicalRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)
	canonicalWorktreePath, err := filepath.EvalSymlinks(worktreePath)
	require.NoError(t, err)

	stdout, stderr, err := runBinary(t, repoRoot, binPath, "cleanup-audit")
	require.NoError(t, err, stdout+stderr)
	assert.Contains(t, stdout, "cleanup audit: worktrees")
	assert.Contains(t, stdout, "keep_main: "+canonicalRepoRoot+" (branch: main; root main worktree)")
	assert.Contains(t, stdout, "keep_dirty: "+canonicalWorktreePath+" (branch: refactor/attached; attached worktree has local modifications)")
	assert.Contains(t, stdout, "keep_dirty: refactor/attached (attached worktree has local modifications)")
	assert.Contains(t, stdout, "safe_remove: refactor/merged (branch tip is reachable from main)")
	assert.Contains(t, stdout, "keep_unmerged: refactor/topic (branch has commits not reachable from main)")
	assert.Contains(t, stdout, "safe_remove: origin/refactor/merged (remote branch tip is reachable from main)")
	assert.Contains(t, stdout, "keep_unmerged: origin/refactor/topic (remote branch has commits not reachable from main)")
}

// Validates: R-6.10.23
func TestNewNightlyCmdPassesFlagsThrough(t *testing.T) {
	t.Parallel()

	var got []devtool.NightlyOptions
	newCmd := func() *cobra.Command {
		cmd := newNightlyCmd(
			func() (string, error) { return testRepoRoot, nil },
			func(_ context.Context, opts devtool.NightlyOptions) error {
				got = append(got, opts)
				return nil
			},
		)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd
	}

	check := newCmd()
	check.SetArgs([]string{"check", "--branch", "fix/x"})
	require.NoError(t, check.Execute())

	release := newCmd()
	release.SetArgs([]string{"release", "--branch", "fix/x", "--key", "TestY"})
	require.NoError(t, release.Execute())

	missingKey := newCmd()
	missingKey.SetArgs([]string{"release", "--branch", "fix/x"})
	require.Error(t, missingKey.Execute())

	report := newCmd()
	report.SetArgs([]string{"report", "--run-id", "4242", "--dry-run=true"})
	require.NoError(t, report.Execute())

	scheduled := newCmd()
	scheduled.SetArgs([]string{"report", "--run-id", "4243", "--dry-run=false"})
	require.NoError(t, scheduled.Execute())

	missingRun := newCmd()
	missingRun.SetArgs([]string{"report"})
	require.Error(t, missingRun.Execute())

	require.Len(t, got, 4)
	assert.Equal(t, devtool.NightlyActionCheck, got[0].Action)
	assert.Equal(t, "fix/x", got[0].Branch)
	assert.Equal(t, testRepoRoot, got[0].RepoRoot)
	assert.Equal(t, devtool.NightlyActionRelease, got[1].Action)
	assert.Equal(t, "TestY", got[1].Key)
	assert.Equal(t, devtool.NightlyActionReport, got[2].Action)
	assert.Equal(t, int64(4242), got[2].RunID)
	assert.True(t, got[2].DryRun)
	assert.Equal(t, int64(4243), got[3].RunID)
	assert.False(t, got[3].DryRun)
}

// executeRoot runs devtool's own root command with args and returns the exit
// code and everything it printed, cobra's output and the error alike.
func executeRoot(t *testing.T, args ...string) (int, string) {
	t.Helper()

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	return execute(t.Context(), root, &out), out.String()
}

// Validates: R-6.10.23
//
// A command that fails while it runs prints its error exactly once, with no
// usage text: the usage says nothing about a GitHub outage or a failed check.
func TestRootCmdPrintsARunErrorOnceWithoutUsage(t *testing.T) {
	t.Parallel()

	code, out := executeRoot(t, "nightly", "report", "--run-id", "0")

	assert.Equal(t, 1, code)
	assert.Equal(t, "run nightly: nightly report: --run-id is required\n", out)
}

// Validates: R-6.10.23
//
// A command line that leaves out a required flag is a mistake in the command
// line, found before the command runs: the usage is shown, and the error is
// still printed once.
func TestRootCmdShowsUsageForAMissingRequiredFlag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		args    []string
		message string
	}{
		{args: []string{"nightly", "report"}, message: `required flag(s) "run-id" not set`},
		{args: []string{"nightly", "release", "--branch", "fix/x"}, message: `required flag(s) "key" not set`},
		{args: []string{"nightly", "check", "--no-such-flag"}, message: "unknown flag: --no-such-flag"},
	} {
		code, out := executeRoot(t, tc.args...)

		assert.Equal(t, 1, code, "%v", tc.args)
		assert.Contains(t, out, "Usage:", "%v", tc.args)
		assert.Equal(t, 1, strings.Count(out, tc.message), "%v: the error is printed once", tc.args)
		assert.NotContains(t, out, "Error:", "%v: cobra does not print it a second time", tc.args)
	}
}

// Validates: R-6.10.39
func TestNewCIRerunUnstartedCmdPassesTheRun(t *testing.T) {
	t.Parallel()

	var got devtool.CIRerunOptions
	calls := 0
	cmd := newCICmd(
		func() (string, error) { return testRepoRoot, nil },
		func(_ context.Context, opts devtool.CIRerunOptions) error {
			calls++
			got = opts
			return nil
		},
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"rerun-unstarted", "--run-id", "4242"})

	require.NoError(t, cmd.Execute())
	require.Equal(t, 1, calls)
	assert.Equal(t, testRepoRoot, got.RepoRoot)
	assert.Equal(t, int64(4242), got.RunID)
	assert.NotNil(t, got.Stdout)
}

// Validates: R-6.10.39
func TestNewCIRerunUnstartedCmdRequiresTheRun(t *testing.T) {
	t.Parallel()

	cmd := newCICmd(
		func() (string, error) { return testRepoRoot, nil },
		func(context.Context, devtool.CIRerunOptions) error { return nil },
	)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"rerun-unstarted"})

	require.Error(t, cmd.Execute())
}
