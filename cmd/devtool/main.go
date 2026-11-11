package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/devtool"
)

type cwdLookup func() (string, error)

type verifyFunc func(context.Context, *devtool.VerifyOptions) error

type benchFunc func(context.Context, devtool.BenchOptions) error

type cleanupAuditFunc func(context.Context, devtool.CleanupAuditOptions) error

type nightlyFunc func(context.Context, devtool.NightlyOptions) error

type worktreeAddFunc func(context.Context, string, string, string) error

type worktreeBootstrapFunc func(string, string) error

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devtool",
		Short: "Development tooling for verification and worktree setup",
	}

	cmd.AddCommand(
		newVerifyCmd(defaultCWD, defaultVerify),
		newBenchCmd(defaultCWD, defaultBench),
		newCleanupAuditCmd(defaultCWD, defaultCleanupAudit),
		newWorktreeCmd(defaultCWD, defaultAddWorktree, defaultBootstrapWorktree),
		newNightlyCmd(defaultCWD, defaultNightly),
	)

	return cmd
}

func newNightlyCmd(getwd cwdLookup, runNightly nightlyFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nightly",
		Short: "Check the nightly CI run and own its failures through claim refs",
	}

	var checkBranch string
	check := &cobra.Command{
		Use:   "check",
		Short: "Claim every failure of the latest nightly run that no other increment holds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runNightly(cmd.Context(), devtool.NightlyOptions{
				RepoRoot: repoRoot,
				Action:   devtool.NightlyActionCheck,
				Branch:   checkBranch,
				Stdout:   cmd.OutOrStdout(),
			})
		},
	}
	check.Flags().StringVar(&checkBranch, "branch", "", "increment branch that owns new claims (defaults to the current branch)")

	var (
		releaseBranch string
		releaseKey    string
	)
	release := &cobra.Command{
		Use:   "release",
		Short: "Give up a claim so another increment picks the failure up",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runNightly(cmd.Context(), devtool.NightlyOptions{
				RepoRoot: repoRoot,
				Action:   devtool.NightlyActionRelease,
				Branch:   releaseBranch,
				Key:      releaseKey,
				Stdout:   cmd.OutOrStdout(),
			})
		},
	}
	release.Flags().StringVar(&releaseBranch, "branch", "", "increment branch that holds the claim")
	release.Flags().StringVar(&releaseKey, "key", "", "claim key, as the check prints it")
	requireFlag(release, "branch")
	requireFlag(release, "key")

	cmd.AddCommand(check, release)

	return cmd
}

// verifyFlags holds the values of the verify command's flags.
type verifyFlags struct {
	coverageThreshold  float64
	coverageFile       string
	shard              string
	e2eLogDir          string
	summaryJSONPath    string
	classifyLiveQuirks bool
	dod                bool
	dodStage           string
	dodPR              int
	dodRecentMerged    int
	dodCommentManifest string
	dodWorktree        string
	dodBranch          string
	dodCITimeout       time.Duration
	dodCIMaxReruns     int
	unitTimeout        time.Duration
	failedTestLog      string
}

func newVerifyCmd(getwd cwdLookup, runVerify verifyFunc) *cobra.Command {
	flags := &verifyFlags{}

	cmd := &cobra.Command{
		Use:   "verify [default|public|pr|quick|lint|platform|e2e|e2e-full|integration|stress|failed-test]",
		Short: "Run repository verification profiles",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// No argument names no profile: the verifier picks `default` for
			// a plain run and `pr` for the DoD pre-pr stage.
			var profile devtool.VerifyProfile
			if len(args) == 1 {
				profile = devtool.VerifyProfile(args[0])
			}

			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runVerify(cmd.Context(), flags.options(cmd, repoRoot, profile))
		},
	}

	flags.registerProfileFlags(cmd)
	flags.registerDODFlags(cmd)

	return cmd
}

// options turns the parsed flags into the verifier's options.
func (f *verifyFlags) options(cmd *cobra.Command, repoRoot string, profile devtool.VerifyProfile) *devtool.VerifyOptions {
	return &devtool.VerifyOptions{
		RepoRoot:           repoRoot,
		Profile:            profile,
		CoverageThreshold:  f.coverageThreshold,
		CoverageFile:       f.coverageFile,
		Shard:              f.shard,
		UnitTimeout:        f.unitTimeout,
		E2ELogDir:          f.e2eLogDir,
		SummaryJSONPath:    f.summaryJSONPath,
		ClassifyLiveQuirks: f.classifyLiveQuirks,
		DOD:                f.dod,
		DODStage:           devtool.DODStage(f.dodStage),
		DODPR:              f.dodPR,
		DODRecentMerged:    f.dodRecentMerged,
		DODCommentManifest: f.dodCommentManifest,
		DODWorktree:        f.dodWorktree,
		DODBranch:          f.dodBranch,
		DODCITimeout:       f.dodCITimeout,
		DODCIMaxReruns:     f.dodCIMaxReruns,
		FailedTestLog:      f.failedTestLog,
		Stdout:             cmd.OutOrStdout(),
		Stderr:             cmd.ErrOrStderr(),
	}
}

