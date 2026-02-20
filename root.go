package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// version is set at build time via ldflags.
var version = "dev"

// Global persistent flags, bound in setupRootCmd().
var (
	flagProfile string
	flagJSON    bool
	flagVerbose bool
	flagQuiet   bool
)

// newRootCmd builds and returns the fully-assembled root command with all
// subcommands registered. Called once from main().
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "onedrive-go",
		Short:   "OneDrive CLI client",
		Long:    "A fast, safe OneDrive CLI and sync client for Linux and macOS.",
		Version: version,
		// Silence Cobra's default error/usage printing — we handle it ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	cmd.PersistentFlags().StringVar(&flagProfile, "profile", "default", "configuration profile name")
	cmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "output in JSON format")
	cmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "enable debug logging")
	cmd.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "suppress informational output")

	// Register subcommands.
	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newWhoamiCmd())
	cmd.AddCommand(newLsCmd())
	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newPutCmd())
	cmd.AddCommand(newRmCmd())
	cmd.AddCommand(newMkdirCmd())
	cmd.AddCommand(newStatCmd())

	return cmd
}

// buildLogger creates an slog.Logger configured by verbose/quiet flags.
func buildLogger() *slog.Logger {
	level := slog.LevelInfo

	if flagVerbose {
		level = slog.LevelDebug
	}

	if flagQuiet {
		level = slog.LevelError
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// exitOnError prints a user-friendly error message to stderr and exits.
func exitOnError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
