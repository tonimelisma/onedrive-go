package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/devtool"
)

type cwdLookup func() (string, error)

type verifyFunc func(context.Context, *devtool.VerifyOptions) error

type benchFunc func(context.Context, devtool.BenchOptions) error

type cleanupAuditFunc func(context.Context, devtool.CleanupAuditOptions) error

type nightlyFunc func(context.Context, devtool.NightlyOptions) error

type worktreePruneFunc func(context.Context, devtool.WorktreePruneOptions) error

type ciRerunFunc func(context.Context, devtool.CIRerunOptions) error

func main() {
	os.Exit(execute(context.Background(), newRootCmd(), os.Stderr))
}

// execute runs root and returns the process's exit code. It prints the error
// root ends with, once: root leaves the printing to it.
func execute(ctx context.Context, root *cobra.Command, stderr io.Writer) int {
	if err := root.ExecuteContext(ctx); err != nil {
		// The exit code reports the failure even when stderr cannot take the
		// message, which has nowhere else to go.
		fmt.Fprintln(stderr, err) //nolint:errcheck // the exit code reports the failure
		return 1
	}

	return 0
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
		newWorktreeCmd(defaultCWD, defaultPruneMergedWorktrees),
		newNightlyCmd(defaultCWD, defaultNightly),
		newCICmd(defaultCWD, defaultCIRerun),
	)
	silenceRunErrorUsage(cmd)

	return cmd
}

// silenceRunErrorUsage makes a command that fails while it runs end with its
// error alone, which execute prints once: cobra neither prints it a second
// time nor follows it with the usage text, which says nothing about a GitHub
// outage or a failed check. A mistake in the command line still shows the
// usage: cobra reports a bad flag or argument before this hook runs, and the
// hook checks the required flags and flag groups itself before it silences
// the usage, because cobra checks them only after it.
func silenceRunErrorUsage(root *cobra.Command) {
	root.SilenceErrors = true
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := cmd.ValidateRequiredFlags(); err != nil {
			return fmt.Errorf("check required flags: %w", err)
		}
		if err := cmd.ValidateFlagGroups(); err != nil {
			return fmt.Errorf("check flag groups: %w", err)
		}
		cmd.SilenceUsage = true

		return nil
	}
}

func newNightlyCmd(getwd cwdLookup, runNightly nightlyFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nightly",
		Short: "Check the nightly CI run, own its failures through claim issues, and report its verdict",
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

	cmd.AddCommand(check, release, newNightlyReportCmd(getwd, runNightly))

	return cmd
}

// newCICmd groups the commands CI itself runs about its own runs.
func newCICmd(getwd cwdLookup, rerun ciRerunFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Commands CI runs about its own runs",
	}
	cmd.AddCommand(newCIRerunUnstartedCmd(getwd, rerun))

	return cmd
}

