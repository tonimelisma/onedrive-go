package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// version is set at build time via ldflags.
var version = "dev"

// Global persistent flags, bound in setupRootCmd().
var (
	flagConfigPath string
	flagProfile    string
	flagJSON       bool
	flagVerbose    bool
	flagQuiet      bool
)

// resolvedCfg holds the effective configuration loaded by PersistentPreRunE.
// It is available to all subcommands after the root pre-run phase completes.
var resolvedCfg *config.ResolvedProfile

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
		// PersistentPreRunE loads configuration before every command. The four-layer
		// override chain (defaults -> file -> env -> CLI) is applied here so that
		// subcommands always see a fully resolved config via resolvedCfg.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return loadConfig(cmd)
		},
	}

	cmd.PersistentFlags().StringVar(&flagConfigPath, "config", "", "config file path")
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
	cmd.AddCommand(newConfigCmd())

	return cmd
}

// loadConfig resolves the effective configuration from the four-layer override
// chain and stores the result in resolvedCfg for use by subcommands.
func loadConfig(cmd *cobra.Command) error {
	cli := config.CLIOverrides{
		ConfigPath: flagConfigPath,
	}

	// Only pass --profile to the resolver if the user explicitly set it.
	// The pflag default "default" is indistinguishable from an explicit
	// --profile=default at the value level, so we check Changed() instead.
	if cmd.Flags().Changed("profile") {
		cli.Profile = flagProfile
	}

	env := config.ReadEnvOverrides()

	resolved, err := config.Resolve(env, cli)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	resolvedCfg = resolved

	return nil
}

// buildLogger creates an slog.Logger configured by the resolved config and
// CLI flags. Config-file log level provides the baseline; --verbose and
// --quiet override it because CLI flags always win.
func buildLogger() *slog.Logger {
	level := slog.LevelInfo

	// Config-based log level (lower priority than CLI flags).
	if resolvedCfg != nil {
		switch resolvedCfg.Logging.LogLevel {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}

	// CLI flags override config (highest priority).
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
