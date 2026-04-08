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

const defaultCoverageThreshold = 76.0

type cwdLookup func() (string, error)

type verifyFunc func(context.Context, devtool.VerifyOptions) error

type cleanupAuditFunc func(context.Context, devtool.CleanupAuditOptions) error

type stateAuditFunc func(context.Context, devtool.StateAuditOptions) error

type watchCaptureFunc func(context.Context, devtool.WatchCaptureOptions) error

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
		newCleanupAuditCmd(defaultCWD, defaultCleanupAudit),
		newStateAuditCmd(defaultStateAudit),
		newWatchCaptureCmd(defaultWatchCapture),
		newWorktreeCmd(defaultCWD, defaultAddWorktree, defaultBootstrapWorktree),
	)

	return cmd
}

func newVerifyCmd(getwd cwdLookup, runVerify verifyFunc) *cobra.Command {
	var (
		coverageThreshold float64
		coverageFile      string
		e2eLogDir         string
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

			return runVerify(cmd.Context(), devtool.VerifyOptions{
				RepoRoot:          repoRoot,
				Profile:           profile,
				CoverageThreshold: coverageThreshold,
				CoverageFile:      coverageFile,
				E2ELogDir:         e2eLogDir,
				Stdout:            cmd.OutOrStdout(),
				Stderr:            cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().Float64Var(&coverageThreshold, "coverage-threshold", defaultCoverageThreshold, "minimum total coverage percentage")
	cmd.Flags().StringVar(&coverageFile, "coverage-file", "", "coverage profile path")
	cmd.Flags().StringVar(&e2eLogDir, "e2e-log-dir", "", "directory for full E2E debug logs")

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

func newStateAuditCmd(runStateAudit stateAuditFunc) *cobra.Command {
	var (
		dbPath     string
		jsonOutput bool
		repairSafe bool
	)

	cmd := &cobra.Command{
		Use:   "state-audit",
		Short: "Inspect and optionally repair sync-state integrity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStateAudit(cmd.Context(), devtool.StateAuditOptions{
				DBPath:     dbPath,
				JSON:       jsonOutput,
				RepairSafe: repairSafe,
				Stdout:     cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "", "path to the sync state database")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON output")
	cmd.Flags().BoolVar(&repairSafe, "repair-safe", false, "apply deterministic safe repairs before re-auditing")
	requireFlag(cmd, "db")

	return cmd
}

func newWatchCaptureCmd(runWatchCapture watchCaptureFunc) *cobra.Command {
	var (
		scenario   string
		jsonOutput bool
		repeat     int
		settle     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "watch-capture",
		Short: "Capture raw fsnotify event sequences for sync watch scenarios",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWatchCapture(cmd.Context(), devtool.WatchCaptureOptions{
				Scenario: scenario,
				JSON:     jsonOutput,
				Repeat:   repeat,
				Settle:   settle,
				Stdout:   cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().StringVar(&scenario, "scenario", "", "watch-capture scenario name")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON output")
	cmd.Flags().IntVar(&repeat, "repeat", 1, "number of times to rerun the scenario")
	cmd.Flags().DurationVar(
		&settle,
		"settle",
		devtool.DefaultWatchCaptureSettle,
		"idle window used to drain raw fsnotify events after each step",
	)
	requireFlag(cmd, "scenario")

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

func defaultVerify(ctx context.Context, opts devtool.VerifyOptions) error {
	if err := devtool.RunVerify(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("run verify: %w", err)
	}

	return nil
}

func defaultCleanupAudit(ctx context.Context, opts devtool.CleanupAuditOptions) error {
	if err := devtool.RunCleanupAudit(ctx, devtool.ExecRunner{}, opts); err != nil {
		return fmt.Errorf("run cleanup audit: %w", err)
	}

	return nil
}

func defaultWatchCapture(ctx context.Context, opts devtool.WatchCaptureOptions) error {
	if err := devtool.RunWatchCapture(ctx, opts); err != nil {
		return fmt.Errorf("run watch capture: %w", err)
	}

	return nil
}

func defaultAddWorktree(ctx context.Context, repoRoot, path, branch string) error {
	if err := devtool.AddWorktree(ctx, devtool.ExecRunner{}, repoRoot, path, branch); err != nil {
		return fmt.Errorf("add worktree: %w", err)
	}

	return nil
}

func defaultStateAudit(ctx context.Context, opts devtool.StateAuditOptions) error {
	if err := devtool.RunStateAudit(ctx, opts); err != nil {
		return fmt.Errorf("run state audit: %w", err)
	}

	return nil
}

func defaultBootstrapWorktree(repoRoot, targetPath string) error {
	if err := devtool.BootstrapWorktree(repoRoot, targetPath); err != nil {
		return fmt.Errorf("bootstrap worktree: %w", err)
	}

	return nil
}