// registerProfileFlags registers the flags of the verification profiles.
func (f *verifyFlags) registerProfileFlags(cmd *cobra.Command) {
	// The default is 0, not a number: the number lives only in requirement
	// R-6.10.4, and the verifier reads it from there.
	cmd.Flags().Float64Var(
		&f.coverageThreshold,
		"coverage-threshold",
		0,
		"minimum total coverage percentage; 0 uses the number requirement R-6.10.4 states, negative reports coverage without gating",
	)
	cmd.Flags().StringVar(&f.coverageFile, "coverage-file", "", "coverage profile path")
	cmd.Flags().StringVar(
		&f.shard,
		"shard",
		"",
		"run one slice of the pr, quick or platform profile's short unit tier, as index/count (for example 1/2)",
	)
	cmd.Flags().DurationVar(
		&f.unitTimeout,
		"unit-timeout",
		0,
		"go test deadline of the full race tier; default 20m, longer on hosts with fewer cores than the CI runners",
	)
	cmd.Flags().StringVar(&f.e2eLogDir, "e2e-log-dir", "", "directory for full E2E debug logs")
	cmd.Flags().StringVar(&f.summaryJSONPath, "summary-json", "", "write verify summary JSON to this path")
	cmd.Flags().StringVar(&f.failedTestLog, "failed-log", "", "CI go-test log to parse for verify failed-test")
	cmd.Flags().BoolVar(
		&f.classifyLiveQuirks,
		"classify-live-quirks",
		false,
		"rerun narrow known live-provider quirk failures once during verification",
	)
}

// registerDODFlags registers the flags of the Definition of Done stages.
func (f *verifyFlags) registerDODFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.dod, "dod", false, "run Definition of Done automation instead of the selected verification profile")
	cmd.Flags().StringVar(&f.dodStage, "stage", "", "DoD stage: start, pre-pr, pre-merge, or post-merge")
	cmd.Flags().IntVar(&f.dodPR, "pr", 0, "GitHub pull request number for DoD PR stages")
	cmd.Flags().IntVar(&f.dodRecentMerged, "recent-merged", devtool.DefaultDODRecentMerged, "merged PR count for DoD review-thread audits")
	cmd.Flags().StringVar(&f.dodCommentManifest, "comment-manifest", "", "DoD PR review-thread manifest path")
	cmd.Flags().StringVar(&f.dodWorktree, "worktree", "", "increment worktree path for DoD post-merge cleanup")
	cmd.Flags().StringVar(&f.dodBranch, "branch", "", "increment branch name: owns nightly claims at DoD start, cleaned up at post-merge")
	cmd.Flags().DurationVar(
		&f.dodCITimeout,
		"ci-timeout",
		0,
		"hard cap on one DoD CI wait; default one stall window (longest job timeout in ci.yml plus 10m) per allowed attempt",
	)
	cmd.Flags().IntVar(
		&f.dodCIMaxReruns,
		"ci-max-reruns",
		devtool.DefaultDODCIMaxReruns,
		"times one DoD CI wait reruns the canceled jobs of the commit it waits for; 0 turns reruns off",
	)
}

func newBenchCmd(getwd cwdLookup, runBench benchFunc) *cobra.Command {
	var (
		scenario       string
		subject        string
		runs           int
		warmup         int
		jsonOutput     bool
		resultJSONPath string
		fixtureSlot    string
	)

	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run repo-owned benchmark scenarios",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runBench(cmd.Context(), devtool.BenchOptions{
				RepoRoot:       repoRoot,
				Scenario:       scenario,
				Subject:        subject,
				Runs:           runs,
				Warmup:         warmup,
				JSON:           jsonOutput,
				ResultJSONPath: resultJSONPath,
				FixtureSlot:    fixtureSlot,
				Stdout:         cmd.OutOrStdout(),
				Stderr:         cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().StringVar(&scenario, "scenario", "", "benchmark scenario ID")
	cmd.Flags().StringVar(&subject, "subject", devtool.DefaultBenchSubjectID, "subject under test")
	cmd.Flags().IntVar(&runs, "runs", -1, "override measured sample count (-1 uses the scenario default)")
	cmd.Flags().IntVar(&warmup, "warmup", -1, "override warmup count (-1 uses the scenario default)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the benchmark result bundle as JSON")
	cmd.Flags().StringVar(&resultJSONPath, "result-json", "", "write benchmark result bundle JSON to this path")
	cmd.Flags().StringVar(&fixtureSlot, "fixture-slot", "", "live benchmark fixture slot ID")
	requireFlag(cmd, "scenario")

	return cmd
}

