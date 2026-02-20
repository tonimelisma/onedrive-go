package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

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
// Auth commands and account management commands handle config loading themselves.
//
// Thread safety: this global is safe because the CLI is single-threaded —
// PersistentPreRunE writes it once, then RunE reads it. If the sync engine
// ever runs commands concurrently, this must be refactored (see B-036).
var resolvedCfg *config.ResolvedDrive

// httpClientTimeout is the default timeout for HTTP requests.
// Prevents hung connections from blocking CLI commands indefinitely.
const httpClientTimeout = 30 * time.Second

// defaultHTTPClient returns an HTTP client with a sensible timeout.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: httpClientTimeout}
}

// skipConfigCommands lists commands that handle config loading themselves,
// either because they bootstrap config (login) or because they load config
// directly to avoid the four-layer resolution (logout, whoami, status, drive).
// Uses CommandPath() for explicit matching, safe against future subcommand collisions
// (e.g., a hypothetical "sync add" would not accidentally skip config loading).
var skipConfigCommands = map[string]bool{
	"onedrive-go login":        true,
	"onedrive-go logout":       true,
	"onedrive-go whoami":       true,
	"onedrive-go status":       true,
	"onedrive-go drive":        true,
	"onedrive-go drive add":    true,
	"onedrive-go drive remove": true,
}

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
		// PersistentPreRunE loads configuration before every command. Auth and
		// account management commands skip config loading because they handle
		// config access directly. Login must bootstrap config before it exists;
		// logout, whoami, status, and drive subcommands load config themselves.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if skipConfigCommands[cmd.CommandPath()] {
				return nil
			}

			return loadConfig(cmd)
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
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newDriveCmd())
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
	// Bootstrap logger derived from CLI flags only (resolvedCfg doesn't exist yet).
	// Logs config resolution inputs and outputs at Debug level so --verbose
	// reveals what config path, drive selector, and env overrides are in play.
	logger := bootstrapLogger()

	cli := config.CLIOverrides{
		ConfigPath: flagConfigPath,
	}

	// Only pass --drive to the resolver if the user explicitly set it.
	if cmd.Flags().Changed("drive") {
		cli.Drive = flagDrive
	}

	env := config.ReadEnvOverrides()

	logger.Debug("resolving config",
		slog.String("config_path", cli.ConfigPath),
		slog.String("cli_drive", cli.Drive),
		slog.String("env_config", env.ConfigPath),
		slog.String("env_drive", env.Drive),
	)

	resolved, err := config.ResolveDrive(env, cli)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger.Debug("config resolved",
		slog.String("canonical_id", resolved.CanonicalID),
		slog.String("sync_dir", resolved.SyncDir),
		slog.String("drive_id", resolved.DriveID),
	)

	resolvedCfg = resolved

	return nil
}

// bootstrapLogger creates a minimal logger from CLI flags before resolvedCfg
// exists. Only --verbose and --quiet are considered (no config-file log level).
func bootstrapLogger() *slog.Logger {
	level := slog.LevelInfo

	if flagVerbose {
		level = slog.LevelDebug
	}

	if flagQuiet {
		level = slog.LevelError
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
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
