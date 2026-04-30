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

const defaultCoverageThreshold = 75.5

type cwdLookup func() (string, error)

type verifyFunc func(context.Context, *devtool.VerifyOptions) error

type benchFunc func(context.Context, devtool.BenchOptions) error

type cleanupAuditFunc func(context.Context, devtool.CleanupAuditOptions) error

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
	)

	return cmd
}

func newVerifyCmd(getwd cwdLookup, runVerify verifyFunc) *cobra.Command {
	var (
		coverageThreshold  float64
		coverageFile       string
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
	)

	cmd := &cobra.Command{
		Use:   "verify [default|public|e2e|e2e-full|integration|stress]",
		Short: "Run repository verification profiles",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := devtool.VerifyDefault
			if len(args) == 1 {
				profile = devtool.VerifyProfile(args[0])
			}

			repoRoot, err := getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runVerify(cmd.Context(), &devtool.VerifyOptions{
				RepoRoot:           repoRoot,
				Profile:            profile,
				CoverageThreshold:  coverageThreshold,
				CoverageFile:       coverageFile,
				E2ELogDir:          e2eLogDir,
				SummaryJSONPath:    summaryJSONPath,
				ClassifyLiveQuirks: classifyLiveQuirks,
				DOD:                dod,
				DODStage:           devtool.DODStage(dodStage),
				DODPR:              dodPR,
				DODRecentMerged:    dodRecentMerged,
				DODCommentManifest: dodCommentManifest,
				DODWorktree:        dodWorktree,
				DODBranch:          dodBranch,
				DODCITimeout:       dodCITimeout,
				Stdout:             cmd.OutOrStdout(),
				Stderr:             cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().Float64Var(&coverageThreshold, "coverage-threshold", defaultCoverageThreshold, "minimum total coverage percentage")
	cmd.Flags().StringVar(&coverageFile, "coverage-file", "", "coverage profile path")
	cmd.Flags().StringVar(&e2eLogDir, "e2e-log-dir", "", "directory for full E2E debug logs")
	cmd.Flags().StringVar(&summaryJSONPath, "summary-json", "", "write verify summary JSON to this path")
	cmd.Flags().BoolVar(&dod, "dod", false, "run Definition of Done automation instead of the selected verification profile")
	cmd.Flags().StringVar(&dodStage, "stage", "", "DoD stage: start, pre-pr, pre-merge, or post-merge")
	cmd.Flags().IntVar(&dodPR, "pr", 0, "GitHub pull request number for DoD PR stages")
	cmd.Flags().IntVar(&dodRecentMerged, "recent-merged", devtool.DefaultDODRecentMerged, "merged PR count for DoD review-thread audits")
	cmd.Flags().StringVar(&dodCommentManifest, "comment-manifest", "", "DoD PR review-thread manifest path")
	cmd.Flags().StringVar(&dodWorktree, "worktree", "", "increment worktree path for DoD post-merge cleanup")
	cmd.Flags().StringVar(&dodBranch, "branch", "", "increment branch name for DoD post-merge cleanup")
	cmd.Flags().DurationVar(&dodCITimeout, "ci-timeout", devtool.DefaultDODCITimeout, "maximum time to wait for DoD CI checks")
	cmd.Flags().BoolVar(
		&classifyLiveQuirks,
		"classify-live-quirks",
		false,
		"rerun narrow known live-provider quirk failures once during verification",
	)

	return cmd
}

func newBenchCmd(getwd cwdLookup, runBench benchFunc) *cobra.Command {
	var (
		scenario       string
		subject        string
		runs           int
		warmup         int
		jsonOutput     bool
		resultJSONPath string
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