func newWorktreeCmd(getwd cwdLookup, addWorktree worktreeAddFunc, bootstrapWorktree worktreeBootstrapFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Create and bootstrap repo worktrees",
	}

	cmd.AddCommand(
		newWorktreeAddCmd(getwd, addWorktree),
		newWorktreeBootstrapCmd(getwd, bootstrapWorktree),
	)

	return cmd
}

func newCleanupAuditCmd(getwd cwdLookup, runCleanupAudit cleanupAuditFunc) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "cleanup-audit",
		Short: "Classify local git cleanup candidates without deleting anything",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runCleanupAudit(cmd.Context(), devtool.CleanupAuditOptions{
				RepoRoot: repoRoot,
				JSON:     jsonOutput,
				Stdout:   cmd.OutOrStdout(),
				Stderr:   cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON output")

	return cmd
}

func newWorktreeAddCmd(getwd cwdLookup, addWorktree worktreeAddFunc) *cobra.Command {
	var (
		path   string
		branch string
	)

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create a worktree from origin/main and apply .worktreeinclude",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return addWorktree(cmd.Context(), repoRoot, path, branch)
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "worktree path")
	cmd.Flags().StringVar(&branch, "branch", "", "new branch name")
	requireFlag(cmd, "path")
	requireFlag(cmd, "branch")

	return cmd
}

func newWorktreeBootstrapCmd(getwd cwdLookup, bootstrapWorktree worktreeBootstrapFunc) *cobra.Command {
	var (
		path       string
		sourceRoot string
	)

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Apply .worktreeinclude entries to an existing worktree",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot := sourceRoot
			if repoRoot == "" {
				var err error
				repoRoot, err = getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
			}

			targetPath := path
			if targetPath == "" {
				targetPath = repoRoot
			}

			absTarget, err := filepath.Abs(targetPath)
			if err != nil {
				return fmt.Errorf("resolve worktree path: %w", err)
			}

			return bootstrapWorktree(repoRoot, absTarget)
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "existing worktree path (defaults to current working tree)")
	cmd.Flags().StringVar(&sourceRoot, "source-root", "", "source repo/worktree path that owns .worktreeinclude and local adjuncts")

	return cmd
}

func requireFlag(cmd *cobra.Command, name string) {
	if err := cmd.MarkFlagRequired(name); err != nil {
		panic(err)
	}
}

func defaultCWD() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	return cwd, nil
}

func defaultVerify(ctx context.Context, opts *devtool.VerifyOptions) error {
	if err := devtool.RunVerify(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("run verify: %w", err)
	}

	return nil
}

//nolint:gocritic // BenchOptions is the Cobra boundary value and is copied once into devtool.
func defaultBench(ctx context.Context, opts devtool.BenchOptions) error {
	if err := devtool.RunBench(ctx, opts); err != nil {
		return fmt.Errorf("run bench: %w", err)
	}

	return nil
}

func defaultCleanupAudit(ctx context.Context, opts devtool.CleanupAuditOptions) error {
	if err := devtool.RunCleanupAudit(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("run cleanup audit: %w", err)
	}

	return nil
}

func defaultAddWorktree(ctx context.Context, repoRoot, path, branch string) error {
	if err := devtool.AddWorktree(ctx, devtool.ExecRunner{}, repoRoot, path, branch); err != nil {
		return fmt.Errorf("add worktree: %w", err)
	}

	return nil
}

func defaultBootstrapWorktree(repoRoot, targetPath string) error {
	if err := devtool.BootstrapWorktree(repoRoot, targetPath); err != nil {
		return fmt.Errorf("bootstrap worktree: %w", err)
	}

	return nil
}

func defaultNightly(ctx context.Context, opts devtool.NightlyOptions) error {
	if err := devtool.RunNightly(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("run nightly: %w", err)
	}

	return nil
}