func newCIRerunUnstartedCmd(getwd cwdLookup, rerun ciRerunFunc) *cobra.Command {
	var runID int64
	cmd := &cobra.Command{
		Use:   "rerun-unstarted",
		Short: "Rerun a finished CI run whose only trouble is jobs no runner took",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return rerun(cmd.Context(), devtool.CIRerunOptions{
				RepoRoot: repoRoot,
				RunID:    runID,
				Stdout:   cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().Int64Var(&runID, "run-id", 0, "the finished CI run to judge")
	requireFlag(cmd, "run-id")

	return cmd
}

func newNightlyReportCmd(getwd cwdLookup, runNightly nightlyFunc) *cobra.Command {
	var (
		runID  int64
		dryRun bool
	)
	report := &cobra.Command{
		Use:   "report",
		Short: "Post a scheduled run's verdict as the commit status nightly and the one nightly-failure issue",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runNightly(cmd.Context(), devtool.NightlyOptions{
				RepoRoot: repoRoot,
				Action:   devtool.NightlyActionReport,
				RunID:    runID,
				DryRun:   dryRun,
				Stdout:   cmd.OutOrStdout(),
			})
		},
	}
	report.Flags().Int64Var(&runID, "run-id", 0, "the workflow run to report on")
	report.Flags().BoolVar(&dryRun, "dry-run", false, "print the status and the issue text, write nothing")
	requireFlag(report, "run-id")

	return report
}

// verifyFlags holds the values of the verify command's flags.
type verifyFlags struct {
	coverageThreshold  float64
	coverageFile       string
	shard              string
	lintPass           string
	e2eLogDir          string
	summaryJSONPath    string
	verifyLogDir       string
	classifyLiveQuirks bool
	dod                bool
	dodStage           string
	dodPR              int
	dodRecentMerged    int
	dodCommentManifest string
	dodBranch          string
	dodCITimeout       time.Duration
	dodCIMaxReruns     int
	unitTimeout        time.Duration
	shortUnitTimeout   time.Duration
	raceTimeout        time.Duration
	failedTestLog      string
	checkFormat        bool
	seedStart          uint64
	asUser             string
}

func newVerifyCmd(getwd cwdLookup, runVerify verifyFunc) *cobra.Command {
	flags := &verifyFlags{}

	cmd := &cobra.Command{
		Use:   "verify [" + devtool.VerifyProfileUsage + "]",
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
		LintPass:           f.lintPass,
		UnitTimeout:        f.unitTimeout,
		ShortUnitTimeout:   f.shortUnitTimeout,
		RaceTimeout:        f.raceTimeout,
		E2ELogDir:          f.e2eLogDir,
		SummaryJSONPath:    f.summaryJSONPath,
		VerifyLogDir:       f.verifyLogDir,
		ClassifyLiveQuirks: f.classifyLiveQuirks,
		DOD:                f.dod,
		DODStage:           devtool.DODStage(f.dodStage),
		DODPR:              f.dodPR,
		DODRecentMerged:    f.dodRecentMerged,
		DODCommentManifest: f.dodCommentManifest,
		DODBranch:          f.dodBranch,
		DODCITimeout:       f.dodCITimeout,
		DODCIMaxReruns:     f.dodCIMaxReruns,
		FailedTestLog:      f.failedTestLog,
		CheckFormat:        f.checkFormat,
		SeedStart:          f.seedStart,
		AsUser:             f.asUser,
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
	cmd.Flags().StringVar(
		&f.lintPass,
		"lint-pass",
		"",
		"run one golangci-lint pass of the lint step (linux, darwin-race or freebsd); default every pass",
	)
	unitDeadline, shortDeadline, raceDeadline := devtool.TierDeadlineDefaults()
	cmd.Flags().DurationVar(
		&f.unitTimeout,
		"unit-timeout",
		0,
		"go test deadline of the full race tier; default "+unitDeadline.String()+", longer on hosts with fewer cores than the CI runners",
	)
	cmd.Flags().DurationVar(
		&f.shortUnitTimeout,
		"short-unit-timeout",
		0,
		"go test deadline of the short unit tier; default "+shortDeadline.String()+", longer on hosts with fewer cores than the CI runners",
	)
	cmd.Flags().DurationVar(
		&f.raceTimeout,
		"race-timeout",
		0,
		"go test deadline of the race profile; default "+raceDeadline.String()+", longer on hosts with fewer cores than the CI runners",
	)
	cmd.Flags().StringVar(&f.e2eLogDir, "e2e-log-dir", "", "directory for full E2E debug logs")
	cmd.Flags().StringVar(&f.summaryJSONPath, "summary-json", "", "write verify summary JSON to this path")
	cmd.Flags().StringVar(
		&f.verifyLogDir,
		"verify-log-dir",
		"",
		"directory where a failed go test step keeps its full output; default .verify-logs under the repository root",
	)
	cmd.Flags().StringVar(&f.failedTestLog, "failed-log", "", "CI go-test log to parse for verify failed-test")
	cmd.Flags().Uint64Var(
		&f.seedStart,
		"seed-start",
		0,
		"first seed of the seeds profile, to rerun a night; 0 derives it from today's UTC date",
	)
	cmd.Flags().BoolVar(
		&f.checkFormat,
		"check-format",
		false,
		"list the files the formatters would change and fail when there are any, instead of rewriting them",
	)
	cmd.Flags().StringVar(
		&f.asUser,
		"as-user",
		os.Getenv(devtool.TestUserEnv),
		"run the go test steps as this user when verify runs as root on Linux (default from "+devtool.TestUserEnv+")",
	)
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

func newWorktreeCmd(getwd cwdLookup, pruneMerged worktreePruneFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Prune the builder worktrees whose work is in origin/main",
	}

	cmd.AddCommand(newWorktreePruneMergedCmd(getwd, pruneMerged))

	return cmd
}

func newWorktreePruneMergedCmd(getwd cwdLookup, pruneMerged worktreePruneFunc) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "prune-merged",
		Short: "Remove the clean builder worktrees under .claude/worktrees whose work is already in origin/main",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return pruneMerged(cmd.Context(), devtool.WorktreePruneOptions{
				RepoRoot: repoRoot,
				DryRun:   dryRun,
				Stdout:   cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be removed and remove nothing")

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

func defaultPruneMergedWorktrees(ctx context.Context, opts devtool.WorktreePruneOptions) error {
	if err := devtool.PruneMergedWorktrees(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("prune merged worktrees: %w", err)
	}

	return nil
}

func defaultCIRerun(ctx context.Context, opts devtool.CIRerunOptions) error {
	if err := devtool.RerunUnstartedCIJobs(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("rerun unstarted ci jobs: %w", err)
	}

	return nil
}

func defaultNightly(ctx context.Context, opts devtool.NightlyOptions) error {
	if err := devtool.RunNightly(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("run nightly: %w", err)
	}

	return nil
}
