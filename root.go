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
	flagAccount    string
	flagDrive      string
	flagJSON       bool
	flagVerbose    bool
	flagQuiet      bool
)

// resolvedCfg holds the effective configuration loaded by PersistentPreRunE.
// It is available to all subcommands after the root pre-run phase completes.
// Auth commands (login, logout, whoami) skip config loading and use --drive directly.
var resolvedCfg *config.ResolvedDrive

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
		// PersistentPreRunE loads configuration before every command. Auth commands
		// (login, logout, whoami) skip config loading because they work with --drive
		// directly — solving the bootstrap problem where login must work before any
		// config file or drive section exists.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			switch cmd.Name() {
			case "login", "logout", "whoami":
				return nil
			default:
				return loadConfig(cmd)
			}
		},
	}

	cmd.PersistentFlags().StringVar(&flagConfigPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&flagAccount, "account", "", "account for auth commands (e.g., user@example.com)")
	cmd.PersistentFlags().StringVar(&flagDrive, "drive", "", "drive selector (canonical ID, alias, or partial match)")
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

// loadConfig resolves the effective configuration from the four-layer override
// chain and stores the result in resolvedCfg for use by subcommands.
func loadConfig(cmd *cobra.Command) error {
	cli := config.CLIOverrides{
		ConfigPath: flagConfigPath,
	}

	// Only pass --drive to the resolver if the user explicitly set it.
	if cmd.Flags().Changed("drive") {
		cli.Drive = flagDrive
	}

	env := config.ReadEnvOverrides()

	resolved, err := config.ResolveDrive(env, cli)
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
		switch resolvedCfg.LogLevel {
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
